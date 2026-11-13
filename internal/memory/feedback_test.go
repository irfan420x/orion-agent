package memory

import (
	"strings"
	"testing"
)

func TestNewFeedbackMemoryCorrection(t *testing.T) {
	m, err := NewFeedbackMemory(SaveCorrection, "Run gofmt first", "Always format before reporting tests green", "Run gofmt before reporting tests green.", "Unformatted code fails CI and wastes a round trip.", "Run `gofmt -l .` after every edit.")
	if err != nil {
		t.Fatalf("NewFeedbackMemory: %v", err)
	}
	if m.Type != TypeFeedback {
		t.Fatalf("Type: got %q want feedback", m.Type)
	}
	parts, err := ParseFeedbackBody(m.Body)
	if err != nil {
		t.Fatalf("ParseFeedbackBody: %v", err)
	}
	if parts.Kind != SaveCorrection {
		t.Errorf("Kind: got %v want correction", parts.Kind)
	}
	if parts.Rule != "Run gofmt before reporting tests green." {
		t.Errorf("Rule: got %q", parts.Rule)
	}
	if !strings.Contains(parts.Why, "CI") {
		t.Errorf("Why: got %q", parts.Why)
	}
	if !strings.Contains(parts.How, "gofmt -l") {
		t.Errorf("How: got %q", parts.How)
	}
	// Round-trips through the file format and validates on save.
	s := mustTypedStore(t, t.TempDir())
	if err := s.Save(m); err != nil {
		t.Fatalf("Save feedback: %v", err)
	}
	loaded, found, err := s.Load("Run gofmt first")
	if err != nil || !found {
		t.Fatalf("Load: found=%v err=%v", found, err)
	}
	if _, err := ParseFeedbackBody(loaded.Body); err != nil {
		t.Fatalf("ParseFeedbackBody after load: %v", err)
	}
}

func TestNewFeedbackMemoryConfirmation(t *testing.T) {
	// A confirmation ("yes exactly") is a valid memory, not only a
	// correction: the confirmed behaviour is what is remembered.
	m, err := NewFeedbackMemory(SaveConfirmation, "Short replies", "Irfan likes short replies", "Keep replies short.", "Long replies bury the answer.", "Answer first, then at most one short paragraph.")
	if err != nil {
		t.Fatalf("NewFeedbackMemory: %v", err)
	}
	parts, err := ParseFeedbackBody(m.Body)
	if err != nil {
		t.Fatalf("ParseFeedbackBody: %v", err)
	}
	if parts.Kind != SaveConfirmation {
		t.Errorf("Kind: got %v want confirmation", parts.Kind)
	}
	if parts.Rule != "Keep replies short." {
		t.Errorf("Rule: got %q", parts.Rule)
	}
	if !strings.Contains(m.Body, "Confirmed by the user:") {
		t.Errorf("confirmation body should carry the marker, got:\n%s", m.Body)
	}
}

func TestNewFeedbackMemoryValidation(t *testing.T) {
	cases := []struct {
		name      string
		rule, why string
		how       string
	}{
		{"empty rule", "", "why", "how"},
		{"empty why", "rule", "", "how"},
		{"empty how", "rule", "why", ""},
	}
	for _, c := range cases {
		if _, err := NewFeedbackMemory(SaveCorrection, "n", "d", c.rule, c.why, c.how); err == nil {
			t.Errorf("%s: expected an error, got none", c.name)
		}
	}
}

func TestValidateFeedbackBadBodies(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no sections", "Just a rule, nothing else."},
		{"missing why", "Rule.\n\nHow to apply: do it.\n"},
		{"missing how", "Rule.\n\nWhy: because.\n"},
		{"how before why", "Rule.\n\nHow to apply: do it.\n\nWhy: because.\n"},
		{"empty why", "Rule.\n\nWhy:\n\nHow to apply: do it.\n"},
		{"empty how", "Rule.\n\nWhy: because.\n\nHow to apply:\n"},
		{"no rule", "Why: because.\n\nHow to apply: do it.\n"},
	}
	for _, c := range cases {
		m := TypedMemory{Name: "n", Description: "d", Type: TypeFeedback, Body: c.body}
		if err := ValidateFeedback(m); err == nil {
			t.Errorf("%s: expected an error, got none", c.name)
		}
		if err := m.Validate(); err == nil {
			t.Errorf("%s: Validate should also fail", c.name)
		}
	}
}

func TestValidateFeedbackWrongType(t *testing.T) {
	m := TypedMemory{Name: "n", Description: "d", Type: TypeUser, Body: "Rule.\n\nWhy: x.\n\nHow to apply: y.\n"}
	if err := ValidateFeedback(m); err == nil {
		t.Error("ValidateFeedback on a user memory: expected an error, got none")
	}
}

func TestParseFeedbackBodyMultilineSections(t *testing.T) {
	body := "Prefer table-driven tests.\n\nWhy: they cover more cases\nwith less code, and failures name the case.\n\nHow to apply: write one test function\nwith a slice of cases.\n"
	parts, err := ParseFeedbackBody(body)
	if err != nil {
		t.Fatalf("ParseFeedbackBody: %v", err)
	}
	if parts.Rule != "Prefer table-driven tests." {
		t.Errorf("Rule: got %q", parts.Rule)
	}
	if !strings.Contains(parts.Why, "failures name the case") {
		t.Errorf("Why: got %q", parts.Why)
	}
	if !strings.Contains(parts.How, "slice of cases") {
		t.Errorf("How: got %q", parts.How)
	}
}

func TestParseFeedbackBodyInlineValues(t *testing.T) {
	body := "Rule here.\nWhy: inline reason.\nHow to apply: inline steps.\n"
	parts, err := ParseFeedbackBody(body)
	if err != nil {
		t.Fatalf("ParseFeedbackBody: %v", err)
	}
	if parts.Why != "inline reason." || parts.How != "inline steps." {
		t.Errorf("inline values: Why=%q How=%q", parts.Why, parts.How)
	}
}

func TestParseSaveKind(t *testing.T) {
	if k, err := ParseSaveKind("confirmation"); err != nil || k != SaveConfirmation {
		t.Errorf("ParseSaveKind(confirmation): %v %v", k, err)
	}
	if k, err := ParseSaveKind("Correction"); err != nil || k != SaveCorrection {
		t.Errorf("ParseSaveKind(Correction): %v %v", k, err)
	}
	if _, err := ParseSaveKind("maybe"); err == nil {
		t.Error("ParseSaveKind(maybe): expected an error")
	}
}
