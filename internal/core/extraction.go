// Background session-memory extraction.
//
// After enough activity (tool calls or turns), a compact summary of
// recent decisions and facts is extracted from the conversation and
// appended to session-memory.md — without interrupting the main flow
// (Claude Code's SessionMemory pattern, re-implemented in Go).
//
// The trigger lives on the Agent: every tool call and every finished
// turn counts toward the thresholds (defaults: 10 tool calls or 5
// turns, configurable with SetExtractionThresholds). When a threshold
// is reached, one background goroutine summarises the recent turns
// with the provider and appends the result via the session-memory
// appender wired with SetSessionMemoryAppender. Extraction is
// best-effort: failures are logged and never break the turn, it
// never runs concurrently with itself (single-flight), and it does
// nothing at all when no appender is wired (workstream A's API is
// not present yet — see recall_v2.go).
package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
)

// Default session-extraction thresholds.
const (
	defaultExtractEveryToolCalls = 10
	defaultExtractEveryTurns     = 5
)

// maxExtractionTurns caps how many recent transcript messages the
// extraction summary is built from.
const maxExtractionTurns = 10

// maxExtractionMessageRunes caps one transcript message's contribution
// to the extraction prompt, so a giant tool result cannot blow up
// the summarisation call.
const maxExtractionMessageRunes = 2000

// sessionExtractor summarises recent turns into a compact
// session-memory entry. The default implementation asks the agent's
// provider; tests inject a fake. It returns the entry text to append
// (already trimmed; empty means "nothing worth saving").
type sessionExtractorFunc func(ctx context.Context, turns []model.Message) (string, error)

// SetExtractionThresholds configures after how many tool calls or
// finished turns a session-memory extraction is triggered. Either
// value <= 0 restores its default. A threshold of 0 after this call
// is impossible by construction (defaults are applied), so pass a
// large number to effectively disable one leg.
func (a *Agent) SetExtractionThresholds(toolCalls, turns int) {
	a.extractMu.Lock()
	defer a.extractMu.Unlock()
	if toolCalls > 0 {
		a.extractThresholdTools = toolCalls
	} else {
		a.extractThresholdTools = defaultExtractEveryToolCalls
	}
	if turns > 0 {
		a.extractThresholdTurns = turns
	} else {
		a.extractThresholdTurns = defaultExtractEveryTurns
	}
}

// SetSessionExtractor replaces the summariser (default: the
// provider-based one). Tests inject a fake here; passing nil
// restores the default.
func (a *Agent) SetSessionExtractor(fn sessionExtractorFunc) {
	a.extractMu.Lock()
	defer a.extractMu.Unlock()
	a.sessionExtractor = fn
}

// extractionThresholds returns the configured thresholds, applying
// defaults for a zero-valued Agent (e.g. one built before these
// fields existed).
func (a *Agent) extractionThresholds() (toolCalls, turns int) {
	a.extractMu.Lock()
	defer a.extractMu.Unlock()
	toolCalls, turns = a.extractThresholdTools, a.extractThresholdTurns
	if toolCalls <= 0 {
		toolCalls = defaultExtractEveryToolCalls
	}
	if turns <= 0 {
		turns = defaultExtractEveryTurns
	}
	return toolCalls, turns
}

// noteTurnCompleted counts one finished turn toward the extraction
// threshold and, when a threshold is reached, launches a background
// extraction over a snapshot of the transcript. It is called via
// defer from Agent.run, so it runs on every return path of a real
// turn; the extraction itself is a goroutine and never blocks the
// turn. The transcript slice is copied (last maxExtractionTurns
// messages) so later runs cannot mutate the snapshot.
func (a *Agent) noteTurnCompleted(ctx context.Context, transcript []model.Message) {
	a.extractMu.Lock()
	a.turnsSinceExtract++
	toolThreshold, turnThreshold := a.extractThresholdTools, a.extractThresholdTurns
	if toolThreshold <= 0 {
		toolThreshold = defaultExtractEveryToolCalls
	}
	if turnThreshold <= 0 {
		turnThreshold = defaultExtractEveryTurns
	}
	reached := a.toolCallsSinceExtract >= toolThreshold || a.turnsSinceExtract >= turnThreshold
	if !reached {
		a.extractMu.Unlock()
		return
	}
	// Reset before launching: activity during the extraction counts
	// toward the next one, and a second trigger cannot double-fire
	// on the same counted activity.
	a.toolCallsSinceExtract = 0
	a.turnsSinceExtract = 0
	a.extractMu.Unlock()

	snapshot := append([]model.Message(nil), transcript...)
	if len(snapshot) > maxExtractionTurns {
		snapshot = snapshot[len(snapshot)-maxExtractionTurns:]
	}
	a.maybeExtractSessionMemory(ctx, snapshot)
}

