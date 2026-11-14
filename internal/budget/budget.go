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
//
// Usage is persisted when a file is set with SetFile (F-A5-P2-19):
// every record auto-saves atomically (temp file + rename + dir
// fsync, like the session store) and a new Budget on the same file
// reloads today's totals, so a daemon restart no longer resets
// every chat's daily spend. Without a file the budget stays
// in-memory only. A stale file (a previous UTC day) loads as
// empty: yesterday's spend never counts against today.
//
// RecordUsage additionally keeps the prompt/completion/subagent
// split for the day, so the USD estimate (internal/cost) can price
// the day's spend: prompt at the input rate, completion and
// subagent tokens at the output rate. RecordUsageForModel keeps the
// same split per (provider, model) (F-A5-P2-18), so /usage prices
// each model's tokens at that model's own rate instead of repricing
// the whole day at the chat's current model. The split is
// accounting only; the budget meter is always the total, and Record
// (total only) stays the simple path for callers without a split.
//
// The usage alert threshold (F-A18-11) reads the same meter with a
// different consequence: CheckAlert reports when a chat's usage
// today reaches a configured absolute token count, once per chat
// per day, and MarkAlerted records the delivery — persisted with
// the usage, so a restart never re-alerts for the same day. The
// alert fires whether the budget limit is finite or unlimited; it
// never refuses a turn, it only notifies the owner (the App layer
// owns the Telegram send — the Budget type has no sender).
package budget

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// modelDay is one (provider, model) slice of a chatDay: the same
// split as chatDay, so /usage can price each model's tokens at that
// model's own rate (F-A5-P2-18). The "" provider / "" model bucket
// holds records made without a model (RecordUsage); it is priced at
// the chat's current model by the caller.
type modelDay struct {
	used       int
	prompt     int
	completion int
	subagent   int
}

// modelKey identifies one (provider, model) bucket. Provider and
// Model are normalised (lowercase, trimmed) at record time, so
// "DeepSeek" and "deepseek" never split a bucket.
type modelKey struct {
	Provider string
	Model    string
}

// normalizeModelKey lowercases and trims a (provider, model) pair.
func normalizeModelKey(provider, modelName string) modelKey {
	return modelKey{
		Provider: strings.ToLower(strings.TrimSpace(provider)),
		Model:    strings.ToLower(strings.TrimSpace(modelName)),
	}
}

// chatDay is one chat's usage for one UTC day. prompt/completion
// are the provider-reported split for the day's turns; subagent is
// the subagent tokens consumed by those turns (already included in
// used — the agent adds them to the turn's TotalTokens — and kept
// separately only so the USD estimate can price them at the output
// rate, since their own split is unknown). used is the budget
// meter: the sum of TotalTokens as reported, subagent included.
// byModel is the same split per (provider, model); it always sums
// to the day totals.
type chatDay struct {
	day        string // "2006-01-02" in UTC
	used       int
	prompt     int
	completion int
	subagent   int
	byModel    map[modelKey]*modelDay
	// alerted records that the owner has already been alerted
	// about this chat's usage today (F-A18-11): the alert fires
	// once per chat per day, never once per turn after the
	// threshold is crossed. It resets with the day (see
	// entryLocked) and is persisted with the usage, so a daemon
	// restart does not re-alert for a day that already alerted.
	alerted bool
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
	// alertThreshold is the per-chat daily usage threshold
	// (F-A18-11) in tokens: when a chat's recorded usage for
	// today reaches it, CheckAlert reports a crossing (once per
	// chat per day — see chatDay.alerted). 0 or negative means
	// disabled. It is an absolute count, independent of limit:
	// it fires with an unlimited budget (limit <= 0) exactly as
	// with a finite one, and may be above a finite limit.
	alertThreshold int
	now            func() time.Time
	usage          map[int64]*chatDay
	// file is the persistence path set with SetFile ("" means
	// in-memory only); loadedChats counts the chats restored by
	// the last SetFile load, so the caller can log the reload.
	file        string
	loadedChats int
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

// SetAlertThreshold changes the per-chat daily usage alert
// threshold (F-A18-11) in tokens. A threshold <= 0 disables the
// alert. The threshold is an absolute token count, independent of
// the budget limit: it fires with an unlimited budget exactly as
// with a finite one. Changing the threshold never clears an
// "alerted today" flag — a chat already alerted today is not
// re-alerted even if the threshold is lowered below its usage.
func (b *Budget) SetAlertThreshold(threshold int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.alertThreshold = threshold
}

// AlertThreshold returns the configured per-chat daily usage alert
// threshold in tokens (<= 0 means the alert is disabled).
func (b *Budget) AlertThreshold() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.alertThreshold
}

