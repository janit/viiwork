package meshapi

// PromptEntry is one captured request as returned by PathPrompts and
// PathMeshPrompt: the prompt that went in and the text that came back.
//
// Two properties of this record are protocol, not implementation detail:
//
//   - **It is addressed by RequestID on the minting node.** Ids are a
//     per-process counter, so a lookup is only meaningful against the node
//     that produced the event. PathMeshPrompt takes an addr for exactly this
//     reason, and forwards only to an address already in the node's peer list
//     — without that check the endpoint is an SSRF primitive, since it fetches
//     what it is handed on a LAN that also carries IPMI.
//
//   - **Bodies are fetched on demand, never pushed.** Putting prompt and
//     output on the activity stream would add per-request payload for a panel
//     that is usually closed.
//
// Output is the text the client actually received, recorded after response
// rewriting rather than before. For a thinking model with reasoning enabled
// that includes the reasoning, labelled rather than merged into the answer —
// such a model puts everything in reasoning_content and leaves content empty,
// so dropping it would blank the output for exactly the requests worth reading.
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
