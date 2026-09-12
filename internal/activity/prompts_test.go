package activity

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestPromptStoreCapAndEviction(t *testing.T) {
	p := NewPromptStore(3)
	if p.Max() != 3 {
		t.Errorf("Max = %d, want 3", p.Max())
	}
	for i := int64(1); i <= 5; i++ {
		p.Store(i, i*10, "m", "prompt")
	}
	// Oldest evicted, newest kept: the store is a ring, not a log.
	for _, rid := range []int64{1, 2} {
		if _, ok := p.Get(rid); ok {
			t.Errorf("rid %d should have been evicted", rid)
		}
	}
	for _, rid := range []int64{3, 4, 5} {
		if _, ok := p.Get(rid); !ok {
			t.Errorf("rid %d should still be held", rid)
		}
	}
}

func TestPromptStoreDefaultsOnBadMax(t *testing.T) {
	// A misconfigured cap must degrade to the default, not to a store that
	// silently drops everything written to it.
	for _, max := range []int{0, -1, -1000} {
		if got := NewPromptStore(max).Max(); got != DefaultPromptHistory {
			t.Errorf("NewPromptStore(%d).Max() = %d, want %d", max, got, DefaultPromptHistory)
		}
	}
}

func TestPromptStoreDropsEmpty(t *testing.T) {
	p := NewPromptStore(10)
	p.Store(1, 0, "m", "")
	if _, ok := p.Get(1); ok {
		t.Error("an empty prompt must not create an entry: it would show a link that opens to nothing")
	}
	p.StoreOutput(2, 0, "m", "", 5)
	if _, ok := p.Get(2); ok {
		t.Error("an empty output must not create an entry")
	}
}

func TestPromptStoreOutputAttachesOrCreates(t *testing.T) {
	p := NewPromptStore(10)
	p.Store(1, 100, "m", "the prompt")
	p.StoreOutput(1, 101, "m", "the answer", 1234)
	e, ok := p.Get(1)
	if !ok || e.Prompt != "the prompt" || e.Output != "the answer" || e.ElapsedMS != 1234 {
		t.Fatalf("output should attach to the existing entry: %+v ok=%v", e, ok)
	}
	if e.Time != 100 {
		t.Errorf("attaching output must not rewrite the prompt's timestamp: %d", e.Time)
	}
	// A request whose prompt could not be extracted still has output worth keeping.
	p.StoreOutput(2, 200, "m", "orphan answer", 7)
	e2, ok := p.Get(2)
	if !ok || e2.Output != "orphan answer" || e2.Prompt != "" {
		t.Errorf("output without a prompt should create an entry: %+v ok=%v", e2, ok)
	}
}

func TestPromptStoreTruncation(t *testing.T) {
	p := NewPromptStore(10)
	exact := strings.Repeat("a", maxPromptChars)
	p.Store(1, 0, "m", exact)
	if e, _ := p.Get(1); e.Prompt != exact {
		t.Errorf("a prompt exactly at the cap must be kept whole, got %d chars", len(e.Prompt))
	}
	over := strings.Repeat("a", maxPromptChars+1)
	p.Store(2, 0, "m", over)
	e, _ := p.Get(2)
	if !strings.HasSuffix(e.Prompt, "... [truncated]") {
		t.Error("a prompt over the cap must be marked truncated")
	}
	if len(e.Prompt) > maxPromptChars+len("... [truncated]") {
		t.Errorf("truncated prompt is %d bytes, beyond cap+marker", len(e.Prompt))
	}
	p.StoreOutput(3, 0, "m", over, 1)
	if e3, _ := p.Get(3); !strings.HasSuffix(e3.Output, "... [truncated]") {
		t.Error("output is capped the same way as the prompt")
	}
}

// The cap is applied in bytes. A prompt in any non-ASCII script — which is the
// normal case on a translation fleet — can therefore be cut mid-character, and
// what is stored then has to survive being marshalled into /v1/prompts.
func TestPromptStoreTruncationIsValidUTF8(t *testing.T) {
	// "ä" is two bytes, so a run of them alone would be cut exactly on a
	// boundary. One ASCII byte in front shifts the cap to an odd offset, which
	// is what a real mixed-script prompt does anyway.
	body := "a" + strings.Repeat("ä", maxPromptChars)
	p := NewPromptStore(10)
	p.Store(1, 0, "m", body)
	e, _ := p.Get(1)
	if !utf8.ValidString(e.Prompt) {
		t.Error("truncation cut mid-character: the tail reaches /v1/prompts as invalid UTF-8")
	}
	if !strings.HasSuffix(e.Prompt, "... [truncated]") {
		t.Error("it should still be marked truncated")
	}
	// What survives must equal a prefix of what was sent, not a mangled one.
	kept := strings.TrimSuffix(e.Prompt, "... [truncated]")
	if !strings.HasPrefix(body, kept) {
		t.Error("the kept text is not a prefix of the original")
	}
	if b, err := json.Marshal(e); err != nil {
		t.Fatalf("marshalling for /v1/prompts: %v", err)
	} else if !utf8.Valid(b) {
		t.Error("the marshalled entry is not valid UTF-8")
	}
}

func TestPromptStoreConcurrent(t *testing.T) {
	p := NewPromptStore(50)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(base int64) {
			defer wg.Done()
			for j := int64(0); j < 200; j++ {
				rid := base*200 + j
				p.Store(rid, j, "m", "p")
				p.StoreOutput(rid, j, "m", "o", j)
				p.Get(rid)
			}
		}(int64(i))
	}
	wg.Wait()
	if got := len(p.entries); got > p.Max() {
		t.Errorf("store holds %d entries, above its cap of %d", got, p.Max())
	}
}
