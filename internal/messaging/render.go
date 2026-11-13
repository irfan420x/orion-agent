// Package messaging — the Hermes-style Telegram "terminal" card.
//
// One card message per agent turn. Each tool call is an entry; the
// gateway creates the card lazily on the first ToolCallStarted event
// and edits it in place as the turn progresses. This file is pure
// rendering: no network, no token.
//
// Safety: every model/tool-derived string is passed through
// RedactDisplayText (secrets + credential-file paths) and then HTML
// escaped before it is wrapped in tags, so a tool argument can never
// break out of the card markup or leak a secret into the chat.
package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/irfan/orion/internal/core"
)

// TurnFunc runs one user turn. emit receives live core.ProgressEvents;
// the gateway renders them into the terminal card. Returning stats
// != nil appends the runtime footer to the final reply; nil means no
// footer.
type TurnFunc func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (reply string, stats *core.TurnStats, err error)

// FormatFooter renders the one-line runtime footer appended to a final
// reply, e.g. "— deepseek-chat · 1,240 tokens · 3 steps · 4.2s". It
// returns "" when stats is nil. The model name is used when set, else
// the provider name. Durations under 1s render as milliseconds
// ("850ms"); otherwise seconds with one decimal ("4.2s"). When typed
// memories were used this turn (stats.MemoriesUsed, workstream B),
// " · <N> memories" is appended ("1 memory" for one); it is omitted
// when no memories were used, so footers with zero memories render
// exactly as before. When subagent results were consumed this turn
// (stats.SubagentTokens), the tokens segment breaks them out —
// "1,240 tokens (incl. 300 subagent)" — so the footer number
// honestly includes delegated work instead of silently folding it
// into the parent's total; it is omitted when zero, so footers with
// no subagent work render exactly as before.
func FormatFooter(stats *core.TurnStats) string {
	if stats == nil {
		return ""
	}
	name := stats.Model
	if name == "" {
		name = stats.Provider
	}
	tokens := formatThousands(stats.Tokens) + " tokens"
	if stats.SubagentTokens > 0 {
		tokens += fmt.Sprintf(" (incl. %s subagent)", formatThousands(stats.SubagentTokens))
	}
	footer := fmt.Sprintf("— %s · %s · %d steps · %s",
		name, tokens, stats.Steps, formatFooterDuration(stats.Duration))
	switch {
	case stats.MemoriesUsed <= 0:
		return footer
	case stats.MemoriesUsed == 1:
		return footer + " · 1 memory"
	default:
		return fmt.Sprintf("%s · %d memories", footer, stats.MemoriesUsed)
	}
}

