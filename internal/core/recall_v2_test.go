package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// --- fakes ---------------------------------------------------------------

type fakeTypedSource struct {
	memories []TypedMemory
	listErr  error
	loadErr  error
	stale    []string // names marked stale via the optional marker
}

func (f *fakeTypedSource) ListTypedMemories() ([]TypedMemory, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]TypedMemory(nil), f.memories...), nil
}

func (f *fakeTypedSource) LoadTypedMemory(name string) (TypedMemory, error) {
	if f.loadErr != nil {
		return TypedMemory{}, f.loadErr
	}
	for _, m := range f.memories {
		if m.Name == name {
			return m, nil
		}
	}
	return TypedMemory{}, fmt.Errorf("no such memory %q", name)
}

// MarkTypedMemoryStale is the optional persistent stale marker.
func (f *fakeTypedSource) MarkTypedMemoryStale(name string) error {
	f.stale = append(f.stale, name)
	return nil
}

func testTypedMemories() []TypedMemory {
	now := time.Now()
	return []TypedMemory{
		{Name: "prefer-brevity", Description: "User prefers short answers", Type: "feedback", Body: "Keep replies short.", UpdatedAt: now.Add(-time.Hour)},
		{Name: "deploy-notes", Description: "How to deploy the web service", Type: "project", Body: "Deploy with the deploy script.", UpdatedAt: now.Add(-2 * time.Hour)},
		{Name: "calculator-warning", Description: "Warning: calculator rounds large numbers", Type: "feedback", Body: "Warning: the calculator tool rounds numbers above 1e9.", UpdatedAt: now.Add(-3 * time.Hour)},
		{Name: "calculator-usage", Description: "How to use the calculator tool", Type: "reference", Body: "Use the calculator tool for arithmetic.", UpdatedAt: now.Add(-4 * time.Hour)},
		{Name: "favourite-number", Description: "User favourite number is 17", Type: "user", Body: "Favourite number is 17.", UpdatedAt: now.Add(-5 * time.Hour)},
	}
}

func newV2TestAgent(t *testing.T) *Agent {
	t.Helper()
	return newTestAgent(t, testConfig(5), &fakeProvider{}, tools.NewRegistry(), newFakeStore())
}

// --- Manifest ------------------------------------------------------------

func TestTypedMemoryManifestNewestFirstAndCapped(t *testing.T) {
	now := time.Now()
	var all []TypedMemory
	for i := 0; i < 250; i++ {
		all = append(all, TypedMemory{Name: fmt.Sprintf("mem-%03d", i), Description: "d", Type: "project", UpdatedAt: now.Add(-time.Duration(i) * time.Minute)})
	}
	// Shuffle input order: newest must still come first.
	all[0], all[249] = all[249], all[0]
	got := typedMemoryManifest(all)
	if len(got) != maxTypedMemories {
		t.Fatalf("manifest len = %d, want %d", len(got), maxTypedMemories)
	}
	if got[0].Name != "mem-000" {
		t.Errorf("manifest[0] = %q, want newest mem-000", got[0].Name)
	}
	if got[len(got)-1].Name != "mem-199" {
		t.Errorf("manifest[last] = %q, want mem-199", got[len(got)-1].Name)
	}
	// Empty names are dropped, not selected.
	got = typedMemoryManifest([]TypedMemory{{Name: "", Description: "x"}, {Name: "ok", Description: "y"}})
	if len(got) != 1 || got[0].Name != "ok" {
		t.Errorf("manifest with empty name = %v, want only [ok]", got)
	}
}

// --- Selector: provider side-query ---------------------------------------

