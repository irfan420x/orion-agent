package model

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// --- CountTokens calibration -------------------------------------------
//
// CountTokens is calibrated against DeepSeek's real usage counts
// (live probes, 2026-11-13 — footer token-accounting investigation).
// The old runes/4 rule tracked English prose (0.9x) and Go code
// (1.0x) but undercounted Bengali text 1.9x and dense markup/URLs
// 2.3x, so a 96,929-rune RSS feed the model fetched cost 56,742 real
// prompt tokens while the history budget measured it at 24,232 —
// letting a ~60k-token prompt through a 32,768-token budget and
// producing the "59,635 tokens for a trivial question" footer.
// These tests pin the calibrated behaviour: the estimate must stay
// near real counts on prose/code, must charge substantially more
// than runes/4 for Bengali and dense content, and must never
// undercount the dense content that blows the budget.

// TestCountTokensCalibration pins the estimator against the live
// measured values. Real counts are DeepSeek prompt_tokens for the
// sample alone (baseline framing subtracted); the estimate must sit
// in [0.9x, 1.6x] of real on every sample — close enough to keep
// the history budget honest, conservative enough never to undercount
// the dense classes.
func TestCountTokensCalibration(t *testing.T) {
	// English prose, 159 runes: real 35.
	prose := "You are Orion, a model-agnostic OS agent running locally on the user's machine. You accomplish tasks by calling the tools available to you, one step at a time."
	// Go code (types.go, 4,069 runes): real 985.
	code := `// Message is a single chat turn. ToolCalls is set on assistant messages
// that request tool execution; ToolCallID links a tool result back.
type Message struct {
	Role       string     ` + "`json:\"role\"`" + ` // system, user, assistant, tool
	Content    string     ` + "`json:\"content\"`" + `
	ToolCalls  []ToolCall ` + "`json:\"tool_calls,omitempty\"`" + `
	ToolCallID string     ` + "`json:\"tool_call_id,omitempty\"`" + `
}`
	// Bengali, 1,004 runes (the exact assistant message from the
	// live turn): real 478.
	bengali := "আজ ১৩ নভেম্বর ২০২৬ (শুক্রবার), ঢাকার সময় সকাল ১০:১১। আজকের প্রধান শিরোনামগুলো (Google News বাংলা ফিড অনুযায়ী):\n\n**বাংলাদেশ**\n- ঢাকা–টাঙ্গাইল মহাসড়ক অবরোধ করে পোশাকশ্রমিকদের বিক্ষোভ; সাভার–আশুলিয়ায় শ্রমিক–পুলিশ পাল্টাপাল্টি ধাওয়া, শতাধিক কারখানা বন্ধ ঘোষণা। (প্রথম আলো, সমকাল, ডেইলি স্টার)\n- খুলনায় এনসিপির বিক্ষোভ মিছিলে দুই পক্ষের হাতাহাতি।\n- এমপির বাড়িতে আগুনের ঘটনায় সংসদে স্বরাষ্ট্রমন্ত্রীর বক্তব্য — নুসরাতের বাড়িতে পেট্রোলবোমার আলামত নেই, শর্টসার্কিট হতে পারে।\n- কপ৩১ সম্মেলন শেষে দেশে ফিরেছেন প্রধানমন্ত্রী।\n- শেখ হাসিনার দেশে ফেরার অঙ্গীকার নিয়ে আলোচনা–সমালোচনা (DW, বিবিসি)।\n\n**বিশ্ব**\n- ইরান ইস্যুতে ট্রাম্পের সিদ্ধান্ত নিয়ে মার্কিন প্রশাসনে মতভেদ (রুবিও বনাম ভ্যান্স)।\n- জম্মু ও কাশ্মীরের কুলগামে জঙ্গি হামলায় দুই পুলিশ কর্মকর্তা নিহত।\n\n**খেলা**\n- ফুটবলে ছোট ভাইয়ের জোড়া গোলে ইতালি হারাল ফ্রান্সকে; আইপিএল নিলামে মুস্তাফিজসহ বাংলাদেশি ক্রিকেটাররা।\n\n**অন্যান্য**\n- ভারতের ভিসা ব্যবস্থায় বড় পরিবর্তন (‘ট্যুরিস্ট ভিসা’ নাম বদলে ‘ভিজিটর ভিসা’)।\n- আজ হুমায়ূন আহমেদের ৭৮তম জন্মদিন।"
	// Go code must stay near runes/4 (it measured 1.03x of it on
	// the full types.go): the estimator must not overcount code
	// while fixing dense text.
	codeRunes := utf8.RuneCountInString(code)
	codeOld := codeRunes / 4
	if got := CountTokens(code); got < codeOld*9/10 || got > codeOld*13/10 {
		t.Fatalf("CountTokens(go code) = %d, want within [0.9x, 1.3x] of runes/4 (%d)", got, codeOld)
	}
	t.Logf("CountTokens(go code) = %d (runes/4 %d)", CountTokens(code), codeOld)

	cases := []struct {
		name string
		text string
		real int
	}{
		{"prose", prose, 35},
		{"bengali", bengali, 478},
	}
	for _, tc := range cases {
		got := CountTokens(tc.text)
		lo := tc.real * 9 / 10
		hi := tc.real*16/10 + 1
		if got < lo || got > hi {
			t.Fatalf("CountTokens(%s) = %d, want within [0.9x, 1.6x] of real %d ([%d, %d])",
				tc.name, got, tc.real, lo, hi)
		}
		t.Logf("CountTokens(%s) = %d (real %d, %.2fx)", tc.name, got, tc.real, float64(got)/float64(tc.real))
	}
}

