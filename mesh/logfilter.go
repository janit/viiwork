package mesh

import (
	"bytes"
	"io"
	"log"
	"strings"
	"sync"
	"time"
)

// repeatWindow is how long an identical log line stays suppressed.
const repeatWindow = 60 * time.Second

// logFilter sits between memberlist's logger (and the mesh's own lines) and
// the node's log. memberlist repeats the same warning for every gossip message
// from a rejected member, so unfiltered, one rogue node fills the log
// (Decision 14). It drops [DEBUG] lines unless debug is on, suppresses a line
// identical to one written in the last 60 s, and prefixes survivors "[mesh] ".
type logFilter struct {
	mu      sync.Mutex
	out     *log.Logger
	debug   bool
	now     func() time.Time
	partial bytes.Buffer
	lastAt  map[string]time.Time
}

func newLogFilter(out io.Writer, debug bool, now func() time.Time) *logFilter {
	return &logFilter{
		out:    log.New(out, "[mesh] ", log.LstdFlags),
		debug:  debug,
		now:    now,
		lastAt: map[string]time.Time{},
	}
}

// Write always reports the full input length, whether or not lines survive.
func (f *logFilter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.partial.Write(p)
	for {
		line, err := f.partial.ReadString('\n')
		if err != nil {
			f.partial.WriteString(line) // no newline yet: keep for the next write
			break
		}
		f.emitLocked(strings.TrimRight(line, "\r\n"))
	}
	return len(p), nil
}

func (f *logFilter) emitLocked(line string) {
	if line == "" || (!f.debug && strings.Contains(line, "[DEBUG]")) {
		return
	}
	now := f.now()
	if last, ok := f.lastAt[line]; ok && now.Sub(last) < repeatWindow {
		return
	}
	f.lastAt[line] = now
	if len(f.lastAt) > 1024 {
		for l, at := range f.lastAt {
			if now.Sub(at) >= repeatWindow {
				delete(f.lastAt, l)
			}
		}
	}
	f.out.Println(line)
}

// logf writes one of the mesh's own lines through the filter.
func (f *logFilter) logf(format string, args ...any) {
	var b strings.Builder
	log.New(&b, "", 0).Printf(format, args...)
	_, _ = f.Write([]byte(b.String()))
}
