package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
)

var thinkBlockRe = regexp.MustCompile(`(?s)<think>.*?</think>\s*`)

// reasoningLineRe matches lines that look like reasoning artifacts:
// bullet points, numbered lists, metadata labels, drafting markers, self-checks.
var reasoningLineRe = regexp.MustCompile(
	`(?i)^\s*[\*\-]\s|` + // bullet points
		`^\s*\d+[\.\)]\s|` + // numbered lists
		`(?i)\*draft|\*refin|\*word count|\*banned|\*constraint|\*check|\*self-correct|\*revis|\*final|` +
		`(?i)^wait[,. ]|^hmm[,. ]|^let me |^let's |^actually[,. ]|^no[,. ]|^ok[,. ]|` +
		`(?i)word count|banned words|constraints? check|content check|formatting check|` +
		`(?i)the prompt says|the prompt |the user |the instruction|` + // prompt references
		`(?i)^is [""].+[""] ok|^is this |^is that |^should i |^should we |` + // self-checks
		`(?i)\? yes\.?$|\? no\.?$|\? ok\.?$|` + // Q&A self-checks
		`(?i)^one detail|^one thing|^note:|^important:|^remember:|` + // meta-commentary
		`(?i)^draft \d|^version \d|^revision|^attempt \d|^try \d|` + // draft markers
		`(?i)expansion strategy|refining|polishing|self-correction|final polish`,
)

// looksLikeReasoning returns true if the text contains significant reasoning artifacts.
func looksLikeReasoning(s string) bool {
	if len(s) < 300 {
		return false // short content is unlikely to be reasoning
	}
	lines := strings.Split(s, "\n")
	artifactLines := 0
	for _, line := range lines {
		if reasoningLineRe.MatchString(line) {
			artifactLines++
		}
	}
	// If more than 30% of lines are artifacts, it's reasoning
	return len(lines) > 3 && float64(artifactLines)/float64(len(lines)) > 0.3
}

// extractQuotedAnswer checks if the text ends with a large quoted block and
// returns its contents. Models sometimes wrap the final answer in quotes.
func extractQuotedAnswer(s string) (string, bool) {
	s = strings.TrimSpace(s)
	// Check for a quoted block at the end: "..." or «...»
	if len(s) > 100 && s[len(s)-1] == '"' {
		// Find the opening quote — scan backwards past the closing quote
		depth := 0
		for i := len(s) - 1; i >= 0; i-- {
			if s[i] == '"' {
				depth++
			}
			// Look for an opening quote preceded by a newline or start of line
			if s[i] == '"' && depth >= 2 && (i == 0 || s[i-1] == '\n' || s[i-1] == ' ') {
				candidate := s[i+1 : len(s)-1]
				candidate = strings.TrimSpace(candidate)
				// Only accept if the quoted block is substantial (>80 chars)
				// and the text before it looks like reasoning
				if len(candidate) > 80 {
					before := strings.TrimSpace(s[:i])
					if before != "" && looksLikeReasoning(before+"\n"+candidate) || looksLikeReasoning(s) {
						return candidate, true
					}
				}
			}
		}
	}
	return "", false
}

// extractCleanContent pulls the final clean paragraphs from content that has
// reasoning artifacts mixed in. It looks for the last run of consecutive clean
// prose lines (no bullets, no metadata markers).
func extractCleanContent(s string) string {
	// First try: check if the answer is wrapped in quotes at the end
	if quoted, ok := extractQuotedAnswer(s); ok {
		return quoted
	}

	lines := strings.Split(s, "\n")

	// Walk backwards to find clean paragraph runs
	var runs [][]string
	var currentRun []string

	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			if len(currentRun) > 0 {
				// Reverse currentRun (we built it backwards)
				for l, r := 0, len(currentRun)-1; l < r; l, r = l+1, r-1 {
					currentRun[l], currentRun[r] = currentRun[r], currentRun[l]
				}
				runs = append(runs, currentRun)
				currentRun = nil
			}
			continue
		}

		if reasoningLineRe.MatchString(line) {
			if len(currentRun) > 0 {
				for l, r := 0, len(currentRun)-1; l < r; l, r = l+1, r-1 {
					currentRun[l], currentRun[r] = currentRun[r], currentRun[l]
				}
				runs = append(runs, currentRun)
				currentRun = nil
			}
			continue
		}

		currentRun = append(currentRun, trimmed)
	}
	if len(currentRun) > 0 {
		for l, r := 0, len(currentRun)-1; l < r; l, r = l+1, r-1 {
			currentRun[l], currentRun[r] = currentRun[r], currentRun[l]
		}
		runs = append(runs, currentRun)
	}

	if len(runs) == 0 {
		return s // give up, return original
	}

	// Find the best run(s): longest clean paragraph blocks from the end.
	// Runs are in reverse order (last paragraph first). Collect runs that
	// look like actual prose (at least 40 chars when joined).
	var best [][]string
	for _, run := range runs {
		joined := strings.Join(run, " ")
		if len(joined) >= 40 {
			best = append(best, run)
		}
		// Stop after collecting 2-3 good paragraphs from the end
		totalChars := 0
		for _, b := range best {
			totalChars += len(strings.Join(b, " "))
		}
		if totalChars > 200 {
			break
		}
	}

	if len(best) == 0 {
		return s
	}

	// Reverse best (we collected from end, need start-to-end order)
	for l, r := 0, len(best)-1; l < r; l, r = l+1, r-1 {
		best[l], best[r] = best[r], best[l]
	}

	var paragraphs []string
	for _, run := range best {
		paragraphs = append(paragraphs, strings.Join(run, " "))
	}
	return strings.Join(paragraphs, "\n\n")
}

