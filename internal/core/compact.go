// Auto-compaction (audit P0 #3).
//
// Before this, the history budget was enforced only by trimming the
// request window and by the 200-message FIFO cap in the memory
// store — both silent: when a long session crossed the budget, old
// turns were hidden from the model and then permanently dropped,
// with no summary and no warning. Auto-compaction closes that
// silent-loss path:
//
//   - Trigger: at the start of a turn, the retained history's
//     pressure is measured — the larger of (estimated tokens /
//     MaxHistoryTokens) and (message count / memory.MaxMessages).
//     At AutoCompactThreshold (80%) the oldest turns (everything
//     older than the recent window) are summarised by the provider
//     and dropped from the working history only after the summary
//     is stored. The transcript (internal/session) already holds
//     every turn, so even a failed compaction loses nothing.
//   - Warning band: from AutoCompactWarningThreshold (70%) the
//     turn emits EventCompactionWarning, so the user/agent is told
//     compaction is approaching before it fires.
//   - Summary-before-drop: the summary is appended to the
//     conversation-summary fact (never overwriting earlier
//     summaries — each compaction appends a dated section, oldest
//     sections trimmed past a cap) AND to the transcript as a
//     summary record, before ReplaceMessages drops anything. A
//     failure at any step leaves history untouched.
//   - Circuit breaker: after maxAutoCompactFailures (3)
//     consecutive failures the compactor trips: it stops retrying
//     (no provider call, no history change) and surfaces the
//     tripped state via CompactionStatus until a successful manual
//     /compress (App resets the breaker) or an explicit reset.
//     Tripping never fails a turn — the request trim still keeps
//     the turn inside budget, and the transcript still records it.
//
// The manual /compress path (App.compressWithOn) is untouched and
// keeps working exactly as before; auto-compaction reuses its
// summarisation shape (same prompt, same what-not-to-save gate)
// but never calls into app, so there is no import cycle.
package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
)

// Auto-compaction thresholds, as fractions of the history budget.
// The trigger sits at 80% (the audit's "~80% of the history
// budget"); the warning band opens at 70% so the user sees it
// coming.
const (
	// AutoCompactThreshold is the history pressure at which a
	// turn auto-compacts before running.
	AutoCompactThreshold = 0.80
	// AutoCompactWarningThreshold is the history pressure at
	// which a turn warns that compaction is approaching.
	AutoCompactWarningThreshold = 0.70
	// AutoCompactKeepRecent is how many recent messages a
	// compaction keeps — the same recent window /compress keeps
	// and the agent loop feeds the model.
	AutoCompactKeepRecent = 10
	// maxAutoCompactFailures is the circuit-breaker trip count:
	// this many consecutive failed compactions stop further
	// attempts until reset.
	maxAutoCompactFailures = 3
	// maxAutoCompactPressure is the ceiling above which a turn
	// does NOT auto-compact: summarising a prefix many times the
	// budget would itself need a provider call far over the
	// context window (and burn more tokens than the turn), so the
	// bounded request trim — the existing safety net — handles
	// the turn instead. A session growing normally never reaches
	// it: compaction fires at 80% on the way up, so pressure can
	// only jump past the ceiling in one turn via a single giant
	// tool result, or arrive there from a legacy store. The
	// transcript still records every turn, and manual /compress
	// remains available for that state.
	maxAutoCompactPressure = 2.0
	// maxSummaryFactRunes caps the appended conversation-summary
	// fact: each compaction appends a dated section, and past the
	// cap the oldest sections are dropped so the fact stays
	// bounded while the newest summaries survive.
	maxSummaryFactRunes = 8000
)

// CompactionStatus is a snapshot of the auto-compactor's state,
// for /status, the footer, and tests.
type CompactionStatus struct {
	// Pressure is the last measured history pressure (0..1+).
	Pressure float64
	// Warning is true when pressure is in the warning band.
	Warning bool
	// NeedsCompact is true when pressure reached the trigger.
	NeedsCompact bool
	// Tripped is true when the circuit breaker stopped retries
	// after repeated failures.
	Tripped bool
	// ConsecutiveFailures counts failures since the last success.
	ConsecutiveFailures int
	// LastError is the most recent compaction failure, "" when none.
	LastError string
	// LastCompactAt is when the last successful compaction ran.
	LastCompactAt time.Time
	// Compactions counts successful auto-compactions on this agent.
	Compactions int
}

