// Package model defines the provider-neutral LLM interface for Orion.
// No provider is hardcoded in the core: providers are selected by config
// only, via ProviderConfig and NewProvider.
package model

import (
	"context"
	"unicode/utf8"
)

// Message is a single chat turn. ToolCalls is set on assistant messages
// that request tool execution; ToolCallID links a tool result back.
type Message struct {
	Role       string     `json:"role"` // system, user, assistant, tool
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is one requested tool invocation.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON object string
}

// ToolDefinition describes a tool to the model (OpenAI-style function schema).
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ChatRequest is a single (non-streaming) completion request.
type ChatRequest struct {
	Messages  []Message        `json:"messages"`
	Tools     []ToolDefinition `json:"tools,omitempty"`
	MaxTokens int              `json:"max_tokens,omitempty"`
	// Temperature is a pointer so an explicit 0 (deterministic
	// sampling) can be sent: nil means "provider default", while
	// a pointer to 0.0 is sent as temperature 0.
	Temperature *float64 `json:"temperature,omitempty"`
}

// Usage reports token consumption when the provider supplies it.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResponse is the model's reply.
type ChatResponse struct {
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"` // stop, tool_calls, length, error
	Usage        Usage   `json:"usage"`
}

// Provider is the interface every LLM provider must implement.
// Implementations must honour ctx cancellation/timeout, never retry
// indefinitely (max 3 attempts with backoff), and return descriptive
// errors when the API key or endpoint is missing/misconfigured.
type Provider interface {
	// Name returns the provider identifier, e.g. "openai", "anthropic", "ollama".
	Name() string
	// Chat performs one completion, possibly requesting tool calls.
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	// Stream performs one streaming completion and returns the full
	// response once the turn completes: the accumulated content, any
	// tool calls the model requested, usage, and the finish reason.
	// fn is called for each content chunk as it arrives, so callers
	// see the reply live; chunks are content only — tool calls are
	// delivered in the returned response, never as chunks. Exactly
	// one provider call backs each Stream invocation (a provider
	// whose stream yields nothing usable may fall back to a single
	// Chat internally and return that response instead). A provider
	// without native streaming may call Chat once and emit the full
	// content as a single chunk.
	Stream(ctx context.Context, req ChatRequest, fn func(chunk string) error) (*ChatResponse, error)
}

// ProviderConfig selects and configures one provider. It is the only
// provider configuration the core ever sees.
type ProviderConfig struct {
	Name    string `json:"name"`     // openai | anthropic | ollama
	APIKey  string `json:"api_key"`  // empty for local providers
	BaseURL string `json:"base_url"` // override endpoint; empty = provider default
	Model   string `json:"model"`
}

// CountTokens is a rough token estimate (used when a provider does not
// report usage). Approximation: ~4 characters per token, counted in
// runes so non-ASCII text is not overestimated (bytes/4 would count a
// 2-byte rune as half a token).
func CountTokens(text string) int {
	n := utf8.RuneCountInString(text) / 4
	if n < 1 && len(text) > 0 {
		return 1
	}
	return n
}
