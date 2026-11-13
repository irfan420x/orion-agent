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
//     /compress (App resets the breaker), an explicit reset, or
//     the cooldown elapses (defaultBreakerCooldown, after which
//     it re-arms itself and retries — a transient provider outage
//     must not disable compaction for the session). Tripping never
//     fails a turn — the request trim still keeps the turn inside
//     budget, and the transcript still records it. While tripped
//     (or while pressure is far over budget and the bounded
//     emergency compaction cannot keep up), turns the FIFO cap
//     drops are NOT silent any more: recordMessage detects the
//     drop, logs it with the count, counts it in
//     CompactionStatus.DroppedWithoutSummary, and spills the dropped
//     turn to the transcript as an explicit "dropped,
//     unsummarised" record (Audit 5 F-A5-1).
//   - Summary verification: a summary is accepted only if it
//     preserves something from the summarised turns (at least
//     minSharedContentTokens distinct shared content tokens, at
//     least minSummaryDistinctTokens distinct content words of
//     its own, a minimum information density, and not below a
//     length floor relative to the input) — a provider returning
//     filler such as "Nothing durable was discussed.", or a
//     degenerate summary of one word repeated hundreds of times,
//     is refused and the prefix is left untouched (Audit 5
//     F-A5-2; Audit 6 F-A6-P2-1).
//
// The manual /compress path (App.compressWithOn) shares the same
// summarisation shape (same prompt, same what-not-to-save gate,
// same preservation check via SummaryPreservesSource, F-A7-P2-4);
// auto-compaction never calls into app, so there is no import
// cycle.
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
	"github.com/irfan/orion/internal/security"
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
	// maxCompactionInputTokens bounds one summarisation call's
	// input (estimated tokens of the oldest turns): the summariser
	// never receives a prompt over the provider context, and a
	// prefix far over budget is chipped away a bounded slice per
	// turn instead of skipped or sent whole (F-A5-1, F-A5-P2-7).
	// 16k tokens is well inside a 64k-context provider with room
	// for the 800-token summary.
	maxCompactionInputTokens = 16000
	// defaultBreakerCooldown is how long a tripped circuit breaker
	// stays down before it re-arms itself and retries: a transient
	// provider outage must not disable auto-compaction for the
	// rest of the session (F-A5-P2-7). A successful manual
	// /compress (ResetCompactionBreaker) still re-arms immediately.
	defaultBreakerCooldown = 5 * time.Minute
	// minSummaryRunes and maxSummaryLengthFloor bound the
	// preservation check's length floor: a summary shorter than
	// inputRunes/summaryLengthDivisor runes (clamped to this range)
	// is rejected as degenerate, alongside the token-overlap check
	// (F-A5-2). The floor is deliberately lenient for filler-heavy
	// input — a long prefix whose only durable content is one fact
	// legitimately summarises short; the overlap check is what
	// catches content-free filler.
	minSummaryRunes       = 20
	maxSummaryLengthFloor = 100
	summaryLengthDivisor  = 2000
	// minSharedContentTokens is how many DISTINCT content tokens
	// a summary must share with the turns it replaces (Audit 6
	// F-A6-P2-1): one shared token is not verification — a summary
	// of the word "project" repeated 200 times shares exactly one
	// token and proves nothing. Three is the earlier design's
	// bar: a summary preserving a single genuine fact ("favourite
	// number is 17") shares three, so real summaries still pass
	// while degenerate ones do not. Refusing a summary is safe:
	// the compaction fails, the breaker counts it, and history
	// is left untouched.
	minSharedContentTokens = 3
	// minSummaryDistinctTokens is how many distinct content words
	// a summary must contain at all: a summary of one word
	// repeated 200 times has exactly one, however long it is.
	minSummaryDistinctTokens = 3
	// minSummaryDensityPercent is the minimum information density
	// of a summary: distinct content words, as a percentage of
	// all words. Repetition padding ("project project project
	// ...") collapses the density towards zero; ordinary prose
	// sits well above 10%.
	minSummaryDensityPercent = 10
	// summariserPromptOverheadTokens reserves room inside
	// maxCompactionInputTokens for the summariser prompt header,
	// the system message and per-message role framing, so the
	// TOTAL provider-bound input — not just the message bodies —
	// stays within the 16,000-token cap (Audit 6 F-A6-P2-2). The
	// header is ~45 tokens and the system message ~8; 128 leaves
	// ample margin for role framing on many-message prefixes.
	summariserPromptOverheadTokens = 128
	// maxDroppedTurnContentRunes truncates one spilled turn's
	// content in the "dropped, unsummarised" transcript record.
	maxDroppedTurnContentRunes = 500
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
	// DroppedWithoutSummary counts turns the store's FIFO cap dropped
	// without a summary since the last breaker reset (Audit 5
	// F-A5-1): the count is visible here, in /status and on the
	// card, and each dropped turn is spilled to the transcript as
	// an explicit "dropped, unsummarised" record. The durable
	// record survives a reset; this counter does not.
	DroppedWithoutSummary int
	// LastDroppedAt is when the most recent unsummarised drop
	// happened, zero when none has.
	LastDroppedAt time.Time
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
	mu                    sync.Mutex
	summariser            compactionSummariser
	threshold             float64
	warningThreshold      float64
	keepRecent            int
	consecutiveFailures   int
	tripped               bool
	trippedAt             time.Time
	breakerCooldown       time.Duration // 0 = defaultBreakerCooldown
	lastErr               string
	lastCompactAt         time.Time
	compactions           int
	pressure              float64
	droppedWithoutSummary int
	lastDroppedAt         time.Time
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
		Pressure:              c.pressure,
		Warning:               c.pressure >= c.warningThreshold && c.pressure < c.threshold,
		NeedsCompact:          c.pressure >= c.threshold,
		Tripped:               c.tripped,
		ConsecutiveFailures:   c.consecutiveFailures,
		LastError:             c.lastErr,
		LastCompactAt:         c.lastCompactAt,
		Compactions:           c.compactions,
		DroppedWithoutSummary: c.droppedWithoutSummary,
		LastDroppedAt:         c.lastDroppedAt,
	}
}

