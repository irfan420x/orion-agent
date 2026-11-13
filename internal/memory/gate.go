// Package memory — the what-not-to-save gate (Memory v2).
//
// Some things are derivable from the repo and must never be saved as
// memories: code patterns, architecture, git history, file structure.
// A memory saying "the repo has a cmd/ directory" adds nothing — the
// code says it better, and it goes stale the moment the code changes.
// What IS worth saving is what cannot be derived: who the user is,
// what they prefer, what they decided, what surprised them, what went
// wrong and why.
//
// The gate classifies a candidate save as save or skip, with a reason.
// Even an explicit user request to save something goes through the
// gate (FilterExplicitSave): the surprising or non-obvious part is
// what is kept; the derivable rest is dropped. If nothing surprising
// remains, nothing is saved.
package memory

import (
	"regexp"
	"strings"
)

// GateDecision is the result of Gate: whether to save, and why.
type GateDecision struct {
	// Save is true when the candidate is worth saving as a memory.
	Save bool
	// Reason explains the decision in one sentence.
	Reason string
}

// skipRule is one derivable-content pattern: a regex plus the reason
// to give when it matches.
type skipRule struct {
	re     *regexp.Regexp
	reason string
}

func mustRule(pattern, reason string) skipRule {
	return skipRule{re: regexp.MustCompile(`(?i)` + pattern), reason: reason}
}

// skipRules are the derivable-content patterns, checked in order.
// The first match decides: skip. They are deliberately specific — a
// broad pattern would swallow genuine user facts ("I prefer the
// repository pattern" is a preference, not a code-pattern note).
var skipRules = []skipRule{
	mustRule(`\b(file|directory|folder) (structure|layout|tree)\b`, "file structure is derivable from the repo, not a memory"),
	mustRule(`\b(repo|repository|project|codebase) has a \w+/?\w* ?(dir|directory|folder|package)\b`, "file structure is derivable from the repo, not a memory"),
	mustRule(`\bhas a cmd/ ?(dir|directory|folder)?\b`, "file structure is derivable from the repo, not a memory"),
	mustRule(`\bgit (history|log|blame)\b`, "git history is derivable from git, not a memory"),
	mustRule(`\bcommit (history|log)\b`, "git history is derivable from git, not a memory"),
	mustRule(`\bwas (added|changed|fixed|introduced) in commit\b`, "git history is derivable from git, not a memory"),
	mustRule(`\b(uses?|using|implements?|follows?) the [a-z ]*(design )?pattern\b`, "code patterns are derivable from the code, not a memory"),
	mustRule(`\b(repository|factory|singleton|observer|strategy|adapter|decorator) pattern\b`, "code patterns are derivable from the code, not a memory"),
	mustRule(`\b(architecture|architectural) (is|of|uses?|follows?)\b`, "architecture is derivable from the code, not a memory"),
	mustRule(`\b(layered|hexagonal|clean|microservices?) architecture\b`, "architecture is derivable from the code, not a memory"),
	mustRule(`\bfunction .* (takes|returns|calls)\b`, "code behaviour is derivable from the code, not a memory"),
	mustRule(`\bdefined in (file |[a-z0-9_./-]+\.go)\b`, "code locations are derivable from the code, not a memory"),
}

// Gate classifies a candidate save. A candidate matching a
// derivable-content pattern is skipped with the pattern's reason.
// Anything else is saved: the gate's job is to block the derivable
// categories, not to second-guess genuine user facts, preferences,
// decisions, bugs and incidents.
func Gate(candidate string) GateDecision {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return GateDecision{Save: false, Reason: "nothing to save: candidate is empty"}
	}
	for _, r := range skipRules {
		if r.re.MatchString(candidate) {
			return GateDecision{Save: false, Reason: r.reason}
		}
	}
	return GateDecision{Save: true, Reason: "no derivable-content signal: a user fact, preference, decision or incident is worth remembering"}
}

// FilterExplicitSave applies the gate to an explicit user request to
// save something ("remember that ..."). The request is split into
// sentences; sentences matching a derivable-content pattern are
// dropped and only the surprising or non-obvious remainder is kept.
// It returns the kept text and ok=false when nothing worth keeping
// remains — in that case nothing should be saved at all.
//
// The filter is sentence-level, deliberately: a sentence mixing a
// derivable claim with a surprising one is dropped whole rather than
// reworded, because the filter never rewrites the user's words — it
// only keeps or drops them.
func FilterExplicitSave(candidate string) (kept string, ok bool) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return "", false
	}
	sentences := splitSentences(candidate)
	var keptParts []string
	for _, s := range sentences {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		skip := false
		for _, r := range skipRules {
			if r.re.MatchString(s) {
				skip = true
				break
			}
		}
		if !skip {
			keptParts = append(keptParts, s)
		}
	}
	if len(keptParts) == 0 {
		return "", false
	}
	return strings.Join(keptParts, " "), true
}

// splitSentences splits text on sentence-ending punctuation followed
// by whitespace (or the end of the string), keeping the punctuation
// attached to its sentence. It is a small heuristic, not a natural
// language parser: unknown forms are left intact, never reworded.
func splitSentences(text string) []string {
	var out []string
	start := 0
	runes := []rune(text)
	for i, r := range runes {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		// A period inside a path, a version number or a decimal is
		// not a sentence end: require whitespace or end-of-string
		// after it, and a non-lowercase start... keep it simple:
		// require whitespace/end after the punctuation.
		if i+1 < len(runes) && runes[i+1] != ' ' && runes[i+1] != '\n' && runes[i+1] != '\t' {
			continue
		}
		out = append(out, strings.TrimSpace(string(runes[start:i+1])))
		start = i + 1
	}
	if tail := strings.TrimSpace(string(runes[start:])); tail != "" {
		out = append(out, tail)
	}
	return out
}
