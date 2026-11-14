package messaging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// F-A17-4 (rc23, W2): the allow-all opt-in window reopened on every
// restart/reboot, because its start time lived only in memory
// (WithAllowAll set allowAllSince = time.Now()). WithAllowAllPersisted
// records the window deadline in <dataDir>/allow-all-until: a restart
// resumes the recorded deadline instead of opening a fresh window.

// TestWithAllowAllPersistedFresh: the first start with the opt-in
// opens a window and writes its deadline to the data dir (0600,
// RFC3339Nano, one line), and every chat is allowed inside it.
func TestWithAllowAllPersistedFresh(t *testing.T) {
	dir := t.TempDir()
	before := time.Now()
	g, err := NewTelegramGateway(testToken, WithAllowAllPersisted(10*time.Minute, dir))
	if err != nil {
		t.Fatal(err)
	}
	if !g.IsChatAllowed(12345) {
		t.Fatal("fresh persisted window: chat refused, want allowed")
	}
	path := filepath.Join(dir, AllowAllStateFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file %q not written: %v", path, err)
	}
	deadline, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("state file %q is not RFC3339Nano %q: %v", path, raw, err)
	}
	// The deadline is ~10 minutes after the gateway was built: it
	// must be after (before + 10m - 5s) and no later than now + 10m.
	if deadline.Before(before.Add(10*time.Minute-5*time.Second)) || deadline.After(time.Now().Add(10*time.Minute)) {
		t.Errorf("deadline = %s, want ~10m after build (before=%s)", deadline, before)
	}
	if fi, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %o, want 0600", fi.Mode().Perm())
	}
}

// TestWithAllowAllPersistedResume: a restart inside the window
// resumes the RECORDED deadline — it does not extend it. A second
// gateway built over the same data dir sees the same deadline, and
// the file is left byte-identical.
func TestWithAllowAllPersistedResume(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AllowAllStateFileName)
	wantDeadline := time.Now().Add(5 * time.Minute)
	if err := os.WriteFile(path, []byte(wantDeadline.Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := NewTelegramGateway(testToken, WithAllowAllPersisted(10*time.Minute, dir))
	if err != nil {
		t.Fatal(err)
	}
	if !g.IsChatAllowed(12345) {
		t.Fatal("resumed window (deadline 5m out): chat refused, want allowed")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != wantDeadline.Format(time.RFC3339Nano) {
		t.Errorf("resume rewrote the state file: %q, want unchanged %q", got, wantDeadline.Format(time.RFC3339Nano))
	}
}

// TestWithAllowAllPersistedExpiredStaysShut: the F-A17-4 bug itself —
// once the recorded deadline has passed, a restart must NOT reopen
// the window, and the file must NOT be rewritten with a fresh
// deadline. The bot stays shut until telegram_allowed_chats is set.
func TestWithAllowAllPersistedExpiredStaysShut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AllowAllStateFileName)
	deadline := time.Now().Add(-time.Minute)
	if err := os.WriteFile(path, []byte(deadline.Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := NewTelegramGateway(testToken, WithAllowAllPersisted(10*time.Minute, dir))
	if err != nil {
		t.Fatal(err)
	}
	if g.IsChatAllowed(12345) {
		t.Fatal("expired persisted window: chat allowed, want refused (restart must not reopen it)")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != deadline.Format(time.RFC3339Nano) {
		t.Errorf("expired window rewrote the state file: %q, want unchanged %q", got, deadline.Format(time.RFC3339Nano))
	}
	// A second restart is shut too — the window never reopens.
	g2, err := NewTelegramGateway(testToken, WithAllowAllPersisted(10*time.Minute, dir))
	if err != nil {
		t.Fatal(err)
	}
	if g2.IsChatAllowed(999) {
		t.Fatal("expired persisted window, second restart: chat allowed, want refused")
	}
}

// TestWithAllowAllPersistedCorruptFailsClosed: an unreadable state
// file must not silently open a fresh window — the gateway refuses
// every chat and logs the file path, so the operator can delete it
// to start over.
func TestWithAllowAllPersistedCorruptFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AllowAllStateFileName)
	if err := os.WriteFile(path, []byte("not-a-time\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs []string
	g, err := NewTelegramGateway(testToken,
		WithLogger(func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		}),
		WithAllowAllPersisted(10*time.Minute, dir))
	if err != nil {
		t.Fatal(err)
	}
	if g.IsChatAllowed(12345) {
		t.Fatal("corrupt state file: chat allowed, want refused (fail closed)")
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, path) || !strings.Contains(joined, "unreadable") {
		t.Errorf("corrupt state: logs %q do not name the file and the problem", joined)
	}
}

// TestWithAllowAllPersistedNoDataDir: with no data dir the option
// behaves exactly like WithAllowAll — an in-memory window only, no
// file anywhere, no failure.
func TestWithAllowAllPersistedNoDataDir(t *testing.T) {
	g, err := NewTelegramGateway(testToken, WithAllowAllPersisted(10*time.Minute, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !g.IsChatAllowed(12345) {
		t.Fatal("no data dir: chat refused, want allowed (in-memory window)")
	}
	if g.allowAllStatePath != "" {
		t.Errorf("allowAllStatePath = %q, want empty with no data dir", g.allowAllStatePath)
	}
}
