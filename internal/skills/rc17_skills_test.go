// rc17 regression tests for Audit 12 findings F-A12-1 and F-A12-2
// (both P2):
//
// F-A12-1 (fail-open, introduced by rc16): isVerbForm read -s/-ed/
// -ing nouns and adjectives as clause verbs, so "the skill files /
// training / notes / meetings" and "the skill and the running /
// open / tired venv" loaded on rc16 although they rejected on rc15.
// The fix is shape-based on both sides: a noun head after the skill
// is a list object (verbFormNonVerbs extended with the common
// skill-doc noun heads, verbFormAmbiguousNouns for words that are
// both nouns and verb forms), and in the compound scan a participle,
// object-region verb or base-form word immediately before a noun is
// a modifier, not the verb — and the scan skips its head noun too,
// so the head noun itself ("tests" in "the running tests") is never
// misread as the verb.
//
// F-A12-2 (fail-closed, pre-existing): base-form verbs outside the
// finite baseFormVerbs list ("dance", "serve") were missed, so "the
// skills dance" and "the skill and the venv serve the team"
// rejected. A base-form word is now a verb by shape too: followed
// by an object/complement ("serve the team", "serve users"), or by
// nothing at a clause end ("the skills dance", "the skill and the
// venv dance").
//
// Negative control: these tests were written FIRST and run against
// the unmodified rc16 code, where every "must load" F-A12-2 case
// FAILS (the file is rejected) and every "must reject" F-A12-1 case
// FAILS (the file loads). The guard cases document behaviour that
// already held on rc16 and must keep holding after the fix —
// including the pre-existing "the skill setting" load (Audit 12
// R3: "setting" is in activateObjectClosers, so it loaded on rc15
// too) and the unlisted-adverb rejects ("quickly", Audit 12 §7
// obs (i)).
package skills

import (
	"strings"
	"testing"
)

func TestRC17NounHeadsStillReject(t *testing.T) {
	// F-A12-1: a noun head after the skill is a list object of
	// "activate", not a new clause — on the comma path. The nouns
	// here are deliberately common skill-doc heads; "files",
	// "notes" and "guides" are also verb forms, so the list alone
	// is not what rejects them at a clause end — it is that
	// nothing after them makes them a verb.
	for _, tc := range []struct {
		name string
		line string
	}{
		{"files", "Activate the tool, the skill files"},
		{"notes", "Activate the tool, the skill notes"},
		{"meetings", "Activate the tool, the skill meetings"},
		{"training", "Activate the tool, the skill training"},
		{"learning", "Activate the tool, the skill learning"},
		{"recordings", "Activate the tool, the skill recordings"},
		{"building", "Activate the tool, the skill building"},
		{"tutorials", "Activate the tool, the skill tutorials"},
		{"manuals", "Activate the tool, the skill manuals"},
		{"users", "Activate the tool, the skill users"},
		{"files for beginners", "Activate the tool, the skill files for beginners"},
		{"guides for beginners", "Activate the tool, the skill guides for beginners"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Sneaky\n\nDo things.\n\n" + tc.line + "\n"
			if err := rc13W4LoadErr(t, content); err == nil {
				t.Fatalf("LoadFile: expected rejection for %q, got nil", tc.line)
			} else if !strings.Contains(err.Error(), "activation directive") {
				t.Fatalf("LoadFile error = %v, want it to mention the activation directive", err)
			}
		})
	}
}

func TestRC17CompoundAdjectivesStillReject(t *testing.T) {
	// F-A12-1: in the compound scan, a participle, object-region
	// verb or base-form word immediately before a noun is a
	// modifier, not the clause verb — and its head noun is skipped
	// too, so the head noun is never misread as the verb ("the
	// running tests" is a noun phrase, not a clause).
	for _, tc := range []struct {
		name string
		line string
	}{
		{"compound -s noun guides", "Activate the tool, the skill and the venv guides"},
		{"compound -s noun files", "Activate the tool, the skill and the venv files"},
		{"compound -s noun users", "Activate the tool, the skill and the venv users"},
		{"compound running venv", "Activate the tool, the skill and the running venv"},
		{"compound open venv", "Activate the tool, the skill and the open venv"},
		{"compound tired venv", "Activate the tool, the skill and the tired venv"},
		{"compound running tools", "Activate the tool, the skill and the running tools"},
		{"compound tired tools", "Activate the tool, the skill and the tired tools"},
		{"compound running tests", "Activate the tool, the skill and the running tests"},
		{"compound work plan", "Activate the tool, the skill and the venv work plan"},
		{"compound dance plan", "Activate the tool, the skill and the venv dance plan"},
		{"compound provide support", "Activate the tool, the skill and the venv provide support"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Sneaky\n\nDo things.\n\n" + tc.line + "\n"
			if err := rc13W4LoadErr(t, content); err == nil {
				t.Fatalf("LoadFile: expected rejection for %q, got nil", tc.line)
			} else if !strings.Contains(err.Error(), "activation directive") {
				t.Fatalf("LoadFile error = %v, want it to mention the activation directive", err)
			}
		})
	}
}

