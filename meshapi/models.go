package meshapi

// ModelEntry.OwnedBy values. local, peer and pipeline are the v1 values.
const (
	OwnedByLocal    = "local"
	OwnedByPeer     = "peer"
	OwnedByPipeline = "pipeline"
	OwnedByAlias    = "alias"
)

// ModelEntry is one OpenAI-compatible model list entry. Target is set only
// for aliases.
type ModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
	Target  string `json:"target,omitempty"`
}

// ModelsResponse is the body of GET PathModels.
type ModelsResponse struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}
