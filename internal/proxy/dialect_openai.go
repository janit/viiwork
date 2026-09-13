package proxy

import (
	"encoding/json"
	"net/http"

	"github.com/janit/viiwork/v2/internal/api"
	"github.com/janit/viiwork/v2/meshapi"
)

// The native dialect. It lives in this package rather than in internal/api
// because it is the only one allowed to be free: it reuses the extraction this
// package already does, so the seam costs the hot path nothing — the same
// single parse, the same fast path, the same bytes reaching the engine.
//
// A second dialect would live in its own package, import internal/api, and be
// blank-imported by the node. There is no second dialect.
type openAIDialect struct{}

func init() { api.Register(openAIDialect{}) }

func (openAIDialect) Name() string { return "openai" }

func (openAIDialect) Paths() []string {
	return []string{meshapi.PathChatCompletions, meshapi.PathCompletions, meshapi.PathEmbeddings}
}

// Decode reads the model, the think extension and the task tag, and strips the
// task field from the body so no engine ever sees it.
//
// The fast path matters: extractModelFast scans for the model without decoding
// the whole document, and proves think and task absent while doing it, so the
// common request never pays a full json.Unmarshal over a prompt that can be
// megabytes. Only a request that actually carries one of the extensions falls
// through to the slow path.
func (openAIDialect) Decode(r *http.Request, body []byte) (api.Request, bool, error) {
	var fields struct {
		Model string `json:"model"`
		Think *bool  `json:"think"`
		Task  string `json:"task"`
	}
	if model, ok := extractModelFast(body); ok {
		// The fast path proved think and task absent, so the zero values are right.
		fields.Model = model
	} else {
		json.Unmarshal(body, &fields)
	}

	// The body's task wins over the header. Engines never see the field, and
	// peers get the task as the header.
	taskID := sanitizeTaskID(fields.Task)
	if taskID == "" {
		taskID = sanitizeTaskID(r.Header.Get(HeaderTask))
	}
	if fields.Task != "" {
		var generic map[string]json.RawMessage
		if err := json.Unmarshal(body, &generic); err == nil {
			if _, present := generic["task"]; present {
				delete(generic, "task")
				if rewritten, err := json.Marshal(generic); err == nil {
					body = rewritten
				}
			}
		}
	}
	return api.Request{Model: fields.Model, Body: body, Task: taskID, Think: fields.Think}, true, nil
}
