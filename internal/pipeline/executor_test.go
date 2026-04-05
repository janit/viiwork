package pipeline

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"text/template"
)

func TestExecutorRun(t *testing.T) {
	// Mock server that returns canned responses based on model name.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req chatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("failed to parse request: %v", err)
			http.Error(w, "bad request", 400)
			return
		}

		var content string
		switch req.Model {
		case "translate-model":
			content = "Boa sorte!"
		case "qc-model":
			content = `{"back_translation":"Good luck!","meaning_drift":"none","naturalness":9,"glossary_violations":[]}`
		default:
			content = "unknown model"
		}

		resp := chatResponse{
			Choices: []chatChoice{
				{Message: chatMessage{Content: content}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	pipe := &Pipeline{
		Name: "localize",
		Steps: []Step{
			{
				Name:        "translate",
				Model:       "translate-model",
				Template:    template.Must(template.New("t").Parse("Translate the following to {{.Locale.Language}}. Use {{.Locale.Formality}} register.\n{{.Glossary}}")),
				Temperature: 0.3,
			},
			{
				Name:        "qc",
				Model:       "qc-model",
				Template:    template.Must(template.New("qc").Parse("Evaluate this translation. Respond in JSON.")),
				Temperature: 0.1,
				JSONOutput:  true,
			},
		},
	}

	locale := &LocaleConfig{
		Language:  "Brazilian Portuguese",
		Audience:  "general",
		Formality: "neutral",
		Glossary: []GlossaryTerm{
			{Source: "luck", Target: "sorte", Note: "common usage"},
		},
	}

	exec := NewExecutor(srv.URL, srv.Client())
	result, err := exec.Run(nil, pipe, locale, "Good luck!")
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if result.Content != "Boa sorte!" {
		t.Errorf("Content = %q, want %q", result.Content, "Boa sorte!")
	}
	if result.QC == nil {
		t.Fatal("QC is nil, expected parsed QCMetadata")
	}
	if result.QC.MeaningDrift != "none" {
		t.Errorf("QC.MeaningDrift = %q, want %q", result.QC.MeaningDrift, "none")
	}
	if result.QC.Naturalness != 9 {
		t.Errorf("QC.Naturalness = %d, want 9", result.QC.Naturalness)
	}
	if result.QC.BackTranslation != "Good luck!" {
		t.Errorf("QC.BackTranslation = %q, want %q", result.QC.BackTranslation, "Good luck!")
	}
	if len(result.StepTimings) != 2 {
		t.Errorf("StepTimings length = %d, want 2", len(result.StepTimings))
	}
	if result.StepTimings[0].Name != "translate" {
		t.Errorf("StepTimings[0].Name = %q, want %q", result.StepTimings[0].Name, "translate")
	}
	if result.StepTimings[0].Duration <= 0 {
		t.Errorf("StepTimings[0].Duration = %v, want > 0", result.StepTimings[0].Duration)
	}
}

func TestExecutorQCParseFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req chatRequest
		json.Unmarshal(body, &req)

		var content string
		switch req.Model {
		case "translate-model":
			content = "Boa sorte!"
		case "qc-model":
			content = "this is not valid JSON at all"
		}

		resp := chatResponse{
			Choices: []chatChoice{
				{Message: chatMessage{Content: content}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	pipe := &Pipeline{
		Name: "localize",
		Steps: []Step{
			{
				Name:        "translate",
				Model:       "translate-model",
				Template:    template.Must(template.New("t").Parse("Translate.")),
				Temperature: 0.3,
			},
			{
				Name:        "qc",
				Model:       "qc-model",
				Template:    template.Must(template.New("qc").Parse("Evaluate.")),
				Temperature: 0.1,
				JSONOutput:  true,
			},
		},
	}

	locale := &LocaleConfig{Language: "Portuguese"}

	exec := NewExecutor(srv.URL, srv.Client())
	result, err := exec.Run(nil, pipe, locale, "Good luck!")
	if err != nil {
		t.Fatalf("Run() error: %v, expected nil (QC parse failure is not an error)", err)
	}
	if result.Content != "Boa sorte!" {
		t.Errorf("Content = %q, want %q", result.Content, "Boa sorte!")
	}
	if result.QC != nil {
		t.Errorf("QC = %+v, want nil (invalid JSON should yield nil QC)", result.QC)
	}
}

func TestExecutorModelUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	pipe := &Pipeline{
		Name: "localize",
		Steps: []Step{
			{
				Name:        "translate",
				Model:       "translate-model",
				Template:    template.Must(template.New("t").Parse("Translate.")),
				Temperature: 0.3,
			},
		},
	}

	locale := &LocaleConfig{Language: "Portuguese"}

	exec := NewExecutor(srv.URL, srv.Client())
	_, err := exec.Run(nil, pipe, locale, "Good luck!")
	if err == nil {
		t.Fatal("Run() error = nil, expected error for 503")
	}

	stepErr, ok := err.(*StepError)
	if !ok {
		t.Fatalf("error type = %T, want *StepError", err)
	}
	if stepErr.Status != 503 {
		t.Errorf("StepError.Status = %d, want 503", stepErr.Status)
	}
	if stepErr.Step != "translate" {
		t.Errorf("StepError.Step = %q, want %q", stepErr.Step, "translate")
	}
}

func TestRenderGlossary(t *testing.T) {
	terms := []GlossaryTerm{
		{Source: "open source", Target: "avoin lähdekoodi", Note: "preferred"},
		{Source: "cloud", Target: "pilvi"},
	}
	got := renderGlossary(terms)
	want := "- \"open source\" → \"avoin lähdekoodi\" (preferred)\n- \"cloud\" → \"pilvi\"\n"
	if got != want {
		t.Errorf("renderGlossary() =\n%q\nwant:\n%q", got, want)
	}
}

func TestRenderGlossaryEmpty(t *testing.T) {
	got := renderGlossary(nil)
	if got != "" {
		t.Errorf("renderGlossary(nil) = %q, want empty", got)
	}
}
