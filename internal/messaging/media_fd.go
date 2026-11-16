package messaging

// Upload-time re-verification of a media file (F-A23W2-12).
//
// validateMediaPath validates a path string; callMultipart used to
// os.Open() that same string later, with no fd, no O_NOFOLLOW and
// no re-verification — a file swapped between validation and open
// (unlink + symlink, or a hard link added) was uploaded unchecked.
// The file tools close exactly this window (files.go: openNoFollow
// + verifyFdPath via /proc/self/fd); media does the same here:
//
//   - the file is opened with O_NOFOLLOW, so a final-component
//     symlink swapped in after validation is refused outright
//     instead of followed;
//   - the opened descriptor is then verified against the path it
//     was opened from: its true on-disk path (from /proc/self/fd,
//     falling back to a device+inode comparison) must be exactly
//     the path validation returned, so a swapped parent directory
//     is caught too;
//   - the denylist, the hard-link rule and the size cap are all
//     re-checked on the opened file itself, so a hard link added
//     or a file grown between validation and open is refused
//     before a single byte is read.
//
// This makes callMultipart self-defending: it re-verifies even when
// called directly through the exported Send* methods, which never
// go through validateMediaPath.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/irfan/orion/internal/config"
)

// mediaUploadMaxBytes clamps a configured per-file cap the same way
// validateMediaPath does: unset or above the 50 MiB Bot API limit
// means the default 50 MiB.
func mediaUploadMaxBytes(maxBytes int64) int64 {
	if maxBytes <= 0 || maxBytes > config.DefaultMediaMaxBytes {
		return config.DefaultMediaMaxBytes
	}
	return maxBytes
}

// openVerifiedMediaFile opens path for upload and re-verifies the
// opened file before returning it: O_NOFOLLOW open, regular-file
// and hard-link checks, fd-path verification against path, and the
// denylist + size re-checks on the opened file. The caller must
// pass the resolved path validation returned (or, for a direct
// Send* call, the path to send); the returned file is the
// descriptor to stream from, and must be closed by the caller.
func openVerifiedMediaFile(path string, maxBytes int64) (*os.File, error) {
	maxBytes = mediaUploadMaxBytes(maxBytes)
	expected := filepath.Clean(path)
	if abs, err := filepath.Abs(expected); err == nil {
		expected = abs
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if strings.Contains(err.Error(), "too many levels of symbolic links") {
			return nil, fmt.Errorf("path %q is a symlink; symlinked files are refused at upload", path)
		}
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.IsDir() {
		f.Close()
		return nil, fmt.Errorf("path %q is a directory, not a file", path)
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("path %q is not a regular file", path)
	}
	// A hard link added between validation and open must not
	// smuggle a denied file past the check (F-A23W2-6).
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		f.Close()
		return nil, fmt.Errorf("file has more than one hard link; refusing to send")
	}
	// The file actually opened must be the file at the validated
	// path: a parent directory swapped after validation redirects
	// the open, and the fd path exposes it.
	fdPath, err := mediaFdResolvedPath(f, expected)
	if err != nil {
		f.Close()
		return nil, err
	}
	if filepath.Clean(fdPath) != expected {
		f.Close()
		return nil, fmt.Errorf("path %q changed during upload (now %q); refusing", path, fdPath)
	}
	// Re-check the denylist and the size cap on the opened file
	// itself, not on the path string as it was at validate time.
	if mediaPathDenied(fdPath) {
		f.Close()
		return nil, fmt.Errorf("that path is never sendable")
	}
	if info.Size() > maxBytes {
		f.Close()
		return nil, fmt.Errorf("file is too large (%s; max %s)", formatMediaSize(info.Size()), formatMediaSize(maxBytes))
	}
	return f, nil
}

// mediaFdResolvedPath returns the true on-disk path of the opened
// file f, from /proc/self/fd on Linux; expected is the path f was
// opened from, used only by the fallback. Where /proc is unavailable
// it falls back to comparing the opened file's device+inode with a
// fresh Lstat of expected — a swap between open and Lstat that swaps
// the inode back is not caught by the fallback, which is documented
// here rather than assumed away (same fallback as verifyFdPath in
// internal/tools/files.go). A file deleted between open and the check
// is refused (fail-closed).
func mediaFdResolvedPath(f *os.File, expected string) (string, error) {
	if resolved, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd())); err == nil {
		return strings.TrimSuffix(resolved, " (deleted)"), nil
	}
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	linfo, err := os.Lstat(expected)
	if err != nil {
		return "", fmt.Errorf("cannot verify %q after open: %w", expected, err)
	}
	if !mediaSameFile(info, linfo) {
		return "", fmt.Errorf("path %q changed during upload; refusing", expected)
	}
	return expected, nil
}

// mediaSameFile reports whether two FileInfos describe the same file
// (same device and inode). It returns false when the platform does
// not expose device/inode data, so callers fail closed.
func mediaSameFile(a, b os.FileInfo) bool {
	sa, okA := a.Sys().(*syscall.Stat_t)
	sb, okB := b.Sys().(*syscall.Stat_t)
	if !okA || !okB {
		return false
	}
	return sa.Dev == sb.Dev && sa.Ino == sb.Ino
}
