// Package tools — grep and glob tools: content search and recursive
// filename matching, confined to the same file roots as the file
// tools (working directory and temp directory).
//
// Grep exists so the agent never needs `grep` via shell_exec for
// content search: a shell grep bypasses file confinement entirely
// (it can read ~/.orion/config.json and exfiltrate it), while this
// tool walks the tree itself, resolves every symlink before reading,
// refuses anything outside the allowed roots or inside a sensitive
// directory, and never touches the shell. Glob exists because
// file_list only lists one directory; recursive patterns such as
// **/*.go need a real matcher.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/irfan/orion/internal/model"
)

const (
	// defaultGrepMaxMatches is the default cap on matching lines a
	// grep call returns; maxGrepMaxMatches is the hard ceiling.
	defaultGrepMaxMatches = 100
	maxGrepMaxMatches     = 500
	// defaultGlobMaxResults is the default cap on paths a glob call
	// returns; maxGlobMaxResults is the hard ceiling.
	defaultGlobMaxResults = 100
	maxGlobMaxResults     = 1000
	// maxGrepFilesScanned / maxGlobFilesScanned bound how many
	// filesystem entries one call walks: a tree with millions of
	// files must not keep the agent waiting unboundedly.
	maxGrepFilesScanned = 10000
	maxGlobFilesScanned = 20000
	// maxGrepPatternRunes bounds the pattern: Go's regexp is RE2
	// (linear time, no catastrophic backtracking), so this is about
	// keeping absurd input out, not about ReDoS.
	maxGrepPatternRunes = 4096
	// maxGrepContextLines is the ceiling on context lines per side.
	maxGrepContextLines = 10
	// maxGrepLineRunes caps one rendered output line: a minified
	// file can hold a megabyte on a single line, and one such line
	// must not eat the whole result cap.
	maxGrepLineRunes = 1000
	// maxGrepTotalBytes caps the total bytes one grep call reads
	// across all files (F-A5-P2-31): the file-count cap alone
	// allows 10k files x 1 MB each, ~10 GB, bounded only by the
	// tool context. 32 MB keeps a no-match scan bounded while
	// still covering a large tree.
	maxGrepTotalBytes = 32 * 1024 * 1024
	// maxGrepRenderedLines caps the total lines one grep call
	// renders, matches and context together (F-A5-P2-32): 500
	// matches x (1 + 2x10 context lines) x 1000 runes/line is
	// ~10 MB in memory before the registry cap. 2000 lines is
	// ~2 MB worst case and still far more than a useful answer.
	maxGrepRenderedLines = 2000
	// maxGlobPatternRunes / maxGlobSegments bound a glob pattern
	// (F-A5-9): the matcher is linear once consecutive "**" is
	// collapsed, but an unbounded pattern is still absurd input.
	maxGlobPatternRunes = 512
	maxGlobSegments     = 64
)

// ---------------------------------------------------------------- helpers

// grepGlobMatch reports whether base name matches a shell-style glob
// (path.Match: * ? [...] over the whole name). An invalid pattern is
// an error, never a silent non-match.
func grepGlobMatch(pattern, name string) (bool, error) {
	return path.Match(pattern, name)
}

// truncateLineRunes shortens s to at most maxRunes runes, appending
// a marker when it cuts. Rune-aware so multi-byte text is never
// split.
func truncateLineRunes(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "… [line truncated]"
}

// isBinaryData reports whether data looks binary: a NUL byte in the
// first 8 KiB. Content search over binaries is noise (and can leak
// binary junk into the model context), so grep skips them.
func isBinaryData(data []byte) bool {
	n := len(data)
	if n > 8192 {
		n = 8192
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- grep

type grepTool struct {
	roots []string
}

func (t *grepTool) Name() string { return "grep" }

func (t *grepTool) Description() string {
	return "Search file contents for a pattern (regex by default, or literal text), recursively under a directory. Returns matching lines as path:line: text with optional context lines. Paths are confined to the working directory and the temp directory, exactly like file_read: paths outside them, symlinks escaping them, and sensitive directories are refused or skipped, never followed. Use this instead of shell_exec grep, which bypasses file confinement. Binary files and .git are skipped."
}

func (t *grepTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":          map[string]any{"type": "string", "description": "Regex pattern to search for (or literal text when literal is true). Required."},
				"path":             map[string]any{"type": "string", "description": "Directory (or file) to search under. Default: working directory."},
				"include":          map[string]any{"type": "string", "description": "Optional glob on the file base name, e.g. \"*.go\" — only matching files are searched."},
				"exclude":          map[string]any{"type": "string", "description": "Optional glob on the file base name, e.g. \"*_test.go\" — matching files are skipped."},
				"literal":          map[string]any{"type": "boolean", "description": "When true, pattern is matched as literal text, not a regex. Default false."},
				"case_insensitive": map[string]any{"type": "boolean", "description": "Case-insensitive matching. Default false."},
				"context_lines":    map[string]any{"type": "integer", "description": "Lines of context to show before and after each match. Default 0, max 10."},
				"max_matches":      map[string]any{"type": "integer", "description": "Maximum matching lines to return. Default 100, max 500."},
			},
			"required": []string{"pattern"},
		},
	}
}

type grepArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path,omitempty"`
	Include         string `json:"include,omitempty"`
	Exclude         string `json:"exclude,omitempty"`
	Literal         bool   `json:"literal,omitempty"`
	CaseInsensitive bool   `json:"case_insensitive,omitempty"`
	ContextLines    *int   `json:"context_lines,omitempty"`
	MaxMatches      *int   `json:"max_matches,omitempty"`
}

// grepMatcher tests one line.
type grepMatcher func(line string) bool

func compileGrepMatcher(pattern string, literal, caseInsensitive bool) (grepMatcher, error) {
	if literal {
		if caseInsensitive {
			folded := strings.ToLower(pattern)
			return func(line string) bool {
				return strings.Contains(strings.ToLower(line), folded)
			}, nil
		}
		return func(line string) bool { return strings.Contains(line, pattern) }, nil
	}
	src := pattern
	if caseInsensitive {
		src = "(?i)" + src
	}
	re, err := regexp.Compile(src)
	if err != nil {
		return nil, fmt.Errorf("invalid regex %q: %w", pattern, err)
	}
	return func(line string) bool { return re.MatchString(line) }, nil
}

// grepHit is one matching line plus its context window.
type grepHit struct {
	path    string
	lineNo  int // 1-based
	text    string
	context bool // false = match line, true = context line
}

