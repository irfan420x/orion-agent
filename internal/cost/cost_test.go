package cost

import (
	"testing"

	"github.com/irfan/orion/internal/model"
)

func TestTableVersionAndDateSet(t *testing.T) {
	tbl := DefaultTable()
	if tbl.Version == "" || tbl.Effective == "" {
		t.Fatalf("DefaultTable version/effective = %q/%q, want both set", tbl.Version, tbl.Effective)
	}
	if tbl.Version != TableVersion || tbl.Effective != TableEffective {
		t.Errorf("table version/effective = %q/%q, want constants %q/%q", tbl.Version, tbl.Effective, TableVersion, TableEffective)
	}
	if TableSourceNote == "" {
		t.Error("TableSourceNote is empty: the estimate must say what it is based on")
	}
}

func TestPriceForKnownModels(t *testing.T) {
	tbl := DefaultTable()
	cases := []struct {
		provider, modelName string
		in, out             float64
	}{
		{"deepseek", "deepseek-chat", 0.28, 0.42},
		{"deepseek", "deepseek-reasoner", 0.55, 2.19},
		{"deepseek", "deepseek-flash", 0.15, 0.60},
		{"openrouter", "deepseek/deepseek-chat", 0.26, 1.03},
		{"openai", "gpt-4o", 2.50, 10.00},
		{"openai", "gpt-4o-mini", 0.15, 0.60},
		{"anthropic", "claude-sonnet-4", 3.00, 15.00},
		{"anthropic", "claude-opus-4", 15.00, 75.00},
		{"ollama", "llama3.1", 0, 0},
	}
	for _, c := range cases {
		pr, ok := tbl.PriceFor(c.provider, c.modelName)
		if !ok {
			t.Errorf("PriceFor(%q, %q) = not found, want %v/%v", c.provider, c.modelName, c.in, c.out)
			continue
		}
		if pr.InputPerMillion != c.in || pr.OutputPerMillion != c.out {
			t.Errorf("PriceFor(%q, %q) = %v/%v, want %v/%v", c.provider, c.modelName, pr.InputPerMillion, pr.OutputPerMillion, c.in, c.out)
		}
	}
	// Normalisation: case and whitespace must not matter.
	if _, ok := tbl.PriceFor("DeepSeek", " DeepSeek-Chat "); !ok {
		t.Error("PriceFor(DeepSeek, ' DeepSeek-Chat ') = not found, want a match (normalised)")
	}
	// Model-only fallback: deepseek-chat served under another key.
	if _, ok := tbl.PriceFor("openai", "deepseek-chat"); !ok {
		t.Error("PriceFor(openai, deepseek-chat) = not found, want the model-only fallback")
	}
	// Any ollama model is free, even one never listed.
	if pr, ok := tbl.PriceFor("ollama", "some-future-local-model"); !ok || pr.InputPerMillion != 0 || pr.OutputPerMillion != 0 {
		t.Errorf("PriceFor(ollama, unlisted) = %+v/%v, want free/true", pr, ok)
	}
}

func TestPriceForUnknownIsUnknownNotZero(t *testing.T) {
	tbl := DefaultTable()
	if _, ok := tbl.PriceFor("deepseek", "deepseek-v99"); ok {
		t.Error("PriceFor(deepseek, deepseek-v99) = found, want unknown (must never silently price a paid model at $0)")
	}
	if _, ok := tbl.PriceFor("", "deepseek-chat"); ok {
		t.Error("PriceFor('', deepseek-chat) = found, want unknown")
	}
	if _, ok := tbl.PriceFor("deepseek", ""); ok {
		t.Error("PriceFor(deepseek, '') = found, want unknown")
	}
	if _, ok := (*Table)(nil).PriceFor("deepseek", "deepseek-chat"); ok {
		t.Error("nil table PriceFor = found, want unknown")
	}
}

