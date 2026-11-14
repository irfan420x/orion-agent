// Package skills provides a registry of Markdown skill files.
//
// A skill is a Markdown document whose first "# " heading is its name,
// whose first paragraph after the heading is its description, and whose
// optional "## Tools" section lists the tools the skill may use. The
// registry loads skills from a directory of *.md files (or a single file),
// keeps them in memory, and supports lookup, listing, and search.
//
// TRUST MODEL (Audit 5 F-A5-8): a skill file is untrusted data, not
// Orion instructions. The "## Tools" list is an anti-drift aid for the
// model — it keeps a skill on its intended tools — not a security
// boundary against skill content: a skill body can always ask the
// model to end the skill or switch skills, and the agent loop lets
// the model do that on its own initiative. Do not install
// third-party skills you have not read. Discovery loads only regular
// files from the skills directory: symlinks are refused, so a synced
// or checked-out directory cannot smuggle in a skill that lives
// outside it; hardlinked files are refused too, and the file is
// opened (O_NOFOLLOW) before it is stat'ed, so the checks apply to
// the file actually read (Audit 6 F-A6-P2-10). Load time also
// refuses any skill body containing a BEGIN/END SKILL delimiter
// marker line or an activation directive (Audit 6 F-A6-2): the
// delimiters are only labelling, so a body that carries its own
// END marker could close the block early and inject a fake footer
// plus fresh instructions, and the bar against that has to be in
// code at load time, not in a prompt instruction.
package skills

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// maxFileSize caps how much of any one skill file is read into memory.
const maxFileSize = 1 << 20 // 1 MiB

// Skill is a single parsed skill definition.
type Skill struct {
	// Name is the skill name: the first "# " heading, or the file name
	// without its .md extension when the file has no such heading.
	Name string
	// Description is the first non-empty, non-heading paragraph line
	// after the name heading.
	Description string
	// Path is the full path of the file the skill was loaded from.
	Path string
	// Content is the full text of the skill file.
	Content string
	// Tools lists the tool names from the "## Tools" section, if any.
	// Both bulleted ("- exec", "* exec") and numbered ("1. exec",
	// "1) exec") items are recognised. The list is an anti-drift aid,
	// not a security boundary against skill content (see the package
	// doc): nil means the skill declares no restriction.
	Tools []string
}

// Registry is a concurrency-safe store of skills, keyed by name.
type Registry struct {
	mu     sync.RWMutex
	skills map[string]Skill
	// knownTools, when set with SetKnownTools, is the tool-name set
	// ## Tools entries are validated against (F-A17-14). Nil means
	// the built-in set (knownBuiltInTools): a Registry built by
	// hand or by tests validates against the tools Orion ships,
	// and the daemon extends it with the live registry's names
	// (subagent/skill/MCP tools) via loadSkills.
	knownTools map[string]bool
}

// knownBuiltInTools is the set of tool names Orion ships (the
// tools DefaultRegistry registers, plus the "skill" tool itself,
// which a skill may always name — the agent loop never blocks it).
// It is the validation baseline for ## Tools (F-A17-14): a name
// outside this set (and outside any SetKnownTools extension, e.g.
// MCP tools) warns at load time instead of silently never
// matching a real tool. UPDATE THIS LIST when a built-in tool is
// added, renamed or removed — the list is data, not derived from
// the tools package, because tools imports skills (the skill tool
// looks skills up), so skills cannot import tools back.
var knownBuiltInTools = map[string]bool{
	"ask_user_question": true,
	"calculator":        true, "datetime": true,
	"file_edit": true, "file_list": true, "file_read": true, "file_write": true,
	"glob": true, "grep": true,
	"process_list":  true,
	"shell_exec":    true,
	"skill":         true,
	"subagent_list": true, "subagent_result": true, "subagent_spawn": true, "subagent_stop": true,
	"task_create": true, "task_get": true, "task_list": true, "task_update": true,
	"todo_write": true,
	"web_fetch":  true, "web_search": true,
}

// KnownBuiltInTools returns the names of every built-in tool
// Orion ships, sorted — the fallback list above. The live set is
// derived from the tool registry at startup (app.loadSkills
// calls SetKnownTools with the registry's names); this fallback
// exists only for hand-built registries that never load skills.
// A test in internal/tools pins it to the factory set, so adding
// a tool without updating the fallback fails loudly instead of
// silently letting a skill call an unknown tool. The returned
// slice is a copy: mutating it cannot change the set.
func KnownBuiltInTools() []string {
	out := make([]string, 0, len(knownBuiltInTools))
	for name := range knownBuiltInTools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// SetKnownTools extends the tool-name set ## Tools entries are
// validated against (F-A17-14): names is added to the built-in set
// (never replaces it — "skill" and the built-ins stay known), so a
// caller that knows more tools than the built-ins (the daemon,
// whose live registry also holds MCP tools) can pass them in
// without re-listing the built-ins. It is additive and safe to call
// before or after loading: skills loaded before the call keep the
// warnings they were loaded with, and Reload re-validates against
// the extended set. A nil/empty names is a no-op. The names are
// copied, so later mutation of the caller's slice cannot change
// the set.
func (r *Registry) SetKnownTools(names []string) {
	if r == nil || len(names) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.knownTools == nil {
		r.knownTools = make(map[string]bool, len(knownBuiltInTools)+len(names))
		for name := range knownBuiltInTools {
			r.knownTools[name] = true
		}
	}
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			r.knownTools[name] = true
		}
	}
}