// formatThousands renders n with "," thousands separators ("1240" ->
// "1,240").
func formatThousands(n int) string {
	if n < 0 {
		return "-" + formatThousands(-n)
	}
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	rem := len(s) % 3
	if rem > 0 {
		b.WriteString(s[:rem])
	}
	for i := rem; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// formatFooterDuration renders d as "850ms" under 1s, else "4.2s".
func formatFooterDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// maxCardRunes is the Telegram card budget: Telegram caps a message at
// 4096 runes, and 4000 leaves margin for markup and the footer path.
const maxCardRunes = 4000

// maxCommandRunes caps a displayed shell command; maxPathRunes caps a
// displayed path; maxSummaryRunes caps a generic argument summary.
const (
	maxCommandRunes = 60
	maxPathRunes    = 80
	maxSummaryRunes = 60
)

// cardHeader is the first line of every card.
const cardHeader = "💻 <b>terminal</b>"

// cardEntry is one tool call in a turn.
type cardEntry struct {
	toolName string
	// display is the redacted plain-text summary (command, path,
	// expression, ...) before HTML escaping and per-kind truncation.
	display string
	// finished reports whether the ToolCallFinished event arrived;
	// failed reports whether it carried an error.
	finished bool
	failed   bool
	// rawArgs is the raw JSON arguments string, used to match a
	// ToolCallFinished event to the entry its ToolCallStarted event
	// created.
	rawArgs string
}

// TerminalCard accumulates the entries for one turn and renders the
// terminal card HTML.
type TerminalCard struct {
	entries []cardEntry
}

// Add consumes one ProgressEvent. Only ToolCallStarted events create
// entries; ToolCallFinished updates the matching entry's status — a
// finished entry keeps its text, a failed one gets a " — failed"
// suffix at render time. (Hermes shows no checkmarks, so successful
// entries get no marker at all.) Compaction events create entries
// too, so the warning band and the auto-compaction itself reach the
// user on the card instead of only in the daemon log: a warning
// (EventCompactionWarning) says compaction is approaching, or — when
// it carries an error — that the circuit breaker tripped and
// /compress is the remedy; EventAutoCompacted says older turns were
// summarised before being dropped. Other event kinds are ignored.
func (c *TerminalCard) Add(ev core.ProgressEvent) {
	switch ev.Kind {
	case core.EventCompactionWarning:
		// The warning carries an error only when the circuit
		// breaker tripped: that entry renders failed, so the
		// tripped state is unmissable on the card.
		display := "history nearly full — auto-compaction approaching"
		failed := false
		if ev.Err != nil {
			display = RedactDisplayText(ev.Err.Error())
			failed = true
		}
		c.entries = append(c.entries, cardEntry{
			toolName: "compaction",
			display:  display,
			finished: true,
			failed:   failed,
		})
	case core.EventAutoCompacted:
		c.entries = append(c.entries, cardEntry{
			toolName: "compaction",
			display:  "older turns summarised before being dropped from working history",
			finished: true,
		})
	case core.EventToolCallStarted:
		c.entries = append(c.entries, cardEntry{
			toolName: ev.ToolName,
			display:  entryDisplay(ev.ToolName, ev.Arguments),
			rawArgs:  ev.Arguments,
		})
	case core.EventToolCallFinished:
		// Match the most recent unfinished entry for this tool call.
		for i := len(c.entries) - 1; i >= 0; i-- {
			e := &c.entries[i]
			if !e.finished && e.toolName == ev.ToolName && e.rawArgs == ev.Arguments {
				e.finished = true
				e.failed = ev.Err != nil
				return
			}
		}
		// No matching started entry (e.g. the handler was attached
		// mid-turn): record the call so the failure is still visible.
		if ev.Err != nil {
			c.entries = append(c.entries, cardEntry{
				toolName: ev.ToolName,
				display:  entryDisplay(ev.ToolName, ev.Arguments),
				rawArgs:  ev.Arguments,
				finished: true,
				failed:   true,
			})
		}
	}
}

// entryDisplay extracts the human-readable summary for a tool call
// from its JSON arguments and redacts it for display.
func entryDisplay(toolName, argsJSON string) string {
	args := parseArgs(argsJSON)
	switch {
	case isShellTool(toolName):
		cmd := argString(args, "command")
		// Only the first line: a multi-line command is one block.
		if i := strings.IndexByte(cmd, '\n'); i >= 0 {
			cmd = cmd[:i]
		}
		return RedactDisplayText(cmd)
	case toolName == "file_write" || toolName == "file_edit":
		return RedactDisplayText(argString(args, "path"))
	case toolName == "calculator":
		return RedactDisplayText(argString(args, "expression"))
	case toolName == "web_search":
		return RedactDisplayText(argString(args, "query"))
	case toolName == "web_fetch":
		return RedactDisplayText(argString(args, "url"))
	default:
		return RedactDisplayText(argSummary(argsJSON))
	}
}

// isShellTool reports whether the tool is a shell tool (shell_exec and
// any other *shell* tool).
func isShellTool(toolName string) bool {
	return strings.Contains(toolName, "shell")
}

// parseArgs decodes a JSON-object arguments string into a string map.
// Non-string values are rendered as JSON. A malformed or non-object
// arguments string yields an empty map.
func parseArgs(argsJSON string) map[string]string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argsJSON), &raw); err != nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			out[k] = s
			continue
		}
		out[k] = string(v)
	}
	return out
}

// argString returns args[key], or "" when absent.
func argString(args map[string]string, key string) string {
	return args[key]
}

// argSummary collapses the raw JSON arguments to a single line for the
// generic "🔧 tool ..." rendering.
func argSummary(argsJSON string) string {
	fields := strings.Fields(argsJSON)
	return strings.Join(fields, " ")
}