// maybeExtractSessionMemory launches one background extraction
// unless one is already running (single-flight) or no appender is
// wired. It never blocks and never returns an error.
func (a *Agent) maybeExtractSessionMemory(ctx context.Context, turns []model.Message) {
	if a.sessionAppender() == nil {
		a.logf("session extraction skipped: no session-memory appender wired")
		return
	}
	a.extractMu.Lock()
	if a.extractRunning {
		a.extractMu.Unlock()
		a.logf("session extraction skipped: one already running")
		return
	}
	a.extractRunning = true
	a.extractMu.Unlock()

	// The run's ctx may be cancelled as soon as the turn returns,
	// so the background work gets its own timeout-bounded ctx.
	bgCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	go func() {
		defer cancel()
		defer func() {
			a.extractMu.Lock()
			a.extractRunning = false
			a.extractMu.Unlock()
		}()
		a.runSessionExtraction(bgCtx, turns)
	}()
}

// runSessionExtraction summarises turns and appends the result to
// session memory. Every failure path logs and returns; nothing here
// can fail a turn.
func (a *Agent) runSessionExtraction(ctx context.Context, turns []model.Message) {
	extractor := a.sessionExtractorFor()
	summary, err := extractor(ctx, turns)
	if err != nil {
		a.logf("session extraction failed: %v", err)
		return
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return
	}
	// What-not-to-save gate (Audit 4 A P1-3): the extractor is the
	// model, so its output is model-derived text. Derivable content
	// (code patterns, file structure, git history) is dropped
	// sentence by sentence; if nothing worth keeping remains,
	// nothing is appended. The gate never rewrites the model's
	// words — it only keeps or drops them.
	if kept, ok := memory.FilterExplicitSave(summary); ok {
		summary = kept
	} else {
		a.logf("session extraction dropped: gate found nothing worth saving")
		return
	}
	if err := a.sessionAppender().Append(summary); err != nil {
		a.logf("session extraction append failed: %v", err)
		return
	}
	a.logf("session extraction appended %d bytes to session memory", len(summary))
}

// sessionExtractorFor returns the configured summariser, or the
// default provider-based one.
func (a *Agent) sessionExtractorFor() sessionExtractorFunc {
	a.extractMu.Lock()
	defer a.extractMu.Unlock()
	if a.sessionExtractor != nil {
		return a.sessionExtractor
	}
	return a.defaultSessionExtractor
}

// defaultSessionExtractor asks the agent's provider for a compact
// summary of decisions and facts from the recent turns. An empty
// summary (or "nothing worth saving") is returned as "" so nothing
// is appended.
func (a *Agent) defaultSessionExtractor(ctx context.Context, turns []model.Message) (string, error) {
	if a.provider == nil {
		return "", fmt.Errorf("no provider available for session extraction")
	}
	temp := 0.0
	resp, err := a.provider.Chat(ctx, model.ChatRequest{
		Messages: []model.Message{
			{Role: "system", Content: "You extract session memory: durable decisions, facts and preferences from a conversation transcript. Reply with a compact markdown bullet list, one fact or decision per line. If nothing durable was decided or stated, reply with nothing."},
			{Role: "user", Content: extractionPrompt(turns)},
		},
		MaxTokens:   400,
		Temperature: &temp,
	})
	if err != nil {
		return "", fmt.Errorf("session extraction provider call: %w", err)
	}
	if resp == nil {
		return "", fmt.Errorf("session extraction provider returned nil response")
	}
	return strings.TrimSpace(resp.Message.Content), nil
}

// extractionPrompt renders recent turns for the summariser: role
// labels plus truncated content, tool calls named rather than
// inlined (their results already appear as tool messages).
func extractionPrompt(turns []model.Message) string {
	var b strings.Builder
	b.WriteString("Recent conversation (most recent last). Summarise only durable decisions, facts and preferences — not chit-chat, not tool output verbatim:\n\n")
	for _, m := range turns {
		content := strings.TrimSpace(m.Content)
		if r := []rune(content); len(r) > maxExtractionMessageRunes {
			content = string(r[:maxExtractionMessageRunes]) + " […truncated]"
		}
		switch m.Role {
		case "tool":
			fmt.Fprintf(&b, "[tool result for %s]: %s\n", m.ToolCallID, content)
		case "assistant":
			if len(m.ToolCalls) > 0 {
				names := make([]string, len(m.ToolCalls))
				for i, tc := range m.ToolCalls {
					names[i] = tc.Name
				}
				fmt.Fprintf(&b, "assistant (called %s): %s\n", strings.Join(names, ", "), content)
			} else {
				fmt.Fprintf(&b, "assistant: %s\n", content)
			}
		default:
			fmt.Fprintf(&b, "%s: %s\n", m.Role, content)
		}
	}
	return b.String()
}
