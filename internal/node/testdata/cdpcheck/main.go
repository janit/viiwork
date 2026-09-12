// cdpcheck loads the dashboards in headless Chrome over the DevTools protocol,
// fails on any uncaught exception or console error, and checks what each page
// rendered from TestWebFixture's canned data (P6 Task 8 Step 4). It is stdlib
// only, and lives under testdata so the module's build ignores it.
//
// Run the fixture, a headless Chrome sharing its network namespace, and this
// program in that namespace too, so nothing is published on the host:
//
//	docker run -d --name viiwork-webfixture -v "$PWD":/src -w /src -e WEBFIXTURE_FOR=10m \
//	    golang:1.27.0 go test -tags webfixture -run TestWebFixture -timeout 20m ./internal/node/
//	docker run -d --name viiwork-chrome --network container:viiwork-webfixture \
//	    zenika/alpine-chrome:latest --no-sandbox --disable-gpu \
//	    --remote-debugging-address=127.0.0.1 --remote-debugging-port=9222 about:blank
//	docker run --rm --network container:viiwork-webfixture \
//	    -v "$PWD/internal/node/testdata/cdpcheck":/check -w /check golang:1.27.0 go run .
//
// PROBE=1 instead loads a page that throws, to show errors are being caught.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type ws struct {
	c  net.Conn
	r  *bufio.Reader
	mu sync.Mutex
}

func dial(u string) (*ws, error) {
	pu, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	c, err := net.Dial("tcp", pu.Host)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	rand.Read(key)
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		pu.RequestURI(), pu.Host, base64.StdEncoding.EncodeToString(key))
	r := bufio.NewReader(c)
	status, err := r.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		return nil, fmt.Errorf("handshake: %q %v", status, err)
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == "\r\n" {
			break
		}
	}
	return &ws{c: c, r: r}, nil
}

func (w *ws) send(payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	hdr := []byte{0x81}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n)|0x80)
	case n < 65536:
		hdr = append(hdr, 126|0x80, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127|0x80)
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, uint64(n))
		hdr = append(hdr, b...)
	}
	mask := make([]byte, 4)
	rand.Read(mask)
	hdr = append(hdr, mask...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, err := w.c.Write(append(hdr, masked...))
	return err
}

func (w *ws) read() ([]byte, error) {
	var msg []byte
	for {
		h := make([]byte, 2)
		if _, err := io.ReadFull(w.r, h); err != nil {
			return nil, err
		}
		fin, op := h[0]&0x80 != 0, h[0]&0x0f
		n := int64(h[1] & 0x7f)
		if n == 126 {
			b := make([]byte, 2)
			io.ReadFull(w.r, b)
			n = int64(binary.BigEndian.Uint16(b))
		} else if n == 127 {
			b := make([]byte, 8)
			io.ReadFull(w.r, b)
			n = int64(binary.BigEndian.Uint64(b))
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(w.r, p); err != nil {
			return nil, err
		}
		switch op {
		case 8:
			return nil, io.EOF
		case 9, 10:
			continue
		}
		msg = append(msg, p...)
		if fin {
			return msg, nil
		}
	}
}

type cdp struct {
	w       *ws
	mu      sync.Mutex
	next    int
	waiting map[int]chan json.RawMessage
	errors  []string
}

func (c *cdp) loop() {
	for {
		raw, err := c.w.read()
		if err != nil {
			return
		}
		var m struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		json.Unmarshal(raw, &m)
		if m.ID != 0 {
			c.mu.Lock()
			ch := c.waiting[m.ID]
			c.mu.Unlock()
			if ch != nil {
				if m.Error != nil {
					ch <- m.Error
				} else {
					ch <- m.Result
				}
			}
			continue
		}
		switch m.Method {
		case "Runtime.exceptionThrown":
			c.mu.Lock()
			c.errors = append(c.errors, "exception: "+string(m.Params))
			c.mu.Unlock()
		case "Runtime.consoleAPICalled":
			var p struct {
				Type string `json:"type"`
			}
			json.Unmarshal(m.Params, &p)
			if p.Type == "error" || p.Type == "assert" {
				c.mu.Lock()
				c.errors = append(c.errors, "console."+p.Type+": "+string(m.Params))
				c.mu.Unlock()
			}
		case "Log.entryAdded":
			var p struct {
				Entry struct {
					Level, Text, URL string
				} `json:"entry"`
			}
			json.Unmarshal(m.Params, &p)
			if p.Entry.Level == "error" {
				c.mu.Lock()
				c.errors = append(c.errors, "log: "+p.Entry.Text+" "+p.Entry.URL)
				c.mu.Unlock()
			}
		}
	}
}

func (c *cdp) call(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan json.RawMessage, 1)
	c.waiting[id] = ch
	c.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err := c.w.send(b); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-time.After(20 * time.Second):
		return nil, fmt.Errorf("%s: timeout", method)
	}
}

func (c *cdp) eval(expr string) string {
	r, err := c.call("Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true})
	if err != nil {
		return "ERR " + err.Error()
	}
	var v struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
	}
	json.Unmarshal(r, &v)
	return fmt.Sprint(v.Result.Value)
}

type check struct{ name, expr, want string }

type page struct {
	url    string
	checks []check
}