func (t *grepTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a grepArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	pattern := a.Pattern
	if strings.TrimSpace(pattern) == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("pattern must not be empty")}
	}
	if utf8.RuneCountInString(pattern) > maxGrepPatternRunes {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("pattern is over %d characters", maxGrepPatternRunes)}
	}
	match, err := compileGrepMatcher(pattern, a.Literal, a.CaseInsensitive)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
	}
	if a.Include != "" {
		if _, err := grepGlobMatch(a.Include, ""); err != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("invalid include glob %q: %w", a.Include, err)}
		}
	}
	if a.Exclude != "" {
		if _, err := grepGlobMatch(a.Exclude, ""); err != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("invalid exclude glob %q: %w", a.Exclude, err)}
		}
	}
	contextLines := 0
	if a.ContextLines != nil {
		contextLines = *a.ContextLines
		if contextLines < 0 {
			contextLines = 0
		}
		if contextLines > maxGrepContextLines {
			contextLines = maxGrepContextLines
		}
	}
	maxMatches := defaultGrepMaxMatches
	if a.MaxMatches != nil {
		maxMatches = *a.MaxMatches
		if maxMatches < 1 {
			maxMatches = 1
		}
		if maxMatches > maxGrepMaxMatches {
			maxMatches = maxGrepMaxMatches
		}
	}
	base := strings.TrimSpace(a.Path)
	if base == "" {
		base = "."
	}
	// The walk root is the RESOLVED confined path: a symlinked
	// root is resolved once here (confinePath refuses sensitive
	// targets), and every entry below it is checked against this
	// root before it is read, so a symlink inside the tree can
	// never smuggle a read outside it.
	root, err := confinePath(base, t.roots)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
	}
	if err := ctx.Err(); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: err}
	}
	// A single file is searched directly (no walk): grep over a
	// file path must behave like grep over a directory holding it.
	if info, err := os.Lstat(root); err == nil && !info.IsDir() {
		// Fail fast on symlinks and non-regular files
		// (fifo/socket/device): the safe read below would block
		// on a writer-less fifo until the read timeout
		// (F-A5-P2-36) — the same pre-check file_edit uses.
		// This is only a fast path: the read itself still goes
		// through the O_NOFOLLOW open + fd verification, so a
		// swap between this check and the read cannot redirect
		// it; and a missing file (Lstat error) falls through to
		// the walk, which reports it.
		if info.Mode()&os.ModeSymlink != 0 {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("path %q is a symlink; symlinked files are refused", root)}
		}
		if !info.Mode().IsRegular() {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("path %q is not a regular file", root)}
		}
		return t.grepFiles(ctx, []string{root}, match, contextLines, maxMatches, root), nil
	}

	var files []string
	scanned := 0
	skippedSymlink := 0
	scanCapped := false
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable entry must not abort the whole
			// search; it is skipped and counted below via the
			// walk error note. Returning the error would make
			// one bad file fail every match.
			return nil
		}
		if scanned%256 == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
		}
		scanned++
		if scanned > maxGrepFilesScanned {
			scanCapped = true
			return filepath.SkipAll
		}
		if d.IsDir() {
			// A symlinked directory never arrives here:
			// WalkDir Lstat-reports it with IsDir()==false, so
			// it is handled by the symlink branch below (which
			// resolves the target and checks confinement). The
			// symlink check that used to live here was dead code
			// (F-A5-P2-36) and is gone.
			if d.Name() == ".git" && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		// Files only from here on.
		if d.Type()&fs.ModeSymlink != 0 {
			// A symlinked file is followed only when its target
			// stays inside the walk root, outside every
			// sensitive directory, and outside any .git
			// directory; anything else is skipped and counted,
			// never read. The .git check closes the bypass where
			// a link to a file inside .git was read even though
			// the walk skips .git dirs (F-A5-P2-29).
			target, rerr := filepath.EvalSymlinks(p)
			if rerr != nil || !within(target, root) || inSensitiveDir(target) || underGitDir(target) {
				skippedSymlink++
				return nil
			}
			// Include/exclude apply to the link name, exactly
			// as for regular files below: a symlinked file must
			// not dodge the filter (F-A5-P2-28).
			if a.Include != "" {
				if ok, _ := grepGlobMatch(a.Include, d.Name()); !ok {
					return nil
				}
			}
			if a.Exclude != "" {
				if ok, _ := grepGlobMatch(a.Exclude, d.Name()); ok {
					return nil
				}
			}
			files = append(files, target)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if a.Include != "" {
			if ok, _ := grepGlobMatch(a.Include, d.Name()); !ok {
				return nil
			}
		}
		if a.Exclude != "" {
			if ok, _ := grepGlobMatch(a.Exclude, d.Name()); ok {
				return nil
			}
		}
		files = append(files, p)
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		if ctx.Err() != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: ctx.Err()}
		}
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot walk %q: %w", root, walkErr)}
	}
	sort.Strings(files)
	out := t.grepFiles(ctx, files, match, contextLines, maxMatches, root)
	if scanCapped {
		out += fmt.Sprintf("\n[stopped after scanning %d entries; refine path/pattern to search more]", maxGrepFilesScanned)
	}
	if skippedSymlink > 0 {
		out += fmt.Sprintf("\n[skipped %d symlinked path(s) outside the searched directory]", skippedSymlink)
	}
	return out, nil
}

// inSensitiveDir reports whether path sits inside a sensitive
// directory ($HOME/.orion, .ssh, .gnupg). Used as defence in depth
// for symlink targets grep resolves itself: the walk root is
// already outside them, but a target must be checked on its own
// terms too.
func inSensitiveDir(path string) bool {
	for _, d := range sensitiveDirs() {
		if within(path, d) {
			return true
		}
	}
	return false
}

// underGitDir reports whether path sits under a .git directory:
// any path component equal to ".git". Grep skips .git contents
// (the walk skips .git dirs), and a symlink into .git must not
// smuggle those files back in (F-A5-P2-29). The same guard is
// applied to glob's symlink targets.
func underGitDir(path string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(path), "/") {
		if seg == ".git" {
			return true
		}
	}
	return false
}