// TestCountTokensDenseContent pins the budget-critical property: a
// long varied run (base64 ID, long URL) must cost near its measured
// real rate (~1.4 runes/token), not runes/4. A 1,652-rune base64 run
// measured 1,176 real tokens; a 1,694-rune URL 1,188.
func TestCountTokensDenseContent(t *testing.T) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var b strings.Builder
	for b.Len() < 1652 {
		b.WriteString(alphabet)
	}
	run := b.String()[:1652]
	if got := CountTokens(run); got < 1000 || got > 1250 {
		t.Fatalf("CountTokens(1652-rune base64 run) = %d, want within [1000, 1250] (measured real 1176)", got)
	}
	url := "https://news.google.com/rss/articles/" + run
	if got := CountTokens(url); got < 1000 {
		t.Fatalf("CountTokens(long URL, %d runes) = %d, want >= 1000 (measured real 1188 for a 1694-rune URL)",
			utf8.RuneCountInString(url), got)
	}
	// A repetitive run still compresses: BPE merges repeats, so
	// "a" x 40 costs 4 real tokens and the estimate keeps the old
	// runes/4 value of 10 (conservative, and the pinned behaviour
	// of TestCountTokensSanity / TestCountTokensNonASCII).
	if got := CountTokens(strings.Repeat("a", 40)); got != 10 {
		t.Fatalf("CountTokens(40 x a) = %d, want 10 (repetitive run)", got)
	}
}

// TestCountTokensBeatsRunesDiv4OnDenseText pins the property that
// failed in production: on dense content the estimate must exceed
// runes/4 by a wide margin. The synthetic document models the real
// 96,929-rune RSS feed (XML tags + long base64 URLs + Bengali
// titles) that measured 2.34x runes/4 in real tokens.
func TestCountTokensBeatsRunesDiv4OnDenseText(t *testing.T) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var item strings.Builder
	for i := 0; i < 20; i++ {
		var id strings.Builder
		for id.Len() < 200 {
			id.WriteString(alphabet)
		}
		item.WriteString(`<item><title>ঢাকা–টাঙ্গাইল মহাসড়ক অবরোধ</title><link>https://news.google.com/rss/articles/`)
		item.WriteString(id.String()[:200])
		item.WriteString(`</link><description>পদ্মা সেতুতে যানবাহনের চাপ বাড়ছে</description></item>`)
	}
	doc := `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>প্রথম আলো</title>` + item.String() + `</channel></rss>`
	runes := utf8.RuneCountInString(doc)
	old := runes / 4
	got := CountTokens(doc)
	if got < 2*old {
		t.Fatalf("CountTokens(dense RSS-like doc, %d runes) = %d, want >= 2x runes/4 (%d)", runes, got, 2*old)
	}
	t.Logf("dense doc: %d runes, runes/4 = %d, CountTokens = %d (%.2fx)", runes, old, got, float64(got)/float64(old))
}