// truncateRunes shortens s to at most n runes, appending "…" when it
// truncates. Rune-based, so multi-byte text is never split mid-rune.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// escapeHTML escapes &, <, > in model/tool-derived text before it is
// wrapped in Telegram HTML tags.
func escapeHTML(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// renderEntry renders one entry, truncating its display text to at
// most maxDisplay runes when maxDisplay >= 0 (no limit when negative).
// The returned string is a complete HTML fragment (or plain line).
func renderEntry(e cardEntry, maxDisplay int) string {
	trunc := func(s string, n int) string {
		if maxDisplay >= 0 && maxDisplay < n {
			n = maxDisplay
		}
		return truncateRunes(s, n)
	}
	switch {
	case isShellTool(e.toolName):
		cmd := trunc(e.display, maxCommandRunes)
		return "<blockquote><b>Shell</b>  &lt;/&gt;\n<code>" + escapeHTML(cmd) + "</code></blockquote>"
	case e.toolName == "file_write":
		return "✍️ Writing " + escapeHTML(trunc(e.display, maxPathRunes))
	case e.toolName == "file_edit":
		return "✍️ Editing " + escapeHTML(trunc(e.display, maxPathRunes))
	case e.toolName == "compaction":
		// Compaction entries carry the remedy ("run /compress")
		// at the END of the message, so they get a wider cap
		// than tool summaries: truncating at 60 runes would cut
		// the remedy off a tripped-breaker entry. The card's
		// 4000-rune budget still caps the total.
		return "🗜 " + escapeHTML(trunc(e.display, 200))
	case e.toolName == "calculator":
		return "🧮 " + escapeHTML(trunc(e.display, maxSummaryRunes))
	case e.toolName == "web_search" || e.toolName == "web_fetch":
		return "🌐 " + escapeHTML(trunc(e.display, maxPathRunes))
	case strings.HasPrefix(e.toolName, "skill"):
		return "💾 " + escapeHTML(trunc(e.display, maxSummaryRunes))
	default:
		summary := trunc(e.display, maxSummaryRunes)
		if summary == "" {
			return "🔧 " + escapeHTML(e.toolName)
		}
		return "🔧 " + escapeHTML(e.toolName) + " " + escapeHTML(summary)
	}
}

// cardGroup is a run of identical consecutive entries, collapsed into
// one rendered block with a "(×N)" line, exactly like Hermes.
type cardGroup struct {
	entry cardEntry
	count int
}

// groupEntries collapses identical consecutive entries. Two entries
// are identical when they render the same and share the same failure
// state; a failed call is never merged with a successful one.
func (c *TerminalCard) groupEntries() []cardGroup {
	var groups []cardGroup
	for _, e := range c.entries {
		if n := len(groups); n > 0 {
			last := &groups[n-1]
			if last.entry.toolName == e.toolName && last.entry.display == e.display && last.entry.failed == e.failed {
				last.count++
				continue
			}
		}
		groups = append(groups, cardGroup{entry: e, count: 1})
	}
	return groups
}

// renderGroup renders one group: the entry, a " — failed" suffix when
// the call failed, and a "(×N)" line when N > 1.
func renderGroup(g cardGroup, maxDisplay int) string {
	s := renderEntry(g.entry, maxDisplay)
	if g.entry.failed {
		s += " — failed"
	}
	if g.count > 1 {
		s += fmt.Sprintf("\n(×%d)", g.count)
	}
	return s
}

// IsEmpty reports whether the card has no entries yet.
func (c *TerminalCard) IsEmpty() bool { return len(c.entries) == 0 }

// Render returns the full card HTML. An empty card renders just the
// header. Output is capped at 4000 runes (Telegram's limit is 4096):
// the oldest entries are dropped first so the newest entries survive,
// and a single entry that alone exceeds the budget gets its display
// text truncated further. Work is rune-based throughout, so multi-byte
// text is never split.
func (c *TerminalCard) Render() string {
	groups := c.groupEntries()
	renderAll := func(gs []cardGroup, maxDisplay int) string {
		parts := make([]string, 0, len(gs)+1)
		parts = append(parts, cardHeader)
		for _, g := range gs {
			parts = append(parts, renderGroup(g, maxDisplay))
		}
		return strings.Join(parts, "\n")
	}
	out := renderAll(groups, -1)
	// Drop the oldest groups until the card fits.
	for len([]rune(out)) > maxCardRunes && len(groups) > 1 {
		groups = groups[1:]
		out = renderAll(groups, -1)
	}
	// One group left and still over budget: shrink its display text.
	if len([]rune(out)) > maxCardRunes && len(groups) == 1 {
		// Binary-search the largest display length that fits.
		lo, hi := 0, len([]rune(groups[0].entry.display))
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if len([]rune(renderAll(groups, mid))) <= maxCardRunes {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		out = renderAll(groups, lo)
	}
	return out
}
