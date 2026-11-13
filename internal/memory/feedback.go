// Package memory — feedback memories (Memory v2).
//
// A feedback memory is guidance from the user on how to work. The
// body structure is fixed: the rule first, then a "Why:" line and a
// "How to apply:" line, both non-empty:
//
//	Always run gofmt before reporting tests green.
//
//	Why: unformatted code fails the repo's CI check and wastes a round trip.
//	How to apply: run `gofmt -l .` after every edit; fix anything it lists.
//
// Both corrections AND confirmations are valid feedback memories —
// only saving corrections makes the agent drift overcautious (the
// "yes exactly" that confirms a behaviour is as worth remembering as
// the "no, do it this way" that corrects one). The SaveKind records
// which one it was.
package memory

import (
	"fmt"
	"strings"
)

// SaveKind says whether a feedback memory came from a user correction
// or a user confirmation.
type SaveKind int

const (
	// SaveCorrection is a feedback memory from a user correction
	// ("no — do it this way instead").
	SaveCorrection SaveKind = iota
	// SaveConfirmation is a feedback memory from a user confirmation
	// ("yes exactly", "that's right") — the confirmed behaviour is
	// worth remembering, not just the corrected one.
	SaveConfirmation
)

// String renders the kind for storage in a memory body.
func (k SaveKind) String() string {
	switch k {
	case SaveCorrection:
		return "correction"
	case SaveConfirmation:
		return "confirmation"
	default:
		return "correction"
	}
}

// ParseSaveKind parses "correction" or "confirmation"; anything else
// is an error.
func ParseSaveKind(s string) (SaveKind, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "correction":
		return SaveCorrection, nil
	case "confirmation":
		return SaveConfirmation, nil
	default:
		return SaveCorrection, fmt.Errorf("memory: unknown save kind %q (want correction or confirmation)", s)
	}
}

// NewFeedbackMemory builds a feedback memory from its parts. kind
// records whether this is a correction or a confirmation; rule is the
// guidance itself; why explains the reasoning; how says how to apply
// it. All three must be non-empty. The returned memory is validated
// before it is returned, so it is always safe to pass to
// TypedStore.Save.
func NewFeedbackMemory(kind SaveKind, name, description, rule, why, how string) (TypedMemory, error) {
	rule = strings.TrimSpace(rule)
	why = strings.TrimSpace(why)
	how = strings.TrimSpace(how)
	if rule == "" {
		return TypedMemory{}, fmt.Errorf("memory: feedback rule must not be empty")
	}
	if why == "" {
		return TypedMemory{}, fmt.Errorf("memory: feedback Why must not be empty")
	}
	if how == "" {
		return TypedMemory{}, fmt.Errorf("memory: feedback How to apply must not be empty")
	}
	var b strings.Builder
	switch kind {
	case SaveConfirmation:
		b.WriteString("Confirmed by the user: ")
		b.WriteString(rule)
	default:
		b.WriteString(rule)
	}
	b.WriteString("\n\nWhy: ")
	b.WriteString(why)
	b.WriteString("\n\nHow to apply: ")
	b.WriteString(how)
	b.WriteString("\n")
	m := TypedMemory{
		Name:        strings.TrimSpace(name),
		Description: strings.TrimSpace(description),
		Type:        TypeFeedback,
		Body:        b.String(),
		// Explicit user confirmation/correction is the ONLY path
		// that creates a feedback memory (Audit 4 A P1-3): the
		// provenance marks it user-written, so recall may render
		// it as standing guidance and TypedStore.Save accepts it.
		Provenance: ProvenanceUser,
	}
	if err := m.Validate(); err != nil {
		return TypedMemory{}, err
	}
	return m, nil
}

// FeedbackParts is the parsed structure of a feedback memory body.
type FeedbackParts struct {
	// Rule is the guidance itself (the first non-empty line(s)).
	Rule string
	// Why is the reasoning, from the "Why:" section.
	Why string
	// How is the application guidance, from the "How to apply:" section.
	How string
	// Kind is SaveConfirmation when the body starts with the
	// confirmation marker, SaveCorrection otherwise.
	Kind SaveKind
}

