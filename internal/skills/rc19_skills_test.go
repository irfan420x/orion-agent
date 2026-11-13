// rc19 regression tests for Audit 14 findings F-A14-1, F-A14-2, F-A14-3
// and F-A14-4 (all P2):
//
// F-A14-1 (fail-closed, regression vs rc17): rc18 closed the verb
// lists in both directions — the clause-final plural rule and the
// following-word rules required `baseFormVerbs` membership (or a
// verb suffix / object-region verb), so an unlisted base-form verb
// after a plural or compound subject lost every position: "…the
// skills build/design/develop/study/watch/code/sing/jump/paint/draw/
// cook/drive/fly/swim/sleep/walk/eat/laugh [the plan]", "…the skill
// and the venv build", "…the skills build guides/you/quickly/for
// beginners/when ready" all rejected on rc18 although they loaded
// on rc17 (40 shapes). The fix is shape-based, not more list
// entries: after a plural or compound subject a bare word in verb
// position is a verb by shape (new helper `isVerbShapedAt`), unless
// it is a never-verb word (see F-A14-4). A singular subject with a
// base-form verb is still ungrammatical and still rejects.
//
// F-A14-2 (fail-closed, regression vs rc17): the bare-plural rule
// fired before any verb-shape check and exempted only
// `baseFormVerbs`, so genuine verbs before a guarded plural object
// rejected: "…the skill makes/creates/runs/tests/checks/helps/
// explains/provides users/tools/meetings/settings/examples",
// "…the skill trained users / files tools / notes settings /
// guides items", "…the skills make/create/use/run/help
// users/tools/settings/meetings", "…the skill running tools" (21
// shapes). The fix exempts verb-shaped candidates from the
// guarded-plural rejection (new helper `verbTakesGuardedObject`):
// an -s/-ed verb or an object-region / listed base-form verb takes
// the guarded noun as its object. A participle in subject position
// ("running tools") still does not — -ing is never a finite verb
// form — and a verb that is also a common adjective ("open") does
// not either ("the open tools" is an adjective + noun).
//
// F-A14-3 (fail-open, regression vs rc17): `compoundSubjectStartsClause`
// set `sawNoun=true` for any word that is not an article, a subject
// modifier or a clause adverb — including words `clauseVerbAt` had
// just early-returned as non-verbs (the adjectives in
// `baseFormNonVerbs`: new/ready/available/online/old/simple/easy/
// active; -ly adverbs: quickly) — so the participle+bare-plural
// branch read the participle as the predicate instead of a
// modifier: "…the skill and the new/ready/quickly/available/online/
// old/simple/easy/active running/waiting tests" loaded on rc18
// although they rejected on rc17 (12 shapes). The fix: only a
// genuine noun head sets `sawNoun` (new helper `isNounHead`).
//
// F-A14-4 (fail-open, pre-existing): pure -s/-ing nouns were read
// as verbs by suffix — `hasVerbSuffix` made never-verbs verb-shaped,
// so `isVerbShaped` passed every rule: "…the skill analysis /
// status / news / physics / morning / evening / mathematics /
// economics / chaos / virus / campus", in clause-final, article,
// preposition, subordinator, bare-plural and pronoun positions (31
// shapes, loading on both rc17 and rc18). The fix is a never-verb
// word list (`neverVerbWords`): a word that is only ever a noun (or
// an adjective) is never verb-shaped, so no following-word rule and
// no clause-final rule can read it as a verb. It can still be the
// subject of a clause ("…the skill analysis is ready" still loads
// — the aux is the verb, F-A13-3).
//
// Negative control: these tests were written FIRST and run against
// the unmodified rc18 code, where every "must load" F-A14-1/F-A14-2
// case FAILS (the file is rejected) and every "must reject"
// F-A14-3/F-A14-4 case FAILS (the file loads). The guard cases
// document behaviour that already held on rc18 and must keep
// holding after the fix.
package skills

import (
	"strings"
	"testing"
)

