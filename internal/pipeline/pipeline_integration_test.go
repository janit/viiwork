//go:build integration

package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"text/template"
)

func TestFullPipelineFlow(t *testing.T) {
	// Track call order
	var callOrder []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string        `json:"model"`
			Messages []chatMessage `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		callOrder = append(callOrder, req.Model)

		w.Header().Set("Content-Type", "application/json")

		var content string
		switch req.Model {
		case "alma-r-13b":
			// Verify translate step has language in prompt
			if len(req.Messages) > 0 && !strings.Contains(req.Messages[0].Content, "Portuguese") {
				t.Errorf("translate step missing language in prompt")
			}
			content = "Quebre uma perna!"
		case "qwen2.5-14b":
			// Verify localize step has both original and translation
			if len(req.Messages) > 0 {
				sysPrompt := req.Messages[0].Content
				if !strings.Contains(sysPrompt, "Quebre uma perna!") {
					t.Error("localize step missing translation from step 1")
				}
			}
			content = "Boa sorte!"
		case "qwen2.5-7b":
			content = `{"back_translation":"Good luck!","meaning_drift":"none","naturalness":9,"glossary_violations":[]}`
		default:
			t.Errorf("unexpected model: %s", req.Model)
			content = "error"
		}

		resp := map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion",
			"choices": []map[string]any{{
				"index": 0, "message": map[string]string{"role": "assistant", "content": content}, "finish_reason": "stop",
			}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	translateTmpl := template.Must(template.New("translate").Parse("Translate from English to {{.Locale.Language}}.\nOriginal: {{.Original}}"))
	localizeTmpl := template.Must(template.New("localize").Parse("Localize for {{.Locale.Audience}}.\nOriginal: {{.Original}}\nTranslation: {{.Translation}}\n{{if .Glossary}}Glossary:\n{{.Glossary}}{{end}}"))
	qcTmpl := template.Must(template.New("qc").Parse("QC review.\nOriginal: {{.Original}}\nLocalized: {{.Translation}}"))

	p := &Pipeline{
		Name:          "localize",
		LocaleAliases: map[string]string{"pt": "pt-BR"},
		Locales: map[string]*LocaleConfig{
			"pt-BR": {
				Language: "Portuguese", Audience: "Brazilian general audience", Formality: "informal",
				Glossary: []GlossaryTerm{{Source: "break a leg", Target: "boa sorte", Note: "idiomatic"}},
			},
		},
		Steps: []Step{
			{Name: "translate", Model: "alma-r-13b", Template: translateTmpl, Temperature: 0.1},
			{Name: "localize", Model: "qwen2.5-14b", Template: localizeTmpl, Temperature: 0.4},
			{Name: "qc", Model: "qwen2.5-7b", Template: qcTmpl, Temperature: 0.1, JSONOutput: true},
		},
	}

	exec := NewExecutor(srv.URL, nil)
	result, err := exec.Run(context.Background(), p, p.Locales["pt-BR"], "Break a leg!")
	if err != nil {
		t.Fatal(err)
	}

	// Verify call order
	if len(callOrder) != 3 {
		t.Fatalf("expected 3 calls, got %d: %v", len(callOrder), callOrder)
	}
	if callOrder[0] != "alma-r-13b" || callOrder[1] != "qwen2.5-14b" || callOrder[2] != "qwen2.5-7b" {
		t.Errorf("wrong call order: %v", callOrder)
	}

	// Verify result
	if result.Content != "Boa sorte!" {
		t.Errorf("content = %q, want %q", result.Content, "Boa sorte!")
	}
	if result.QC == nil {
		t.Fatal("QC is nil")
	}
	if result.QC.BackTranslation != "Good luck!" {
		t.Errorf("back_translation = %q", result.QC.BackTranslation)
	}
	if result.QC.MeaningDrift != "none" {
		t.Errorf("drift = %q", result.QC.MeaningDrift)
	}
	if result.QC.Naturalness != 9 {
		t.Errorf("naturalness = %d", result.QC.Naturalness)
	}
	if len(result.StepTimings) != 3 {
		t.Errorf("expected 3 timings, got %d", len(result.StepTimings))
	}
}

func TestPipelineLocaleResolutionIntegration(t *testing.T) {
	p := &Pipeline{
		Name: "localize",
		LocaleAliases: map[string]string{
			"fi": "fi-FI", "pt": "pt-BR", "se": "sv-SE", "es": "es-MX", "fr": "fr-FR", "de": "de-DE",
		},
		Locales: map[string]*LocaleConfig{
			"pt-BR": {Language: "Portuguese", Audience: "Brazilian audience"},
			"pt-PT": {Language: "Portuguese", Audience: "Portuguese audience"},
			"fi-FI": {Language: "Finnish", Audience: "Finnish audience"},
			"sv-SE": {Language: "Swedish", Audience: "Swedish audience"},
			"es-MX": {Language: "Spanish", Audience: "Mexican audience"},
			"es-ES": {Language: "Spanish", Audience: "Spanish audience"},
			"fr-FR": {Language: "French", Audience: "French audience"},
			"de-DE": {Language: "German", Audience: "German audience"},
		},
	}

	tests := []struct {
		model    string
		wantKey  string
		wantLang string
	}{
		{"localize-pt", "pt-BR", "Portuguese"},
		{"localize-pt-pt", "pt-PT", "Portuguese"},
		{"localize-fi", "fi-FI", "Finnish"},
		{"localize-se", "sv-SE", "Swedish"},
		{"localize-es", "es-MX", "Spanish"},
		{"localize-es-es", "es-ES", "Spanish"},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			lc, key, ok := p.Resolve(tt.model)
			if !ok {
				t.Fatalf("Resolve(%q) failed", tt.model)
			}
			if key != tt.wantKey {
				t.Errorf("key = %q, want %q", key, tt.wantKey)
			}
			if lc.Language != tt.wantLang {
				t.Errorf("language = %q, want %q", lc.Language, tt.wantLang)
			}
		})
	}
}