// rewriteThinkResponse rewrites a non-streaming JSON response when think is
// disabled. It strips <think>...</think> blocks from reasoning_content and
// moves the cleaned remainder into content. If no think tags are found, the
// entire reasoning_content is moved as-is. Returns the original body unchanged
// if no rewriting is needed.
func rewriteThinkResponse(body []byte) []byte {
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return body
	}

	choices, ok := data["choices"].([]any)
	if !ok {
		return body
	}

	modified := false
	for _, choice := range choices {
		cm, ok := choice.(map[string]any)
		if !ok {
			continue
		}
		msg, ok := cm["message"].(map[string]any)
		if !ok {
			continue
		}

		content, _ := msg["content"].(string)
		reasoning, hasReasoning := msg["reasoning_content"].(string)

		// Strip reasoning_content when present
		if hasReasoning {
			delete(msg, "reasoning_content")
			modified = true
		}

		// If content already populated, check for reasoning artifacts in it
		if content != "" {
			if looksLikeReasoning(content) {
				msg["content"] = extractCleanContent(content)
				modified = true
			}
			continue
		}

		if !hasReasoning {
			continue
		}

		if reasoning == "" {
			delete(msg, "reasoning_content")
			modified = true
			continue
		}

		// Strip <think>...</think> blocks and surrounding whitespace
		cleaned := thinkBlockRe.ReplaceAllString(reasoning, "")
		cleaned = strings.TrimSpace(cleaned)
		if cleaned == "" {
			// No think tags found, or everything was inside think tags —
			// use the full text so the client gets something.
			cleaned = strings.TrimSpace(reasoning)
		}
		msg["content"] = cleaned
		delete(msg, "reasoning_content")
		modified = true
	}

	if !modified {
		return body
	}

	result, err := json.Marshal(data)
	if err != nil {
		return body
	}
	return result
}

