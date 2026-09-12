package accept

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/janit/viiwork/v2/meshapi"
)

// chatReply is an OpenAI chat completion with one choice.
func chatReply(model, content, finish string, toolCalls ...string) map[string]any {
	msg := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		var calls []any
		for _, name := range toolCalls {
			calls = append(calls, map[string]any{
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": `{"city":"Helsinki"}`},
			})
		}
		msg["tool_calls"] = calls
	}
	return map[string]any{
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
	}
}

func isToolsRequest(body map[string]any) bool {
	_, ok := body["tools"]
	return ok
}

func nodeHeaders(node string, more ...string) http.Header {
	h := http.Header{}
	h.Set(meshapi.HeaderNode, node)
	for i := 0; i+1 < len(more); i += 2 {
		h.Set(more[i], more[i+1])
	}
	return h
}

// servingNode is node-a serving model a, answering content and tool calls
// correctly unless the test replaces its chat handler.
type servingNode struct {
	*fakeNode
	mu       sync.Mutex
	requests []*http.Request
	bodies   []map[string]any
}

func newServingNode(t *testing.T, name string) *servingNode {
	t.Helper()
	n := &servingNode{fakeNode: newFakeNode(t, name)}
	n.SetStatus(nodeStatus(name, backendModel("a", "healthy")))
	n.answer(func(r *http.Request, body map[string]any) (int, http.Header, any) {
		if isToolsRequest(body) {
			return 200, nodeHeaders(name), chatReply("a", "", "tool_calls", "get_weather")
		}
		return 200, nodeHeaders(name), chatReply("a", "ready", "stop")
	})
	return n
}

// answer installs h and records every chat request.
func (n *servingNode) answer(h func(r *http.Request, body map[string]any) (int, http.Header, any)) {
	n.OnChat(func(r *http.Request, body map[string]any) (int, http.Header, any) {
		n.mu.Lock()
		n.requests = append(n.requests, r)
		n.bodies = append(n.bodies, body)
		n.mu.Unlock()
		return h(r, body)
	})
}

func TestModelsContentAndTools(t *testing.T) { // M1
	node := newServingNode(t, "node-a")
	r := CheckModels(context.Background(), DefaultEnv(), node.URL(), nil, ModelCheckOptions{})
	if got := strings.Join(checkNames(r), "|"); got != "content a|tools a" {
		t.Fatalf("checks: %+v", r.Checks)
	}
	if !r.Pass() {
		t.Fatalf("checks: %+v", r.Checks)
	}
	if c := r.Checks[0]; c.Detail != "ready" {
		t.Errorf("content detail %q", c.Detail)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.requests) != 2 {
		t.Fatalf("%d chat requests", len(node.requests))
	}
	content, tools := node.bodies[0], node.bodies[1]
	if q := node.requests[0].URL.Query().Get("host"); q != "node-a" {
		t.Errorf("content request host pin %q", q)
	}
	if content["enable_thinking"] != false || content["stream"] != false || content["temperature"] != float64(0) || content["max_tokens"] != float64(64) {
		t.Errorf("content body: %v", content)
	}
	if kw, _ := content["chat_template_kwargs"].(map[string]any); kw == nil || kw["enable_thinking"] != false {
		t.Errorf("chat_template_kwargs: %v", content["chat_template_kwargs"])
	}
	if tools["tool_choice"] != "auto" || tools["max_tokens"] != float64(256) || node.requests[1].URL.Query().Get("host") != "node-a" {
		t.Errorf("tools body: %v", tools)
	}
	fns, _ := tools["tools"].([]any)
	if len(fns) != 1 || !strings.Contains(toJSON(fns[0]), `"name":"get_weather"`) || !strings.Contains(toJSON(fns[0]), `"required":["city"]`) {
		t.Errorf("tools: %v", tools["tools"])
	}
}

func TestModelsToolsNotCalled(t *testing.T) { // M2
	node := newServingNode(t, "node-a")
	node.answer(func(r *http.Request, body map[string]any) (int, http.Header, any) {
		return 200, nodeHeaders("node-a"), chatReply("a", "It is sunny.", "stop")
	})
	r := CheckModels(context.Background(), DefaultEnv(), node.URL(), []string{"a"}, ModelCheckOptions{})
	c := checkByName(t, r, "tools a")
	if c.Pass || !strings.Contains(c.Detail, "stop") || !strings.Contains(c.Detail, "0") {
		t.Errorf("tools a: %+v", c)
	}
}

func TestModelsBlankContent(t *testing.T) { // M3
	node := newServingNode(t, "node-a")
	node.answer(func(r *http.Request, body map[string]any) (int, http.Header, any) {
		return 200, nodeHeaders("node-a"), chatReply("a", "   ", "stop")
	})
	r := CheckModels(context.Background(), DefaultEnv(), node.URL(), []string{"a"}, ModelCheckOptions{})
	if c := checkByName(t, r, "content a"); c.Pass {
		t.Errorf("content a: %+v", c)
	}
}

