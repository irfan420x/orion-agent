// Package model — Ollama provider (local /api/chat endpoint).
package model

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type ollamaProvider struct {
	cfg    ProviderConfig
	client *http.Client
}

func (p *ollamaProvider) Name() string { return "ollama" }

// --- wire types ---

type ollamaMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// ollamaToolCall is a tool call in Ollama's wire shape. Unlike OpenAI,
// Ollama expects function arguments as a JSON object, not a string, so
// Arguments is a RawMessage holding the object itself.
type ollamaToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"` // "function"
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []openAITool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaChatResponse struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
		// Ollama tool_calls carry arguments as a JSON object (not a string).
		ToolCalls []struct {
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls,omitempty"`
	} `json:"message"`
	DoneReason string `json:"done_reason,omitempty"`
	Done       bool   `json:"done"`
	Error      string `json:"error,omitempty"`
	// Token counts, when the server reports them.
	PromptEvalCount int `json:"prompt_eval_count,omitempty"`
	EvalCount       int `json:"eval_count,omitempty"`
}

// --- conversion ---

// toOllamaMessages converts history to Ollama's wire shape, sending
// tool-call arguments as JSON objects (what Ollama expects), not
// strings. An error is returned when a tool call's arguments are not
// valid JSON: silently substituting {} would mask a malformed turn.
func toOllamaMessages(msgs []Message) ([]ollamaMessage, error) {
	out := make([]ollamaMessage, 0, len(msgs))
	for _, m := range msgs {
		om := ollamaMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			args := strings.TrimSpace(tc.Arguments)
			if args == "" {
				args = "{}"
			}
			var raw json.RawMessage
			if err := json.Unmarshal([]byte(args), &raw); err != nil {
				return nil, fmt.Errorf("ollama: invalid JSON arguments for tool %q: %w", tc.Name, err)
			}
			var call ollamaToolCall
			call.ID = tc.ID
			call.Type = "function"
			call.Function.Name = tc.Name
			call.Function.Arguments = raw
			om.ToolCalls = append(om.ToolCalls, call)
		}
		out = append(out, om)
	}
	return out, nil
}

// ollamaToolArguments converts a ToolCall's JSON-string arguments into the
// form Ollama expects. Ollama accepts either an object or a string; an
// object is preferred when the arguments parse as one.
func ollamaToolArguments(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	// If it is a JSON string, unwrap it to the inner JSON text.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return "{}"
		}
		return s
	}
	return string(raw)
}

func (p *ollamaProvider) buildRequest(req ChatRequest, stream bool) (ollamaChatRequest, error) {
	messages, err := toOllamaMessages(req.Messages)
	if err != nil {
		return ollamaChatRequest{}, err
	}
	r := ollamaChatRequest{
		Model:    p.cfg.Model,
		Messages: messages,
		Tools:    toOpenAITools(req.Tools),
		Stream:   stream,
	}
	if req.Temperature != nil {
		r.Options = map[string]any{"temperature": *req.Temperature}
	}
	return r, nil
}

// Chat performs one Ollama /api/chat call (stream:false).
func (p *ollamaProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	built, err := p.buildRequest(req, false)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(built)
	if err != nil {
		return nil, fmt.Errorf("ollama: encoding request: %w", err)
	}
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/api/chat"
	status, body, err := doWithRetry(ctx, p.client, http.MethodPost, url, map[string]string{"Content-Type": "application/json"}, payload)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, apiError("ollama", status, body)
	}
	var resp ollamaChatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("ollama: decoding response: %w (body: %s)", err, bodyExcerpt(body))
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("ollama: API error: %s", resp.Error)
	}
	msg := Message{Role: resp.Message.Role, Content: resp.Message.Content}
	if msg.Role == "" {
		msg.Role = "assistant"
	}
	for i, tc := range resp.Message.ToolCalls {
		// Ollama does not return tool-call IDs. Synthesise a stable
		// one per call (call-1, call-2, ...): an empty ID breaks
		// tool-result linkage when the turn is converted back for
		// OpenAI/Anthropic, which require a tool_call_id /
		// tool_use_id. The agent loop also synthesises IDs centrally
		// for any provider that omits them.
		id := fmt.Sprintf("call-%d", i+1)
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:        id,
			Name:      tc.Function.Name,
			Arguments: ollamaToolArguments(tc.Function.Arguments),
		})
	}
	finish := "stop"
	if len(msg.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	// DoneReason describes how the turn ended; it must not overwrite
	// the tool_calls finish reason for a tool-call turn.
	if resp.DoneReason != "" && len(msg.ToolCalls) == 0 {
		finish = resp.DoneReason
	}
	usage := estimateUsage(req, msg, Usage{
		PromptTokens:     resp.PromptEvalCount,
		CompletionTokens: resp.EvalCount,
		TotalTokens:      resp.PromptEvalCount + resp.EvalCount,
	})
	return &ChatResponse{Message: msg, FinishReason: finish, Usage: usage}, nil
}

