package activity

import "sync"

// maxPrompts bounds prompt history to the most recent N requests, in memory
// only. Same ring-buffer-via-reslice idiom as Log.emit's event cap.
const maxPrompts = 100

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

// PromptStore holds the last maxPrompts request prompts in memory. It backs
// the mesh dashboard's per-request prompt modal — nothing here is persisted,
// and it does not survive a restart.
type PromptStore struct {
	mu      sync.Mutex
	entries []PromptEntry
}

func NewPromptStore() *PromptStore {
	return &PromptStore{}
}

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
	if len(p.entries) > maxPrompts {
		p.entries = p.entries[len(p.entries)-maxPrompts:]
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
