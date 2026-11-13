// Drift verification for recalled typed memories.
//
// A typed memory that names a file, function or flag is a claim
// about the tree as it was when the memory was written. Before the
// agent injects (or acts on) such a memory, the claim is verified
// against the current tree: files must exist, function and flag
// names must still appear somewhere in the tree. On conflict the
// current state wins — the memory is dropped from injection and
// marked stale (see Agent.markStaleMemory in recall_v2.go) —
// instead of the agent confidently recommending something that no
// longer exists.
//
// References are extracted from the memory body (and description)
// in three forms: backticked paths / identifiers, bare
// `path/to/file.go` paths, `FuncName(` calls, and `--flag` forms.
// Verification is read-only and bounded: the tree walk skips
// version-control and hidden directories, skips files over 1 MiB,
// and stops after maxDriftFilesScanned files, so a huge tree cannot
// stall a turn. A memory with no references is never stale.
package core

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// maxDriftFilesScanned bounds the tree grep for symbol/flag checks.
const maxDriftFilesScanned = 5000

// maxDriftFileBytes bounds how much of one file is read while grepping.
const maxDriftFileBytes = 1 << 20 // 1 MiB

// refKind names what a codeReference points at.
type refKind string

const (
	refFile   refKind = "file"
	refSymbol refKind = "symbol"
	refFlag   refKind = "flag"
)

// codeReference is one extracted claim from a memory body.
type codeReference struct {
	Kind refKind
	Text string // file path (relative), symbol name, or flag (with --)
}

func (r codeReference) String() string {
	return string(r.Kind) + ":" + r.Text
}

// backtickPattern finds `...` spans in a memory body.
var backtickPattern = regexp.MustCompile("`([^`\\n]{1,200})`")

// barePathPattern finds path/to/file.ext forms (with a slash or a
// known code extension) in plain prose.
var barePathPattern = regexp.MustCompile(`\b[\w.\-]+(?:/[\w.\-]+)+\.(?:go|md|json|ts|tsx|js|jsx|yaml|yml|toml|sh|txt|py|rs)\b`)

// bareFilePattern finds a bare filename with a code extension and no
// slash (e.g. "see config.json"). It is only used for backticked
// spans and bare paths are handled above; a bare extension match in
// prose is too noisy ("version 1.0" is not a file).
var codeExtPattern = regexp.MustCompile(`\.(?:go|md|json|ts|tsx|js|jsx|yaml|yml|toml|sh|py|rs)$`)

// funcCallPattern finds ExportedName( calls in prose: Go exported
// identifiers (capital first letter) followed by "(". Lowercase
// prose words before "(" ("if (", "call (") are not code.
var funcCallPattern = regexp.MustCompile(`\b([A-Z][A-Za-z0-9_]*)\(`)

// flagPattern finds --flag forms.
var flagPattern = regexp.MustCompile(`--[a-z][a-z0-9\-]*`)

// extractReferences pulls candidate file/symbol/flag references out
// of text (a memory body plus description). Results are deduplicated,
// sorted for determinism, and empty when the text names nothing.
//
// Plausibility rule (Audit 4 B P1-3): a bare --flag or Capitalised(
// in plain prose is NOT a tree claim on its own — flags of external
// tools ("Deploy with --prod", "pass --no-preserve-root") and
// ordinary prose symbols ("Call Start( to begin") are not claims
// about this repo, and treating them as such permanently killed
// valid guidance. Prose flags/symbols only count as claims when the
// memory shows code context: a backticked span or a bare file path
// in the same text. Backticked refs are always claims (backticks
// signal code intent), as are bare file paths.
func extractReferences(text string) []codeReference {
	seen := map[codeReference]struct{}{}
	add := func(kind refKind, s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		seen[codeReference{Kind: kind, Text: s}] = struct{}{}
	}

	// Backticked spans: classify by shape. Any backticked span is
	// code context for the prose rules below.
	backticked := backtickPattern.FindAllStringSubmatch(text, -1)
	hasCodeContext := len(backticked) > 0
	for _, m := range backticked {
		inner := strings.TrimSpace(m[1])
		switch {
		case strings.HasPrefix(inner, "--"):
			add(refFlag, flagPattern.FindString(inner))
		case strings.HasSuffix(inner, "(") || strings.Contains(inner, "("):
			// `Run(` or `Run(ctx` — take the identifier before "(".
			if idx := strings.Index(inner, "("); idx > 0 {
				add(refSymbol, inner[:idx])
			}
		case strings.Contains(inner, "/") || codeExtPattern.MatchString(inner):
			add(refFile, strings.TrimPrefix(inner, "./"))
		case funcCallPattern.MatchString(inner):
			add(refSymbol, funcCallPattern.FindStringSubmatch(inner)[1])
		}
	}

	// Bare paths in prose. A bare path is code context too.
	barePaths := barePathPattern.FindAllString(text, -1)
	if len(barePaths) > 0 {
		hasCodeContext = true
	}
	for _, p := range barePaths {
		add(refFile, strings.TrimPrefix(p, "./"))
	}
	if !hasCodeContext {
		// No code context: prose flags and symbols are not tree
		// claims, so there is nothing more to extract.
		return sortedReferences(seen)
	}
	// Exported function calls in prose.
	for _, m := range funcCallPattern.FindAllStringSubmatch(text, -1) {
		add(refSymbol, m[1])
	}
	// Flags in prose.
	for _, f := range flagPattern.FindAllString(text, -1) {
		add(refFlag, f)
	}

	return sortedReferences(seen)
}

