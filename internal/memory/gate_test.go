package memory

import (
	"strings"
	"testing"
)

func TestGateSaves(t *testing.T) {
	// Real examples of things worth saving: user facts, preferences,
	// decisions, bugs, incidents.
	save := []string{
		"my favourite editor is Zed",
		"I prefer short replies with the answer first",
		"the production deploy failed on 2026-11-13 because the disk was full",
		"always run tests with -race, I was bitten by a data race in November",
		"Irfan's Telegram UID is 7429947930",
		"the DeepSeek provider is slow on first call after idle, retry once",
		"do not deploy on Fridays",
	}
	for _, c := range save {
		d := Gate(c)
		if !d.Save {
			t.Errorf("Gate(%q): got skip (%s), want save", c, d.Reason)
		}
		if d.Reason == "" {
			t.Errorf("Gate(%q): empty reason", c)
		}
	}
}

func TestGateSkips(t *testing.T) {
	// Real examples of derivable things that must never be saved.
	skip := []string{
		"the repo has a cmd/ dir",
		"the repository has a cmd directory and an internal package",
		"the file structure is cmd/, internal/, and web/",
		"the git history shows the sandbox was added in November",
		"the commit history for internal/memory is long",
		"the code uses the repository pattern for storage",
		"it follows a layered architecture with a core package",
		"the architecture is layered: core, tools, messaging",
		"the handler function takes a context and returns an error",
		"the retry logic is defined in file internal/core/agent.go",
	}
	for _, c := range skip {
		d := Gate(c)
		if d.Save {
			t.Errorf("Gate(%q): got save, want skip", c)
		}
		if d.Reason == "" {
			t.Errorf("Gate(%q): empty reason", c)
		}
	}
}

func TestGateEmpty(t *testing.T) {
	if d := Gate("   "); d.Save {
		t.Error("Gate(empty): expected skip")
	}
}

func TestFilterExplicitSave(t *testing.T) {
	// Mixed request: the derivable sentence is dropped, the
	// surprising sentence is kept.
	kept, ok := FilterExplicitSave("Remember that the repo has a cmd/ dir. I always want tests run with -race.")
	if !ok {
		t.Fatal("FilterExplicitSave: expected something kept")
	}
	if strings.Contains(kept, "cmd/") {
		t.Errorf("kept text still contains derivable part: %q", kept)
	}
	if !strings.Contains(kept, "-race") {
		t.Errorf("kept text lost the surprising part: %q", kept)
	}

	// Wholly derivable: nothing is kept, nothing should be saved.
	if _, ok := FilterExplicitSave("Remember the file structure is cmd/, internal/, and web/."); ok {
		t.Error("FilterExplicitSave(wholly derivable): expected ok=false")
	}
	if _, ok := FilterExplicitSave(""); ok {
		t.Error("FilterExplicitSave(empty): expected ok=false")
	}

	// Wholly worth saving: everything is kept.
	kept, ok = FilterExplicitSave("Remember that my favourite editor is Zed.")
	if !ok || !strings.Contains(kept, "Zed") {
		t.Errorf("FilterExplicitSave(preference): kept=%q ok=%v", kept, ok)
	}
}
