// viiwork-kvsync is a development tool for moving engine session files (Strata,
// llama.cpp) between hosts as deltas (internal/kvshare): a receiver, a sender, and a
// local planner for measuring what a delta would carry.
//
//	viiwork-kvsync serve -listen :7950 -transport tcp -dir /var/lib/kvsync
//	viiwork-kvsync push  -to host:7950 -transport tcp -key conv1 -file /sessions/conv1.bin
//	viiwork-kvsync materialize -dir /var/lib/kvsync -key conv1 -out /sessions/conv1.bin
//	viiwork-kvsync plan  OLD NEW
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Engines register their session file layouts with kvshare.
	_ "github.com/janit/viiwork/v2/internal/engine/all"
	"github.com/janit/viiwork/v2/internal/kvshare"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "push":
		push(os.Args[2:])
	case "plan":
		plan(os.Args[2:])
	case "materialize":
		materialize(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: viiwork-kvsync serve|push|materialize|plan [flags]  (-h for each)")
	os.Exit(2)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "viiwork-kvsync:", err)
	os.Exit(1)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:7950", "address to listen on")
	spec := fs.String("transport", "tcp", "transport: tcp, http, flate+tcp, ...")
	dir := fs.String("dir", "", "directory holding <key>.bin")
	_ = fs.Parse(args)
	if *dir == "" {
		fail(fmt.Errorf("-dir is required"))
	}
	t, err := kvshare.New(*spec)
	if err != nil {
		fail(err)
	}
	srv, err := t.Listen(*listen, kvshare.NewStore(*dir))
	if err != nil {
		fail(err)
	}
	fmt.Printf("serving %s on %s, files in %s\n", t.Name(), srv.Addr(), *dir)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	_ = srv.Close()
}

func push(args []string) {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	to := fs.String("to", "127.0.0.1:7950", "receiver address")
	spec := fs.String("transport", "tcp", "transport")
	key := fs.String("key", "", "conversation key")
	file := fs.String("file", "", "session file to send")
	full := fs.Bool("full", false, "ignore the receiver's base and send the whole file")
	_ = fs.Parse(args)
	if *key == "" || *file == "" {
		fail(fmt.Errorf("-key and -file are required"))
	}
	t, err := kvshare.New(*spec)
	if err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	start := time.Now()
	var m *kvshare.Manifest
	if !*full {
		if m, err = t.Manifest(ctx, *to, *key); err != nil {
			fail(err)
		}
	}
	manifestMs := float64(time.Since(start).Microseconds()) / 1000
	res, err := kvshare.Push(ctx, t, *to, *key, *file, m)
	if err != nil {
		fail(err)
	}
	out := map[string]any{
		"transport":   t.Name(),
		"size":        res.Plan.Size,
		"data":        res.Plan.Data,
		"items":       len(res.Plan.Items),
		"had_base":    m != nil,
		"manifest_ms": manifestMs,
		"diff_ms":     res.DiffMs,
		"wire_ms":     res.WireMs,
		"apply_ms":    res.Put.ApplyMs,
		"total_ms":    manifestMs + res.TotalMs,
		"wire_MBps":   float64(res.Plan.Data) / 1e3 / max(res.WireMs, 0.001),
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

func plan(args []string) {
	if len(args) != 2 {
		fail(fmt.Errorf("plan OLD NEW"))
	}
	open := func(p string) (*os.File, *kvshare.Layout) {
		f, err := os.Open(p)
		if err != nil {
			fail(err)
		}
		st, err := f.Stat()
		if err != nil {
			fail(err)
		}
		l, err := kvshare.ParseLayout(f, st.Size())
		if err != nil {
			fail(err)
		}
		return f, l
	}
	of, ol := open(args[0])
	nf, nl := open(args[1])
	start := time.Now()
	m, err := kvshare.FileManifest(of, ol)
	if err != nil {
		fail(err)
	}
	manifestMs := float64(time.Since(start).Microseconds()) / 1000
	start = time.Now()
	p, err := kvshare.Diff(nf, nl, m)
	if err != nil {
		fail(err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"size": p.Size, "data": p.Data, "items": len(p.Items),
		"ratio": float64(p.Data) / float64(p.Size), "manifest_ms": manifestMs,
		"diff_ms": float64(time.Since(start).Microseconds()) / 1000,
	})
}

func materialize(args []string) {
	fs := flag.NewFlagSet("materialize", flag.ExitOnError)
	dir := fs.String("dir", "", "the receiver's directory")
	key := fs.String("key", "", "conversation key")
	out := fs.String("out", "", "session file to write")
	_ = fs.Parse(args)
	if *dir == "" || *key == "" || *out == "" {
		fail(fmt.Errorf("-dir, -key and -out are required"))
	}
	res, err := kvshare.NewStore(*dir).Materialize(*key, *out)
	if err != nil {
		fail(err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(res)
}