// ParseFeedbackBody splits a feedback body into its parts. It accepts
// multi-line Why/How sections (continuation lines until the next
// section label) and a "Why:" or "How to apply:" value on the same
// line as its label. It does not require the kind marker.
func ParseFeedbackBody(body string) (FeedbackParts, error) {
	lines := strings.Split(body, "\n")
	// Find the section labels. The rule is everything before "Why:".
	whyIdx, howIdx := -1, -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if whyIdx < 0 && (lower == "why:" || strings.HasPrefix(lower, "why: ")) {
			whyIdx = i
			continue
		}
		if howIdx < 0 && (lower == "how to apply:" || strings.HasPrefix(lower, "how to apply: ")) {
			howIdx = i
		}
	}
	if whyIdx < 0 {
		return FeedbackParts{}, fmt.Errorf("memory: feedback body is missing a Why: line")
	}
	if howIdx < 0 {
		return FeedbackParts{}, fmt.Errorf("memory: feedback body is missing a How to apply: line")
	}
	if howIdx < whyIdx {
		return FeedbackParts{}, fmt.Errorf("memory: feedback body has How to apply: before Why: (want rule, then Why:, then How to apply:)")
	}
	ruleLines := lines[:whyIdx]
	// Drop trailing blank lines of the rule section.
	for len(ruleLines) > 0 && strings.TrimSpace(ruleLines[len(ruleLines)-1]) == "" {
		ruleLines = ruleLines[:len(ruleLines)-1]
	}
	rule := strings.TrimSpace(strings.Join(ruleLines, "\n"))
	if rule == "" {
		return FeedbackParts{}, fmt.Errorf("memory: feedback body has no rule before Why:")
	}
	kind := SaveCorrection
	if strings.HasPrefix(rule, "Confirmed by the user: ") {
		kind = SaveConfirmation
		// The marker records provenance, not guidance: the rule
		// itself is what follows it.
		rule = strings.TrimSpace(strings.TrimPrefix(rule, "Confirmed by the user: "))
		if rule == "" {
			return FeedbackParts{}, fmt.Errorf("memory: feedback body has a confirmation marker but no rule after it")
		}
	}
	why := sectionText(lines, whyIdx, howIdx, "Why:")
	how := sectionText(lines, howIdx, len(lines), "How to apply:")
	if strings.TrimSpace(why) == "" {
		return FeedbackParts{}, fmt.Errorf("memory: feedback Why: section must not be empty")
	}
	if strings.TrimSpace(how) == "" {
		return FeedbackParts{}, fmt.Errorf("memory: feedback How to apply: section must not be empty")
	}
	return FeedbackParts{Rule: rule, Why: why, How: how, Kind: kind}, nil
}

// sectionText returns the text of a labelled section: the remainder of
// the label line after the label, plus any following lines up to end
// (exclusive), joined and trimmed.
func sectionText(lines []string, start, end int, label string) string {
	parts := []string{}
	first := strings.TrimSpace(lines[start])
	if rest := strings.TrimSpace(first[len(label):]); rest != "" {
		parts = append(parts, rest)
	}
	for _, line := range lines[start+1 : end] {
		parts = append(parts, line)
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// ValidateFeedback checks that m is a feedback memory with the
// required body structure: rule first, then a non-empty Why: section,
// then a non-empty How to apply: section. A memory of another type
// fails with a type error; a feedback memory with a malformed body
// fails with a structure error.
func ValidateFeedback(m TypedMemory) error {
	if m.Type != TypeFeedback {
		return fmt.Errorf("memory: ValidateFeedback called on a %q memory, want feedback", string(m.Type))
	}
	_, err := ParseFeedbackBody(m.Body)
	return err
}