// HistoryPressure measures how full the retained history is: the
// larger of the token ratio (estimated tokens of msgs, measured
// exactly like the request trim, over tokenBudget) and the count
// ratio (len(msgs) over memory.MaxMessages). Either leg can trip
// compaction: a token-heavy session compacts on tokens, a
// many-short-turns session compacts on count before the FIFO cap
// can silently drop a turn. A non-positive budget disables the
// token leg.
func HistoryPressure(msgs []model.Message, tokenBudget int) float64 {
	pressure := 0.0
	if tokenBudget > 0 {
		tokens := 0
		for _, m := range msgs {
			tokens += messageTokens(m)
		}
		pressure = float64(tokens) / float64(tokenBudget)
	}
	if n := float64(len(msgs)) / float64(memory.MaxMessages); n > pressure {
		pressure = n
	}
	return pressure
}

// compactionSummariser summarises the dropped prefix into the text
// stored before the drop. The default asks the agent's provider;
// tests inject a fake.
type compactionSummariser func(ctx context.Context, old []model.Message) (string, error)

// transcriptSink is the transcript an agent records turns into
// (internal/session.Transcript satisfies it). A nil sink means
// "no transcript wired" — the agent runs exactly as before.
type transcriptSink interface {
	AppendMessage(msg model.Message) error
	AppendSummary(text string) error
}

// compactor holds the auto-compaction state for one agent. All
// fields are guarded by mu; it is safe for concurrent use, though
// in practice only the serialised agent loop touches it.
type compactor struct {
	mu                  sync.Mutex
	summariser          compactionSummariser
	threshold           float64
	warningThreshold    float64
	keepRecent          int
	consecutiveFailures int
	tripped             bool
	lastErr             string
	lastCompactAt       time.Time
	compactions         int
	pressure            float64
}

func newCompactor() *compactor {
	return &compactor{
		threshold:        AutoCompactThreshold,
		warningThreshold: AutoCompactWarningThreshold,
		keepRecent:       AutoCompactKeepRecent,
	}
}

// SetCompactionSummariser replaces the summariser (default: the
// provider-based one). Tests inject a fake here; passing nil
// restores the default.
func (a *Agent) SetCompactionSummariser(fn compactionSummariser) {
	a.compactorMu.Lock()
	defer a.compactorMu.Unlock()
	if a.compactor == nil {
		a.compactor = newCompactor()
	}
	a.compactor.mu.Lock()
	defer a.compactor.mu.Unlock()
	a.compactor.summariser = fn
}

