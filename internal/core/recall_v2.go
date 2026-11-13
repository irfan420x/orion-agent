// Recall v2: typed-memory manifest + small-provider side-query selector
// with keyword fallback, drift-checked before injection.
//
// This extends the v1 token-overlap recall in recall.go; it does not
// replace it. v1 facts/message sentences are still injected first (up
// to 3); typed memories add at most 5 more, under their own section
// header, so the model can tell recorded guidance apart from older
// conversation.
//
// Contract with workstream A (internal/memory, built in parallel):
// the typed store is expected to expose
//
//	func (s *FileStore) ListTypedMemories() ([]TypedMemory, error)
//	func (s *FileStore) LoadTypedMemory(name string) (TypedMemory, error)
//
// with TypedMemory carrying Name, Description, Type ("user" |
// "feedback" | "project" | "reference"), Body, Path and UpdatedAt.
// That API was NOT present in internal/memory when this file was
// written, so the shapes are defined locally here (TypedMemory,
// typedMemorySource) with exactly the contracted names and fields.
// When workstream A lands, the coordinator wires Agent.typedMem to
// the real store: either by aliasing these types to memory.TypedMemory
// or by adapting — no recall logic changes. The same applies to the
// session-memory append side, defined here as sessionMemoryAppender
// (Append); see extraction.go.
//
// Recall v2 never fails a run: every error in the manifest, selector
// or drift check degrades to fewer (or no) typed items with a log
// line, never an error return to the loop.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/irfan/orion/internal/model"
)

// maxTypedMemories caps how many typed-memory headers the manifest
// scan considers, newest first (Claude Code's manifest cap).
const maxTypedMemories = 200

// maxTypedRecallItems caps how many typed memories one turn injects.
const maxTypedRecallItems = 5

// maxFeedbackMemories caps how many feedback memories are injected
// as standing guidance each turn (see recallTypedFor).
const maxFeedbackMemories = 2

// maxSelectorTokens is the tiny max_tokens budget for the side-query
// selector call: it only has to return a short JSON list of names.
const maxSelectorTokens = 256

// typedMemorySectionHeader introduces injected typed memories in the
// system message, distinct from the v1 "Relevant things the user
// told you earlier" section.
const typedMemorySectionHeader = "\n\nRelevant saved memories (typed memories — recorded guidance from the user, prefer them over guessing):\n"

// TypedMemory is one typed memory file: markdown with frontmatter
// (name, description, type) plus a body. This is the contracted
// shape from workstream A; see the package note above. When
// internal/memory lands its own TypedMemory, this type is replaced
// by an alias to it.
type TypedMemory struct {
	Name        string
	Description string
	Type        string // "user" | "feedback" | "project" | "reference"
	Body        string
	Path        string
	UpdatedAt   time.Time
}

// typedMemorySource is the read side of the typed-memory store
// (contracted from workstream A). The Agent holds one only when a
// caller wires it via SetTypedMemorySource; a nil source disables
// recall v2 entirely and v1 recall carries on alone.
type typedMemorySource interface {
	ListTypedMemories() ([]TypedMemory, error)
	LoadTypedMemory(name string) (TypedMemory, error)
}

// sessionMemoryAppender is the append side of the session-memory
// file (see extraction.go). Its single method matches
// memory.SessionLog.Append exactly, so once internal/memory
// compiles the coordinator can wire a *memory.SessionLog here with
// no adapter.
type sessionMemoryAppender interface {
	Append(entry string) error
}

// memorySelectorFunc picks typed memories for the current input. It
// receives the manifest (newest first, already capped), the user
// input, the names of recently-used tools, and the names already
// surfaced this session (to exclude). It returns the selected memory
// names, best first, at most maxTypedRecallItems. Any error means
// "selector unavailable" and the caller falls back to keyword
// scoring; it never fails the turn.
type memorySelectorFunc func(ctx context.Context, manifest []TypedMemory, input string, recentTools []string, surfaced []string) ([]string, error)

