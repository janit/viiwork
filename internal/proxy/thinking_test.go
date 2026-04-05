package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/balancer"
)

func TestRewriteThinkResponse_WithThinkTags(t *testing.T) {
	input := `{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"<think>\nLet me think about this.\n</think>\n\nHello, world!"}}]}`
	result := rewriteThinkResponse([]byte(input))

	var data map[string]any
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices := data["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)

	if msg["content"] != "Hello, world!" {
		t.Errorf("expected 'Hello, world!', got %q", msg["content"])
	}
	if _, has := msg["reasoning_content"]; has {
		t.Error("expected reasoning_content to be removed")
	}
}

func TestRewriteThinkResponse_NoThinkTags(t *testing.T) {
	input := `{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"The answer is 42."}}]}`
	result := rewriteThinkResponse([]byte(input))

	var data map[string]any
	json.Unmarshal(result, &data)
	choices := data["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)

	if msg["content"] != "The answer is 42." {
		t.Errorf("expected 'The answer is 42.', got %q", msg["content"])
	}
	if _, has := msg["reasoning_content"]; has {
		t.Error("expected reasoning_content to be removed")
	}
}

func TestRewriteThinkResponse_ContentAlreadyPresent(t *testing.T) {
	input := `{"choices":[{"message":{"content":"existing","reasoning_content":"thinking"}}]}`
	result := rewriteThinkResponse([]byte(input))

	// Content preserved, reasoning_content stripped
	var data map[string]any
	json.Unmarshal(result, &data)
	choices := data["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)

	if msg["content"] != "existing" {
		t.Errorf("expected 'existing', got %q", msg["content"])
	}
	if _, has := msg["reasoning_content"]; has {
		t.Error("expected reasoning_content to be stripped when think is disabled")
	}
}

func TestRewriteThinkResponse_NoReasoningContent(t *testing.T) {
	input := `{"choices":[{"message":{"content":"hello"}}]}`
	result := rewriteThinkResponse([]byte(input))

	if string(result) != input {
		t.Errorf("expected no change, got %s", result)
	}
}

func TestRewriteThinkResponse_OnlyThinkBlock(t *testing.T) {
	// When everything is inside <think> tags, fall back to full text
	input := `{"choices":[{"message":{"content":"","reasoning_content":"<think>all thinking no answer</think>"}}]}`
	result := rewriteThinkResponse([]byte(input))

	var data map[string]any
	json.Unmarshal(result, &data)
	choices := data["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)

	content := msg["content"].(string)
	if content == "" {
		t.Error("expected non-empty content as fallback")
	}
}

func TestLooksLikeReasoning(t *testing.T) {
	// Short content — never reasoning
	if looksLikeReasoning("Hello!") {
		t.Error("short content should not be reasoning")
	}

	// Clean prose — not reasoning
	clean := "It is just an 18 km drive, usually taking around 25 minutes. You will head out via Mannerheimintie and eventually merge onto Tuusulanväylä. The trip begins with slow urban driving through the city. When you arrive in Vantaa, you will find yourself near the Jumbo shopping mall."
	if looksLikeReasoning(clean) {
		t.Error("clean prose should not be detected as reasoning")
	}

	// Reasoning artifacts
	reasoning := `*   Origin: Helsinki
*   Destination: Vantaa
*   Distance: 18 km
*   Duration: 25 minutes
*   Primary road: Mannerheimintie

    *   Length: 1-2 paragraphs, 100-150 words.
    *   Style: Local driver, casual, direct.
    *   Content:
        *   Start with distance/duration/road name.
        *   Mention roads, junctions, airports.

    *   *Drafting the route description:*
        It is an 18 km drive that usually takes about 25 minutes.

    *   *Refining and adding destination fact:*
        Vantaa is where Helsinki Airport is located.`
	if !looksLikeReasoning(reasoning) {
		t.Error("bullet-heavy reasoning should be detected")
	}
}

func TestExtractCleanContent(t *testing.T) {
	input := `*   Origin: Helsinki
*   Destination: Vantaa
*   Distance: 18 km

    *   *Drafting the route description:*

It is just an 18 km drive, usually taking around 25 minutes if you avoid peak hours. You will head out via Mannerheimintie and eventually merge onto Tuusulanväylä. The trip begins with slow urban stop-and-go traffic through the city.

    *   *Word count check:* 45 words.
    *   *Banned words check:* None used.

When you arrive in Vantaa, you will find yourself near the Jumbo shopping mall, which is one of the largest shopping centers in the country.`

	result := extractCleanContent(input)
	if !strings.Contains(result, "Jumbo shopping mall") {
		t.Errorf("expected final paragraph about Jumbo, got: %s", result)
	}
	if !strings.Contains(result, "18 km drive") {
		t.Errorf("expected main paragraph about the drive, got: %s", result)
	}
	if strings.Contains(result, "Word count") {
		t.Errorf("should not contain reasoning markers, got: %s", result)
	}
	if strings.Contains(result, "*   Origin") {
		t.Errorf("should not contain bullet metadata, got: %s", result)
	}
}

func TestExtractCleanContent_QuotedAnswer(t *testing.T) {
	input := `One detail: The prompt says "End with one concrete point."
"right next to the airport" is a fact.

Is "urban" okay? Yes.
Is "outskirts" okay? Yes.

"It is a quick 18 km drive that usually takes about 25 minutes via Mannerheimintie and Tuusulanväylä. You start with some urban stop-and-go traffic as you leave Helsinki. Once you merge onto the main highway, the drive becomes a much faster multi-lane run. When you reach Vantaa, you will be right next to the Helsinki-Vantaa Airport terminals."`

	result := extractCleanContent(input)
	if strings.Contains(result, "okay? Yes") {
		t.Errorf("should not contain self-checks, got: %s", result)
	}
	if !strings.Contains(result, "18 km drive") {
		t.Errorf("expected the quoted answer, got: %s", result)
	}
	if strings.HasPrefix(result, "\"") {
		t.Errorf("should not start with quote, got: %s", result)
	}
}

func TestLooksLikeReasoning_SelfChecks(t *testing.T) {
	input := `One detail: The prompt says "End with something."
"near the airport" is a fact.

Is "urban" okay? Yes.
Is "outskirts" okay? Yes.
The prompt says to end with a fact.

Let me refine this draft.
Wait, I should check the word count.

"It is a quick drive that takes about 25 minutes via Mannerheimintie. You start with urban traffic. Once you merge onto the highway, things speed up. When you reach Vantaa, you are near the airport terminals."`

	if !looksLikeReasoning(input) {
		t.Error("self-check style reasoning should be detected")
	}
}

func TestRewriteThinkResponse_ReasoningInContent(t *testing.T) {
	// Simulates the Gemma 4 case: everything in content, no reasoning_content
	reasoning := `*   Origin: Helsinki
*   Destination: Vantaa
*   Distance: 18 km
*   Duration: 25 minutes

    *   Length: 1-2 paragraphs, 100-150 words.
    *   Style: Local driver, casual.
    *   *Drafting:*

It is an 18 km drive taking about 25 minutes. You start on Mannerheimintie and merge onto Tuusulanväylä. The trip begins with urban stop-and-go before opening into a fast motorway.

    *   *Word count:* 35 words.

When you reach Vantaa, you are near the Jumbo shopping mall, one of the largest in Finland.`

	input, _ := json.Marshal(map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"content": reasoning,
				},
			},
		},
	})
	result := rewriteThinkResponse(input)

	var data map[string]any
	json.Unmarshal(result, &data)
	choices := data["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	content := msg["content"].(string)

	if strings.Contains(content, "Word count") {
		t.Errorf("reasoning artifacts should be stripped, got: %s", content)
	}
	if !strings.Contains(content, "Jumbo") {
		t.Errorf("expected clean answer with Jumbo, got: %s", content)
	}
}

func TestRewriteThinkResponse_InvalidJSON(t *testing.T) {
	input := `not json at all`
	result := rewriteThinkResponse([]byte(input))
	if string(result) != input {
		t.Error("expected unchanged output for invalid JSON")
	}
}

func TestStreamThinkDisabled_SuppressesThinkBlock(t *testing.T) {
	// Simulate SSE stream: <think> tokens, then answer tokens
	sse := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"<think>\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Let me think.\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"</think>\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Hello!\"}}]}\n\n" +
		"data: [DONE]\n\n"

	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	streamThinkDisabled(w, strings.NewReader(sse), func() {})

	body := w.Body.String()

	// Should NOT contain thinking text
	if strings.Contains(body, "Let me think") {
		t.Errorf("thinking tokens should be suppressed, got:\n%s", body)
	}

	// Should contain the answer as content
	if !strings.Contains(body, `"content":"Hello!"`) {
		t.Errorf("expected answer in content field, got:\n%s", body)
	}

	// Should contain DONE
	if !strings.Contains(body, "[DONE]") {
		t.Error("expected [DONE] marker")
	}
}

func TestStreamThinkDisabled_NoThinkTags(t *testing.T) {
	// Model doesn't use think tags — everything renamed to content
	sse := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"The answer\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\" is 42.\"}}]}\n\n" +
		"data: [DONE]\n\n"

	w := httptest.NewRecorder()
	streamThinkDisabled(w, strings.NewReader(sse), func() {})

	body := w.Body.String()
	if !strings.Contains(body, `"content":"The answer"`) {
		t.Errorf("expected reasoning_content renamed to content, got:\n%s", body)
	}
	if !strings.Contains(body, `"content":" is 42."`) {
		t.Errorf("expected second chunk renamed, got:\n%s", body)
	}
	if strings.Contains(body, "reasoning_content") {
		t.Error("reasoning_content should not appear in output")
	}
}

func TestStreamThinkDisabled_TextAfterCloseTag(t *testing.T) {
	// </think> and answer text in the same token
	sse := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"<think>\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking...\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"</think>\\nAnswer here\"}}]}\n\n" +
		"data: [DONE]\n\n"

	w := httptest.NewRecorder()
	streamThinkDisabled(w, strings.NewReader(sse), func() {})

	body := w.Body.String()
	if !strings.Contains(body, "Answer here") {
		t.Errorf("expected text after </think> to be emitted, got:\n%s", body)
	}
	if strings.Contains(body, "thinking...") {
		t.Error("thinking tokens should be suppressed")
	}
}

func TestStreamThinkDisabled_PassesThroughContentField(t *testing.T) {
	// If delta already has content (not reasoning_content), pass through
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"direct answer\"}}]}\n\n" +
		"data: [DONE]\n\n"

	w := httptest.NewRecorder()
	streamThinkDisabled(w, strings.NewReader(sse), func() {})

	body := w.Body.String()
	if !strings.Contains(body, "direct answer") {
		t.Errorf("expected content to pass through, got:\n%s", body)
	}
}

func TestStreamThinkDisabled_FinishReason(t *testing.T) {
	// finish_reason chunk with empty delta should pass through
	sse := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"answer\"}},{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	w := httptest.NewRecorder()
	streamThinkDisabled(w, strings.NewReader(sse), func() {})

	body := w.Body.String()
	if !strings.Contains(body, "stop") {
		t.Errorf("expected finish_reason to pass through, got:\n%s", body)
	}
}

// Integration test: full proxy with think:false and a mock backend
func TestProxyThinkFalseNonStreaming(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"<think>\nLet me reason.\n</think>\n\nThe answer is 42."}}]}`))
	}))
	defer backend.Close()

	state := balancer.NewBackendState(0, backend.Listener.Addr().String())
	state.SetStatus(balancer.StatusHealthy)
	bal := balancer.New([]*balancer.BackendState{state}, 7, 4)
	h := NewHandler(bal, "/models/test.gguf", 30*time.Second)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hi"}],"think":false}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	choices := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)

	if msg["content"] != "The answer is 42." {
		t.Errorf("expected 'The answer is 42.', got %q", msg["content"])
	}
	if _, has := msg["reasoning_content"]; has {
		t.Error("expected reasoning_content to be removed")
	}
}

func TestProxyThinkFalseStreaming(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		chunks := []string{
			`{"choices":[{"delta":{"reasoning_content":"<think>"}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"thinking..."}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"</think>"}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"Hello!"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			f.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer backend.Close()

	state := balancer.NewBackendState(0, backend.Listener.Addr().String())
	state.SetStatus(balancer.StatusHealthy)
	bal := balancer.New([]*balancer.BackendState{state}, 7, 4)
	h := NewHandler(bal, "/models/test.gguf", 30*time.Second)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hi"}],"think":false,"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()

	if strings.Contains(body, "thinking...") {
		t.Errorf("thinking tokens should be suppressed, got:\n%s", body)
	}
	if !strings.Contains(body, `"content":"Hello!"`) {
		t.Errorf("expected answer as content, got:\n%s", body)
	}
	if strings.Contains(body, "reasoning_content") {
		t.Errorf("reasoning_content should not appear in output, got:\n%s", body)
	}
}