// streamThinkDisabled streams an SSE response with think-block suppression.
// Tokens inside <think>...</think> are suppressed; tokens after the block are
// emitted as delta.content. If no think tags appear, reasoning_content is
// renamed to content for every chunk.
//
// If the model is truncated (finish_reason: "length") while still inside a
// think block, buffered reasoning tokens are salvaged via extractCleanContent
// and emitted as a final content chunk so the client gets something.
//
// Returns true if the client disconnected early.
func streamThinkDisabled(w http.ResponseWriter, body io.Reader, cancel func()) (clientAborted bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		io.Copy(w, body)
		return false
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	inThinkBlock := false
	var thinkBuf strings.Builder // buffer reasoning tokens for salvage on truncation
	var lastChunkTemplate map[string]any // keep a template for synthetic chunks

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			// Empty lines, SSE comments — pass through
			if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
				cancel()
				return true
			}
			continue
		}

		payload := line[6:]
		if payload == "[DONE]" {
			if _, err := fmt.Fprintf(w, "data: [DONE]\n\n"); err != nil {
				cancel()
				return true
			}
			f.Flush()
			continue
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				cancel()
				return true
			}
			f.Flush()
			continue
		}

		// Save top-level fields (id, model, etc.) for building salvage chunks
		if lastChunkTemplate == nil {
			lastChunkTemplate = make(map[string]any)
			for k, v := range chunk {
				if k != "choices" {
					lastChunkTemplate[k] = v
				}
			}
		}

		choices, ok := chunk["choices"].([]any)
		if !ok {
			if _, err := emitSSE(w, chunk); err != nil {
				cancel()
				return true
			}
			f.Flush()
			continue
		}

		emit := false
		for _, choice := range choices {
			cm, ok := choice.(map[string]any)
			if !ok {
				emit = true
				continue
			}
			delta, ok := cm["delta"].(map[string]any)
			if !ok {
				// No delta — finish_reason-only chunk.
				// If we're in an unclosed think block, salvage before emitting.
				if fr, _ := cm["finish_reason"].(string); fr != "" && inThinkBlock && thinkBuf.Len() > 0 {
					if aborted := emitSalvage(w, f, cancel, &thinkBuf, lastChunkTemplate); aborted {
						return true
					}
					inThinkBlock = false
				}
				emit = true
				continue
			}

			reasoning, hasReasoning := delta["reasoning_content"].(string)
			if !hasReasoning {
				// No reasoning_content — check for finish_reason with unclosed think
				if fr, _ := cm["finish_reason"].(string); fr != "" && inThinkBlock && thinkBuf.Len() > 0 {
					if aborted := emitSalvage(w, f, cancel, &thinkBuf, lastChunkTemplate); aborted {
						return true
					}
					inThinkBlock = false
				}
				// Pass through as-is
				emit = true
				continue
			}

			// If delta already has content, just strip reasoning_content
			if _, hasContent := delta["content"].(string); hasContent {
				delete(delta, "reasoning_content")
				emit = true
				continue
			}

			// Handle <think> open tag
			if strings.Contains(reasoning, "<think>") {
				inThinkBlock = true
				thinkBuf.Reset()
				// Check if </think> is also in this same token
				if strings.Contains(reasoning, "</think>") {
					inThinkBlock = false
					thinkBuf.Reset()
					after := extractAfterThinkClose(reasoning)
					if after != "" {
						delta["content"] = after
						delete(delta, "reasoning_content")
						emit = true
					}
				}
				continue
			}

			// Inside think block — buffer and suppress, or detect close
			if inThinkBlock {
				if strings.Contains(reasoning, "</think>") {
					inThinkBlock = false
					thinkBuf.Reset()
					after := extractAfterThinkClose(reasoning)
					if after != "" {
						delta["content"] = after
						delete(delta, "reasoning_content")
						emit = true
					}
				} else {
					thinkBuf.WriteString(reasoning)
				}
				continue
			}

			// Outside think block — rename reasoning_content → content
			delta["content"] = reasoning
			delete(delta, "reasoning_content")
			emit = true
		}

		if emit {
			if _, err := emitSSE(w, chunk); err != nil {
				log.Printf("[debug] stream think-disabled: client write error: %v", err)
				cancel()
				return true
			}
			f.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[debug] stream think-disabled: scanner error (backend may have died): %v", err)
	}
	return false
}

// emitSalvage extracts clean content from buffered reasoning and emits it
// as a synthetic content delta. Called when a think block is never closed.
func emitSalvage(w http.ResponseWriter, f http.Flusher, cancel func(), buf *strings.Builder, template map[string]any) (clientAborted bool) {
	raw := buf.String()
	buf.Reset()

	// Strip any unclosed <think> tag from the start
	raw = strings.TrimPrefix(raw, "<think>")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}

	salvaged := extractCleanContent(raw)
	if salvaged == "" {
		salvaged = raw
	}

	synthChunk := make(map[string]any, len(template)+1)
	for k, v := range template {
		synthChunk[k] = v
	}
	synthChunk["choices"] = []any{
		map[string]any{
			"delta": map[string]any{
				"content": salvaged,
			},
		},
	}

	if _, err := emitSSE(w, synthChunk); err != nil {
		cancel()
		return true
	}
	f.Flush()
	return false
}

// extractAfterThinkClose returns trimmed text after the first </think> tag.
func extractAfterThinkClose(s string) string {
	idx := strings.Index(s, "</think>")
	if idx < 0 {
		return ""
	}
	return strings.TrimLeft(s[idx+len("</think>"):], "\n\r")
}


func emitSSE(w io.Writer, data any) (int, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	return fmt.Fprintf(w, "data: %s\n\n", b)
}