// knownSet returns a copy of the tool-name set to validate ## Tools
// against: the registry's extension when set, else the built-in
// set. It is a copy so a concurrent SetKnownTools cannot race a
// load that is reading the set.
func (r *Registry) knownSet() map[string]bool {
	if r == nil {
		return knownBuiltInTools
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.knownTools == nil {
		return knownBuiltInTools
	}
	out := make(map[string]bool, len(r.knownTools))
	for name := range r.knownTools {
		out[name] = true
	}
	return out
}

// NewRegistry returns an empty skill registry.
func NewRegistry() *Registry {
	return &Registry{skills: make(map[string]Skill)}
}

// clone returns a copy of s whose Tools slice does not share its
// backing array with the original, so a caller mutating the returned
// skill can never corrupt the registry (and vice versa).
func (s Skill) clone() Skill {
	if s.Tools != nil {
		tools := make([]string, len(s.Tools))
		copy(tools, s.Tools)
		s.Tools = tools
	}
	return s
}

// Register adds a skill to the registry.
//
// It returns an error when the skill has an empty name or when a skill
// with the same name is already registered. The registry is unchanged
// on error. The skill is copied on entry, so later mutation of the
// caller's Skill (in particular its Tools slice) cannot corrupt the
// registry. A nil Registry is an error, not a panic.
//
// The skill's content is validated the same way file loads are
// (see validateSkillContent): a body containing a BEGIN/END SKILL
// delimiter marker line or an activation directive is refused here
// too, so the registry — which is what the skill tool invokes from
// — can never hold a skill whose body blurs the delimiter boundary,
// however the skill was constructed (Audit 6 F-A6-2).
func (r *Registry) Register(s Skill) error {
	if r == nil {
		return errors.New("skills: Register on nil Registry")
	}
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("skills: cannot register skill with empty name")
	}
	if err := validateSkillContent(s.Content); err != nil {
		return fmt.Errorf("skills: cannot register skill %q: %w", s.Name, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.skills[s.Name]; exists {
		return fmt.Errorf("skills: duplicate skill %q", s.Name)
	}
	r.skills[s.Name] = s.clone()
	return nil
}

// Get returns the skill with the given name and whether it was found.
// The returned Skill is a copy: mutating its Tools slice does not
// affect the registry. A nil Registry reports every name missing.
func (r *Registry) Get(name string) (Skill, bool) {
	if r == nil {
		return Skill{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.skills[name]
	if !ok {
		return Skill{}, false
	}
	return s.clone(), true
}

// List returns all registered skills sorted by name. Each returned
// Skill is a copy (see Get). A nil Registry lists nothing.
func (r *Registry) List() []Skill {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Skill, 0, len(r.skills))
	for _, s := range r.skills {
		out = append(out, s.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Search returns skills whose name or description contains query as a
// case-insensitive substring. An empty query matches every skill.
// Results are sorted by name. A nil Registry matches nothing.
func (r *Registry) Search(query string) []Skill {
	if r == nil {
		return nil
	}
	q := strings.ToLower(strings.TrimSpace(query))
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []Skill{}
	for _, s := range r.skills {
		if q == "" ||
			strings.Contains(strings.ToLower(s.Name), q) ||
			strings.Contains(strings.ToLower(s.Description), q) {
			out = append(out, s.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LoadDir reads every *.md file in dir (non-recursive) and registers the
// skills it can parse. It returns the number of skills loaded.
//
// Symlinks are refused (see the per-file check below): a skills dir
// populated by a sync or checkout that preserves symlinks must not
// load a skill that lives outside the dir.
//
// Files that fail to parse (for example an empty file) are skipped; each
// failure is collected and the collected errors are returned as one joined
// error at the end, while the skills that parsed successfully are still
// loaded. A failure to read the directory itself, or a directory that
// contains no loadable skills and no parse errors (for example no *.md
// files at all), returns 0 and an error describing the problem.
func (r *Registry) LoadDir(dir string) (int, error) {
	if strings.TrimSpace(dir) == "" {
		return 0, errors.New("skills: LoadDir called with empty directory path")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("skills: cannot read directory %q: %w", dir, err)
	}

	var errs []error
	loaded := 0
	sawMD := false
	// The known-tool set is snapshotted once per load, so a
	// concurrent SetKnownTools cannot change validation mid-dir.
	known := r.knownSet()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			continue
		}
		sawMD = true
		path := filepath.Join(dir, entry.Name())
		// Symlinks are refused, not followed: a symlink inside
		// the skills dir pointing anywhere on disk would
		// otherwise load as a skill (Audit 5 F-A5-P2-26), which
		// defeats the documented "loads only from the data dir"
		// guarantee — e.g. for a skills dir populated by a sync
		// or checkout that preserves symlinks. Lstat (not Stat)
		// sees the link itself. The refusal is recorded like
		// any other per-file failure: the remaining skills still
		// load, and the caller logs the joined error.
		if info, err := os.Lstat(path); err != nil {
			errs = append(errs, fmt.Errorf("skills: cannot stat %q: %w", path, err))
			continue
		} else if info.Mode()&os.ModeSymlink != 0 {
			errs = append(errs, fmt.Errorf("skills: refusing symlink %q: skill files must be regular files inside %q", path, dir))
			continue
		}
		skill, err := parseFileWithKnownTools(path, known)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := r.Register(skill); err != nil {
			errs = append(errs, fmt.Errorf("skills: %s: %w", path, err))
			continue
		}
		loaded++
	}

	if !sawMD {
		return 0, fmt.Errorf("skills: no .md files found in %q", dir)
	}
	if len(errs) > 0 {
		return loaded, errors.Join(errs...)
	}
	return loaded, nil
}

// Reload re-reads every *.md file in dir (F-A17-15) without
// restarting the daemon: new files are picked up, changed files
// re-parsed, removed files dropped. It loads into a fresh registry
// first and swaps the contents in only when the load produced at
// least one skill: a directory that cannot be read at all, or one
// with no loadable skills, leaves the current registry untouched
// (the previously loaded skills stay live) and returns the error.
// A partial load (some files broken) still swaps — the good files
// are the directory's current truth — and returns the joined error
// alongside the count, so the caller can report both. The swap is
// one lock hold, so readers never see a half-reloaded registry,
// and the registry pointer itself never changes: the agent and the
// skill tool keep working on the same Registry. The fresh load is
// validated against this registry's known-tool set (see
// SetKnownTools), so a reload re-warns about unknown tools.
func (r *Registry) Reload(dir string) (int, error) {
	if r == nil {
		return 0, errors.New("skills: Reload on nil Registry")
	}
	fresh := NewRegistry()
	// Carry the known-tool extension over, so the reload
	// re-validates against the same set (a nil extension means
	// the built-in default, which fresh already uses).
	r.mu.RLock()
	if r.knownTools != nil {
		cp := make(map[string]bool, len(r.knownTools))
		for name := range r.knownTools {
			cp[name] = true
		}
		fresh.knownTools = cp
	}
	r.mu.RUnlock()
	loaded, err := fresh.LoadDir(dir)
	if loaded == 0 && err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.skills = fresh.skills
	r.mu.Unlock()
	return loaded, err
}

// LoadFile parses a single skill file and returns it. The skill is not
// registered; use Register to add it to the registry.
func (r *Registry) LoadFile(path string) (Skill, error) {
	return parseFileWithKnownTools(path, r.knownSet())
}

// parseFile reads one skill file (capped at maxFileSize) and parses it.
// Symlinks are refused here too, so the single-file LoadFile path has
// the same guarantee as LoadDir (Audit 5 F-A5-P2-26).
//
// The file is opened first and stat'ed second, from the open file
// descriptor (Audit 6 F-A6-P2-10): the old Lstat-then-Open order let
// the path be swapped between the check and the read. Opening with
// O_NOFOLLOW refuses a symlink at open time (ELOOP) instead of
// following it, O_NONBLOCK keeps a FIFO in the skills dir from
// hanging the open, and the fstat on the opened descriptor is what
// the regular-file and hardlink (nlink > 1) refusals are based on —
// so every check describes the file actually read, not the path
// that was checked a moment earlier. LoadDir's own Lstat pre-check
// is only an early, clearer error; the checks here are authoritative.
func parseFile(path string) (Skill, error) {
	return parseFileWithKnownTools(path, knownBuiltInTools)
}

// parseFileWithKnownTools is parseFile validated against known: the
// tool names in the file's "## Tools" section are checked against
// it (see parseSkillWithKnownTools), and unknown names warn instead
// of failing the load.
func parseFileWithKnownTools(path string, known map[string]bool) (Skill, error) {
	if strings.TrimSpace(path) == "" {
		return Skill{}, errors.New("skills: empty file path")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return Skill{}, fmt.Errorf("skills: refusing symlink %q: skill files must be regular files", path)
		}
		return Skill{}, fmt.Errorf("skills: cannot open %q: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Skill{}, fmt.Errorf("skills: cannot stat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return Skill{}, fmt.Errorf("skills: refusing non-regular file %q (mode %s): skill files must be regular files", path, info.Mode())
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return Skill{}, fmt.Errorf("skills: refusing hardlinked file %q (link count %d): skill files must not be hardlinked", path, st.Nlink)
	}

	// LimitReader caps memory use; reading one byte past the cap lets us
	// detect an oversized file instead of silently truncating it.
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return Skill{}, fmt.Errorf("skills: cannot read %q: %w", path, err)
	}
	if len(data) > maxFileSize {
		return Skill{}, fmt.Errorf("skills: file %q exceeds %d byte limit", path, maxFileSize)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return Skill{}, fmt.Errorf("skills: file %q is empty", path)
	}
	// A body that carries its own delimiter marker (or an
	// activation directive) must never load: it could close the
	// BEGIN/END block early at invoke time and inject a fake
	// footer plus fresh instructions (Audit 6 F-A6-2).
	if err := validateSkillContent(string(data)); err != nil {
		return Skill{}, fmt.Errorf("skills: file %q: %w", path, err)
	}
	skill, warnings := parseSkillWithKnownTools(path, string(data), known)
	for _, w := range warnings {
		log.Printf("orion: skills: %s: %s", path, w)
	}
	if strings.TrimSpace(skill.Name) == "" {
		// Reachable when the filename itself yields no name (e.g. a
		// file literally called ".md") and the file has no "# " heading:
		// an empty-named skill could never be registered, so fail here
		// instead of returning an unusable Skill.
		return Skill{}, fmt.Errorf("skills: file %q has no skill name (no \"# \" heading and the file name yields none)", path)
	}
	return skill, nil
}

// skillMarkerLine matches a line that parses as a skill-content
// delimiter: the exact form formatSkillBody emits
// (===== BEGIN SKILL "name" CONTENT ===== /
// ===== END SKILL "name" CONTENT =====), with the name quoted or
// bare. The match is deliberately fuzzy, not exact (Audit 7
// F-A7-P2-1, widened by Audit 8 F-A8-P2-5 and Audit 9 F-A9-P2-5):
// a model's reading is fuzzy, so the delimiter is recognised by
// its OPENING, not by an exact spelling. The old 4-6 "=" bound was
// arbitrary — 3- and 7-equals variants loaded — so any run of a
// delimiter character ("=", "-" or "*", one or more) counts; the
// spaces around BEGIN/END are optional, including between END and
// SKILL ("ENDSKILL" loaded on rc12), and "-" or "_" may join the
// marker words instead ("END-SKILL", "BEGIN_SKILL" loaded on
// rc13); the word is SKILL or SKILLS; and nothing more is
// required. CONTENT is optional, a closing delimiter run is
// optional, and trailing text is irrelevant: a line that opens
// like a delimiter reads as one to the model even truncated
// ("===== END SKILL \"x\" CONTENT") or annotated ("... CONTENT
// (continued) ====="). A list-item prefix ("- ", "* ", "1. ")
// before the delimiter run does not hide the marker either
// ("- ===== END SKILL ..." loaded on rc13). The match is
// anchored at the start of the (trimmed, quote-stripped) line,
// apart from that optional list prefix: a marker merely mentioned
// mid-line ("See the ===== END SKILL ... format") does not match,
// and ordinary prose that merely mentions SKILL does not match —
// there must be a leading delimiter run followed by BEGIN/END
// SKILL. The word boundaries matter: "SKILLFUL" and "ENDLESS"
// are different words and do not match.
var skillMarkerLine = regexp.MustCompile(`(?i)^(?:[-*+]\s+|\d+[.)]\s+)?[=\-*]+\s*(BEGIN|END)[-_\s]*SKILLS?\b`)

// stripMarkerQuotePrefix removes leading Markdown blockquote
// markers ("> ") from a trimmed line, so a quoted marker line
// ("> ===== END SKILL ... =====") is still recognised as a
// marker: quoting does not stop the line reading as a delimiter.
// Repeated markers (">> ...") are stripped too.
func stripMarkerQuotePrefix(line string) string {
	s := line
	for strings.HasPrefix(s, ">") {
		s = strings.TrimSpace(strings.TrimPrefix(s, ">"))
	}
	return s
}

// validateSkillContent rejects skill text that can blur the
// BEGIN/END delimiter boundary (Audit 6 F-A6-2, fuzzy variants
// Audit 7 F-A7-P2-1, widened Audit 8 F-A8-P2-5 and Audit 9
// F-A9-P2-5): any line that parses as a BEGIN/END SKILL marker (case-insensitive, any "="
// run, optional spaces, optionally blockquoted, CONTENT and the
// closing run optional), and any line shaped as an activation
// directive (see isActivateDirective). The delimiters are only
// labelling — a body containing its own END marker can close the
// block early and inject a fake footer plus fresh instructions,
// and a body-directed switch ("END SKILL" + "BEGIN SKILL
// \"other\"" + "Activate") is the same attack one step further
// along — so the bar has to be in code at load time, not in the
// prompt's hierarchy instruction, which is advisory. Skills are
// operator-installed, so this is third-party-skill risk; a
// legitimate skill never needs a marker line or an activation
// directive in its body, and the error names the offending line so
// the author can rephrase it instead of guessing.
func validateSkillContent(content string) error {
	for i, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if skillMarkerLine.MatchString(stripMarkerQuotePrefix(trimmed)) {
			return fmt.Errorf("line %d contains a skill delimiter marker %q: skill bodies must not contain BEGIN/END SKILL marker lines", i+1, truncateLine(trimmed))
		}
		if isActivateDirective(trimmed) {
			return fmt.Errorf("line %d contains an activation directive %q: skill bodies must not direct skill activation", i+1, truncateLine(trimmed))
		}
	}
	return nil
}

// NeutralizeSkillMarkers rewrites any line in content that parses as
// a BEGIN/END SKILL delimiter marker so it no longer parses as one:
// the line is prefixed with "[neutralised skill marker] ", which
// breaks the line-anchored marker match while keeping the original
// text readable. This is the belt-and-braces second bar behind the
// load-time rejection in validateSkillContent: a Skill constructed
// programmatically (tests, or a future loader path) can never blur
// the delimiter boundary when rendered by tools.formatSkillBody,
// even if it bypasses load-time validation. Activation directives
// are not neutralised here — they are rejected at load time, and
// the renderer's job is the delimiter boundary only.
func NeutralizeSkillMarkers(content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if skillMarkerLine.MatchString(stripMarkerQuotePrefix(strings.TrimSpace(line))) {
			lines[i] = "[neutralised skill marker] " + line
		}
	}
	return strings.Join(lines, "\n")
}

// truncateLine caps a line quoted in an error message, so a
// hostile multi-kilobyte line cannot flood the log.
func truncateLine(line string) string {
	const maxLen = 120
	if len(line) <= maxLen {
		return line
	}
	return line[:maxLen] + "…"
}

// activateWord matches the whole word "activate" and its verb and
// noun forms (any case): activate/activates/activated/activating,
// reactivate/reactivates/reactivated/reactivating, and the -ion
// nouns activation/reactivation (Audit 9 F-A9-P2-4). Re-activating
// is directing activation, so the re- forms must match — the old
// `\bactivate\b` let "Reactivate the skill" and "Start activating
// the skill now" load. The word boundaries still matter:
// "deactivate" and "deactivation" must not match (there is no word
// boundary between "de" and "activate"), and the -ion nouns are
// matched only so isActivateOccurrenceDirective can apply its
// prose guard to them: a line merely discussing activation
// ("Skill activation is manual.") is not a directive.
var activateWord = regexp.MustCompile(`(?i)\b(?:re)?activat(?:e|es|ed|ing|ion)\b`)

// skillWord matches the whole word "skill"/"skills" (any case) in
// the text after "activate".
var skillWord = regexp.MustCompile(`(?i)\bskills?\b`)

// skillToolBigram matches the compound "skill tool" / "skills tools"
// (any case): the sanctioned activation path — activation goes
// through the skill tool.
var skillToolBigram = regexp.MustCompile(`(?i)\bskills?\s+tools?\b`)

// activateVerbForms is the set of verb forms of "activate" (Audit 9
// F-A9-P2-4): the forms a negation can govern, and the forms that
// can direct activation. The -ion nouns are not verbs and are
// deliberately absent: they are handled by the prose guard in
// isActivateOccurrenceDirective.
var activateVerbForms = map[string]bool{
	"activate": true, "activates": true, "activated": true, "activating": true,
	"reactivate": true, "reactivates": true, "reactivated": true, "reactivating": true,
}

// activationDirectiveVerbs is the set of verbs that can govern an
// -ion activation noun as a directive (Audit 9 F-A9-P2-4): "Begin
// activation of the skill" directs activation, while a noun on its
// own ("Reactivation happens automatically") is discussion, not a
// directive.
var activationDirectiveVerbs = map[string]bool{
	"start": true, "starts": true, "started": true, "starting": true,
	"begin": true, "begins": true, "began": true, "begun": true, "beginning": true,
	"trigger": true, "triggers": true, "triggered": true, "triggering": true,
	"initiate": true, "initiates": true, "initiated": true, "initiating": true,
	"commence": true, "commences": true, "commenced": true, "commencing": true,
	"perform": true, "performs": true, "performed": true, "performing": true,
	"request": true, "requests": true, "requested": true, "requesting": true,
	"enable": true, "enables": true, "enabled": true, "enabling": true,
}

// negationSkipWords are the words skipped when finding the verb a
// negation word negates (Audit 9 F-A9-P2-1): the infinitive marker
// "to", adverbs, auxiliaries, and articles. Skipping them lands on
// the lexical verb: "should not ever activate" → "activate";
// "do not forget to activate" → "forget".
var negationSkipWords = map[string]bool{
	"to":   true,
	"ever": true, "really": true, "just": true, "only": true,
	"actually": true, "simply": true, "merely": true,
	"certainly": true, "definitely": true, "even": true, "still": true,
	"be": true, "been": true, "being": true, "am": true, "is": true,
	"are": true, "was": true, "were": true,
	"have": true, "has": true, "had": true,
	"do": true, "does": true, "did": true,
	"the": true, "a": true, "an": true, "this": true, "that": true,
	"these": true, "those": true,
}

// activateObjectClosers are words that end the direct-object region
// after "activate" (Audit 9 F-A9-P2-3): a preposition or
// instrumental head ("using", "with", "via", "by", "through",
// "for", "to", "of", ...), a subordinating conjunction, or a verb
// starting a new predicate ("run", "check", "use", ...). A "skill"
// named only after one of these is the object of that preposition
// or verb, not of "activate", so it does not make the line a
// directive.
var activateObjectClosers = map[string]bool{
	// Prepositions / instrumental heads.
	"using": true, "with": true, "via": true, "by": true, "through": true,
	"for": true, "to": true, "of": true, "in": true, "on": true, "at": true,
	"from": true, "into": true, "over": true, "under": true, "per": true,
	// Subordinating conjunctions: what follows is a new clause.
	"when": true, "while": true, "if": true, "because": true, "before": true,
	"after": true, "unless": true, "although": true, "though": true,
	"since": true, "that": true, "which": true, "who": true, "as": true,
	// Verbs starting a new predicate: "Activate the tool and run
	// the skill" names the skill as the object of "run", not of
	// "activate". ("start"/"begin" are deliberately absent: starting
	// a skill is close enough to activating it that the line errs
	// toward rejection.)
	"run": true, "runs": true, "running": true,
	"check": true, "checks": true, "checking": true,
	"use": true, "uses": true,
	"open": true, "opens": true, "opening": true,
	"close": true, "closes": true, "closing": true,
	"load": true, "loads": true, "loading": true,
	"call": true, "calls": true, "calling": true,
	"invoke": true, "invokes": true, "invoking": true,
	"send": true, "sends": true, "sending": true,
	"get": true, "gets": true, "getting": true,
	"set": true, "sets": true, "setting": true,
	"read": true, "reads": true, "reading": true,
	"write": true, "writes": true, "writing": true,
	"create": true, "creates": true, "creating": true,
	"delete": true, "deletes": true, "deleting": true,
	"list": true, "lists": true, "listing": true,
	"show": true, "shows": true, "showing": true,
	"print": true, "prints": true, "printing": true,
	"deploy": true, "deploys": true, "deploying": true,
	"install": true, "installs": true, "installing": true,
	"configure": true, "configures": true, "configuring": true,
	"test": true, "tests": true, "testing": true,
	"verify": true, "verifies": true, "verifying": true,
	"do": true, "does": true, "did": true,
	"make": true, "makes": true, "making": true,
	"go": true, "goes": true, "going": true,
	"come": true, "comes": true, "coming": true,
	"take": true, "takes": true, "taking": true,
	"give": true, "gives": true, "giving": true,
	"put": true, "puts": true, "putting": true,
	"keep": true, "keeps": true, "keeping": true,
	"find": true, "finds": true, "finding": true,
	"try": true, "tries": true, "trying": true,
	"let": true, "lets": true, "letting": true,
	"help": true, "helps": true, "helping": true,
	"see": true, "sees": true, "seeing": true,
}

// nonSkillObjectWord matches a word that names a non-skill object
// a quoted name can denote: a mode, a virtual environment, a
// profile. Used only inside, or immediately after, a quoted object
// following "activate" (see isQuotedSkillReference).
var nonSkillObjectWord = regexp.MustCompile(`(?i)\b(mode|venv|virtualenv|environment|conda|profile)\b`)

// nonSkillObjectWords is the same set as a lookup table for the
// single word following a quoted object.
var nonSkillObjectWords = map[string]bool{
	"mode": true, "venv": true, "virtualenv": true,
	"environment": true, "conda": true, "profile": true,
}

// isActivateDirective reports whether line is shaped as an
// instruction to activate a skill (Audit 6 F-A6-2, widened by
// Audit 7 F-A7-P2-1, refined by Audit 8 F-A8-P2-6/F-A8-P2-7 and
// Audit 9 F-A9-P2-1/F-A9-P2-2/F-A9-P2-3/F-A9-P2-4).
//
// EVERY occurrence of "activate" — in any of its forms (activate,
// activates, activated, activating, reactivate and its forms, and
// the -ion nouns activation/reactivation, Audit 9 F-A9-P2-4) — in
// the line is examined (F-A8-P2-6): the old code looked only at the
// first, so a "tool" object on the first occurrence shielded a
// second directive ("Activate the tool, then activate the skill").
// Each occurrence is judged on its own segment — the text up to the
// next "activate" (or the end of the line) — so a skill named by a
// later occurrence is never attributed to an earlier one, and
// vice versa.
//
// One occurrence is a directive when, in its segment, what
// follows it names a skill, or the line is a bare "Activate":
//   - the word "skill" ("Activate skill ...", "Activate the
//     other skill", "Please activate skill \"other\"");
//   - a quoted name ("Activate \"name\" ..."), which in this
//     context is a skill reference — unless the quoted name is,
//     or is followed by, a named non-skill object (see
//     isQuotedSkillReference).
//
// Three carve-outs keep legitimate prose loading (F-A8-P2-7).
// Each is deliberately narrow, and each is scoped to the clause
// of the occurrence it excuses, so it cannot shield a later
// directive:
//   - Negation: when the clause immediately before the occurrence
//     negates an activate verb form ("should not activate", "do
//     not activate", "never activate", "must not activate",
//     "cannot activate", "refuse to activate"), the line is a
//     defensive instruction, not a directive. The negation must
//     govern activate itself (Audit 9 F-A9-P2-1): "do not forget
//     to activate" and "do not hesitate to activate" negate a
//     different verb ("forget"/"hesitate") and are still
//     directives. The negation is looked for only in the final
//     clause fragment before the occurrence (split on sentence
//     terminators and clause separators), so it never leaks
//     across a sentence ("The skill is not broken. Activate the
//     skill" is still a directive) or to a later occurrence
//     ("Do not activate the tool, activate the skill" is still
//     a directive).
//   - Sanctioned path: an infinitive ("to activate") that names
//     the skill tool as the means ("To activate the skill, use
//     the skill tool.") documents the sanctioned activation path
//     rather than directing a switch. The infinitive alone does
//     not exempt ("To activate the skill, just do it." is still
//     a directive), and neither does naming some other tool
//     ("To activate the skill, use the tool.").
//   - Object order: every object of the occurrence in its segment
//     is considered (Audit 9 F-A9-P2-3), not just the first. A
//     skill named as a conjoined or elided object ("Activate the
//     tool and the skill", "Activate the tool, then the skill")
//     is still a directive; a skill named only after an
//     instrumental or prepositional head ("Activate the tool
//     using the skill", "Activate the tool for this skill") is
//     the object of that phrase, not of "activate", and loads.
//     A skill after a separator that starts a new clause with
//     itself (or a compound subject) as the subject ("Activate
//     the tool, the skill waits", "Activate the tool and the
//     skill is ready") is likewise not an object (Audit 11
//     F-A11-1: the clause is recognised by shape — any verb
//     form — not by a finite verb list). "To activate the tool,
//     run it once" (tool only, or no skill at all) is prose and
//     loads; "Activate the skill using the tool" (skill first)
//     is a directive and is refused.
//   - Reactivation forms: "reactivate" and "activating" direct
//     activation like "activate" does (Audit 9 F-A9-P2-4). The
//     -ion nouns ("activation"/"reactivation") direct it only
//     when governed by a directive verb in the same clause
//     ("Begin activation of the skill"); a line merely discussing
//     them ("Reactivation happens automatically", "This skill
//     explains activation") loads.
//
// SECURITY JUDGEMENT (F-A8-P2-7 vs Audit 7): the Audit 8 false
// positive "bare Activate." (e.g. a venv step) is NOT admitted.
// A line consisting solely of "Activate" — with or without
// trailing punctuation (".", "!", "?", ";", ":", "," — Audit 9
// F-A9-P2-2: the trim set is the punctuation the validator treats
// as a sentence terminator or clause separator) — is
// indistinguishable from the Audit 7 bare-"Activate" evasion, and
// admitting the punctuated form would admit the evasion with a
// period appended. The bare rule therefore stays as it was. A venv
// step can name its object ("Activate the venv." loads) or be a
// list item ("- Activate." already loads: the line is not bare). Where this function had
// to choose between a false positive (legitimate prose blocked)
// and a false negative (an evasion admitted), it errs toward
// security (reject). A line about a skill that merely mentions
// activation the other way round ("This skill helps you activate
// the tool") does not match either, because "skill" precedes
// "activate". There is no defined textual activation syntax in
// the codebase — activation goes through the skill tool — so any
// line shaped like one is a body-directed switch attempt, not a
// legitimate instruction.
func isActivateDirective(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	if lower == "" {
		return false
	}
	// Bare "Activate" (allowing trailing sentence punctuation):
	// an imperative with no object at all, in a skill body,
	// can only be read as "activate [a skill]". See the
	// SECURITY JUDGEMENT above: this includes "Activate.",
	// "Activate?", and "Activate;" (Audit 9 F-A9-P2-2).
	if bare := strings.TrimRight(lower, ".!?,;:"); bare == "activate" {
		return true
	}
	locs := activateWord.FindAllStringIndex(lower, -1)
	for i, loc := range locs {
		end := len(lower)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if isActivateOccurrenceDirective(lower, loc, lower[loc[1]:end]) {
			return true
		}
	}
	return false
}

// isActivateOccurrenceDirective reports whether one occurrence of
// "activate" (at loc in the lowercased line, in any of its forms —
// see activateWord) is a directive, judged on its segment — the
// text after it up to the next "activate".
func isActivateOccurrenceDirective(lower string, loc []int, segment string) bool {
	seg := strings.TrimSpace(segment)
	if seg == "" {
		return false
	}
	word := lower[loc[0]:loc[1]]
	before := lower[:loc[0]]
	// -ion noun forms (Audit 9 F-A9-P2-4): "activation" /
	// "reactivation" is a noun, not a verb, so on its own it is
	// discussion, not a directive. It directs activation only when
	// governed by a directive verb in the same clause ("Begin
	// activation of the skill"), and only when the skill is named
	// as its complement ("of the skill"). A negated governing verb
	// ("do not begin activation of the skill") is defensive, like
	// a negated activate verb form.
	if strings.HasSuffix(word, "ion") {
		if clauseNegatesVerb(before, activateVerbForms) || clauseNegatesVerb(before, activationDirectiveVerbs) {
			return false
		}
		if !clauseGovernorIsDirectiveVerb(before) {
			return false
		}
		return segmentNamesSkillForActivationNoun(seg)
	}
	// Negated in its own clause: a defensive instruction, not a
	// directive (see isActivateDirective). The negation must govern
	// an activate verb form (Audit 9 F-A9-P2-1).
	if clauseHasNegation(before) {
		return false
	}
	// Infinitive documenting the sanctioned skill-tool path.
	if prevWordIs(before, "to") && skillToolBigram.MatchString(seg) {
		return false
	}
	// "Activate \"name\" ..." — a quoted name after Activate is a
	// skill reference in this context, unless it names a non-skill
	// object (see isQuotedSkillReference).
	if seg[0] == '"' || seg[0] == '\'' {
		return isQuotedSkillReference(seg)
	}
	// Object rule (Audit 9 F-A9-P2-3): any "skill" named as an
	// object of this occurrence in its clause is a directive —
	// including a conjoined or elided object ("and the skill",
	// ", then the skill"). A skill named only after an object
	// closer ("using", "for", ...) is the object of that phrase
	// instead, and does not make the line a directive.
	return segmentNamesSkillAsActivateObject(seg)
}

// isObjectContinuationWord reports whether word (a raw whitespace
// field, punctuation included) can continue the direct-object
// region after a comma: only a conjunction or the elision "then"
// ("Activate the tool, then the skill"). A comma before anything
// else ends the region — UNLESS what follows is a comma-separated
// object list naming the skill (F-A10-3): "Activate the tool, the
// skill" lists the skill as another object of the same "activate",
// so commaListNamesSkill keeps the region open for exactly that
// shape. A comma before a new clause does not: "Activate the tool,
// the skill is ready" names the skill as the subject of a new
// clause, not as an object of "activate".
func isObjectContinuationWord(raw string) bool {
	w := strings.Trim(raw, ".,;:()[]\"'`!?")
	return w == "then" || w == "and" || w == "or"
}

// clauseVerbAux are the copula/auxiliary forms that start a new
// clause when they follow a skill subject ("the skill is ready",
// "the skill has finished"): the skill is the subject of that
// clause, not an object of "activate" (F-A10-3).
var clauseVerbAux = map[string]bool{
	"is": true, "are": true, "was": true, "were": true,
	"be": true, "been": true, "being": true,
	"has": true, "have": true, "had": true,
	"do": true, "does": true, "did": true,
	"will": true, "would": true, "can": true, "could": true,
	"shall": true, "should": true, "may": true, "might": true, "must": true,
}

// objectCloserNonVerbs are the activateObjectClosers entries that
// are prepositions or subordinating conjunctions, not verbs: they
// head a phrase modifying a noun ("the skill for beginners"),
// they do not start a new clause (F-A10-3).
var objectCloserNonVerbs = map[string]bool{
	// Prepositions / instrumental heads.
	"using": true, "with": true, "via": true, "by": true, "through": true,
	"for": true, "to": true, "of": true, "in": true, "on": true, "at": true,
	"from": true, "into": true, "over": true, "under": true, "per": true,
	// Subordinating conjunctions.
	"when": true, "while": true, "if": true, "because": true, "before": true,
	"after": true, "unless": true, "although": true, "though": true,
	"since": true, "that": true, "which": true, "who": true, "as": true,
}

// isClauseVerb reports whether word can start a new clause with a
// skill as its subject: a copula/auxiliary, or a verb that closes
// the object region (see activateObjectClosers) — but not a
// preposition or subordinating conjunction, which only modifies
// the noun phrase ("the skill for beginners" is still an object
// of "activate", not a new clause).
func isClauseVerb(word string) bool {
	if clauseVerbAux[word] {
		return true
	}
	return activateObjectClosers[word] && !objectCloserNonVerbs[word]
}

// verbFormNonVerbs are words that look like a verb form by suffix
// (-s, -ed, -ing) but are never verbs in the position after a skill
// subject, so the clause test must not read them as a new clause
// (F-A11-1, F-A12-1):
//   - nouns that form a noun-noun compound with "skill" ("the
//     skill tools", "the skill settings", "the skill files") — the
//     compound is still a list object of "activate", not a clause;
//     F-A12-1 extends this to the common skill-doc noun heads
//     ("files", "notes", "guides", "meetings", "recordings",
//     "tutorials", "manuals", "users", ...) — these are pure nouns
//     here, so they override every following-word rule (a "tools"
//     is never a verb, even before "the");
//   - the subject noun itself ("skill", "skills"), pronouns and
//     demonstratives ("this", "its", "his");
//   - prepositions and preposition-like participles that head a
//     phrase modifying the noun ("the skill during the meeting",
//     "the skill according to the manual", "the skill including
//     the venv" — "including X" extends the list with X, so the
//     skill is still an object);
//   - the noun "speed" (ends in -ed but is not a past-tense verb
//     here).
//
// Words that are BOTH common nouns and common verb forms ("files",
// "notes", "guides", "training", "learning", "building") are NOT
// here — they live in verbFormAmbiguousNouns, where the word after
// them decides (an article or object makes them a verb, a clause
// end or a preposition leaves them a noun).
var verbFormNonVerbs = map[string]bool{
	// Noun-noun compounds with "skill".
	"items": true, "options": true, "settings": true,
	"examples": true, "parts": true, "tools": true,
	// Common skill-doc noun heads (F-A12-1): pure nouns, never
	// verbs in this position.
	"meetings": true, "recordings": true, "tutorials": true,
	"manuals": true, "resources": true, "materials": true,
	"templates": true, "summaries": true, "sessions": true,
	"conversations": true, "responses": true, "problems": true,
	"decisions": true, "discussions": true, "jobs": true,
	"tickets": true, "users": true, "members": true,
	"teams": true, "goals": true, "customers": true,
	"clients": true, "editors": true, "developers": true,
	"managers": true, "students": true, "teachers": true,
	"owners": true, "admins": true, "events": true,
	"policies": true, "procedures": true, "workflows": true,
	"warnings": true,
	// The subject noun itself, pronouns, demonstratives.
	"skill": true, "skills": true,
	"this": true, "its": true, "his": true, "hers": true,
	"ours": true, "yours": true, "theirs": true, "yes": true,
	// Prepositions / preposition-like participles.
	"during": true, "according": true, "including": true,
	"excluding": true, "regarding": true, "concerning": true,
	"besides": true, "towards": true,
	// -ed nouns.
	"speed": true,
	// -ing nouns/pronouns.
	"thing": true, "something": true, "anything": true,
	"nothing": true, "everything": true,
}

// verbFormAmbiguousNouns are words that are common skill-doc noun
// heads AND common verb forms (F-A12-1): "files", "notes" and
// "guides" are third-person singular verbs as well as plural nouns,
// "training", "learning" and "building" are progressives as well as
// (mass) nouns. They are nouns at a clause end ("the skill files"),
// before a singular noun ("files report") or before a preposition
// ("files for beginners"), but verbs when followed by an object or
// complement ("files the report", "training the users", "notes
// the time"). clauseVerbAt applies the following-word rule; a pure
// noun in verbFormNonVerbs never becomes a verb.
var verbFormAmbiguousNouns = map[string]bool{
	"files": true, "notes": true, "guides": true,
	"training": true, "learning": true, "building": true,
}

// baseFormNonVerbs are bare words that are never base-form verbs
// after a plural or compound subject (F-A12-2): named non-skill
// objects ("mode", "venv", "environment", ... — see
// nonSkillObjectWords), the object noun "tool", and common
// adjectives ("ready", "available", "new", ...) that can sit at a
// clause end after a plural subject ("the skills ready" is missing
// its copula, not a clause). A base-form word at a clause end is
// otherwise a verb by shape — the audit's F-A12-2 direction — so
// this set is deliberately small: only words that are plainly not
// verbs.
var baseFormNonVerbs = map[string]bool{
	"mode": true, "venv": true, "virtualenv": true,
	"environment": true, "conda": true, "profile": true,
	"tool": true, "ready": true, "available": true,
	"new": true, "old": true, "online": true, "offline": true,
	"active": true, "simple": true, "easy": true,
}

// neverVerbWords are words that are only ever nouns or adjectives,
// never verbs in any form (F-A14-4): pure -s/-ing nouns ("analysis",
// "status", "news", "physics", "morning", "evening", "mathematics",
// "economics", "chaos", "virus", "campus") and pure nouns from the
// rc18 guards ("documentation", "handbook", "manual", "summary",
// "library", "overview", "framework"), plus common adjectives that
// are never verbs ("happy", "sad", "glad", "good", "bad", "great",
// "big", "small"). They end in a verb-form suffix (-s/-ing) or sit
// bare after a plural subject, so morphology and the plural
// base-form rule would otherwise read them as verbs — but they
// have no verb reading at all. A word here is never verb-shaped
// (see isVerbShaped): no following-word rule and no clause-final
// rule can read it as a verb. It can still be the SUBJECT of a
// clause ("…the skill analysis is ready" loads — the aux is the
// verb, F-A13-3), because the aux and following-verb rules do not
// consult verb shape. "free" and "slow" are genuine transitive
// verbs too, so they stay out (see transitiveOnlyVerbs, F-A16-4).
// Words that ARE also verbs are deliberately absent: "reference"
// (to reference), "report" (to report), "setting" (an
// object-region verb, R3) and the ambiguous nouns ("files",
// "notes", "guides", "training", "learning", "building") are
// verbs too, so they stay out.
//
// F-A15-1/F-A15-2 extend the list with the never-verb classes
// Audit 15's probe surface shows. A bare noun or adjective after
// a plural subject is a noun phrase, not a verb by shape, and a
// suffix-bearing pure noun or adjective cannot take a guarded
// plural as its object:
//   - bare never-verb adjectives ("hot", "tall", "young", "red",
//     "cold", "dark", "wide", "deep", "soft", "hard", "angry",
//     "sorry", "afraid", "alive", "asleep", "aware", "able");
//   - adjective/adverb-dominant words whose marginal verb
//     readings never take a bare-plural or gerund object in
//     skill-doc positions ("fast" — to fast is intransitive;
//     "busy" — to busy takes a reflexive, not a bare plural;
//     "very" — a degree adverb, never a verb). "slow" and
//     "free" are NOT here: they are genuine transitive verbs
//     ("free the plan", "slow the rollout") and live in
//     transitiveOnlyVerbs (F-A16-4) — the adjective reading
//     dominates only where no object follows, and the validator
//     errs toward rejection for adjective-dominant words (the
//     D2 precedent);
//   - pure -ss nouns ("business", "success", "loss", "class",
//     "glass") — no verb reading; genuine -ss verbs ("boss",
//     "progress", "process", "address", "access") stay out;
//   - pure -s nouns ("series", "species") — no verb reading;
//   - a pure -ing noun ("ceiling") — no verb reading; genuine
//     -ing verbs ("feeling", "meaning", "flooring") and the
//     ambiguous nouns ("building", "training", "learning")
//     stay out;
//   - pure -ed adjectives ("rugged", "wicked") — no verb
//     reading; genuine -ed verb forms ("closed", "trained",
//     "opened", "finished", "tired", "excited", "learned",
//     "crooked") stay out (Audit 15 §6 O4);
//   - bare nouns ("notebook", "guidebook", "keyboard",
//     "pencil") — no verb reading in skill-doc clause-final
//     position; "keyboard"/"pencil" have marginal verb readings
//     ("keyboard the plan", "pencil the plan") that never occur
//     as bare clause-final verbs, so the noun reading wins.
//
// A noun + subordinator is still a noun phrase, not a clause
// ("…the skill series/ceiling/business that helps" rejects —
// the subordinator attaches a relative clause; only a genuine
// verb form such as "reports" loads before "that"), so these
// words reject in every verb position and the -s/-ss asymmetry
// Audit 15 §6 O16/O18 documented is now consistent.
var neverVerbWords = map[string]bool{
	// Pure -s/-ing nouns (F-A14-4).
	"analysis": true, "status": true, "news": true,
	"physics": true, "morning": true, "evening": true,
	"mathematics": true, "economics": true, "chaos": true,
	"virus": true, "campus": true,
	// Pure nouns from the rc18 guards (F-A13-1).
	"documentation": true, "handbook": true, "manual": true,
	"summary": true, "library": true, "overview": true,
	"framework": true,
	// Common adjectives that are never verbs.
	"happy": true, "sad": true, "glad": true, "good": true,
	"bad": true, "great": true, "big": true, "small": true,
	// Bare never-verb adjectives (F-A15-1).
	"hot": true, "tall": true, "young": true, "red": true,
	"cold": true, "dark": true, "wide": true, "deep": true,
	"soft": true, "hard": true, "angry": true, "sorry": true,
	"afraid": true, "alive": true, "asleep": true,
	"aware": true, "able": true,
	// Adjective/adverb-dominant words (F-A15-1/F-A15-3).
	"fast": true, "busy": true,
	"very": true,
	// Pure -ss nouns (F-A15-1).
	"business": true, "success": true, "loss": true,
	"class": true, "glass": true,
	// Pure -s nouns (F-A15-1/F-A15-2).
	"series": true, "species": true,
	// Pure -ing noun (F-A15-2).
	"ceiling": true,
	// Pure -ed adjectives (F-A15-2).
	"rugged": true, "wicked": true,
	// Bare nouns (F-A15-1).
	"notebook": true, "guidebook": true, "keyboard": true,
	"pencil": true,
}

// transitiveOnlyVerbs are genuine transitive verbs that are also
// common adjectives (F-A16-4): "free" (to free the plan, to free
// users) and "slow" (to slow the rollout, to slow users). They were
// in neverVerbWords on rc20, which over-blocked their verb +
// object forms — the auditor's probe showed "free the plan", "free
// users", "slow the rollout" and "slow users" all rejecting, while
// the bare forms ("the skills free/slow") correctly reject. They
// are deliberately NOT in neverVerbWords, baseFormVerbs or
// isVerbShaped/isVerbShapedAt: a transitive-only verb has no verb
// reading without an object, so the general following-word rules
// must not treat it as a verb — only the explicit object rules in
// clauseVerbAt admit it, and only where the subject is complete
// (see clauseVerbAt: an article, a pronoun, or a bare plural
// object after a complete subject). Everywhere else the adjective
// reading wins, exactly as if the word were still in
// neverVerbWords:
//   - clause-final bare ("the skills free", "the skill and the
//     venv slow") rejects: no intransitive reading, so the plural
//     base-form rule never fires for it;
//   - inside the subject noun phrase ("the skill and the free
//     tools", Audit 13 §7 D2 / rc20 guard) rejects: the adjective
//     modifies the head noun, so it never takes the guarded
//     object (see verbTakesGuardedObject);
//   - before a participle ("the skill and the free running
//     tests", rc20 F-A15-3 guard; "the skills free running
//     tests") rejects: it never takes a gerund/participle
//     complement, so it is a modifier, not a clause head;
//   - before an adverb, preposition, subordinator or bare
//     singular noun ("free quickly", "free for beginners",
//     "free plan") rejects: no object follows.
//
// "fast" and "busy" are NOT here and stay in neverVerbWords: to
// fast is intransitive and to busy takes a reflexive, not a bare
// plural — "fast the plan" and "busy the plan" still reject.
var transitiveOnlyVerbs = map[string]bool{
	"free": true, "slow": true,
}

// adjectiveVerbs are verbs that are also common adjectives in
// skill-doc noun phrases (F-A14-2): "open" heads "the open tools"
// and "the open venv" — an adjective + noun, not a verb + object.
// A word here never takes a guarded noun as its bare plural object,
// so the guarded-plural rejection still fires for it ("…the skill
// and the open tools" rejects, Audit 13 §7 D2). The class is the
// object-closer verbs (see activateObjectClosers) that are also
// common adjectives in attributive position: "open", "close"
// ("nearby tools") and "set" ("tools of the set", "a set routine")
// — F-A15-4. The rc19 comment's "only open is documented as an
// adjective" was wrong for `close` and `set`. Suffixed forms
// ("opened", "closed", "sets") still take the object, because the
// list holds base forms only and a suffixed form has verb evidence
// ("…the skill and the venv opened users" loads). Noun-verbs such
// as "test"/"list"/"check" are NOT in the class: "test tools" is a
// noun+noun compound, not an adjective + noun phrase, so their
// verb reading wins (F-A14-2). The other object-region verbs
// ("make", "create", "use", "run", "help") are never adjectives.
var adjectiveVerbs = map[string]bool{
	"open": true, "close": true, "set": true,
}

// subjectModifiers are determiners and adjectives that can sit
// inside a subject noun phrase ("the other venv", "the same skill")
// but are never the clause verb (F-A12-2): when the clause test
// meets one while scanning a compound subject, it is a subject
// word, not a verb — so "the skill and the other skills" (no verb
// after it) stays a list of objects.
var subjectModifiers = map[string]bool{
	"the": true, "a": true, "an": true,
	"other": true, "another": true, "same": true,
	"next": true, "first": true, "last": true,
	"main": true, "each": true, "every": true,
	"all": true, "both": true, "own": true,
	"different": true, "several": true, "various": true,
	"multiple": true, "single": true, "entire": true,
	"whole": true, "total": true, "these": true, "those": true,
	"only": true, "such": true, "certain": true,
}

// clausePrepositions are the prepositions in objectCloserNonVerbs,
// split out so the clause test can tell a verb's prepositional
// phrase ("waits for the team") from a subordinating conjunction
// ("waits when ready"): both follow a verb, but only a preposition
// can also head a phrase modifying a noun ("files for beginners"
// is a noun phrase, so an ambiguous noun before a preposition
// stays a noun — F-A12-1).
var clausePrepositions = map[string]bool{
	"using": true, "with": true, "via": true, "by": true,
	"through": true, "for": true, "to": true, "of": true,
	"in": true, "on": true, "at": true, "from": true,
	"into": true, "over": true, "under": true, "per": true,
}

// clauseSubordinators are the subordinating conjunctions in
// objectCloserNonVerbs: what follows a verb here is a subordinate
// clause ("waits when ready"), never a noun phrase, so a candidate
// before one is always a verb (F-A12-1).
var clauseSubordinators = map[string]bool{
	"when": true, "while": true, "if": true, "because": true,
	"before": true, "after": true, "unless": true,
	"although": true, "though": true, "since": true,
	"that": true, "which": true, "who": true, "as": true,
}

// clausePronouns are pronouns, demonstratives and possessives that
// can be a verb's object ("helps you", "explains it", "explains
// its purpose"): a candidate before one is a transitive verb
// (F-A12-2).
var clausePronouns = map[string]bool{
	"it": true, "them": true, "us": true, "him": true,
	"her": true, "me": true, "you": true,
	"this": true, "that": true, "these": true, "those": true,
	"its": true, "their": true, "his": true, "my": true,
	"our": true, "your": true, "theirs": true, "ours": true,
	"yours": true, "mine": true, "himself": true,
	"herself": true, "itself": true, "themselves": true,
	"ourselves": true, "yourself": true, "yourselves": true,
	"one": true, "ones": true, "all": true, "none": true,
	"some": true, "any": true, "each": true, "both": true,
	"either": true, "neither": true,
}

// baseFormVerbs are common verbs in their base form, recognised
// only after a plural or compound subject (F-A11-1): a plural
// subject takes a base-form verb ("the skills wait", "the skill
// and the venv wait"), and a base form has no distinctive suffix,
// so morphology cannot recognise it. The set is deliberately
// small and gated to plural subjects — a singular subject with a
// base-form verb is ungrammatical, and the word is more likely a
// noun modifier ("the skill work" = work about skills), so base
// forms are never recognised after a singular "skill". "start"
// and "begin" are deliberately absent, as in
// activateObjectClosers: starting a skill is close enough to
// activating it that the line errs toward rejection. "dance" and
// "serve" join the list for F-A12-2: they are the base-form verbs
// of "the skills dance" and "the skill and the venv serve the
// team" (Audit 12). The list is now only a fallback: after a
// plural or compound subject ANY bare word is a verb by shape
// (see isVerbShapedAt, F-A14-1), so the list matters on the
// singular path (a listed base-form verb after "the skill" is
// still recognised where an unlisted one is not — "the skill
// work" rejects) and in the participle rule (a listed base-form
// verb takes a gerund/participle complement, F-A13-2).
var baseFormVerbs = map[string]bool{
	"wait": true, "work": true, "explain": true,
	"finish": true, "complete": true, "happen": true,
	"remain": true, "continue": true, "stop": true,
	"dance": true, "serve": true,
	"fail": true, "succeed": true, "pass": true,
	"respond": true, "reply": true, "report": true,
	"provide": true, "support": true, "handle": true,
	"manage": true, "perform": true, "operate": true,
	"function": true, "apply": true, "belong": true,
	"depend": true, "matter": true, "change": true,
	"grow": true, "improve": true, "appear": true,
	"seem": true, "become": true, "exist": true,
	"need": true, "want": true, "include": true,
	"contain": true, "require": true, "allow": true,
	"enable": true, "cause": true, "lead": true,
	"follow": true, "hold": true, "bring": true,
	"carry": true, "move": true, "stay": true,
	"live": true, "play": true, "talk": true,
	"speak": true, "listen": true, "look": true,
	"feel": true, "think": true, "know": true,
	"understand": true, "learn": true, "teach": true,
	"ask": true, "answer": true, "arrive": true,
	"leave": true, "die": true, "prefer": true,
	"mean": true,
}

// clauseAdverbs are adverbs that can sit between a subject and its
// verb ("the skill then waits", "the skill now works"): the verb
// after the adverb still starts the clause (F-A11-1).
var clauseAdverbs = map[string]bool{
	"then": true, "now": true, "already": true, "still": true,
	"just": true, "also": true, "always": true, "never": true,
	"soon": true, "later": true, "first": true, "finally": true,
	"eventually": true,
}

// hasVerbSuffix reports whether word has a verb-form suffix:
// third-person singular present (-s, not -ss, which is a noun
// like "class"), past tense (-ed) or progressive participle
// (-ing). Used by clauseVerbAt for the morphology check.
func hasVerbSuffix(word string) bool {
	if strings.HasSuffix(word, "ing") && len(word) > 4 {
		return true
	}
	if strings.HasSuffix(word, "ed") && len(word) > 3 {
		return true
	}
	if strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss") && len(word) > 2 {
		return true
	}
	return false
}

// isVerbShaped reports whether word looks like a verb by shape:
// a verb-form suffix (-s/-ed/-ing), an object-region verb (see
// isClauseVerb), or a listed base-form verb (see baseFormVerbs).
// A bare noun ("handbook") is not verb-shaped, and neither is a
// never-verb word ("analysis", "documentation", "happy" — see
// neverVerbWords): the suffix alone must not make a word that has
// no verb reading at all verb-shaped (F-A14-4). The following-word
// rules in clauseVerbAt (F-A13-1) require the candidate itself to
// be verb-shaped before they fire: a noun followed by an article,
// pronoun, adverb, preposition, subordinating conjunction or bare
// plural is a noun phrase, not a verb phrase. A transitive-only
// verb ("free", "slow" — see transitiveOnlyVerbs) is never
// verb-shaped here either (F-A16-4): it has no verb reading
// without an object, so the general following-word rules must not
// admit it — only the explicit object rules in clauseVerbAt do.
func isVerbShaped(word string) bool {
	if neverVerbWords[word] || transitiveOnlyVerbs[word] {
		return false
	}
	return hasVerbSuffix(word) || isClauseVerb(word) || baseFormVerbs[word]
}

// isVerbShapedAt reports whether word is a verb by shape in a
// clause whose subject is plural (plural true) or singular (plural
// false). After a plural or compound subject a bare word in verb
// position is a verb by shape (F-A14-1, F-A12-2): the subject is
// plural, so a base form is grammatical, and a bare word there is
// the verb, not a noun modifier — unless it is a never-verb word
// (F-A14-4). After a singular subject a bare word is not a verb by
// shape ("the skill build the plan" is ungrammatical), so only
// verb morphology, an object-region verb or a listed base-form
// verb counts. Words that can never be verbs are excluded in both
// positions. A transitive-only verb ("free", "slow" — see
// transitiveOnlyVerbs) is excluded too (F-A16-4): the plural
// base-form rule would otherwise read it as a verb before an
// adverb, preposition or subordinator ("free quickly", "free
// for beginners"), where no object follows. Only the explicit
// object rules in clauseVerbAt admit it. Callers only ever pass
// words that have already passed
// the early returns in clauseVerbAt (not a pure noun head, not a
// known non-verb, not a subject modifier or adverb), so a bare
// word reaching here is a genuine verb candidate.
func isVerbShapedAt(word string, plural bool) bool {
	if neverVerbWords[word] || transitiveOnlyVerbs[word] {
		return false
	}
	if plural {
		return true
	}
	return hasVerbSuffix(word) || isClauseVerb(word) || baseFormVerbs[word]
}

// verbTakesGuardedObject reports whether word can take a guarded
// noun ("tools", "items", "settings", "users", "meetings",
// "examples" — see verbFormNonVerbs) as its bare plural object
// (F-A14-2): a finite verb form does — an -s/-ed verb ("makes
// users", "trained users"), an object-region verb ("make users",
// "create tools", "use settings") or a listed base-form verb
// ("serve tools", "dance settings"). A never-verb word
// ("analysis", "documentation") never does (F-A14-4, F-A13-1),
// and neither does a verb that is also a common adjective ("open":
// "the open tools" is an adjective + noun, Audit 13 §7 D2). A
// participle in subject position ("running tools") does not
// either: -ing is never a finite verb form, so the participle is
// an attributive adjective and the guarded noun is the head of the
// noun phrase (F-A12-1) — in predicate position ("the skill
// running tools") the same participle does take the object, so the
// exclusion applies only while inSubject.
//
// F-A15-2: a suffix-bearing pure noun or adjective ("series",
// "species", "ceiling", "rugged", "wicked") never takes a guarded
// plural either — the suffix alone is not verb evidence. The
// never-verb check below is the discriminator, not a blanket -ed
// exclusion: Audit 15 suggested excluding -ed forms in subject
// position uniformly, but a genuine -ed verb DOES take a guarded
// object for a compound subject ("…the skill and the venv
// closed/trained/opened/finished/tired/excited tools" load on
// both rc18 and rc19 and are defensible, Audit 15 §6 O4), so an
// -ed exclusion would break genuine past-tense verb+object
// clauses. Only a never-verb -ed adjective ("rugged", "wicked")
// is excluded, via neverVerbWords.
//
// F-A16-4: a transitive-only verb ("free", "slow" — see
// transitiveOnlyVerbs) takes a guarded object only as the
// predicate verb of a complete subject ("the skills free users",
// "the skill and the venv free tools"), never as an adjective
// inside the subject noun phrase ("the skill and the free tools",
// Audit 13 §7 D2 / rc20 guard). subjectComplete says whether a
// noun head of the (compound) subject has already been seen
// (compoundSubjectStartsClause passes its sawNoun flag; the direct
// singular/plural path passes false with inSubject false, so the
// predicate reading always applies there).
func verbTakesGuardedObject(word string, inSubject bool, subjectComplete bool) bool {
	if neverVerbWords[word] {
		return false
	}
	if adjectiveVerbs[word] {
		return false
	}
	if transitiveOnlyVerbs[word] {
		return !inSubject || subjectComplete
	}
	if inSubject && strings.HasSuffix(word, "ing") && len(word) > 4 {
		return false
	}
	return isVerbShaped(word)
}

// isNounHead reports whether word is a genuine noun head of the
// (compound) subject (F-A14-3): a pure noun head
// (verbFormNonVerbs), an ambiguous noun head
// (verbFormAmbiguousNouns), a named non-skill object noun ("venv",
// "tool", "mode" — nonSkillObjectWords plus "tool"), or a bare
// plural ("reports", "answers", "analysis"). A pure -ss noun
// ("class", "glass", "business", "success", "loss") is a singular
// noun head too (F-A15-3): it is never a verb form — hasVerbSuffix
// excludes -ss for exactly that reason — so it can be the subject
// of a participle phrase ("…the skill and the class running
// tests" loads, Audit 15 §6 O9). Adjectives ("new", "ready",
// "happy") and adverbs ("quickly") are not noun heads, even when
// they sit inside the subject noun phrase: only a noun head
// completes the phrase, so a participle after an adjective ("the
// new running tests") is still a modifier, not the predicate.
func isNounHead(word string) bool {
	if verbFormNonVerbs[word] || verbFormAmbiguousNouns[word] {
		return true
	}
	if nonSkillObjectWords[word] || word == "tool" {
		return true
	}
	if strings.HasSuffix(word, "s") && len(word) > 2 {
		return true
	}
	return false
}

// clauseVerbAt reports whether the word at fields[p] is the verb of
// a new clause with the (compound) skill subject, and whether it is
// instead a modifier of the following noun (F-A12-1, F-A12-2). A
// verb is recognised by shape — what follows it — not by a finite
// verb list:
//   - a copula/auxiliary is always a verb ("is ready");
//   - a word followed by an article, pronoun, adverb, preposition
//     or subordinating conjunction is a verb ("explains the plan",
//     "helps you", "waits for the team", "waits when ready") — a
//     transitive verb and its object, or a verb and its complement;
//     an ambiguous noun before a preposition stays a noun ("files
//     for beginners" is a noun phrase, not a clause). Every one of
//     these rules first requires the candidate itself to be
//     verb-shaped (see isVerbShaped): a bare noun followed by an
//     article, pronoun, adverb, preposition, subordinator or bare
//     plural ("documentation for beginners", "documentation that
//     helps", "documentation quickly", "documentation the plan",
//     "documentation tools") is a noun phrase, not a clause
//     (F-A13-1). A subordinator after an ambiguous noun ("files
//     that helps") attaches a relative clause, not a subordinate
//     clause, so it is a noun phrase too. The one exception to the
//     verb-shaped requirement is a transitive-only verb ("free",
//     "slow" — see transitiveOnlyVerbs, F-A16-4): it is a verb
//     only where an object follows (article, pronoun, bare
//     plural), and only as the predicate verb of a complete
//     subject — never bare at a clause end, never inside the
//     subject noun phrase, never before a participle, adverb,
//     preposition, subordinator or bare singular noun;
//   - a word followed by a bare plural noun is a verb with a bare
//     plural object ("serve users", "dance users", "files notes",
//     "running tests") — unless the following word is a guarded
//     noun ("tools", "items", "settings"), in which case it is a
//     noun compound ("documentation tools"), not a verb phrase,
//     unless the candidate can take a guarded noun as its object
//     (a finite verb form — "makes users", "make users",
//     "trained users", F-A14-2; "dance users", "serve users",
//     F-A13-1, F-A12-2 pin-down). A participle in subject position
//     ("running tools") and an adjective-verb ("open tools")
//     never can. A word followed by a bare plural
//     that is itself an object-region verb ("documentation runs",
//     "running tests") is a subject with that verb, so the clause
//     exists either way. This rule, and the participle rule below,
//     come BEFORE the following-verb rule on purpose: a word like
//     "tests" is both a plural noun and an object-region verb, and
//     as the word after a candidate it is the object ("running
//     tests"), not the verb — checking it as a verb first would
//     misread the candidate as a subject word and then misread
//     "tests" itself as the clause verb;
//   - a word followed by a participle is a modifier ("dance
//     waiting") unless it is an -s verb ("tests waiting"), a
//     listed base-form verb taking a gerund/participle complement
//     ("continue learning", "stop waiting", "remain seated"), or
//     a participle with an object ("files running tests", "files
//     running the tests"), which is a verb phrase whose subject
//     is the candidate (F-A13-2);
//   - a word followed by an aux ("files are ready") or by any
//     other verb ("files run") is the subject of a clause whose
//     verb is that following word (F-A13-3). A guarded noun
//     ("tools") still rejects, since it returns earlier;
//   - a word at a clause end is a verb by morphology (-s/-ed/-ing,
//     or an object-region verb), or — after a plural or compound
//     subject — by base form ("the skills dance", "the skills
//     build", "the skill and the venv dance", F-A12-2; F-A14-1):
//     a bare word with no verb morphology is a verb by shape
//     there, unless it is a never-verb word ("the venv
//     documentation", F-A13-1; F-A14-4); after a singular subject
//     a bare word ("the skill work") is a noun;
//   - a word followed by a bare singular noun is a modifier, not a
//     verb ("running venv", "open venv", "tired venv", "work plan"
//     are noun phrases) — except an -s verb ("tests code" is verb
//     plus object), and except in predicate position (inSubject
//     false: "the skill running tests" has the participle as its
//     verb, because there is nothing else it could be).
//
// A pure noun in verbFormNonVerbs ("tools", "users") is never a
// verb, whatever follows it. An adverb ("quickly") is never a verb
// either, so unlisted adverbs still reject as on rc15/rc16. A
// participle in subject position ("running tools") is a modifier:
// the scan must skip its head noun too, so the modifier flag tells
// compoundSubjectStartsClause to advance an extra word. The
// subjectComplete flag says whether a noun head of the (compound)
// subject has already been seen before position p: a participle
// before a bare plural is a modifier only while the subject noun
// phrase is still open ("the running tests"), and is the predicate
// participle with a bare plural object once a noun head has been
// seen ("the venv waiting tests", F-A13-2).
func clauseVerbAt(fields []string, p int, plural bool, inSubject bool, subjectComplete bool) (isVerb bool, isModifier bool) {
	raw := fields[p]
	word := strings.Trim(raw, ".,;:()[]\"'`!?")
	if word == "" {
		return false, false
	}
	if clauseVerbAux[word] {
		return true, false
	}
	if objectCloserNonVerbs[word] {
		return false, false
	}
	if clauseAdverbs[word] {
		return false, false
	}
	if strings.HasSuffix(word, "ly") && len(word) > 4 && !baseFormVerbs[word] {
		// An adverb ("quickly"), not a verb.
		return false, false
	}
	if subjectModifiers[word] {
		return false, false
	}
	if verbFormNonVerbs[word] {
		// A pure noun head ("tools", "users") — never a verb here.
		return false, false
	}
	if baseFormNonVerbs[word] {
		// A known non-verb ("tool", "venv", "ready") — a subject
		// word, never the clause verb: "the skill and the tool
		// wait" still finds "wait", and "the tool for beginners"
		// still ends the scan at the preposition.
		return false, false
	}
	// The word after the candidate: "" when the candidate's field
	// ends the clause with a comma or sentence terminator, or when
	// there is no next field.
	after := ""
	if !strings.ContainsAny(raw, ",.!?;:") && p+1 < len(fields) {
		after = strings.Trim(fields[p+1], ".,;:()[]\"'`!?")
	}
	if after == "" {
		// Clause-final: an ambiguous noun ("files") is still a
		// noun, and so is a never-verb word ("analysis",
		// "documentation") whatever its suffix (F-A14-4); a
		// verb form or an object-region verb is a verb; a bare
		// word is a verb by shape after a plural or compound
		// subject (F-A14-1, F-A12-2) — the subject is plural,
		// so a base form is grammatical — but a bare word after
		// a singular subject ("the skill work") is a noun.
		// Known non-verbs never reach here (verbFormNonVerbs,
		// baseFormNonVerbs and subjectModifiers return earlier).
		if verbFormAmbiguousNouns[word] {
			return false, false
		}
		if neverVerbWords[word] {
			return false, false
		}
		if transitiveOnlyVerbs[word] {
			// A transitive-only verb ("free", "slow") has no
			// intransitive reading, so the clause-final bare form
			// is never a verb — not even after a plural subject,
			// where the base-form rule would otherwise fire
			// (F-A16-4; rc20:163-165 pins "the skills free" and
			// "the skill and the venv free").
			return false, false
		}
		if hasVerbSuffix(word) {
			return true, false
		}
		if isClauseVerb(word) {
			return true, false
		}
		if plural {
			return true, false
		}
		return false, false
	}
	if after == "the" || after == "a" || after == "an" {
		// A transitive verb and its object ("explains the plan",
		// "serve the team", "files the report"): the article is
		// decisive, even for an ambiguous noun ("files") — but
		// only when the candidate is itself verb-shaped; a bare
		// noun before an article ("documentation the plan") is a
		// noun phrase, not a verb phrase (F-A13-1). After a
		// plural or compound subject a bare word is a verb by
		// shape ("the skills build the plan", F-A14-1), and a
		// never-verb word is never one. A transitive-only verb
		// ("free", "slow") is a verb here too — the article
		// introduces its object ("free the plan", "slow the
		// rollout", F-A16-4) — but only as the predicate verb of
		// a complete subject: inside the subject noun phrase
		// ("the skill and the free the plan") it is an adjective,
		// so it stays a subject word and the scan continues.
		if transitiveOnlyVerbs[word] {
			return !inSubject || subjectComplete, false
		}
		return isVerbShapedAt(word, plural), false
	}
	if clausePronouns[after] {
		// A transitive verb and its object ("helps you",
		// "explains it", "explains its purpose") — but only
		// when the candidate is itself verb-shaped; a bare noun
		// before a pronoun ("documentation you") is a noun
		// phrase, not a verb phrase (F-A13-1). After a plural
		// or compound subject a bare word is a verb by shape
		// ("the skills build you", F-A14-1). A transitive-only
		// verb ("free", "slow") is a verb here too — the pronoun
		// is its object ("free you", F-A16-4) — but only as the
		// predicate verb of a complete subject, as in the
		// article rule above.
		if transitiveOnlyVerbs[word] {
			return !inSubject || subjectComplete, false
		}
		return isVerbShapedAt(word, plural), false
	}
	if clauseAdverbs[after] || (strings.HasSuffix(after, "ly") && len(after) > 4 && !baseFormVerbs[after]) {
		// A verb and its adverb ("waits quietly") — but only
		// when the candidate is itself verb-shaped; a bare noun
		// before an adverb ("documentation quickly") is a noun
		// phrase, not a verb phrase (F-A13-1). After a plural
		// or compound subject a bare word is a verb by shape
		// ("the skills build quickly", F-A14-1).
		return isVerbShapedAt(word, plural), false
	}
	if clausePrepositions[after] {
		// A verb and its prepositional phrase ("waits for the
		// team"); an ambiguous noun with a PP modifier ("files
		// for beginners") is still a noun — and a bare noun
		// before a preposition ("documentation for beginners")
		// is a noun phrase too, because it is not verb-shaped
		// (F-A13-1). After a plural or compound subject a bare
		// word is a verb by shape ("the skills build for
		// beginners", F-A14-1).
		if verbFormAmbiguousNouns[word] {
			return false, false
		}
		return isVerbShapedAt(word, plural), false
	}
	if clauseSubordinators[after] {
		// A verb and its subordinate clause ("waits when
		// ready") — but only when the candidate is itself
		// verb-shaped; a bare noun before a subordinator
		// ("documentation when ready") is a noun phrase
		// (F-A13-1). A subordinator after an ambiguous noun
		// ("files that helps") attaches a relative clause, not
		// a subordinate clause, so it is a noun phrase too.
		// After a plural or compound subject a bare word is a
		// verb by shape ("the skills build when ready",
		// F-A14-1).
		if verbFormAmbiguousNouns[word] {
			return false, false
		}
		return isVerbShapedAt(word, plural), false
	}
	if clauseVerbAux[after] {
		// The word after the candidate is the verb ("files are
		// ready"): the candidate is its subject, so a clause
		// starts here (F-A13-3). A guarded noun ("tools") still
		// rejects, since it returns earlier.
		return true, false
	}
	if strings.HasSuffix(after, "s") && !strings.HasSuffix(after, "ss") && len(after) > 2 {
		// A bare plural object ("serve users", "dance users",
		// "files notes", "running tests"): the candidate is its
		// verb — unless the following word is a guarded noun
		// ("tools", "items", "settings"), in which case it is a
		// noun compound ("reference tools", "documentation
		// tools"), not a verb phrase, unless the candidate can
		// take a guarded noun as its object (see
		// verbTakesGuardedObject: a finite verb form — "makes
		// users", "make users", "trained users", "files tools",
		// F-A14-2 — but not a never-verb word, not an
		// adjective-verb like "open", and not a participle in
		// subject position, F-A12-1/F-A13-1; a transitive-only
		// verb — "free", "slow" — takes it only as the predicate
		// verb of a complete subject, F-A16-4). A participle in
		// subject position ("running tools") is an attributive
		// adjective (F-A12-1) — but only while the subject noun
		// phrase is still open ("the running tests"); once a noun
		// head has been seen ("the venv waiting tests") it is the
		// predicate participle with a bare plural object
		// (F-A13-2). A bare plural that is itself an
		// object-region verb ("documentation runs", "running
		// tests") is the verb of the clause, with the candidate
		// as its subject, so the clause exists either way. This
		// rule comes before the object-region verb check below on
		// purpose: a word like "tests" is both a plural noun and
		// an object-region verb, and as the word after a candidate
		// it is the object ("running tests"), not the verb —
		// checking it as a verb first would misread the candidate
		// as a subject word and then misread "tests" itself as
		// the clause verb.
		if verbFormNonVerbs[after] && !verbTakesGuardedObject(word, inSubject, subjectComplete) {
			return false, false
		}
		if inSubject && (strings.HasSuffix(word, "ing") && len(word) > 4 || strings.HasSuffix(word, "ed") && len(word) > 3) {
			if !subjectComplete {
				return false, true
			}
			return true, false
		}
		if isClauseVerb(after) {
			return true, false
		}
		// A transitive-only verb ("free", "slow") is a verb
		// before a bare plural object ("free users", "slow
		// guides", F-A16-4) — but only as the predicate verb of
		// a complete subject: inside the subject noun phrase it
		// is an adjective ("the skill and the free guides"), so
		// it stays a subject word and the scan continues. The
		// carve-out is the only path that admits it here:
		// isVerbShapedAt below returns false for a
		// transitive-only verb on either subject (it has no verb
		// reading without an object), so without the carve-out
		// even "the skills free users" would reject — while
		// "the skill free users" loads, as "the skill serve the
		// team" does for a listed base-form verb.
		if transitiveOnlyVerbs[word] {
			return !inSubject || subjectComplete, false
		}
		return isVerbShapedAt(word, plural), false
	}
	if strings.HasSuffix(after, "ing") && len(after) > 4 || strings.HasSuffix(after, "ed") && len(after) > 3 {
		// A participle after the candidate ("dance waiting"): the
		// candidate modifies it, unless it is an -s verb
		// ("tests waiting"). This also comes before the
		// object-region verb check: a participle like "running"
		// is a noun phrase head here, not the clause verb.
		// A transitive-only verb ("free", "slow") never takes a
		// gerund/participle complement, so before a participle it
		// is a modifier, not a clause head ("the skill and the
		// free running tests", rc20 F-A15-3 guard; "the skills
		// free running tests") — F-A16-4. Without this guard,
		// removing "free"/"slow" from neverVerbWords would let
		// the participle-with-object tail below read them as the
		// head of a participle phrase.
		if transitiveOnlyVerbs[word] {
			return false, true
		}
		if strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss") && len(word) > 2 && !verbFormAmbiguousNouns[word] && !neverVerbWords[word] {
			return true, false
		}
		// A listed base-form verb takes a gerund/participle
		// complement ("continue learning", "stop waiting",
		// "remain seated") — rc16 loaded these for the right
		// reason (F-A13-2).
		if baseFormVerbs[word] {
			return true, false
		}
		// The candidate must be able to head the clause: a noun
		// head (the subject of the participle phrase — "files
		// running tests") or a verb (taking the participle
		// phrase as a gerund object — "build waiting tests").
		// A never-verb word that is not a noun head ("big
		// running tests", F-A15-3) can be neither, so the
		// participle phrase is a noun-phrase modifier, not a
		// clause. A participle (-ing/-ed) is an adjective in
		// this position, not a head ("tired running tests") —
		// unless it is itself a noun head ("training running
		// tests"). A bare word after a plural subject is a verb
		// by shape (F-A14-1), so the plural path needs no
		// noun/verb evidence beyond not being a never-verb
		// word; after a singular subject a bare word needs noun
		// or verb evidence. A bare singular noun the validator
		// cannot recognise as a noun head ("notebook running
		// tests") is a known limitation: it rejects here, while
		// a listed noun head ("class running tests") loads.
		if neverVerbWords[word] && !isNounHead(word) {
			return false, true
		}
		if (strings.HasSuffix(word, "ing") && len(word) > 4 || strings.HasSuffix(word, "ed") && len(word) > 3) && !isNounHead(word) {
			return false, true
		}
		if !plural && !isNounHead(word) && !isVerbShaped(word) {
			return false, true
		}
		// A participle with an object ("files running tests",
		// "files running the tests") is a verb phrase, so the
		// candidate is its subject and a clause starts here
		// (F-A13-2). A participle with no object, or with a bare
		// singular noun ("running test") or a guarded noun
		// ("running tools"), is a modifier.
		if p+2 < len(fields) && !strings.ContainsAny(fields[p+1], ",.!?;:") {
			afterAfter := strings.Trim(fields[p+2], ".,;:()[]\"'`!?")
			if afterAfter == "the" || afterAfter == "a" || afterAfter == "an" || clausePronouns[afterAfter] || (strings.HasSuffix(afterAfter, "s") && !strings.HasSuffix(afterAfter, "ss") && len(afterAfter) > 2 && !verbFormNonVerbs[afterAfter]) {
				return true, false
			}
		}
		return false, true
	}
	if isClauseVerb(after) {
		// The word after the candidate is the verb ("files
		// run"): the candidate is its subject, so a clause
		// starts here — the same shape as the aux rule above
		// (F-A13-3). "The skill files run" loaded on rc16 (for
		// the wrong reason: "files" read as the verb) and
		// rejects on rc17; it loads here for the right reason.
		return true, false
	}
	// A bare singular word after the candidate ("venv", "plan",
	// "code", "team"): the candidate modifies it ("running venv",
	// "open venv", "tired venv", "work plan") — unless the
	// candidate is an -s verb ("tests code"), or (outside the
	// subject) a participle or object-region verb in predicate
	// position ("the skill running tests", "the skill open venv").
	if strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss") && len(word) > 2 {
		if verbFormAmbiguousNouns[word] {
			return false, true
		}
		if neverVerbWords[word] {
			// A never-verb word ("analysis") is a noun
			// modifier ("analysis plan"), not a verb
			// (F-A14-4).
			return false, true
		}
		return true, false
	}
	if strings.HasSuffix(word, "ing") && len(word) > 4 || strings.HasSuffix(word, "ed") && len(word) > 3 {
		if neverVerbWords[word] {
			// A never-verb word ("morning") is a noun
			// modifier ("morning code"), not a verb
			// (F-A14-4).
			return false, true
		}
		if inSubject {
			return false, true
		}
		return true, false
	}
	if isClauseVerb(word) {
		if inSubject {
			return false, true
		}
		return true, false
	}
	// A base-form word followed by a singular noun is a modifier
	// ("work plan"); anything else here is a subject word. (A
	// known non-verb — "tool", "venv", "ready" — already returned
	// above, so it never reaches this branch and never hides the
	// real verb: "the skill and the tool wait" still loads.)
	if inSubject {
		return false, true
	}
	return false, false
}

// skillStartsNewClause reports whether the skill at fields[j] is
// the subject of a new clause, so it is not an object of
// "activate" (F-A11-1, F-A12-1, F-A12-2). A new clause starts when
// the word after the skill is a verb (see clauseVerbAt — a
// copula/auxiliary, an object-region verb, or any other verb form
// recognised by shape, including a base form after a plural or
// compound subject), possibly after an adverb ("the skill then
// waits") or after a parenthetical comma ("the skill, is ready"),
// or when the skill is the first part of a compound subject ("the
// skill and the venv are ready"). Anything else after the skill —
// the end of the line, punctuation, a preposition or subordinating
// conjunction ("the skill for beginners"), another noun ("the
// skill the tool"), or a further list item ("the skill and the
// venv", with no verb after it) — leaves the skill a list object.
// A noun head after the skill ("the skill files") is a list
// object, not a clause (F-A12-1); a verb and its object ("the
// skill files the report") is a clause (F-A12-2).
func skillStartsNewClause(fields []string, j int) bool {
	// A plural subject ("the skills ...") takes a base-form verb
	// ("the skills wait"), a singular one an -s/-ed/-ing form.
	plural := strings.Trim(fields[j], ".,;:()[]\"'`!?") == "skills"
	// A parenthetical comma after the skill ("the skill, is
	// ready") separates the subject from its verb; skip it.
	k := j + 1
	for k < len(fields) {
		word := strings.Trim(fields[k], ".,;:()[]\"'`!?")
		if word == "" {
			raw := fields[k]
			if strings.Contains(raw, ",") && !strings.ContainsAny(raw, ".!?;:") {
				k++
				continue
			}
			return false
		}
		break
	}
	if k >= len(fields) {
		return false
	}
	next := strings.Trim(fields[k], ".,;:()[]\"'`!?")
	if next == "and" || next == "or" {
		// A conjunction after the skill: either another list
		// item ("the skill and the venv") or a compound
		// subject ("the skill and the venv are ready").
		return compoundSubjectStartsClause(fields, k)
	}
	if objectCloserNonVerbs[next] {
		// A preposition or subordinating conjunction heads a
		// phrase modifying the noun ("the skill for
		// beginners", "the skill that helps"), not a clause.
		return false
	}
	if clauseAdverbs[next] {
		// An adverb between the subject and its verb ("the
		// skill then waits"): the verb after the adverb
		// still starts the clause.
		if k+1 >= len(fields) {
			return false
		}
		after := strings.Trim(fields[k+1], ".,;:()[]\"'`!?")
		if after == "" {
			return false
		}
		isVerb, _ := clauseVerbAt(fields, k+1, plural, false, false)
		return isVerb
	}
	isVerb, _ := clauseVerbAt(fields, k, plural, false, false)
	return isVerb
}

// compoundSubjectStartsClause reports whether the "and"/"or" at
// fields[k] joins the skill into a compound subject of a new
// clause (F-A11-1): "the skill and the venv are ready". It scans
// the words after the conjunction until it finds a verb (a new
// clause, with the compound subject as its subject) or a list
// terminator — the end of the line, a comma or sentence
// terminator, or a preposition ("the skill and the venv", "the
// skill and the venv, the tool", "the skill and the venv for
// beginners" are still lists of objects). A further "and"/"or"
// continues the compound subject ("the skill and the venv and the
// tool are ready"). The compound subject is plural, so a verb may
// be a base form ("the skill and the venv wait") as well as an
// -s/-ed/-ing form or an auxiliary — and, since F-A12-2, a base
// form is recognised by shape (followed by an object/complement,
// or at a clause end), not only from baseFormVerbs.
// A word that modifies the following noun is not the verb
// (F-A12-1): an -ing/-ed participle, an object-region verb or a
// base-form word immediately before a noun ("the running venv",
// "the open venv", "the tired venv", "work plan") is an
// attributive adjective or noun compound, so the scan skips it
// AND its head noun — otherwise the head noun itself ("tests" in
// "the running tests") would be misread as the verb. The scan
// never accepts a word in object position as the verb: once a
// verb is found ("dance" in "the skill and the venv dance
// users"), the scan stops there and never reaches its object
// ("users").
func compoundSubjectStartsClause(fields []string, k int) bool {
	// sawNoun tracks whether a noun head of the compound subject
	// has been seen before the current position (F-A13-2): a
	// participle before a bare plural is a modifier only while
	// the subject noun phrase is still open ("the running
	// tests"), and is the predicate participle once a noun head
	// has been seen ("the venv waiting tests"). A conjunction
	// starts a new part of the compound subject, so it resets
	// the flag.
	sawNoun := false
	for j := k + 1; j < len(fields); j++ {
		raw := fields[j]
		word := strings.Trim(raw, ".,;:()[]\"'`!?")
		if word == "" {
			// A punctuation-only field ends the list here.
			return false
		}
		if objectCloserNonVerbs[word] {
			// A preposition or subordinating conjunction
			// heads a phrase modifying the noun, not a
			// clause.
			return false
		}
		if word == "and" || word == "or" {
			// Another part of the compound subject.
			sawNoun = false
			continue
		}
		isVerb, isModifier := clauseVerbAt(fields, j, true, true, sawNoun)
		if isVerb {
			return true
		}
		if isModifier {
			// A modifier and its head noun are both part of
			// the subject ("the running venv"): skip both,
			// so the head noun is never misread as the verb.
			// The head noun has been seen, so the subject
			// noun phrase is complete from here on. But a
			// modifier before a participle ("big running
			// tests", F-A15-3) heads a participle phrase,
			// not a single noun: the participle and its
			// object are part of the phrase too, so skip
			// all three and do NOT mark the subject
			// complete — the participle is a modifier, not
			// a noun head. Skipping only two would leave
			// the object ("tests") exposed as a
			// clause-final -s verb.
			if j+1 < len(fields) {
				next := strings.Trim(fields[j+1], ".,;:()[]\"'`!?")
				if strings.HasSuffix(next, "ing") && len(next) > 4 || strings.HasSuffix(next, "ed") && len(next) > 3 {
					j += 2
					continue
				}
			}
			sawNoun = true
			j++
			continue
		}
		if strings.ContainsAny(raw, ".,!?;:") {
			// A comma or sentence terminator attached to a
			// non-verb ends the list here.
			return false
		}
		// Otherwise a subject word (an article, an adjective,
		// or a noun): keep scanning for the verb. Only a
		// genuine noun head completes the subject noun phrase
		// (F-A14-3): an article, a subject modifier, an adverb
		// or an adjective ("new", "ready", "quickly") is a
		// subject word, not a noun head, so "the new running
		// tests" stays a noun phrase.
		if isNounHead(word) {
			sawNoun = true
		}
	}
	return false
}

// commaListNamesSkill reports whether the fields after a comma
// (starting at index i+1) name the skill as another object of
// the same "activate" — a comma-separated object list (F-A10-3):
// "Activate the tool, the skill" and "Activate the tool, the
// skill, and the venv" list the skill alongside the tool, so the
// skill is an object of "activate" and the line is a directive.
// The words between the comma and the skill are list-item
// modifiers (articles, adjectives, further commas and
// conjunctions); the scan stops at the first word that is a verb,
// an object closer, or a sentence terminator. A skill found this
// way is a list object UNLESS it starts a new clause with itself
// (or a compound subject) as the subject ("Activate the tool, the
// skill is ready" — "is" makes the skill the subject of a new
// clause, not an object), in which case it is not (F-A11-1: the
// clause is recognised by shape, see skillStartsNewClause, not
// by a finite verb list).
func commaListNamesSkill(fields []string, i int) bool {
	for j := i + 1; j < len(fields); j++ {
		raw := fields[j]
		word := strings.Trim(raw, ".,;:()[]\"'`!?")
		if word == "" {
			// A punctuation-only field: a sentence terminator
			// ends the scan (no list object here); a comma
			// just separates two more list items.
			if strings.ContainsAny(raw, ".!?;:") {
				return false
			}
			continue
		}
		if word == "skill" || word == "skills" {
			// The skill, named as a list item. It is an object
			// of "activate" unless it starts a new clause with
			// itself (or a compound subject) as the subject.
			return !skillStartsNewClause(fields, j)
		}
		if activateObjectClosers[word] {
			return false
		}
		if strings.ContainsAny(raw, ".!?;:") {
			return false
		}
	}
	return false
}

// segmentNamesSkillAsActivateObject reports whether seg — the text
// after one "activate" occurrence, up to the next occurrence —
// names a skill as an object of that "activate" (Audit 9
// F-A9-P2-3). Every object of the verb in the clause is
// considered, not just the first: "Activate the tool and the
// skill" and "Activate the tool, then the skill" are directives,
// because the skill is a conjoined / elided object of the same
// "activate". A skill named only after an object closer (see
// activateObjectClosers) is not: "Activate the tool using the
// skill" names the skill as the instrument of "using", and
// "Activate the tool for this skill" names it in a "for" phrase,
// so both still load. The object region runs from the occurrence
// through articles, adjectives, and the conjunctions "and"/"or"
// and the elision "then"; a comma keeps it open only before
// "then"/"and"/"or" — or before a comma-separated object list
// naming the skill ("Activate the tool, the skill", F-A10-3),
// which is still an object of the occurrence unless the skill
// starts a new clause with itself (or a compound subject) as the
// subject ("Activate the tool, the skill is ready"); and a
// sentence terminator (".", "!", "?", ";", ":") closes it.
//
// The new-clause carve-out applies only to a skill reached after
// a separator (a comma or a conjunction/elision, F-A11-1):
// "Activate the tool and the skill is ready" and "Activate the
// tool, and the skill is ready" name the skill as the subject of
// "is ready", not as an object of "activate". A skill in the
// direct-object position — immediately after "activate", before
// any separator ("Activate the skill is ready", "Activate the
// skill runs the tests") — is the object of "activate" whatever
// follows it: the verb after it is a relative clause ("the skill
// [that] runs the tests"), and if it were not the object then
// "activate" would be bare, which is itself a directive.
func segmentNamesSkillAsActivateObject(seg string) bool {
	fields := strings.Fields(seg)
	// seenSep records whether a comma or a conjunction/elision
	// ("and"/"or"/"then") has been passed before the skill: only
	// then is the skill a list/conjoined item that the new-clause
	// carve-out can excuse (F-A11-1).
	seenSep := false
	for i, raw := range fields {
		word := strings.Trim(raw, ".,;:()[]\"'`!?")
		if word == "" {
			// A standalone punctuation field: a sentence
			// terminator closes the region; a comma keeps it
			// open only before "then"/"and"/"or" — or before a
			// comma-separated object list naming the skill
			// (F-A10-3: "Activate the tool, the skill").
			if strings.ContainsAny(raw, ".!?;:") {
				return false
			}
			if strings.Contains(raw, ",") {
				if i+1 < len(fields) && isObjectContinuationWord(fields[i+1]) {
					seenSep = true
					continue
				}
				if commaListNamesSkill(fields, i) {
					return true
				}
				return false
			}
			continue
		}
		if word == "skill" || word == "skills" {
			// A skill in the direct-object position is an
			// object of "activate" whatever follows it; after
			// a separator it is a list/conjoined item, and it
			// is not an object when it starts a new clause
			// with itself (or a compound subject) as the
			// subject (F-A11-1).
			if seenSep && skillStartsNewClause(fields, i) {
				return false
			}
			return true
		}
		if word == "and" || word == "or" || word == "then" {
			// A conjunction/elision before the skill: what
			// follows is a conjoined or elided object, or a
			// new clause.
			seenSep = true
			continue
		}
		if activateObjectClosers[word] {
			return false
		}
		if strings.ContainsAny(raw, ".!?;:") {
			// A sentence terminator attached to this word
			// ("tool.") ends the clause: nothing after it in
			// this segment can be an object of this occurrence.
			return false
		}
		if strings.HasSuffix(raw, ",") {
			if i+1 < len(fields) && isObjectContinuationWord(fields[i+1]) {
				seenSep = true
				continue
			}
			// A comma-separated object list naming the skill
			// ("Activate the tool, the skill") is still an
			// object of this "activate" (F-A10-3); anything
			// else after the comma ends the region.
			if commaListNamesSkill(fields, i) {
				return true
			}
			return false
		}
	}
	return false
}

// segmentNamesSkillForActivationNoun reports whether seg — the text
// after an -ion activation noun — names the skill being activated:
// the complement ("of the skill"), or a quoted skill reference
// ("activation of \"other\""). The noun's complement is an "of"
// phrase, so unlike the verb form the skill may follow a
// preposition here (Audit 9 F-A9-P2-4).
func segmentNamesSkillForActivationNoun(seg string) bool {
	s := strings.TrimSpace(seg)
	// Strip a leading "of" complement head (and its article).
	for {
		fields := strings.Fields(s)
		if len(fields) == 0 {
			return false
		}
		first := strings.Trim(fields[0], ".,;:()[]\"'`")
		if first == "of" || first == "the" || first == "a" || first == "an" {
			s = strings.TrimSpace(strings.Join(fields[1:], " "))
			continue
		}
		break
	}
	if s != "" && (s[0] == '"' || s[0] == '\'') {
		return isQuotedSkillReference(s)
	}
	return skillWord.MatchString(s)
}

// isNegationWord reports whether w negates a verb: "not", "never",
// "cannot", "refuse" (and its forms), or any "n't" contraction.
func isNegationWord(w string) bool {
	switch w {
	case "not", "never", "cannot", "refuse", "refuses", "refused":
		return true
	}
	return strings.HasSuffix(w, "n't")
}

// clauseNegatesVerb reports whether the final clause fragment of
// before negates a verb in verbs (Audit 9 F-A9-P2-1). The fragment
// is cut at the last sentence terminator or clause separator (".",
// "!", "?", ",", ";", ":"), so a negation in an earlier clause or
// sentence never excuses this occurrence. For each negation word
// in the fragment, the verb it negates is the first word after it
// once the skip words (see negationSkipWords) are passed over; the
// carve-out applies only when that verb is in verbs. When the
// words run out, the negated verb is the occurrence itself (the
// next word after the fragment), which is activation-related by
// construction, so that counts too: "should not activate"
// (negation, then nothing) is a genuine negation of activate,
// while "do not forget to activate" is not — the verb after "not"
// is "forget".
func clauseNegatesVerb(before string, verbs map[string]bool) bool {
	frag := before
	if idx := strings.LastIndexAny(frag, ".!?,;:"); idx >= 0 {
		frag = frag[idx+1:]
	}
	words := make([]string, 0, len(strings.Fields(frag)))
	for _, f := range strings.Fields(frag) {
		words = append(words, strings.Trim(f, ".,;:()[]\"'`"))
	}
	for i, w := range words {
		if !isNegationWord(w) {
			continue
		}
		j := i + 1
		for j < len(words) && negationSkipWords[words[j]] {
			j++
		}
		if j >= len(words) {
			return true
		}
		if verbs[words[j]] {
			return true
		}
	}
	return false
}

// clauseHasNegation reports whether the final clause fragment of
// before negates the "activate" occurrence that follows it (Audit
// 9 F-A9-P2-1): the negation must govern an activate verb form,
// not merely appear in the clause. "should not activate", "do not
// activate", "never activate", "must not activate", "cannot
// activate", and "refuse to activate" are defensive instructions
// and load; "do not forget to activate", "do not hesitate to
// activate", and "never hesitate to activate" negate a different
// verb ("forget"/"hesitate") and are still directives.
func clauseHasNegation(before string) bool {
	return clauseNegatesVerb(before, activateVerbForms)
}

// clauseGovernorIsDirectiveVerb reports whether the last word of
// the final clause fragment of before (articles skipped) is a verb
// that can govern an -ion activation noun as a directive (Audit 9
// F-A9-P2-4): "Begin activation of the skill" is governed by
// "begin", while "This skill explains activation" is governed by
// "explains" and "Skill activation is manual" by nothing at all —
// both discussion, not directives.
func clauseGovernorIsDirectiveVerb(before string) bool {
	frag := before
	if idx := strings.LastIndexAny(frag, ".!?,;:"); idx >= 0 {
		frag = frag[idx+1:]
	}
	fields := strings.Fields(frag)
	for i := len(fields) - 1; i >= 0; i-- {
		w := strings.Trim(fields[i], ".,;:()[]\"'`")
		switch w {
		case "the", "a", "an", "this", "that", "these", "those":
			continue
		}
		return activationDirectiveVerbs[w]
	}
	return false
}

// prevWordIs reports whether the last word of before is word
// (punctuation around it ignored).
func prevWordIs(before, word string) bool {
	fields := strings.Fields(before)
	if len(fields) == 0 {
		return false
	}
	return strings.Trim(fields[len(fields)-1], ".,;:()[]\"'`") == word
}

// isQuotedSkillReference reports whether a segment that starts with
// a quoted object names a skill. A quoted name containing "skill"
// is a skill reference; a quoted name containing a non-skill object
// word ("the venv", "dark mode") is not; otherwise the word after
// the closing quote decides — a non-skill noun ("production"
// mode, "work" profile) makes it a non-skill object, and anything
// else (including nothing at all) leaves the Audit 7 rule: a bare
// quoted name after Activate is a skill reference. When in doubt
// it returns true (a directive): an unrecognised quoted object is
// treated as a skill, not waved through.
func isQuotedSkillReference(seg string) bool {
	q := seg[0]
	rest := seg[1:]
	quoted := rest
	after := ""
	if idx := strings.IndexByte(rest, q); idx >= 0 {
		quoted = rest[:idx]
		after = rest[idx+1:]
	}
	if skillWord.MatchString(quoted) {
		return true
	}
	if nonSkillObjectWord.MatchString(quoted) {
		return false
	}
	if fields := strings.Fields(after); len(fields) > 0 {
		if first := strings.Trim(fields[0], ".,;:()[]\"'`"); nonSkillObjectWords[first] {
			return false
		}
	}
	return true
}

// parseSkill extracts the name, description, and tools from file content.
// It never fails: a file with no "# " heading falls back to the file name
// (without .md) as the skill name, and missing description or tools
// sections simply leave those fields empty. Callers must reject empty
// files before calling this.
//
// The second result is warnings about headings that look like a Tools
// section but were not recognised as one (see normalizeToolsHeading):
// the file's restriction is then silently absent, and the caller logs
// the warning so the author finds out instead of running unrestricted.
//
// It also warns about "## Tools" entries that name no known tool
// (F-A17-14): the skill still loads with the name kept — an unknown
// name can never match a real tool at enforcement time, so keeping
// it is harmless — but the author is told, with a did-you-mean
// suggestion when one is close. parseSkill validates against the
// built-in tools; parseSkillWithKnownTools takes the caller's set
// (see Registry.SetKnownTools).
func parseSkill(path, content string) (Skill, []string) {
	return parseSkillWithKnownTools(path, content, knownBuiltInTools)
}

// parseSkillWithKnownTools is parseSkill validated against known.
func parseSkillWithKnownTools(path, content string, known map[string]bool) (Skill, []string) {
	if known == nil {
		known = knownBuiltInTools
	}
	skill := Skill{
		Path:    path,
		Content: content,
		Name:    strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
	}
	var warnings []string
	warnedHeadings := map[string]bool{}

	lines := strings.Split(content, "\n")

	// Name: first "# " heading.
	headingIdx := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			if name := strings.TrimSpace(strings.TrimPrefix(trimmed, "# ")); name != "" {
				skill.Name = name
				headingIdx = i
				break
			}
		}
	}

	// Description: first non-empty, non-heading, non-bullet paragraph
	// line after the name heading (or from the top when there is no
	// heading). Scanning stops at the first section heading, so a
	// file that jumps straight to a section has no description.
	// Fenced code blocks and list items are not prose: a file that
	// opens with a code fence used to take the fence marker itself
	// ("```go") as its description, and a numbered list ("1. ...")
	// the same way.
	inFence := false
	for i := headingIdx + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if isFenceLine(trimmed) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if headingLevel(trimmed) > 0 {
			break
		}
		if isBulletLine(trimmed) {
			continue
		}
		if _, ok := numberedItem(trimmed); ok {
			continue
		}
		skill.Description = trimmed
		break
	}

	// Tools: bulleted (or numbered) items under the "## Tools"
	// section, up to the next heading. Any ATX heading ends the
	// section unless it is a new "## " section header: a "### "
	// sub-heading inside Tools used to leave the section open, so
	// bullets under a "### Notes" subsection were parsed as tools.
	// The heading is normalised (see normalizeToolsHeading): a
	// trailing colon and the "Allowed/Available Tools" variants
	// count, because a skill author who writes one of those
	// intends a restriction, and the old exact-match-only parse
	// silently gave them none (Audit 5 F-A5-P2-25).
	//
	// A level-3 "### Tools" heading is accepted as the Tools
	// section too (Audit 6 F-A6-P2-9): a skill author who nests
	// the section under a "## " section used to get no tool list
	// and no error at all. It still warns, so the author learns
	// the canonical form is "## Tools"; a level-3 heading that
	// merely looks like Tools warns the same way a level-2 one
	// does. Deeper headings (#### and below) still end the
	// section without opening one, but they now warn too
	// (Audit 7 F-A7-P2-5): a "#### Tools" heading used to be
	// silently ignored, leaving the skill unrestricted with no
	// hint to the author.
	inTools := false
	seenTools := map[string]bool{}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if lvl := headingLevel(trimmed); lvl > 0 {
			switch lvl {
			case 2, 3:
				title := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
				isTools, toolsLike := normalizeToolsHeading(title)
				headingKey := strings.Repeat("#", lvl) + " " + title
				if lvl == 3 && isTools {
					inTools = true
					if !warnedHeadings[headingKey] {
						warnedHeadings[headingKey] = true
						warnings = append(warnings, fmt.Sprintf("level-3 Tools heading %q treated as the Tools section; prefer \"## Tools\"", title))
					}
				} else {
					inTools = isTools
				}
				if toolsLike && !warnedHeadings[headingKey] {
					warnedHeadings[headingKey] = true
					warnings = append(warnings, fmt.Sprintf("unrecognised Tools-like heading %q — no tool restriction applied; use \"## Tools\", \"## Allowed Tools\", or \"## Available Tools\"", title))
				}
			default:
				inTools = false
				// Level 4+ (level 1 is the name heading, and
				// is not a Tools section by construction):
				// a Tools or Tools-like heading this deep
				// cannot be the Tools section, so warn
				// instead of silently running unrestricted.
				if lvl >= 4 {
					title := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
					isTools, toolsLike := normalizeToolsHeading(title)
					if isTools || toolsLike {
						headingKey := strings.Repeat("#", lvl) + " " + title
						if !warnedHeadings[headingKey] {
							warnedHeadings[headingKey] = true
							warnings = append(warnings, fmt.Sprintf("level-%d Tools heading %q is too deep to be the Tools section — no tool restriction applied; use \"## Tools\"", lvl, title))
						}
					}
				}
			}
			continue
		}
		if !inTools {
			continue
		}
		item, numbered := numberedItem(trimmed)
		if !numbered {
			switch {
			case strings.HasPrefix(trimmed, "- "):
				item = strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			case strings.HasPrefix(trimmed, "* "):
				item = strings.TrimSpace(strings.TrimPrefix(trimmed, "* "))
			default:
				continue
			}
		}
		if item == "" {
			continue
		}
		// Strip a trailing " - description" or ": description" suffix
		// FIRST, then surrounding backticks. The old order (backticks
		// first) left a stray backtick on the name whenever a backticked
		// tool carried a description, e.g. "`exec` - runs shell"
		// became `exec`` instead of `exec`.
		if idx := strings.Index(item, " - "); idx >= 0 {
			item = strings.TrimSpace(item[:idx])
		}
		if idx := strings.Index(item, ": "); idx >= 0 {
			item = strings.TrimSpace(item[:idx])
		}
		item = strings.Trim(item, "`")
		item = strings.TrimSpace(item)
		if item == "" || seenTools[item] {
			continue
		}
		seenTools[item] = true
		skill.Tools = append(skill.Tools, item)
		// Unknown tool names warn (F-A17-14) instead of failing
		// the load: a misspelled name ("shell_exe") would
		// otherwise silently give the skill a tool it can never
		// receive, and the author would never find out. The name
		// stays in the list — it can never match a real tool at
		// enforcement time, so keeping it is harmless, and a
		// tool added later (or an MCP tool the loader did not
		// know about) then just works.
		if !known[item] {
			msg := fmt.Sprintf("unknown tool %q in \"## Tools\" of skill %q — the skill will never receive that tool", item, skill.Name)
			if sug := closestKnownTool(item, known); sug != "" {
				msg += fmt.Sprintf("; did you mean %q?", sug)
			}
			warnings = append(warnings, msg)
		}
	}

	return skill, warnings
}

