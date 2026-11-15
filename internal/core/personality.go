package core

import (
	"bytes"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// Personality — Orion's voice.
//
// The personality file is style and voice ONLY. It lives in the user's
// data dir (for example ~/.orion/personality.md) and is loaded once at
// agent startup, so the user can change how Orion sounds without
// rebuilding. It is never allowed to change what Orion may do: the
// operating instructions in systemPrompt (safety, denylist, gate,
// approval behavior) always win on any conflict, and SystemPrompt
// states that ordering in the assembled prompt.

const (
	// personalityFileName is the user-editable voice file, read from
	// the agent's data dir at startup.
	personalityFileName = "personality.md"
	// maxPersonalityBytes caps the file: a personality is a few
	// paragraphs of voice, not a manual. Oversize input is
	// truncated, never rejected.
	maxPersonalityBytes = 8192
)

// defaultPersonality is the built-in voice, used when the user file is
// missing, unreadable, or empty. Short, Bengali-first, human — no
// AI-ish filler.
const defaultPersonality = `বাংলায় কথা বলি — টেকনিক্যাল শব্দ ইংরেজিতে। সংক্ষেপে, কাজের কথায় আসি; ভূমিকা নেই, "আমি AI" জাতীয় কথা নেই।
উত্তর গুছিয়ে দিই: দরকারি কথা আগে, তারপর বিস্তারিত — মানুষের মতো করে, লম্বা লিস্ট নয়।
কাজ শেষে বলি কী হলো আর কী বাকি — তিন লাইনের বেশি নয়।`

// loadPersonality reads the user voice file from dir. A missing or
// unreadable file, or an empty one, falls back to the built-in
// default — never an error. Oversize input is truncated to
// maxPersonalityBytes without splitting a multi-byte rune.
func loadPersonality(dir string) string {
	if dir == "" {
		return defaultPersonality
	}
	b, err := os.ReadFile(filepath.Join(dir, personalityFileName))
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return defaultPersonality
	}
	if len(b) > maxPersonalityBytes {
		b = b[:maxPersonalityBytes]
		for len(b) > 0 && !utf8.Valid(b) {
			b = b[:len(b)-1]
		}
	}
	return string(b)
}