func TestProxyThinkTruePassesThrough(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"","reasoning_content":"thinking and answer mixed"}}]}`))
	}))
	defer backend.Close()

	state := balancer.NewBackendState(0, backend.Listener.Addr().String())
	state.SetStatus(balancer.StatusHealthy)
	bal := balancer.New([]*balancer.BackendState{state}, 7, 4)
	h := NewHandler(bal, "/models/test.gguf", 30*time.Second)

	// think:true — should be transparent, no rewriting
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hi"}],"think":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	choices := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)

	if _, has := msg["reasoning_content"]; !has {
		t.Error("expected reasoning_content to pass through when think:true")
	}
}

func TestProxyNoThinkParamStripsReasoning(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"","reasoning_content":"<think>\nreasoning\n</think>\n\nthe answer"}}]}`))
	}))
	defer backend.Close()

	state := balancer.NewBackendState(0, backend.Listener.Addr().String())
	state.SetStatus(balancer.StatusHealthy)
	bal := balancer.New([]*balancer.BackendState{state}, 7, 4)
	h := NewHandler(bal, "/models/test.gguf", 30*time.Second)

	// No think param — reasoning stripped by default
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	choices := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)

	if msg["content"] != "the answer" {
		t.Errorf("expected 'the answer', got %q", msg["content"])
	}
	if _, has := msg["reasoning_content"]; has {
		t.Error("expected reasoning_content to be stripped by default")
	}
}

