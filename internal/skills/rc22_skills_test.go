// rc22 regression tests for Audit 16 finding F-A16-4 (P2) and the
// Audit 13 §7 residual D4:
//
// F-A16-4 (fail-closed, over-blocking): `neverVerbWords` listed
// "free" and "slow" alongside "fast" and "busy", so `isVerbShapedAt`
// and `isVerbShaped` returned false for them whatever followed —
// "free the plan", "free users", "slow the rollout" and "slow users"
// all rejected, although "free" and "slow" are genuine transitive
// verbs there ("free the plan", "slow the rollout"). The fix gives
// them their own class, `transitiveOnlyVerbs`: a verb reading only
// where an object follows (an article, a pronoun, or a bare plural
// object), never bare at a clause end (transitive-only: no
// intransitive reading), never as an adjective inside the subject
// noun phrase ("the skill and the free tools" still rejects, the
// rc20 guard), and never before a participle ("the free running
// tests" is still a modifier, the rc20 F-A15-3 guard). "fast" and
// "busy" stay in `neverVerbWords`: to fast is intransitive, to busy
// takes a reflexive, not a bare plural — so "fast the plan" and
// "busy the plan" still reject.
//
// D4 (Audit 13 §7): "the skill and the venv running tests" rejects
// vs "running the tests" loads — covered by F-A13-2; the
// article/bare-plural asymmetry is the bug. Audit 16 §3.2 could not
// verify D4 because the "D4" label is nowhere in the rc21 tree (the
// auditor looked for AUDIT13 in ~/workspace/orion-audit/; the file
// is ~/workspace/orion-go/AUDIT13_2026-11-14.md). The shape itself
// has loaded since rc18 (TestRC18GerundParticipleClausesLoad,
// "compound running tests"); this file pins it under the D4 label
// so the record is verifiable in-tree.
//
// Negative control: these tests were written FIRST and run against
// the unmodified rc21 code, where every "must load" F-A16-4 case
// FAILS (the file rejects) except the two compound article cases,
// which already loaded on rc21 for the wrong reason (the plural
// widening reads the trailing "plan"/"rollout" as the verb), and
// the D4 cases, which already loaded on rc21 for the right reason.
// Every "must reject" case PASSES on rc21 and must keep passing
// after the fix.
package skills

import (
	"strings"
	"testing"
)

func TestRC22FreeSlowVerbObjectLoad(t *testing.T) {
	// F-A16-4: "free" and "slow" are genuine transitive verbs when
	// an object follows — an article + noun, a pronoun, or a bare
	// plural (guarded or not). Plural, compound and singular
	// subjects all load: a listed transitive verb + object loads
	// after a singular subject too ("the skill serve the team",
	// "the skill dance users" already load on rc21).
	for _, tc := range []struct {
		name string
		line string
	}{
		// Article + noun object (auditor probe shapes).
		{"plural free the plan", "Activate the tool, the skills free the plan"},
		{"compound free the plan", "Activate the tool, the skill and the venv free the plan"},
		{"singular free the plan", "Activate the tool, the skill free the plan"},
		{"plural slow the rollout", "Activate the tool, the skills slow the rollout"},
		{"compound slow the rollout", "Activate the tool, the skill and the venv slow the rollout"},
		{"singular slow the rollout", "Activate the tool, the skill slow the rollout"},
		// Bare plural object (auditor probe shapes).
		{"plural free users", "Activate the tool, the skills free users"},
		{"compound free users", "Activate the tool, the skill and the venv free users"},
		{"singular free users", "Activate the tool, the skill free users"},
		{"plural slow users", "Activate the tool, the skills slow users"},
		{"compound slow users", "Activate the tool, the skill and the venv slow users"},
		{"singular slow users", "Activate the tool, the skill slow users"},
		// Bare plural object that is not a guarded noun.
		{"plural free guides", "Activate the tool, the skills free guides"},
		{"plural slow guides", "Activate the tool, the skills slow guides"},
		// Pronoun object.
		{"plural free you", "Activate the tool, the skills free you"},
		{"plural slow you", "Activate the tool, the skills slow you"},
		// Guarded plural object with a COMPLETE subject: the
		// subject is "the skill and the venv" and "free" is its
		// predicate verb, so "free tools" is verb + object — the
		// counterpart of the rc20 guard "the skill and the free
		// tools", where "free" sits inside the subject.
		{"compound venv free tools", "Activate the tool, the skill and the venv free tools"},
		{"compound venv slow tools", "Activate the tool, the skill and the venv slow tools"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Helper\n\n" + tc.line + "\n\nDo things.\n"
			if err := rc13W4LoadErr(t, content); err != nil {
				t.Fatalf("LoadFile(%s): expected success, got %v", tc.name, err)
			}
		})
	}
}

