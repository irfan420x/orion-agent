// Package model — OpenAI-compatible provider.
//
// Covers OpenAI itself and any OpenAI-compatible endpoint (vLLM,
// OpenRouter, Hermes-class open models, etc.) reached through the
// configured BaseURL.
package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type openAIProvider struct {
	cfg    ProviderConfig
	client *http.Client
}

func (p *openAIProvider) Name() string { return "openai" }

// --- wire types ---

type openAIFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type openAITool struct {
	Type     string         `json:"type"` // "function"
	Function openAIFunction `json:"function"`
}

type openAIToolCall struct {
	ID       string                 `json:"id"`
	Index    int                    `json:"index,omitempty"` // stream deltas only
	Type     string                 `json:"type,omitempty"`  // "function"
	Function openAIToolCallFunction `json:"function"`
}

// openAIToolCallFunction is one function call. Arguments is normally a
// JSON string holding a JSON object, but some OpenAI-compatible servers
// (vLLM, Ollama's OpenAI-compat endpoint, Hermes-class servers) send
// it as a JSON object instead. UnmarshalJSON accepts both shapes (plus
// null/missing) and always normalises to the JSON-object string the
// rest of the code expects, so a wire variant cannot break the loop.
type openAIToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (f *openAIToolCallFunction) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	f.Name = raw.Name
	trimmed := bytes.TrimSpace(raw.Arguments)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		f.Arguments = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		f.Arguments = s
		return nil
	}
	// Object/array/number form: keep the compact JSON text.
	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return err
	}
	compact, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f.Arguments = string(compact)
	return nil
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIChatRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Tools       []openAITool    `json:"tools,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message struct {
			Role      string           `json:"role"`
			Content   string           `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type,omitempty"`
		Code    string `json:"code,omitempty"`
	} `json:"error,omitempty"`
}

type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string           `json:"content,omitempty"`
			ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason,omitempty"`
	} `json:"choices"`
}

// --- conversion ---

func toOpenAIMessages(msgs []Message) []openAIMessage {
	out := make([]openAIMessage, 0, len(msgs))
	for _, m := range msgs {
		om := openAIMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			args := tc.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			var call openAIToolCall
			call.ID = tc.ID
			call.Type = "function"
			call.Function.Name = tc.Name
			call.Function.Arguments = args
			om.ToolCalls = append(om.ToolCalls, call)
		}
		out = append(out, om)
	}
	return out
}

func toOpenAITools(tools []ToolDefinition) []openAITool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openAITool, 0, len(tools))
	for _, t := range tools {
		out = append(out, openAITool{
			Type: "function",
			Function: openAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return out
}

func (p *openAIProvider) buildRequest(req ChatRequest, stream bool) openAIChatRequest {
	r := openAIChatRequest{
		Model:    p.cfg.Model,
		Messages: toOpenAIMessages(req.Messages),
		Tools:    toOpenAITools(req.Tools),
		Stream:   stream,
	}
	if req.Temperature != nil {
		r.Temperature = req.Temperature
	}
	if req.MaxTokens != 0 {
		m := req.MaxTokens
		r.MaxTokens = &m
	}
	return r
}

func (p *openAIProvider) headers() map[string]string {
	return map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + p.cfg.APIKey,
	}
}

// Chat performs one OpenAI-compatible chat completion.
func (p *openAIProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(p.buildRequest(req, false))
	if err != nil {
		return nil, fmt.Errorf("openai: encoding request: %w", err)
	}
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/chat/completions"
	status, body, err := doWithRetry(ctx, p.client, http.MethodPost, url, p.headers(), payload)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, apiError("openai", status, body)
	}
	var resp openAIChatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("openai: decoding response: %w (body: %s)", err, bodyExcerpt(body))
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return nil, fmt.Errorf("openai: API error: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("openai: response contained no choices")
	}
	choice := resp.Choices[0]
	msg := Message{Role: choice.Message.Role, Content: choice.Message.Content}
	if msg.Role == "" {
		msg.Role = "assistant"
	}
	for _, tc := range choice.Message.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	usage := estimateUsage(req, msg, Usage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
	})
	return &ChatResponse{Message: msg, FinishReason: choice.FinishReason, Usage: usage}, nil
}

