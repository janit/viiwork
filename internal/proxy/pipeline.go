package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/internal/activity"
	"github.com/janit/viiwork/v2/internal/pipeline"
	"github.com/janit/viiwork/v2/meshapi"
)

// Ported from the v1 proxy's pipeline.go. Model entries are meshapi's, the
// handler is v2's, and writeJSON lives in handler.go; the behaviour, headers and
// response shape are unchanged.

// PipelineResolver resolves model names to pipelines.
type PipelineResolver struct {
	pipelines []*pipeline.Pipeline
}

// NewPipelineResolver creates a resolver and validates that no pipeline step
// references a virtual model name (which would cause infinite recursion).
func NewPipelineResolver(pipelines []*pipeline.Pipeline) *PipelineResolver {
	pr := &PipelineResolver{pipelines: pipelines}
	// Build set of all virtual model names for cycle detection
	virtualNames := make(map[string]bool)
	for _, p := range pipelines {
		for _, name := range p.VirtualModels() {
			virtualNames[name] = true
		}
	}
	// Check that no pipeline step references a virtual model
	for _, p := range pipelines {
		for _, step := range p.Steps {
			if virtualNames[step.Model] {
				log.Fatalf("pipeline %q step %q references virtual model %q — this would cause infinite recursion", p.Name, step.Name, step.Model)
			}
		}
	}
	return pr
}

// Resolve checks if a model name matches any pipeline.
// Returns pipeline, locale config, canonical locale key, and match status.
func (pr *PipelineResolver) Resolve(modelName string) (*pipeline.Pipeline, *pipeline.LocaleConfig, string, bool) {
	for _, p := range pr.pipelines {
		if lc, key, ok := p.Resolve(modelName); ok {
			return p, lc, key, true
		}
	}
	return nil, nil, "", false
}

// VirtualModels returns all virtual model entries from all pipelines.
func (pr *PipelineResolver) VirtualModels() []meshapi.ModelEntry {
	var entries []meshapi.ModelEntry
	for _, p := range pr.pipelines {
		for _, name := range p.VirtualModels() {
			entries = append(entries, meshapi.ModelEntry{ID: name, Object: "model", OwnedBy: meshapi.OwnedByPipeline})
		}
	}
	return entries
}

// VirtualModelNames returns just the model name strings.
func (pr *PipelineResolver) VirtualModelNames() []string {
	var names []string
	for _, e := range pr.VirtualModels() {
		names = append(names, e.ID)
	}
	return names
}

// AvailableLocales returns all valid locale suffixes for a pipeline (for error messages).
func (pr *PipelineResolver) AvailableLocales(pipelineName string) []string {
	for _, p := range pr.pipelines {
		if p.Name == pipelineName {
			var locales []string
			for alias := range p.LocaleAliases {
				locales = append(locales, alias)
			}
			for key := range p.Locales {
				locales = append(locales, strings.ToLower(key))
			}
			return locales
		}
	}
	return nil
}

// MatchesPipelinePrefix checks if a model name starts with any pipeline prefix.
func (pr *PipelineResolver) MatchesPipelinePrefix(modelName string) (string, bool) {
	for _, p := range pr.pipelines {
		if strings.HasPrefix(modelName, p.Name+"-") {
			return p.Name, true
		}
	}
	return "", false
}

// pipelineSourceText is the last user message, the text a pipeline processes.
func pipelineSourceText(body []byte) string {
	var fullReq struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(body, &fullReq)
	for i := len(fullReq.Messages) - 1; i >= 0; i-- {
		if fullReq.Messages[i].Role == "user" {
			return fullReq.Messages[i].Content
		}
	}
	return ""
}

func (h *Handler) handlePipeline(w http.ResponseWriter, r *http.Request, p *pipeline.Pipeline, locale *pipeline.LocaleConfig, localeKey string, sourceText string, modelName string, taskID string) {
	rid := activity.NewRequestID()
	if h.d.Activity != nil {
		h.d.Activity.EmitRequestTask(rid, -1, taskID, "[pipeline] %s started", modelName)
		h.d.Activity.StorePrompt(rid, modelName, sourceText)
	}

	start := time.Now()
	result, err := h.d.PipelineExec.Run(r.Context(), p, locale, sourceText)
	if err != nil {
		if h.d.Activity != nil {
			h.d.Activity.EmitRequestTask(rid, -1, taskID, "[pipeline] %s failed: %v", modelName, err)
		}
		if stepErr, ok := err.(*pipeline.StepError); ok && stepErr.Status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "5")
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": map[string]string{
					"message": fmt.Sprintf("pipeline step '%s' failed: model '%s' unavailable", stepErr.Step, stepErr.Step),
					"type":    "server_error",
				},
			})
			return
		}
		log.Printf("[pipeline] %s error: %v", modelName, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{
				"message": "pipeline processing failed",
				"type":    "server_error",
			},
		})
		return
	}

	elapsed := time.Since(start)
	if h.d.Activity != nil {
		var parts []string
		for _, st := range result.StepTimings {
			parts = append(parts, fmt.Sprintf("%s:%s", st.Name, st.Duration.Round(time.Millisecond)))
		}
		h.d.Activity.EmitRequestTask(rid, -1, taskID, "[pipeline] %s done (%s) [%s]", modelName, elapsed.Round(time.Millisecond), strings.Join(parts, ","))
		// A pipeline assembles its own response below rather than proxying one,
		// so there is no stream to capture — the final text is already in hand.
		h.d.Activity.StoreOutput(rid, modelName, result.Content, elapsed.Milliseconds())
	}

	// Pipeline headers
	w.Header().Set("X-Pipeline", p.Name)
	w.Header().Set("X-Pipeline-Locale", localeKey)
	var timingParts []string
	for _, st := range result.StepTimings {
		timingParts = append(timingParts, fmt.Sprintf("%s:%dms", st.Name, st.Duration.Milliseconds()))
	}
	w.Header().Set("X-Pipeline-Steps", strings.Join(timingParts, ","))

	// OpenAI-compatible response with extra QC field
	resp := map[string]any{
		"id":     fmt.Sprintf("chatcmpl-pipeline-%d", time.Now().UnixMilli()),
		"object": "chat.completion",
		"model":  modelName,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": result.Content},
			"finish_reason": "stop",
		}},
		"qc": result.QC,
		"usage": map[string]int{
			"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}