func TestRC19UnlistedBaseFormVerbsLoad(t *testing.T) {
	// F-A14-1: an unlisted base-form verb after a plural or
	// compound subject is a verb by shape, in every position:
	// clause-final, before an article, a bare plural, a pronoun,
	// an adverb, a preposition, or a subordinator, and after
	// "or" as well as "and".
	for _, tc := range []struct {
		name string
		line string
	}{
		// (a) Clause-final, plural subject.
		{"plural build", "Activate the tool, the skills build"},
		{"plural design", "Activate the tool, the skills design"},
		{"plural develop", "Activate the tool, the skills develop"},
		{"plural study", "Activate the tool, the skills study"},
		{"plural watch", "Activate the tool, the skills watch"},
		{"plural code", "Activate the tool, the skills code"},
		{"plural sing", "Activate the tool, the skills sing"},
		{"plural jump", "Activate the tool, the skills jump"},
		{"plural paint", "Activate the tool, the skills paint"},
		{"plural draw", "Activate the tool, the skills draw"},
		{"plural cook", "Activate the tool, the skills cook"},
		{"plural drive", "Activate the tool, the skills drive"},
		{"plural fly", "Activate the tool, the skills fly"},
		{"plural swim", "Activate the tool, the skills swim"},
		{"plural sleep", "Activate the tool, the skills sleep"},
		{"plural walk", "Activate the tool, the skills walk"},
		{"plural eat", "Activate the tool, the skills eat"},
		{"plural laugh", "Activate the tool, the skills laugh"},
		// (b) Clause-final, compound subject.
		{"compound build", "Activate the tool, the skill and the venv build"},
		{"compound design", "Activate the tool, the skill and the venv design"},
		{"compound develop", "Activate the tool, the skill and the venv develop"},
		{"compound study", "Activate the tool, the skill and the venv study"},
		// (c) Before an article (transitive verb + object).
		{"plural build the plan", "Activate the tool, the skills build the plan"},
		{"plural design the plan", "Activate the tool, the skills design the plan"},
		{"plural develop the plan", "Activate the tool, the skills develop the plan"},
		{"plural study the plan", "Activate the tool, the skills study the plan"},
		{"compound build the plan", "Activate the tool, the skill and the venv build the plan"},
		// (d) Before a bare plural object.
		{"plural build guides", "Activate the tool, the skills build guides"},
		{"plural build reports", "Activate the tool, the skills build reports"},
		{"plural design answers", "Activate the tool, the skills design answers"},
		// (e) Before a pronoun object.
		{"plural build you", "Activate the tool, the skills build you"},
		{"plural study it", "Activate the tool, the skills study it"},
		// (f) Before an adverb.
		{"plural build quickly", "Activate the tool, the skills build quickly"},
		{"plural design slowly", "Activate the tool, the skills design slowly"},
		// (g) Before a preposition.
		{"plural build for beginners", "Activate the tool, the skills build for beginners"},
		{"plural design with examples", "Activate the tool, the skills design with examples"},
		// (h) Before a subordinator.
		{"plural build when ready", "Activate the tool, the skills build when ready"},
		{"plural design that helps", "Activate the tool, the skills design that helps"},
		// (i) After "or" as well as "and".
		{"or build", "Activate the tool, the skill or the venv build"},
		{"or build the plan", "Activate the tool, the skill or the venv build the plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Helper\n\n" + tc.line + "\n\nDo things.\n"
			if err := rc13W4LoadErr(t, content); err != nil {
				t.Fatalf("LoadFile(%s): expected success, got %v", tc.name, err)
			}
		})
	}
}

func TestRC19GuardedPluralVerbsLoad(t *testing.T) {
	// F-A14-2: a genuine verb before a guarded plural object
	// ("tools", "items", "settings", "users", "meetings",
	// "examples") takes it as its object — the guarded-plural
	// rule must not fire before the verb-shape check.
	for _, tc := range []struct {
		name string
		line string
	}{
		// (a) -s verbs + guarded plural (singular subject).
		{"makes users", "Activate the tool, the skill makes users"},
		{"creates tools", "Activate the tool, the skill creates tools"},
		{"runs meetings", "Activate the tool, the skill runs meetings"},
		{"tests users", "Activate the tool, the skill tests users"},
		{"checks settings", "Activate the tool, the skill checks settings"},
		{"helps users", "Activate the tool, the skill helps users"},
		{"explains examples", "Activate the tool, the skill explains examples"},
		{"provides examples", "Activate the tool, the skill provides examples"},
		// (b) -ed verbs + guarded plural.
		{"trained users", "Activate the tool, the skill trained users"},
		// (c) Ambiguous -s nouns as verbs + guarded plural.
		{"files tools", "Activate the tool, the skill files tools"},
		{"notes settings", "Activate the tool, the skill notes settings"},
		{"guides items", "Activate the tool, the skill guides items"},
		// (d) Base-form (object-region) verbs + guarded plural.
		{"plural make users", "Activate the tool, the skills make users"},
		{"plural create tools", "Activate the tool, the skills create tools"},
		{"plural use settings", "Activate the tool, the skills use settings"},
		{"plural run meetings", "Activate the tool, the skills run meetings"},
		{"plural help users", "Activate the tool, the skills help users"},
		{"plural make tools", "Activate the tool, the skills make tools"},
		{"compound make users", "Activate the tool, the skill and the venv make users"},
		{"compound create tools", "Activate the tool, the skill and the venv create tools"},
		// (e) Participle + guarded plural in predicate position.
		{"running tools", "Activate the tool, the skill running tools"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Helper\n\n" + tc.line + "\n\nDo things.\n"
			if err := rc13W4LoadErr(t, content); err != nil {
				t.Fatalf("LoadFile(%s): expected success, got %v", tc.name, err)
			}
		})
	}
}

