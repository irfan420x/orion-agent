// Package model — provider context windows.
//
// Each provider/model declares its own context window (verified
// against the official provider docs on 2026-11-15; see
// ~/workspace/orion-verification/context-window/
// provider-windows-research.md for the sources). The agent derives
// its history budget as half the active provider's window (see
// internal/core maxHistoryTokens), so the window must be a real,
// declared value — never guessed from memory.
//
// Values below are exact token counts from the research file. Where
// a provider states only "1M" or "128K", the table uses 1,000,000 /
// 131,072 (= 128 x 1024) as the research file does. A model with no
// verified window (e.g. Ollama's mistral, for which no official
// statement was found) is deliberately NOT in any table: it falls
// through to the provider's conservative fallback.
package model

import "strings"

// ContextWindowProvider is the OPTIONAL capability a provider
// implements when it can declare its model's context window. It is
// optional (same pattern as the tools framework's optional
// interfaces) so test fakes and future providers that declare
// nothing keep working: ContextWindow reports ok=false for them
// and the core falls back to the legacy 32768-token budget.
type ContextWindowProvider interface {
	// ContextWindow returns the context window, in tokens, of the
	// model this provider is configured for. It always returns a
	// positive value: a table hit, or the provider's fallback for
	// an unknown model. It never returns 0 or a negative value.
	ContextWindow() int
}

// ContextWindow returns the declared window of p and true when p
// implements ContextWindowProvider, or (0, false) when it does not.
func ContextWindow(p Provider) (int, bool) {
	if p == nil {
		return 0, false
	}
	cw, ok := p.(ContextWindowProvider)
	if !ok {
		return 0, false
	}
	return cw.ContextWindow(), true
}

