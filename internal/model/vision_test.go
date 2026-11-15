package model

import "testing"

func TestSupportsVision(t *testing.T) {
	cases := []struct {
		provider, model string
		want            bool
	}{
		// DeepSeek: flash sees, retired aliases route to flash.
		{"deepseek", "deepseek-flash", true},
		{"deepseek", "deepseek-v4-flash", true},
		{"deepseek", "deepseek-v4-flash-vision-exp", true},
		// DeepSeek: retired text-only aliases and text-only pro.
		{"deepseek", "deepseek-chat", false},
		{"deepseek", "deepseek-reasoner", false},
		{"deepseek", "deepseek-v4-pro", false},
		{"deepseek", "deepseek-4.1-flash", false}, // not a real API name
		{"deepseek", "deepseek-v4.1-flash", false},
		{"deepseek", "something-new", false},
		{"deepseek", "", false},
		// OpenAI: the two multimodal models only.
		{"openai", "gpt-4o", true},
		{"openai", "gpt-4o-mini", true},
		{"openai", "gpt-4.1", false},
		{"openai", "o4-mini", false},
		{"openai", "", false},
		// Anthropic / Ollama: false even for vision-capable
		// models — no image wire path exists yet (note-and-skip).
		{"anthropic", "claude-sonnet-4-5", false},
		{"ollama", "llava", false},
		{"ollama", "llama3.2-vision", false},
		// Unknown provider / empty input: honest false, never a guess.
		{"unknown-provider", "deepseek-flash", false},
		{"", "deepseek-flash", false},
		{"", "", false},
		// Matching is case-insensitive and trims whitespace.
		{"DeepSeek", "DeepSeek-Flash", true},
		{" deepseek ", " deepseek-flash ", true},
		{"OPENAI", "GPT-4O-MINI", true},
	}
	for _, tc := range cases {
		if got := SupportsVision(tc.provider, tc.model); got != tc.want {
			t.Errorf("SupportsVision(%q, %q) = %v, want %v", tc.provider, tc.model, got, tc.want)
		}
	}
}
