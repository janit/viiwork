package accept

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// freePort returns a port that was free for both TCP and UDP on 127.0.0.1 a
// moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	for range 20 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		pc, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", port))
		ln.Close()
		if err == nil {
			pc.Close()
			return port
		}
	}
	t.Fatal("no port free for both tcp and udp")
	return 0
}

// waitTCP waits until something accepts on addr.
func waitTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing listens on %s", addr)
}

func checkByName(t *testing.T, r Report, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", name, r.Checks)
	return Check{}
}

func TestPortsServeAndProbe(t *testing.T) { // PT1
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var logged []string
	done := make(chan error, 1)
	go func() {
		done <- ServePorts(ctx, netip.MustParseAddr("127.0.0.1"), port, func(f string, a ...any) {
			mu.Lock()
			logged = append(logged, fmt.Sprintf(f, a...))
			mu.Unlock()
		})
	}()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	waitTCP(t, addr)

	r := ProbePorts(context.Background(), DefaultEnv(), "127.0.0.1", port, port, 2*time.Second)
	if len(r.Checks) != 3 || !r.Pass() {
		t.Fatalf("probe: %+v", r.Checks)
	}
	for _, name := range []string{
		"api tcp " + addr,
		"gossip tcp " + addr,
		"gossip udp " + addr,
	} {
		checkByName(t, r, name)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ServePorts after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServePorts did not return after cancel")
	}
	mu.Lock()
	defer mu.Unlock()
	var tcpLog, udpLog bool
	for _, l := range logged {
		tcpLog = tcpLog || strings.Contains(l, "tcp") && strings.Contains(l, "127.0.0.1:")
		udpLog = udpLog || strings.Contains(l, "udp") && strings.Contains(l, "127.0.0.1:")
	}
	if !tcpLog || !udpLog {
		t.Errorf("log lacks a tcp or udp line with the peer address: %q", logged)
	}
}

func TestPortsProbeNothingListening(t *testing.T) { // PT2
	port := freePort(t)
	start := time.Now()
	r := ProbePorts(context.Background(), DefaultEnv(), "127.0.0.1", port, port, 300*time.Millisecond)
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("probe took %v", took)
	}
	if len(r.Checks) != 3 {
		t.Fatalf("checks: %+v", r.Checks)
	}
	for _, c := range r.Checks {
		if c.Pass {
			t.Errorf("%s passed with nothing listening", c.Name)
		}
	}
}

func TestPortsProbeUDPRetries(t *testing.T) { // PT3
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 256)
		for n := 1; ; n++ {
			k, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n >= 3 {
				pc.WriteTo(buf[:k], from)
			}
		}
	}()
	port := pc.LocalAddr().(*net.UDPAddr).Port
	r := ProbePorts(context.Background(), DefaultEnv(), "127.0.0.1", port, port, 5*time.Second)
	if c := checkByName(t, r, fmt.Sprintf("gossip udp 127.0.0.1:%d", port)); !c.Pass {
		t.Errorf("gossip udp failed: %+v", c)
	}
}

func TestPortsServeBindFailure(t *testing.T) { // PT4
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = ServePorts(ctx, netip.MustParseAddr("127.0.0.1"), port, func(string, ...any) {})
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if err == nil || !strings.Contains(err.Error(), "tcp") || !strings.Contains(err.Error(), addr) {
		t.Errorf("error %v, want one naming tcp and %s", err, addr)
	}
}

func TestPortsProbeNonceMismatch(t *testing.T) { // PT5
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				bufio.NewReader(c).ReadString('\n')
				fmt.Fprint(c, "viiwork-accept 0000000000000000\n")
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	r := ProbePorts(context.Background(), DefaultEnv(), "127.0.0.1", port, port, time.Second)
	c := checkByName(t, r, fmt.Sprintf("gossip tcp 127.0.0.1:%d", port))
	if c.Pass || !strings.Contains(c.Detail, "nonce mismatch") {
		t.Errorf("gossip tcp: %+v", c)
	}
}
