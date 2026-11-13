// Package budget enforces a per-chat daily token budget (P2-O9).
//
// One Telegram chat must not be able to spend the daemon's whole
// allowance: each chat gets its own token budget per UTC day, and
// turns are refused once the chat is over it. The budget is
// standalone — it keeps no provider, session or config state and
// imports nothing from Orion — so it can be constructed from the
// config value (config.PerChatDailyTokenBudget) and consulted at the
// turn boundary by the daemon (see the call-site snippet in the rc8
// report; the coordinator wires it into runTurn).
//
// Semantics: Check is a pure pre-flight test — "would recording
// tokens more for this chat today exceed the budget?" — and Record
// is the enforcing commit. Record ALWAYS accounts the tokens
// (accounting must reflect reality, including a turn that pushed a
// chat over the budget) and returns a *BudgetExceededError when the
// new total is over the limit, so the caller can refuse the NEXT
// turn. A limit <= 0 means unlimited: Check always passes and
// Record never errors.
//
// The day is the UTC calendar date; usage resets at UTC midnight.
// Usage is in-memory only: a daemon restart resets the counters.
// That is deliberate — the budget is a rate control, not billing —
// and it is documented here so nobody mistakes it for durable
// accounting.
//
// RecordUsage additionally keeps the prompt/completion/subagent
// split for the day, so the USD estimate (internal/cost) can price
// the day's spend: prompt at the input rate, completion and
// subagent tokens at the output rate. The split is accounting only;
// the budget meter is always the total, and Record (total only)
// stays the simple path for callers without a split.
package budget

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/irfan/orion/internal/model"
)

// ErrBudgetExceeded is the sentinel for a breached per-chat daily
// budget; errors.Is(err, ErrBudgetExceeded) matches it.
var ErrBudgetExceeded = errors.New("budget: per-chat daily token budget exceeded")

// BudgetExceededError describes a breach: which chat, how much it
// had already used today, how much was requested/recorded, and the
// limit. It unwraps to ErrBudgetExceeded.
type BudgetExceededError struct {
	ChatID    int64
	Used      int // tokens already consumed today (before this request)
	Requested int // tokens the request would add (Check) or added (Record)
	Limit     int
}

func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("budget: chat %d exceeded its daily token budget: %d used + %d requested > %d limit (resets at UTC midnight)", e.ChatID, e.Used, e.Requested, e.Limit)
}

// Unwrap lets errors.Is(err, ErrBudgetExceeded) match.
func (e *BudgetExceededError) Unwrap() error { return ErrBudgetExceeded }

// chatDay is one chat's usage for one UTC day. prompt/completion
// are the provider-reported split for the day's turns; subagent is
// the subagent tokens consumed by those turns (already included in
// used — the agent adds them to the turn's TotalTokens — and kept
// separately only so the USD estimate can price them at the output
// rate, since their own split is unknown). used is the budget
// meter: the sum of TotalTokens as reported, subagent included.
type chatDay struct {
	day        string // "2006-01-02" in UTC
	used       int
	prompt     int
	completion int
	subagent   int
}

// DayUsage is one chat's token usage for today, with the split the
// USD estimate needs. Total is the budget meter (used); Prompt +
// Completion + Subagent need not sum to Total: a provider that
// reports no split leaves Prompt/Completion at 0 while Total still
// counts, and subagent tokens are already inside Total.
type DayUsage struct {
	Prompt     int
	Completion int
	Subagent   int
	Total      int
}

// Budget is a per-chat daily token budget. The zero value is not
// usable (use New); it is safe for concurrent use.
type Budget struct {
	mu    sync.Mutex
	limit int
	now   func() time.Time
	usage map[int64]*chatDay
}

// New returns a Budget with the given per-chat daily token limit. A
// limit <= 0 means unlimited.
func New(limit int) *Budget {
	return NewWithClock(limit, time.Now)
}

// NewWithClock is New with an injectable clock (for tests that need
// to cross UTC midnight without waiting for it). A nil clock means
// time.Now.
func NewWithClock(limit int, now func() time.Time) *Budget {
	if now == nil {
		now = time.Now
	}
	return &Budget{limit: limit, now: now, usage: map[int64]*chatDay{}}
}

// Limit returns the configured per-chat daily token limit (<= 0
// means unlimited).
func (b *Budget) Limit() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit
}