func TestModelsWrongNode(t *testing.T) { // M4
	node := newServingNode(t, "node-a")
	node.answer(func(r *http.Request, body map[string]any) (int, http.Header, any) {
		return 200, nodeHeaders("node-b"), chatReply("a", "ready", "stop")
	})
	r := CheckModels(context.Background(), DefaultEnv(), node.URL(), []string{"a"}, ModelCheckOptions{})
	c := checkByName(t, r, "content a")
	if c.Pass || !strings.Contains(c.Detail, "node-a") || !strings.Contains(c.Detail, "node-b") {
		t.Errorf("content a: %+v", c)
	}
}

func TestModelsPinThroughAnotherNode(t *testing.T) { // M5, M6
	for _, withOrigin := range []bool{true, false} {
		node := newServingNode(t, "node-a")
		via := newServingNode(t, "node-b")
		via.answer(func(r *http.Request, body map[string]any) (int, http.Header, any) {
			h := nodeHeaders("node-a")
			if withOrigin {
				h.Set(meshapi.HeaderOrigin, "node-b")
			}
			return 200, h, chatReply("a", "ready", "stop")
		})
		r := CheckModels(context.Background(), DefaultEnv(), node.URL(), []string{"a"}, ModelCheckOptions{Via: via.URL()})
		c := checkByName(t, r, "pin a")
		if c.Pass != withOrigin {
			t.Errorf("origin %v: pin a: %+v", withOrigin, c)
		}
		via.mu.Lock()
		if len(via.requests) != 1 || via.requests[0].URL.Query().Get("host") != "node-a" {
			t.Errorf("via requests: %d", len(via.requests))
		}
		via.mu.Unlock()
	}
}

func TestModelsHeadersOnEveryRequest(t *testing.T) { // M7
	node := newServingNode(t, "node-a")
	via := newServingNode(t, "node-b")
	via.answer(func(r *http.Request, body map[string]any) (int, http.Header, any) {
		return 200, nodeHeaders("node-a", meshapi.HeaderOrigin, "node-b"), chatReply("a", "ready", "stop")
	})
	e := DefaultEnv()
	e.Headers = http.Header{"Authorization": {"Bearer k"}}
	CheckModels(context.Background(), e, node.URL(), nil, ModelCheckOptions{Via: via.URL()})
	seen := append(node.Seen(), via.Seen()...)
	if len(seen) < 5 {
		t.Fatalf("requests: %q", seen)
	}
	for _, s := range seen {
		if !strings.HasSuffix(s, " Bearer k") {
			t.Errorf("request without the header: %q", s)
		}
	}
}

func aliasEntry(t *testing.T, model string) *servingNode {
	t.Helper()
	n := newServingNode(t, "node-a")
	n.answer(func(r *http.Request, body map[string]any) (int, http.Header, any) {
		h := nodeHeaders("node-a", meshapi.HeaderAlias, "stable-coder", meshapi.HeaderModel, model)
		return 200, h, chatReply(model, "ready", "stop")
	})
	return n
}

func TestAliasThroughEntries(t *testing.T) { // A1, A2, and a wrong body model
	good1, good2 := aliasEntry(t, "Qwen3.8-27B"), aliasEntry(t, "Qwen3.8-27B")
	bad := aliasEntry(t, "gemma-4-31B-it")
	// Right headers, but the body names another model.
	wrongBody := newServingNode(t, "node-b")
	wrongBody.answer(func(r *http.Request, body map[string]any) (int, http.Header, any) {
		h := nodeHeaders("node-b", meshapi.HeaderAlias, "stable-coder", meshapi.HeaderModel, "Qwen3.8-27B")
		return 200, h, chatReply("stable-coder", "ready", "stop")
	})
	entries := []string{good1.URL(), "http://" + good2.URL() + "/", bad.URL(), wrongBody.URL()}
	r := CheckAlias(context.Background(), DefaultEnv(), entries, "stable-coder", "Qwen3.8-27B", ModelCheckOptions{})
	if r.Command != "alias" || len(r.Checks) != 4 {
		t.Fatalf("report: %+v", r)
	}
	for i, c := range r.Checks {
		if c.Name != "alias stable-coder via "+entries[i] {
			t.Errorf("check %d named %q", i, c.Name)
		}
		if c.Pass != (i < 2) {
			t.Errorf("check %d: %+v", i, c)
		}
	}
	good1.mu.Lock()
	defer good1.mu.Unlock()
	if good1.bodies[0]["model"] != "stable-coder" || good1.requests[0].URL.RawQuery != "" {
		t.Errorf("alias request: %v ?%s", good1.bodies[0], good1.requests[0].URL.RawQuery)
	}
}

func TestModelsStatusUnreachable(t *testing.T) {
	addr, _ := closedAddr(t)
	r := CheckModels(context.Background(), DefaultEnv(), addr, nil, ModelCheckOptions{})
	if len(r.Checks) != 1 || r.Checks[0].Name != "status reachable" || r.Checks[0].Pass {
		t.Errorf("checks: %+v", r.Checks)
	}
}
