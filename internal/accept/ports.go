package accept

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

const (
	echoMax         = 128             // longest line or datagram echoed
	echoReadTimeout = 5 * time.Second // a silent TCP client is dropped after this
	udpAttempts     = 3
	udpInterval     = time.Second
)

// ServePorts echoes on addr:port over TCP and UDP until ctx ends, so a probe
// from another machine can prove both protocols get through. A TCP client
// gets one line of at most 128 bytes back and is closed; a datagram of at most
// 128 bytes goes back to its sender. A port that cannot be bound is an error
// naming the protocol and address.
func ServePorts(ctx context.Context, addr netip.Addr, port int, logf func(string, ...any)) error {
	hostPort := netip.AddrPortFrom(addr, uint16(port)).String()
	ln, err := net.Listen("tcp", hostPort)
	if err != nil {
		return fmt.Errorf("tcp %s: %w", hostPort, err)
	}
	pc, err := net.ListenPacket("udp", hostPort)
	if err != nil {
		ln.Close()
		return fmt.Errorf("udp %s: %w", hostPort, err)
	}
	logf("echoing on tcp and udp %s", hostPort)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		serveTCPEcho(ln, logf)
	}()
	go func() {
		defer wg.Done()
		serveUDPEcho(pc, logf)
	}()
	<-ctx.Done()
	ln.Close()
	pc.Close()
	wg.Wait()
	return nil
}

func serveTCPEcho(ln net.Listener, logf func(string, ...any)) {
	var conns sync.WaitGroup
	defer conns.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		conns.Add(1)
		go func() {
			defer conns.Done()
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(echoReadTimeout))
			line, err := bufio.NewReader(io.LimitReader(c, echoMax)).ReadString('\n')
			if line == "" {
				logf("tcp %s: nothing to echo: %v", c.RemoteAddr(), err)
				return
			}
			if _, err := io.WriteString(c, line); err != nil {
				logf("tcp %s: echo failed: %v", c.RemoteAddr(), err)
				return
			}
			logf("tcp %s: echoed %d bytes", c.RemoteAddr(), len(line))
		}()
	}
}

func serveUDPEcho(pc net.PacketConn, logf func(string, ...any)) {
	buf := make([]byte, echoMax)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if _, err := pc.WriteTo(buf[:n], from); err != nil {
			logf("udp %s: echo failed: %v", from, err)
			continue
		}
		logf("udp %s: echoed %d bytes", from, n)
	}
}

// ProbePorts checks from this machine that host accepts TCP on apiPort and
// echoes a nonce over TCP and UDP on gossipPort. Each check is bounded by
// timeout.
func ProbePorts(ctx context.Context, e Env, host string, apiPort, gossipPort int, timeout time.Duration) Report {
	start := e.Now()
	r := Report{Command: "ports probe", Target: host, Started: start}
	api := net.JoinHostPort(host, strconv.Itoa(apiPort))
	gossip := net.JoinHostPort(host, strconv.Itoa(gossipPort))
	payload := "viiwork-accept " + nonce() + "\n"

	r.Checks = append(r.Checks, timed(e, "api tcp "+api, func() (bool, string) {
		c, err := dialTimeout(ctx, "tcp", api, timeout)
		if err != nil {
			return false, err.Error()
		}
		c.Close()
		return true, ""
	}))
	r.Checks = append(r.Checks, timed(e, "gossip tcp "+gossip, func() (bool, string) {
		return probeTCPEcho(ctx, gossip, payload, timeout)
	}))
	r.Checks = append(r.Checks, timed(e, "gossip udp "+gossip, func() (bool, string) {
		return probeUDPEcho(ctx, gossip, payload, timeout)
	}))
	return r
}

// timed runs one check and records how long it took.
func timed(e Env, name string, f func() (bool, string)) Check {
	start := e.Now()
	pass, detail := f()
	return Check{Name: name, Pass: pass, Detail: detail, Elapsed: e.Now().Sub(start)}
}

func nonce() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func dialTimeout(ctx context.Context, network, addr string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func probeTCPEcho(ctx context.Context, addr, payload string, timeout time.Duration) (bool, string) {
	deadline := time.Now().Add(timeout)
	c, err := dialTimeout(ctx, "tcp", addr, timeout)
	if err != nil {
		return false, err.Error()
	}
	defer c.Close()
	_ = c.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Now()) })
	defer stop()
	if _, err := io.WriteString(c, payload); err != nil {
		return false, err.Error()
	}
	line, err := bufio.NewReader(io.LimitReader(c, echoMax)).ReadString('\n')
	if err != nil {
		return false, err.Error()
	}
	if line != payload {
		return false, "nonce mismatch"
	}
	return true, ""
}

// probeUDPEcho sends the payload up to three times, a second apart, and passes
// at the first matching reply. Datagrams get lost, and a single send would
// fail a healthy path.
func probeUDPEcho(ctx context.Context, addr, payload string, timeout time.Duration) (bool, string) {
	deadline := time.Now().Add(timeout)
	c, err := dialTimeout(ctx, "udp", addr, timeout)
	if err != nil {
		return false, err.Error()
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Now()) })
	defer stop()

	buf := make([]byte, echoMax)
	lastErr := errors.New("no reply")
	for attempt := 0; attempt < udpAttempts && time.Now().Before(deadline); attempt++ {
		if _, err := io.WriteString(c, payload); err != nil {
			lastErr = err
		}
		wait := time.Now().Add(udpInterval)
		if attempt == udpAttempts-1 || wait.After(deadline) {
			wait = deadline
		}
		for time.Now().Before(wait) {
			if ctx.Err() != nil {
				return false, ctx.Err().Error()
			}
			_ = c.SetReadDeadline(wait)
			n, err := c.Read(buf)
			if err != nil {
				var ne net.Error
				if !errors.As(err, &ne) || !ne.Timeout() {
					// Refused (an ICMP answer) returns at once; wait out
					// the interval rather than spinning.
					lastErr = err
					if err := sleepCtx(ctx, time.Until(wait)); err != nil {
						return false, err.Error()
					}
				}
				break
			}
			if string(buf[:n]) == payload {
				return true, ""
			}
			lastErr = errors.New("nonce mismatch")
		}
	}
	return false, lastErr.Error()
}