// sortedReferences renders the deduped set as a sorted slice.
func sortedReferences(seen map[codeReference]struct{}) []codeReference {
	out := make([]codeReference, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Text < out[j].Text
	})
	return out
}

// driftCheck verifies a typed memory's references against the tree
// rooted at root ("" means the process working directory). It
// returns stale=true plus the missing references when any reference
// does not exist; stale=false when the memory names nothing or
// everything checks out. It never returns an error: an unreadable
// root degrades to "not stale" with no verification, so recall is
// never blocked by a filesystem problem.
func driftCheck(root string, mem TypedMemory) (bool, []codeReference) {
	refs := extractReferences(mem.Body + "\n" + mem.Description)
	if len(refs) == 0 {
		return false, nil
	}
	if strings.TrimSpace(root) == "" {
		wd, err := os.Getwd()
		if err != nil {
			return false, nil
		}
		root = wd
	}
	// An unreadable root (missing, not a directory) means
	// verification cannot run: degrade to "not stale" rather than
	// condemning every reference and blocking recall.
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return false, nil
	}
	missing := verifyReferences(root, refs)
	if len(missing) == 0 {
		return false, nil
	}
	return true, missing
}

// verifyReferences returns the subset of refs that do not exist in
// the tree rooted at root: files that cannot be stat'ed, symbols and
// flags that appear in no scanned file.
func verifyReferences(root string, refs []codeReference) []codeReference {
	var missing []codeReference
	var needGrep []codeReference
	for _, r := range refs {
		switch r.Kind {
		case refFile:
			if !fileExistsIn(root, r.Text) {
				missing = append(missing, r)
			}
		case refSymbol, refFlag:
			needGrep = append(needGrep, r)
		}
	}
	if len(needGrep) == 0 {
		return missing
	}
	found := grepTree(root, needGrep)
	for _, r := range needGrep {
		if !found[r] {
			missing = append(missing, r)
		}
	}
	return missing
}

// fileExistsIn reports whether path (relative to root, or absolute)
// exists as a file or directory.
func fileExistsIn(root, path string) bool {
	full := path
	if !filepath.IsAbs(full) {
		full = filepath.Join(root, path)
	}
	_, err := os.Stat(full)
	return err == nil
}

// grepTree scans the tree under root for each wanted symbol/flag and
// returns the set that appears in at least one scanned file. The
// walk skips hidden and version-control directories, skips files
// over maxDriftFileBytes, and stops after maxDriftFilesScanned
// files, so verification stays cheap on large trees.
func grepTree(root string, want []codeReference) map[codeReference]bool {
	found := map[codeReference]bool{}
	pending := len(want)
	if pending == 0 {
		return found
	}
	needles := make([]string, len(want))
	for i, r := range want {
		needles[i] = r.Text
	}
	scanned := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || pending == 0 || scanned >= maxDriftFilesScanned {
			if err == nil && (pending == 0 || scanned >= maxDriftFilesScanned) {
				return filepath.SkipAll
			}
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxDriftFileBytes {
			return nil
		}
		scanned++
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		content := string(data)
		for i, needle := range needles {
			if !found[want[i]] && strings.Contains(content, needle) {
				found[want[i]] = true
				pending--
				if pending == 0 {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	return found
}

// formatMissingRefs renders missing references for a log line.
func formatMissingRefs(missing []codeReference) string {
	parts := make([]string, len(missing))
	for i, r := range missing {
		parts[i] = r.String()
	}
	return strings.Join(parts, ", ")
}
