// rc20 regression tests for Audit 15 findings F-A15-1, F-A15-2, F-A15-3
// and F-A15-4 (all P2, all fail-open):
//
// F-A15-1 (fail-open, regression vs rc18): rc19's `isVerbShapedAt`
// re-opened noun phrases after plural/compound subjects — the
// plural widening returned true for ANY word not in
// `neverVerbWords`, so a bare noun or a bare adjective after a
// plural subject read as a verb by shape: "…the skills
// business/success/loss/class/glass" (pure -ss nouns, clause-final
// and following-word), "…the skills notebook/guidebook/keyboard/
// pencil" (bare nouns) and "…the skills hot/tall/young/red/cold/
// dark/wide/deep/soft/hard" (bare adjectives), "…the skills
// angry/sorry/afraid/alive/asleep/aware/able" (more adjectives),
// "…the skills free/busy" and "…the skill and the venv free/busy"
// (transitive-only verbs, no intransitive reading) — 52 shapes, all
// loading on rc19 and rejecting on rc18. The fix extends
// `neverVerbWords` with the never-verb classes the probe surface
// shows: bare never-verb adjectives, pure -ss nouns, pure -s nouns
// (series, species), a pure -ing noun (ceiling), pure -ed
// adjectives (rugged, wicked), and bare nouns
// (notebook/guidebook/keyboard/pencil). Genuine -ss verbs
// (boss/progress/process/address/access) and genuine -ing/-ed verb
// forms (feeling/meaning/flooring, building/training/learning,
// closed/trained/opened/finished) stay OUT of the list, so they
// keep loading.
//
// F-A15-2 (fail-open, regression vs rc18): `verbTakesGuardedObject`
// let suffix-bearing pure nouns/adjectives take guarded plurals —
// it excluded only `adjectiveVerbs` ("open") and -ing in subject
// position, then returned `isVerbShaped`, which is true for any
// -s/-ed/-ing suffix: "…the skill series/species tools/users/
// settings/items/meetings", "…the skill ceiling tools/items/users/
// meetings", "…the skill rugged/wicked tools/users" (21 shapes,
// loading on rc19, rejecting on rc18). The fix is the same
// `neverVerbWords` extension, now consulted by
// `verbTakesGuardedObject` before the suffix test. The audit also
// suggested a uniform -ed exclusion in subject position; that is
// DELIBERATELY NOT applied — a genuine -ed verb takes a guarded
// object for a compound subject ("…the skill and the venv
// closed/trained/opened/finished/tired/excited tools" load on both
// and are defensible, Audit 15 §6 O4), so the discriminator is the
// word's class (never-verb adjective vs genuine verb form), not
// its suffix.
//
// F-A15-3 (fail-open, pre-existing): the participle-with-object
// branch in `clauseVerbAt` never consulted the candidate's class —
// it returned true for any candidate followed by a participle and
// an object, so a never-verb adjective or adverb before the
// participle read as a clause head: "…the skill and the big/small/
// happy/sad/glad/good/bad/great/very/fast/free/busy/tired/hot/tall/
// young/red running tests", "…the skill and the big/very waiting
// tests", "…the skill big/very running tests", "…the skill and the
// slow/soft/dark running tests", "…the skill and the big running
// guides", "…the skill and the angry/sorry running tests" (29
// shapes, loading on both rc18 and rc19 — note `big` and `happy`
// are already IN `neverVerbWords` and still loaded here, because
// this branch never asked). The F-A14-3 `isNounHead` fix never
// reached this branch. The fix gates the branch: the candidate
// must be able to head the clause — a noun head (the subject of
// the participle phrase, "files running tests") or a verb (taking
// the participle phrase as a gerund object, "build waiting
// tests"). A never-verb word that is not a noun head ("big
// running tests") and a participle that is not itself a noun head
// ("tired running tests") can be neither. A bare word after a
// plural subject is a verb by shape (F-A14-1), so the plural path
// needs no noun/verb evidence beyond not being a never-verb word.
// `isNounHead` now also recognises a pure -ss noun ("class",
// "glass") as a singular noun head, so the O9 noun-head +
// predicate-participle shapes keep loading.
//
// F-A15-4 (fail-open, regression vs rc18): `adjectiveVerbs` listed
// only "open", so the other adjective-verbs took a guarded plural
// as their object: "…the skill and the close/set tools/users/
// meetings", "…the skill close/set tools/users", "…the skills
// close tools" (10 shapes, loading on rc19, rejecting on rc18,
// while "…the open tools" rejects on both). The rc19 comment's
// "only open is documented as an adjective" is wrong for `close`
// ("nearby tools") and `set` ("tools of the set"). The fix extends
// the class to {open, close, set} — the object-closer verbs that
// are also common adjectives in attributive position — and
// documents the class property. Suffixed forms ("opened",
// "closed", "sets") still take the object, because the list holds
// base forms only. Noun-verbs like "test"/"list" are NOT in the
// class: "test tools" is a noun+noun compound, not an adjective +
// noun phrase, so their verb reading wins (F-A14-2).
//
// Negative control: these tests were written FIRST and run against
// the unmodified rc19 code, where every "must reject" F-A15-1/2/3/4
// case FAILS (the file loads) and every guard case PASSES (the
// behaviour already held on rc19 and must keep holding after the
// fix). The bonus cases in TestRC20GuardsStillReject are
// pre-existing fail-open shapes (Audit 15 §6 O3/O16 and the -ed
// suffix residual) that this fix closes as a consequence of the
// `neverVerbWords` extension — they load on rc19 and must reject
// on rc20.
package skills

