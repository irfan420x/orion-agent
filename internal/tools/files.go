// Package tools — file tools: file_read, file_write, file_edit, file_list.
//
// All paths are confined to an allowlist (the process working directory
// and the OS temp directory) and sensitive directories ($HOME/.orion,
// $HOME/.ssh, $HOME/.gnupg) are never reachable, even when they sit
// under an allowed root. Symlinks are resolved before the check, so a
// symlink cannot smuggle a path out of bounds — and files are opened
// with O_NOFOLLOW and verified against the opened descriptor, so a
// symlink swapped in between the check and the open cannot redirect
// the access either (P2-O7); symlinked files are refused outright.
// Reads are capped at 1MB;
// writes are capped at 1MB and atomic; every failure is a typed
// ToolError. Nothing here touches the shell.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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

// errHardlinkRefused is returned by readFileBounded for a regular
// file with more than one hard link (F-A5-P2-30): a hard link inside
// an allowed root can point at a sensitive file outside it, and the
// read path cannot distinguish the two names for one inode —
// device/inode checks only prove the file is the one at the path,
// not that no other name reaches it. Refusing nlink>1 is the
// least-breaking safe option: ordinary files have nlink=1, and a
// hardlinked file is refused with a clear error instead of read.
// Callers that must distinguish it from other read errors use
// errors.Is.
var errHardlinkRefused = errors.New("file has more than one hard link; refusing to read (a hard link inside the allowed directories could point at a file outside them)")

// readFileBounded safely opens path and reads up to maxFileRead+1
// bytes from it. path is the caller's absolute path BEFORE symlink
// resolution; confined is the confinePath result for the same path.
// The open uses path (O_NOFOLLOW), never confined: opening the
// resolved path would follow a symlink that confinePath itself just
// resolved, so a symlink swapped in before the check would be
// opened as its target. Opening path refuses a final-component
// symlink outright, and verifyFdPath then confirms the opened
// file's true path is exactly confined, so a parent directory
// swapped after confinePath cannot redirect the read into a
// sensitive directory either. The returned FileInfo is fstat of
// the opened file, so the caller's size checks describe the file
// actually read, not the path as it was before the open.
//
// It returns promptly when ctx is cancelled or after
// maxFileReadTimeout, instead of blocking forever on a fifo with no
// writer. Reading one byte past the cap is enough to know the file
// is over the cap; the caller truncates and marks it. A read that
// is abandoned leaves its goroutine blocked on the file until a
// writer appears (or the process exits) — one leaked goroutine per
// abandoned read, bounded by the number of such reads, which is
// far better than hanging the tool call.
func readFileBounded(ctx context.Context, path, confined string) ([]byte, os.FileInfo, error) {
	type result struct {
		data []byte
		info os.FileInfo
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := openNoFollow(path)
		if err != nil {
			ch <- result{nil, nil, err}
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			ch <- result{nil, nil, err}
			return
		}
		if info.IsDir() {
			ch <- result{nil, nil, fmt.Errorf("path %q is a directory, not a file", path)}
			return
		}
		// A hardlinked regular file is refused outright, before
		// any byte is read (F-A5-P2-30; see errHardlinkRefused).
		// Directories and special files are unaffected: only
		// regular files can be a hard link to outside content.
		if info.Mode().IsRegular() {
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
				ch <- result{nil, nil, errHardlinkRefused}
				return
			}
		}
		if err := verifyFdPath(f, confined); err != nil {
			ch <- result{nil, nil, err}
			return
		}
		data, err := io.ReadAll(io.LimitReader(f, maxFileRead+1))
		ch <- result{data, info, err}
	}()
	timer := time.NewTimer(maxFileReadTimeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.data, r.info, r.err
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-timer.C:
		return nil, nil, fmt.Errorf("timed out reading %q", path)
	}
}

