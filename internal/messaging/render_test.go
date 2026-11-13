package messaging

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/irfan/orion/internal/core"
)

func mustJSONArgs(t *testing.T, pairs ...string) string {
	t.Helper()
	if len(pairs)%2 != 0 {
		t.Fatalf("mustJSONArgs: odd number of args")
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i < len(pairs); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q:%q", pairs[i], pairs[i+1])
	}
	b.WriteByte('}')
	return b.String()
}

func started(tool, args string) core.ProgressEvent {
	return core.ProgressEvent{Kind: core.EventToolCallStarted, ToolName: tool, Arguments: args}
}

func finished(tool, args string, err error) core.ProgressEvent {
	return core.ProgressEvent{Kind: core.EventToolCallFinished, ToolName: tool, Arguments: args, Err: err}
}

// TestTerminalCardEmptyRendersHeader verifies a card with no entries
// is just the header.
func TestTerminalCardEmptyRendersHeader(t *testing.T) {
	var c TerminalCard
	if got := c.Render(); got != cardHeader {
		t.Fatalf("empty card = %q, want %q", got, cardHeader)
	}
}

// TestTerminalCardShellEntry verifies the Shell block shape.
func TestTerminalCardShellEntry(t *testing.T) {
	var c TerminalCard
	c.Add(started("shell_exec", mustJSONArgs(t, "command", "echo hello")))
	want := cardHeader + "\n<blockquote><b>Shell</b>  &lt;/&gt;\n<code>echo hello</code></blockquote>"
	if got := c.Render(); got != want {
		t.Fatalf("card = %q, want %q", got, want)
	}
}