// SetTypedMemorySource wires the typed-memory store for recall v2.
// A nil source disables typed recall. It never blocks on a running
// run: recall reads the pointer under surfacedMu-free atomic-ish
// access via a plain assignment guarded by the same lock used in
// recallTypedFor — in practice sources are wired once at startup,
// before any run.
func (a *Agent) SetTypedMemorySource(src typedMemorySource) {
	a.typedMu.Lock()
	defer a.typedMu.Unlock()
	a.typedMem = src
}

// SetSessionMemoryAppender wires the session-memory append target
// for background extraction (see extraction.go). A nil appender
// disables extraction.
func (a *Agent) SetSessionMemoryAppender(app sessionMemoryAppender) {
	a.typedMu.Lock()
	defer a.typedMu.Unlock()
	a.sessionMem = app
}

// SetDriftRoot overrides the tree root drift verification checks
// file/function/flag references against. Empty resets to the process
// working directory at check time.
func (a *Agent) SetDriftRoot(root string) {
	a.typedMu.Lock()
	defer a.typedMu.Unlock()
	a.driftRoot = root
}

// SetMemorySelector replaces the side-query selector (default: the
// provider-based selector). Tests inject a fake here; passing nil
// restores the default.
func (a *Agent) SetMemorySelector(fn memorySelectorFunc) {
	a.typedMu.Lock()
	defer a.typedMu.Unlock()
	a.memorySelector = fn
}