// openNoFollow opens path for reading without following a
// final-component symlink (O_NOFOLLOW). A symlink is refused with a
// clear error instead of followed: the check (confinePath) and the
// open must never be separable by a swap in between.
func openNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("path %q is a symlink; symlinked files are refused", path)
		}
		return nil, err
	}
	return f, nil
}

// verifyFdPath confirms the already-opened file f is the file at
// confined: its true on-disk path must be exactly the confined
// path. On Linux the true path comes from /proc/self/fd, which
// resolves through every symlink — including a parent directory
// swapped after confinePath ran — so a swapped parent is caught
// here even though the open itself succeeded. Where /proc is
// unavailable (e.g. macOS) it falls back to comparing the opened
// file's device+inode with a fresh Lstat of the path: a swap
// between open and Lstat that swaps the inode back is not caught
// by the fallback, which is documented here rather than assumed
// away. A file deleted between open and the check is refused
// (fail-closed).
func verifyFdPath(f *os.File, confined string) error {
	if resolved, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd())); err == nil {
		resolved = strings.TrimSuffix(resolved, " (deleted)")
		if filepath.Clean(resolved) != confined {
			return fmt.Errorf("path %q changed during open (now %q); refusing", confined, resolved)
		}
		return nil
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	linfo, err := os.Lstat(confined)
	if err != nil {
		return fmt.Errorf("cannot verify %q after open: %w", confined, err)
	}
	if !sameFile(info, linfo) {
		return fmt.Errorf("path %q changed during open; refusing", confined)
	}
	return nil
}

// sameFile reports whether two FileInfos describe the same file
// (same device and inode). It returns false when the platform does
// not expose device/inode data, so callers fail closed.
func sameFile(a, b os.FileInfo) bool {
	sa, okA := a.Sys().(*syscall.Stat_t)
	sb, okB := b.Sys().(*syscall.Stat_t)
	if !okA || !okB {
		return false
	}
	return sa.Dev == sb.Dev && sa.Ino == sb.Ino
}

// checkWriteTarget refuses a write target that is an existing
// symlink or directory, by Lstat (which never follows the final
// component). A symlink target must never be written through: the
// atomic rename below would replace the link itself, but refusing
// outright is clearer and closes the swap window before any write
// happens. A non-existent path is fine.
func checkWriteTarget(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("path %q is a symlink; symlinked files are refused", path)
	}
	if info.IsDir() {
		return fmt.Errorf("path %q is a directory", path)
	}
	return nil
}

// verifyParentDir re-checks the confinement of dir's longest existing
// ancestor immediately before creating directories or a temp file
// under it: confinePath checked the full path earlier, but a parent
// swapped since then would otherwise let MkdirAll / CreateTemp
// create files under a sensitive directory. It resolves symlinks
// the same way confinePath does, and applies the same roots and
// sensitive-directory rules.
func verifyParentDir(dir string, roots []string) error {
	resolved, err := resolveSymlinks(dir)
	if err != nil {
		return err
	}
	for _, d := range sensitiveDirs() {
		if within(resolved, d) {
			return fmt.Errorf("path %q is in a protected directory", dir)
		}
	}
	if len(roots) == 0 {
		roots = defaultFileRoots()
	}
	for _, r := range roots {
		if within(resolved, r) {
			return nil
		}
	}
	return fmt.Errorf("path %q is outside the allowed directories (working directory and temp directory)", dir)
}

