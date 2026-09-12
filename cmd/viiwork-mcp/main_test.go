package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/meshapi"
)

func req(t *testing.T, method, params string) rpcRequest {
	t.Helper()
	r := rpcRequest{Method: method, ID: json.RawMessage(`1`)}
	if params != "" {
		r.Params = json.RawMessage(params)
	}
	return r
}

func TestHandleDispatch(t *testing.T) {
	if r := handle(req(t, "initialize", "")); r == nil || r.Error != nil {
		t.Fatalf("initialize: %+v", r)
	}
	// A notification has no id and must produce no response at all: answering
	// one is a protocol violation.
	if r := handle(req(t, "notifications/initialized", "")); r != nil {
		t.Errorf("a notification must not be answered, got %+v", r)
	}
	if r := handle(req(t, "tools/list", "")); r == nil || r.Error != nil {
		t.Fatalf("tools/list: %+v", r)
	}
	// Unknown methods and malformed params map to the JSON-RPC codes clients
	// branch on, not to a generic failure.
	if r := handle(req(t, "no/such/method", "")); r == nil || r.Error == nil || r.Error.Code != -32601 {
		t.Errorf("unknown method should be -32601, got %+v", r)
	}
	if r := handle(req(t, "tools/call", `{"name":`)); r == nil || r.Error == nil || r.Error.Code != -32602 {
		t.Errorf("malformed params should be -32602, got %+v", r)
	}
}

func TestToolsListMatchesDispatch(t *testing.T) {
	// Every advertised tool must actually dispatch, or a client is told about
	// something that answers "unknown tool".
	for _, tool := range toolDefinitions() {
		r := callTool(json.RawMessage(`1`), toolCallParams{Name: tool.Name, Arguments: json.RawMessage(`{}`)})
		if r == nil {
			t.Fatalf("tool %q produced no response", tool.Name)
		}
		var res toolResult
		if err := json.Unmarshal(mustResult(t, r), &res); err != nil {
			t.Fatalf("tool %q result: %v", tool.Name, err)
		}
		for _, c := range res.Content {
			if strings.Contains(c.Text, "unknown tool") {
				t.Errorf("tool %q is advertised but not dispatched", tool.Name)
			}
		}
	}
}

func TestUnknownToolIsAToolErrorNotAProtocolError(t *testing.T) {
	r := callTool(json.RawMessage(`1`), toolCallParams{Name: "nope", Arguments: json.RawMessage(`{}`)})
	if r == nil || r.Error != nil {
		t.Fatalf("an unknown tool is a tool-level error, not JSON-RPC: %+v", r)
	}
	var res toolResult
	if err := json.Unmarshal(mustResult(t, r), &res); err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("result should be marked IsError so the model sees it failed")
	}
}

func TestQueryRequiresAPrompt(t *testing.T) {
	for _, args := range []string{`{}`, `{"prompt":""}`, `{"model":"m"}`} {
		r := toolQuery(json.RawMessage(`1`), json.RawMessage(args))
		var res toolResult
		if err := json.Unmarshal(mustResult(t, r), &res); err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Errorf("query(%s) should be an error result: a prompt is required", args)
		}
	}
	// Malformed arguments are reported, never panicked on.
	r := toolQuery(json.RawMessage(`1`), json.RawMessage(`{"prompt":`))
	var res toolResult
	if err := json.Unmarshal(mustResult(t, r), &res); err != nil || !res.IsError {
		t.Errorf("malformed arguments should produce an error result: %+v", res)
	}
}

// The tools talk to a real node over HTTP. Pointing them at a test server
// checks the paths they use, which are meshapi's rather than literals.
func TestToolsCallTheDocumentedPaths(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		switch r.URL.Path {
		case meshapi.PathModels:
			w.Write([]byte(`{"object":"list","data":[{"id":"m","owned_by":"local"}]}`))
		case meshapi.PathCluster:
			w.Write([]byte(`{"view":"node-a","mesh":"open","members":[]}`))
		case meshapi.PathChatCompletions:
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	old := viiworkURL
	viiworkURL = srv.URL
	defer func() { viiworkURL = old }()

	toolModels(json.RawMessage(`1`))
	toolStatus(json.RawMessage(`1`))
	toolQuery(json.RawMessage(`1`), json.RawMessage(`{"prompt":"hello","model":"m"}`))

	got := strings.Join(seen, " ")
	for _, want := range []string{meshapi.PathModels, meshapi.PathCluster, meshapi.PathChatCompletions} {
		if !strings.Contains(got, want) {
			t.Errorf("no request to %s; saw %v", want, seen)
		}
	}
}

func TestToolsReportAnUnreachableNodeAsAnError(t *testing.T) {
	old := viiworkURL
	viiworkURL = "http://127.0.0.1:1" // nothing listens here
	defer func() { viiworkURL = old }()

	for name, call := range map[string]func() *rpcResponse{
		"models": func() *rpcResponse { return toolModels(json.RawMessage(`1`)) },
		"status": func() *rpcResponse { return toolStatus(json.RawMessage(`1`)) },
		"query": func() *rpcResponse {
			return toolQuery(json.RawMessage(`1`), json.RawMessage(`{"prompt":"x","model":"m"}`))
		},
	} {
		r := call()
		var res toolResult
		if err := json.Unmarshal(mustResult(t, r), &res); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !res.IsError {
			t.Errorf("%s against an unreachable node should be an error result, not a silent empty answer", name)
		}
	}
}

func mustResult(t *testing.T, r *rpcResponse) []byte {
	t.Helper()
	if r == nil {
		t.Fatal("no response")
	}
	b, err := json.Marshal(r.Result)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
