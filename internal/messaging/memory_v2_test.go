// Memory v2 (workstream C) tests: /memory UX (typed memories, ages,
// forget by name/number), /compress drawing on session memory, and
// the Telegram footer memory-usage segment.
//
// The stub envs here stand in for the wiring the coordinator still
// has to do in cmd/oriond/buildCommandEnv (TypedMemories ->
// memory.TypedStore.List, ForgetTypedMemory -> TypedStore.Delete,
// SessionMemory -> memory.SessionLog.Read, CompressWithSessionMemory
// -> the app-side variant): the fakes implement the exact
// contracted names, and the coordinator wires the real ones.
package messaging

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
)

// stubMemoryEnv returns a CommandEnv whose memory/compress fields
// record which function was called, so tests can assert the routing
// (fact number vs typed-memory name, plain vs session-memory
// compress) instead of only the reply text.
type stubMemoryEnv struct {
	env *CommandEnv

	memoryInfo string
	typed      []TypedMemory
	typedErr   error

	forgetFactCalled   int
	forgetFactIndex    int
	forgetTypedCalled  string
	forgetTypedDeleted string
	forgetTypedErr     error

	sessionMemory     string
	sessionMemoryErr  error
	compressCalled    bool
	compressVariant   bool
	compressBefore    int
	compressAfter     int
	compressSummary   int
	compressSessionIn string
}

func newStubMemoryEnv() *stubMemoryEnv {
	s := &stubMemoryEnv{
		memoryInfo: "Memory: 1 fact(s), 2 message(s)\nData dir: /tmp/orion\nFacts:\n  1. k: v",
	}
	s.env = &CommandEnv{
		ChatID: 7,
		Session: func(chatID int64) *Session {
			return &Session{ChatID: chatID, FooterEnabled: true}
		},
		SetFooter: func(chatID int64, on bool) error { return nil },
		MemoryInfo: func() (string, error) {
			return s.memoryInfo, nil
		},
		ForgetFact: func(index int) (string, error) {
			s.forgetFactCalled++
			s.forgetFactIndex = index
			return "k", nil
		},
		TypedMemories: func() ([]TypedMemory, error) {
			return s.typed, s.typedErr
		},
		ForgetTypedMemory: func(name string) (string, error) {
			s.forgetTypedCalled = name
			if s.forgetTypedErr != nil {
				return "", s.forgetTypedErr
			}
			if s.forgetTypedDeleted != "" {
				return s.forgetTypedDeleted, nil
			}
			return name, nil
		},
		SessionMemory: func(ctx context.Context) (string, error) {
			return s.sessionMemory, s.sessionMemoryErr
		},
		Compress: func(ctx context.Context, chatID int64, keepRecent int) (int, int, int, error) {
			s.compressCalled = true
			return s.compressBefore, s.compressAfter, s.compressSummary, nil
		},
		CompressWithSessionMemory: func(ctx context.Context, chatID int64, keepRecent int, sessionMemory string) (int, int, int, error) {
			s.compressVariant = true
			s.compressSessionIn = sessionMemory
			return s.compressBefore, s.compressAfter, s.compressSummary, nil
		},
	}
	return s
}

func dispatch(t *testing.T, s *stubMemoryEnv, text string) string {
	t.Helper()
	r := DefaultRegistry(s.env)
	reply, handled, err := r.Dispatch(context.Background(), s.env, text)
	if err != nil {
		t.Fatalf("Dispatch(%q) error: %v", text, err)
	}
	if !handled {
		t.Fatalf("Dispatch(%q) not handled", text)
	}
	return reply
}

// TestFormatAgeAt covers the age formatter's units and the
// future-timestamp case (never a negative age).
func TestFormatAgeAt(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		at   time.Time
		want string
	}{
		{now, "just now"},
		{now.Add(-30 * time.Second), "just now"},
		{now.Add(-90 * time.Second), "1m ago"},
		{now.Add(-45 * time.Minute), "45m ago"},
		{now.Add(-2 * time.Hour), "2h ago"},
		{now.Add(-25 * time.Hour), "1d ago"},
		{now.Add(-3 * 24 * time.Hour), "3d ago"},
		{now.Add(-45 * 24 * time.Hour), "1mo ago"},
		{now.Add(-400 * 24 * time.Hour), "1y ago"},
		// Future (clock skew): "just now", never negative.
		{now.Add(time.Hour), "just now"},
		{now.Add(24 * time.Hour), "just now"},
	}
	for _, c := range cases {
		if got := formatAgeAt(c.at, now); got != c.want {
			t.Errorf("formatAgeAt(%v) = %q, want %q", c.at, got, c.want)
		}
	}
}

