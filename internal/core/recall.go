// Fact-aware memory recall for the agent loop.
//
// The loop's recent window (maxRecentMessages) only covers the last few
// turns, so a fact the user stated long ago — or stored explicitly with
// AddFact — is invisible to the model when it is needed. Recall bridges
// that gap by injecting a short, ranked "Relevant things the user told
// you earlier" section into the system message.
//
// Two sources are scored, because the loop never calls AddFact: a fact
// stated in conversation lives only in old messages, so reading
// ListFacts alone would change nothing for those cases.
//
//   - (a) Explicit long-term facts from memory.ListFacts(), rendered
//     as "key: value". These are deliberate, so a single shared content
//     token is enough to inject one (score >= 1).
//   - (b) Declarative sentences from OLDER user messages, i.e. messages
//     outside the recent window. These are noisy — a turn usually mixes
//     statements with instructions — so they need at least two shared
//     content tokens (score >= 2). Questions (ending in "?") and
//     instruction/tool sentences (containing "use the", "call the",
//     "you must", "write", "read the file", "compute", "tell me" or
//     "do not") are dropped outright, which keeps "My favourite number
//     is 17." but drops "Use the calculator tool to compute my
//     favourite number times 3".
//
// Scoring is plain token overlap: the number of shared content tokens
// between the current input and the candidate, after lowercasing,
// splitting on non-alphanumerics and dropping stopwords. There is no
// stemming and no embedding — overlap on distinctive words (names,
// numbers, nouns) is what matters, and it is deterministic and cheap.
// Ranking is score descending, ties broken by facts before message
// sentences, then most recent first. At most 3 items are injected,
// each truncated to 200 characters, and a candidate whose content is
// already visible in the recent window is never injected twice.
// The conversation-summary fact is the exception: its latest
// section is injected in its own extra slot, truncated to 2000
// runes, so a compaction summary actually reaches the model.
//
// Recall never fails a run: if ListFacts or the full history cannot be
// read, the loop logs and continues with whatever source survived
// (or none at all).
package core

import (
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
)

// maxRecallItems caps how many recalled items are injected into one run.
const maxRecallItems = 3

// maxRecallItemRunes caps the length of a single injected item.
const maxRecallItemRunes = 200

// maxSummaryRecallRunes caps the injected conversation-summary
// section (Audit 5 F-A5-2): the summary fact holds up to 8000
// runes of distilled history, and the 200-rune item cap made all
// but a fragment of it invisible to the model. The latest section
// gets a much larger cap and its own slot, so a good summary
// actually reaches the model instead of only the matching fragment.
const maxSummaryRecallRunes = 2000

// summaryFactKey is the fact key auto-compaction and /compress
// write the conversation summary under.
const summaryFactKey = "conversation-summary"

// recallSectionHeader introduces the injected items in the system
// message. It tells the model these are the user's own earlier words,
// to be preferred over guessing.
const recallSectionHeader = "\n\nRelevant things the user told you earlier (stored facts and earlier messages — treat them as what the user said before, and prefer them over guessing):\n"

// recallStopwords are dropped before scoring. They carry no topical
// signal ("what is my favourite number" is about "favourite number",
// not about "what" or "is"), and dropping them is what keeps a
// greeting such as "Hello, how are you?" from matching every fact
// that happens to contain "you" or "is".
var recallStopwords = map[string]struct{}{
	"the": {}, "is": {}, "a": {}, "an": {}, "my": {}, "what": {},
	"do": {}, "you": {}, "your": {}, "i": {}, "me": {}, "it": {},
	"to": {}, "of": {}, "and": {}, "in": {}, "on": {}, "for": {},
	"please": {}, "just": {}, "briefly": {}, "with": {}, "reply": {},
	"answer": {}, "tell": {}, "say": {}, "so": {}, "then": {},
	"that": {}, "this": {}, "are": {}, "was": {}, "were": {},
	"be": {}, "been": {}, "have": {}, "has": {}, "had": {},
	"can": {}, "could": {}, "would": {}, "should": {}, "very": {},
	"really": {}, "now": {}, "here": {}, "there": {}, "back": {},
	"again": {}, "after": {}, "before": {}, "earlier": {}, "look": {},
	"our": {}, "conversation": {},
}