// grepFiles searches each file in paths (already confined, sorted)
// and renders the matches with context.
func (t *grepTool) grepFiles(ctx context.Context, paths []string, match grepMatcher, contextLines, maxMatches int, root string) string {
	_ = root
	var hits []grepHit
	matchCount := 0
	filesWithMatches := 0
	binarySkipped := 0
	truncatedFiles := 0
	hardlinkSkipped := 0
	totalBytes := 0
	capped := false
	bytesCapped := false
	renderCapped := false
	for _, p := range paths {
		if matchCount >= maxMatches {
			capped = true
			break
		}
		if renderCapped {
			break
		}
		if err := ctx.Err(); err != nil {
			break
		}
		// Total-bytes cap (F-A5-P2-31): stop starting new files
		// once the call has read maxGrepTotalBytes; the file
		// that crosses the cap is still searched in full, so
		// the total overshoots by at most one file.
		if totalBytes >= maxGrepTotalBytes {
			bytesCapped = true
			break
		}
		// readFileBounded opens with O_NOFOLLOW and verifies the
		// opened fd is the confined path, so a file swapped for
		// a symlink between the walk and the read is refused
		// instead of followed. It also caps the read at 1MB.
		data, info, err := readFileBounded(ctx, p, p)
		if err != nil {
			// One unreadable file must not fail the search.
			// A hardlink refusal is counted separately so the
			// footer can say why the file was not searched
			// (F-A5-P2-30).
			if errors.Is(err, errHardlinkRefused) {
				hardlinkSkipped++
			}
			continue
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			continue
		}
		totalBytes += len(data)
		if len(data) > maxFileRead {
			truncatedFiles++
		}
		if isBinaryData(data) {
			binarySkipped++
			continue
		}
		lines := strings.Split(string(data), "\n")
		var matched []int // 0-based indexes of matching lines
		for i, line := range lines {
			if match(line) {
				matched = append(matched, i)
				matchCount++
				if matchCount >= maxMatches {
					// The cap stopped the scan mid-file: there
					// may be more matches below, so the footer
					// must say so even when no further file
					// follows to trip the outer-loop check.
					capped = true
					break
				}
			}
		}
		if len(matched) == 0 {
			continue
		}
		filesWithMatches++
		// Merge overlapping context windows so a cluster of
		// matches prints once, with context lines interleaved.
		type window struct{ from, to int }
		var windows []window
		for _, m := range matched {
			from := m - contextLines
			if from < 0 {
				from = 0
			}
			to := m + contextLines
			if to >= len(lines) {
				to = len(lines) - 1
			}
			if len(windows) > 0 && from <= windows[len(windows)-1].to+1 {
				if to > windows[len(windows)-1].to {
					windows[len(windows)-1].to = to
				}
			} else {
				windows = append(windows, window{from, to})
			}
		}
		matchedSet := make(map[int]bool, len(matched))
		for _, m := range matched {
			matchedSet[m] = true
		}
		// Render cap (F-A5-P2-32): stop appending rendered lines
		// at maxGrepRenderedLines, matches and context together.
		// The footer says so; the outer loop then stops scanning
		// further files, since more matches could not be shown.
	render:
		for _, w := range windows {
			for i := w.from; i <= w.to; i++ {
				if len(hits) >= maxGrepRenderedLines {
					renderCapped = true
					break render
				}
				hits = append(hits, grepHit{
					path:    p,
					lineNo:  i + 1,
					text:    truncateLineRunes(lines[i], maxGrepLineRunes),
					context: !matchedSet[i],
				})
			}
		}
	}
	if matchCount == 0 {
		// A scan that found nothing still reports why it
		// stopped early, when it did: otherwise a capped scan
		// looks identical to an empty tree.
		var notes []string
		if bytesCapped {
			notes = append(notes, fmt.Sprintf("stopped after scanning %d bytes (cap %d MB)", totalBytes, maxGrepTotalBytes/(1024*1024)))
		}
		if hardlinkSkipped > 0 {
			notes = append(notes, fmt.Sprintf("skipped %d file(s) with multiple hard links; refusing to read", hardlinkSkipped))
		}
		if len(notes) > 0 {
			return "(no matches)\n[" + strings.Join(notes, "; ") + "]"
		}
		return "(no matches)"
	}
	var sb strings.Builder
	for _, h := range hits {
		sep := ":"
		if h.context {
			sep = "-"
		}
		fmt.Fprintf(&sb, "%s%s%d%s %s\n", h.path, sep, h.lineNo, sep, h.text)
	}
	fmt.Fprintf(&sb, "[%d matching line(s) in %d file(s)", matchCount, filesWithMatches)
	if capped {
		fmt.Fprintf(&sb, "; capped at %d matches", maxMatches)
	}
	if bytesCapped {
		fmt.Fprintf(&sb, "; stopped after scanning %d bytes (cap %d MB)", totalBytes, maxGrepTotalBytes/(1024*1024))
	}
	if renderCapped {
		fmt.Fprintf(&sb, "; output truncated to %d rendered lines", maxGrepRenderedLines)
	}
	if hardlinkSkipped > 0 {
		fmt.Fprintf(&sb, "; skipped %d file(s) with multiple hard links", hardlinkSkipped)
	}
	if binarySkipped > 0 {
		fmt.Fprintf(&sb, "; skipped %d binary file(s)", binarySkipped)
	}
	if truncatedFiles > 0 {
		fmt.Fprintf(&sb, "; %d file(s) over 1MB searched only in the first 1MB", truncatedFiles)
	}
	sb.WriteString("]")
	return strings.TrimRight(sb.String(), "\n")
}