// typedMemoryManifest builds the recall manifest from a list of typed
// memories: newest first (UpdatedAt descending, name ascending as a
// deterministic tiebreak), capped at maxTypedMemories. It is a pure
// function over headers so it is unit-testable without disk. Entries
// with an empty name are dropped — a memory without a name cannot be
// selected by name.
func typedMemoryManifest(all []TypedMemory) []TypedMemory {
	out := make([]TypedMemory, 0, len(all))
	for _, m := range all {
		if strings.TrimSpace(m.Name) == "" {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > maxTypedMemories {
		out = out[:maxTypedMemories]
	}
	return out
}

// surfacedMemories returns the names already surfaced this session,
// as a sorted snapshot. The set lives on the Agent for the process
// lifetime (a session here is the daemon's lifetime); the selector
// excludes these so a memory is not re-injected turn after turn.
func (a *Agent) surfacedMemories() []string {
	a.surfacedMu.Lock()
	defer a.surfacedMu.Unlock()
	out := make([]string, 0, len(a.surfaced))
	for name := range a.surfaced {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// markSurfaced records names as surfaced this session.
func (a *Agent) markSurfaced(names []string) {
	if len(names) == 0 {
		return
	}
	a.surfacedMu.Lock()
	defer a.surfacedMu.Unlock()
	if a.surfaced == nil {
		a.surfaced = map[string]struct{}{}
	}
	for _, n := range names {
		a.surfaced[n] = struct{}{}
	}
}

// isStaleMemory reports whether a memory was already marked stale by
// drift verification (in-memory set; see markStaleMemory).
func (a *Agent) isStaleMemory(name string) bool {
	a.staleMu.Lock()
	defer a.staleMu.Unlock()
	_, ok := a.stale[name]
	return ok
}

// markStaleMemory records that a recalled memory's references no
// longer exist in the current tree, so it is never injected again
// this session. If the typed store offers a persistent stale marker
// (MarkTypedMemoryStale), it is called best-effort; workstream A's
// API has no such method yet, so the in-memory set is the marker
// until the coordinator wires persistence — that gap is reported,
// not papered over.
func (a *Agent) markStaleMemory(name string) {
	a.staleMu.Lock()
	if a.stale == nil {
		a.stale = map[string]struct{}{}
	}
	a.stale[name] = struct{}{}
	a.staleMu.Unlock()
	if src := a.typedSource(); src != nil {
		if marker, ok := src.(interface{ MarkTypedMemoryStale(string) error }); ok {
			if err := marker.MarkTypedMemoryStale(name); err != nil {
				a.logf("typed memory %q: persistent stale mark failed: %v", name, err)
			}
		}
	}
}

// typedSource returns the wired typed-memory source, or nil.
func (a *Agent) typedSource() typedMemorySource {
	a.typedMu.RLock()
	defer a.typedMu.RUnlock()
	return a.typedMem
}

// sessionAppender returns the wired session-memory appender, or nil.
func (a *Agent) sessionAppender() sessionMemoryAppender {
	a.typedMu.RLock()
	defer a.typedMu.RUnlock()
	return a.sessionMem
}

// driftRootFor returns the configured drift root, or "" meaning
// "process working directory" (resolved by driftCheck).
func (a *Agent) driftRootFor() string {
	a.typedMu.RLock()
	defer a.typedMu.RUnlock()
	return a.driftRoot
}

// memorySelectorFor returns the configured selector, or the default
// provider-based one.
func (a *Agent) memorySelectorFor() memorySelectorFunc {
	a.typedMu.RLock()
	defer a.typedMu.RUnlock()
	if a.memorySelector != nil {
		return a.memorySelector
	}
	return a.defaultMemorySelector
}

// recentToolNames returns the tools executed on recent turns, most
// recent last, capped at 8. The selector uses these to avoid
// re-surfacing usage docs for tools already in active use.
func (a *Agent) recentToolNames() []string {
	a.recentToolsMu.Lock()
	defer a.recentToolsMu.Unlock()
	return append([]string(nil), a.recentTools...)
}

// noteToolCallExecuted records a tool execution for the
// recently-used-tools list (bounded, most recent last, deduped
// consecutively) and for the session-extraction counter.
func (a *Agent) noteToolCallExecuted(toolName string) {
	a.recentToolsMu.Lock()
	if len(a.recentTools) == 0 || a.recentTools[len(a.recentTools)-1] != toolName {
		a.recentTools = append(a.recentTools, toolName)
		if len(a.recentTools) > 8 {
			a.recentTools = a.recentTools[len(a.recentTools)-8:]
		}
	}
	a.recentToolsMu.Unlock()

	a.extractMu.Lock()
	a.toolCallsSinceExtract++
	a.extractMu.Unlock()
}

// recallTypedFor selects, drift-checks and returns the typed memories
// to inject for this turn, best first, at most maxTypedRecallItems.
// It never returns an error and never fails a run: a missing source,
// a manifest/selector/drift failure, or an empty manifest all degrade
// to no typed items with a log line. Selected memories are recorded
// as surfaced so the next turn's selector excludes them.
func (a *Agent) recallTypedFor(ctx context.Context, input string, recent []model.Message) []TypedMemory {
	src := a.typedSource()
	if src == nil {
		return nil
	}
	all, err := src.ListTypedMemories()
	if err != nil {
		a.logf("typed memory ListTypedMemories failed (continuing without typed recall): %v", err)
		return nil
	}
	manifest := typedMemoryManifest(all)
	// Drop memories already proven stale this session before
	// selection, so a stale memory never even reaches the selector.
	live := manifest[:0]
	for _, m := range manifest {
		if !a.isStaleMemory(m.Name) {
			live = append(live, m)
		}
	}
	manifest = live
	// Feedback memories are standing guidance (rules on how to
	// work), not topical facts: topical relevance is the wrong
	// filter for them — a style rule governs every turn — and the
	// surfaced exclusion would silence a rule after its first
	// turn. They bypass the selector and the surfaced set (but
	// still go through drift verification below), capped at
	// maxFeedbackMemories. The rest go through the selector.
	var feedback []TypedMemory
	var topical []TypedMemory
	for _, m := range manifest {
		if m.Type == "feedback" {
			feedback = append(feedback, m)
		} else {
			topical = append(topical, m)
		}
	}
	if len(feedback) > maxFeedbackMemories {
		feedback = feedback[:maxFeedbackMemories]
	}
	if len(feedback) == 0 && len(topical) == 0 {
		return nil
	}
	byName := make(map[string]TypedMemory, len(manifest))
	for _, m := range manifest {
		byName[m.Name] = m
	}
	manifest = topical

	surfaced := a.surfacedMemories()
	recentTools := a.recentToolNames()
	selector := a.memorySelectorFor()
	names, selErr := selector(ctx, manifest, input, recentTools, surfaced)
	if selErr != nil {
		a.logf("typed memory selector failed (%v); falling back to keyword scoring", selErr)
		names = fallbackSelectMemories(manifest, input, recentTools, surfaced)
	}
	if len(names) == 0 && len(feedback) == 0 {
		return nil
	}

	// Dedupe, resolve, enforce the cap, and apply the tool-usage
	// filter even to selector output (a provider can return a usage
	// doc for a tool already in use; the rule is enforced in Go,
	// not just in the prompt). The surfaced set is enforced here
	// too: a selector that ignores it must not re-inject a memory
	// surfaced earlier this session.
	surfacedSet := make(map[string]struct{}, len(surfaced))
	for _, name := range surfaced {
		surfacedSet[name] = struct{}{}
	}
	var selected []TypedMemory
	seen := map[string]struct{}{}
	for _, name := range names {
		if len(selected) >= maxTypedRecallItems {
			break
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if _, wasSurfaced := surfacedSet[name]; wasSurfaced {
			continue
		}
		m, ok := byName[name]
		if !ok {
			a.logf("typed memory selector returned unknown name %q; dropping", name)
			continue
		}
		if memoryIsToolUsageDoc(m, recentTools) {
			continue
		}
		selected = append(selected, m)
	}
	// Standing guidance first: feedback memories are prepended
	// (deduplicated, within the overall cap) so a rule is never
	// crowded out by topical items.
	if len(feedback) > 0 {
		merged := make([]TypedMemory, 0, len(feedback)+len(selected))
		for _, m := range feedback {
			if _, dup := seen[m.Name]; dup {
				continue
			}
			seen[m.Name] = struct{}{}
			merged = append(merged, m)
			if len(merged) >= maxTypedRecallItems {
				break
			}
		}
		for _, m := range selected {
			if len(merged) >= maxTypedRecallItems {
				break
			}
			merged = append(merged, m)
		}
		selected = merged
	}
	if len(selected) == 0 {
		return nil
	}

	// Drift verification: a selected memory that names a file,
	// function or flag is a claim about the current tree. Verify
	// before injecting; on conflict trust the current state and
	// mark the memory stale instead of injecting it.
	root := a.driftRootFor()
	feedbackSet := make(map[string]struct{}, len(feedback))
	for _, m := range feedback {
		feedbackSet[m.Name] = struct{}{}
	}
	var kept []TypedMemory
	var surfacedNames []string
	for _, m := range selected {
		if stale, missing := driftCheck(root, m); stale {
			a.logf("typed memory %q is stale (missing: %s); dropping and marking stale", m.Name, formatMissingRefs(missing))
			a.markStaleMemory(m.Name)
			continue
		}
		kept = append(kept, m)
		// Standing guidance is not marked surfaced: a rule must
		// recur every turn, not just the first.
		if _, isFeedback := feedbackSet[m.Name]; !isFeedback {
			surfacedNames = append(surfacedNames, m.Name)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	a.markSurfaced(surfacedNames)
	return kept
}

// defaultMemorySelector is the provider-based side-query: it sends
// the manifest headers plus the input to the agent's provider with a
// tiny max_tokens and parses the returned JSON list of names. The
// prompt instructs the model to select only clearly-relevant
// memories, to exclude already-surfaced ones, and not to re-surface
// usage docs for tools already in active use — but to surface
// warnings/gotchas about them. Parse failures and provider errors
// are returned so the caller can fall back to keyword scoring.
func (a *Agent) defaultMemorySelector(ctx context.Context, manifest []TypedMemory, input string, recentTools []string, surfaced []string) ([]string, error) {
	return selectMemoriesWithProvider(ctx, a.provider, manifest, input, recentTools, surfaced)
}

// selectMemoriesWithProvider runs one tiny provider call to pick
// relevant typed memories. It takes the provider explicitly so tests
// can inject a fake. The response must be a JSON array of memory
// names; anything else is an error (the caller falls back).
func selectMemoriesWithProvider(ctx context.Context, provider model.Provider, manifest []TypedMemory, input string, recentTools []string, surfaced []string) ([]string, error) {
	if provider == nil {
		return nil, fmt.Errorf("no provider available for memory selection")
	}
	if len(manifest) == 0 {
		return nil, nil
	}
	prompt := selectorPrompt(manifest, input, recentTools, surfaced)
	temp := 0.0
	resp, err := provider.Chat(ctx, model.ChatRequest{
		Messages: []model.Message{
			{Role: "system", Content: "You select which saved memories are relevant to the user's current message. Reply with ONLY a JSON array of memory names, e.g. [\"name-one\", \"name-two\"]. No other text."},
			{Role: "user", Content: prompt},
		},
		MaxTokens:   maxSelectorTokens,
		Temperature: &temp,
	})
	if err != nil {
		return nil, fmt.Errorf("memory selector provider call: %w", err)
	}
	if resp == nil {
		return nil, fmt.Errorf("memory selector provider returned nil response")
	}
	return parseSelectorResponse(resp.Message.Content, manifest)
}

// selectorPrompt renders the manifest and the selection rules for
// the side-query. Headers only (name, type, description, updated)
// — bodies are not sent, keeping the call cheap.
func selectorPrompt(manifest []TypedMemory, input string, recentTools []string, surfaced []string) string {
	var b strings.Builder
	b.WriteString("Saved memories (newest first):\n")
	for _, m := range manifest {
		fmt.Fprintf(&b, "- %q (type %s, updated %s): %s\n", m.Name, m.Type, m.UpdatedAt.Format("2006-01-02"), m.Description)
	}
	fmt.Fprintf(&b, "\nCurrent user message: %q\n", input)
	if len(recentTools) > 0 {
		fmt.Fprintf(&b, "Tools recently in active use: %s.\n", strings.Join(recentTools, ", "))
	}
	if len(surfaced) > 0 {
		fmt.Fprintf(&b, "Memories already surfaced this session (do NOT select these again): %s.\n", strings.Join(surfaced, ", "))
	}
	b.WriteString(`Select at most 5 memories that are CLEARLY relevant to the current message. ` +
		`If none is clearly relevant, return []. ` +
		`Do NOT re-surface usage documentation for a tool already in active use, but DO surface a memory if it contains a warning, gotcha or pitfall about such a tool. ` +
		`Reply with ONLY a JSON array of memory names.`)
	return b.String()
}

// parseSelectorResponse extracts a JSON array of strings from the
// provider's reply, defensively: it finds the first '[' ... last ']'
// span (the model may wrap the array in prose), drops entries that
// are not known manifest names or are duplicates, and caps at
// maxTypedRecallItems. A reply with no JSON array at all is an
// error, so the caller falls back to keyword scoring instead of
// silently injecting nothing relevant.
func parseSelectorResponse(content string, manifest []TypedMemory) ([]string, error) {
	start := strings.Index(content, "[")
	end := strings.LastIndex(content, "]")
	if start < 0 || end < 0 || end <= start {
		return nil, fmt.Errorf("memory selector response contains no JSON array: %q", truncateRecallItem(content))
	}
	var raw []string
	if err := json.Unmarshal([]byte(content[start:end+1]), &raw); err != nil {
		return nil, fmt.Errorf("memory selector response is not a JSON array of names: %w", err)
	}
	known := make(map[string]struct{}, len(manifest))
	for _, m := range manifest {
		known[m.Name] = struct{}{}
	}
	var out []string
	seen := map[string]struct{}{}
	for _, name := range raw {
		if len(out) >= maxTypedRecallItems {
			break
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if _, ok := known[name]; !ok {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// fallbackSelectMemories is the keyword fallback: it reuses the v1
// token-overlap scorer (contentTokens / sharedTokenCount from
// recall.go — the scorer is not duplicated) over each memory's name,
// description and type. One shared content token suffices (a typed
// memory is deliberate, like a v1 fact). Selection order is score
// descending, then newest first. The surfaced set and the
// tool-usage-doc rule are applied here too, and the result is
// deduplicated and capped at maxTypedRecallItems.
func fallbackSelectMemories(manifest []TypedMemory, input string, recentTools []string, surfaced []string) []string {
	queryTokens := contentTokens(input)
	if len(queryTokens) == 0 {
		return nil
	}
	excluded := make(map[string]struct{}, len(surfaced))
	for _, name := range surfaced {
		excluded[name] = struct{}{}
	}
	type scored struct {
		name  string
		score int
		when  time.Time
	}
	var cands []scored
	for _, m := range manifest {
		if _, skip := excluded[m.Name]; skip {
			continue
		}
		if memoryIsToolUsageDoc(m, recentTools) {
			continue
		}
		text := m.Name + " " + m.Description + " " + m.Type
		if score := sharedTokenCount(queryTokens, contentTokens(text)); score >= 1 {
			cands = append(cands, scored{name: m.Name, score: score, when: m.UpdatedAt})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].when.After(cands[j].when)
	})
	if len(cands) > maxTypedRecallItems {
		cands = cands[:maxTypedRecallItems]
	}
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.name
	}
	return out
}

// warningWords mark a memory as a warning/gotcha memory rather than
// a usage doc. Warnings about a tool in active use are still
// surfaced; plain usage docs for that tool are not.
var warningWords = []string{"warning", "gotcha", "pitfall", "caution", "watch out", "never ", "do not ", "don't ", "beware", "trap"}

// memoryIsToolUsageDoc reports whether m is a usage document for one
// of the recently-used tools (and not a warning about it). Matching
// is by tool name appearing in the memory's name or description.
// A memory with no tool overlap is never a usage doc.
func memoryIsToolUsageDoc(m TypedMemory, recentTools []string) bool {
	if len(recentTools) == 0 {
		return false
	}
	haystack := strings.ToLower(m.Name + " " + m.Description + " " + m.Body)
	for _, w := range warningWords {
		if strings.Contains(haystack, w) {
			return false
		}
	}
	for _, tool := range recentTools {
		tool = strings.ToLower(strings.TrimSpace(tool))
		if tool == "" {
			continue
		}
		if strings.Contains(strings.ToLower(m.Name+" "+m.Description), tool) {
			return true
		}
	}
	return false
}

// formatTypedMemorySection renders typed memories as the section
// appended to the system message. Each entry carries its type, name,
// description and a truncated body so the model sees the guidance,
// not just a pointer. It returns "" when there is nothing to inject.
// typedFeedbackSectionHeader introduces standing guidance
// (feedback memories) in the system message. It is phrased as an
// instruction, not a recollection, because a rule governs every
// reply — burying it under "relevant memories" is why live models
// ignore it.
const typedFeedbackSectionHeader = "\n\nStanding guidance from the user — follow these on every reply:\n"

func formatTypedMemorySection(mems []TypedMemory) string {
	if len(mems) == 0 {
		return ""
	}
	var feedback, rest []TypedMemory
	for _, m := range mems {
		if m.Type == "feedback" {
			feedback = append(feedback, m)
		} else {
			rest = append(rest, m)
		}
	}
	var b strings.Builder
	if len(feedback) > 0 {
		b.WriteString(typedFeedbackSectionHeader)
		for _, m := range feedback {
			b.WriteString(formatTypedMemoryLine(m) + "\n")
		}
	}
	if len(rest) > 0 {
		b.WriteString(typedMemorySectionHeader)
		for _, m := range rest {
			b.WriteString(formatTypedMemoryLine(m) + "\n")
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// formatTypedMemoryLine renders one memory as
// "- [type] name: description — body".
func formatTypedMemoryLine(m TypedMemory) string {
	body := strings.TrimSpace(m.Body)
	if body != "" {
		body = " — " + truncateRecallItem(body)
	}
	return "- [" + m.Type + "] " + m.Name + ": " + m.Description + body
}