func TestRC19AdjectiveNounPhrasesStillReject(t *testing.T) {
	// F-A14-3: an adjective or adverb inside the compound subject
	// is a subject word, not a noun head — it must not complete
	// the subject noun phrase, so the participle after it is still
	// a modifier ("the new running tests" is a noun phrase, not a
	// clause).
	for _, tc := range []struct {
		name string
		line string
	}{
		{"new running tests", "Activate the tool, the skill and the new running tests"},
		{"new waiting tests", "Activate the tool, the skill and the new waiting tests"},
		{"ready running tests", "Activate the tool, the skill and the ready running tests"},
		{"ready waiting tests", "Activate the tool, the skill and the ready waiting tests"},
		{"quickly running tests", "Activate the tool, the skill and the quickly running tests"},
		{"available waiting tests", "Activate the tool, the skill and the available waiting tests"},
		{"online running tests", "Activate the tool, the skill and the online running tests"},
		{"old waiting tests", "Activate the tool, the skill and the old waiting tests"},
		{"simple running tests", "Activate the tool, the skill and the simple running tests"},
		{"easy waiting tests", "Activate the tool, the skill and the easy waiting tests"},
		{"active running tests", "Activate the tool, the skill and the active running tests"},
		{"stacked new running tests", "Activate the tool, the skill and the venv and the new running tests"},
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

func TestRC19NeverVerbNounsStillReject(t *testing.T) {
	// F-A14-4: a word that is only ever a noun (or an adjective)
	// is never verb-shaped, whatever its suffix — so no
	// following-word rule and no clause-final rule can read it as
	// a verb phrase.
	for _, tc := range []struct {
		name string
		line string
	}{
		// (a) Clause-final, singular subject.
		{"analysis", "Activate the tool, the skill analysis"},
		{"status", "Activate the tool, the skill status"},
		{"news", "Activate the tool, the skill news"},
		{"physics", "Activate the tool, the skill physics"},
		{"morning", "Activate the tool, the skill morning"},
		{"evening", "Activate the tool, the skill evening"},
		{"mathematics", "Activate the tool, the skill mathematics"},
		{"economics", "Activate the tool, the skill economics"},
		{"chaos", "Activate the tool, the skill chaos"},
		{"virus", "Activate the tool, the skill virus"},
		{"campus", "Activate the tool, the skill campus"},
		// (b) Clause-final, plural subject.
		{"plural analysis", "Activate the tool, the skills analysis"},
		{"plural status", "Activate the tool, the skills status"},
		{"plural news", "Activate the tool, the skills news"},
		{"plural morning", "Activate the tool, the skills morning"},
		{"plural evening", "Activate the tool, the skills evening"},
		// (c) Clause-final, compound subject.
		{"compound analysis", "Activate the tool, the skill and the venv analysis"},
		{"compound morning", "Activate the tool, the skill and the venv morning"},
		// (d) Before an article.
		{"analysis the plan", "Activate the tool, the skill analysis the plan"},
		{"morning the plan", "Activate the tool, the skill morning the plan"},
		{"status the plan", "Activate the tool, the skill status the plan"},
		// (e) Before a preposition.
		{"analysis for beginners", "Activate the tool, the skill analysis for beginners"},
		{"morning for beginners", "Activate the tool, the skill morning for beginners"},
		{"news for beginners", "Activate the tool, the skill news for beginners"},
		// (f) Before a subordinator.
		{"analysis that helps", "Activate the tool, the skill analysis that helps"},
		{"morning that helps", "Activate the tool, the skill morning that helps"},
		{"status which explains", "Activate the tool, the skill status which explains"},
		// (g) Before a bare plural.
		{"analysis reports", "Activate the tool, the skill analysis reports"},
		{"morning guides", "Activate the tool, the skill morning guides"},
		{"analysis files", "Activate the tool, the skill analysis files"},
		// (h) Before a pronoun.
		{"analysis you", "Activate the tool, the skill analysis you"},
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

func TestRC19GuardsStillLoad(t *testing.T) {
	// Guard: the behaviours Audit 14 verified as correct on rc18
	// must keep holding after the fix — the F-A13-2 participle
	// carve-outs, the F-A13-3 aux/following-verb rules, the
	// F-A14-1/F-A14-2 controls, and the pre-existing boundaries.
	for _, tc := range []struct {
		name string
		line string
	}{
		// F-A13-2: participle + object is a verb phrase.
		{"build waiting tests", "Activate the tool, the skills build waiting tests"},
		{"build running the tests", "Activate the tool, the skills build running the tests"},
		{"continue running tools", "Activate the tool, the skills continue running tools"},
		{"stop waiting", "Activate the tool, the skills stop waiting"},
		{"files running tests", "Activate the tool, the skill files running tests"},
		{"compound running tests", "Activate the tool, the skill and the venv running tests"},
		{"compound waiting tests", "Activate the tool, the skill and the venv waiting tests"},
		{"tool waiting tests", "Activate the tool, the skill and the tool waiting tests"},
		{"tools waiting tests", "Activate the tool, the skill and the tools waiting tests"},
		// F-A13-3: aux / following verb — the candidate is the subject.
		{"analysis is ready", "Activate the tool, the skill analysis is ready"},
		{"very is ready", "Activate the tool, the skill very is ready"},
		{"today is ready", "Activate the tool, the skill today is ready"},
		{"happy is ready", "Activate the tool, the skill happy is ready"},
		{"documentation is ready", "Activate the tool, the skill documentation is ready"},
		{"files run", "Activate the tool, the skill files run"},
		{"analysis runs", "Activate the tool, the skill analysis runs"},
		{"morning runs", "Activate the tool, the skill morning runs"},
		{"very runs", "Activate the tool, the skill very runs"},
		{"happy runs", "Activate the tool, the skill happy runs"},
		// F-A14-1/F-A14-2 controls (already loaded on rc18).
		{"plural dance", "Activate the tool, the skills dance"},
		{"plural wait", "Activate the tool, the skills wait"},
		{"plural work", "Activate the tool, the skills work"},
		{"plural serve users", "Activate the tool, the skills serve users"},
		{"compound serve the team", "Activate the tool, the skill and the venv serve the team"},
		{"plural serve tools", "Activate the tool, the skills serve tools"},
		{"plural dance settings", "Activate the tool, the skills dance settings"},
		{"plural wait users", "Activate the tool, the skills wait users"},
		{"plural make guides", "Activate the tool, the skills make guides"},
		{"makes guides", "Activate the tool, the skill makes guides"},
		{"creates reports", "Activate the tool, the skill creates reports"},
		{"running tests", "Activate the tool, the skill running tests"},
		{"compound dance users", "Activate the tool, the skill and the venv dance users"},
		{"files the report", "Activate the tool, the skill files the report"},
		{"training the users", "Activate the tool, the skill training the users"},
		{"skill running tests", "Activate the tool, the skill running tests"},
		{"skill running the tests", "Activate the tool, the skill running the tests"},
		{"skill waits", "Activate the tool, the skill waits"},
		{"skill starts", "Activate the tool, the skill starts"},
		{"skill then waits", "Activate the tool, the skill then waits"},
		{"compound are ready", "Activate the tool, the skill and the venv are ready"},
		{"compound wait", "Activate the tool, the skill and the venv wait"},
		{"open venv waits", "Activate the tool, the skill and the open venv waits"},
		{"tired venv running tests", "Activate the tool, the skill and the tired venv running tests"},
		{"open venv running tests", "Activate the tool, the skill and the open venv running tests"},
		{"running venv are ready", "Activate the tool, the skill and the running venv are ready"},
		{"explain that helps", "Activate the tool, the skills explain that helps"},
		{"reports that helps", "Activate the tool, the skill reports that helps"},
		{"skill setting (R3)", "Activate the tool, the skill setting"},
		{"running skill", "Activate the tool, the running skill"},
		{"open skill", "Activate the tool, the open skill"},
		{"using the skill", "Activate the tool using the skill"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Helper\n\n" + tc.line + "\n\nDo things.\n"
			if err := rc13W4LoadErr(t, content); err != nil {
				t.Fatalf("LoadFile(%s): expected success, got %v", tc.name, err)
			}
		})
	}
}

func TestRC19GuardsStillReject(t *testing.T) {
	// Guard: the rejections Audit 14 verified as correct on rc18
	// must keep holding after the fix — the rc18-correct
	// behaviours (singular base form, guarded noun compounds,
	// the open-tools noun phrase) and the pre-existing residuals.
	for _, tc := range []struct {
		name string
		line string
	}{
		// Singular subject + base-form verb is ungrammatical.
		{"singular build the plan", "Activate the tool, the skill build the plan"},
		{"singular design the plan", "Activate the tool, the skill design the plan"},
		// Participle with no object is a modifier (rc17/rc18).
		{"build waiting (no object)", "Activate the tool, the skills build waiting"},
		// Guarded noun compounds (F-A13-1).
		{"analysis tools", "Activate the tool, the skill analysis tools"},
		{"documentation tools", "Activate the tool, the skill documentation tools"},
		{"reference tools", "Activate the tool, the skill reference tools"},
		{"happy tools", "Activate the tool, the skill happy tools"},
		{"handbook tools", "Activate the tool, the skill handbook tools"},
		{"documentation settings", "Activate the tool, the skill documentation settings"},
		{"compound documentation tools", "Activate the tool, the skill and the venv documentation tools"},
		{"compound documentation settings", "Activate the tool, the skill and the venv documentation settings"},
		{"compound reference items", "Activate the tool, the skill and the venv reference items"},
		{"plural documentation tools", "Activate the tool, the skills documentation tools"},
		// Guarded noun + aux still rejects (F-A13-3 guard).
		{"tools are ready", "Activate the tool, the skill tools are ready"},
		// The open-tools noun phrase (Audit 13 §7 D2).
		{"open tools", "Activate the tool, the skill and the open tools"},
		// Noun-phrase compounds (F-A12-1 / F-A13-1).
		{"compound running tests (D3)", "Activate the tool, the skill and the running tests"},
		{"compound running tools", "Activate the tool, the skill and the running tools"},
		{"compound venv guides", "Activate the tool, the skill and the venv guides"},
		{"compound venv running tools", "Activate the tool, the skill and the venv running tools"},
		{"files running tools", "Activate the tool, the skill files running tools"},
		{"other running tests", "Activate the tool, the skill and the other running tests"},
		{"then running tests", "Activate the tool, the skill and the then running tests"},
		// Bare nouns at a clause end (F-A13-1).
		{"skills documentation", "Activate the tool, the skills documentation"},
		{"skills handbook", "Activate the tool, the skills handbook"},
		{"skills summary", "Activate the tool, the skills summary"},
		{"compound documentation", "Activate the tool, the skill and the venv documentation"},
		{"skill files", "Activate the tool, the skill files"},
		{"skill work (singular base form)", "Activate the tool, the skill work"},
		{"skill tools", "Activate the tool, the skill tools"},
		// Bare noun + following word (F-A13-1).
		{"documentation for beginners", "Activate the tool, the skill documentation for beginners"},
		{"documentation when ready", "Activate the tool, the skill documentation when ready"},
		{"documentation quickly", "Activate the tool, the skill documentation quickly"},
		{"documentation that helps", "Activate the tool, the skill documentation that helps"},
		{"documentation which explains", "Activate the tool, the skill documentation which explains"},
		// No verb at all / plain list objects.
		{"compound no verb", "Activate the tool, the skill and the venv"},
		{"list then", "Activate the tool, then the skill"},
		{"list and", "Activate the tool and the skill"},
		{"list comma", "Activate the tool, the skill"},
		// Unlisted / stacked adverbs (L6/L11 residuals).
		{"compound quickly", "Activate the tool, the skill and the venv quickly"},
		{"plural quickly", "Activate the tool, the skills quickly"},
		{"plural then quickly", "Activate the tool, the skills then quickly"},
		{"compound then quickly", "Activate the tool, the skill and the venv then quickly"},
		{"quickly waits", "Activate the tool, the skill quickly waits"},
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
