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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type openAIProvider struct {
	// name is the provider name reported by Name() and used in error
	// labels. Empty means "openai" (the default); the DeepSeek and
	// OpenRouter cases in NewProvider set it to "deepseek" /
	// "openrouter" so a DeepSeek or OpenRouter endpoint is never
	// mislabelled as OpenAI.
	name   string
	cfg    ProviderConfig
	client *http.Client
}

// Name reports the provider name: "openai" by default, "deepseek"
// when this client was built for a DeepSeek endpoint, "openrouter"
// when built for an OpenRouter endpoint.
func (p *openAIProvider) Name() string {
	if p.name != "" {
		return p.name
	}
	return "openai"
}

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
	Role string `json:"role"`
	// Content is a plain string when the message carries no images
	// (the historic wire shape, unchanged), or a []openAIContentPart
	// (text + image_url parts) when it does. It is any — not a
	// custom marshaller — so the no-image case serialises exactly
	// as before and no provider ever sees a parts array it did
	// not ask for.
	Content    any              `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// openAIContentPart is one block in a multi-part message content
// (OpenAI Chat Completions format, which DeepSeek's Vision guide
// documents for image input): a text block, or an image_url block
// carrying a base64 data URL.
type openAIContentPart struct {
	Type     string          `json:"type"` // "text" | "image_url"
	Text     string          `json:"text,omitempty"`
	ImageURL *openAIImageURL `json:"image_url,omitempty"`
}

// openAIImageURL is the payload of an image_url content part: a data
// URL ("data:<mime>;base64,..."), so no image ever leaves the machine
// except inside the provider request it belongs to.
type openAIImageURL struct {
	URL string `json:"url"`
}

// imagePlaceholder marks an older message whose image was stripped
// by the history rule (see toOpenAIMessages): the turn still shows
// that a photo was sent, without re-sending its bytes.
const imagePlaceholder = "[ছবি]"

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
	Error *openAIAPIError `json:"error,omitempty"`
}

type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string           `json:"content,omitempty"`
			ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason,omitempty"`
	} `json:"choices"`
	// Error is set on SSE error events (data: {"error": {...}}): a
	// provider-side failure delivered inside a 200 stream. It must
	// surface as an error, never be skipped as an unusable chunk —
	// skipping it let the Chat fallback silently mask a failure the
	// provider explicitly reported (Audit 3 stress).
	Error *openAIAPIError `json:"error,omitempty"`
}

// openAIAPIError is one provider error object. Message may be empty
// (some servers send {"error":{"type":...}} with no message); the
// error is still an error — describe() falls back to type/code so an
// empty message can never make an error object look like success.
type openAIAPIError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
}

func (e *openAIAPIError) describe() string {
	if e == nil {
		return "unspecified error"
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Type != "" && e.Code != "" {
		return e.Type + " (" + e.Code + ")"
	}
	if e.Type != "" {
		return e.Type
	}
	if e.Code != "" {
		return e.Code
	}
	return "unspecified error"
}

// --- conversion ---

// toOpenAIMessages converts provider-neutral messages to the
// OpenAI-compatible wire shape.
//
// History rule (vision): only the MOST RECENT user message may carry
// image parts. An older message that still holds images is sent as
// text-only — its text plus the "[ছবি]" placeholder — so a photo is
// never re-sent (as base64) on every later turn: that would
// re-charge the image tokens and bloat the context on each step.
// Tool messages keep their screenshot images (rendered by
// openAIContent; core eviction bounds them to the last 3).
// Images on assistant messages are dropped the same way as older
// photos (DeepSeek rejects images in assistant messages with a
// 400); the placeholder keeps the transcript honest about what
// was sent.
func toOpenAIMessages(msgs []Message) []openAIMessage {
	// Find the last user message that carries usable images.
	lastImageIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" && len(msgs[i].Images) > 0 {
			lastImageIdx = i
			break
		}
	}
	out := make([]openAIMessage, 0, len(msgs))
	for i, m := range msgs {
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
		if len(m.Images) == 0 {
			out = append(out, om)
			continue
		}
		if m.Role == "tool" {
			// Tool screenshot images: text plus one image_url
			// part per usable image (see openAIContent). The
			// eviction in core keeps at most the last 3
			// screenshot images in the request, so tool images
			// stay bounded.
			om.Content = openAIContent(m)
			out = append(out, om)
			continue
		}
		if i == lastImageIdx {
			// Most recent user message with images: text part
			// (when there is text) plus one image_url part per
			// image. An image whose bytes are empty or whose
			// type cannot be sniffed is skipped, never sent
			// with a guessed MIME type; the MIME type is
			// sniffed from the bytes when not already set.
			parts := make([]openAIContentPart, 0, len(m.Images)+1)
			if m.Content != "" {
				parts = append(parts, openAIContentPart{Type: "text", Text: m.Content})
			}
			for _, img := range m.Images {
				if len(img.Data) == 0 {
					continue
				}
				// The MIME type is always sniffed from the
				// bytes: a declared type is never trusted.
				// An image whose type cannot be sniffed is
				// skipped, never sent with a guessed type.
				mime := SniffImageMIME(img.Data)
				if mime == "" {
					continue
				}
				parts = append(parts, openAIContentPart{
					Type:     "image_url",
					ImageURL: &openAIImageURL{URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data)},
				})
			}
			if len(parts) == 0 {
				// No text and no sendable image: plain empty
				// string, as before.
				out = append(out, om)
				continue
			}
			if len(parts) == 1 && parts[0].Type == "text" {
				// Only text survived (all images unusable):
				// keep the historic string shape.
				out = append(out, om)
				continue
			}
			om.Content = parts
			out = append(out, om)
			continue
		}
		// Older (or non-user, non-tool) message with images: text-only
		// with the placeholder, so the transcript records that
		// a photo was part of this turn without re-sending it.
		if m.Content != "" {
			om.Content = m.Content + " " + imagePlaceholder
		} else {
			om.Content = imagePlaceholder
		}
		out = append(out, om)
	}
	return out
}