// CheckAlert reports whether chatID's usage today has reached the
// alert threshold and the owner has not been alerted about this
// chat today yet. It returns the chat's usage today, the threshold
// it was evaluated against, and whether the alert should fire. It
// is a pure check: it never marks the chat alerted — the caller
// marks it with MarkAlerted only after the alert was actually
// delivered, so a failed delivery is retried on the next record
// instead of silently lost for the day. With the alert disabled
// (threshold <= 0) it never fires, whatever the usage is; a chat
// already alerted today never fires again until the UTC day rolls
// over; usage below the threshold never fires. The threshold is
// an absolute count, so the budget limit (finite or unlimited)
// plays no part in the decision.
func (b *Budget) CheckAlert(chatID int64) (used, threshold int, crossed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	threshold = b.alertThreshold
	e := b.entryLocked(chatID)
	if threshold <= 0 {
		return e.used, threshold, false
	}
	if e.alerted {
		return e.used, threshold, false
	}
	if e.used < threshold {
		return e.used, threshold, false
	}
	return e.used, threshold, true
}

// MarkAlerted records that the owner has been alerted about
// chatID's usage today, so CheckAlert stays silent for this chat
// until the UTC day rolls over. The flag is persisted with the
// usage (when a file is set), so a daemon restart does not
// re-alert for a day that already alerted. A save failure is
// returned — the in-memory flag is already set, so the alert
// still fires at most once for this process's lifetime; the
// caller logs the error.
func (b *Budget) MarkAlerted(chatID int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entryLocked(chatID)
	e.alerted = true
	return b.saveLocked()
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
		e = &chatDay{day: today, byModel: map[modelKey]*modelDay{}}
		b.usage[chatID] = e
		return e
	}
	if e.day != today {
		e.day = today
		e.used = 0
		e.prompt = 0
		e.completion = 0
		e.subagent = 0
		e.byModel = map[modelKey]*modelDay{}
		// The alert flag resets with the day: a chat alerted
		// yesterday may alert again today (F-A18-11).
		e.alerted = false
	}
	if e.byModel == nil {
		e.byModel = map[modelKey]*modelDay{}
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
	return b.RecordUsageForModel(chatID, "", "", u, subagentTokens)
}

// RecordUsageForModel is RecordUsage with the (provider, model)
// the tokens were spent on, so the day's USD estimate can price
// each model's tokens at that model's own rate (F-A5-P2-18). The
// per-model buckets always sum to the day totals; the budget meter
// is unchanged. An empty provider/model records into the "" bucket,
// priced by the caller at the chat's current model. The same
// negative-count and over-limit rules as RecordUsage apply.
func (b *Budget) RecordUsageForModel(chatID int64, provider, modelName string, u model.Usage, subagentTokens int) error {
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
	key := normalizeModelKey(provider, modelName)
	md := e.byModel[key]
	if md == nil {
		md = &modelDay{}
		e.byModel[key] = md
	}
	md.used += u.TotalTokens
	md.prompt += u.PromptTokens
	md.completion += u.CompletionTokens
	md.subagent += subagentTokens
	// Best-effort auto-save: a failed persist must never lose the
	// in-memory accounting or fail the turn that just completed —
	// the spend is recorded either way, and Save() surfaces the
	// error to a caller that checks. The save runs while mu is
	// held so a concurrent record cannot interleave a torn write.
	b.saveLocked()
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

// DayUsageByModel returns chatID's token usage for today split by
// (provider, model), so the caller can price each model's tokens at
// that model's own rate. It is empty for a chat with no usage; the
// buckets always sum to DayUsage. The "" provider / "" model bucket
// holds records made without a model (RecordUsage); the caller
// prices that bucket at the chat's current model.
func (b *Budget) DayUsageByModel(chatID int64) map[modelKey]DayUsage {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entryLocked(chatID)
	out := make(map[modelKey]DayUsage, len(e.byModel))
	for k, md := range e.byModel {
		out[k] = DayUsage{Prompt: md.prompt, Completion: md.completion, Subagent: md.subagent, Total: md.used}
	}
	return out
}

// ModelKey is the exported form of the (provider, model) bucket key
// returned by DayUsageByModel. Provider and Model are normalised
// (lowercase, trimmed).
type ModelKey = modelKey

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

// --- Persistence (F-A5-P2-19) -------------------------------------------
//
// The budget is a rate control, but an in-memory-only counter is a
// bypass: any daemon restart (crash, /restart, VM reboot) reset
// every chat to zero spend. With a file set, every record
// auto-saves atomically and a fresh Budget reloads today's totals
// on SetFile, so spend survives restarts. The file holds one UTC
// day only; a file from a previous day loads as empty.
//
// Wiring (coordinator: app.Build): after budget.New,
// if cfg.DataDir != "" {
//     if n, err := b.SetFile(filepath.Join(cfg.DataDir, "budget.json")); err != nil {
//         logger.Printf("budget: persist disabled (%v); usage is in-memory only", err)
//     } else if n > 0 {
//         logger.Printf("budget: restored today's usage for %d chat(s) from %s", n, filepath.Join(cfg.DataDir, "budget.json"))
//     }
// }
// No shutdown save is needed: records auto-save.

// budgetFile is the on-disk shape: one UTC day, per-chat totals
// plus the per-model split. The limit is config, never persisted.
type budgetFile struct {
	Version int                       `json:"version"`
	Day     string                    `json:"day"`
	Chats   map[string]budgetFileChat `json:"chats"`
}

type budgetFileChat struct {
	Used       int               `json:"used"`
	Prompt     int               `json:"prompt"`
	Completion int               `json:"completion"`
	Subagent   int               `json:"subagent"`
	Models     []budgetFileModel `json:"models,omitempty"`
	// AlertedToday records that the owner was already alerted
	// about this chat's usage today (F-A18-11): a reload must
	// not re-alert for a day that already alerted.
	AlertedToday bool `json:"alerted_today,omitempty"`
}

type budgetFileModel struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Used       int    `json:"used"`
	Prompt     int    `json:"prompt"`
	Completion int    `json:"completion"`
	Subagent   int    `json:"subagent"`
}