// TestRenderTypedMemoriesGrouping verifies group order (user,
// feedback, project, reference, then unknown types sorted), newest
// first within a group, ages, and descriptions.
func TestRenderTypedMemoriesGrouping(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	mems := []TypedMemory{
		{Name: "old-proj", Description: "older project note", Type: memory.MemoryType("project"), UpdatedAt: now.Add(-5 * 24 * time.Hour)},
		{Name: "weird", Description: "unknown type", Type: memory.MemoryType("custom"), UpdatedAt: now.Add(-time.Hour)},
		{Name: "prefs", Description: "prefers concise replies", Type: memory.MemoryType("user"), UpdatedAt: now.Add(-3 * 24 * time.Hour)},
		{Name: "fix-style", Description: "correction on style", Type: memory.MemoryType("feedback"), UpdatedAt: now.Add(-2 * time.Hour)},
		{Name: "new-proj", Description: "", Type: memory.MemoryType("project"), UpdatedAt: now.Add(-30 * time.Minute)},
		{Name: "board", Description: "Grafana board link", Type: memory.MemoryType("reference"), UpdatedAt: now.Add(-24 * time.Hour)},
	}
	got := renderTypedMemories(mems, now)
	for _, want := range []string{
		"Typed memories:",
		"  user (1):",
		"    • prefs (3d ago) — prefers concise replies",
		"  feedback (1):",
		"    • fix-style (2h ago) — correction on style",
		"  project (2):",
		"    • new-proj (30m ago)",
		"    • old-proj (5d ago) — older project note",
		"  reference (1):",
		"    • board (1d ago) — Grafana board link",
		"  custom (1):",
		"    • weird (1h ago) — unknown type",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered listing missing %q:\n%s", want, got)
		}
	}
	// Group order: user < feedback < project < reference < custom.
	idx := func(s string) int { return strings.Index(got, s) }
	if !(idx("user (1):") < idx("feedback (1):") && idx("feedback (1):") < idx("project (2):") &&
		idx("project (2):") < idx("reference (1):") && idx("reference (1):") < idx("custom (1):")) {
		t.Errorf("groups out of order:\n%s", got)
	}
	// Newest first within the project group.
	if !(idx("new-proj") < idx("old-proj")) {
		t.Errorf("project group not newest-first:\n%s", got)
	}
	// Empty description renders no " — " tail.
	if strings.Contains(got, "new-proj (30m ago) —") {
		t.Errorf("empty description should render no dash tail:\n%s", got)
	}
}

// TestMemoryCommandListing verifies /memory shows facts plus typed
// memories grouped with ages.
func TestMemoryCommandListing(t *testing.T) {
	s := newStubMemoryEnv()
	now := time.Now()
	s.typed = []TypedMemory{
		{Name: "prefs", Description: "prefers concise replies", Type: memory.MemoryType("user"), UpdatedAt: now.Add(-49 * time.Hour)},
		{Name: "orion", Description: "Go core, TS UI", Type: memory.MemoryType("project"), UpdatedAt: now.Add(-3 * time.Hour)},
	}
	reply := dispatch(t, s, "/memory")
	for _, want := range []string{
		"Memory: 1 fact(s)",
		"  1. k: v",
		"Typed memories:",
		"  user (1):",
		"    • prefs (2d ago) — prefers concise replies",
		"  project (1):",
		"    • orion (3h ago) — Go core, TS UI",
	} {
		if !strings.Contains(reply, want) {
			t.Errorf("/memory reply missing %q:\n%s", want, reply)
		}
	}
}

// TestMemoryCommandListingUnwired verifies /memory is unchanged
// (facts only) when the typed-memory source is not wired.
func TestMemoryCommandListingUnwired(t *testing.T) {
	s := newStubMemoryEnv()
	s.env.TypedMemories = nil
	reply := dispatch(t, s, "/memory")
	if strings.Contains(reply, "Typed memories") {
		t.Errorf("/memory with unwired typed memories should show facts only:\n%s", reply)
	}
	if !strings.Contains(reply, "  1. k: v") {
		t.Errorf("/memory missing facts:\n%s", reply)
	}
}

