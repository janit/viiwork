package logging

import (
	"bytes"
	"io"
	"sync"
)

type PrefixWriter struct {
	out    io.Writer
	prefix []byte
	mu     sync.Mutex
	buf    bytes.Buffer
}

func NewPrefixWriter(out io.Writer, prefix string) *PrefixWriter {
	return &PrefixWriter{out: out, prefix: []byte(prefix)}
}

func (w *PrefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	total := len(p)
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadBytes('\n')
		if err != nil {
			w.buf.Write(line)
			break
		}
		w.out.Write(w.prefix)
		w.out.Write(line)
	}
	return total, nil
}