func TestSelectorWithFakeProvider(t *testing.T) {
	manifest := typedMemoryManifest(testTypedMemories())
	prov := &fakeProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		if req.MaxTokens != maxSelectorTokens {
			t.Errorf("selector MaxTokens = %d, want %d", req.MaxTokens, maxSelectorTokens)
		}
		return &model.ChatResponse{
			Message:      model.Message{Role: "assistant", Content: `["prefer-brevity", "favourite-number"]`},
			FinishReason: "stop",
		}, nil
	}}
	got, err := selectMemoriesWithProvider(context.Background(), prov, manifest, "keep it short, my favourite number?", nil, nil)
	if err != nil {
		t.Fatalf("selectMemoriesWithProvider: %v", err)
	}
	if len(got) != 2 || got[0] != "prefer-brevity" || got[1] != "favourite-number" {
		t.Errorf("selector = %v, want [prefer-brevity favourite-number]", got)
	}
}

func TestSelectorParsesDefensively(t *testing.T) {
	manifest := typedMemoryManifest(testTypedMemories())
	cases := []struct {
		name    string
		content string
		want    []string
		wantErr bool
	}{
		{"wrapped in prose", "Here are the relevant ones:\n[\"prefer-brevity\"]\nHope that helps.", []string{"prefer-brevity"}, false},
		{"unknown name dropped", `["prefer-brevity", "no-such-memory", "deploy-notes"]`, []string{"prefer-brevity", "deploy-notes"}, false},
		{"duplicates dropped", `["deploy-notes", "deploy-notes"]`, []string{"deploy-notes"}, false},
		{"over five capped", `["prefer-brevity","deploy-notes","calculator-warning","calculator-usage","favourite-number","prefer-brevity"]`, []string{"prefer-brevity", "deploy-notes", "calculator-warning", "calculator-usage", "favourite-number"}, false},
		{"empty list ok", `[]`, nil, false},
		{"no json is error", "I think prefer-brevity is relevant.", nil, true},
		{"malformed json is error", `["prefer-brevity",`, nil, true},
		{"not an array is error", `{"name": "prefer-brevity"}`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &fakeProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
				return &model.ChatResponse{Message: model.Message{Role: "assistant", Content: tc.content}, FinishReason: "stop"}, nil
			}}
			got, err := selectMemoriesWithProvider(context.Background(), prov, manifest, "test", nil, nil)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got %v", got)
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(got) != len(tc.want) {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
				for i := range tc.want {
					if got[i] != tc.want[i] {
						t.Errorf("got[%d] = %q, want %q", i, got[i], tc.want[i])
					}
				}
			}
		})
	}
}

func TestSelectorPromptExcludesSurfacedAndNamesTools(t *testing.T) {
	manifest := typedMemoryManifest(testTypedMemories())
	p := selectorPrompt(manifest, "hello", []string{"calculator"}, []string{"prefer-brevity"})
	for _, want := range []string{"prefer-brevity", "calculator", "do NOT select these again", "warning, gotcha or pitfall", "at most 5"} {
		if !strings.Contains(p, want) {
			t.Errorf("selector prompt missing %q:\n%s", want, p)
		}
	}
}

// --- Fallback ------------------------------------------------------------