// verifyWrittenFile re-opens a just-written file (O_NOFOLLOW, on
// the caller's unresolved path) and confirms its true path is the
// confined path, so a parent swapped during the write is caught
// and the misplaced file can be removed instead of left in the
// wrong directory.
func verifyWrittenFile(path, confined string) error {
	f, err := openNoFollow(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return verifyFdPath(f, confined)
}

// absClean returns the absolute, cleaned form of a user-supplied
// path — the form the filesystem operations use, BEFORE symlink
// resolution. confinePath resolves symlinks on top of this; the
// two forms are kept separate so opens can use the unresolved path
// (O_NOFOLLOW) while verification compares against the resolved
// one. See readFileBounded.
func absClean(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("cannot resolve path %q: %w", p, err)
	}
	return filepath.Clean(abs), nil
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
	// fsync the directory so the rename itself is durable across
	// OS/power loss, not just a process crash (F-A5-P2-36): the
	// file fsync above only orders the file's own bytes, and
	// without a directory fsync a power cut can roll the rename
	// back. Best-effort on purpose: the rename already committed,
	// so a directory-sync failure must not misreport the write
	// itself as failed.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
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
	return "Read a text file (up to 1MB) and return its contents. Larger files are truncated to 1MB with a marker stating the true size. Paths are confined to the working directory and the temp directory. Symlinked files are refused."
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
	// orig is the unresolved absolute path: the open below uses it,
	// never the resolved path (see readFileBounded).
	orig, err := absClean(a.Path)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
	}
	if err := ctx.Err(); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: err}
	}
	// The Stat and the read both describe the OPENED file now
	// (readFileBounded opens with O_NOFOLLOW and verifies the fd):
	// a symlink swapped in between confinePath and the open is
	// refused instead of followed, and the size below is the size
	// of the file actually read.
	data, info, err := readFileBounded(ctx, orig, path)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot read %q: %w", path, err)}
	}
	if info.IsDir() {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("path %q is a directory, not a file", path)}
	}
	// No size refusal here: a file over the read cap is truncated
	// with a marker (below), not an error — the model gets the
	// first megabyte and knows the rest was cut, instead of getting
	// nothing. readFileBounded reads at most maxFileRead+1 bytes,
	// so memory stays bounded whatever the file size.
	if len(data) > maxFileRead {
		// The file grew between fstat and read: truncate and say so
		// instead of silently returning a partial file. Back off to a
		// UTF-8 boundary so the cut never splits a multi-byte rune.
		// When fstat knew the true size (a regular file over the cap),
		// report it; for a FIFO the size is unknown (0 at fstat) and
		// the plain marker is used, as the grow-path tests expect.
		cut := data[:maxFileRead]
		for len(cut) > 0 && !utf8.Valid(cut) {
			cut = cut[:len(cut)-1]
		}
		if info.Size() > maxFileRead {
			return string(cut) + fmt.Sprintf("\n[truncated to 1MB] (file is %d bytes)", info.Size()), nil
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
	return "Write content to a file (up to 1MB), creating parent directories as needed. Overwrites an existing file; refuses paths that are directories. Paths are confined to the working directory and the temp directory. Symlinked files are refused."
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
	// orig is the unresolved absolute path: every filesystem
	// operation below uses it, never the resolved path, so a
	// symlink swapped in after confinePath is met by O_NOFOLLOW /
	// Lstat on the real path, not resolved away first.
	orig, err := absClean(a.Path)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
	}
	if len(a.Content) > maxFileWrite {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("content is %d bytes, over the 1MB write limit", len(a.Content))}
	}
	if err := ctx.Err(); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: err}
	}
	// Refuse an existing symlink or directory before writing: the
	// rename below would replace a symlink itself, but refusing
	// outright closes the swap window before any write happens.
	if err := checkWriteTarget(orig); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
	}
	if dir := filepath.Dir(orig); dir != "" && dir != "." {
		// Re-check the parent's confinement immediately before
		// creating anything under it (see verifyParentDir): a
		// parent swapped after confinePath must not receive the
		// new directories or the temp file.
		if err := verifyParentDir(dir, t.roots); err != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot create parent directory %q: %w", dir, err)}
		}
	}
	// Atomic write, 0600: agent-written files are not world-readable.
	if err := atomicWriteFile(orig, []byte(a.Content), 0o600); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot write %q: %w", path, err)}
	}
	// Post-write verification: re-open the result (O_NOFOLLOW) and
	// confirm it landed at the confined path. A parent swapped
	// during the write is caught here; the misplaced file is
	// removed rather than left in the wrong directory.
	if err := verifyWrittenFile(orig, path); err != nil {
		_ = os.Remove(orig)
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot verify %q after write: %w", path, err)}
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), path), nil
}

