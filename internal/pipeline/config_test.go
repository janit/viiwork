package pipeline

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"text/template"
)

func TestNormalizeLocale(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"pt-br", "pt-BR"},
		{"fi", "fi"},
		{"PT-BR", "pt-BR"},
		{"sv-SE", "sv-SE"},
		{"Pt-Br", "pt-BR"},
		{"en-us", "en-US"},
		{"EN", "en"},
		{"", ""},
	}
	for _, tc := range cases {
		got := NormalizeLocale(tc.in)
		if got != tc.want {
			t.Errorf("NormalizeLocale(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func testPipeline() *Pipeline {
	return &Pipeline{
		Name: "localize",
		LocaleAliases: map[string]string{
			"fi": "fi-FI",
			"sv": "sv-SE",
		},
		Locales: map[string]*LocaleConfig{
			"pt-BR": {Language: "Brazilian Portuguese", Audience: "general", Formality: "neutral"},
			"fi-FI": {Language: "Finnish", Audience: "general", Formality: "formal"},
			"sv-SE": {Language: "Swedish", Audience: "general", Formality: "neutral"},
		},
		Steps: []Step{
			{Name: "translate", Model: "qwen", Template: template.Must(template.New("t").Parse("translate to {{.Language}}")), Temperature: 0.3},
		},
	}
}

func TestResolveLocale(t *testing.T) {
	p := testPipeline()

	tests := []struct {
		name      string
		model     string
		wantCode  string
		wantMatch bool
	}{
		{"direct locale match", "localize-pt-br", "pt-BR", true},
		{"alias lookup", "localize-fi", "fi-FI", true},
		{"explicit full locale", "localize-fi-FI", "fi-FI", true},
		{"case insensitive", "localize-PT-BR", "pt-BR", true},
		{"alias sv", "localize-sv", "sv-SE", true},
		{"unknown locale", "localize-de-DE", "", false},
		{"non-pipeline model", "qwen", "", false},
		{"no suffix", "localize", "", false},
		{"just prefix dash", "localize-", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lc, code, ok := p.Resolve(tc.model)
			if ok != tc.wantMatch {
				t.Fatalf("Resolve(%q) ok = %v, want %v", tc.model, ok, tc.wantMatch)
			}
			if !ok {
				return
			}
			if code != tc.wantCode {
				t.Errorf("Resolve(%q) code = %q, want %q", tc.model, code, tc.wantCode)
			}
			if lc == nil {
				t.Fatalf("Resolve(%q) returned nil LocaleConfig", tc.model)
			}
		})
	}
}

func TestVirtualModels(t *testing.T) {
	p := testPipeline()
	models := p.VirtualModels()
	sort.Strings(models)

	// Should include alias names and full locale names
	want := []string{
		"localize-fi",
		"localize-fi-fi",
		"localize-pt-br",
		"localize-sv",
		"localize-sv-se",
	}
	sort.Strings(want)

	if len(models) != len(want) {
		t.Fatalf("VirtualModels() returned %d models, want %d: %v", len(models), len(want), models)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Errorf("VirtualModels()[%d] = %q, want %q", i, models[i], want[i])
		}
	}
}

