// Package tools — file tools: file_read, file_write, file_edit, file_list.
//
// All paths are confined to an allowlist (the process working directory
// and the OS temp directory) and sensitive directories ($HOME/.orion,
// $HOME/.ssh, $HOME/.gnupg) are never reachable, even when they sit
// under an allowed root. Symlinks are resolved before the check, so a
// symlink cannot smuggle a path out of bounds. Reads are capped at 1MB;
// writes are capped at 1MB and atomic; every failure is a typed
// ToolError. Nothing here touches the shell.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/irfan/orion/internal/model"
)

const (
	maxFileRead    = 1 * 1024 * 1024 // 1MB
	maxFileWrite   = 1 * 1024 * 1024 // 1MB
	maxListEntries = 200
	// maxListRead caps how many directory entries file_list reads
	// into memory. A directory with millions of entries must not
	// exhaust memory; the total is counted exactly up to this cap
	// and reported as "more than N" beyond it.
	maxListRead = 10000
	// maxFileReadTimeout bounds how long a file read may block when
	// the caller's context has no deadline. A fifo with no writer
	// blocks forever on read, and os.ReadFile does not watch the
	// context, so a read is abandoned after this long.
	maxFileReadTimeout = 30 * time.Second
)

// readFileBounded reads up to maxFileRead+1 bytes from path. It
// returns promptly when ctx is cancelled or after
// maxFileReadTimeout, instead of blocking forever on a fifo with no
// writer. Reading one byte past the cap is enough to know the file
// is over the cap; the caller truncates and marks it. A read that
// is abandoned leaves its goroutine blocked on the file until a
// writer appears (or the process exits) — one leaked goroutine per
// abandoned read, bounded by the number of such reads, which is
// far better than hanging the tool call.
func readFileBounded(ctx context.Context, path string) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := os.Open(path)
		if err != nil {
			ch <- result{nil, err}
			return
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, maxFileRead+1))
		ch <- result{data, err}
	}()
	timer := time.NewTimer(maxFileReadTimeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.data, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("timed out reading %q", path)
	}
}

// defaultFileRoots are the directories file tools may touch: the process
// working directory and the OS temp directory. Everything else — /etc,
// $HOME, other users' files — is out of bounds.
func defaultFileRoots() []string {
	roots := []string{os.TempDir()}
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd)
	}
	return roots
}

// sensitiveDirs are never reachable through file tools, even when they
// sit under an allowed root (e.g. cwd == $HOME): the agent's own config
// (Telegram token, API keys), SSH keys, and GPG keys.
func sensitiveDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".orion"),
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".gnupg"),
	}
}

// within reports whether path is dir itself or inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

// resolveSymlinks resolves symlinks in abs, following the longest
// existing ancestor for paths that do not exist yet (e.g. a file to be
// created under a symlinked directory).
func resolveSymlinks(abs string) (string, error) {
	cur := abs
	var suffix []string
	for {
		if _, err := os.Lstat(cur); err == nil {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", fmt.Errorf("cannot resolve %q: %w", abs, err)
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}

// confinePath validates a user-supplied path: it must resolve (after
// symlink resolution) inside one of roots — the working directory and
// the temp directory by default — and must not be inside a sensitive
// directory. It returns the resolved absolute path.
func confinePath(p string, roots []string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("cannot resolve path %q: %w", p, err)
	}
	abs = filepath.Clean(abs)
	resolved, err := resolveSymlinks(abs)
	if err != nil {
		return "", err
	}
	for _, d := range sensitiveDirs() {
		if within(resolved, d) {
			return "", fmt.Errorf("path %q is in a protected directory", p)
		}
	}
	if len(roots) == 0 {
		roots = defaultFileRoots()
	}
	for _, r := range roots {
		if within(resolved, r) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("path %q is outside the allowed directories (working directory and temp directory)", p)
}

// atomicWriteFile writes data to path via a temp file + rename, so a
// crash mid-write never leaves a torn file. The temp file is created
// in the same directory (same filesystem) with the given permissions.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".orion-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Chmod(perm); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// decodeArgs unmarshals tool arguments, returning an invalid_args ToolError.
func decodeArgs(tool string, args json.RawMessage, dst any) error {
	if len(args) == 0 {
		return &ToolError{Tool: tool, Kind: "invalid_args", Err: fmt.Errorf("missing arguments")}
	}
	if err := json.Unmarshal(args, dst); err != nil {
		return &ToolError{Tool: tool, Kind: "invalid_args", Err: fmt.Errorf("invalid JSON arguments: %w", err)}
	}
	return nil
}

// ---------------------------------------------------------------- file_read

type fileReadTool struct {
	// roots confines the tool; nil means the default roots (working
	// directory and temp directory).
	roots []string
}

func (t *fileReadTool) Name() string { return "file_read" }

func (t *fileReadTool) Description() string {
	return "Read a text file (up to 1MB) and return its contents. Paths are confined to the working directory and the temp directory."
}

func (t *fileReadTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Path of the file to read."},
			},
			"required": []string{"path"},
		},
	}
}

