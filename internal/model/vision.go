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
// Anthropic's Claude 4.5 models are multimodal, and the wire path
// exists: anthropic.go renders image content blocks for user
// messages (anthropicImageBlocks) and tool results
// (anthropicToolResultContent) — proven by images_test.go.
// Ollama's vision models carry base64 images in the native Images
// field (ollama.go toOllamaMessages) — likewise proven by
// images_test.go. F-M2R-31: the table used to claim both wire
// paths did not exist; they do, so the table now says so — the
// photo path (fail-closed on !SupportsVision) and the
// ground/screenshot path (which always sends tool images) finally
// agree.
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
	case "anthropic":
		// Claude 4.5 models are multimodal, and the wire path
		// exists (anthropic.go image blocks for user messages
		// and tool results). Any other Anthropic model name is
		// unknown here until added explicitly.
		switch m {
		case "claude-sonnet-4-5", "claude-opus-4-5", "claude-haiku-4-5":
			return true
		default:
			return false
		}
	case "ollama":
		// The Ollama wire path carries base64 images in the
		// native Images field (ollama.go). Only the vision
		// models Orion knows are listed: llava and
		// llama3.2-vision. A local model tag with a version
		// suffix (e.g. "llava:13b") is NOT matched — the tag is
		// part of the model name, and guessing that every
		// suffixed tag sees images would over-claim.
		switch m {
		case "llava", "llama3.2-vision":
			return true
		default:
			return false
		}
	default:
		return false
	}
}