func TestFallbackKeywordPath(t *testing.T) {
	manifest := typedMemoryManifest(testTypedMemories())
	got := fallbackSelectMemories(manifest, "what is my favourite number?", nil, nil)
	if len(got) == 0 || got[0] != "favourite-number" {
		t.Errorf("fallback = %v, want favourite-number first", got)
	}
	// No shared tokens: nothing.
	if got := fallbackSelectMemories(manifest, "zzzz qqqq", nil, nil); len(got) != 0 {
		t.Errorf("fallback(gibberish) = %v, want none", got)
	}
	// Surfaced memories are excluded from the fallback.
	got = fallbackSelectMemories(manifest, "what is my favourite number?", nil, []string{"favourite-number"})
	for _, n := range got {
		if n == "favourite-number" {
			t.Errorf("fallback re-surfaced %q", n)
		}
	}
	// Usage docs for tools in active use are dropped, warnings kept.
	got = fallbackSelectMemories(manifest, "calculator tool arithmetic", []string{"calculator"}, nil)
	for _, n := range got {
		if n == "calculator-usage" {
			t.Errorf("fallback surfaced usage doc %q for a tool in active use", n)
		}
	}
	foundWarning := false
	for _, n := range got {
		if n == "calculator-warning" {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Errorf("fallback = %v, want calculator-warning kept (warnings are surfaced)", got)
	}
}

func TestFallbackUsesSharedScorer(t *testing.T) {
	// The fallback must reuse contentTokens/sharedTokenCount, not a
	// second scorer: a stopword-only query scores nothing.
	manifest := typedMemoryManifest(testTypedMemories())
	if got := fallbackSelectMemories(manifest, "what is the", nil, nil); len(got) != 0 {
		t.Errorf("fallback(stopwords only) = %v, want none", got)
	}
}

// --- Wiring: recallTypedFor ----------------------------------------------

func TestRecallTypedForSelectorFirstFallbackSecond(t *testing.T) {
	a := newV2TestAgent(t)
	a.SetTypedMemorySource(&fakeTypedSource{memories: testTypedMemories()})
	// Selector succeeds: its picks win — and standing guidance
	// (feedback) is prepended ahead of them.
	a.SetMemorySelector(func(ctx context.Context, manifest []TypedMemory, input string, recentTools []string, surfaced []string) ([]string, error) {
		return []string{"deploy-notes"}, nil
	})
	got := a.recallTypedFor(context.Background(), "how do I deploy?", nil)
	want := []string{"prefer-brevity", "calculator-warning", "deploy-notes"}
	if len(got) != len(want) {
		t.Fatalf("recallTypedFor(selector) = %v, want %v", namesOf(got), want)
	}
	for i, n := range want {
		if got[i].Name != n {
			t.Fatalf("recallTypedFor(selector)[%d] = %q, want %q", i, got[i].Name, n)
		}
	}
	// Surfaced set excludes the topical pick next turn — but the
	// feedback guidance recurs every turn.
	got = a.recallTypedFor(context.Background(), "how do I deploy?", nil)
	want = []string{"prefer-brevity", "calculator-warning"}
	if len(got) != len(want) {
		t.Fatalf("recallTypedFor second turn = %v, want %v", namesOf(got), want)
	}
	for i, n := range want {
		if got[i].Name != n {
			t.Fatalf("recallTypedFor second turn[%d] = %q, want %q", i, got[i].Name, n)
		}
	}

	// Selector fails: keyword fallback runs instead.
	a2 := newV2TestAgent(t)
	a2.SetTypedMemorySource(&fakeTypedSource{memories: testTypedMemories()})
	a2.SetMemorySelector(func(ctx context.Context, manifest []TypedMemory, input string, recentTools []string, surfaced []string) ([]string, error) {
		return nil, errors.New("provider down")
	})
	got = a2.recallTypedFor(context.Background(), "what is my favourite number?", nil)
	if len(got) == 0 || got[0].Name != "prefer-brevity" {
		t.Errorf("recallTypedFor(fallback) = %v, want feedback first", namesOf(got))
	}
	found := false
	for _, m := range got {
		if m.Name == "favourite-number" {
			found = true
		}
	}
	if !found {
		t.Errorf("recallTypedFor(fallback) = %v, want favourite-number among them", namesOf(got))
	}
}

func namesOf(mems []TypedMemory) []string {
	out := make([]string, len(mems))
	for i, m := range mems {
		out[i] = m.Name
	}
	return out
}

// TestRecallTypedForFeedbackAlwaysOn: feedback memories are standing
// guidance — they are injected every turn even when the selector
// picks nothing, they recur (never surfaced-excluded), and they are
// never crowded out by topical picks.
func TestRecallTypedForFeedbackAlwaysOn(t *testing.T) {
	a := newV2TestAgent(t)
	a.SetTypedMemorySource(&fakeTypedSource{memories: testTypedMemories()})
	// Selector selects nothing at all.
	a.SetMemorySelector(func(ctx context.Context, manifest []TypedMemory, input string, recentTools []string, surfaced []string) ([]string, error) {
		return []string{}, nil
	})
	got := a.recallTypedFor(context.Background(), "what is Orion?", nil)
	want := []string{"prefer-brevity", "calculator-warning"}
	if len(got) != len(want) {
		t.Fatalf("recallTypedFor(selector empty) = %v, want %v", namesOf(got), want)
	}
	for i, n := range want {
		if got[i].Name != n {
			t.Fatalf("recallTypedFor(selector empty)[%d] = %q, want %q", i, got[i].Name, n)
		}
	}
	// Second turn: the guidance recurs.
	got = a.recallTypedFor(context.Background(), "what is Orion?", nil)
	if len(got) != len(want) {
		t.Fatalf("recallTypedFor second turn = %v, want %v (guidance recurs)", namesOf(got), want)
	}
	// Crowding: six topical picks plus two feedback memories — the
	// total stays capped and the guidance stays first.
	var many []TypedMemory
	many = append(many, testTypedMemories()...)
	for i := 0; i < 6; i++ {
		many = append(many, TypedMemory{Name: fmt.Sprintf("proj-%d", i), Description: "project notes", Type: "project", Body: "body"})
	}
	a2 := newV2TestAgent(t)
	a2.SetTypedMemorySource(&fakeTypedSource{memories: many})
	a2.SetMemorySelector(func(ctx context.Context, manifest []TypedMemory, input string, recentTools []string, surfaced []string) ([]string, error) {
		return []string{"proj-0", "proj-1", "proj-2", "proj-3", "proj-4", "proj-5"}, nil
	})
	got = a2.recallTypedFor(context.Background(), "project", nil)
	if len(got) != maxTypedRecallItems {
		t.Fatalf("recallTypedFor crowded = %v, want %d items", namesOf(got), maxTypedRecallItems)
	}
	if got[0].Name != "prefer-brevity" || got[1].Name != "calculator-warning" {
		t.Fatalf("recallTypedFor crowded = %v, want guidance first", namesOf(got))
	}
}

func TestRecallTypedForNeverFails(t *testing.T) {
	// No source wired: nil, no panic.
	a := newV2TestAgent(t)
	if got := a.recallTypedFor(context.Background(), "hello", nil); got != nil {
		t.Errorf("recallTypedFor(no source) = %v, want nil", got)
	}
	// List error: nil, no panic.
	a.SetTypedMemorySource(&fakeTypedSource{listErr: errors.New("disk gone")})
	if got := a.recallTypedFor(context.Background(), "hello", nil); got != nil {
		t.Errorf("recallTypedFor(list error) = %v, want nil", got)
	}
	// Selector returns only unknown names: nil (with no feedback
	// memories present — feedback is standing guidance and is
	// injected even when the selector picks nothing).
	a2 := newV2TestAgent(t)
	a2.SetTypedMemorySource(&fakeTypedSource{memories: []TypedMemory{
		{Name: "deploy-notes", Description: "How to deploy", Type: "project", Body: "Deploy."},
	}})
	a2.SetMemorySelector(func(ctx context.Context, manifest []TypedMemory, input string, recentTools []string, surfaced []string) ([]string, error) {
		return []string{"ghost-one", "ghost-two"}, nil
	})
	if got := a2.recallTypedFor(context.Background(), "hello", nil); got != nil {
		t.Errorf("recallTypedFor(unknown names) = %v, want nil", got)
	}
}

func TestRecallTypedForDedupesAndCaps(t *testing.T) {
	a := newV2TestAgent(t)
	var many []TypedMemory
	for i := 0; i < 8; i++ {
		many = append(many, TypedMemory{Name: fmt.Sprintf("mem-%d", i), Description: "deploy notes", Type: "project", Body: "body"})
	}
	a.SetTypedMemorySource(&fakeTypedSource{memories: many})
	a.SetMemorySelector(func(ctx context.Context, manifest []TypedMemory, input string, recentTools []string, surfaced []string) ([]string, error) {
		return []string{"mem-0", "mem-0", "mem-1", "mem-2", "mem-3", "mem-4", "mem-5", "mem-6", "mem-7"}, nil
	})
	got := a.recallTypedFor(context.Background(), "deploy", nil)
	if len(got) != maxTypedRecallItems {
		t.Fatalf("recallTypedFor = %d items, want %d (cap)", len(got), maxTypedRecallItems)
	}
	seen := map[string]bool{}
	for _, m := range got {
		if seen[m.Name] {
			t.Errorf("duplicate %q injected", m.Name)
		}
		seen[m.Name] = true
	}
}

func TestFormatTypedMemorySection(t *testing.T) {
	if got := formatTypedMemorySection(nil); got != "" {
		t.Errorf("formatTypedMemorySection(nil) = %q, want empty", got)
	}
	// Feedback-only renders under the standing-guidance header,
	// not the topical header.
	got := formatTypedMemorySection([]TypedMemory{{Name: "prefer-brevity", Type: "feedback", Description: "User prefers short answers", Body: "Keep replies short."}})
	for _, want := range []string{"Standing guidance", "[feedback]", "prefer-brevity", "User prefers short answers", "Keep replies short."} {
		if !strings.Contains(got, want) {
			t.Errorf("typed section missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Relevant saved memories") {
		t.Errorf("feedback-only section must not use the topical header:\n%s", got)
	}
	// Topical-only renders under the topical header.
	got = formatTypedMemorySection([]TypedMemory{{Name: "deploy-notes", Type: "project", Description: "How to deploy", Body: "Deploy with the script."}})
	if !strings.Contains(got, "Relevant saved memories") {
		t.Errorf("topical section missing topical header:\n%s", got)
	}
	// Mixed: guidance first, then topical.
	got = formatTypedMemorySection([]TypedMemory{
		{Name: "deploy-notes", Type: "project", Description: "How to deploy", Body: "Deploy."},
		{Name: "prefer-brevity", Type: "feedback", Description: "User prefers short answers", Body: "Keep replies short."},
	})
	if !strings.Contains(got, "Standing guidance") || !strings.Contains(got, "Relevant saved memories") {
		t.Errorf("mixed section missing a header:\n%s", got)
	}
	if strings.Index(got, "Standing guidance") > strings.Index(got, "Relevant saved memories") {
		t.Errorf("guidance must come before topical memories:\n%s", got)
	}
}

// --- System prompt save rules --------------------------------------------

func TestSystemPromptHasMemorySaveRules(t *testing.T) {
	a := newV2TestAgent(t)
	p := a.SystemPrompt()
	for _, want := range []string{
		"user", "feedback", "project", "reference", // four types
		"corrections AND confirmations",
		"surprising or non-obvious",
		"Never save code patterns",
		"architecture", "git history", "file structure",
		"verify it still exists",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("SystemPrompt missing memory save rule %q", want)
		}
	}
}

// --- Run integration: MemoriesUsed ----------------------------------------

func TestRunReportsMemoriesUsedOnTurnFinished(t *testing.T) {
	prov := &fakeProvider{}
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), newFakeStore())
	a.SetTypedMemorySource(&fakeTypedSource{memories: testTypedMemories()})
	a.SetMemorySelector(func(ctx context.Context, manifest []TypedMemory, input string, recentTools []string, surfaced []string) ([]string, error) {
		return []string{"favourite-number"}, nil
	})
	var finished *ProgressEvent
	a.SetEventHandler(func(ev ProgressEvent) {
		if ev.Kind == EventTurnFinished {
			evCopy := ev
			finished = &evCopy
		}
	})
	out, err := a.Run(context.Background(), "what is my favourite number?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out == "" {
		t.Fatal("Run returned empty answer")
	}
	if finished == nil {
		t.Fatal("no TurnFinished event")
	}
	// favourite-number (selected) + the two feedback memories
	// (standing guidance, always injected).
	if finished.MemoriesUsed != 3 {
		t.Errorf("TurnFinished.MemoriesUsed = %d, want 3", finished.MemoriesUsed)
	}
	// The typed section reached the provider request.
	if !strings.Contains(prov.lastReq.Messages[0].Content, "favourite-number") {
		t.Errorf("system message missing injected typed memory; got:\n%s", prov.lastReq.Messages[0].Content)
	}
}
