// viiwork-mcp is an MCP (Model Context Protocol) server that exposes a viiwork
// inference cluster as tools for AI coding assistants. It communicates via
// JSON-RPC 2.0 over stdio.
//
// Tools:
//   - query:   Send a prompt to a local model and get a response
//   - models:  List available models on the cluster
//   - status:  Get cluster health and load information
//
// Configuration:
//   --url flag or VIIWORK_URL env var (default http://localhost:8080)
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

var viiworkURL string

// JSON-RPC 2.0

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any         `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCP protocol types

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type capabilities struct {
	Tools *struct{} `json:"tools"`
}

type initResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	ServerInfo      serverInfo   `json:"serverInfo"`
	Capabilities    capabilities `json:"capabilities"`
}

type toolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

type toolsListResult struct {
	Tools []toolDef `json:"tools"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// Tool argument types

type queryArgs struct {
	Prompt      string   `json:"prompt"`
	System      string   `json:"system,omitempty"`
	Model       string   `json:"model,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
}

// OpenAI-compatible API types

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	Stream      bool          `json:"stream"`
}

type chatChoice struct {
	Message chatMessage `json:"message"`
}

type chatResponse struct {
	Choices []chatChoice `json:"choices"`
	Model   string       `json:"model"`
	Usage   struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func main() {
	urlFlag := flag.String("url", "", "viiwork base URL (default http://localhost:8080)")
	flag.Parse()

	viiworkURL = *urlFlag
	if viiworkURL == "" {
		viiworkURL = os.Getenv("VIIWORK_URL")
	}
	if viiworkURL == "" {
		viiworkURL = "http://localhost:8080"
	}

	log.SetOutput(os.Stderr)
	log.SetPrefix("[viiwork-mcp] ")
	log.Printf("starting, endpoint: %s", viiworkURL)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB buffer

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("invalid JSON-RPC: %v", err)
			continue
		}

		resp := handle(req)
		if resp == nil {
			continue // notification, no response
		}

		out, err := json.Marshal(resp)
		if err != nil {
			log.Printf("marshal error: %v", err)
			continue
		}
		fmt.Fprintf(os.Stdout, "%s\n", out)
	}

	if err := scanner.Err(); err != nil {
		log.Fatalf("stdin read error: %v", err)
	}
}

func handle(req rpcRequest) *rpcResponse {
	switch req.Method {
	case "initialize":
		return success(req.ID, initResult{
			ProtocolVersion: "2024-11-05",
			ServerInfo:      serverInfo{Name: "viiwork-mcp", Version: "1.0.0"},
			Capabilities:    capabilities{Tools: &struct{}{}},
		})

	case "notifications/initialized":
		return nil // notification, no response

	case "tools/list":
		return success(req.ID, toolsListResult{Tools: toolDefinitions()})

	case "tools/call":
		var params toolCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errResp(req.ID, -32602, "invalid params: "+err.Error())
		}
		return callTool(req.ID, params)

	default:
		return errResp(req.ID, -32601, "method not found: "+req.Method)
	}
}

func toolDefinitions() []toolDef {
	return []toolDef{
		{
			Name:        "query",
			Description: "Send a prompt to a locally hosted LLM via the viiwork cluster and return its response. Use this to delegate code generation, review, analysis, or other tasks to the local model.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{
						"type":        "string",
						"description": "The prompt to send to the model",
					},
					"system": map[string]any{
						"type":        "string",
						"description": "Optional system prompt to set context/role",
					},
					"model": map[string]any{
						"type":        "string",
						"description": "Model ID to use (omit to use the first available model)",
					},
					"max_tokens": map[string]any{
						"type":        "integer",
						"description": "Maximum tokens to generate (default 4096)",
					},
					"temperature": map[string]any{
						"type":        "number",
						"description": "Sampling temperature (default 0.7)",
					},
				},
				"required": []string{"prompt"},
			},
		},
		{
			Name:        "models",
			Description: "List all models available on the viiwork cluster. Returns model IDs that can be passed to the query tool.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "status",
			Description: "Get viiwork cluster health: node info, per-GPU backend status, in-flight requests, and peer nodes. Use this to check capacity before sending work.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

func callTool(id json.RawMessage, params toolCallParams) *rpcResponse {
	switch params.Name {
	case "query":
		return toolQuery(id, params.Arguments)
	case "models":
		return toolModels(id)
	case "status":
		return toolStatus(id)
	default:
		return success(id, toolResult{
			Content: []textContent{{Type: "text", Text: "unknown tool: " + params.Name}},
			IsError: true,
		})
	}
}

func toolQuery(id json.RawMessage, rawArgs json.RawMessage) *rpcResponse {
	var args queryArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return success(id, toolResult{
			Content: []textContent{{Type: "text", Text: "invalid arguments: " + err.Error()}},
			IsError: true,
		})
	}

	if args.Prompt == "" {
		return success(id, toolResult{
			Content: []textContent{{Type: "text", Text: "prompt is required"}},
			IsError: true,
		})
	}

	// Resolve model if not specified
	model := args.Model
	if model == "" {
		var err error
		model, err = firstModel()
		if err != nil {
			return success(id, toolResult{
				Content: []textContent{{Type: "text", Text: "could not resolve model: " + err.Error()}},
				IsError: true,
			})
		}
	}

	maxTokens := args.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	var messages []chatMessage
	if args.System != "" {
		messages = append(messages, chatMessage{Role: "system", Content: args.System})
	}
	messages = append(messages, chatMessage{Role: "user", Content: args.Prompt})

	chatReq := chatRequest{
		Model:       model,
		Messages:    messages,
		MaxTokens:   maxTokens,
		Temperature: args.Temperature,
		Stream:      false,
	}

	body, err := json.Marshal(chatReq)
	if err != nil {
		return success(id, toolResult{
			Content: []textContent{{Type: "text", Text: "marshal error: " + err.Error()}},
			IsError: true,
		})
	}

	log.Printf("query: model=%s tokens=%d prompt_len=%d", model, maxTokens, len(args.Prompt))

	resp, err := httpPost(viiworkURL+"/v1/chat/completions", body)
	if err != nil {
		return success(id, toolResult{
			Content: []textContent{{Type: "text", Text: "request failed: " + err.Error()}},
			IsError: true,
		})
	}

	var chatResp chatResponse
	if err := json.Unmarshal(resp, &chatResp); err != nil {
		return success(id, toolResult{
			Content: []textContent{{Type: "text", Text: "invalid response: " + err.Error()}},
			IsError: true,
		})
	}

	if len(chatResp.Choices) == 0 {
		return success(id, toolResult{
			Content: []textContent{{Type: "text", Text: "no response from model"}},
			IsError: true,
		})
	}

	text := chatResp.Choices[0].Message.Content
	footer := fmt.Sprintf("\n\n---\nmodel: %s | tokens: %d prompt + %d completion",
		chatResp.Model, chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens)

	return success(id, toolResult{
		Content: []textContent{{Type: "text", Text: text + footer}},
	})
}

func toolModels(id json.RawMessage) *rpcResponse {
	resp, err := httpGet(viiworkURL + "/v1/models")
	if err != nil {
		return success(id, toolResult{
			Content: []textContent{{Type: "text", Text: "request failed: " + err.Error()}},
			IsError: true,
		})
	}

	// Pretty-print
	var buf bytes.Buffer
	json.Indent(&buf, resp, "", "  ")

	return success(id, toolResult{
		Content: []textContent{{Type: "text", Text: buf.String()}},
	})
}

func toolStatus(id json.RawMessage) *rpcResponse {
	resp, err := httpGet(viiworkURL + "/v1/cluster")
	if err != nil {
		return success(id, toolResult{
			Content: []textContent{{Type: "text", Text: "request failed: " + err.Error()}},
			IsError: true,
		})
	}

	var buf bytes.Buffer
	json.Indent(&buf, resp, "", "  ")

	return success(id, toolResult{
		Content: []textContent{{Type: "text", Text: buf.String()}},
	})
}

// HTTP helpers

var httpClient = &http.Client{Timeout: 60 * time.Minute}

func httpGet(url string) ([]byte, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

func httpPost(url string, data []byte) ([]byte, error) {
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

func firstModel() (string, error) {
	resp, err := httpGet(viiworkURL + "/v1/models")
	if err != nil {
		return "", err
	}

	var modelsResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &modelsResp); err != nil {
		return "", err
	}
	if len(modelsResp.Data) == 0 {
		return "", fmt.Errorf("no models available")
	}
	return modelsResp.Data[0].ID, nil
}

// JSON-RPC helpers

func success(id json.RawMessage, result any) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errResp(id json.RawMessage, code int, msg string) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}
