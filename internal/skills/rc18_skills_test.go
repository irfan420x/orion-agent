// rc18 regression tests for Audit 13 findings F-A13-1, F-A13-2 and
// F-A13-3 (all P2):
//
// F-A13-1 (fail-open, introduced by rc17): the new context-aware
// clauseVerbAt predicate never required the candidate itself to be
// verb-shaped — its following-word rules looked only at what
// follows the candidate. So a bare noun at a clause end after a
// plural/compound subject ("the venv documentation"), a noun
// followed by a preposition/subordinator/adverb ("documentation for
// beginners", "documentation that helps", "documentation quickly")
// or a noun followed by a bare plural ("reference tools") was read
// as a verb phrase, and the line loaded although it rejected on
// rc16. The fix requires the candidate to be verb-shaped before
// any following-word rule fires; "that"/"which"/"who" after a noun
// attaches a relative clause, not a subordinate clause; the
// bare-plural rule does not fire for a guarded noun; and the
// clause-final plural rule does not accept a bare word with no
// verb morphology (baseFormVerbs carries the base-form verbs —
// "dance" and "serve" join it for F-A12-2).
//
// F-A13-2 (fail-closed, regression vs rc16): the participle-after
// rule rejected verb + gerund/participle clauses — "continue /
// stop / remain" are listed base-form verbs that legitimately take
// a gerund/participle complement, and a participle with a bare
// plural object ("running tests") is a verb phrase, not a
// modifier. The fix exempts listed base-form verbs before a
// participle and admits participle + bare plural object (or
// article) as verb + object. In the compound scan a participle is
// a predicate only once a noun head has been seen ("the venv
// waiting tests"); while the subject noun phrase is still open it
// is an adjective ("the running tests" still rejects).
//
// F-A13-3 (fail-closed, regression vs rc16): the aux-after rule
// rejected noun-head-subject + copula clauses — "the skill files
// are ready" loaded on rc16 (for the wrong reason: "files" read as
// the verb) and rejects on rc17, although the clause exists with
// the aux as its verb. The fix: when the word after the candidate
// is an aux (or any other verb — "files run"), the clause exists
// with that word as its verb and the candidate as its subject. A
// guarded noun ("tools") still rejects, since it returns earlier.
//
// Negative control: these tests were written FIRST and run against
// the unmodified rc17 code, where every "must load" F-A13-2/F-A13-3
// case FAILS (the file is rejected) and every "must reject" F-A13-1
// case FAILS (the file loads). The guard cases document behaviour
// that already held on rc17 and must keep holding after the fix.
package skills

import (
	"strings"
	"testing"
)

