package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/janit/viiwork/internal/activity"
	"github.com/janit/viiwork/internal/model"
	"github.com/janit/viiwork/internal/pipeline"
)

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
func (pr *PipelineResolver) VirtualModels() []model.ModelEntry {
	var entries []model.ModelEntry
	for _, p := range pr.pipelines {
		for _, name := range p.VirtualModels() {
			entries = append(entries, model.ModelEntry{ID: name, Object: "model", OwnedBy: "pipeline"})
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

func (h *Handler) SetPipelines(resolver *PipelineResolver, exec *pipeline.Executor) {
	h.pipelineResolver = resolver
	h.pipelineExecutor = exec
}

func (h *Handler) handlePipeline(w http.ResponseWriter, r *http.Request, p *pipeline.Pipeline, locale *pipeline.LocaleConfig, localeKey string, sourceText string, modelName string) {
	rid := activity.NewRequestID()
	if h.activity != nil {
		h.activity.EmitRequest(rid, -1, "[pipeline] %s started", modelName)
	}

	start := time.Now()
	result, err := h.pipelineExecutor.Run(r.Context(), p, locale, sourceText)
	if err != nil {
		if h.activity != nil {
			h.activity.EmitRequest(rid, -1, "[pipeline] %s failed: %v", modelName, err)
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
	if h.activity != nil {
		var parts []string
		for _, st := range result.StepTimings {
			parts = append(parts, fmt.Sprintf("%s:%s", st.Name, st.Duration.Round(time.Millisecond)))
		}
		h.activity.EmitRequest(rid, -1, "[pipeline] %s done (%s) [%s]", modelName, elapsed.Round(time.Millisecond), strings.Join(parts, ","))
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