func TestLoadGlossary(t *testing.T) {
	content := `terms:
  - source: "open source"
    target: "avoin lähdekoodi"
    note: "preferred Finnish translation"
  - source: "cloud"
    target: "pilvi"
  - source: "API"
    target: "rajapinta"
    note: "use Finnish term in user-facing text"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "glossary.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	terms, err := LoadGlossary(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(terms) != 3 {
		t.Fatalf("got %d terms, want 3", len(terms))
	}
	if terms[0].Source != "open source" || terms[0].Target != "avoin lähdekoodi" {
		t.Errorf("term[0] = %+v, want source=open source, target=avoin lähdekoodi", terms[0])
	}
	if terms[0].Note != "preferred Finnish translation" {
		t.Errorf("term[0].Note = %q, want %q", terms[0].Note, "preferred Finnish translation")
	}
	if terms[1].Note != "" {
		t.Errorf("term[1].Note = %q, want empty", terms[1].Note)
	}
	if terms[2].Source != "API" || terms[2].Target != "rajapinta" {
		t.Errorf("term[2] = %+v", terms[2])
	}
}

func TestLoadGlossaryMissing(t *testing.T) {
	_, err := LoadGlossary("/nonexistent/glossary.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadPipeline(t *testing.T) {
	// Create a glossary file for the test
	dir := t.TempDir()
	glossaryPath := filepath.Join(dir, "glossary.yaml")
	if err := os.WriteFile(glossaryPath, []byte("terms:\n  - source: test\n    target: testi\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create template files
	tmpl1Path := filepath.Join(dir, "translate.tmpl")
	if err := os.WriteFile(tmpl1Path, []byte("Translate to {{.Locale.Language}}"), 0644); err != nil {
		t.Fatal(err)
	}
	tmpl2Path := filepath.Join(dir, "review.tmpl")
	if err := os.WriteFile(tmpl2Path, []byte("Review: {{.Translation}}"), 0644); err != nil {
		t.Fatal(err)
	}

	raw := PipelineConfig{
		LocaleAliases: map[string]string{"fi": "fi-FI"},
		Locales: map[string]LocaleFileConfig{
			"fi-FI": {
				Language:  "Finnish",
				Audience:  "general",
				Formality: "formal",
				Glossary:  glossaryPath,
			},
			"sv-SE": {
				Language:  "Swedish",
				Audience:  "general",
				Formality: "neutral",
			},
		},
		Steps: []StepConfig{
			{Name: "translate", Model: "qwen", Prompt: tmpl1Path, Temperature: 0.3},
			{Name: "review", Model: "qwen", Prompt: tmpl2Path, Temperature: 0.1, JSONOutput: true},
		},
	}

	p, err := LoadPipeline("localize", raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "localize" {
		t.Errorf("Name = %q, want %q", p.Name, "localize")
	}
	if len(p.Locales) != 2 {
		t.Fatalf("got %d locales, want 2", len(p.Locales))
	}
	if p.Locales["fi-FI"].Language != "Finnish" {
		t.Errorf("fi-FI language = %q", p.Locales["fi-FI"].Language)
	}
	if len(p.Locales["fi-FI"].Glossary) != 1 {
		t.Errorf("fi-FI glossary len = %d, want 1", len(p.Locales["fi-FI"].Glossary))
	}
	if p.Locales["sv-SE"].Glossary != nil {
		t.Errorf("sv-SE glossary should be nil, got %v", p.Locales["sv-SE"].Glossary)
	}
	if len(p.Steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(p.Steps))
	}
	if p.Steps[0].Name != "translate" {
		t.Errorf("step[0].Name = %q", p.Steps[0].Name)
	}
	if p.Steps[1].JSONOutput != true {
		t.Errorf("step[1].JSONOutput = %v, want true", p.Steps[1].JSONOutput)
	}
	if p.LocaleAliases["fi"] != "fi-FI" {
		t.Errorf("alias fi = %q, want fi-FI", p.LocaleAliases["fi"])
	}
}

func TestLoadPipelineBadTemplate(t *testing.T) {
	dir := t.TempDir()
	badPath := filepath.Join(dir, "bad.tmpl")
	if err := os.WriteFile(badPath, []byte("{{.Broken"), 0644); err != nil {
		t.Fatal(err)
	}

	raw := PipelineConfig{
		Locales: map[string]LocaleFileConfig{
			"fi-FI": {Language: "Finnish"},
		},
		Steps: []StepConfig{
			{Name: "bad", Model: "qwen", Prompt: badPath, Temperature: 0.3},
		},
	}
	_, err := LoadPipeline("test", raw)
	if err == nil {
		t.Fatal("expected error for bad template")
	}
}

func TestLoadPipelineMissingTemplate(t *testing.T) {
	raw := PipelineConfig{
		Locales: map[string]LocaleFileConfig{
			"fi-FI": {Language: "Finnish"},
		},
		Steps: []StepConfig{
			{Name: "bad", Model: "qwen", Prompt: "/nonexistent/template.tmpl"},
		},
	}
	_, err := LoadPipeline("test", raw)
	if err == nil {
		t.Fatal("expected error for missing template file")
	}
}