// ResetCompactionBreaker clears the circuit breaker after a
// successful manual /compress (or an explicit operator reset), so
// auto-compaction can try again. It does not reset the success
// counters — but it does reset the dropped-without-summary count:
// the count is "since the last reset", and a reset marks the
// operator having taken action (the durable record of the drops
// themselves lives in the transcript's spill records and the log).
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
	c.trippedAt = time.Time{}
	c.consecutiveFailures = 0
	c.lastErr = ""
	c.droppedWithoutSummary = 0
	c.lastDroppedAt = time.Time{}
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
//
// FIFO-drop spill (Audit 5 F-A5-1): the store caps retained history
// at memory.MaxMessages, dropping the oldest turns first. When this
// add pushes the store to its cap, the oldest turn(s) are dropped —
// and a drop with no summary must not be silent. Before adding,
// the current oldest turn is snapshotted; after adding, a store
// still pinned at the cap proves the drop happened, and the dropped
// turn is spilled (counted, logged, written to the transcript as an
// explicit "dropped, unsummarised" record). A store without a cap
// (a test fake, or one that grew past it) never pins at the cap, so
// no spill is recorded for it.
func (a *Agent) recordMessage(msg model.Message) {
	var before []model.Message
	if all, err := a.memory.RecentMessages(0); err == nil && len(all) >= memory.MaxMessages {
		before = all
	}
	if err := a.memory.AddMessage(msg); err != nil {
		a.logf("memory AddMessage (%s) failed: %v", msg.Role, err)
		return
	}
	if len(before) > 0 {
		if after, err := a.memory.RecentMessages(0); err == nil && len(after) == memory.MaxMessages {
			// The store was at (or over) its cap and is still
			// pinned at it after the add: the oldest
			// len(before)+1-MaxMessages turns were dropped.
			n := len(before) + 1 - memory.MaxMessages
			if n > len(before) {
				n = len(before)
			}
			if n > 0 {
				a.spillDroppedTurns(before[:n])
			}
		}
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

// dropSink is the optional transcript leg for FIFO-spill records: a
// sink that records turns the cap dropped without a summary. The
// real session.Transcript implements it; a test fake need not — the
// spill then still counts and logs, it just has nowhere durable to
// go in that test.
type dropSink interface {
	AppendDropped(text string) error
}

// spillDroppedTurns records turns the FIFO cap dropped without a
// summary: count them in CompactionStatus, log them with the count,
// and spill each one to the transcript as an explicit "dropped,
// unsummarised" record. A transcript failure is logged and never
// fails the turn — the count and the log already record the loss.
func (a *Agent) spillDroppedTurns(dropped []model.Message) {
	if len(dropped) == 0 {
		return
	}
	c := a.compactorOrDefault()
	c.mu.Lock()
	c.droppedWithoutSummary += len(dropped)
	c.lastDroppedAt = time.Now()
	total := c.droppedWithoutSummary
	c.mu.Unlock()
	first := dropped[0]
	a.logf("FIFO cap dropped %d turn(s) without summary (%d total since last reset; oldest was %s: %q)", len(dropped), total, first.Role, truncateRunesForLog(first.Content, 80))
	a.transcriptMu.RLock()
	sink := a.transcript
	a.transcriptMu.RUnlock()
	ds, ok := sink.(dropSink)
	if !ok {
		return
	}
	for _, m := range dropped {
		if err := ds.AppendDropped(formatDroppedTurn(m)); err != nil {
			a.logf("transcript AppendDropped failed (drop already counted): %v", err)
		}
	}
}

// formatDroppedTurn renders one dropped turn for the spill record:
// its role and truncated content, so the record says what was lost
// without keeping a whole giant tool result. An assistant message
// with no content names its tool calls instead.
func formatDroppedTurn(m model.Message) string {
	content := strings.TrimSpace(m.Content)
	if content == "" && len(m.ToolCalls) > 0 {
		names := make([]string, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			names[i] = tc.Name
		}
		content = "tool calls: " + strings.Join(names, ", ")
	}
	if r := []rune(content); len(r) > maxDroppedTurnContentRunes {
		content = string(r[:maxDroppedTurnContentRunes]) + "..."
	}
	return "Dropped without summary (auto-compaction unavailable): " + m.Role + ": " + content
}

// truncateRunesForLog shortens s to at most n runes for a log line,
// appending "..." when it truncates.
func truncateRunesForLog(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// recordSummary appends a compaction summary to the wired
// transcript, if any. It returns the transcript error: a failed
// summary persist aborts the compaction before anything is dropped
// (Audit 5 F-A5-P2-1) — the transcript is the unbounded backstop,
// and dropping the prefix while the summary never landed there
// would silently lose it. With no transcript wired there is
// nothing to persist and the call succeeds.
func (a *Agent) recordSummary(summary string) error {
	a.transcriptMu.RLock()
	sink := a.transcript
	a.transcriptMu.RUnlock()
	if sink == nil {
		return nil
	}
	if err := sink.AppendSummary(summary); err != nil {
		return fmt.Errorf("core: transcript AppendSummary: %w", err)
	}
	return nil
}

// maybeAutoCompact measures the retained history's pressure and,
// when it reaches the trigger, compacts it before the turn runs.
// It returns true when a compaction ran and changed history (the
// caller re-reads the recent window afterwards). It never fails
// the turn: a compaction failure is counted toward the circuit
// breaker, logged, and the turn proceeds on the uncompacted
// history (the request trim still keeps it inside budget).
//
// Bounded input (F-A5-P2-7): only a bounded slice of the oldest
// turns is ever summarised (maxCompactionInputTokens), so a prefix
// far over budget is chipped away a slice per turn instead of
// skipped entirely (F-A5-1) or sent whole to the provider. Only
// the slice actually summarised is dropped; the rest stays for
// the next turn's compaction.
func (a *Agent) maybeAutoCompact(ctx context.Context, all []model.Message) bool {
	c := a.compactorOrDefault()
	pressure := HistoryPressure(all, a.maxHistoryTokens())
	c.mu.Lock()
	prevPressure := c.pressure
	c.pressure = pressure
	tripped := c.tripped
	trippedAt := c.trippedAt
	cooldown := c.breakerCooldown
	if cooldown <= 0 {
		cooldown = defaultBreakerCooldown
	}
	threshold := c.threshold
	warningThreshold := c.warningThreshold
	keepRecent := c.keepRecent
	dropped := c.droppedWithoutSummary
	c.mu.Unlock()

	if pressure >= warningThreshold && pressure < threshold {
		a.emit(ProgressEvent{Kind: EventCompactionWarning, Step: 0})
		a.logf("history pressure %.0f%%: compaction approaching (trigger %.0f%%)", pressure*100, threshold*100)
		return false
	}
	if pressure < threshold {
		return false
	}
	// Jump warning (F-A5-P2-7): a single giant turn can leap over
	// the warning band without ever being inside it — the user
	// still gets the advance notice, and compaction proceeds.
	if prevPressure < warningThreshold {
		a.emit(ProgressEvent{Kind: EventCompactionWarning, Step: 0})
		a.logf("history pressure jumped from %.0f%% to %.0f%%, skipping the warning band (auto-compacts at %.0f%%)", prevPressure*100, pressure*100, threshold*100)
	}
	if tripped {
		// Circuit breaker: re-arm after the cooldown (a transient
		// provider outage must not disable compaction for the
		// rest of the session), otherwise do not retry, do not
		// call the provider, do not touch history. Surface it
		// every time the trigger is hit so the tripped state
		// cannot go unnoticed while turns keep running — and name
		// any turns already dropped without summary.
		if !trippedAt.IsZero() && time.Since(trippedAt) >= cooldown {
			c.mu.Lock()
			c.tripped = false
			c.trippedAt = time.Time{}
			c.consecutiveFailures = 0
			c.lastErr = ""
			c.mu.Unlock()
			a.logf("auto-compaction circuit breaker re-armed after %s cooldown; retrying", cooldown)
		} else {
			errText := "core: auto-compaction circuit breaker tripped after repeated failures; run /compress"
			if dropped > 0 {
				errText = fmt.Sprintf("core: auto-compaction circuit breaker tripped after repeated failures; %d turn(s) dropped without summary; run /compress", dropped)
			}
			a.emit(ProgressEvent{Kind: EventCompactionWarning, Step: 0, Err: errors.New(errText)})
			a.logf("history pressure %.0f%% but auto-compaction circuit breaker is tripped; run /compress", pressure*100)
			return false
		}
	}
	if len(all) <= keepRecent {
		return false
	}
	old := append([]model.Message(nil), all[:len(all)-keepRecent]...)
	recent := append([]model.Message(nil), all[len(all)-keepRecent:]...)
	// Bounded input, total not per-message (F-A6-P2-2): the
	// summariser's whole input — messages plus the prompt
	// overhead it is wrapped in — fits the 16,000-token cap, and
	// a single giant message is truncated to the cap instead of
	// being handed to the provider whole.
	toSummarise := boundedOldPrefix(old, maxCompactionInputTokens-summariserPromptOverheadTokens)
	kept := append(append([]model.Message(nil), old[len(toSummarise):]...), recent...)

	summariser := c.summariserFor(a)
	summaryCtx, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()
	summary, err := summariser(summaryCtx, toSummarise)
	if err != nil {
		a.noteCompactionFailure(fmt.Errorf("summarisation: %w", err))
		return false
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		a.noteCompactionFailure(errors.New("summariser returned an empty summary"))
		return false
	}
	// Redact before anything else touches the summary (Audit 6
	// F-A6-P2-3): a summariser echoing a secret from the turns
	// must not reach the stored fact or the transcript. The
	// transcript's own scrub is a second layer, not the only one
	// — the memory fact has no scrub of its own.
	summary = security.RedactSecrets(summary)
	if strings.TrimSpace(summary) == "" {
		a.noteCompactionFailure(errors.New("summary contained nothing after redaction"))
		return false
	}
	// What-not-to-save gate, as in /compress: the summary is
	// model-derived text on its way to becoming a stored fact.
	// Derivable content is dropped; if nothing worth keeping
	// remains, the compaction is refused and history is left
	// untouched.
	if keptSummary, ok := memory.FilterExplicitSave(summary); ok {
		summary = keptSummary
	} else {
		a.noteCompactionFailure(errors.New("summary contained nothing worth saving (gate dropped it all)"))
		return false
	}
	// Preservation check (F-A5-2, hardened F-A6-P2-1): the summary
	// must preserve something from the turns it replaces — at
	// least minSharedContentTokens distinct shared content tokens,
	// at least minSummaryDistinctTokens distinct content words of
	// its own, a minimum information density, and not below a
	// length floor relative to the input. A provider returning
	// filler ("Nothing durable was discussed.") passes the gate
	// above but preserves nothing, and dropping the prefix on its
	// word would lose the history for a sentence that says
	// nothing; a summary of one word repeated 200 times shares a
	// single token and fails the same way.
	if !summaryPreservesSource(summary, toSummarise) {
		a.noteCompactionFailure(errors.New("summary preserved nothing from the summarised turns (no shared content tokens, or below the length floor)"))
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
	if err := a.recordSummary(summary); err != nil {
		a.noteCompactionFailure(err)
		return false
	}
	if err := a.memory.ReplaceMessages(kept); err != nil {
		a.noteCompactionFailure(fmt.Errorf("drop summarised messages: %w", err))
		return false
	}
	c.mu.Lock()
	c.consecutiveFailures = 0
	c.lastErr = ""
	c.lastCompactAt = time.Now()
	c.compactions++
	// Pressure is measured on the history that is actually kept
	// now — /status and the footer must not keep reporting the
	// pre-compaction pressure (F-A5-P2-2).
	c.pressure = HistoryPressure(kept, a.maxHistoryTokens())
	c.mu.Unlock()
	a.emit(ProgressEvent{Kind: EventAutoCompacted, Step: 0})
	a.logf("auto-compacted: %d messages summarised, %d kept", len(toSummarise), len(kept))
	return true
}

// boundedOldPrefix takes the oldest messages of old whose estimated
// tokens fit within maxTokens, so the summariser never receives a
// prompt over the provider context. A single message that alone
// exceeds the cap is TRUNCATED to the cap (Audit 6 F-A6-P2-2) —
// the old "at least one message, even if it alone exceeds the cap"
// rule handed one 20,004-token message to the provider uncapped.
// Truncation only bounds what the summariser sees: the caller
// still drops the whole message (len counts the original), so the
// giant turn is summarised from its truncated view and then gone,
// never left behind un-summarised.
func boundedOldPrefix(old []model.Message, maxTokens int) []model.Message {
	total := 0
	n := 0
	for i, m := range old {
		t := messageTokens(m)
		if n > 0 && total+t > maxTokens {
			break
		}
		if n == 0 && t > maxTokens {
			return []model.Message{truncateMessageToTokens(m, maxTokens)}
		}
		total += t
		n = i + 1
	}
	if n == 0 && len(old) > 0 {
		// maxTokens <= 0: nothing fits whole, so the first
		// message is truncated to (effectively) nothing rather
		// than handed over whole.
		return []model.Message{truncateMessageToTokens(old[0], maxTokens)}
	}
	return old[:n]
}

// truncateMessageToTokens returns a copy of m whose estimated token
// cost fits budget. Content is truncated by runes; tool calls are
// dropped from the copy — their arguments are not summarised, and
// keeping them could blow the budget on their own. A message that
// already fits is returned unchanged.
func truncateMessageToTokens(m model.Message, budget int) model.Message {
	if messageTokens(m) <= budget {
		return m
	}
	out := m
	out.ToolCalls = nil
	// messageTokens charges 4 framing tokens; the rest of the
	// budget is content.
	contentBudget := budget - 4
	if contentBudget <= 0 {
		out.Content = ""
		return out
	}
	out.Content = truncateRunesToTokens(m.Content, contentBudget)
	return out
}

// truncateRunesToTokens returns the longest rune prefix of s whose
// estimated token count fits budget. CountTokens is not strictly
// monotonic in prefix length (a run's per-token cost changes as
// its distinct-rune count crosses 2→3), so the binary search is
// followed by a verify-and-shrink loop: the result always fits,
// and the shrink is short because the non-monotonicity is only a
// few tokens. Truncating by runes keeps the result valid UTF-8.
func truncateRunesToTokens(s string, budget int) string {
	r := []rune(s)
	if model.CountTokens(s) <= budget {
		return s
	}
	lo, hi := 0, len(r)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if model.CountTokens(string(r[:mid])) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	for lo > 0 && model.CountTokens(string(r[:lo])) > budget {
		lo--
	}
	return string(r[:lo])
}

// summaryPreservesSource reports whether summary preserves anything
// from source: at least minSharedContentTokens DISTINCT shared
// content tokens with the source turns, at least
// minSummaryDistinctTokens distinct content words of its own, a
// minimum information density, and not below a length floor
// relative to the input size. A source with no content tokens at
// all cannot verify any summary, so every summary of it is
// refused.
func summaryPreservesSource(summary string, source []model.Message) bool {
	var sb strings.Builder
	for _, m := range source {
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	src := sb.String()
	summary = strings.TrimSpace(summary)
	srcTokens := contentTokens(src)
	if len(srcTokens) == 0 {
		// Nothing in the source to preserve — and nothing to
		// verify a summary against. A summary of content-free
		// turns is refused rather than trusted.
		return false
	}
	summaryTokens := contentTokens(summary)
	// Distinctness (F-A6-P2-1): a summary of one word repeated
	// 200 times has exactly one distinct content token, however
	// long it is — it says one thing, not a summary.
	if len(summaryTokens) < minSummaryDistinctTokens {
		return false
	}
	if sharedTokenCount(summaryTokens, srcTokens) < minSharedContentTokens {
		return false
	}
	// Information density (F-A6-P2-1): distinct content words
	// must be at least minSummaryDensityPercent of all words, so
	// repetition padding collapses the ratio towards zero while
	// ordinary prose stays well above it.
	if words := len(strings.Fields(summary)); words > 0 && len(summaryTokens)*100 < minSummaryDensityPercent*words {
		return false
	}
	floor := len([]rune(src)) / summaryLengthDivisor
	if floor < minSummaryRunes {
		floor = minSummaryRunes
	}
	if floor > maxSummaryLengthFloor {
		floor = maxSummaryLengthFloor
	}
	return len([]rune(summary)) >= floor
}

// SummaryPreservesSource is the exported form of
// summaryPreservesSource, shared with the manual /compress path
// (internal/app compressWithOn, F-A7-P2-4): the same preservation
// bar auto-compaction applies must hold for a manual compress —
// a degenerate summary must never cost the history it claims to
// replace, whichever path produced it.
func SummaryPreservesSource(summary string, source []model.Message) bool {
	return summaryPreservesSource(summary, source)
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
		c.trippedAt = time.Now()
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
	// Bound the TOTAL provider-bound input (Audit 6 F-A6-P2-2):
	// a direct caller may pass an unbounded slice, so re-apply
	// the cap here — messages plus prompt overhead fit
	// maxCompactionInputTokens, with a giant message truncated
	// rather than sent whole. Redact (F-A6-P2-3): the prompt
	// must never carry a secret pasted into the transcript.
	old = boundedOldPrefix(old, maxCompactionInputTokens-summariserPromptOverheadTokens)
	var b strings.Builder
	// Name what must survive, explicitly: pending items, open
	// questions and user instructions/preferences used to depend
	// on the model volunteering them (Audit 5 F-A5-2). NOTE: the
	// same prompt is copied in internal/app/commands.go
	// (compressWithOn) — keep the two in sync.
	b.WriteString("Summarise the following conversation messages into a concise memory. Preserve facts, decisions, names, numbers, pending items and open questions, and anything the user asked to remember, including user instructions and preferences. Do not invent anything not in the messages.\n\n")
	for _, m := range old {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, security.RedactSecrets(m.Content))
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
	// Meter the summarisation call (F-A5-5): auto-compaction runs
	// outside the main loop, so without this its tokens never
	// reach TurnFinished or the budget. A failed call reports no
	// usage (resp is nil on error); a successful call's reported
	// usage is folded into this turn's usage by finishTurn.
	a.AddSideChannelUsage(resp.Usage)
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
	// Redact defensively (Audit 6 F-A6-P2-3): the fact store has
	// no scrub of its own, so a summary carrying a secret must be
	// cleaned here as well as at the call site. Redaction is
	// idempotent, so double application is harmless.
	summary = security.RedactSecrets(strings.TrimSpace(summary))
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