// TestMemoryCommandListingTypedError verifies a typed-list failure
// degrades to a note and never hides the facts.
func TestMemoryCommandListingTypedError(t *testing.T) {
	s := newStubMemoryEnv()
	s.typedErr = errors.New("memories dir unreadable")
	reply := dispatch(t, s, "/memory")
	if !strings.Contains(reply, "  1. k: v") {
		t.Errorf("/memory missing facts after typed-list error:\n%s", reply)
	}
	if !strings.Contains(reply, "Typed memories could not be listed") {
		t.Errorf("/memory should note the typed-list failure:\n%s", reply)
	}
}

// TestMemoryForgetByNumber verifies numbers still refer to facts.
func TestMemoryForgetByNumber(t *testing.T) {
	s := newStubMemoryEnv()
	reply := dispatch(t, s, "/memory forget 2")
	if s.forgetFactCalled != 1 || s.forgetFactIndex != 2 {
		t.Errorf("ForgetFact called %d times with index %d, want 1 call with 2", s.forgetFactCalled, s.forgetFactIndex)
	}
	if s.forgetTypedCalled != "" {
		t.Errorf("ForgetTypedMemory should not be called for a number, got %q", s.forgetTypedCalled)
	}
	if want := "🗑 Forgot fact 2 (k)."; reply != want {
		t.Errorf("reply = %q, want %q", reply, want)
	}
}

// TestMemoryForgetByName verifies a non-numeric argument deletes by
// typed-memory name.
func TestMemoryForgetByName(t *testing.T) {
	s := newStubMemoryEnv()
	s.forgetTypedDeleted = "prefs"
	reply := dispatch(t, s, "/memory forget prefs")
	if s.forgetTypedCalled != "prefs" {
		t.Errorf("ForgetTypedMemory called with %q, want %q", s.forgetTypedCalled, "prefs")
	}
	if s.forgetFactCalled != 0 {
		t.Errorf("ForgetFact should not be called for a name")
	}
	if want := `🗑 Forgot typed memory "prefs".`; reply != want {
		t.Errorf("reply = %q, want %q", reply, want)
	}
}

// TestMemoryForgetByNameUnwired verifies forget-by-name degrades
// gracefully when the store delete is not wired, without breaking
// forget-by-number.
func TestMemoryForgetByNameUnwired(t *testing.T) {
	s := newStubMemoryEnv()
	s.env.ForgetTypedMemory = nil
	reply := dispatch(t, s, "/memory forget prefs")
	if !strings.Contains(reply, "not wired up yet") {
		t.Errorf("reply should explain typed forgetting is unwired:\n%s", reply)
	}
	if s.forgetFactCalled != 0 {
		t.Errorf("ForgetFact should not be called for a name")
	}
}

// TestMemoryForgetByNameError verifies a store error is surfaced.
func TestMemoryForgetByNameError(t *testing.T) {
	s := newStubMemoryEnv()
	s.forgetTypedErr = errors.New(`no typed memory named "nope"`)
	reply := dispatch(t, s, "/memory forget nope")
	if !strings.Contains(reply, "❌") || !strings.Contains(reply, "nope") {
		t.Errorf("reply should surface the error:\n%s", reply)
	}
}

// TestMemoryForgetUsage verifies the usage text documents both
// forget forms.
func TestMemoryForgetUsage(t *testing.T) {
	s := newStubMemoryEnv()
	if reply := dispatch(t, s, "/memory forget 0"); !strings.Contains(reply, "Usage: /memory forget N") {
		t.Errorf("/memory forget 0 reply = %q, want usage", reply)
	}
	if reply := dispatch(t, s, "/memory bogus"); !strings.Contains(reply, "/memory forget <name>") {
		t.Errorf("/memory bogus reply should document forget by name:\n%s", reply)
	}
}

// TestCompressWithSessionMemory verifies /compress passes the
// session-memory text to the session-aware variant and says the
// summary drew on it.
func TestCompressWithSessionMemory(t *testing.T) {
	s := newStubMemoryEnv()
	s.sessionMemory = "Decision: use Go for the core.\nFact: budget $2M."
	s.compressBefore, s.compressAfter, s.compressSummary = 25, 10, 140
	reply := dispatch(t, s, "/compress")
	if !s.compressVariant {
		t.Errorf("CompressWithSessionMemory not called")
	}
	if s.compressCalled {
		t.Errorf("plain Compress should not be called when session memory is used")
	}
	if s.compressSessionIn != s.sessionMemory {
		t.Errorf("variant got session memory %q, want %q", s.compressSessionIn, s.sessionMemory)
	}
	if !strings.Contains(reply, "drawing on session memory") {
		t.Errorf("reply should say the summary drew on session memory:\n%s", reply)
	}
	if !strings.Contains(reply, "25 → 10 messages") {
		t.Errorf("reply missing counts:\n%s", reply)
	}
}