// ---------------------------------------------------------------- file_edit

type fileEditTool struct {
	roots []string
}

func (t *fileEditTool) Name() string { return "file_edit" }

func (t *fileEditTool) Description() string {
	return "Replace an exact text match in a file. Fails if old_text is absent or matches more than once. Paths are confined to the working directory and the temp directory. Symlinked files are refused."
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
	// orig is the unresolved absolute path: every filesystem
	// operation below uses it, never the resolved path (see
	// readFileBounded).
	orig, err := absClean(a.Path)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
	}
	if a.OldText == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("old_text must not be empty")}
	}
	if err := ctx.Err(); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: err}
	}
	// Fail fast on symlinks and non-regular files (fifo/socket/
	// device): the safe open below would block on a writer-less
	// fifo until the read timeout. This is only a fast path — the
	// read itself still goes through the O_NOFOLLOW open + fd
	// verification, so a swap between this check and the open
	// cannot redirect the read; and a missing file (Lstat error)
	// falls through to the read, which reports it.
	if linfo, lerr := os.Lstat(orig); lerr == nil {
		if linfo.Mode()&os.ModeSymlink != 0 {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("path %q is a symlink; symlinked files are refused", path)}
		}
		if linfo.IsDir() {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("path %q is a directory, not a file", path)}
		}
		if !linfo.Mode().IsRegular() {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("path %q is not a regular file", path)}
		}
	}
	// The read describes the OPENED file (O_NOFOLLOW + fd-path
	// verification inside readFileBounded): a symlink swapped in
	// between confinePath and the open is refused, and the mode
	// below is the opened file's mode, not the path's.
	data, info, err := readFileBounded(ctx, orig, path)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot read %q: %w", path, err)}
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
	// Re-check the target first: the file may have been swapped
	// for a symlink since the read (the rename would replace the
	// link itself, but refusing outright is clearer).
	if err := checkWriteTarget(orig); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
	}
	if err := atomicWriteFile(orig, []byte(updated), info.Mode().Perm()); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot write %q: %w", path, err)}
	}
	// Post-write verification, as in file_write: a parent swapped
	// during the write is caught here and the misplaced file is
	// removed. (The original content is already replaced at this
	// point when the check fails — the same failure mode as a
	// crash mid-rename — but nothing is left in the wrong place.)
	if err := verifyWrittenFile(orig, path); err != nil {
		_ = os.Remove(orig)
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot verify %q after write: %w", path, err)}
	}
	return fmt.Sprintf("edited %s: replaced 1 occurrence (%d bytes -> %d bytes)", path, len(content), len(updated)), nil
}

// ---------------------------------------------------------------- file_list

type fileListTool struct {
	roots []string
}

func (t *fileListTool) Name() string { return "file_list" }

func (t *fileListTool) Description() string {
	return "List the entries of a directory, sorted by name, with type and size. Paths are confined to the working directory and the temp directory. Symlinked directories are refused."
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
	// orig is the unresolved absolute path: the open below uses it,
	// never the resolved path, so a symlink swapped in after
	// confinePath is met by O_NOFOLLOW on the real path.
	orig, err := absClean(a.Path)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
	}
	if err := ctx.Err(); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: err}
	}
	// Open without following a final-component symlink (P2-O7):
	// a symlinked directory swapped in after confinePath is
	// refused instead of listed, and the fd-path check below
	// catches a parent swapped since the check.
	f, err := os.OpenFile(orig, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("path %q is a symlink; symlinked directories are refused", path)}
		}
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot list %q: %w", path, err)}
	}
	if info, err := f.Stat(); err != nil {
		_ = f.Close()
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot list %q: %w", path, err)}
	} else if !info.IsDir() {
		_ = f.Close()
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("path %q is not a directory", path)}
	}
	if err := verifyFdPath(f, path); err != nil {
		_ = f.Close()
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