func TestRC18NounPhrasesStillReject(t *testing.T) {
	// F-A13-1: a noun phrase after the skill is a list object of
	// "activate", not a new clause — on the comma path and the
	// compound path. The candidate is a bare noun ("documentation",
	// "handbook", "manual", "reference", "happy"), so no
	// following-word rule may read it as a verb phrase.
	for _, tc := range []struct {
		name string
		line string
	}{
		// (a) Clause-final plural rule: a bare word with no
		// verb morphology at a clause end is a noun.
		{"compound documentation", "Activate the tool, the skill and the venv documentation"},
		{"compound handbook", "Activate the tool, the skill and the venv handbook"},
		{"compound manual", "Activate the tool, the skill and the venv manual"},
		{"compound library", "Activate the tool, the skill and the venv library"},
		{"compound overview", "Activate the tool, the skill and the venv overview"},
		{"compound framework", "Activate the tool, the skill and the venv framework"},
		{"plural documentation", "Activate the tool, the skills documentation"},
		{"plural handbook", "Activate the tool, the skills handbook"},
		{"plural summary", "Activate the tool, the skills summary"},
		{"compound other venv documentation", "Activate the tool, the skill and the other venv documentation"},
		// (b) Preposition / subordinator / adverb after a noun.
		{"documentation for beginners", "Activate the tool, the skill documentation for beginners"},
		{"documentation with examples", "Activate the tool, the skill documentation with examples"},
		{"documentation when ready", "Activate the tool, the skill documentation when ready"},
		{"documentation quickly", "Activate the tool, the skill documentation quickly"},
		{"documentation that helps", "Activate the tool, the skill documentation that helps"},
		{"handbook which explains", "Activate the tool, the skill handbook which explains"},
		{"compound documentation that helps", "Activate the tool, the skill and the venv documentation that helps"},
		{"plural documentation when ready", "Activate the tool, the skills documentation when ready"},
		{"overview for beginners", "Activate the tool, the skill overview for beginners"},
		{"manual that explains", "Activate the tool, the skill manual that explains"},
		{"library with examples", "Activate the tool, the skill library with examples"},
		{"framework when ready", "Activate the tool, the skill framework when ready"},
		{"summary quickly", "Activate the tool, the skill summary quickly"},
		// (c) Bare-plural rule: a noun followed by a guarded
		// noun ("tools", "items", "settings") is a noun
		// compound, not a verb with a bare plural object.
		{"reference tools", "Activate the tool, the skill reference tools"},
		{"documentation tools", "Activate the tool, the skill documentation tools"},
		{"compound documentation tools", "Activate the tool, the skill and the venv documentation tools"},
		{"compound documentation settings", "Activate the tool, the skill and the venv documentation settings"},
		{"happy tools", "Activate the tool, the skill happy tools"},
		{"compound reference items", "Activate the tool, the skill and the venv reference items"},
		{"documentation settings", "Activate the tool, the skill documentation settings"},
		{"handbook tools", "Activate the tool, the skill handbook tools"},
		{"plural documentation tools", "Activate the tool, the skills documentation tools"},
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

func TestRC18GerundParticipleClausesLoad(t *testing.T) {
	// F-A13-2: a listed base-form verb takes a gerund/participle
	// complement, and a participle with a bare plural object (or
	// an article) is a verb phrase — so the skill is the subject
	// of a new clause, not an object of "activate".
	for _, tc := range []struct {
		name string
		line string
	}{
		{"compound continue learning", "Activate the tool, the skill and the venv continue learning"},
		{"compound continue running", "Activate the tool, the skill and the venv continue running"},
		{"plural continue learning", "Activate the tool, the skills continue learning"},
		{"plural continue working", "Activate the tool, the skills continue working"},
		{"plural stop waiting", "Activate the tool, the skills stop waiting"},
		{"plural stop learning", "Activate the tool, the skills stop learning"},
		{"compound remain waiting", "Activate the tool, the skill and the venv remain waiting"},
		{"compound remain seated", "Activate the tool, the skill and the venv remain seated"},
		{"files running tests", "Activate the tool, the skill files running tests"},
		{"compound waiting tests", "Activate the tool, the skill and the venv waiting tests"},
		{"files running the tests", "Activate the tool, the skill files running the tests"},
		{"compound running tests", "Activate the tool, the skill and the venv running tests"},
		{"compound continue working", "Activate the tool, the skill and the venv continue working"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Helper\n\n" + tc.line + "\n\nDo things.\n"
			if err := rc13W4LoadErr(t, content); err != nil {
				t.Fatalf("LoadFile(%s): expected success, got %v", tc.name, err)
			}
		})
	}
}

func TestRC18NounHeadAuxClausesLoad(t *testing.T) {
	// F-A13-3: when the word after the candidate is an aux, the
	// clause exists with the aux as its verb and the candidate
	// (a noun head) as its subject — so the skill is the subject
	// of a new clause, not an object of "activate".
	for _, tc := range []struct {
		name string
		line string
	}{
		{"files are ready", "Activate the tool, the skill files are ready"},
		{"notes are ready", "Activate the tool, the skill notes are ready"},
		{"guides are ready", "Activate the tool, the skill guides are ready"},
		{"reports are ready", "Activate the tool, the skill reports are ready"},
		{"training is ready", "Activate the tool, the skill training is ready"},
		{"files will help you", "Activate the tool, the skill files will help you"},
		{"files have finished", "Activate the tool, the skill files have finished"},
		{"documentation is ready", "Activate the tool, the skill documentation is ready"},
		{"handbook will help you", "Activate the tool, the skill handbook will help you"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Helper\n\n" + tc.line + "\n\nDo things.\n"
			if err := rc13W4LoadErr(t, content); err != nil {
				t.Fatalf("LoadFile(%s): expected success, got %v", tc.name, err)
			}
		})
	}
}

