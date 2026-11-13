// P2-O7 tests (rc8): the file-tool check-then-open TOCTOU. The
// attack is a symlink swapped in for the target (or for a parent
// directory) between confinePath and the open, redirecting the
// access into a sensitive directory. The fix opens with O_NOFOLLOW
// and verifies the opened descriptor's true path, so the swap is
// refused instead of followed.
package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// p2SecretMarker is content that must never come back from a file
// tool: if any result contains it, the tool followed a swapped
// symlink into the "sensitive" file.
const p2SecretMarker = "P2-TOCTOU-SECRET-DO-NOT-LEAK"

// TestP2FileReadRefusesSymlink: a file that IS a symlink — even one
// pointing at another file inside the allowed roots — is refused,
// not followed.
func TestP2FileReadRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte(p2SecretMarker), 0o600); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	tool := &fileReadTool{roots: []string{dir}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": link}))
	if err == nil {
		t.Fatalf("file_read on a symlink: got %q, want an error (refused, not followed)", out)
	}
	if strings.Contains(out, p2SecretMarker) {
		t.Fatalf("file_read on a symlink leaked the secret")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("file_read on a symlink: error %q, want it to mention symlink", err)
	}
}

// TestP2FileReadRefusesSymlinkOutsideRoots: a symlink pointing
// outside the allowed roots is refused (this was already refused by
// confinePath; the O_NOFOLLOW open must refuse it too, not follow).
func TestP2FileReadRefusesSymlinkOutsideRoots(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte(p2SecretMarker), 0o600); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	tool := &fileReadTool{roots: []string{dir}}
	if out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": link})); err == nil {
		t.Fatalf("file_read on an outside symlink: got %q, want an error", out)
	}
}

// TestP2FileReadSwapRace: while a swapper alternates the target
// between a real file and a symlink to the secret, file_read runs
// repeatedly. No result may ever contain the secret: the old
// check-then-open could return it whenever the swap landed between
// confinePath and os.Open; O_NOFOLLOW + fd verification cannot.
func TestP2FileReadSwapRace(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte(p2SecretMarker), 0o600); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("real content"), 0o600); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	tool := &fileReadTool{roots: []string{dir}}

	stop := make(chan struct{})
	var swapWG sync.WaitGroup
	swapWG.Add(1)
	go func() {
		defer swapWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Swap to the symlink, then back to the real file.
			_ = os.Remove(target)
			_ = os.Symlink(secret, target)
			_ = os.Remove(target)
			_ = os.WriteFile(target, []byte("real content"), 0o600)
		}
	}()

	leaked := 0
	reads := 0
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": target}))
		reads++
		if err == nil {
			if strings.Contains(out, p2SecretMarker) {
				leaked++
			} else if out != "real content" && out != "" {
				// "" is a legitimate race outcome: the swapper
				// re-creates the file with WriteFile, which
				// creates it empty before writing the content,
				// so a read can land in the empty window.
				t.Errorf("file_read returned unexpected content %q", out)
			}
		}
		// Errors are expected (the path is a symlink half the
		// time, or briefly missing); only a leak fails.
	}
	close(stop)
	swapWG.Wait()
	if reads == 0 {
		t.Fatal("no reads ran")
	}
	if leaked > 0 {
		t.Fatalf("file_read leaked the secret in %d of %d reads (TOCTOU not closed)", leaked, reads)
	}
	t.Logf("swap race: %d reads, 0 leaks", reads)
}

// TestP2FileWriteRefusesSymlink: writing through a symlink must be
// refused, and the link's target must be untouched.
func TestP2FileWriteRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte(p2SecretMarker), 0o600); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	tool := &fileWriteTool{roots: []string{dir}}
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": link, "content": "pwned"})); err == nil {
		t.Fatal("file_write through a symlink: got nil, want an error")
	}
	after, err := os.ReadFile(secret)
	if err != nil {
		t.Fatalf("ReadFile secret: %v", err)
	}
	if string(after) != p2SecretMarker {
		t.Fatalf("secret after refused write = %q, want it untouched", after)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link after refused write is no longer a symlink (err=%v)", err)
	}
}

// TestP2FileWriteSwapRace: while a swapper alternates the target
// between a real file and a symlink to the secret, file_write runs
// repeatedly. The secret must never gain the written content.
func TestP2FileWriteSwapRace(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte(p2SecretMarker), 0o600); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("real content"), 0o600); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	tool := &fileWriteTool{roots: []string{dir}}

	stop := make(chan struct{})
	var swapWG sync.WaitGroup
	swapWG.Add(1)
	go func() {
		defer swapWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Remove(target)
			_ = os.Symlink(secret, target)
			_ = os.Remove(target)
			_ = os.WriteFile(target, []byte("real content"), 0o600)
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, _ = tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": target, "content": "attacker-content"}))
	}
	close(stop)
	swapWG.Wait()
	after, err := os.ReadFile(secret)
	if err != nil {
		t.Fatalf("ReadFile secret: %v", err)
	}
	if string(after) != p2SecretMarker {
		t.Fatalf("secret after write swap race = %q, want it untouched", after)
	}
}

// TestP2FileEditRefusesSymlink: editing through a symlink must be
// refused, and the link's target must be untouched.
func TestP2FileEditRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("hello "+p2SecretMarker), 0o600); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	tool := &fileEditTool{roots: []string{dir}}
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": link, "old_text": "hello", "new_text": "pwned"})); err == nil {
		t.Fatal("file_edit through a symlink: got nil, want an error")
	}
	after, err := os.ReadFile(secret)
	if err != nil {
		t.Fatalf("ReadFile secret: %v", err)
	}
	if strings.Contains(string(after), "pwned") {
		t.Fatalf("secret after refused edit = %q, want it untouched", after)
	}
}

// TestP2FileListRefusesSymlinkedDir: a symlinked directory is
// refused, not listed.
func TestP2FileListRefusesSymlinkedDir(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(dir, "linkdir")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	tool := &fileListTool{roots: []string{dir}}
	if out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": link})); err == nil {
		t.Fatalf("file_list on a symlinked dir: got %q, want an error", out)
	}
	// The real directory itself still lists fine.
	if out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": real})); err != nil {
		t.Fatalf("file_list on the real dir: %v", err)
	} else if !strings.Contains(out, "a.txt") {
		t.Errorf("file_list on the real dir = %q, want a.txt", out)
	}
}

// TestP2SymlinkedParentStillWorks: a symlinked PARENT directory is
// fine — confinePath resolves it, so the confined path has no
// symlink components and the O_NOFOLLOW open succeeds. Only the
// final component is refused when it is a symlink.
func TestP2SymlinkedParentStillWorks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "f.txt"), []byte("via parent link"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	parentLink := filepath.Join(dir, "parentlink")
	if err := os.Symlink(real, parentLink); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	tool := &fileReadTool{roots: []string{dir}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": filepath.Join(parentLink, "f.txt")}))
	if err != nil {
		t.Fatalf("file_read via symlinked parent: %v", err)
	}
	if out != "via parent link" {
		t.Errorf("file_read via symlinked parent = %q, want %q", out, "via parent link")
	}
}