// TestTerminalCardEscaping verifies &, <, > are escaped and Bengali
// and emoji survive unbroken.
func TestTerminalCardEscaping(t *testing.T) {
	var c TerminalCard
	cmd := `echo '<script>alert("x")</script>' & কেমন আছো? 🎉`
	c.Add(started("shell_exec", mustJSONArgs(t, "command", cmd)))
	got := c.Render()
	if strings.Contains(got, "<script>") {
		t.Fatalf("card contains raw <script>: %q", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Fatalf("card = %q, want escaped &lt;script&gt;", got)
	}
	if !strings.Contains(got, "&amp;") {
		t.Fatalf("card = %q, want escaped &amp;", got)
	}
	if !strings.Contains(got, "কেমন আছো? 🎉") {
		t.Fatalf("card = %q, want Bengali + emoji preserved", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("card is not valid UTF-8: %q", got)
	}
}

// TestTerminalCardCommandTruncation verifies a long command is cut to
// 60 runes plus "…".
func TestTerminalCardCommandTruncation(t *testing.T) {
	var c TerminalCard
	long := strings.Repeat("x", 100)
	c.Add(started("shell_exec", mustJSONArgs(t, "command", long)))
	got := c.Render()
	want := strings.Repeat("x", 60) + "…"
	if !strings.Contains(got, "<code>"+want+"</code>") {
		t.Fatalf("card = %q, want command truncated to 60 runes + …", got)
	}
	// Multi-byte truncation must not split a rune: 100 Bengali chars.
	var c2 TerminalCard
	bn := strings.Repeat("অ", 100)
	c2.Add(started("shell_exec", mustJSONArgs(t, "command", bn)))
	got2 := c2.Render()
	if !strings.Contains(got2, strings.Repeat("অ", 60)+"…") {
		t.Fatalf("card = %q, want 60 Bengali runes + …", got2)
	}
	if !utf8.ValidString(got2) {
		t.Fatalf("card is not valid UTF-8: %q", got2)
	}
}

// TestTerminalCardFirstLineOnly verifies only the first line of a
// multi-line command is shown.
func TestTerminalCardFirstLineOnly(t *testing.T) {
	var c TerminalCard
	c.Add(started("shell_exec", mustJSONArgs(t, "command", "echo one\necho two")))
	if got := c.Render(); strings.Contains(got, "echo two") || !strings.Contains(got, "echo one") {
		t.Fatalf("card = %q, want only the first line", got)
	}
}

// TestTerminalCardFileWrite verifies file-write (and edit) lines.
func TestTerminalCardFileWrite(t *testing.T) {
	var c TerminalCard
	c.Add(started("file_write", mustJSONArgs(t, "path", "/home/irfan/bin/pulse.sh", "content", "x")))
	if got := c.Render(); !strings.Contains(got, "✍️ Writing /home/irfan/bin/pulse.sh") {
		t.Fatalf("card = %q, want file-write line", got)
	}
	var c2 TerminalCard
	c2.Add(started("file_edit", mustJSONArgs(t, "path", "/home/irfan/bin/pulse.sh")))
	if got := c2.Render(); !strings.Contains(got, "✍️ Editing /home/irfan/bin/pulse.sh") {
		t.Fatalf("card = %q, want file-edit line", got)
	}
}

// TestTerminalCardOtherTools verifies calculator, web, skill and
// generic renderings.
func TestTerminalCardOtherTools(t *testing.T) {
	var c TerminalCard
	c.Add(started("calculator", mustJSONArgs(t, "expression", "17*23+5")))
	c.Add(started("web_search", mustJSONArgs(t, "query", "singapore weather")))
	c.Add(started("web_fetch", mustJSONArgs(t, "url", "https://example.com/page")))
	c.Add(started("skill_writer", mustJSONArgs(t, "name", "n8n-automation")))
	c.Add(started("datetime", mustJSONArgs(t, "timezone", "Asia/Singapore")))
	got := c.Render()
	for _, want := range []string{
		"🧮 17*23+5",
		"🌐 singapore weather",
		"🌐 https://example.com/page",
		"💾",
		"🔧 datetime",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("card = %q, want it to contain %q", got, want)
		}
	}
}

// TestTerminalCardDedupe verifies identical consecutive entries
// collapse with "(×N)".
func TestTerminalCardDedupe(t *testing.T) {
	var c TerminalCard
	ev := started("shell_exec", mustJSONArgs(t, "command", "echo hi"))
	c.Add(ev)
	c.Add(ev)
	if got := c.Render(); !strings.Contains(got, "(×2)") {
		t.Fatalf("card = %q, want (×2)", got)
	}
	c.Add(ev)
	if got := c.Render(); !strings.Contains(got, "(×3)") || strings.Contains(got, "(×2)") {
		t.Fatalf("card = %q, want (×3)", got)
	}
	// A different entry in between breaks the run.
	var c2 TerminalCard
	c2.Add(ev)
	c2.Add(started("calculator", mustJSONArgs(t, "expression", "1+1")))
	c2.Add(ev)
	if got := c2.Render(); strings.Contains(got, "(×2)") {
		t.Fatalf("card = %q, non-consecutive duplicates must not collapse", got)
	}
}

// TestTerminalCardFailedCall verifies a failed tool call gets the
// " — failed" suffix and a successful one gets no marker.
func TestTerminalCardFailedCall(t *testing.T) {
	var c TerminalCard
	args := mustJSONArgs(t, "command", "false")
	c.Add(started("shell_exec", args))
	c.Add(finished("shell_exec", args, fmt.Errorf("exit status 1")))
	got := c.Render()
	if !strings.Contains(got, " — failed") {
		t.Fatalf("card = %q, want failed marker", got)
	}
	var c2 TerminalCard
	c2.Add(started("shell_exec", args))
	c2.Add(finished("shell_exec", args, nil))
	if got := c2.Render(); strings.Contains(got, "failed") || strings.Contains(got, "✓") {
		t.Fatalf("card = %q, successful call must have no marker", got)
	}
	// A failed call must not merge with an identical successful one.
	var c3 TerminalCard
	c3.Add(started("shell_exec", args))
	c3.Add(finished("shell_exec", args, nil))
	c3.Add(started("shell_exec", args))
	c3.Add(finished("shell_exec", args, fmt.Errorf("boom")))
	if got := c3.Render(); strings.Contains(got, "(×2)") {
		t.Fatalf("card = %q, success + failure must not collapse", got)
	}
}

// TestTerminalCardBudget verifies a card with 50 shell entries fits
// the 4000-rune budget and keeps the newest entries.
func TestTerminalCardBudget(t *testing.T) {
	var c TerminalCard
	for i := 0; i < 50; i++ {
		c.Add(started("shell_exec", mustJSONArgs(t, "command", fmt.Sprintf("echo entry-%02d %s", i, strings.Repeat("y", 80)))))
	}
	got := c.Render()
	if n := len([]rune(got)); n > 4000 {
		t.Fatalf("card = %d runes, want <= 4000", n)
	}
	if !strings.Contains(got, "entry-49") {
		t.Fatalf("card = %q, want the newest entry kept", got)
	}
	if strings.Contains(got, "entry-00") {
		t.Fatalf("card = %q, oldest entries must be trimmed first", got)
	}
	if !strings.HasPrefix(got, cardHeader) {
		t.Fatalf("card = %q, want the header kept", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("card is not valid UTF-8")
	}
}

// TestTerminalCardSingleHugeEntry verifies one entry that alone
// exceeds the budget is truncated to fit, header intact.
func TestTerminalCardSingleHugeEntry(t *testing.T) {
	var c TerminalCard
	c.Add(started("shell_exec", mustJSONArgs(t, "command", strings.Repeat("z", 100000))))
	got := c.Render()
	if n := len([]rune(got)); n > 4000 {
		t.Fatalf("card = %d runes, want <= 4000", n)
	}
	if !strings.HasPrefix(got, cardHeader) {
		t.Fatalf("card = %q, want the header kept", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("card is not valid UTF-8")
	}
}

// TestTerminalCardIgnoresNonToolEvents verifies step/turn events add
// no entries.
func TestTerminalCardIgnoresNonToolEvents(t *testing.T) {
	var c TerminalCard
	c.Add(core.ProgressEvent{Kind: core.EventStepStarted, Step: 1})
	c.Add(core.ProgressEvent{Kind: core.EventTurnFinished, Step: 2})
	if got := c.Render(); got != cardHeader {
		t.Fatalf("card = %q, want just the header", got)
	}
}

// TestFormatFooter verifies footer rendering.
func TestFormatFooter(t *testing.T) {
	if got := FormatFooter(nil); got != "" {
		t.Fatalf("FormatFooter(nil) = %q, want empty", got)
	}
	stats := &core.TurnStats{Provider: "deepseek", Model: "deepseek-chat", Tokens: 1240, Steps: 3, Duration: 4200 * time.Millisecond}
	if got, want := FormatFooter(stats), "— deepseek-chat · 1,240 tokens · 3 steps · 4.2s"; got != want {
		t.Fatalf("FormatFooter = %q, want %q", got, want)
	}
	// Model empty: fall back to the provider name. Sub-second:
	// milliseconds, no decimal.
	stats2 := &core.TurnStats{Provider: "ollama", Tokens: 42, Steps: 1, Duration: 850 * time.Millisecond}
	if got, want := FormatFooter(stats2), "— ollama · 42 tokens · 1 steps · 850ms"; got != want {
		t.Fatalf("FormatFooter = %q, want %q", got, want)
	}
	// Thousands separators on a big token count.
	stats3 := &core.TurnStats{Provider: "p", Model: "m", Tokens: 1234567, Steps: 1, Duration: time.Second}
	if got := FormatFooter(stats3); !strings.Contains(got, "1,234,567 tokens") {
		t.Fatalf("FormatFooter = %q, want 1,234,567 tokens", got)
	}
}
