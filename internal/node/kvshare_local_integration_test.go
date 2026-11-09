//go:build integration

package node

// kv_share.local on one node with the fake llama-server: three sessions on
// two slots, so every turn after the second takes another session's slot.
// The displaced session is saved before its slot is reused and restored
// from the node's own copy when it comes back.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh/meshtest"
)

func llamaLocalModel(t *testing.T, dir, saves, log string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "  - name: q\n    engine: llamacpp\n    path: %q\n    context: 4096\n    parallel: 2\n", writeWeights(t, dir, 1))
	fmt.Fprintf(&b, "    llamacpp:\n      binary: %q\n      threads: 1\n", os.Args[0])
	fmt.Fprintf(&b, "      kv_share:\n        local: true\n        saves: %s\n        min_tokens: 1000\n        save_dir: %q\n", saves, filepath.Join(dir, "slots"))
	b.WriteString("    env:\n      NODE_HELPER: llama-server\n")
	fmt.Fprintf(&b, "      FAKE_PROMPT_TOKENS: \"2000\"\n      FAKE_LOG: %q\n", log)
	return b.String()
}

func TestLlamaKVShareLocalRestoresADisplacedSession(t *testing.T) {
	for _, saves := range []string{"turn", "lazy"} {
		t.Run(saves, func(t *testing.T) {
			net := meshtest.NewNetwork()
			dir := t.TempDir()
			logA := filepath.Join(dir, "log")
			a := startNodeYAML(t, net, "A", []string{llamaLocalModel(t, dir, saves, logA)}, "", seeded(net, "A"))
			waitForModels(t, a, "q")

			for _, s := range []string{"s1", "s2", "s3"} {
				if got := sessionTurn(t, a, "q", s, "A"); got != "A" {
					t.Fatalf("%s ran on %s", s, got)
				}
			}
			// s3 took slot 0 from s1: s1 was saved before s3's turn ran.
			l := linesWith(logA, "chat", "save", "restore")
			i := index(l, "chat slot=0 sess=s3")
			if i < 0 || index(l[:i], "save 0 2000") < 0 {
				t.Fatalf("engine saw %q; want slot 0 saved before s3's turn there", l)
			}
			// s1 comes back and is given slot 1 (s2's, least recently used):
			// its own copy is restored there before its turn.
			if got := sessionTurn(t, a, "q", "s1", "A"); got != "A" {
				t.Fatalf("s1 ran on %s", got)
			}
			until(t, 5*time.Second, "s1's turn", func() bool { return index(linesWith(logA, "chat"), "chat slot=1 sess=s1") >= 0 })
			l = linesWith(logA, "chat", "save", "restore")
			j := index(l, "chat slot=1 sess=s1")
			if j < 1 || l[j-1] != "restore 1 2000" {
				t.Fatalf("engine saw %q; want s1 restored into slot 1 before its turn", l)
			}
			if saves == "lazy" {
				// Saves are only those of a slot about to be reused.
				if n := len(linesWith(logA, "save")); n != 2 {
					t.Fatalf("lazy: %d saves, want 2: %q", n, linesWith(logA, "save"))
				}
			}
		})
	}
}

func index(l []string, s string) int {
	for i, x := range l {
		if x == s {
			return i
		}
	}
	return -1
}
