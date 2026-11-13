// Package cost turns token usage into an estimated USD amount.
//
// The price table is DATA, not magic numbers scattered through the
// code: one versioned table (TableVersion + EffectiveDate) holds the
// per-million-token input/output price for every provider/model
// Orion can drive. Prices are ESTIMATES, labelled as such wherever
// they are shown: providers change prices, run peak/off-peak and
// cache-hit variants, and offer discounts Orion cannot see, so the
// number /usage and the footer report is the standard on-demand
// list price at the table's effective date — never the bill. The
// version + date make staleness visible: when the table is old, the
// reader can see it is old.
//
// Lookup is by (provider, model), normalised (lowercase, trimmed).
// A model with no entry is UNKNOWN, never silently $0: reporting
// $0.00 for a paid model would be worse than reporting nothing. The
// one exception is local inference: provider "ollama" is free by
// definition (the user pays for hardware, not tokens), so any ollama
// model prices at $0.
//
// Subagent tokens: a child agent reports one total, with no
// prompt/completion split (see core.Agent.AddSubagentUsage), so the
// split is unknowable at the parent. They are priced at the model's
// OUTPUT rate — the conservative side, since output is the dearer
// meter — and the estimate documents that choice. The split is
// preserved separately (PromptTokens/CompletionTokens on the usage)
// so the priced part of a turn is exact; only the subagent part is
// an approximation.
package cost

import (
	"fmt"
	"sort"
	"strings"

	"github.com/irfan/orion/internal/model"
)

// TableVersion identifies this price table; EffectiveDate is the
// date the prices were last checked against the providers' published
// rate cards (YYYY-MM-DD). Bump both together when the prices are
// re-checked: a reader comparing the date against today can tell at
// a glance whether the estimate is fresh.
const (
	TableVersion    = "2026-11-13"
	TableEffective  = "2026-11-13"
	TableSourceNote = "standard on-demand list prices, USD per 1M tokens, checked 2026-11-13 against the providers' published rate cards (DeepSeek, OpenAI, Anthropic); cache-hit, peak/off-peak, batch and enterprise rates are NOT reflected"
)

// Price is one model's rate card: USD per 1,000,000 tokens.
type Price struct {
	InputPerMillion  float64 // USD per 1M prompt tokens (cache miss)
	OutputPerMillion float64 // USD per 1M completion tokens
}

// Table is a versioned price table: provider -> model -> Price.
// The zero Table has no entries; use DefaultTable.
type Table struct {
	Version   string
	Effective string // YYYY-MM-DD the prices were checked
	entries   map[string]map[string]Price
}

// DefaultTable returns the versioned price table Orion ships.
// Prices are standard on-demand list prices in USD per 1M tokens
// (see TableSourceNote). Local models cost $0 by definition.
func DefaultTable() *Table {
	t := &Table{Version: TableVersion, Effective: TableEffective, entries: map[string]map[string]Price{}}
	add := func(provider, modelName string, in, out float64) {
		p := strings.ToLower(strings.TrimSpace(provider))
		m := strings.ToLower(strings.TrimSpace(modelName))
		if t.entries[p] == nil {
			t.entries[p] = map[string]Price{}
		}
		t.entries[p][m] = Price{InputPerMillion: in, OutputPerMillion: out}
	}
	// DeepSeek (api-docs.deepseek.com rate card, via the
	// widely-reported list prices checked 2026-11-13; deepseek-chat
	// $0.28/$0.42, deepseek-reasoner $0.55/$2.19 — cache-miss input).
	add("deepseek", "deepseek-chat", 0.28, 0.42)
	add("deepseek", "deepseek-reasoner", 0.55, 2.19)
	// OpenAI (openai.com/api/pricing list prices).
	add("openai", "gpt-4o", 2.50, 10.00)
	add("openai", "gpt-4o-mini", 0.15, 0.60)
	add("openai", "gpt-4.1", 2.00, 8.00)
	add("openai", "gpt-4.1-mini", 0.40, 1.60)
	add("openai", "o4-mini", 1.10, 4.40)
	// Anthropic (anthropic.com/pricing list prices).
	add("anthropic", "claude-sonnet-4", 3.00, 15.00)
	add("anthropic", "claude-sonnet-4-20250514", 3.00, 15.00)
	add("anthropic", "claude-sonnet-4-5", 3.00, 15.00)
	add("anthropic", "claude-sonnet-4-5-20250929", 3.00, 15.00)
	add("anthropic", "claude-opus-4", 15.00, 75.00)
	add("anthropic", "claude-opus-4-20250514", 15.00, 75.00)
	add("anthropic", "claude-haiku-3-5", 0.80, 4.00)
	add("anthropic", "claude-3-5-sonnet", 3.00, 15.00)
	add("anthropic", "claude-3-5-sonnet-20241022", 3.00, 15.00)
	// Local inference: no per-token bill. (Any ollama model is
	// free — see PriceFor — these entries document the two models
	// Orion itself ships configured.)
	add("ollama", "llama3.1", 0, 0)
	add("ollama", "qwen2.5-0.5b-local", 0, 0)
	return t
}