// instructionPattern matches instruction/tool-direction sentences.
// The phrases are matched on word boundaries (\b), not as raw
// substrings: a substring match drops legitimate fact sentences —
// "write" matches "writer" ("My favourite writer is Asimov.") and
// "compute" matches "computer" ("My computer runs Linux."). With
// boundaries, "Write a poem ..." and "Compute my favourite number
// ..." are still dropped, while the fact sentences survive.
var instructionPattern = regexp.MustCompile(`\b(?:use the|call the|you must|write|read the file|compute|tell me|do not)\b`)

// sentencePattern splits a message into sentences, keeping the
// terminator so questions (ending in "?") can be recognised and
// dropped. Newlines also break sentences.
var sentencePattern = regexp.MustCompile(`[^.!?\n]+[.!?]?`)

// recallCandidate is one scored recall item.
type recallCandidate struct {
	text    string // text as injected (already truncated)
	score   int    // shared content tokens with the query
	isFact  bool   // explicit fact (a) vs message sentence (b)
	recency int64  // facts: UpdatedAt UnixNano; sentences: scan order
}

// recallItems ranks recall candidates for query and returns at most
// maxRecallItems of them, best first. facts are explicit long-term
// facts; older holds user messages from outside the recent window;
// recent is the current recent window, used only to suppress items
// whose content the model can already see.
func recallItems(query string, facts []memory.Fact, older, recent []model.Message) []string {
	queryTokens := contentTokens(query)
	if len(queryTokens) == 0 {
		return nil
	}

	var cands []recallCandidate

	// The conversation-summary fact is handled separately below
	// (latest section, raised cap, its own slot): the generic
	// fact path would truncate it to 200 runes and let it crowd
	// out other facts.
	var summaryFact *memory.Fact

	// (a) Explicit facts: deliberate, so one shared token suffices.
	for _, f := range facts {
		if f.Key == summaryFactKey {
			ff := f
			summaryFact = &ff
			continue
		}
		text := f.Key + ": " + f.Value
		if alreadyVisible(f.Value, recent) || alreadyVisible(text, recent) {
			continue
		}
		score := sharedTokenCount(queryTokens, contentTokens(text))
		if score < 1 {
			continue
		}
		cands = append(cands, recallCandidate{
			text:    truncateRecallItem(text),
			score:   score,
			isFact:  true,
			recency: f.UpdatedAt.UnixNano(),
		})
	}

	// (b) Declarative sentences from older user messages: noisy, so
	// they need at least two shared tokens.
	var order int64
	for _, m := range older {
		if m.Role != "user" {
			continue
		}
		for _, s := range declarativeSentences(m.Content) {
			order++
			if alreadyVisible(s, recent) {
				continue
			}
			score := sharedTokenCount(queryTokens, contentTokens(s))
			if score < 2 {
				continue
			}
			cands = append(cands, recallCandidate{
				text:    truncateRecallItem(s),
				score:   score,
				recency: order,
			})
		}
	}

	// Best score first; on ties, facts before message sentences,
	// then most recent first. SliceStable keeps ListFacts order for
	// facts whose UpdatedAt is identical (e.g. zero in tests).
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		if cands[i].isFact != cands[j].isFact {
			return cands[i].isFact
		}
		return cands[i].recency > cands[j].recency
	})
	if len(cands) > maxRecallItems {
		cands = cands[:maxRecallItems]
	}

	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.text
	}

	// (a2) Latest conversation-summary section, in its own slot:
	// it still needs a topical match (one shared content token),
	// but when it matches the model gets the latest section at
	// the raised cap instead of a 200-rune fragment — and it never
	// displaces a precisely-matching fact or sentence. The latest
	// section is the newest "## <timestamp>" section; a manual
	// /compress summary (no header) is a single section.
	if summaryFact != nil {
		latest := latestSummarySection(summaryFact.Value)
		if latest != "" && !alreadyVisible(latest, recent) {
			if sharedTokenCount(queryTokens, contentTokens(latest)) >= 1 {
				out = append(out, truncateRecallItemTo(latest, maxSummaryRecallRunes))
			}
		}
	}
	return out
}