// ---------------------------------------------------------------- glob

type globTool struct {
	roots []string
}

func (t *globTool) Name() string { return "glob" }

func (t *globTool) Description() string {
	return "Find files and directories by recursive glob pattern, e.g. \"**/*.go\" or \"src/**/*.ts\", under a directory. Patterns are matched against the path relative to that directory: \"*\" matches within one path segment, \"**\" matches zero or more segments, \"?\" matches one character and \"[...]\" a character class. \"*.go\" matches only the top level; use \"**/*.go\" for all depths. Paths are confined to the working directory and the temp directory, exactly like file_list. Symlinks escaping the directory are skipped, never followed."
}

func (t *globTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":     map[string]any{"type": "string", "description": "Glob pattern relative to path, e.g. \"**/*.go\". Required."},
				"path":        map[string]any{"type": "string", "description": "Directory to search under. Default: working directory."},
				"max_results": map[string]any{"type": "integer", "description": "Maximum paths to return. Default 100, max 1000."},
			},
			"required": []string{"pattern"},
		},
	}
}

type globArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
	MaxResults *int   `json:"max_results,omitempty"`
}

// validateGlobPattern checks a relative glob pattern: no absolute
// paths, no "." or ".." segments (they would escape the base), no
// empty segments, and every non-"**" segment must be a valid
// path.Match pattern. It returns the cleaned segments.
//
// The pattern is bounded (F-A5-9): at most maxGlobPatternRunes
// runes and maxGlobSegments segments, and consecutive "**"
// segments collapse to one — "**/**" matches exactly what "**"
// does, but the old recursive matcher re-tried every split for
// each "**", so 20 "**" segments took 42s for a single match on a
// 12-deep tree. The matcher itself is linear-time now (see
// matchGlobSegments), and the collapse keeps patterns small.
func validateGlobPattern(pattern string) ([]string, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, fmt.Errorf("pattern must not be empty")
	}
	if filepath.IsAbs(pattern) {
		return nil, fmt.Errorf("pattern %q must be relative to the search directory, not absolute", pattern)
	}
	pattern = filepath.ToSlash(pattern)
	if strings.Contains(pattern, "\x00") {
		return nil, fmt.Errorf("pattern must not contain NUL")
	}
	if utf8.RuneCountInString(pattern) > maxGlobPatternRunes {
		return nil, fmt.Errorf("pattern is over %d characters", maxGlobPatternRunes)
	}
	parts := strings.Split(pattern, "/")
	if len(parts) > maxGlobSegments {
		return nil, fmt.Errorf("pattern has %d segments, over the %d-segment limit", len(parts), maxGlobSegments)
	}
	collapsed := make([]string, 0, len(parts))
	for _, seg := range parts {
		switch seg {
		case "", ".", "..":
			return nil, fmt.Errorf("pattern %q has an empty, \".\" or \"..\" segment; patterns stay inside the search directory", pattern)
		case "**":
			// Consecutive "**" is semantically one "**".
			if len(collapsed) > 0 && collapsed[len(collapsed)-1] == "**" {
				continue
			}
			collapsed = append(collapsed, seg)
		default:
			if _, err := path.Match(seg, ""); err != nil {
				return nil, fmt.Errorf("invalid glob segment %q in pattern %q: %w", seg, pattern, err)
			}
			collapsed = append(collapsed, seg)
		}
	}
	return collapsed, nil
}