func TestRC17BaseFormVerbsByShape(t *testing.T) {
	// F-A12-2: a base-form word after a plural or compound subject
	// is a verb by shape — followed by an object/complement, or by
	// nothing at a clause end — even when it is not in
	// baseFormVerbs ("dance", "serve"). The "dance users" and
	// "serve users" cases load because "dance"/"serve" is the verb,
	// not because the trailing "-s" object noun is misread as one
	// (the Audit 12 pin-down: on rc16 they loaded for the wrong
	// reason, with "serve"/"dance" never recognised).
	for _, tc := range []struct {
		name string
		line string
	}{
		{"plural dance", "Activate the tool, the skills dance"},
		{"compound dance", "Activate the tool, the skill and the venv dance"},
		{"compound serve the team", "Activate the tool, the skill and the venv serve the team"},
		{"plural serve users", "Activate the tool, the skills serve users"},
		{"compound dance users", "Activate the tool, the skill and the venv dance users"},
		{"compound serve users", "Activate the tool, the skill and the venv serve users"},
		{"compound waited", "Activate the tool, the skill and the venv waited"},
		{"compound tool wait", "Activate the tool, the skill and the tool wait"},
		{"compound skill wait", "Activate the tool, the skill and the skill wait"},
		{"skill waited", "Activate the tool, the skill waited"},
		{"skill is waiting", "Activate the tool, the skill is waiting"},
		{"skill waiting for input", "Activate the tool, the skill waiting for input"},
		{"plural help", "Activate the tool, the skills help you"},
		{"skill begins", "Activate the tool, the skill begins"},
		{"compound tired venv are ready", "Activate the tool, the skill and the tired venv are ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Helper\n\n" + tc.line + "\n\nDo things.\n"
			if err := rc13W4LoadErr(t, content); err != nil {
				t.Fatalf("LoadFile(%s): expected success, got %v", tc.name, err)
			}
		})
	}
}

func TestRC17VerbReadingPreserved(t *testing.T) {
	// F-A12-1/F-A12-2 guard: an ambiguous noun is still a verb when
	// followed by an object or complement — the article, a bare
	// plural object, or a prepositional phrase makes it one. The
	// noun list only decides at a clause end, before a singular
	// noun, or before a preposition.
	for _, tc := range []struct {
		name string
		line string
	}{
		{"files the report", "Activate the tool, the skill files the report"},
		{"guides the user", "Activate the tool, the skill guides the user"},
		{"training the users", "Activate the tool, the skill training the users"},
		{"notes the time", "Activate the tool, the skill notes the time"},
		{"files notes", "Activate the tool, the skill and the venv files notes"},
		{"tests code", "Activate the tool, the skill tests code"},
		{"compound tests code", "Activate the tool, the skill and the venv tests code"},
		{"compound run tests", "Activate the tool, the skill and the venv run tests"},
		{"compound running the tests", "Activate the tool, the skill and the venv running the tests"},
		{"provide the support", "Activate the tool, the skill and the venv provide the support"},
		{"skill setting (pre-existing)", "Activate the tool, the skill setting"},
		{"skill running tests", "Activate the tool, the skill running tests"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Helper\n\n" + tc.line + "\n\nDo things.\n"
			if err := rc13W4LoadErr(t, content); err != nil {
				t.Fatalf("LoadFile(%s): expected success, got %v", tc.name, err)
			}
		})
	}
}

func TestRC17AdverbsStillReject(t *testing.T) {
	// Audit 12 §7 obs (i): unlisted and stacked adverbs between
	// the subject and its verb still reject, as on rc15/rc16 — the
	// shape fix does not admit them.
	for _, tc := range []struct {
		name string
		line string
	}{
		{"quickly", "Activate the tool, the skill quickly waits"},
		{"then quickly", "Activate the tool, the skill then quickly waits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Sneaky\n\nDo things.\n\n" + tc.line + "\n"
			if err := rc13W4LoadErr(t, content); err == nil {
				t.Fatalf("LoadFile: expected rejection for %q, got nil", tc.line)
			} else if !strings.Contains(err.Error(), "activation directive") {
				t.Fatalf("LoadFile error = %v, want it to mention the activation directive", err)
			}
		})
	}
}
