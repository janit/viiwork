package activity

import "sync"

// maxPrompts bounds prompt history to the most recent N requests, in memory
// only. Same ring-buffer-via-reslice idiom as Log.emit's event cap.
const maxPrompts = 100

// maxPromptChars keeps one pathological multi-megabyte prompt from dominating
// the store; it is truncated rather than kept whole.
const maxPromptChars = 50000

// PromptEntry is one captured request prompt, keyed by RequestID.
type PromptEntry struct {
	RequestID int64  `json:"rid"`
	Time      int64  `json:"t"`
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
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
	if len(prompt) > maxPromptChars {
		prompt = prompt[:maxPromptChars] + "... [truncated]"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = append(p.entries, PromptEntry{RequestID: rid, Time: t, Model: model, Prompt: prompt})
	if len(p.entries) > maxPrompts {
		p.entries = p.entries[len(p.entries)-maxPrompts:]
	}
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