// matchGlobSegments reports whether the path segments name match
// the pattern segments pat. "**" matches zero or more segments;
// any other segment must match exactly one segment via path.Match.
//
// Dynamic programming over (pattern, name) prefixes: O(len(pat) x
// len(name)) time, no recursion, no exponential blowup on "**"
// (F-A5-9). The walk already bounds name depth by the filesystem,
// and validateGlobPattern bounds pat, so one match call is cheap
// and the walk's periodic ctx check is enough to interrupt a call.
func matchGlobSegments(pat, name []string) bool {
	// dp[j] = pat[:i] matches name[:j] for the current i.
	dp := make([]bool, len(name)+1)
	dp[0] = true
	for i := 0; i < len(pat); i++ {
		ndp := make([]bool, len(name)+1)
		if pat[i] == "**" {
			// "**" eats zero segments (dp[j]) or one more
			// segment on top of what it already ate (ndp[j-1]).
			for j := 0; j <= len(name); j++ {
				if dp[j] {
					ndp[j] = true
				} else if j > 0 && ndp[j-1] {
					ndp[j] = true
				}
			}
		} else {
			for j := 1; j <= len(name); j++ {
				if dp[j-1] {
					if ok, _ := path.Match(pat[i], name[j-1]); ok {
						ndp[j] = true
					}
				}
			}
		}
		dp = ndp
	}
	return dp[len(name)]
}

func (t *globTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a globArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	pat, err := validateGlobPattern(a.Pattern)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
	}
	maxResults := defaultGlobMaxResults
	if a.MaxResults != nil {
		maxResults = *a.MaxResults
		if maxResults < 1 {
			maxResults = 1
		}
		if maxResults > maxGlobMaxResults {
			maxResults = maxGlobMaxResults
		}
	}
	base := strings.TrimSpace(a.Path)
	if base == "" {
		base = "."
	}
	root, err := confinePath(base, t.roots)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
	}
	if err := ctx.Err(); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: err}
	}
	// A single file as the base matches only when the pattern
	// matches its own name.
	if info, err := os.Lstat(root); err == nil && !info.IsDir() {
		if matchGlobSegments(pat, []string{filepath.Base(root)}) {
			return root, nil
		}
		return "(no matches)", nil
	}

	var matches []string
	scanned := 0
	skippedSymlink := 0
	capped := false
	scanCapped := false
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if scanned%256 == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
		}
		scanned++
		if scanned > maxGlobFilesScanned {
			scanCapped = true
			return filepath.SkipAll
		}
		if p == root {
			return nil
		}
		if d.IsDir() {
			// A symlinked directory never arrives here:
			// WalkDir Lstat-reports it with IsDir()==false, so
			// it is handled by the symlink branch below. The
			// symlink check that used to live here was dead code
			// (F-A5-P2-36) and is gone.
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
		} else if d.Type()&fs.ModeSymlink != 0 {
			// Symlinked files match only when their target stays
			// inside the walk root, outside sensitive dirs, and
			// outside any .git directory (same guard as grep:
			// a link into .git must not smuggle its files back
			// in, F-A5-P2-29).
			target, rerr := filepath.EvalSymlinks(p)
			if rerr != nil || !within(target, root) || inSensitiveDir(target) || underGitDir(target) {
				skippedSymlink++
				return nil
			}
		} else if !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		if !matchGlobSegments(pat, strings.Split(filepath.ToSlash(rel), "/")) {
			return nil
		}
		if len(matches) >= maxResults {
			capped = true
			return filepath.SkipAll
		}
		display := p
		if d.IsDir() {
			display = p + string(filepath.Separator)
		}
		matches = append(matches, display)
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		if ctx.Err() != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: ctx.Err()}
		}
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot walk %q: %w", root, walkErr)}
	}
	if len(matches) == 0 {
		return "(no matches)", nil
	}
	sort.Strings(matches)
	out := strings.Join(matches, "\n")
	if capped {
		out += fmt.Sprintf("\n[showing first %d matches; refine the pattern to see more]", maxResults)
	}
	if scanCapped {
		out += fmt.Sprintf("\n[stopped after scanning %d entries; refine path/pattern to search more]", maxGlobFilesScanned)
	}
	if skippedSymlink > 0 {
		out += fmt.Sprintf("\n[skipped %d symlinked path(s) outside the searched directory]", skippedSymlink)
	}
	return out, nil
}
