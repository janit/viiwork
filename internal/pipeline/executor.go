package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Result is what the executor returns to the proxy.
type Result struct {
	Content     string       `json:"content"`
	QC          *QCMetadata  `json:"qc"`
	StepTimings []StepTiming `json:"-"`
}

// QCMetadata holds quality-check output from a JSONOutput step.
type QCMetadata struct {
	BackTranslation    string   `json:"back_translation"`
	MeaningDrift       string   `json:"meaning_drift"`
	Naturalness        int      `json:"naturalness"`
	GlossaryViolations []string `json:"glossary_violations"`
}

// StepTiming records wall-clock duration for one pipeline step.
type StepTiming struct {
	Name     string
	Duration time.Duration
}

// TemplateData is passed to each step's prompt template.
type TemplateData struct {
	Source      string
	Original    string
	Translation string
	Locale      *LocaleConfig
	Glossary    string
	Step        string
}

// StepError is returned when a pipeline step fails at the HTTP level.
type StepError struct {
	Step    string
	Status  int
	Message string
}

func (e *StepError) Error() string {
	return fmt.Sprintf("step %q: HTTP %d: %s", e.Step, e.Status, e.Message)
}

// chatRequest is the OpenAI-compatible request body.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`
}

// chatMessage is a single message in the chat conversation.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is the OpenAI-compatible response body.
type chatResponse struct {
	Choices []chatChoice `json:"choices"`
}

// chatChoice is a single choice in the response.
type chatChoice struct {
	Message chatMessage `json:"message"`
}

// Executor runs a localization pipeline by calling the LLM API for each step.
type Executor struct {
	baseURL string
	client  *http.Client
}

// NewExecutor creates an Executor that calls the given base URL.
func NewExecutor(baseURL string, client *http.Client) *Executor {
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	return &Executor{baseURL: baseURL, client: client}
}

// Run executes all steps in the pipeline sequentially.
func (e *Executor) Run(ctx context.Context, pipe *Pipeline, locale *LocaleConfig, sourceText string) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	glossary := renderGlossary(locale.Glossary)

	var (
		lastNonQCOutput string
		qc              *QCMetadata
		timings         []StepTiming
	)

	for i, step := range pipe.Steps {
		td := TemplateData{
			Source:      sourceText,
			Original:    sourceText,
			Translation: lastNonQCOutput,
			Locale:      locale,
			Glossary:    glossary,
			Step:        step.Name,
		}

		var buf bytes.Buffer
		if err := step.Template.Execute(&buf, td); err != nil {
			return nil, fmt.Errorf("step %q: template execute: %w", step.Name, err)
		}
		systemPrompt := buf.String()

		log.Printf("[debug] pipeline %s step %d/%d %q calling model %q (prompt_len=%d)", pipe.Name, i+1, len(pipe.Steps), step.Name, step.Model, len(systemPrompt))
		start := time.Now()
		output, err := e.callModel(ctx, step.Name, step.Model, systemPrompt, sourceText, step.Temperature)
		elapsed := time.Since(start)

		timings = append(timings, StepTiming{Name: step.Name, Duration: elapsed})

		if err != nil {
			log.Printf("[debug] pipeline %s step %q failed after %s: %v", pipe.Name, step.Name, elapsed.Round(time.Millisecond), err)
			return nil, err
		}
		log.Printf("[debug] pipeline %s step %q done in %s (output_len=%d)", pipe.Name, step.Name, elapsed.Round(time.Millisecond), len(output))

		if step.JSONOutput {
			var parsed QCMetadata
			if json.Unmarshal([]byte(output), &parsed) == nil {
				qc = &parsed
			}
			// Parse failure -> nil QC, not an error.
		} else {
			lastNonQCOutput = output
		}
	}

	return &Result{
		Content:     lastNonQCOutput,
		QC:          qc,
		StepTimings: timings,
	}, nil
}

// callModel sends a non-streaming chat completion request.
func (e *Executor) callModel(ctx context.Context, stepName, model, systemPrompt, userMessage string, temperature float64) (string, error) {
	reqBody := chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userMessage},
		},
		Temperature: temperature,
		Stream:      false,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("step %q: marshal request: %w", stepName, err)
	}

	url := strings.TrimRight(e.baseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("step %q: create request: %w", stepName, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("[debug] pipeline step %q: context cancelled: %v", stepName, ctx.Err())
		}
		return "", fmt.Errorf("step %q: HTTP call: %w", stepName, err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		log.Printf("[debug] pipeline step %q: body read error (status=%d): %v", stepName, resp.StatusCode, readErr)
	}

	if resp.StatusCode == http.StatusServiceUnavailable {
		log.Printf("[debug] pipeline step %q: 503 from backend: %s", stepName, string(body))
		return "", &StepError{Step: stepName, Status: 503, Message: string(body)}
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("[debug] pipeline step %q: HTTP %d from backend: %s", stepName, resp.StatusCode, string(body))
		return "", &StepError{Step: stepName, Status: resp.StatusCode, Message: string(body)}
	}

	var chatResp chatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return "", &StepError{Step: stepName, Status: resp.StatusCode, Message: fmt.Sprintf("parse response: %v", err)}
	}

	if len(chatResp.Choices) == 0 || chatResp.Choices[0].Message.Content == "" {
		return "", &StepError{Step: stepName, Status: resp.StatusCode, Message: "empty content in response"}
	}

	return chatResp.Choices[0].Message.Content, nil
}

// renderGlossary formats glossary terms as a readable list.
func renderGlossary(terms []GlossaryTerm) string {
	if len(terms) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, t := range terms {
		sb.WriteString("- \"")
		sb.WriteString(t.Source)
		sb.WriteString("\" → \"")
		sb.WriteString(t.Target)
		sb.WriteString("\"")
		if t.Note != "" {
			sb.WriteString(" (")
			sb.WriteString(t.Note)
			sb.WriteString(")")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