// TestCompressFallsBackWithoutSessionMemory verifies an empty
// session memory uses plain Compress (no variant call).
func TestCompressFallsBackWithoutSessionMemory(t *testing.T) {
	s := newStubMemoryEnv()
	s.sessionMemory = "  \n"
	s.compressBefore, s.compressAfter, s.compressSummary = 25, 10, 140
	reply := dispatch(t, s, "/compress")
	if !s.compressCalled || s.compressVariant {
		t.Errorf("compressCalled=%v variant=%v, want plain only", s.compressCalled, s.compressVariant)
	}
	if strings.Contains(reply, "drawing on session memory") {
		t.Errorf("reply should not claim session memory when there was none:\n%s", reply)
	}
}

// TestCompressFallsBackWhenUnwired verifies /compress works exactly
// as before when neither session-memory field is wired.
func TestCompressFallsBackWhenUnwired(t *testing.T) {
	s := newStubMemoryEnv()
	s.env.SessionMemory = nil
	s.env.CompressWithSessionMemory = nil
	s.compressBefore, s.compressAfter, s.compressSummary = 25, 10, 140
	reply := dispatch(t, s, "/compress")
	if !s.compressCalled {
		t.Errorf("plain Compress not called")
	}
	if want := "🗜 Compressed: 25 → 10 messages. Summary (140 chars) saved as a memory fact."; reply != want {
		t.Errorf("reply = %q, want %q", reply, want)
	}
}

// TestCompressSessionMemoryReadError verifies a session-memory read
// failure never blocks compression: plain Compress runs and the
// reply says the summary drew on messages only.
func TestCompressSessionMemoryReadError(t *testing.T) {
	s := newStubMemoryEnv()
	s.sessionMemoryErr = errors.New("session-memory.md unreadable")
	s.compressBefore, s.compressAfter, s.compressSummary = 25, 10, 140
	reply := dispatch(t, s, "/compress")
	if !s.compressCalled || s.compressVariant {
		t.Errorf("compressCalled=%v variant=%v, want plain only", s.compressCalled, s.compressVariant)
	}
	if !strings.Contains(reply, "session memory could not be read") {
		t.Errorf("reply should note the session-memory read failure:\n%s", reply)
	}
}

// TestCompressSessionMemoryNothingToCompress verifies the
// nothing-to-compress reply still works through the variant.
func TestCompressSessionMemoryNothingToCompress(t *testing.T) {
	s := newStubMemoryEnv()
	s.sessionMemory = "some session memory"
	s.compressBefore, s.compressAfter = 5, 5
	reply := dispatch(t, s, "/compress")
	if !strings.Contains(reply, "Nothing old enough to compress") {
		t.Errorf("reply = %q, want nothing-to-compress", reply)
	}
}

// TestFormatFooterWithMemories verifies the memory segment reads
// core.TurnStats.MemoriesUsed (workstream B): omitted at 0 (existing
// rendering unchanged), singular at 1, plural above.
func TestFormatFooterWithMemories(t *testing.T) {
	stats := &core.TurnStats{Provider: "deepseek", Model: "deepseek-chat", Tokens: 1240, Steps: 3, Duration: 4200 * time.Millisecond}
	if got, want := FormatFooter(stats), "— deepseek-chat · 1,240 tokens · 3 steps · 4.2s"; got != want {
		t.Errorf("0 memories: got %q, want %q", got, want)
	}
	stats.MemoriesUsed = 1
	if got, want := FormatFooter(stats), "— deepseek-chat · 1,240 tokens · 3 steps · 4.2s · 1 memory"; got != want {
		t.Errorf("1 memory: got %q, want %q", got, want)
	}
	stats.MemoriesUsed = 3
	if got, want := FormatFooter(stats), "— deepseek-chat · 1,240 tokens · 3 steps · 4.2s · 3 memories"; got != want {
		t.Errorf("3 memories: got %q, want %q", got, want)
	}
	stats.MemoriesUsed = -1
	if got := FormatFooter(stats); strings.Contains(got, "memor") {
		t.Errorf("negative MemoriesUsed should omit the segment, got %q", got)
	}
}

// TestFooterHelpDocumentsMemories verifies /footer documents the
// new memory segment.
func TestFooterHelpDocumentsMemories(t *testing.T) {
	s := newStubMemoryEnv()
	reply := dispatch(t, s, "/footer")
	if !strings.Contains(reply, "memories") {
		t.Errorf("/footer help should document the memory segment:\n%s", reply)
	}
}