func TestEstimateMath(t *testing.T) {
	tbl := DefaultTable()
	// 1M prompt + 1M completion on deepseek-chat: 0.28 + 0.42.
	usd, ok := tbl.Estimate("deepseek", "deepseek-chat", model.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000, TotalTokens: 2_000_000}, 0)
	if !ok || usd < 0.70-1e-12 || usd > 0.70+1e-12 {
		t.Errorf("Estimate deepseek-chat 1M/1M = $%v/%v, want $0.70/true", usd, ok)
	}
	// Subagent tokens price at the OUTPUT rate: 1M subagent tokens
	// on gpt-4o-mini cost $0.60, not the $0.15 input rate.
	usd, ok = tbl.Estimate("openai", "gpt-4o-mini", model.Usage{}, 1_000_000)
	if !ok || usd != 0.60 {
		t.Errorf("Estimate gpt-4o-mini 1M subagent = $%v/%v, want $0.60/true (output rate)", usd, ok)
	}
	// A realistic small turn: 3,337 tokens, ~3,000 prompt + 337
	// completion on deepseek-chat — a fraction of a cent, not $0.00
	// rounded away by the estimate itself (formatting is separate).
	usd, ok = tbl.Estimate("deepseek", "deepseek-chat", model.Usage{PromptTokens: 3000, CompletionTokens: 337, TotalTokens: 3337}, 0)
	if !ok {
		t.Fatal("Estimate small deepseek turn = unknown, want a price")
	}
	want := 3000*0.28/1e6 + 337*0.42/1e6
	if diff := usd - want; diff < -1e-12 || diff > 1e-12 {
		t.Errorf("Estimate small turn = $%v, want $%v", usd, want)
	}
	// Unknown model: no price, never $0.
	if usd, ok := tbl.Estimate("deepseek", "deepseek-v99", model.Usage{PromptTokens: 100}, 0); ok || usd != 0 {
		t.Errorf("Estimate unknown model = $%v/%v, want $0/false", usd, ok)
	}
	// deepseek-flash (vision fix): 1M prompt + 1M completion at
	// the off-peak cache-miss rates is $0.15 + $0.60 — so /usage
	// no longer reports "unknown" for the vision model.
	usd, ok = tbl.Estimate("deepseek", "deepseek-flash", model.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000, TotalTokens: 2_000_000}, 0)
	if !ok || usd < 0.75-1e-12 || usd > 0.75+1e-12 {
		t.Errorf("Estimate deepseek-flash 1M/1M = $%v/%v, want $0.75/true", usd, ok)
	}
	// Negative counts are caller errors: no price.
	if _, ok := tbl.Estimate("deepseek", "deepseek-chat", model.Usage{PromptTokens: -1}, 0); ok {
		t.Error("Estimate negative prompt = ok, want false")
	}
	if _, ok := tbl.Estimate("deepseek", "deepseek-chat", model.Usage{}, -5); ok {
		t.Error("Estimate negative subagent = ok, want false")
	}
	// Free local model: $0, and that IS a known price.
	if usd, ok := tbl.Estimate("ollama", "llama3.1", model.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}, 0); !ok || usd != 0 {
		t.Errorf("Estimate ollama = $%v/%v, want $0/true", usd, ok)
	}
}

func TestEstimateTotalsAgreesWithEstimate(t *testing.T) {
	tbl := DefaultTable()
	a, okA := tbl.EstimateTotals("anthropic", "claude-sonnet-4", 2000, 500, 100)
	b, okB := tbl.Estimate("anthropic", "claude-sonnet-4", model.Usage{PromptTokens: 2000, CompletionTokens: 500, TotalTokens: 2600}, 100)
	if !okA || !okB || a != b {
		t.Errorf("EstimateTotals = $%v/%v vs Estimate $%v/%v, want them equal", a, okA, b, okB)
	}
}

func TestFormatUSD(t *testing.T) {
	for _, c := range []struct {
		v    float64
		want string
	}{
		{0, "$0.0000"},
		{0.00098154, "$0.0010"},
		{0.42, "$0.4200"},
		{1, "$1.00"},
		{12.345, "$12.35"},
		{-1, "$0.0000"},
	} {
		if got := FormatUSD(c.v); got != c.want {
			t.Errorf("FormatUSD(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestTableModels(t *testing.T) {
	tbl := DefaultTable()
	cases := []struct {
		provider string
		want     []string
	}{
		{"deepseek", []string{"deepseek-chat", "deepseek-flash", "deepseek-reasoner"}},
		{"openai", []string{"gpt-4.1", "gpt-4.1-mini", "gpt-4o", "gpt-4o-mini", "o4-mini"}},
		{"anthropic", []string{"claude-3-5-sonnet", "claude-3-5-sonnet-20241022", "claude-haiku-3-5", "claude-opus-4", "claude-opus-4-20250514", "claude-sonnet-4", "claude-sonnet-4-20250514", "claude-sonnet-4-5", "claude-sonnet-4-5-20250929"}},
		{"openrouter", []string{"deepseek/deepseek-chat"}},
		{"ollama", []string{"llama3.1", "qwen2.5-0.5b-local"}},
		{"unknown-provider", nil},
		{"", nil},
	}
	for _, tc := range cases {
		got := tbl.Models(tc.provider)
		if len(got) != len(tc.want) {
			t.Errorf("Models(%q) = %v, want %v", tc.provider, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("Models(%q) = %v, want %v", tc.provider, got, tc.want)
				break
			}
		}
	}
	// Case-insensitive, and the result is a fresh slice: mutating
	// it cannot affect the table.
	if got := tbl.Models("DeepSeek"); len(got) != 3 {
		t.Errorf("Models(DeepSeek) = %v, want 3 deepseek models", got)
	}
	got := tbl.Models("deepseek")
	got[0] = "tampered"
	if tbl.Models("deepseek")[0] == "tampered" {
		t.Error("Models returned a slice aliasing the table")
	}
	// Nil table: no models, never a panic.
	var nilTbl *Table
	if nilTbl.Models("deepseek") != nil {
		t.Error("nil Table.Models returned non-nil, want nil")
	}
}