// openAIContent renders a message's content for the wire: a plain
// string when there are no images, otherwise content parts — the
// text first, then one image_url part per image (base64 data URL).
// The MIME type is always sniffed from the bytes (see
// SniffImageMIME): a declared type is never trusted, and an image
// whose type cannot be sniffed (or whose data is empty) is skipped,
// so a malformed image degrades to text instead of producing a 400;
// when every image is skipped the content is the plain string.
func openAIContent(m Message) any {
	if len(m.Images) == 0 {
		return m.Content
	}
	parts := []openAIContentPart{}
	if strings.TrimSpace(m.Content) != "" {
		parts = append(parts, openAIContentPart{Type: "text", Text: m.Content})
	}
	for _, img := range m.Images {
		if len(img.Data) == 0 {
			continue
		}
		mime := SniffImageMIME(img.Data)
		if mime == "" {
			continue
		}
		parts = append(parts, openAIContentPart{
			Type:     "image_url",
			ImageURL: &openAIImageURL{URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data)},
		})
	}
	if len(parts) == 0 {
		return m.Content
	}
	// A text-only parts list is still returned as parts when an
	// image was present but skipped: the caller already decided
	// this message is image-bearing, and a single text part is a
	// valid content shape.
	return parts
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
		return nil, fmt.Errorf("%s: encoding request: %w", p.Name(), err)
	}
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/chat/completions"
	status, body, err := doWithRetry(ctx, p.client, http.MethodPost, url, p.headers(), payload)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, apiError(p.Name(), status, body)
	}
	var resp openAIChatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("%s: decoding response: %w (body: %s)", p.Name(), err, bodyExcerpt(body))
	}
	if resp.Error != nil {
		// Any error object is a failure, even with an empty
		// message: a present-but-empty error used to fall through
		// and return the choices as success (Audit 3 stress).
		return nil, fmt.Errorf("%s: API error: %s", p.Name(), resp.Error.describe())
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("%s: response contained no choices", p.Name())
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
	if msg.Content == "" && len(msg.ToolCalls) == 0 {
		// An empty reply with no tool calls is not an answer:
		// returning it as success hands the agent loop a blank
		// turn (Audit 3 stress). A null content with tool calls
		// is legitimate and unaffected.
		return nil, fmt.Errorf("%s: response contained no content or tool calls", p.Name())
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
		return nil, fmt.Errorf("%s: Stream requires a non-nil chunk callback", p.Name())
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(p.buildRequest(req, true))
	if err != nil {
		return nil, fmt.Errorf("%s: encoding stream request: %w", p.Name(), err)
	}
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/chat/completions"
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
			return nil, fmt.Errorf("%s: stream request failed: %w", p.Name(), err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
			resp.Body.Close()
			return nil, apiError(p.Name(), resp.StatusCode, body)
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
			if chunk.Error != nil {
				// An explicit provider error inside the
				// stream: report it, never skip it and
				// let the Chat fallback mask it.
				resp.Body.Close()
				return nil, fmt.Errorf("%s: stream error: %s", p.Name(), chunk.Error.describe())
			}
			for _, choice := range chunk.Choices {
				if choice.Delta.Content != "" {
					emitted++
					content.WriteString(choice.Delta.Content)
					if err := fn(choice.Delta.Content); err != nil {
						resp.Body.Close()
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
		scanErr := scanner.Err()
		resp.Body.Close()
		// A merged call counts as usable only when it has a name:
		// mergeStreamToolCalls pads the list when a delta index
		// jumps, and a padded entry can never execute.
		haveToolCalls := false
		for _, tc := range toolCalls {
			if strings.TrimSpace(tc.Function.Name) != "" {
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
			return nil, fmt.Errorf("%s: reading stream: %w", p.Name(), scanErr)
		}
		// Filter the merged calls before deciding whether anything
		// usable arrived: mergeStreamToolCalls pads the list when a
		// delta index jumps, and a padded entry has no name and can
		// never execute — it must not count as a tool call here, nor
		// reach the agent as a nameless call.
		msg := Message{Role: "assistant", Content: content.String()}
		for _, tc := range toolCalls {
			if strings.TrimSpace(tc.Function.Name) == "" {
				continue
			}
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		if emitted == 0 && len(msg.ToolCalls) == 0 {
			// Nothing usable was streamed — including a valid SSE stream
			// of role-only (empty) deltas, which previously returned nil
			// having delivered nothing. Fall back to a plain Chat so the
			// caller still gets an answer instead of an empty reply.
			return p.streamFallback(ctx, req, fn)
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
	return nil, fmt.Errorf("%s: stream failed after %d attempts", p.Name(), maxAttempts)
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
		return nil, fmt.Errorf("%s: stream fallback failed: %w", p.Name(), err)
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