func TestRC18FollowingVerbClausesLoad(t *testing.T) {
	// F-A13-3, same mechanism as the aux rule: when the word
	// after the candidate is any other verb ("run", "help"), the
	// clause exists with that word as its verb and the candidate
	// as its subject. "The skill files run" loaded on rc16 (for
	// the wrong reason: "files" read as the verb) and rejects on
	// rc17; it loads here for the right reason.
	for _, tc := range []struct {
		name string
		line string
	}{
		{"files run", "Activate the tool, the skill files run"},
		{"documentation runs", "Activate the tool, the skill documentation runs"},
		{"files run the tests", "Activate the tool, the skill files run the tests"},
		{"files help", "Activate the tool, the skill files help"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Helper\n\n" + tc.line + "\n\nDo things.\n"
			if err := rc13W4LoadErr(t, content); err != nil {
				t.Fatalf("LoadFile(%s): expected success, got %v", tc.name, err)
			}
		})
	}
}

func TestRC18GuardsStillLoad(t *testing.T) {
	// Guard: the F-A11-1 must-load shapes, the F-A12-2 base-form
	// cases and the Audit 12 pin-down still load after the fix.
	for _, tc := range []struct {
		name string
		line string
	}{
		{"plural dance (F-A12-2)", "Activate the tool, the skills dance"},
		{"compound dance (F-A12-2)", "Activate the tool, the skill and the venv dance"},
		{"compound serve the team (F-A12-2)", "Activate the tool, the skill and the venv serve the team"},
		{"plural serve users (F-A12-2)", "Activate the tool, the skills serve users"},
		{"compound dance users (pin-down)", "Activate the tool, the skill and the venv dance users"},
		{"compound serve users (pin-down)", "Activate the tool, the skill and the venv serve users"},
		{"files the report", "Activate the tool, the skill files the report"},
		{"training the users", "Activate the tool, the skill training the users"},
		{"skill running tests", "Activate the tool, the skill running tests"},
		{"compound run tests", "Activate the tool, the skill and the venv run tests"},
		{"compound running the tests", "Activate the tool, the skill and the venv running the tests"},
		{"skill waits", "Activate the tool, the skill waits"},
		{"skill starts", "Activate the tool, the skill starts"},
		{"compound are ready", "Activate the tool, the skill and the venv are ready"},
		{"plural wait", "Activate the tool, the skills wait"},
		{"compound wait", "Activate the tool, the skill and the venv wait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Helper\n\n" + tc.line + "\n\nDo things.\n"
			if err := rc13W4LoadErr(t, content); err != nil {
				t.Fatalf("LoadFile(%s): expected success, got %v", tc.name, err)
			}
		})
	}
}

func TestRC18GuardsStillReject(t *testing.T) {
	// Guard: the F-A12-1 flip cases and the rc17 compound
	// adjectives still reject, and a guarded noun before an aux
	// ("tools are ready") is still a list object.
	for _, tc := range []struct {
		name string
		line string
	}{
		{"tools are ready (guarded noun + aux)", "Activate the tool, the skill tools are ready"},
		{"compound running tests", "Activate the tool, the skill and the running tests"},
		{"compound running tools", "Activate the tool, the skill and the running tools"},
		{"compound venv guides", "Activate the tool, the skill and the venv guides"},
		{"skill files (clause end)", "Activate the tool, the skill files"},
		{"compound no verb", "Activate the tool, the skill and the venv"},
		{"skill tools (noun compound)", "Activate the tool, the skill tools"},
		{"skill work (singular base form)", "Activate the tool, the skill work"},
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
