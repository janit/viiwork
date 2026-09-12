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