func main() {
	base := "http://127.0.0.1:18086"
	var target struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	var err error
	for i := 0; i < 50; i++ {
		var req *http.Request
		req, _ = http.NewRequest(http.MethodPut, "http://127.0.0.1:9222/json/new?about:blank", nil)
		var resp *http.Response
		resp, err = http.DefaultClient.Do(req)
		if err == nil {
			json.NewDecoder(resp.Body).Decode(&target)
			resp.Body.Close()
			if target.WS != "" {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if target.WS == "" {
		fmt.Println("no devtools target:", err)
		os.Exit(2)
	}
	w, err := dial(target.WS)
	if err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
	c := &cdp{w: w, waiting: map[int]chan json.RawMessage{}}
	go c.loop()
	for _, m := range []string{"Runtime.enable", "Log.enable", "Page.enable"} {
		if _, err := c.call(m, map[string]any{}); err != nil {
			fmt.Println(err)
			os.Exit(2)
		}
	}

	if os.Getenv("PROBE") != "" {
		c.call("Page.navigate", map[string]any{"url": "data:text/html,<script>console.error('probe-error');notAFunction();</script>"})
		time.Sleep(2 * time.Second)
		c.mu.Lock()
		fmt.Printf("probe captured %d errors: %v\n", len(c.errors), c.errors)
		c.mu.Unlock()
		return
	}
	pages := []page{
		{"/mesh", []check{
			{"scope line", `document.getElementById('scope').textContent`, "node-a · 2/3 members alive · open mesh · v2.0.0-fixture"},
			{"backend ids", `['m/0','n/0','x/0'].every(id => document.getElementById('backendsBody').innerText.includes(id))`, "true"},
			{"greyed dead host rows", `document.querySelectorAll('#backendsBody tr.gone').length`, "1"},
			{"dead host named", `document.querySelector('#backendsBody tr.gone').innerText.includes('node-c')`, "true"},
			{"alias rows", `document.querySelectorAll('#aliasesBody tr').length`, "2"},
			{"aliases named", `['stable','legacy','retired-model'].every(s => document.getElementById('aliasesBody').innerText.includes(s))`, "true"},
			// The table is dot | name | target | backends: the state is the dot
			// (and the row's title), so 'fallback' is no longer written out.
			{"alias state is the dot", `document.querySelectorAll('#aliasesBody .dot.starting').length + '/' + document.querySelectorAll('#aliasesBody .dot.healthy').length`, "1/1"},
			// legacy points at retired-model, which nothing serves: 0, in red.
			{"alias backend counts", `[...document.querySelectorAll('#aliasesBody tr')].map(r => r.lastElementChild.textContent).join(',')`, "0,1"},
			{"power rows per host", `document.querySelectorAll('#powerLegend tr').length`, "3"},
			{"offline power row", `document.querySelectorAll('#powerLegend tr.offline').length`, "1"},
			{"GPUs busy tile", `document.getElementById('totGpu').textContent`, "2/4"},
			{"in-flight row", `document.getElementById('inflight').innerText.includes('m/0')`, "true"},
			{"prompt rows", `document.querySelectorAll('#prompts a.prompt-row').length`, "2"},
			{"model pills", `document.querySelectorAll('#models button').length`, "3"},
		}},
		{"/", []check{
			{"one table per model", `document.querySelectorAll('#backends-section table').length`, "2"},
			{"model headings", `document.getElementById('backends-section').innerText.includes('m') && document.getElementById('backends-section').innerText.includes('llamacpp')`, "true"},
			{"backend row", `document.getElementById('backends-section').innerText.includes('m/0')`, "true"},
			{"fleet line", `document.getElementById('fleet-section').innerText.includes('2/3 members alive')`, "true"},
			{"title", `document.getElementById('title').textContent`, "viiwork — node-a"},
		}},
		{"/chat", []check{
			{"model options", `[...document.querySelectorAll('#model-select option')].map(o => o.textContent).join('|')`, "m|n|x|stable → m"},
		}},
		{"/prompt?rid=1&model=m", []check{
			{"prompt text", `document.getElementById('promptBody').textContent.includes("good morning")`, "true"},
		}},
	}

	failed := false
	for _, p := range pages {
		c.mu.Lock()
		c.errors = nil
		c.mu.Unlock()
		if _, err := c.call("Page.navigate", map[string]any{"url": base + p.url}); err != nil {
			fmt.Println(p.url, err)
			failed = true
			continue
		}
		time.Sleep(4 * time.Second)
		for _, ch := range p.checks {
			got := c.eval(ch.expr)
			status := "ok  "
			if got != ch.want {
				status = "FAIL"
				failed = true
			}
			fmt.Printf("%s %-6s %-24s got %q", status, p.url, ch.name, got)
			if got != ch.want {
				fmt.Printf(" want %q", ch.want)
			}
			fmt.Println()
		}
		c.mu.Lock()
		errs := append([]string(nil), c.errors...)
		c.mu.Unlock()
		for _, e := range errs {
			fmt.Printf("FAIL %-6s console error: %s\n", p.url, e)
			failed = true
		}
		if len(errs) == 0 {
			fmt.Printf("ok   %-6s no console errors or exceptions\n", p.url)
		}
	}
	if failed {
		os.Exit(1)
	}
}
