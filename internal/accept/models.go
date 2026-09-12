package accept

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

const (
	contentPrompt    = "Reply with the single word: ready"
	toolsPrompt      = "What is the weather in Helsinki? Use the get_weather tool."
	contentMaxTokens = 64
	toolsMaxTokens   = 256
	defaultTimeout   = 10 * time.Minute
	statusTimeout    = 30 * time.Second
	detailChars      = 80
)

// ModelCheckOptions tune CheckModels and CheckAlias.
type ModelCheckOptions struct {
	Via       string        // "" = no pin-through check
	MaxTokens int           // 0 = 64 for content, 256 for tools
	Timeout   time.Duration // per request; 0 = 10m
}

func (o ModelCheckOptions) maxTokens(def int) int {
	if o.MaxTokens > 0 {
		return o.MaxTokens
	}
	return def
}

func (o ModelCheckOptions) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return defaultTimeout
}

// chatRequest is the acceptance request: deterministic, not streamed, and
// with thinking off both ways an engine may read it.
func chatRequest(model, prompt string, maxTokens int) map[string]any {
	return map[string]any{
		"model":                model,
		"messages":             []any{map[string]any{"role": "user", "content": prompt}},
		"temperature":          0,
		"stream":               false,
		"max_tokens":           maxTokens,
		"enable_thinking":      false,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}
}

var weatherTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "get_weather",
		"description": "Get the current weather for a city",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}},
			"required":   []string{"city"},
		},
	},
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

type chatResult struct {
	status int
	header http.Header
	body   []byte
	resp   chatResponse
	err    error
}