type filePathArgs struct {
	Path string `json:"path"`
}

func (t *fileReadTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a filePathArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	path, err := confinePath(a.Path, t.roots)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
	}
	if err := ctx.Err(); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: err}
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot stat %q: %w", path, err)}
	}
	if info.IsDir() {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("path %q is a directory, not a file", path)}
	}
	if info.Size() > maxFileRead {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("file %q is %d bytes, over the 1MB read limit", path, info.Size())}
	}
	data, err := readFileBounded(ctx, path)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot read %q: %w", path, err)}
	}
	if len(data) > maxFileRead {
		// The file grew between Stat and ReadFile: truncate and say so
		// instead of silently returning a partial file. Back off to a
		// UTF-8 boundary so the cut never splits a multi-byte rune.
		cut := data[:maxFileRead]
		for len(cut) > 0 && !utf8.Valid(cut) {
			cut = cut[:len(cut)-1]
		}
		return string(cut) + "\n[truncated to 1MB]", nil
	}
	return string(data), nil
}

// ---------------------------------------------------------------- file_write

type fileWriteTool struct {
	roots []string
}

func (t *fileWriteTool) Name() string { return "file_write" }

func (t *fileWriteTool) Description() string {
	return "Write content to a file (up to 1MB), creating parent directories as needed. Overwrites an existing file; refuses paths that are directories. Paths are confined to the working directory and the temp directory."
}

func (t *fileWriteTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "Path of the file to write."},
				"content": map[string]any{"type": "string", "description": "Content to write."},
			},
			"required": []string{"path", "content"},
		},
	}
}

type fileWriteArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *fileWriteTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a fileWriteArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	path, err := confinePath(a.Path, t.roots)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
	}
	if len(a.Content) > maxFileWrite {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("content is %d bytes, over the 1MB write limit", len(a.Content))}
	}
	if err := ctx.Err(); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: err}
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("path %q is a directory", path)}
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot create parent directory %q: %w", dir, err)}
		}
	}
	// Atomic write, 0600: agent-written files are not world-readable.
	if err := atomicWriteFile(path, []byte(a.Content), 0o600); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot write %q: %w", path, err)}
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), path), nil
}

// ---------------------------------------------------------------- file_edit

type fileEditTool struct {
	roots []string
}

func (t *fileEditTool) Name() string { return "file_edit" }

func (t *fileEditTool) Description() string {
	return "Replace an exact text match in a file. Fails if old_text is absent or matches more than once. Paths are confined to the working directory and the temp directory."
}

func (t *fileEditTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":     map[string]any{"type": "string", "description": "Path of the file to edit."},
				"old_text": map[string]any{"type": "string", "description": "Exact text to replace (must occur exactly once)."},
				"new_text": map[string]any{"type": "string", "description": "Replacement text."},
			},
			"required": []string{"path", "old_text", "new_text"},
		},
	}
}