func TestStreamThinkDisabled_TruncatedThinkBlock(t *testing.T) {
	// Model starts thinking, gets truncated (finish_reason: "length") without
	// ever closing the </think> tag. Buffered reasoning should be salvaged.
	sse := `data: {"id":"x","model":"test","choices":[{"delta":{"reasoning_content":"<think>"}}]}` + "\n\n" +
		`data: {"id":"x","model":"test","choices":[{"delta":{"reasoning_content":"Let me think about Helsinki to Vantaa."}}]}` + "\n\n" +
		`data: {"id":"x","model":"test","choices":[{"delta":{"reasoning_content":"\n\nIt is a quick 18 km drive taking about 25 minutes via Tuusulanväylä."}}]}` + "\n\n" +
		`data: {"id":"x","model":"test","choices":[{"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	w := httptest.NewRecorder()
	streamThinkDisabled(w, strings.NewReader(sse), func() {})

	body := w.Body.String()

	// Should contain salvaged content
	if !strings.Contains(body, "18 km drive") {
		t.Errorf("expected salvaged content with '18 km drive', got:\n%s", body)
	}

	// Should NOT contain raw reasoning markers
	if strings.Contains(body, "<think>") {
		t.Errorf("should not contain <think> tag, got:\n%s", body)
	}

	// Should contain [DONE]
	if !strings.Contains(body, "[DONE]") {
		t.Error("expected [DONE] marker")
	}
}

func TestStreamThinkDisabled_TruncatedAllReasoning(t *testing.T) {
	// Model only produced reasoning artifacts, no clean prose at all
	sse := `data: {"id":"x","model":"test","choices":[{"delta":{"reasoning_content":"<think>"}}]}` + "\n\n" +
		`data: {"id":"x","model":"test","choices":[{"delta":{"reasoning_content":"* Origin: Helsinki\n* Destination: Vantaa\n* Distance: 18 km"}}]}` + "\n\n" +
		`data: {"id":"x","model":"test","choices":[{"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	w := httptest.NewRecorder()
	streamThinkDisabled(w, strings.NewReader(sse), func() {})

	body := w.Body.String()

	// Should still emit something (raw fallback) rather than empty
	if !strings.Contains(body, "Helsinki") {
		t.Errorf("expected some salvaged content, got:\n%s", body)
	}
}

func TestPeerProxyThinkFalse(t *testing.T) {
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"","reasoning_content":"<think>reasoning</think>\n\npeer answer"}}]}`))
	}))
	defer peerSrv.Close()

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"peer-model","messages":[],"think":false}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxyToPeer(w, req, peerSrv.Listener.Addr().String(), "viiwork-test", true)

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	choices := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)

	if msg["content"] != "peer answer" {
		t.Errorf("expected 'peer answer', got %q", msg["content"])
	}
	if _, has := msg["reasoning_content"]; has {
		t.Error("expected reasoning_content removed for peer proxy with think:false")
	}
}