import (
	"strings"
	"testing"
)

func TestRC20PluralNounPhrasesStillReject(t *testing.T) {
	// F-A15-1: a bare noun or adjective after a plural or compound
	// subject is a noun phrase, not a clause — it must reject.
	for _, tc := range []struct {
		name string
		line string
	}{
		// Pure -ss nouns, clause-final (a1).
		{"plural business", "Activate the tool, the skills business"},
		{"plural success", "Activate the tool, the skills success"},
		{"plural loss", "Activate the tool, the skills loss"},
		{"plural class", "Activate the tool, the skills class"},
		{"plural glass", "Activate the tool, the skills glass"},
		{"compound venv class", "Activate the tool, the skill and the venv class"},
		{"compound venv loss", "Activate the tool, the skill and the venv loss"},
		{"compound venv business", "Activate the tool, the skill and the venv business"},
		// Pure -ss nouns, following-word positions (a2).
		{"plural business the plan", "Activate the tool, the skills business the plan"},
		{"plural class the plan", "Activate the tool, the skills class the plan"},
		{"plural loss the plan", "Activate the tool, the skills loss the plan"},
		{"plural class guides", "Activate the tool, the skills class guides"},
		{"plural loss guides", "Activate the tool, the skills loss guides"},
		{"plural class you", "Activate the tool, the skills class you"},
		{"plural class quickly", "Activate the tool, the skills class quickly"},
		{"plural class for beginners", "Activate the tool, the skills class for beginners"},
		{"plural class when ready", "Activate the tool, the skills class when ready"},
		// Bare nouns and bare adjectives (a3).
		{"plural notebook", "Activate the tool, the skills notebook"},
		{"plural guidebook", "Activate the tool, the skills guidebook"},
		{"plural keyboard", "Activate the tool, the skills keyboard"},
		{"plural pencil", "Activate the tool, the skills pencil"},
		{"plural hot", "Activate the tool, the skills hot"},
		{"plural tall", "Activate the tool, the skills tall"},
		{"plural young", "Activate the tool, the skills young"},
		{"plural red", "Activate the tool, the skills red"},
		{"plural cold", "Activate the tool, the skills cold"},
		{"plural dark", "Activate the tool, the skills dark"},
		{"plural wide", "Activate the tool, the skills wide"},
		{"plural deep", "Activate the tool, the skills deep"},
		{"plural soft", "Activate the tool, the skills soft"},
		{"plural hard", "Activate the tool, the skills hard"},
		{"compound venv notebook", "Activate the tool, the skill and the venv notebook"},
		{"compound venv hot", "Activate the tool, the skill and the venv hot"},
		{"compound venv tall", "Activate the tool, the skill and the venv tall"},
		{"plural notebook the plan", "Activate the tool, the skills notebook the plan"},
		{"plural hot the plan", "Activate the tool, the skills hot the plan"},
		{"plural notebook guides", "Activate the tool, the skills notebook guides"},
		{"plural tall guides", "Activate the tool, the skills tall guides"},
		// More pure adjectives (g4).
		{"plural angry", "Activate the tool, the skills angry"},
		{"plural sorry", "Activate the tool, the skills sorry"},
		{"plural afraid", "Activate the tool, the skills afraid"},
		{"plural alive", "Activate the tool, the skills alive"},
		{"plural asleep", "Activate the tool, the skills asleep"},
		{"plural aware", "Activate the tool, the skills aware"},
		{"plural able", "Activate the tool, the skills able"},
		{"compound venv angry", "Activate the tool, the skill and the venv angry"},
		{"plural angry the plan", "Activate the tool, the skills angry the plan"},
		{"plural afraid guides", "Activate the tool, the skills afraid guides"},
		// Transitive-only verbs, no intransitive reading (d).
		{"plural free", "Activate the tool, the skills free"},
		{"plural busy", "Activate the tool, the skills busy"},
		{"compound venv free", "Activate the tool, the skill and the venv free"},
		{"compound venv busy", "Activate the tool, the skill and the venv busy"},
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

func TestRC20GuardedPluralNonVerbsStillReject(t *testing.T) {
	// F-A15-2: a suffix-bearing pure noun or adjective cannot take
	// a guarded plural as its object.
	for _, tc := range []struct {
		name string
		line string
	}{
		// Pure -s nouns (a4 + g5).
		{"series tools", "Activate the tool, the skill series tools"},
		{"species tools", "Activate the tool, the skill species tools"},
		{"series users", "Activate the tool, the skill series users"},
		{"series settings", "Activate the tool, the skill series settings"},
		{"compound venv series tools", "Activate the tool, the skill and the venv series tools"},
		{"plural series tools", "Activate the tool, the skills series tools"},
		{"series items", "Activate the tool, the skill series items"},
		{"species users", "Activate the tool, the skill species users"},
		{"series meetings", "Activate the tool, the skill series meetings"},
		// Pure -ing noun (a4 + g5).
		{"ceiling tools", "Activate the tool, the skill ceiling tools"},
		{"plural ceiling tools", "Activate the tool, the skills ceiling tools"},
		{"ceiling items", "Activate the tool, the skill ceiling items"},
		{"ceiling users", "Activate the tool, the skill ceiling users"},
		{"ceiling meetings", "Activate the tool, the skill ceiling meetings"},
		// Pure -ed adjectives (g1).
		{"rugged tools", "Activate the tool, the skill rugged tools"},
		{"rugged users", "Activate the tool, the skill rugged users"},
		{"compound venv rugged tools", "Activate the tool, the skill and the venv rugged tools"},
		{"compound venv rugged meetings", "Activate the tool, the skill and the venv rugged meetings"},
		{"plural rugged tools", "Activate the tool, the skills rugged tools"},
		{"wicked tools", "Activate the tool, the skill wicked tools"},
		{"compound venv wicked tools", "Activate the tool, the skill and the venv wicked tools"},
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

func TestRC20ParticipleBranchNonHeadsStillReject(t *testing.T) {
	// F-A15-3: a never-verb adjective or adverb before a
	// participle + object is a modifier, not a clause head.
	for _, tc := range []struct {
		name string
		line string
	}{
		// Compound subject + adjective/adverb + participle + plural (c1).
		{"big running tests", "Activate the tool, the skill and the big running tests"},
		{"small running tests", "Activate the tool, the skill and the small running tests"},
		{"happy running tests", "Activate the tool, the skill and the happy running tests"},
		{"sad running tests", "Activate the tool, the skill and the sad running tests"},
		{"glad running tests", "Activate the tool, the skill and the glad running tests"},
		{"good running tests", "Activate the tool, the skill and the good running tests"},
		{"bad running tests", "Activate the tool, the skill and the bad running tests"},
		{"great running tests", "Activate the tool, the skill and the great running tests"},
		{"very running tests", "Activate the tool, the skill and the very running tests"},
		{"fast running tests", "Activate the tool, the skill and the fast running tests"},
		{"free running tests", "Activate the tool, the skill and the free running tests"},
		{"busy running tests", "Activate the tool, the skill and the busy running tests"},
		{"tired running tests", "Activate the tool, the skill and the tired running tests"},
		{"hot running tests", "Activate the tool, the skill and the hot running tests"},
		{"tall running tests", "Activate the tool, the skill and the tall running tests"},
		{"young running tests", "Activate the tool, the skill and the young running tests"},
		{"red running tests", "Activate the tool, the skill and the red running tests"},
		{"big waiting tests", "Activate the tool, the skill and the big waiting tests"},
		{"very waiting tests", "Activate the tool, the skill and the very waiting tests"},
		{"venv and big running tests", "Activate the tool, the skill and the venv and the big running tests"},
		{"venv and very running tests", "Activate the tool, the skill and the venv and the very running tests"},
		// Singular subject + adjective + participle + plural (c1).
		{"singular big running tests", "Activate the tool, the skill big running tests"},
		{"singular very running tests", "Activate the tool, the skill very running tests"},
		// More modifiers before participle + plural (g3).
		{"slow running tests", "Activate the tool, the skill and the slow running tests"},
		{"soft running tests", "Activate the tool, the skill and the soft running tests"},
		{"dark running tests", "Activate the tool, the skill and the dark running tests"},
		{"big running guides", "Activate the tool, the skill and the big running guides"},
		{"angry running tests", "Activate the tool, the skill and the angry running tests"},
		{"sorry running tests", "Activate the tool, the skill and the sorry running tests"},
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

func TestRC20AdjectiveVerbsStillReject(t *testing.T) {
	// F-A15-4: an adjective-verb ("close", "set", like "open")
	// does not take a guarded plural as its object.
	for _, tc := range []struct {
		name string
		line string
	}{
		// Compound subject (b3 + g2).
		{"compound close tools", "Activate the tool, the skill and the close tools"},
		{"compound set tools", "Activate the tool, the skill and the set tools"},
		{"compound close meetings", "Activate the tool, the skill and the close meetings"},
		{"compound set users", "Activate the tool, the skill and the set users"},
		{"compound close users", "Activate the tool, the skill and the close users"},
		{"compound set meetings", "Activate the tool, the skill and the set meetings"},
		// Singular and plural subjects (b3 + g2).
		{"singular close tools", "Activate the tool, the skill close tools"},
		{"singular set tools", "Activate the tool, the skill set tools"},
		{"singular close users", "Activate the tool, the skill close users"},
		{"plural close tools", "Activate the tool, the skills close tools"},
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

func TestRC20GuardsStillLoad(t *testing.T) {
	// Guard: the loads Audit 15 verified as correct on rc19 must
	// keep holding after the fix — the F-A14-1/F-A14-2 verbs, the
	// F-A13-2 participle carve-outs, the F-A13-3 aux/following-verb
	// rules, and the Audit 15 §6 observations the report calls
	// defensible (genuine -ss verbs, genuine -ing/-ed verb forms,
	// noun-head + predicate participle, suffixed adjective-verbs).
	for _, tc := range []struct {
		name string
		line string
	}{
		// F-A14-1: unlisted base-form verbs after a plural subject.
		{"plural build", "Activate the tool, the skills build"},
		{"plural design", "Activate the tool, the skills design"},
		{"plural develop", "Activate the tool, the skills develop"},
		{"plural study", "Activate the tool, the skills study"},
		{"plural build the plan", "Activate the tool, the skills build the plan"},
		{"plural build guides", "Activate the tool, the skills build guides"},
		{"plural build you", "Activate the tool, the skills build you"},
		{"plural build quickly", "Activate the tool, the skills build quickly"},
		{"plural build for beginners", "Activate the tool, the skills build for beginners"},
		{"compound build", "Activate the tool, the skill and the venv build"},
		// F-A14-2: genuine verbs take a guarded plural as object.
		{"makes users", "Activate the tool, the skill makes users"},
		{"creates tools", "Activate the tool, the skill creates tools"},
		{"trained users", "Activate the tool, the skill trained users"},
		{"files tools", "Activate the tool, the skill files tools"},
		{"plural make users", "Activate the tool, the skills make users"},
		{"running tools", "Activate the tool, the skill running tools"},
		// F-A13-2: participle + object is a verb phrase.
		// F-A13-3: aux / following verb — the candidate is the subject.
		{"build waiting tests", "Activate the tool, the skills build waiting tests"},
		{"build running the tests", "Activate the tool, the skills build running the tests"},
		{"files running tests", "Activate the tool, the skill files running tests"},
		{"compound running tests", "Activate the tool, the skill and the venv running tests"},
		{"analysis is ready", "Activate the tool, the skill analysis is ready"},
		// Audit 15 §6 O1/O20: genuine -ss verbs.
		{"plural boss", "Activate the tool, the skills boss"},
		{"plural progress", "Activate the tool, the skills progress"},
		{"plural process", "Activate the tool, the skills process"},
		{"plural address", "Activate the tool, the skills address"},
		{"plural access", "Activate the tool, the skills access"},
		// Audit 15 §6 O2: genuine -ing verbs (ambiguous nouns).
		{"building tools", "Activate the tool, the skill building tools"},
		{"training tools", "Activate the tool, the skill training tools"},
		{"learning tools", "Activate the tool, the skill learning tools"},
		// Audit 15 §6 O9/c2: noun head + predicate participle.
		{"analysis running tests", "Activate the tool, the skill and the analysis running tests"},
		{"reports running tests", "Activate the tool, the skill and the reports running tests"},
		{"builds running tests", "Activate the tool, the skill and the builds running tests"},
		{"makes running tests", "Activate the tool, the skill and the makes running tests"},
		{"training running tests", "Activate the tool, the skill and the training running tests"},
		{"glass running tests", "Activate the tool, the skill and the glass running tests"},
		{"class running tests", "Activate the tool, the skill and the class running tests"},
		{"series running tests", "Activate the tool, the skill and the series running tests"},
		// Audit 15 §6 O4: genuine -ed verbs after a compound subject.
		{"venv closed tools", "Activate the tool, the skill and the venv closed tools"},
		{"venv trained tools", "Activate the tool, the skill and the venv trained tools"},
		{"venv opened tools", "Activate the tool, the skill and the venv opened tools"},
		{"venv finished tools", "Activate the tool, the skill and the venv finished tools"},
		{"venv tired tools", "Activate the tool, the skill and the venv tired tools"},
		{"venv excited tools", "Activate the tool, the skill and the venv excited tools"},
		{"venv crooked tools", "Activate the tool, the skill and the venv crooked tools"},
		{"venv learned tools", "Activate the tool, the skill and the venv learned tools"},
		// F-A14-2 b1: other -s/-ed verbs + guarded plurals.
		{"writes users", "Activate the tool, the skill writes users"},
		{"opened tools", "Activate the tool, the skill opened tools"},
		{"closed meetings", "Activate the tool, the skill closed meetings"},
		{"sends users", "Activate the tool, the skill sends users"},
		// Audit 15 §6 O12/g2: singular base-form verbs + guarded plural.
		{"make tools", "Activate the tool, the skill make tools"},
		{"create tools", "Activate the tool, the skill create tools"},
		{"use tools", "Activate the tool, the skill use tools"},
		{"run tools", "Activate the tool, the skill run tools"},
		{"help users", "Activate the tool, the skill help users"},
		{"makes tools", "Activate the tool, the skill makes tools"},
		{"venv opened users", "Activate the tool, the skill and the venv opened users"},
		// Audit 15 §6 e: interactions that still load. "reports
		// that helps" loads because "reports" is a genuine verb
		// form (verb + subordinate clause); "series that helps"
		// and "business that helps" are noun + relative clause,
		// so they reject — see TestRC20GuardsStillReject.
		{"training runs tests", "Activate the tool, the skill training runs tests"},
		{"reports that helps", "Activate the tool, the skill reports that helps"},
		// D2 control + R3.
		{"open venv waits", "Activate the tool, the skill and the open venv waits"},
		{"skill setting (R3)", "Activate the tool, the skill setting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Helper\n\n" + tc.line + "\n\nDo things.\n"
			if err := rc13W4LoadErr(t, content); err != nil {
				t.Fatalf("LoadFile(%s): expected success, got %v", tc.name, err)
			}
		})
	}
}

func TestRC20GuardsStillReject(t *testing.T) {
	// Guard: the rejections Audit 15 verified as correct on rc19
	// must keep holding after the fix — the F-A14-3/F-A14-4
	// fixes, the F-A13-1 guarded nouns, the D2/D3 noun phrases,
	// and the pre-existing residuals. The BONUS cases are
	// pre-existing fail-open shapes (Audit 15 §6 O3/O16 and the
	// -ed suffix residual) that this fix closes as a consequence
	// of the `neverVerbWords` extension: a pure -s/-ss/-ing noun
	// or a never-verb -ed adjective is never a verb in any
	// position, so it rejects clause-finally, before an article,
	// before a pronoun/adverb/preposition/subordinator, and
	// before a participle with no object.
	for _, tc := range []struct {
		name string
		line string
	}{
		// F-A14-3: non-noun-head words do not complete the subject.
		{"new running tests", "Activate the tool, the skill and the new running tests"},
		{"ready running tests", "Activate the tool, the skill and the ready running tests"},
		{"old running tests", "Activate the tool, the skill and the old running tests"},
		{"quickly running tests", "Activate the tool, the skill and the quickly running tests"},
		{"slowly running tests", "Activate the tool, the skill and the slowly running tests"},
		// F-A14-4: pure -s/-ing nouns are never verbs.
		{"plural analysis", "Activate the tool, the skills analysis"},
		{"plural status", "Activate the tool, the skills status"},
		{"plural news", "Activate the tool, the skills news"},
		{"plural morning", "Activate the tool, the skills morning"},
		// F-A13-1: guarded noun compounds.
		{"documentation tools", "Activate the tool, the skill documentation tools"},
		{"reference tools", "Activate the tool, the skill reference tools"},
		{"happy tools", "Activate the tool, the skill happy tools"},
		{"documentation for beginners", "Activate the tool, the skill documentation for beginners"},
		// D2/D3 noun phrases.
		{"open tools", "Activate the tool, the skill and the open tools"},
		{"compound running tests (D3)", "Activate the tool, the skill and the running tests"},
		// No noun head: the participle is a modifier (g1 control).
		{"compound rugged tools (no venv)", "Activate the tool, the skill and the rugged tools"},
		// Guarded object after a participle phrase (g3).
		{"big running tools", "Activate the tool, the skill and the big running tools"},
		{"big running meetings", "Activate the tool, the skill and the big running meetings"},
		{"big running users", "Activate the tool, the skill and the big running users"},
		// Singular base form / no verb at all.
		{"skill work", "Activate the tool, the skill work"},
		{"skill files", "Activate the tool, the skill files"},
		{"compound no verb", "Activate the tool, the skill and the venv"},
		// No verb evidence: adjective + guarded plural (b3).
		{"compound free tools", "Activate the tool, the skill and the free tools"},
		{"compound ready tools", "Activate the tool, the skill and the ready tools"},
		{"compound busy tools", "Activate the tool, the skill and the busy tools"},
		// Adjective-verb without a noun head (g2).
		{"compound open users", "Activate the tool, the skill and the open users"},
		{"compound opened users", "Activate the tool, the skill and the opened users"},
		// Noun + subordinator is a noun phrase, not a clause
		// (the subordinator attaches a relative clause): a
		// never-verb noun before "that" rejects, while a genuine
		// verb form ("reports that helps") loads. "series that
		// helps" only loaded on rc19 because the -s suffix
		// heuristic misread a pure noun as a verb; the
		// `neverVerbWords` extension corrects it, so the -s and
		// -ss nouns now behave consistently.
		{"BONUS series that helps", "Activate the tool, the skill series that helps"},
		{"business that helps", "Activate the tool, the skill business that helps"},
		{"class that helps", "Activate the tool, the skill class that helps"},
		{"glass that helps", "Activate the tool, the skill glass that helps"},
		{"loss that helps", "Activate the tool, the skill loss that helps"},
		{"success that helps", "Activate the tool, the skill success that helps"},
		// BONUS (pre-existing fail-open, closed by this fix):
		// pure -s nouns (O16).
		{"BONUS plural series", "Activate the tool, the skills series"},
		{"BONUS plural species", "Activate the tool, the skills species"},
		{"BONUS plural series the plan", "Activate the tool, the skills series the plan"},
		{"BONUS plural series guides", "Activate the tool, the skills series guides"},
		{"BONUS plural series you", "Activate the tool, the skills series you"},
		{"BONUS plural series quickly", "Activate the tool, the skills series quickly"},
		{"BONUS plural series for beginners", "Activate the tool, the skills series for beginners"},
		{"BONUS plural series when ready", "Activate the tool, the skills series when ready"},
		{"BONUS singular series the plan", "Activate the tool, the skill series the plan"},
		{"BONUS singular series guides", "Activate the tool, the skill series guides"},
		// BONUS: pure -ing noun (O3).
		{"BONUS singular ceiling", "Activate the tool, the skill ceiling"},
		{"BONUS plural ceiling", "Activate the tool, the skills ceiling"},
		{"BONUS singular ceiling the plan", "Activate the tool, the skill ceiling the plan"},
		{"BONUS plural ceiling the plan", "Activate the tool, the skills ceiling the plan"},
		{"BONUS ceiling for beginners", "Activate the tool, the skill ceiling for beginners"},
		{"BONUS ceiling that helps", "Activate the tool, the skill ceiling that helps"},
		// BONUS: never-verb -ed adjective (g1).
		{"BONUS singular rugged", "Activate the tool, the skill rugged"},
		{"BONUS rugged the plan", "Activate the tool, the skill rugged the plan"},
		// BONUS: pure -s noun, clause-final (O16).
		{"BONUS singular series", "Activate the tool, the skill series"},
		{"BONUS compound venv series", "Activate the tool, the skill and the venv series"},
		// BONUS: -s noun + participle with no object.
		{"BONUS plural analysis waiting", "Activate the tool, the skills analysis waiting"},
		{"BONUS plural series waiting", "Activate the tool, the skills series waiting"},
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