// CompactionStatus returns a snapshot of the auto-compactor's
// state. It never blocks on a running run.
func (a *Agent) CompactionStatus() CompactionStatus {
	a.compactorMu.Lock()
	c := a.compactor
	a.compactorMu.Unlock()
	if c == nil {
		return CompactionStatus{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return CompactionStatus{
		Pressure:            c.pressure,
		Warning:             c.pressure >= c.warningThreshold && c.pressure < c.threshold,
		NeedsCompact:        c.pressure >= c.threshold,
		Tripped:             c.tripped,
		ConsecutiveFailures: c.consecutiveFailures,
		LastError:           c.lastErr,
		LastCompactAt:       c.lastCompactAt,
		Compactions:         c.compactions,
	}
}

// ResetCompactionBreaker clears the circuit breaker after a
// successful manual /compress (or an explicit operator reset), so
// auto-compaction can try again. It does not reset the success
// counters.
func (a *Agent) ResetCompactionBreaker() {
	a.compactorMu.Lock()
	c := a.compactor
	a.compactorMu.Unlock()
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tripped = false
	c.consecutiveFailures = 0
	c.lastErr = ""
}

// SetTranscript wires the per-session transcript the agent
// records every stored message into. A nil sink unwires it (the
// agent then records into memory only, exactly as before). It is
// safe for concurrent use with a running turn: the next stored
// message sees the new sink.
func (a *Agent) SetTranscript(s transcriptSink) {
	a.transcriptMu.Lock()
	defer a.transcriptMu.Unlock()
	a.transcript = s
}

// recordMessage stores one message in memory and, when a
// transcript is wired, appends it there too. A transcript failure
// is logged and never fails the turn: memory is the working store,
// the transcript is the record, and a failed record must not lose
// the turn itself.
func (a *Agent) recordMessage(msg model.Message) {
	if err := a.memory.AddMessage(msg); err != nil {
		a.logf("memory AddMessage (%s) failed: %v", msg.Role, err)
		return
	}
	a.transcriptMu.RLock()
	sink := a.transcript
	a.transcriptMu.RUnlock()
	if sink == nil {
		return
	}
	if err := sink.AppendMessage(msg); err != nil {
		a.logf("transcript AppendMessage (%s) failed (turn kept in memory): %v", msg.Role, err)
	}
}

// recordSummary appends a compaction summary to the wired
// transcript, if any. Failures are logged, never fatal: the
// summary already lives in the conversation-summary fact.
func (a *Agent) recordSummary(summary string) {
	a.transcriptMu.RLock()
	sink := a.transcript
	a.transcriptMu.RUnlock()
	if sink == nil {
		return
	}
	if err := sink.AppendSummary(summary); err != nil {
		a.logf("transcript AppendSummary failed (summary kept as fact): %v", err)
	}
}

// maybeAutoCompact measures the retained history's pressure and,
// when it reaches the trigger, compacts it before the turn runs.
// It returns true when a compaction ran and changed history (the
// caller re-reads the recent window afterwards). It never fails
// the turn: a compaction failure is counted toward the circuit
// breaker, logged, and the turn proceeds on the uncompacted
// history (the request trim still keeps it inside budget).
func (a *Agent) maybeAutoCompact(ctx context.Context, all []model.Message) bool {
	c := a.compactorOrDefault()
	pressure := HistoryPressure(all, a.maxHistoryTokens())
	c.mu.Lock()
	c.pressure = pressure
	tripped := c.tripped
	threshold := c.threshold
	warningThreshold := c.warningThreshold
	keepRecent := c.keepRecent
	c.mu.Unlock()

	if pressure >= warningThreshold && pressure < threshold {
		a.emit(ProgressEvent{Kind: EventCompactionWarning, Step: 0})
		a.logf("history pressure %.0f%%: compaction approaching (trigger %.0f%%)", pressure*100, threshold*100)
		return false
	}
	if pressure < threshold {
		return false
	}
	if pressure > maxAutoCompactPressure {
		// Severely over budget: do not burn a summarisation
		// call on a prefix many times the budget — the
		// request trim handles this turn, as before.
		a.logf("history pressure %.0f%% is far over budget; skipping auto-compaction (request trim handles this turn)", pressure*100)
		return false
	}
	if tripped {
		// Circuit breaker: do not retry, do not call the
		// provider, do not touch history. Surface it every
		// time the trigger is hit so the tripped state cannot
		// go unnoticed while turns keep running.
		a.emit(ProgressEvent{Kind: EventCompactionWarning, Step: 0, Err: errors.New("core: auto-compaction circuit breaker tripped after repeated failures; run /compress")})
		a.logf("history pressure %.0f%% but auto-compaction circuit breaker is tripped; run /compress", pressure*100)
		return false
	}
	if len(all) <= keepRecent {
		return false
	}
	old := append([]model.Message(nil), all[:len(all)-keepRecent]...)
	recent := append([]model.Message(nil), all[len(all)-keepRecent:]...)

	summariser := c.summariserFor(a)
	summaryCtx, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()
	summary, err := summariser(summaryCtx, old)
	if err != nil {
		a.noteCompactionFailure(fmt.Errorf("summarisation: %w", err))
		return false
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		a.noteCompactionFailure(errors.New("summariser returned an empty summary"))
		return false
	}
	// What-not-to-save gate, as in /compress: the summary is
	// model-derived text on its way to becoming a stored fact.
	// Derivable content is dropped; if nothing worth keeping
	// remains, the compaction is refused and history is left
	// untouched.
	if kept, ok := memory.FilterExplicitSave(summary); ok {
		summary = kept
	} else {
		a.noteCompactionFailure(errors.New("summary contained nothing worth saving (gate dropped it all)"))
		return false
	}
	// Summary-before-drop: persist the summary FIRST (fact +
	// transcript), and only then drop the summarised prefix.
	// Either persist failing aborts the compaction with history
	// untouched.
	if err := appendSummaryFact(a.memory, summary); err != nil {
		a.noteCompactionFailure(fmt.Errorf("store summary: %w", err))
		return false
	}
	a.recordSummary(summary)
	if err := a.memory.ReplaceMessages(recent); err != nil {
		a.noteCompactionFailure(fmt.Errorf("drop summarised messages: %w", err))
		return false
	}
	c.mu.Lock()
	c.consecutiveFailures = 0
	c.lastErr = ""
	c.lastCompactAt = time.Now()
	c.compactions++
	c.mu.Unlock()
	a.emit(ProgressEvent{Kind: EventAutoCompacted, Step: 0})
	a.logf("auto-compacted: %d messages summarised, %d kept", len(old), len(recent))
	return true
}

// noteCompactionFailure counts one failed compaction toward the
// circuit breaker and trips it at the limit. The error is logged
// and surfaced via CompactionStatus; the turn is never failed.
func (a *Agent) noteCompactionFailure(err error) {
	c := a.compactorOrDefault()
	c.mu.Lock()
	c.consecutiveFailures++
	failures := c.consecutiveFailures
	c.lastErr = err.Error()
	tripped := failures >= maxAutoCompactFailures
	if tripped {
		c.tripped = true
	}
	c.mu.Unlock()
	if tripped {
		a.logf("auto-compaction failed %d times in a row; circuit breaker tripped: %v (run /compress)", maxAutoCompactFailures, err)
		a.emit(ProgressEvent{Kind: EventCompactionWarning, Step: 0, Err: fmt.Errorf("core: auto-compaction circuit breaker tripped: %w", err)})
	} else {
		a.logf("auto-compaction failed (%d/%d): %v", failures, maxAutoCompactFailures, err)
	}
}

// compactorOrDefault returns the agent's compactor, creating it
// for a zero-valued Agent (one built before this field existed).
func (a *Agent) compactorOrDefault() *compactor {
	a.compactorMu.Lock()
	defer a.compactorMu.Unlock()
	if a.compactor == nil {
		a.compactor = newCompactor()
	}
	return a.compactor
}

// summariserFor returns the configured summariser, or the default
// provider-based one.
func (c *compactor) summariserFor(a *Agent) compactionSummariser {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.summariser != nil {
		return c.summariser
	}
	return a.defaultCompactionSummariser
}

// defaultCompactionSummariser asks the agent's provider for a
// concise summary of the dropped prefix, in the same shape as
// /compress: preserve facts, decisions, names, numbers and anything
// the user asked to remember; invent nothing.
func (a *Agent) defaultCompactionSummariser(ctx context.Context, old []model.Message) (string, error) {
	if a.provider == nil {
		return "", errors.New("no provider available for auto-compaction")
	}
	var b strings.Builder
	b.WriteString("Summarise the following conversation messages into a concise memory. Preserve facts, decisions, names, numbers and anything the user asked to remember. Do not invent anything not in the messages.\n\n")
	for _, m := range old {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
	}
	temp := 0.0
	resp, err := a.provider.Chat(ctx, model.ChatRequest{
		Messages: []model.Message{
			{Role: "system", Content: "You summarise conversations into compact memories."},
			{Role: "user", Content: b.String()},
		},
		MaxTokens:   800,
		Temperature: &temp,
	})
	if err != nil {
		return "", fmt.Errorf("auto-compaction provider call: %w", err)
	}
	if resp == nil {
		return "", errors.New("auto-compaction provider returned nil response")
	}
	return strings.TrimSpace(resp.Message.Content), nil
}

// appendSummaryFact appends a dated section to the
// conversation-summary fact, so repeated compactions accumulate
// instead of overwriting each other (manual /compress overwrites
// the fact with one fresh summary; auto-compaction must not
// destroy the summary of the previous compaction). Past
// maxSummaryFactRunes the oldest sections are dropped, newest
// kept. The fact key is shared with /compress on purpose: the
// fact-aware recall that surfaces it does not care which path
// wrote it.
func appendSummaryFact(store memory.Store, summary string) error {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return errors.New("core: summary must not be empty")
	}
	section := "## " + time.Now().UTC().Format("2006-01-02 15:04:05 UTC") + "\n" + summary
	existing, found, err := store.GetFact("conversation-summary")
	if err != nil {
		return fmt.Errorf("core: read existing summary fact: %w", err)
	}
	combined := section
	if found && strings.TrimSpace(existing.Value) != "" {
		combined = strings.TrimSpace(existing.Value) + "\n\n" + section
	}
	combined = trimSummaryFact(combined, maxSummaryFactRunes)
	return store.AddFact("conversation-summary", combined)
}

// trimSummaryFact drops the oldest "## <timestamp>" sections until
// the fact fits within maxRunes. Text before the first header (a
// manual /compress summary, which has no header) is treated as its
// own oldest section.
func trimSummaryFact(combined string, maxRunes int) string {
	r := []rune(combined)
	if len(r) <= maxRunes {
		return combined
	}
	sections := splitSummarySections(combined)
	for len(sections) > 1 && totalSummaryRunes(sections) > maxRunes {
		sections = sections[1:]
	}
	if len(sections) == 1 && len([]rune(sections[0])) > maxRunes {
		rr := []rune(sections[0])
		return string(rr[len(rr)-maxRunes:])
	}
	return strings.Join(sections, "\n\n")
}

// splitSummarySections splits a summary fact into its "## "
// sections; leading text without a header stays attached to the
// first section.
func splitSummarySections(combined string) []string {
	var sections []string
	var cur strings.Builder
	for _, line := range strings.SplitAfter(combined, "\n") {
		if strings.HasPrefix(line, "## ") && cur.Len() > 0 {
			sections = append(sections, strings.TrimRight(cur.String(), "\n"))
			cur.Reset()
		}
		cur.WriteString(line)
	}
	if cur.Len() > 0 {
		sections = append(sections, strings.TrimRight(cur.String(), "\n"))
	}
	if len(sections) == 0 {
		return []string{combined}
	}
	return sections
}

func totalSummaryRunes(sections []string) int {
	n := 0
	for _, s := range sections {
		n += len([]rune(s)) + 2 // section + separator
	}
	return n
}
