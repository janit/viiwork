package web

import (
	"regexp"
	"strings"
	"testing"
)

// v1Reads are reads of v1's /v1/cluster shape (peers, local, listen_addr,
// total_in_flight, single_host, gpu_id). meshapi v2 has none of them, so a page
// still reading one renders blanks or breaks outright.
var v1Reads = regexp.MustCompile(`\.peers\b|\.local\b|listen_addr|total_in_flight|single_host|gpu_id`)

func TestPagesReadMeshapiV2(t *testing.T) {
	pages := map[string][]byte{
		"mesh.html":      MeshHTML,
		"dashboard.html": DashboardHTML,
		"chat.html":      ChatHTML,
		"prompt.html":    PromptHTML,
	}
	for name, page := range pages {
		if len(page) == 0 {
			t.Errorf("%s is empty", name)
			continue
		}
		for i, line := range strings.Split(string(page), "\n") {
			if v1Reads.MatchString(line) {
				t.Errorf("%s:%d %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// perEventHandlers are the mesh page's per-activity-event functions. Every
// event on the merged stream runs both of them, and a stream replays each
// member's whole ring when it opens — during a soak that is tens of thousands
// of events arriving back to back.
//
// Neither may render. Painting from here binds repaints to arrival rather than
// to time: the prompt list alone is a thousand rows, so a burst rebuilt it once
// per event and the page stopped painting altogether (measured: 4 frames in 30
// seconds, single tasks blocking the main thread for 79 s). The renderers are
// called from the drain tick instead, which folds a burst into one repaint —
// nothing is dropped, because the state maps these functions write are what the
// tick reads.
var perEventHandlers = []string{"applyActivity", "recordPrompt"}

var rendersFromHandler = regexp.MustCompile(`\brender(Prompts|Activity|Inflight)\s*\(`)

func TestMeshPerEventHandlersDoNotRender(t *testing.T) {
	src := string(MeshHTML)
	for _, name := range perEventHandlers {
		body, ok := jsFunc(src, name)
		if !ok {
			t.Errorf("mesh.html: function %s not found", name)
			continue
		}
		if m := rendersFromHandler.FindString(body); m != "" {
			t.Errorf("mesh.html: %s calls %s — per-event rendering; mark dirty and let the drain tick paint", name, m)
		}
	}
}

// jsFunc returns the body of a top-level `function name(...) { ... }`, matched
// by counting braces from the opening one.
func jsFunc(src, name string) (string, bool) {
	i := strings.Index(src, "function "+name+"(")
	if i < 0 {
		return "", false
	}
	open := strings.Index(src[i:], "{")
	if open < 0 {
		return "", false
	}
	depth, start := 0, i+open
	for j := start; j < len(src); j++ {
		switch src[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : j+1], true
			}
		}
	}
	return "", false
}
