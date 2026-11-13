// rc16 regression tests for Audit 11 finding F-A11-1 (P2):
// the rc15 comma-list fix (F-A10-3) over-reached — its new-clause
// carve-out recognised only a finite verb list (clauseVerbAux plus
// the verb entries of activateObjectClosers), so ordinary new-clause
// verbs ("waits", "works", "explains") and compound subjects ("the
// skill and the venv are ready") were rejected, although they loaded
// on rc14. The carve-out now recognises a new clause by shape — any
// verb form after the skill, or a conjunction plus a subject
// followed by a verb — on both the comma path and the "and"/"then"
// path, which had no carve-out at all ("Activate the tool, and the
// skill is ready" rejected on both rc14 and rc15).
//
// Negative control: these tests were written FIRST and run against
// the unmodified rc15 code, where every "must load" case below
// FAILS (the file is rejected) and every "must reject" case PASSES.
// The guard cases document behaviour that already held on rc15 and
// must keep holding after the fix.
package skills

import (
	"strings"
	"testing"
)

func TestRC16CommaListNewClauseVerbs(t *testing.T) {
	// F-A11-1: a verb of any form after the skill starts a new
	// clause with the skill as its subject, so the skill is not
	// an object of "activate" — on the comma path. The verbs here
	// are deliberately NOT in any verb list in the code: the
	// carve-out recognises them by shape (-s, -ed, -ing), not by
	// enumeration.
	for _, tc := range []struct {
		name string
		line string
	}{
		{"waits", "Activate the tool, the skill waits"},
		{"works", "Activate the tool, the skill works"},
		{"explains the plan", "Activate the tool, the skill explains the plan"},
		{"starts", "Activate the tool, the skill starts"},
		{"finished, past tense", "Activate the tool, the skill finished"},
		{"waiting, participle", "Activate the tool, the skill waiting"},
		{"adverb then", "Activate the tool, the skill then waits"},
		{"adverb now", "Activate the tool, the skill now works"},
		{"plural subject, base form", "Activate the tool, the skills wait"},
		{"plural subject, base form work", "Activate the tool, the skills work"},
		{"parenthetical comma", "Activate the tool, the skill, is ready"},
		{"plural subject, explains", "Activate the tool, the skills explain the plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Helper\n\n" + tc.line + "\n\nDo things.\n"
			if err := rc13W4LoadErr(t, content); err != nil {
				t.Fatalf("LoadFile(%s): expected success, got %v", tc.name, err)
			}
		})
	}
}

func TestRC16CompoundSubject(t *testing.T) {
	// F-A11-1: a compound subject ("the skill and the venv ...")
	// followed by a verb is a new clause with the compound subject
	// as its subject, so the skill is not an object of "activate".
	for _, tc := range []struct {
		name string
		line string
	}{
		{"are ready", "Activate the tool, the skill and the venv are ready"},
		{"base form wait", "Activate the tool, the skill and the venv wait"},
		{"three-part subject", "Activate the tool, the skill and the venv and the tool are ready"},
		{"adjective in subject", "Activate the tool, the skill and the other venv are ready"},
		{"comma and verb", "Activate the tool, the skill, and the venv are ready"},
		{"or subject", "Activate the tool, the skill or the venv is ready"},
		{"compound subject, explains", "Activate the tool, the skill and the venv explain the plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Helper\n\n" + tc.line + "\n\nDo things.\n"
			if err := rc13W4LoadErr(t, content); err != nil {
				t.Fatalf("LoadFile(%s): expected success, got %v", tc.name, err)
			}
		})
	}
}

func TestRC16AndThenPath(t *testing.T) {
	// F-A11-1: the carve-out exists on the "and"/"then" path too.
	// "Activate the tool, and/then the skill is ready" rejected on
	// BOTH rc14 and rc15, because the main loop returned true on a
	// bare "skill" without consulting the following word.
	for _, tc := range []struct {
		name string
		line string
	}{
		{"comma and is ready", "Activate the tool, and the skill is ready"},
		{"comma then is ready", "Activate the tool, then the skill is ready"},
		{"and is ready, no comma", "Activate the tool and the skill is ready"},
		{"and waits", "Activate the tool and the skill waits"},
		{"and works", "Activate the tool and the skill works"},
		{"and explains", "Activate the tool and the skill explains the plan"},
		{"and compound subject", "Activate the tool and the skill and the venv are ready"},
		{"or is ready", "Activate the tool or the skill is ready"},
		{"then waits", "Activate the tool, then the skill waits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Helper\n\n" + tc.line + "\n\nDo things.\n"
			if err := rc13W4LoadErr(t, content); err != nil {
				t.Fatalf("LoadFile(%s): expected success, got %v", tc.name, err)
			}
		})
	}
}

func TestRC16CommaListStillRejects(t *testing.T) {
	// F-A11-1 guard: all 12 rc15 reject variants from Audit 11
	// (W3 probe) must keep rejecting — the wider carve-out must
	// not open any of them.
	for _, tc := range []struct {
		name string
		line string
	}{
		{"headline-1", "Activate the tool, the skill"},
		{"headline-2", "Activate the tool, the skill, and the venv"},
		{"venv-first", "Activate the venv, the skill"},
		{"skill-first", "Activate the skill, the tool"},
		{"three-list-skill-last", "Activate the tool, the venv, the skill"},
		{"trailing-period", "Activate the tool, the skill."},
		{"trailing-bang", "Activate the tool, the skill!"},
		{"plural", "Activate the tool, the skills"},
		{"other-skill", "Activate the tool, the other skill"},
		{"skill-for-beginners", "Activate the tool, the skill for beginners"},
		{"trailing-question", "Activate the tool, the skill?"},
		{"list-continues-after-skill", "Activate the tool, the skill, the venv"},
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

func TestRC16NewClauseStillRejects(t *testing.T) {
	// F-A11-1 guard: shapes the wider carve-out must NOT open.
	// A compound subject with no verb after it is still a list of
	// objects; a noun-noun compound ("the skill tools") and a
	// preposition-like participle ("including", "during") still
	// modify the noun; and a skill in the direct-object position
	// — immediately after "activate", before any separator — is
	// the object of "activate" whatever follows it ("Activate the
	// skill is ready": the verb after it is a relative clause, and
	// if the skill were not the object then "activate" would be
	// bare, which is itself a directive).
	for _, tc := range []struct {
		name string
		line string
	}{
		{"compound, no verb", "Activate the tool, the skill and the venv"},
		{"compound, comma list, no verb", "Activate the tool, the skill and the venv, the tool"},
		{"compound, preposition", "Activate the tool, the skill and the venv for beginners"},
		{"noun compound tools", "Activate the tool, the skill tools"},
		{"noun compound settings", "Activate the tool, the skill settings"},
		{"noun compound items", "Activate the tool, the skill items"},
		{"during phrase", "Activate the tool, the skill during the meeting"},
		{"including extends list", "Activate the tool, the skill including the venv"},
		{"direct object, is ready", "Activate the skill is ready"},
		{"direct object, runs the tests", "Activate the skill runs the tests"},
		{"direct object, waits", "Activate the skill waits"},
		{"and path, no verb", "Activate the tool and the skill and the venv"},
		{"and path, preposition", "Activate the tool and the skill for beginners"},
		{"singular base form is a noun", "Activate the tool, the skill work"},
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
