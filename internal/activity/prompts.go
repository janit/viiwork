package activity

import "sync"

// DefaultPromptHistory is the number of requests a node keeps when nothing is
// configured. Memory is roughly this times maxPromptChars times two (prompt and
// output), so the default trades about 100 MB of worst-case headroom for a
// history deep enough to still contain a request someone asks about an hour
// later. Configurable via server-side config; see config.ActivityConfig.
const DefaultPromptHistory = 1000

// maxPromptChars keeps one pathological multi-megabyte prompt from dominating
// the store; it is truncated rather than kept whole. The same cap applies to
// captured output: a reasoning model answering at length can run far past any
// prompt, and neither is worth unbounded memory for a debugging panel.
const maxPromptChars = 50000

// PromptEntry is one captured request, keyed by RequestID: the prompt that
// went in and, once the request finishes, the text that came back.
type PromptEntry struct {
	RequestID int64  `json:"rid"`
	Time      int64  `json:"t"`
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	Output    string `json:"output,omitempty"`
	// Elapsed is the wall time of the request in milliseconds, recorded with
	// the output. Zero while the request is still running.
	ElapsedMS int64 `json:"elapsed_ms,omitempty"`
}

// PromptStore holds the last max request prompts in memory. It backs
// the mesh dashboard's per-request prompt modal — nothing here is persisted,
// and it does not survive a restart.
type PromptStore struct {
	mu      sync.Mutex
	entries []PromptEntry
	max     int
}

// NewPromptStore returns a store holding the most recent max requests. A max
// below 1 falls back to the default rather than producing a store that drops
// everything written to it — a misconfigured cap should degrade to the normal
// behaviour, not to a silently empty panel.
func NewPromptStore(max int) *PromptStore {
	if max < 1 {
		max = DefaultPromptHistory
	}
	return &PromptStore{max: max}
}

// Max reports the configured capacity, so it can be published to the dashboard
// rather than the browser hardcoding a second copy of the number.
func (p *PromptStore) Max() int { return p.max }

// Store records a prompt for rid. Empty prompts are dropped rather than kept
// as blank entries — some requests have none, and a stored blank would still
// show a (broken) link in the dashboard.
func (p *PromptStore) Store(rid int64, t int64, model, prompt string) {
	if prompt == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.append(PromptEntry{RequestID: rid, Time: t, Model: model, Prompt: truncate(prompt)})
}

// StoreOutput attaches the response text to an existing entry, or creates one
// if the prompt was never stored. The create case is not dead code: a request
// whose prompt could not be extracted (multimodal content parts, see
// proxy.extractPromptText) still produces output worth keeping, and dropping
// it would leave the dashboard showing a row that opens to nothing.
func (p *PromptStore) StoreOutput(rid int64, t int64, model, output string, elapsedMS int64) {
	if output == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.entries) - 1; i >= 0; i-- {
		if p.entries[i].RequestID == rid {
			p.entries[i].Output = truncate(output)
			p.entries[i].ElapsedMS = elapsedMS
			return
		}
	}
	p.append(PromptEntry{RequestID: rid, Time: t, Model: model, Output: truncate(output), ElapsedMS: elapsedMS})
}

// append adds an entry and trims the ring. Callers hold p.mu.
func (p *PromptStore) append(e PromptEntry) {
	p.entries = append(p.entries, e)
	if len(p.entries) > p.max {
		p.entries = p.entries[len(p.entries)-p.max:]
	}
}

func truncate(s string) string {
	if len(s) > maxPromptChars {
		return s[:maxPromptChars] + "... [truncated]"
	}
	return s
}

// Get looks up a prompt by request id. Request ids share activity.NewRequestID's
// per-process counter, not a cluster-wide namespace, so a lookup only makes
// sense scoped to the node that minted the id — see Handler.handleMeshPrompt.
func (p *PromptStore) Get(rid int64) (PromptEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.entries) - 1; i >= 0; i-- {
		if p.entries[i].RequestID == rid {
			return p.entries[i], true
		}
	}
	return PromptEntry{}, false
}