type fileEditArgs struct {
	Path    string `json:"path"`
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

func (t *fileEditTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a fileEditArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	path, err := confinePath(a.Path, t.roots)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
	}
	if a.OldText == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("old_text must not be empty")}
	}
	if err := ctx.Err(); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: err}
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot stat %q: %w", path, err)}
	}
	if info.IsDir() {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("path %q is a directory, not a file", path)}
	}
	if !info.Mode().IsRegular() {
		// A fifo, socket or device would block (or never terminate)
		// on read; only regular files are editable here.
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("path %q is not a regular file", path)}
	}
	if info.Size() > maxFileRead {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("file %q is over the 1MB limit", path)}
	}
	data, err := readFileBounded(ctx, path)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot read %q: %w", path, err)}
	}
	content := string(data)
	count := strings.Count(content, a.OldText)
	switch {
	case count == 0:
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("old_text not found in %q", path)}
	case count > 1:
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("old_text matches %d times in %q; it must match exactly once", count, path)}
	}
	updated := strings.Replace(content, a.OldText, a.NewText, 1)
	// The replacement may grow the file past the write cap (the read
	// cap only bounds the original); refuse instead of writing a file
	// larger than file_write would ever produce. The original is left
	// untouched.
	if len(updated) > maxFileWrite {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("edited file would be %d bytes, over the 1MB write limit", len(updated))}
	}
	// Atomic write, preserving the file's existing permissions.
	if err := atomicWriteFile(path, []byte(updated), info.Mode().Perm()); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot write %q: %w", path, err)}
	}
	return fmt.Sprintf("edited %s: replaced 1 occurrence (%d bytes -> %d bytes)", path, len(content), len(updated)), nil
}

// ---------------------------------------------------------------- file_list

type fileListTool struct {
	roots []string
}

func (t *fileListTool) Name() string { return "file_list" }

func (t *fileListTool) Description() string {
	return "List the entries of a directory, sorted by name, with type and size. Paths are confined to the working directory and the temp directory."
}

func (t *fileListTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Path of the directory to list."},
			},
			"required": []string{"path"},
		},
	}
}

func (t *fileListTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a filePathArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	path, err := confinePath(a.Path, t.roots)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
	}
	if err := ctx.Err(); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: err}
	}
	// Read in chunks and stop at maxListRead: a directory with
	// millions of entries must not exhaust memory. The total is
	// counted exactly up to that cap and reported as "more than N"
	// beyond it; the first maxListEntries entries are shown.
	f, err := os.Open(path)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot list %q: %w", path, err)}
	}
	var entries []os.DirEntry
	total := 0
	moreThanCap := false
	for {
		chunk, err := f.ReadDir(512)
		if err != nil && err != io.EOF {
			_ = f.Close()
			return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot list %q: %w", path, err)}
		}
		// ReadDir may return a final partial chunk together with
		// io.EOF; count it before stopping.
		total += len(chunk)
		if total > maxListRead {
			moreThanCap = true
			// Keep only the first maxListRead entries; the rest
			// are counted, not stored.
			entries = append(entries, chunk[:max(0, maxListRead-(total-len(chunk)))]...)
			break
		}
		entries = append(entries, chunk...)
		if err == io.EOF || len(chunk) == 0 {
			break
		}
	}
	if err := f.Close(); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot list %q: %w", path, err)}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	if total == 0 {
		return fmt.Sprintf("%s: (empty directory)", path), nil
	}
	var sb strings.Builder
	sb.WriteString(path + ":\n")
	// Cap the output: a directory with 100k entries must not flood
	// the model context. Report the total so the cap is visible.
	shown := entries
	truncated := false
	if len(entries) > maxListEntries {
		shown = entries[:maxListEntries]
		truncated = true
	}
	for _, e := range shown {
		kind := "file"
		if e.IsDir() {
			kind = "dir"
		}
		size := int64(0)
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		fmt.Fprintf(&sb, "%s\t%s\t%d bytes\n", e.Name(), kind, size)
	}
	if moreThanCap {
		fmt.Fprintf(&sb, "... (more than %d entries, showing first %d)\n", maxListRead, maxListEntries)
	} else if truncated {
		fmt.Fprintf(&sb, "... (%d total entries, showing first %d)\n", total, maxListEntries)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}
