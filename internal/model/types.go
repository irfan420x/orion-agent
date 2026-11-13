// Package model defines the provider-neutral LLM interface for Orion.
// No provider is hardcoded in the core: providers are selected by config
// only, via ProviderConfig and NewProvider.
package model

import (
	"context"
	"unicode"
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

// CountTokens estimates how many tokens text costs a provider.
//
// It is a BPE-shaped estimate, calibrated against DeepSeek's real
// usage counts (live probes, 2026-11-13 — see the footer
// token-accounting investigation): the old runes/4 rule tracked
// English prose (0.9x) and Go code (1.0x) but undercounted the two
// content classes that actually blow the history budget — Bengali
// text (1.9x under: ~2 runes per token) and dense markup/URLs (2.3x
// under: a 96,929-rune RSS feed the model fetched cost 56,742 real
// prompt tokens, not 24,232). The budget in internal/core is enforced
// in these units, so an estimator that undercounts dense content by
// 2.3x lets a ~60k-token prompt through a 32,768-token budget —
// which is exactly the "59,635 tokens for a trivial question"
// footer the investigation reproduced.
//
// Rules, measured against DeepSeek (real counts in brackets):
//
//   - Whitespace costs nothing.
//   - A run of letters/digits is one word when short (<= 12 runes):
//     one token. English words and identifiers tokenize this way
//     ("hello world, this is a test" = 7 [real ~7]).
//   - A long varied run (> 12 runes, e.g. a URL or base64 article
//     ID) costs ~2 tokens per 3 runes: a 1,652-rune base64 run costs
//     1,176 real tokens [measured], a 1,694-rune URL 1,188 [measured].
//   - A repetitive run (at most 2 distinct runes, e.g. "xxxx…")
//     costs runes/4: BPE merges repeats efficiently ("a" x 40 costs
//     4 real tokens; the estimate says 10 — conservative).
//   - Every other rune — punctuation, symbols, combining marks —
//     costs one token: XML tags, JSON punctuation and Bengali vowel
//     signs each tokenize near one-per-rune.
//
// The estimate is deliberately conservative (it overcounts Bengali
// ~1.5x and never undercounts the measured dense samples: prose
// 1.03x, code 1.06x, Bengali 1.54x, RSS feed 1.06x of real). For a
// budget estimator that is the safe direction: overcounting trims
// history a little early, undercounting blows the context window and
// the bill. Counts are in runes, never bytes, so non-ASCII text is
// not inflated by its UTF-8 length.
func CountTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	n := 0
	rest := text
	for len(rest) > 0 {
		r, size := utf8.DecodeRuneInString(rest)
		rest = rest[size:]
		if unicode.IsSpace(r) {
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			// Consume the whole letter/digit run, tracking how
			// many distinct runes it holds (only the 1-vs-2-vs-3+
			// distinction matters).
			length := 1
			var first, second rune = r, 0
			distinct := 1
			for len(rest) > 0 {
				r2, size2 := utf8.DecodeRuneInString(rest)
				if !unicode.IsLetter(r2) && !unicode.IsDigit(r2) {
					break
				}
				rest = rest[size2:]
				length++
				if distinct == 1 && r2 != first {
					second, distinct = r2, 2
				} else if distinct == 2 && r2 != first && r2 != second {
					distinct = 3
				}
			}
			n += runTokens(length, distinct)
			continue
		}
		// Punctuation, symbols, combining marks: ~1 token each.
		n++
	}
	if n < 1 {
		// Non-empty but all whitespace: still one token, as before.
		return 1
	}
	return n
}

// runTokens is the token cost of one letter/digit run of length runes
// holding distinct distinct runes. See CountTokens for the measured
// basis.
func runTokens(length, distinct int) int {
	if distinct <= 2 {
		// Repetitive: BPE merges repeats, ~4+ runes per token.
		return (length + 3) / 4
	}
	if length <= 12 {
		// A word: about one token however long it is (up to 12).
		return 1
	}
	// A long varied run (URL, base64 ID, hash): ~1.4 runes/token
	// measured, so 2 tokens per 3 runes, rounded up.
	return (2*length + 2) / 3
}