// closestKnownTool returns the known tool name closest to name
// (Levenshtein distance ≤ 2), or "" when none is close — the
// did-you-mean suggestion for an unknown ## Tools entry
// (F-A17-14). Ties break towards the lexicographically smallest
// name, so the suggestion is deterministic.
func closestKnownTool(name string, known map[string]bool) string {
	best := ""
	bestDist := 3 // anything above the threshold never wins
	for candidate := range known {
		if d := levenshteinDistance(name, candidate); d < bestDist || (d == bestDist && d <= 2 && candidate < best) {
			best, bestDist = candidate, d
		}
	}
	if bestDist > 2 {
		return ""
	}
	return best
}

// levenshteinDistance is the edit distance between a and b
// (insert/delete/substitute, cost 1), over runes. Only used for
// the did-you-mean suggestion above, so the simple O(n*m) table
// is fine — tool names are a dozen characters at most.
func levenshteinDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur := make([]int, len(br)+1)
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 0
			if ar[i-1] != br[j-1] {
				cost = 1
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(br)]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// normalizeToolsHeading reports whether a level-2 heading title is a
// Tools section, and whether it merely looks like one.
//
// Recognised (case-insensitive, trailing colon ignored): "Tools",
// "Allowed Tools", "Available Tools". A title that is not recognised
// but contains "tool" or "tools" as a word ("Tool", "Tools and
// Setup", "My Tools") is Tools-like: the author probably meant a
// restriction, so the caller should warn rather than silently run
// the skill unrestricted (Audit 5 F-A5-P2-25).
func normalizeToolsHeading(title string) (isTools, toolsLike bool) {
	t := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(title), ":"))
	switch strings.ToLower(t) {
	case "tools", "allowed tools", "available tools":
		return true, false
	}
	for _, w := range strings.Fields(strings.ToLower(t)) {
		w = strings.Trim(w, ".,;:()[]\"'`")
		if w == "tool" || w == "tools" {
			return false, true
		}
	}
	return false, false
}

// headingLevel returns the ATX heading level (1-6) of line, or 0 when
// line is not an ATX heading ("# " ... "###### "). A line like
// "#hashtag" (no space after the hashes) is not a heading, and
// neither is a run of more than six hashes.
func headingLevel(line string) int {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0
	}
	if n < len(line) && line[n] != ' ' && line[n] != '\t' {
		return 0
	}
	return n
}

// isFenceLine reports whether line opens or closes a fenced code block
// ("```" or "~~~", optionally followed by an info string like "go").
func isFenceLine(line string) bool {
	return strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")
}

// isBulletLine reports whether line is a bulleted list item ("- " or
// "* ").
func isBulletLine(line string) bool {
	return strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ")
}

// numberedItem parses a numbered list item ("1. exec", "1) exec") and
// returns the item text. The second result is false when line is not
// a numbered item: no leading digits, no "." or ")" after them, or no
// space after the marker ("1.exec" is not a list item).
func numberedItem(line string) (string, bool) {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(line) {
		return "", false
	}
	if line[i] != '.' && line[i] != ')' {
		return "", false
	}
	rest := line[i+1:]
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return "", false
	}
	return strings.TrimSpace(rest), true
}