// postChat sends one chat completion to base, pinned to host when host is set.
func postChat(ctx context.Context, e Env, base, host string, body map[string]any, timeout time.Duration) chatResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	target := base + meshapi.PathChatCompletions
	if host != "" {
		target += "?host=" + url.QueryEscape(host)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return chatResult{err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return chatResult{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	addHeaders(req, e.Headers)
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return chatResult{err: err}
	}
	defer resp.Body.Close()
	res := chatResult{status: resp.StatusCode, header: resp.Header}
	res.body, res.err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if res.err == nil && res.status == http.StatusOK {
		if err := json.Unmarshal(res.body, &res.resp); err != nil {
			res.err = fmt.Errorf("decoding response: %w", err)
		}
	}
	return res
}

// failure describes a request that did not come back as a decodable 200.
func (c chatResult) failure() string {
	if c.err != nil && c.status == 0 {
		return c.err.Error()
	}
	if c.status != http.StatusOK {
		return fmt.Sprintf("HTTP %d: %s", c.status, clip(strings.TrimSpace(string(c.body))))
	}
	if c.err != nil {
		return c.err.Error()
	}
	return ""
}

func clip(s string) string {
	r := []rune(s)
	if len(r) > detailChars {
		return string(r[:detailChars])
	}
	return s
}

func (c chatResult) choice() (content, finish string, tools []string) {
	if len(c.resp.Choices) == 0 {
		return "", "", nil
	}
	ch := c.resp.Choices[0]
	for _, tc := range ch.Message.ToolCalls {
		tools = append(tools, tc.Function.Name)
	}
	return ch.Message.Content, ch.FinishReason, tools
}

// readStatus reads a node's /v1/status.
func readStatus(ctx context.Context, e Env, base string, timeout time.Duration) (meshapi.NodeStatus, error) {
	var st meshapi.NodeStatus
	err := getJSON(ctx, e, base, meshapi.PathStatus, min(timeout, statusTimeout), &st)
	return st, err
}

// CheckModels runs a content and a tool-call request per model, pinned to the
// node, and with Via set the same content request through another node.
// models empty means every model the node lists.
func CheckModels(ctx context.Context, e Env, nodeAPI string, models []string, o ModelCheckOptions) Report {
	r := Report{Command: "models", Target: nodeAPI, Started: e.Now()}
	base := baseURL(nodeAPI)
	st, err := readStatus(ctx, e, base, o.timeout())
	if err != nil {
		r.Checks = []Check{{Name: "status reachable", Detail: err.Error()}}
		return r
	}
	node := st.Node
	if len(models) == 0 {
		for _, m := range st.Models {
			models = append(models, m.Name)
		}
	}

	var viaNode string
	var viaErr error
	if o.Via != "" {
		var vst meshapi.NodeStatus
		vst, viaErr = readStatus(ctx, e, baseURL(o.Via), o.timeout())
		viaNode = vst.Node
	}

	for _, m := range models {
		r.Checks = append(r.Checks, timed(e, "content "+m, func() (bool, string) {
			res := postChat(ctx, e, base, node, chatRequest(m, contentPrompt, o.maxTokens(contentMaxTokens)), o.timeout())
			if f := res.failure(); f != "" {
				return false, f
			}
			content, _, _ := res.choice()
			if got := res.header.Get(meshapi.HeaderNode); got != node {
				return false, fmt.Sprintf("answered by %q, want %s", got, node)
			}
			if strings.TrimSpace(content) == "" {
				return false, "empty content"
			}
			return true, clip(content)
		}))

		r.Checks = append(r.Checks, timed(e, "tools "+m, func() (bool, string) {
			body := chatRequest(m, toolsPrompt, o.maxTokens(toolsMaxTokens))
			body["tools"] = []any{weatherTool}
			body["tool_choice"] = "auto"
			res := postChat(ctx, e, base, node, body, o.timeout())
			if f := res.failure(); f != "" {
				return false, f
			}
			_, finish, tools := res.choice()
			noun := "tool calls"
			if len(tools) == 1 {
				noun = "tool call"
			}
			detail := fmt.Sprintf("finish_reason %s, %d %s", finish, len(tools), noun)
			called := false
			for _, name := range tools {
				called = called || name == "get_weather"
			}
			return finish == "tool_calls" && called, detail
		}))

		if o.Via == "" {
			continue
		}
		r.Checks = append(r.Checks, timed(e, "pin "+m, func() (bool, string) {
			if viaErr != nil {
				return false, "reading via status: " + viaErr.Error()
			}
			res := postChat(ctx, e, baseURL(o.Via), node, chatRequest(m, contentPrompt, o.maxTokens(contentMaxTokens)), o.timeout())
			if f := res.failure(); f != "" {
				return false, f
			}
			gotNode, gotOrigin := res.header.Get(meshapi.HeaderNode), res.header.Get(meshapi.HeaderOrigin)
			if gotNode != node {
				return false, fmt.Sprintf("answered by %q, want %s", gotNode, node)
			}
			if viaNode != node && gotOrigin != viaNode {
				return false, fmt.Sprintf("origin %q, want %s", gotOrigin, viaNode)
			}
			return true, fmt.Sprintf("%s via %s", gotNode, viaNode)
		}))
	}
	return r
}

// CheckAlias sends the content request for alias through every entry, which
// may be a node's host:port or a gateway URL, and checks each resolved it to
// expectModel.
func CheckAlias(ctx context.Context, e Env, entries []string, alias, expectModel string, o ModelCheckOptions) Report {
	r := Report{Command: "alias", Target: alias, Started: e.Now()}
	for _, entry := range entries {
		r.Checks = append(r.Checks, timed(e, "alias "+alias+" via "+entry, func() (bool, string) {
			res := postChat(ctx, e, baseURL(entry), "", chatRequest(alias, contentPrompt, o.maxTokens(contentMaxTokens)), o.timeout())
			if f := res.failure(); f != "" {
				return false, f
			}
			gotAlias, gotModel := res.header.Get(meshapi.HeaderAlias), res.header.Get(meshapi.HeaderModel)
			var problems []string
			if gotAlias != alias {
				problems = append(problems, fmt.Sprintf("%s %q", meshapi.HeaderAlias, gotAlias))
			}
			if gotModel != expectModel {
				problems = append(problems, fmt.Sprintf("%s %q", meshapi.HeaderModel, gotModel))
			}
			if res.resp.Model != expectModel {
				problems = append(problems, "body model "+strconv.Quote(res.resp.Model))
			}
			if len(problems) > 0 {
				return false, strings.Join(problems, ", ") + ", want " + expectModel
			}
			return true, gotModel + " on " + res.header.Get(meshapi.HeaderNode)
		}))
	}
	return r
}
