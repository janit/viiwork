//go:build integration

package node

// kv_share for llama.cpp over a real two-node mesh with the fake
// llama-server: the node names every request's slot (id_slot), saves the
// slot a session's turn ran in, and restores the replica into the slot a
// spilled turn was given, before the engine sees the turn.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh/meshtest"
)

// llamaKVModel is a kv_share llama.cpp model served by the fake. weights is
// the model file: both members must serve the same bytes, or their
// fingerprints differ and nothing is restored.
func llamaKVModel(t *testing.T, dir, name, weights string, promptTokens int, log, counter string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "  - name: %s\n    engine: llamacpp\n    path: %q\n    context: 4096\n    parallel: 2\n", name, weights)
	fmt.Fprintf(&b, "    llamacpp:\n      binary: %q\n      threads: 1\n", os.Args[0])
	fmt.Fprintf(&b, "      kv_share:\n        group: g\n        min_tokens: 1000\n        save_dir: %q\n", filepath.Join(dir, "slots"))
	b.WriteString("    env:\n      NODE_HELPER: llama-server\n")
	fmt.Fprintf(&b, "      FAKE_PROMPT_TOKENS: %q\n      FAKE_LOG: %q\n      FAKE_PROMPT_COUNTER: %q\n", strconv.Itoa(promptTokens), log, counter)
	return b.String()
}

func writeWeights(t *testing.T, dir string, seed byte) string {
	t.Helper()
	b := make([]byte, 32<<20)
	for i := range b {
		b[i] = byte(i>>8) ^ seed
	}
	p := filepath.Join(dir, "q.gguf")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func startLlamaPair(t *testing.T, seedB byte) (a, b *testNode, logA, logB string) {
	net := meshtest.NewNetwork()
	dirA, dirB := t.TempDir(), t.TempDir()
	logA, logB = filepath.Join(dirA, "log"), filepath.Join(dirB, "log")
	counter := filepath.Join(t.TempDir(), "turns") // the conversation grows 100 tokens a turn
	a = startNodeYAML(t, net, "A", []string{llamaKVModel(t, dirA, "q", writeWeights(t, dirA, 1), 2000, logA, counter)}, "", seeded(net, "A"))
	b = startNodeYAML(t, net, "B", []string{llamaKVModel(t, dirB, "q", writeWeights(t, dirB, seedB), 2000, logB, counter)}, "", seeded(net, "B", a))
	until(t, 10*time.Second, "A and B joined", allAlive(a, b))
	waitForModels(t, a, "q")
	waitForModels(t, b, "q")
	until(t, 10*time.Second, "each sees the other's group", func() bool {
		for _, tn := range []*testNode{a, b} {
			found := false
			for _, rep := range tn.capPoller.Reports() {
				for _, mc := range rep.Models {
					found = found || (rep.Node != tn.name && mc.KVShareGroup == "g" && mc.HealthyBackends > 0)
				}
			}
			if !found {
				return false
			}
		}
		return true
	})
	return a, b, logA, logB
}

// linesWith is the log lines that start with one of prefixes.
func linesWith(path string, prefixes ...string) []string {
	var out []string
	for _, l := range readLog(path) {
		for _, p := range prefixes {
			if strings.HasPrefix(l, p) {
				out = append(out, l)
				break
			}
		}
	}
	return out
}

func TestLlamaKVShareSpillRestoresIntoTheGivenSlot(t *testing.T) {
	a, b, logA, logB := startLlamaPair(t, 1)
	sess, _ := sessionFavouring(t, "A", "A", "B")

	if got := sessionTurn(t, a, "q", sess, "A"); got != "A" {
		t.Fatalf("the home turn ran on %s", got)
	}
	until(t, 15*time.Second, "A's turn reaches B", func() bool {
		return kvStats(t, b, "q").Puts == 1 && kvStats(t, a, "q").Pushes == 1
	})
	l := readLog(logA)
	if len(l) < 2 || l[0] != "chat slot=0 sess="+sess || l[1] != "save 0 2000" {
		t.Fatalf("A's engine saw %q; want the turn in slot 0, then slot 0 saved", l)
	}
	first := kvStats(t, a, "q").BytesSent

	// The session spills to B, entering through A: B restores the replica
	// into the slot the request was given, then serves it there.
	if got := sessionTurn(t, a, "q", sess, "B"); got != "B" {
		t.Fatalf("the spilled turn ran on %s", got)
	}
	l = readLog(logB)
	if len(l) < 2 || l[0] != "restore 0 2000" || l[1] != "chat slot=0 sess="+sess {
		t.Fatalf("B's engine saw %q; want the restore into slot 0 before the turn there", l)
	}
	if s := kvStats(t, b, "q"); s.Imports != 1 || s.ImportFails != 0 {
		t.Fatalf("B's stats %+v", s)
	}

	// B ran the newest turn and sends it home; A's next turn finds its own
	// slot but restores the newer replica into it first.
	until(t, 15*time.Second, "B's turn reaches A", func() bool { return kvStats(t, a, "q").Puts == 1 })
	if got := sessionTurn(t, a, "q", sess, "A"); got != "A" {
		t.Fatalf("the turn back home ran on %s", got)
	}
	l = linesWith(logA, "chat", "restore")
	if len(l) < 3 || l[1] != "restore 0 2100" || l[2] != "chat slot=0 sess="+sess {
		t.Fatalf("A's engine saw %q; want B's replica restored into slot 0 before the turn", l)
	}

	// A's next save is a delta against what B holds: the same conversation.
	until(t, 15*time.Second, "A's second push", func() bool { return kvStats(t, a, "q").Pushes == 2 })
	if second := kvStats(t, a, "q").BytesSent - first; second <= 0 || second >= first/4 {
		t.Fatalf("the second push sent %d bytes, the first %d", second, first)
	}
}

// A member serving another file under the same name refuses the replica and
// never restores one: its fingerprint differs.
func TestLlamaKVShareOtherWeightsNeverRestore(t *testing.T) {
	a, b, _, logB := startLlamaPair(t, 2)
	sess, _ := sessionFavouring(t, "A", "A", "B")
	sessionTurn(t, a, "q", sess, "A")
	// B refuses the replica of other weights before storing it.
	until(t, 15*time.Second, "B refuses A's turn", func() bool { return kvStats(t, a, "q").PushFailures >= 1 })
	if s := kvStats(t, b, "q"); s.Puts != 0 {
		t.Fatalf("B stored a replica of other weights: %+v", s)
	}
	if got := sessionTurn(t, a, "q", sess, "B"); got != "B" {
		t.Fatalf("the spilled turn ran on %s", got)
	}
	if l := linesWith(logB, "restore"); len(l) != 0 {
		t.Fatalf("B restored another model's replica: %q", l)
	}
	if l := readLog(logB); len(l) == 0 || l[0] != "chat slot=0 sess="+sess {
		t.Fatalf("B's engine saw %q; want the turn served cold", l)
	}
}