// Stream performs a streaming completion, calling fn for each content
// delta as it arrives, and returns the full response: the accumulated
// content, any tool calls the model requested, and the finish reason.
// Tool-call deltas are merged by their stream index (arguments arrive
// as fragments and are concatenated). The stream setup is retried on
// 429/5xx, like Chat; once chunks flow, a failure is reported, never
// silently restarted. If the SSE stream yields nothing usable (no
// content and no tool calls — e.g. role-only deltas), it falls back to
// a single Chat call and returns that response instead, so the caller
// never gets an empty reply with a nil error.
func (p *openAIProvider) Stream(ctx context.Context, req ChatRequest, fn func(chunk string) error) (*ChatResponse, error) {
	if fn == nil {
		return nil, fmt.Errorf("openai: Stream requires a non-nil chunk callback")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(p.buildRequest(req, true))
	if err != nil {
		return nil, fmt.Errorf("openai: encoding stream request: %w", err)
	}
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/chat/completions"
	headers := p.headers()
	headers["Accept"] = "text/event-stream"
	resp, err := doStreamRequest(ctx, p.client, http.MethodPost, url, headers, payload)
	if err != nil {
		return nil, fmt.Errorf("openai: stream request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return nil, apiError("openai", resp.StatusCode, body)
	}

	var content strings.Builder
	var toolCalls []openAIToolCall
	finishReason := ""
	emitted := 0
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				emitted++
				content.WriteString(choice.Delta.Content)
				if err := fn(choice.Delta.Content); err != nil {
					return nil, err
				}
			}
			if len(choice.Delta.ToolCalls) > 0 {
				mergeStreamToolCalls(&toolCalls, choice.Delta.ToolCalls)
			}
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		// A truncated stream with usable chunks already delivered is not
		// a failure; only fall back when nothing was delivered.
		if emitted == 0 && len(toolCalls) == 0 {
			return p.streamFallback(ctx, req, fn)
		}
		return nil, fmt.Errorf("openai: reading stream: %w", err)
	}
	if emitted == 0 && len(toolCalls) == 0 {
		// Nothing usable was streamed — including a valid SSE stream
		// of role-only (empty) deltas, which previously returned nil
		// having delivered nothing. Fall back to a plain Chat so the
		// caller still gets an answer instead of an empty reply.
		return p.streamFallback(ctx, req, fn)
	}
	msg := Message{Role: "assistant", Content: content.String()}
	for _, tc := range toolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	if finishReason == "" {
		if len(msg.ToolCalls) > 0 {
			finishReason = "tool_calls"
		} else {
			finishReason = "stop"
		}
	}
	// Streaming responses carry no usage unless stream_options is set;
	// estimate the fields the provider left at zero.
	usage := estimateUsage(req, msg, Usage{})
	return &ChatResponse{Message: msg, FinishReason: finishReason, Usage: usage}, nil
}

// mergeStreamToolCalls merges one SSE delta's tool calls into the
// accumulated list, keyed by the delta index. Arguments arrive as
// fragments across deltas and are concatenated; the id and function
// name arrive (whole) with the first delta for an index.
func mergeStreamToolCalls(dst *[]openAIToolCall, deltas []openAIToolCall) {
	for _, d := range deltas {
		idx := d.Index
		if idx < 0 {
			idx = 0
		}
		for len(*dst) <= idx {
			*dst = append(*dst, openAIToolCall{Type: "function"})
		}
		cur := &(*dst)[idx]
		if d.ID != "" {
			cur.ID = d.ID
		}
		if d.Function.Name != "" {
			cur.Function.Name = d.Function.Name
		}
		cur.Function.Arguments += d.Function.Arguments
	}
}

// streamFallback runs a plain Chat and emits its content as one chunk,
// returning the full Chat response (including any tool calls it holds).
func (p *openAIProvider) streamFallback(ctx context.Context, req ChatRequest, fn func(chunk string) error) (*ChatResponse, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("openai: stream fallback failed: %w", err)
	}
	if resp.Message.Content != "" {
		if err := fn(resp.Message.Content); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func joinContents(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}
