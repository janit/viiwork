package pipeline

import (
	"fmt"
	"os"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// Pipeline is the resolved, ready-to-use pipeline configuration.
type Pipeline struct {
	Name          string
	LocaleAliases map[string]string
	Locales       map[string]*LocaleConfig
	Steps         []Step
}

// LocaleConfig holds the resolved locale settings including loaded glossary.
type LocaleConfig struct {
	Language  string
	Audience  string
	Formality string
	Glossary  []GlossaryTerm
}

// GlossaryTerm is a source/target translation pair with optional note.
type GlossaryTerm struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
	Note   string `yaml:"note"`
}

// Step is a resolved pipeline step with a parsed template.
type Step struct {
	Name        string
	Model       string
	Template    *template.Template
	Temperature float64
	JSONOutput  bool
}

// StepConfig is the YAML-level step configuration before template parsing.
type StepConfig struct {
	Name        string  `yaml:"name"`
	Model       string  `yaml:"model"`
	Prompt      string  `yaml:"prompt"`
	Temperature float64 `yaml:"temperature"`
	JSONOutput  bool    `yaml:"json_output"`
}

// LocaleFileConfig is the YAML-level locale configuration.
type LocaleFileConfig struct {
	Language  string `yaml:"language"`
	Audience  string `yaml:"audience"`
	Formality string `yaml:"formality"`
	Glossary  string `yaml:"glossary"`
}

// PipelineConfig is the top-level YAML configuration for a pipeline.
type PipelineConfig struct {
	LocaleAliases map[string]string          `yaml:"locale_aliases"`
	Locales       map[string]LocaleFileConfig `yaml:"locales"`
	Steps         []StepConfig                `yaml:"steps"`
}

// NormalizeLocale normalizes a BCP 47 locale tag.
// "pt-br" -> "pt-BR", "fi" -> "fi", "PT-BR" -> "pt-BR".
func NormalizeLocale(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.SplitN(raw, "-", 2)
	parts[0] = strings.ToLower(parts[0])
	if len(parts) == 2 {
		parts[1] = strings.ToUpper(parts[1])
	}
	return strings.Join(parts, "-")
}

// Resolve strips the pipeline prefix from a model name, normalizes the locale
// suffix, and returns the matching LocaleConfig with the canonical locale code.
// Returns (nil, "", false) if the model doesn't match this pipeline.
func (p *Pipeline) Resolve(modelName string) (*LocaleConfig, string, bool) {
	prefix := p.Name + "-"
	if !strings.HasPrefix(modelName, prefix) {
		return nil, "", false
	}
	suffix := modelName[len(prefix):]
	if suffix == "" {
		return nil, "", false
	}

	normalized := NormalizeLocale(suffix)

	// Try direct locale match first.
	if lc, ok := p.Locales[normalized]; ok {
		return lc, normalized, true
	}

	// Try alias lookup: the suffix (normalized) may be a short alias.
	if full, ok := p.LocaleAliases[normalized]; ok {
		fullNorm := NormalizeLocale(full)
		if lc, ok := p.Locales[fullNorm]; ok {
			return lc, fullNorm, true
		}
	}

	return nil, "", false
}

// VirtualModels returns all model names this pipeline responds to,
// including both alias-based and full locale-based names.
func (p *Pipeline) VirtualModels() []string {
	var models []string
	for locale := range p.Locales {
		models = append(models, p.Name+"-"+strings.ToLower(locale))
	}
	for alias := range p.LocaleAliases {
		models = append(models, p.Name+"-"+alias)
	}
	return models
}

// glossaryFile is the YAML structure for a glossary file.
type glossaryFile struct {
	Terms []GlossaryTerm `yaml:"terms"`
}

// LoadGlossary reads and parses a YAML glossary file.
func LoadGlossary(path string) ([]GlossaryTerm, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read glossary %s: %w", path, err)
	}
	var gf glossaryFile
	if err := yaml.Unmarshal(data, &gf); err != nil {
		return nil, fmt.Errorf("parse glossary %s: %w", path, err)
	}
	return gf.Terms, nil
}

// LoadPipeline resolves a PipelineConfig into a ready-to-use Pipeline.
// It loads glossary files and parses prompt templates.
func LoadPipeline(name string, raw PipelineConfig) (*Pipeline, error) {
	p := &Pipeline{
		Name:          name,
		LocaleAliases: raw.LocaleAliases,
		Locales:       make(map[string]*LocaleConfig, len(raw.Locales)),
	}

	if p.LocaleAliases == nil {
		p.LocaleAliases = make(map[string]string)
	}

	for code, lfc := range raw.Locales {
		lc := &LocaleConfig{
			Language:  lfc.Language,
			Audience:  lfc.Audience,
			Formality: lfc.Formality,
		}
		if lfc.Glossary != "" {
			terms, err := LoadGlossary(lfc.Glossary)
			if err != nil {
				return nil, fmt.Errorf("locale %s: %w", code, err)
			}
			lc.Glossary = terms
		}
		p.Locales[code] = lc
	}

	p.Steps = make([]Step, len(raw.Steps))
	for i, sc := range raw.Steps {
		tmpl, err := template.ParseFiles(sc.Prompt)
		if err != nil {
			return nil, fmt.Errorf("step %q: parsing template %s: %w", sc.Name, sc.Prompt, err)
		}
		p.Steps[i] = Step{
			Name:        sc.Name,
			Model:       sc.Model,
			Template:    tmpl,
			Temperature: sc.Temperature,
			JSONOutput:  sc.JSONOutput,
		}
	}

	return p, nil
}