func TestRC22FreeSlowBareStillReject(t *testing.T) {
	// F-A16-4 controls: the clause-final bare form still rejects
	// (transitive-only verbs have no intransitive reading), "busy"
	// and "fast" still reject with or without an object (they stay
	// in neverVerbWords), "free"/"slow" inside the subject noun
	// phrase still reject (adjective + noun, the rc20 guards), and
	// "free"/"slow" before a participle, adverb, preposition or
	// bare singular noun still reject (no object follows the verb).
	for _, tc := range []struct {
		name string
		line string
	}{
		// Clause-final bare form (rc20:163-165 pins "free").
		{"plural free", "Activate the tool, the skills free"},
		{"plural slow", "Activate the tool, the skills slow"},
		{"singular free", "Activate the tool, the skill free"},
		{"singular slow", "Activate the tool, the skill slow"},
		{"compound venv free", "Activate the tool, the skill and the venv free"},
		{"compound venv slow", "Activate the tool, the skill and the venv slow"},
		// "busy"/"fast" with an object still reject. Only the
		// plural-subject forms pin this: a compound subject +
		// article + bare singular noun ("…the skill and the venv
		// busy the plan") already loads on rc21 for the wrong
		// reason — the F-A14-1 plural widening reads the trailing
		// "plan" as the verb — so it cannot pin "busy" at all.
		{"plural busy the plan", "Activate the tool, the skills busy the plan"},
		{"plural fast the plan", "Activate the tool, the skills fast the plan"},
		{"plural busy users", "Activate the tool, the skills busy users"},
		{"plural fast users", "Activate the tool, the skills fast users"},
		// Adjective inside the subject noun phrase (rc20 guard:
		// "compound free tools" in TestRC20GuardsStillReject).
		{"compound free tools", "Activate the tool, the skill and the free tools"},
		{"compound free users", "Activate the tool, the skill and the free users"},
		{"compound slow tools", "Activate the tool, the skill and the slow tools"},
		{"compound slow users", "Activate the tool, the skill and the slow users"},
		// Before a participle: a modifier, not a clause head
		// (rc20 F-A15-3 guard; "free"/"slow" never take a
		// gerund/participle complement).
		{"compound free running tests", "Activate the tool, the skill and the free running tests"},
		{"compound slow running tests", "Activate the tool, the skill and the slow running tests"},
		{"plural free running tests", "Activate the tool, the skills free running tests"},
		{"plural slow running tests", "Activate the tool, the skills slow running tests"},
		// No object follows: adverb, preposition, bare singular.
		{"plural free quickly", "Activate the tool, the skills free quickly"},
		{"plural slow quickly", "Activate the tool, the skills slow quickly"},
		{"plural free for beginners", "Activate the tool, the skills free for beginners"},
		{"plural slow for beginners", "Activate the tool, the skills slow for beginners"},
		{"plural free plan", "Activate the tool, the skills free plan"},
		{"plural slow plan", "Activate the tool, the skills slow plan"},
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

func TestRC22Audit13D4StillLoads(t *testing.T) {
	// Audit 13 §7 D4: "the skill and the venv running tests"
	// rejects on rc17 vs "running the tests" loads — covered by
	// F-A13-2; the article/bare-plural asymmetry is the bug. The
	// rc18 fix (participle + bare plural object is a verb phrase)
	// closed it, and the shape has loaded since (rc18 "compound
	// running tests", rc19/rc20 guards). Pinned here under the D4
	// label, which Audit 16 §3.2 could not find anywhere in the
	// rc21 tree.
	for _, tc := range []struct {
		name string
		line string
	}{
		{"D4 venv running tests", "Activate the tool, the skill and the venv running tests"},
		{"D4 venv running the tests", "Activate the tool, the skill and the venv running the tests"},
		{"D4 venv waiting tests", "Activate the tool, the skill and the venv waiting tests"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Helper\n\n" + tc.line + "\n\nDo things.\n"
			if err := rc13W4LoadErr(t, content); err != nil {
				t.Fatalf("LoadFile(%s): expected success, got %v", tc.name, err)
			}
		})
	}
}
