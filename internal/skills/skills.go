// Package skills provides a registry of Markdown skill files.
//
// A skill is a Markdown document whose first "# " heading is its name,
// whose first paragraph after the heading is its description, and whose
// optional "## Tools" section lists the tools the skill may use. The
// registry loads skills from a directory of *.md files (or a single file),
// keeps them in memory, and supports lookup, listing, and search.
package skills

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	// "1) exec") items are recognised.
	Tools []string
}

// Registry is a concurrency-safe store of skills, keyed by name.
type Registry struct {
	mu     sync.RWMutex
	skills map[string]Skill
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
func (r *Registry) Register(s Skill) error {
	if r == nil {
		return errors.New("skills: Register on nil Registry")
	}
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("skills: cannot register skill with empty name")
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
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			continue
		}
		sawMD = true
		path := filepath.Join(dir, entry.Name())
		skill, err := parseFile(path)
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

// LoadFile parses a single skill file and returns it. The skill is not
// registered; use Register to add it to the registry.
func (r *Registry) LoadFile(path string) (Skill, error) {
	return parseFile(path)
}

// parseFile reads one skill file (capped at maxFileSize) and parses it.
func parseFile(path string) (Skill, error) {
	if strings.TrimSpace(path) == "" {
		return Skill{}, errors.New("skills: empty file path")
	}
	f, err := os.Open(path)
	if err != nil {
		return Skill{}, fmt.Errorf("skills: cannot open %q: %w", path, err)
	}
	defer f.Close()

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
	skill := parseSkill(path, string(data))
	if strings.TrimSpace(skill.Name) == "" {
		// Reachable when the filename itself yields no name (e.g. a
		// file literally called ".md") and the file has no "# " heading:
		// an empty-named skill could never be registered, so fail here
		// instead of returning an unusable Skill.
		return Skill{}, fmt.Errorf("skills: file %q has no skill name (no \"# \" heading and the file name yields none)", path)
	}
	return skill, nil
}

// parseSkill extracts the name, description, and tools from file content.
// It never fails: a file with no "# " heading falls back to the file name
// (without .md) as the skill name, and missing description or tools
// sections simply leave those fields empty. Callers must reject empty
// files before calling this.
func parseSkill(path, content string) Skill {
	skill := Skill{
		Path:    path,
		Content: content,
		Name:    strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
	}

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
	inTools := false
	seenTools := map[string]bool{}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if lvl := headingLevel(trimmed); lvl > 0 {
			if lvl == 2 {
				title := strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
				inTools = strings.EqualFold(title, "Tools")
			} else {
				inTools = false
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
	}

	return skill
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
