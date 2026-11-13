// Package model — Anthropic provider (Messages API).
package model

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const anthropicVersion = "2023-06-01"

type anthropicProvider struct {
	cfg    ProviderConfig
	client *http.Client
}

func (p *anthropicProvider) Name() string { return "anthropic" }

// --- wire types ---

// anthropicContentBlock is one block in a message content array, or in a
// response. Fields are used per block type.
type anthropicContentBlock struct {
	Type string `json:"type"` // text | tool_use | tool_result
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"` // user | assistant
	Content any    `json:"content"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
}

type anthropicResponse struct {
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text,omitempty"`
		ID    string          `json:"id,omitempty"`
		Name  string          `json:"name,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type,omitempty"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// --- conversion ---

// toAnthropicMessages extracts system prompts to a top-level string and
// converts the remaining turns, turning tool results into tool_result
// blocks and assistant tool calls into tool_use blocks.
//
// Consecutive tool messages are merged into a single user turn carrying
// one tool_result block per result: Anthropic requires strictly
// alternating user/assistant turns, so emitting one user turn per tool
// result would produce consecutive user turns and an HTTP 400 on the
// next Chat whenever a turn called two or more tools.
//
// Consecutive same-role messages are merged in general, for the same
// reason: the agent loop can append a plain user message (the
// deferred-call reminder) right after a tool_result user turn, and
// memory can hold two adjacent user turns; either would be a 400
// without the merge. Assistant turns merge the same way.
//
// An error is returned when a tool call's arguments are not valid JSON
// (or are an empty string): silently substituting {} would mask a
// malformed turn and send the model a call it never made.
func toAnthropicMessages(msgs []Message) (system string, out []anthropicMessage, err error) {
	var systemParts []string
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		switch m.Role {
		case "system":
			systemParts = append(systemParts, m.Content)
		case "tool":
			blocks := []anthropicContentBlock{}
			for i < len(msgs) && msgs[i].Role == "tool" {
				blocks = append(blocks, anthropicContentBlock{
					Type:      "tool_result",
					ToolUseID: msgs[i].ToolCallID,
					Content:   msgs[i].Content,
				})
				i++
			}
			i-- // outer loop's i++ moves to the first non-tool message
			out = appendAnthropicMessage(out, anthropicMessage{Role: "user", Content: blocks})
		case "assistant":
			if len(m.ToolCalls) == 0 {
				out = appendAnthropicMessage(out, anthropicMessage{Role: "assistant", Content: m.Content})
				continue
			}
			blocks := make([]anthropicContentBlock, 0, len(m.ToolCalls)+1)
			if m.Content != "" {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(`{}`)
				if strings.TrimSpace(tc.Arguments) != "" {
					var raw json.RawMessage
					if jerr := json.Unmarshal([]byte(tc.Arguments), &raw); jerr != nil {
						return "", nil, fmt.Errorf("anthropic: invalid JSON arguments for tool %q: %w", tc.Name, jerr)
					}
					input = raw
				}
				blocks = append(blocks, anthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: input,
				})
			}
			out = appendAnthropicMessage(out, anthropicMessage{Role: "assistant", Content: blocks})
		default:
			// "user" is passed through as a user turn. Any unknown role
			// is also mapped to user: Anthropic only accepts
			// user/assistant (plus extracted system), so passing an
			// unknown role verbatim would produce a 400.
			out = appendAnthropicMessage(out, anthropicMessage{Role: "user", Content: m.Content})
		}
	}
	return strings.Join(systemParts, "\n"), out, nil
}

// appendAnthropicMessage appends a turn, merging it into the previous
// turn when both have the same role, so the converted conversation
// always strictly alternates user/assistant as Anthropic requires.
func appendAnthropicMessage(out []anthropicMessage, m anthropicMessage) []anthropicMessage {
	if n := len(out); n > 0 && out[n-1].Role == m.Role {
		out[n-1].Content = mergeAnthropicContent(out[n-1].Content, m.Content)
		return out
	}
	return append(out, m)
}

// mergeAnthropicContent concatenates the content of two same-role
// turns. String + string joins with a newline; block content gains
// the other's text as a text block (a tool_result turn followed by a
// plain reminder becomes one user turn with a tool_result block and
// a text block). Content here is only ever a string or a
// []anthropicContentBlock.
func mergeAnthropicContent(a, b any) any {
	aStr, aIsStr := a.(string)
	bStr, bIsStr := b.(string)
	switch {
	case aIsStr && bIsStr:
		if aStr == "" {
			return bStr
		}
		if bStr == "" {
			return aStr
		}
		return aStr + "\n" + bStr
	case aIsStr:
		blocks := []anthropicContentBlock{}
		if aStr != "" {
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: aStr})
		}
		return append(blocks, b.([]anthropicContentBlock)...)
	default:
		blocks := a.([]anthropicContentBlock)
		if bIsStr {
			if bStr == "" {
				return blocks
			}
			return append(blocks, anthropicContentBlock{Type: "text", Text: bStr})
		}
		return append(blocks, b.([]anthropicContentBlock)...)
	}
}

func toAnthropicTools(tools []ToolDefinition) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return out
}

func mapAnthropicStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		if reason == "" {
			return "stop"
		}
		return reason
	}
}

func (p *anthropicProvider) headers() map[string]string {
	return map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         p.cfg.APIKey,
		"anthropic-version": anthropicVersion,
	}
}

func (p *anthropicProvider) buildRequest(req ChatRequest, stream bool) (anthropicRequest, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	system, messages, err := toAnthropicMessages(req.Messages)
	if err != nil {
		return anthropicRequest{}, err
	}
	if len(messages) == 0 {
		// A system prompt alone is not a conversation: Anthropic
		// rejects an empty messages array with a 400, so fail
		// here with a clear error instead of sending it.
		return anthropicRequest{}, errors.New("anthropic: request has no messages (a system prompt alone is not a conversation)")
	}
	r := anthropicRequest{
		Model:     p.cfg.Model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  messages,
		Tools:     toAnthropicTools(req.Tools),
		Stream:    stream,
	}
	if req.Temperature != nil {
		r.Temperature = req.Temperature
	}
	return r, nil
}

// Chat performs one Anthropic Messages API call.
func (p *anthropicProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	built, err := p.buildRequest(req, false)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(built)
	if err != nil {
		return nil, fmt.Errorf("anthropic: encoding request: %w", err)
	}
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/messages"
	status, body, err := doWithRetry(ctx, p.client, http.MethodPost, url, p.headers(), payload)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, apiError("anthropic", status, body)
	}
	var resp anthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("anthropic: decoding response: %w (body: %s)", err, bodyExcerpt(body))
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return nil, fmt.Errorf("anthropic: API error: %s", resp.Error.Message)
	}
	msg := Message{Role: "assistant"}
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			msg.Content += block.Text
		case "tool_use":
			args := "{}"
			if len(block.Input) > 0 {
				args = string(block.Input)
			}
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: args,
			})
		}
	}
	usage := estimateUsage(req, msg, Usage{
		PromptTokens:     resp.Usage.InputTokens,
		CompletionTokens: resp.Usage.OutputTokens,
		TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
	})
	return &ChatResponse{
		Message:      msg,
		FinishReason: mapAnthropicStopReason(resp.StopReason),
		Usage:        usage,
	}, nil
}

// Stream performs a real Anthropic SSE streaming completion: text
// deltas are delivered to fn as they arrive and tool_use blocks are
// assembled from content_block_start / content_block_delta events
// (input_json_delta fragments are concatenated into the arguments
// object). It returns the full response — content, tool calls, usage
// and finish reason — once the turn completes. The stream setup is
// retried on 429/5xx, like Chat; once events flow, a failure is
// reported, never silently restarted. If the stream yields nothing
// usable it falls back to a single Chat call and returns that
// response, so the caller never gets an empty reply with a nil error.
func (p *anthropicProvider) Stream(ctx context.Context, req ChatRequest, fn func(chunk string) error) (*ChatResponse, error) {
	if fn == nil {
		return nil, fmt.Errorf("anthropic: Stream requires a non-nil chunk callback")
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
		return nil, fmt.Errorf("anthropic: encoding stream request: %w", err)
	}
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/messages"
	headers := p.headers()
	headers["Accept"] = "text/event-stream"
	// totalTimeout bounds the whole Stream call — every setup
	// attempt, the scan, and the Chat fallback — so a stalled
	// provider can never eat the agent step's whole budget.
	ctx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resp, err := doStreamRequest(ctx, p.client, http.MethodPost, url, headers, payload)
		if err != nil {
			return nil, fmt.Errorf("anthropic: stream request failed: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
			resp.Body.Close()
			return nil, apiError("anthropic", resp.StatusCode, body)
		}

		// Accumulate one entry per content block, keyed by its stream index.
		type streamBlock struct {
			typ   string // text | tool_use
			id    string
			name  string
			text  strings.Builder
			input strings.Builder
		}
		var blocks []*streamBlock
		blockAt := func(index int) *streamBlock {
			for len(blocks) <= index {
				blocks = append(blocks, &streamBlock{})
			}
			return blocks[index]
		}
		inputTokens, outputTokens := 0, 0
		stopReason := ""
		emitted := 0

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		event := ""
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				// A blank line ends one SSE event: the event type
				// must not leak into the next block, or a data-only
				// line would be misrouted to the previous event.
				event = ""
				continue
			}
			if strings.HasPrefix(line, "event:") {
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			switch event {
			case "message_start":
				var ev struct {
					Message struct {
						Usage struct {
							InputTokens int `json:"input_tokens"`
						} `json:"usage"`
					} `json:"message"`
				}
				if err := json.Unmarshal([]byte(data), &ev); err == nil {
					inputTokens = ev.Message.Usage.InputTokens
				}
			case "content_block_start":
				var ev struct {
					Index        int `json:"index"`
					ContentBlock struct {
						Type  string          `json:"type"`
						ID    string          `json:"id,omitempty"`
						Name  string          `json:"name,omitempty"`
						Input json.RawMessage `json:"input,omitempty"`
					} `json:"content_block"`
				}
				if err := json.Unmarshal([]byte(data), &ev); err != nil {
					continue
				}
				b := blockAt(ev.Index)
				b.typ = ev.ContentBlock.Type
				b.id = ev.ContentBlock.ID
				b.name = ev.ContentBlock.Name
				if len(ev.ContentBlock.Input) > 0 {
					b.input.Write(ev.ContentBlock.Input)
				}
			case "content_block_delta":
				var ev struct {
					Index int `json:"index"`
					Delta struct {
						Type        string `json:"type"`
						Text        string `json:"text,omitempty"`
						PartialJSON string `json:"partial_json,omitempty"`
					} `json:"delta"`
				}
				if err := json.Unmarshal([]byte(data), &ev); err != nil {
					continue
				}
				b := blockAt(ev.Index)
				switch ev.Delta.Type {
				case "text_delta":
					if ev.Delta.Text != "" {
						emitted++
						b.text.WriteString(ev.Delta.Text)
						if err := fn(ev.Delta.Text); err != nil {
							resp.Body.Close()
							return nil, err
						}
					}
				case "input_json_delta":
					b.input.WriteString(ev.Delta.PartialJSON)
				}
			case "message_delta":
				var ev struct {
					Delta struct {
						StopReason string `json:"stop_reason,omitempty"`
					} `json:"delta"`
					Usage struct {
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal([]byte(data), &ev); err == nil {
					if ev.Delta.StopReason != "" {
						stopReason = ev.Delta.StopReason
					}
					if ev.Usage.OutputTokens != 0 {
						outputTokens = ev.Usage.OutputTokens
					}
				}
			}
		}
		scanErr := scanner.Err()
		resp.Body.Close()
		// A tool_use block counts as usable only when it has a name:
		// a bare content_block_start with no name can never execute.
		haveToolCalls := false
		for _, b := range blocks {
			if b.typ == "tool_use" && strings.TrimSpace(b.name) != "" {
				haveToolCalls = true
				break
			}
		}
		if scanErr != nil {
			if isProgressTimeout(scanErr) && emitted == 0 && !haveToolCalls && attempt < maxAttempts-1 {
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
			// A truncated stream with usable chunks already delivered is not
			// a failure; only fall back when nothing was delivered.
			if emitted == 0 && !haveToolCalls {
				return p.streamFallback(ctx, req, fn)
			}
			return nil, fmt.Errorf("anthropic: reading stream: %w", scanErr)
		}
		var content strings.Builder
		var toolCalls []ToolCall
		for _, b := range blocks {
			switch b.typ {
			case "text":
				content.WriteString(b.text.String())
			case "tool_use":
				args := strings.TrimSpace(b.input.String())
				if args == "" {
					args = "{}"
				}
				toolCalls = append(toolCalls, ToolCall{ID: b.id, Name: b.name, Arguments: args})
			}
		}
		if emitted == 0 && len(toolCalls) == 0 {
			// Nothing usable was streamed. Fall back to a plain Chat so
			// the caller still gets an answer instead of an empty reply.
			return p.streamFallback(ctx, req, fn)
		}
		msg := Message{Role: "assistant", Content: content.String(), ToolCalls: toolCalls}
		usage := estimateUsage(req, msg, Usage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			TotalTokens:      inputTokens + outputTokens,
		})
		return &ChatResponse{
			Message:      msg,
			FinishReason: mapAnthropicStopReason(stopReason),
			Usage:        usage,
		}, nil
	}
	return nil, fmt.Errorf("anthropic: stream failed after %d attempts", maxAttempts)
}

// streamFallback runs a plain Chat and emits its content as one chunk,
// returning the full Chat response (including any tool calls it holds).
func (p *anthropicProvider) streamFallback(ctx context.Context, req ChatRequest, fn func(chunk string) error) (*ChatResponse, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: stream fallback failed: %w", err)
	}
	if resp.Message.Content != "" {
		if err := fn(resp.Message.Content); err != nil {
			return nil, err
		}
	}
	return resp, nil
}