// latestSummarySection returns the newest section of a
// conversation-summary fact (the last "## <timestamp>" section, or
// the whole value when it has no headers).
func latestSummarySection(value string) string {
	sections := splitSummarySections(value)
	if len(sections) == 0 {
		return ""
	}
	return strings.TrimSpace(sections[len(sections)-1])
}

// recallFor gathers both recall sources for the current input and
// returns the ranked items to inject. recent is the window already
// included in the run's messages. The full history is fetched to find
// messages outside that window; the current input (just stored) is
// excluded from the older set. A failure reading either source logs
// and degrades to the other source (or none), never failing the run.
func (a *Agent) recallFor(input string, recent []model.Message) []string {
	facts, err := a.memory.ListFacts()
	if err != nil {
		a.logf("memory ListFacts failed (continuing without fact recall): %v", err)
		facts = nil
	}
	full, err := a.memory.RecentMessages(0)
	if err != nil {
		a.logf("memory RecentMessages(0) failed (continuing without message recall): %v", err)
		return recallItems(input, facts, nil, recent)
	}
	// older = full history minus the current input (last, just stored)
	// minus the recent window (its tail).
	older := full
	if n := len(older); n > 0 && older[n-1].Role == "user" && older[n-1].Content == input {
		older = older[:n-1]
	}
	if len(older) > len(recent) {
		older = older[:len(older)-len(recent)]
	} else {
		older = nil
	}
	return recallItems(input, facts, older, recent)
}

// formatRecallSection renders items as the section appended to the
// system message. It returns "" when there is nothing to inject, so
// callers can leave the system prompt byte-identical in that case.
func formatRecallSection(items []string) string {
	if len(items) == 0 {
		return ""
	}
	lines := make([]string, len(items))
	for i, it := range items {
		lines[i] = "- " + it
	}
	return recallSectionHeader + strings.Join(lines, "\n")
}

// contentTokens lowercases s, splits on non-alphanumerics and drops
// stopwords, returning the set of remaining tokens.
func contentTokens(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, tok := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if _, stop := recallStopwords[tok]; !stop {
			out[tok] = struct{}{}
		}
	}
	return out
}

// sharedTokenCount is the overlap size of two content-token sets.
func sharedTokenCount(a, b map[string]struct{}) int {
	if len(a) > len(b) {
		a, b = b, a
	}
	n := 0
	for tok := range a {
		if _, ok := b[tok]; ok {
			n++
		}
	}
	return n
}

// declarativeSentences splits text into sentences and keeps only
// declarative ones: questions and instruction/tool sentences are
// dropped, since they state no fact worth recalling.
func declarativeSentences(text string) []string {
	var out []string
	for _, m := range sentencePattern.FindAllString(text, -1) {
		s := strings.TrimSpace(m)
		if s == "" {
			continue
		}
		if strings.HasSuffix(s, "?") {
			continue
		}
		// Sentences are lowercased before matching (the pattern is
		// lowercase), so "Write ..." and "write ..." match alike.
		if instructionPattern.MatchString(strings.ToLower(s)) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// alreadyVisible reports whether every content token of text already
// appears in some recent message. Token containment (rather than
// substring search) avoids false positives such as the fact value
// "17" matching a recent "2017". A text with no content tokens is
// never considered visible.
func alreadyVisible(text string, recent []model.Message) bool {
	want := contentTokens(text)
	if len(want) == 0 {
		return false
	}
	for _, m := range recent {
		have := contentTokens(m.Content)
		visible := true
		for tok := range want {
			if _, ok := have[tok]; !ok {
				visible = false
				break
			}
		}
		if visible {
			return true
		}
	}
	return false
}

// truncateRecallItem cuts an item to maxRecallItemRunes runes.
func truncateRecallItem(s string) string {
	return truncateRecallItemTo(s, maxRecallItemRunes)
}

// truncateRecallItemTo cuts an item to at most n runes.
func truncateRecallItemTo(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