// SetLimit changes the per-chat daily token limit. A limit <= 0
// means unlimited. Usage already recorded is kept: lowering the
// limit below current usage makes the next Check fail until the day
// rolls over, it does not erase the usage.
func (b *Budget) SetLimit(limit int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.limit = limit
}

// today returns the current UTC calendar date as "2006-01-02".
func (b *Budget) today() string {
	return b.now().UTC().Format("2006-01-02")
}

// entryLocked returns the chat's entry for today, resetting it when
// the stored day is stale. Caller must hold b.mu.
func (b *Budget) entryLocked(chatID int64) *chatDay {
	today := b.today()
	e, ok := b.usage[chatID]
	if !ok {
		e = &chatDay{day: today}
		b.usage[chatID] = e
		return e
	}
	if e.day != today {
		e.day = today
		e.used = 0
		e.prompt = 0
		e.completion = 0
		e.subagent = 0
	}
	return e
}

// Check reports whether recording tokens more for chatID today would
// exceed the budget. It does not record anything. A negative tokens
// value is a caller error. With an unlimited budget (limit <= 0)
// Check always passes.
func (b *Budget) Check(chatID int64, tokens int) error {
	if tokens < 0 {
		return fmt.Errorf("budget: tokens must not be negative, got %d", tokens)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		return nil
	}
	e := b.entryLocked(chatID)
	if e.used+tokens > b.limit {
		return &BudgetExceededError{ChatID: chatID, Used: e.used, Requested: tokens, Limit: b.limit}
	}
	return nil
}

// Record adds tokens to chatID's usage for today and reports whether
// the new total exceeds the budget. The tokens are always recorded —
// even when they push the chat over the limit — because the spend
// already happened and the next Check must see it. A negative tokens
// value is a caller error and records nothing. It is RecordUsage
// without a split (prompt/completion/subagent stay 0); the budget
// meter is identical either way.
func (b *Budget) Record(chatID int64, tokens int) error {
	return b.RecordUsage(chatID, model.Usage{TotalTokens: tokens}, 0)
}

// RecordUsage adds a turn's reported usage to chatID's usage for
// today and reports whether the new total exceeds the budget. It is
// Record with the split kept: u.TotalTokens is the budget meter
// (the agent already folds subagent tokens into TotalTokens, so
// they are NOT added again), while u.PromptTokens,
// u.CompletionTokens and subagentTokens are recorded separately for
// the USD estimate. The tokens are always recorded — even when they
// push the chat over the limit — because the spend already happened
// and the next Check must see it. A negative count in any field is
// a caller error and records nothing.
func (b *Budget) RecordUsage(chatID int64, u model.Usage, subagentTokens int) error {
	if u.PromptTokens < 0 || u.CompletionTokens < 0 || u.TotalTokens < 0 || subagentTokens < 0 {
		return fmt.Errorf("budget: token counts must not be negative, got prompt=%d completion=%d total=%d subagent=%d", u.PromptTokens, u.CompletionTokens, u.TotalTokens, subagentTokens)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entryLocked(chatID)
	e.used += u.TotalTokens
	e.prompt += u.PromptTokens
	e.completion += u.CompletionTokens
	e.subagent += subagentTokens
	if b.limit > 0 && e.used > b.limit {
		return &BudgetExceededError{ChatID: chatID, Used: e.used - u.TotalTokens, Requested: u.TotalTokens, Limit: b.limit}
	}
	return nil
}

// DayUsage returns chatID's token usage for today, with the
// prompt/completion/subagent split. It is zero for a chat with no
// usage, and it resets at UTC midnight.
func (b *Budget) DayUsage(chatID int64) DayUsage {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entryLocked(chatID)
	return DayUsage{Prompt: e.prompt, Completion: e.completion, Subagent: e.subagent, Total: e.used}
}

// Usage returns the tokens chatID has consumed today. It is 0 for a
// chat with no usage, and it resets at UTC midnight.
func (b *Budget) Usage(chatID int64) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.entryLocked(chatID).used
}

// Remaining returns how many tokens chatID may still consume today.
// It is 0 when the chat is at or over its limit, and -1 when the
// budget is unlimited.
func (b *Budget) Remaining(chatID int64) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		return -1
	}
	rem := b.limit - b.entryLocked(chatID).used
	if rem < 0 {
		return 0
	}
	return rem
}

// ResetAt returns the UTC midnight at which today's usage resets.
func (b *Budget) ResetAt() time.Time {
	now := b.now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
}