// SetFile enables persistence at path: it loads today's usage from
// the file when present (a missing file means "nothing recorded
// yet", not an error; a file from a previous UTC day loads as
// empty) and auto-saves on every later record. It returns how many
// chats were restored, so the caller can log the reload. A corrupt
// file is an error and persistence stays disabled — the in-memory
// counters keep working, and the corrupt file is left alone for
// the operator (fix or delete it and restart; it is never
// overwritten while SetFile has failed).
func (b *Budget) SetFile(path string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.file = ""
	b.loadedChats = 0
	if strings.TrimSpace(path) == "" {
		return 0, fmt.Errorf("budget: persistence path must not be empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			b.file = path
			return 0, nil
		}
		return 0, fmt.Errorf("budget: read persistence file %q: %w", path, err)
	}
	var f budgetFile
	if err := json.Unmarshal(data, &f); err != nil {
		return 0, fmt.Errorf("budget: parse persistence file %q: %w", path, err)
	}
	if f.Version != 1 {
		return 0, fmt.Errorf("budget: persistence file %q has unsupported version %d (want 1)", path, f.Version)
	}
	today := b.today()
	if f.Day != today {
		// Yesterday's spend never counts against today.
		b.file = path
		return 0, nil
	}
	restored := 0
	for idStr, fc := range f.Chats {
		chatID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		if fc.Used < 0 || fc.Prompt < 0 || fc.Completion < 0 || fc.Subagent < 0 {
			continue
		}
		e := &chatDay{
			day:        today,
			used:       fc.Used,
			prompt:     fc.Prompt,
			completion: fc.Completion,
			subagent:   fc.Subagent,
			byModel:    map[modelKey]*modelDay{},
			alerted:    fc.AlertedToday,
		}
		for _, fm := range fc.Models {
			if fm.Used < 0 || fm.Prompt < 0 || fm.Completion < 0 || fm.Subagent < 0 {
				continue
			}
			e.byModel[normalizeModelKey(fm.Provider, fm.Model)] = &modelDay{
				used: fm.Used, prompt: fm.Prompt, completion: fm.Completion, subagent: fm.Subagent,
			}
		}
		// A chat with no per-model rows (written by an older
		// build, or RecordUsage without a model) keeps its
		// totals; the "" bucket is rebuilt so the split still
		// sums to the totals.
		if len(e.byModel) == 0 && (fc.Used > 0 || fc.Prompt > 0 || fc.Completion > 0 || fc.Subagent > 0) {
			e.byModel[modelKey{}] = &modelDay{used: fc.Used, prompt: fc.Prompt, completion: fc.Completion, subagent: fc.Subagent}
		}
		b.usage[chatID] = e
		restored++
	}
	b.file = path
	b.loadedChats = restored
	return restored, nil
}

// PersistPath returns the persistence file set with SetFile, or ""
// when the budget is in-memory only.
func (b *Budget) PersistPath() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.file
}

// LoadedChats returns how many chats the last SetFile load restored.
func (b *Budget) LoadedChats() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.loadedChats
}

// Save persists today's usage atomically (temp file 0600 in the
// same directory + fsync + rename + dir fsync, like the session
// store). It is a no-op when no file is set. Records already
// auto-save; Save exists for callers that want an explicit,
// checked persist (e.g. shutdown).
func (b *Budget) Save() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.saveLocked()
}

// saveLocked writes the file; caller must hold b.mu. It returns
// nil when no file is set, and never mutates in-memory state.
func (b *Budget) saveLocked() error {
	if b.file == "" {
		return nil
	}
	f := budgetFile{Version: 1, Day: b.today(), Chats: map[string]budgetFileChat{}}
	for chatID, e := range b.usage {
		if e.day != f.Day {
			continue
		}
		fc := budgetFileChat{Used: e.used, Prompt: e.prompt, Completion: e.completion, Subagent: e.subagent, AlertedToday: e.alerted}
		for k, md := range e.byModel {
			fc.Models = append(fc.Models, budgetFileModel{
				Provider: k.Provider, Model: k.Model,
				Used: md.used, Prompt: md.prompt, Completion: md.completion, Subagent: md.subagent,
			})
		}
		f.Chats[fmt.Sprintf("%d", chatID)] = fc
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("budget: encode usage: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(b.file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("budget: create persistence dir %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".budget-*.tmp")
	if err != nil {
		return fmt.Errorf("budget: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("budget: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("budget: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("budget: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("budget: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, b.file); err != nil {
		return fmt.Errorf("budget: rename temp file: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
