// Package model — vision capability detection.
//
// SupportsVision answers one question: can THIS provider/model pair
// actually see an image sent by Orion — i.e. does the model support
// image input AND does Orion have a wire path that can carry images
// to it? Both halves matter. A vision-capable model on a provider
// whose client cannot serialise image content parts is still
// unusable, and reporting it as supported would make the bot
// promise sight it cannot deliver, then fail (or worse, answer
// about an image it never saw).
//
// The table is explicit per (provider, model): an unknown model is
// NOT vision-capable (false), never "probably". The honest fallback
// is always "I can't see this", never a guess.
//
// Sources: DeepSeek's own API docs (Models & Pricing + Vision
// guide, fetched 2026-11-15): the vision model is exactly
// "deepseek-flash" (DeepSeek-V4.1-Flash); "deepseek-chat" and
// "deepseek-reasoner" are retired (2026-07-24) text-only aliases;
// "deepseek-v4-pro" is text-only; the retired aliases
// "deepseek-v4-flash" and "deepseek-v4-flash-vision-exp" still route
// to V4.1 Flash. OpenAI's gpt-4o / gpt-4o-mini are multimodal.
//
// NOTE-AND-SKIP (deliberate, not an oversight): Anthropic and Ollama
// return false even for vision-capable models, because their wire
// paths do not exist yet — the Anthropic client has no image block
// type (anthropic.go) and the Ollama client has no images field
// (ollama.go). When those wire paths land, add the models here.
// A dedicated image-model slot and live probing of a provider's
// capabilities are likewise out of scope.
package model

import "strings"

// SupportsVision reports whether provider/model can receive image
// input from Orion. Provider and model are matched case-insensitively
// after trimming; anything unlisted — including an unknown provider
// or an empty string — returns false.
func SupportsVision(provider, model string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	m := strings.ToLower(strings.TrimSpace(model))
	switch p {
	case "deepseek":
		// deepseek-flash is the current vision model
		// (DeepSeek-V4.1-Flash). The two retired aliases still
		// route to it per DeepSeek's pricing-page footnote, so
		// they see images too. deepseek-chat / deepseek-reasoner
		// are retired text-only aliases; deepseek-v4-pro is
		// text-only ("Not supported" in the pricing table).
		switch m {
		case "deepseek-flash", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp":
			return true
		default:
			return false
		}
	case "openai":
		// gpt-4o and gpt-4o-mini are multimodal. Every other
		// OpenAI model name is unknown here until it is added
		// explicitly — never assumed vision-capable.
		switch m {
		case "gpt-4o", "gpt-4o-mini":
			return true
		default:
			return false
		}
	case "anthropic", "ollama":
		// Vision-capable models exist on both providers, but
		// Orion has no image wire path for them yet (see the
		// package doc): capability here means "can see AND this
		// client can send images", so this is false for now.
		return false
	default:
		return false
	}
}