// Stream performs a streaming Ollama completion, parsing the
// newline-delimited JSON lines and calling fn per content delta, and
// returns the full response: accumulated content, any tool calls
// (parsed from the streamed message.tool_calls, with IDs synthesised
// as call-1, call-2, ... since Ollama returns none), usage and the
// finish reason. DoneReason never overwrites a tool_calls finish
// reason. The stream setup is retried on 429/5xx, like Chat; once
// lines flow, a failure is reported, never silently restarted. A
// stream that yields nothing usable falls back to a single Chat call.
func (p *ollamaProvider) Stream(ctx context.Context, req ChatRequest, fn func(chunk string) error) (*ChatResponse, error) {
	if fn == nil {
		return nil, fmt.Errorf("ollama: Stream requires a non-nil chunk callback")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	built, err := p.buildRequest(req, true)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(built)
	if err != nil {
		return nil, fmt.Errorf("ollama: encoding stream request: %w", err)
	}
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/api/chat"
	// totalTimeout bounds the whole Stream call — every setup
	// attempt, the scan, and the Chat fallback — so a stalled
	// provider can never eat the agent step's whole budget.
	ctx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resp, err := doStreamRequest(ctx, p.client, http.MethodPost, url, map[string]string{"Content-Type": "application/json"}, payload)
		if err != nil {
			return nil, fmt.Errorf("ollama: stream request failed: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
			resp.Body.Close()
			return nil, apiError("ollama", resp.StatusCode, body)
		}

		var content strings.Builder
		var toolCalls []ToolCall
		role := ""
		doneReason := ""
		promptEval, evalCount := 0, 0
		emitted := 0
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var chunk ollamaChatResponse
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				// A single malformed line must not kill a stream that
				// is otherwise flowing (cf. OpenAI, which skips bad
				// SSE chunks): skip it and keep reading. A stream of
				// nothing but garbage still falls back to Chat below.
				continue
			}
			if chunk.Error != "" {
				resp.Body.Close()
				return nil, fmt.Errorf("ollama: API error: %s", chunk.Error)
			}
			if chunk.Message.Role != "" {
				role = chunk.Message.Role
			}
			if chunk.Message.Content != "" {
				emitted++
				content.WriteString(chunk.Message.Content)
				if err := fn(chunk.Message.Content); err != nil {
					resp.Body.Close()
					return nil, err
				}
			}
			for _, tc := range chunk.Message.ToolCalls {
				call := ToolCall{
					Name:      tc.Function.Name,
					Arguments: ollamaToolArguments(tc.Function.Arguments),
				}
				// Ollama may repeat the full tool-call list in the
				// final done chunk: an identical (name, arguments)
				// call already recorded is the same call, not a
				// second one. Without the dedupe the agent defers a
				// phantom duplicate and later reminds the model to
				// perform a call it already executed.
				dup := false
				for _, have := range toolCalls {
					if have.Name == call.Name && have.Arguments == call.Arguments {
						dup = true
						break
					}
				}
				if !dup {
					toolCalls = append(toolCalls, call)
				}
			}
			if chunk.PromptEvalCount != 0 {
				promptEval = chunk.PromptEvalCount
			}
			if chunk.EvalCount != 0 {
				evalCount = chunk.EvalCount
			}
			if chunk.DoneReason != "" {
				doneReason = chunk.DoneReason
			}
			// Break on done regardless of whether this chunk carried
			// content: a done:true chunk with empty content must still
			// end the loop, or it waits for EOF / the context deadline.
			if chunk.Done {
				break
			}
		}
		scanErr := scanner.Err()
		resp.Body.Close()
		if scanErr != nil {
			if isProgressTimeout(scanErr) && emitted == 0 && len(toolCalls) == 0 && attempt < maxAttempts-1 {
				// The stream went silent before delivering
				// anything usable: the attempt is worthless
				// and safe to repeat — nothing was shown to
				// the caller yet, so re-running the stream
				// cannot duplicate a live chunk. Set it up
				// again.
				if berr := sleepBackoff(ctx, backoffDelays[attempt]); berr != nil {
					return nil, berr
				}
				continue
			}
			if emitted == 0 && len(toolCalls) == 0 {
				return p.streamFallback(ctx, req, fn)
			}
			return nil, fmt.Errorf("ollama: reading stream: %w", scanErr)
		}
		if emitted == 0 && len(toolCalls) == 0 {
			// Nothing usable was streamed. Fall back to a plain Chat so
			// the caller still gets an answer instead of an empty reply.
			return p.streamFallback(ctx, req, fn)
		}
		// Ollama returns no tool-call IDs; synthesise stable ones.
		for i := range toolCalls {
			if toolCalls[i].ID == "" {
				toolCalls[i].ID = fmt.Sprintf("call-%d", i+1)
			}
		}
		if role == "" {
			role = "assistant"
		}
		finish := "stop"
		if len(toolCalls) > 0 {
			finish = "tool_calls"
		} else if doneReason != "" {
			// DoneReason describes how the turn ended; it must not
			// overwrite the tool_calls finish reason for a tool-call turn.
			finish = doneReason
		}
		msg := Message{Role: role, Content: content.String(), ToolCalls: toolCalls}
		usage := estimateUsage(req, msg, Usage{
			PromptTokens:     promptEval,
			CompletionTokens: evalCount,
			TotalTokens:      promptEval + evalCount,
		})
		return &ChatResponse{Message: msg, FinishReason: finish, Usage: usage}, nil
	}
	return nil, fmt.Errorf("ollama: stream failed after %d attempts", maxAttempts)
}

// streamFallback runs a plain Chat and emits its content as one chunk,
// returning the full Chat response (including any tool calls it holds).
func (p *ollamaProvider) streamFallback(ctx context.Context, req ChatRequest, fn func(chunk string) error) (*ChatResponse, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("ollama: stream fallback failed: %w", err)
	}
	if resp.Message.Content != "" {
		if err := fn(resp.Message.Content); err != nil {
			return nil, err
		}
	}
	return resp, nil
}