// PriceFor returns the rate card for provider/model. It reports
// false when the pair is not in the table — the caller must say
// "price unknown", never $0. Lookup order: exact (provider, model),
// then any entry with that model name (a model configured under a
// different provider key, e.g. an OpenAI-compatible gateway serving
// deepseek-chat), then the ollama free fallback for local models.
func (t *Table) PriceFor(provider, modelName string) (Price, bool) {
	if t == nil {
		return Price{}, false
	}
	p := strings.ToLower(strings.TrimSpace(provider))
	m := strings.ToLower(strings.TrimSpace(modelName))
	if p == "" || m == "" {
		return Price{}, false
	}
	if byModel, ok := t.entries[p]; ok {
		if pr, ok := byModel[m]; ok {
			return pr, true
		}
	}
	// Model-only fallback: the same model served under another
	// provider key. Deterministic (sorted provider keys) so the
	// result never depends on map iteration order.
	providers := make([]string, 0, len(t.entries))
	for k := range t.entries {
		providers = append(providers, k)
	}
	sort.Strings(providers)
	for _, k := range providers {
		if pr, ok := t.entries[k][m]; ok {
			return pr, true
		}
	}
	// Local inference is free whatever the model is called.
	if p == "ollama" {
		return Price{}, true
	}
	return Price{}, false
}

// Estimate prices usage at the (provider, model) rate card: prompt
// tokens at the input rate, completion tokens at the output rate,
// and subagentTokens (whose split is unknown — see the package doc)
// at the output rate. It returns false when the price is unknown or
// any count is negative; the caller must then report "unknown",
// never $0. A zero-token usage on a known model estimates $0 and
// returns true.
func (t *Table) Estimate(provider, modelName string, u model.Usage, subagentTokens int) (float64, bool) {
	if u.PromptTokens < 0 || u.CompletionTokens < 0 || u.TotalTokens < 0 || subagentTokens < 0 {
		return 0, false
	}
	pr, ok := t.PriceFor(provider, modelName)
	if !ok {
		return 0, false
	}
	const perMillion = 1_000_000.0
	usd := float64(u.PromptTokens)*pr.InputPerMillion/perMillion +
		float64(u.CompletionTokens)*pr.OutputPerMillion/perMillion +
		float64(subagentTokens)*pr.OutputPerMillion/perMillion
	return usd, true
}

// EstimateTotals is Estimate over raw counts, for callers that
// track prompt/completion/subagent separately (e.g. the per-chat
// daily budget) instead of carrying a model.Usage.
func (t *Table) EstimateTotals(provider, modelName string, prompt, completion, subagent int) (float64, bool) {
	return t.Estimate(provider, modelName, model.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion + subagent,
	}, subagent)
}

// FormatUSD renders an estimated amount for display: four decimals
// under $1 (a day of DeepSeek chat is a fraction of a cent and
// "$0.00" would hide it), two decimals at $1 and above. Callers
// append " (est.)" themselves where the surrounding text does not
// already say the figure is estimated.
func FormatUSD(v float64) string {
	if v < 0 {
		v = 0
	}
	if v < 1 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}