// normaliseModel lower-cases and trims a model name for table
// lookup. Ollama lookups additionally strip a ":tag" suffix
// (e.g. "llama3.1:latest" -> "llama3.1"); the other providers'
// model IDs carry no tags, so only Ollama strips.
func normaliseModel(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func ollamaModelName(model string) string {
	name := normaliseModel(model)
	if i := strings.Index(name, ":"); i >= 0 {
		name = name[:i]
	}
	return name
}

// --- DeepSeek ---
//
// Validated live 2026-11-20 against the official DeepSeek Models
// & Pricing page (api-docs.deepseek.com/quick_start/pricing,
// crawled 2026-11-19): the current API models are deepseek-flash
// (DeepSeek-V4.1-Flash-1017) and deepseek-v4-pro
// (DeepSeek-V4-Pro-0813), both at a 1M context. deepseek-chat /
// deepseek-reasoner were legacy aliases routing to
// deepseek-v4-flash; DeepSeek retired them 2026-07-24 15:59 UTC
// ("fully retired and inaccessible", V4 Preview release
// news260424) and the pricing page no longer lists them. Their
// table values are therefore the models' native documented
// windows — 128K for both (DeepSeek-V3.1 release news250821:
// "128K context for both"), 131,072 per this file's "128K"
// convention — NOT the 1M the pre-retirement routing used to
// imply. A stale config naming a retired alias degrades to a
// conservative budget instead of claiming 1M. Unknown DeepSeek
// model -> 131,072: the smallest verified window, so an unknown
// model never over-claims (the same principle as every other
// provider's fallback in this file).
var deepSeekContextWindows = map[string]int{
	"deepseek-chat":     131072,
	"deepseek-reasoner": 131072,
	"deepseek-flash":    1000000,
	"deepseek-v4-pro":   1000000,
}

const deepSeekUnknownModelWindow = 131072

func deepSeekContextWindow(model string) int {
	if w, ok := deepSeekContextWindows[normaliseModel(model)]; ok {
		return w
	}
	return deepSeekUnknownModelWindow
}

// --- OpenAI ---
//
// gpt-4o / gpt-4o-mini: 128,000; gpt-4.1 / gpt-4.1-mini: 1,047,576
// (official per-model pages, 2026-11-15; all four are marked
// Deprecated there). Unknown OpenAI model -> 128,000, the smallest
// verified window, so an unknown model never over-claims.
var openAIContextWindows = map[string]int{
	"gpt-4o-mini":  128000,
	"gpt-4o":       128000,
	"gpt-4.1-mini": 1047576,
	"gpt-4.1":      1047576,
}

const openAIUnknownModelWindow = 128000

func openAIContextWindow(model string) int {
	if w, ok := openAIContextWindows[normaliseModel(model)]; ok {
		return w
	}
	return openAIUnknownModelWindow
}

// --- OpenRouter ---
//
// deepseek/deepseek-chat: 128,000 (openrouter.ai/api/v1/models,
// top_provider context_length, checked 2026-11-19; the model's
// own context_length is 163,840 across providers, so the table
// holds the conservative per-route figure). Unknown OpenRouter
// model -> 128,000, the smallest verified window, so an unknown
// model never over-claims.
var openRouterContextWindows = map[string]int{
	"deepseek/deepseek-chat": 128000,
}

const openRouterUnknownModelWindow = 128000

func openRouterContextWindow(model string) int {
	if w, ok := openRouterContextWindows[normaliseModel(model)]; ok {
		return w
	}
	return openRouterUnknownModelWindow
}

// --- Anthropic ---
//
// claude-sonnet-4-5 / claude-opus-4-5 / claude-haiku-4-5: 200,000
// each (official Models overview table, 2026-11-15). Unknown
// model -> 200,000: every verified Anthropic model is 200K.
var anthropicContextWindows = map[string]int{
	"claude-sonnet-4-5": 200000,
	"claude-opus-4-5":   200000,
	"claude-haiku-4-5":  200000,
}

const anthropicUnknownModelWindow = 200000

func anthropicContextWindow(model string) int {
	if w, ok := anthropicContextWindows[normaliseModel(model)]; ok {
		return w
	}
	return anthropicUnknownModelWindow
}

// --- Ollama ---
//
// llama3.1 / llama3.2: 131,072 ("128K" per Meta's model table);
// qwen2.5-0.5b / -7b / -14b: 32,768 (Ollama registry per-tag
// value; Qwen's own cards claim 131,072 for 7b/14b with YaRN,
// but the shipped Ollama config is 32,768 — see the research
// file). mistral is UNVERIFIED (no official statement found) and
// is deliberately absent: it falls to the unknown-model fallback.
// Unknown Ollama model -> 32,768.
var ollamaContextWindows = map[string]int{
	"llama3.1":     131072,
	"llama3.2":     131072,
	"qwen2.5-0.5b": 32768,
	"qwen2.5-7b":   32768,
	"qwen2.5-14b":  32768,
}

const ollamaUnknownModelWindow = 32768

func ollamaContextWindow(model string) int {
	if w, ok := ollamaContextWindows[ollamaModelName(model)]; ok {
		return w
	}
	return ollamaUnknownModelWindow
}

// ContextWindow returns the window for this provider's configured
// model. The DeepSeek provider is an openAIProvider with
// name="deepseek" (see NewProvider), so it branches on the provider
// name: DeepSeek models use the DeepSeek table, OpenRouter models
// the OpenRouter table, OpenAI models the OpenAI table — never the
// other way round.
func (p *openAIProvider) ContextWindow() int {
	if p.Name() == "deepseek" {
		return deepSeekContextWindow(p.cfg.Model)
	}
	if p.Name() == "openrouter" {
		return openRouterContextWindow(p.cfg.Model)
	}
	return openAIContextWindow(p.cfg.Model)
}

// ContextWindow returns the window for this provider's configured
// Anthropic model.
func (p *anthropicProvider) ContextWindow() int {
	return anthropicContextWindow(p.cfg.Model)
}

// ContextWindow returns the window for this provider's configured
// Ollama model.
func (p *ollamaProvider) ContextWindow() int {
	return ollamaContextWindow(p.cfg.Model)
}
