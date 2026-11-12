package security

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// NOTE: every "secret" in this file is an obviously fake test value.
// No real credentials appear anywhere in this package.

// ---------------------------------------------------------------------
// Secret scanner — P2-16 follow-ups.
// ---------------------------------------------------------------------

func TestRedactAssignmentWordBoundary(t *testing.T) {
	// The key must not match inside a longer word.
	for _, in := range []string{
		"mytoken = hunter2secret",
		"mytoken: hunter2secret",
		"tokenized = hunter2secret",
		"my_password = hunter2secret",
	} {
		if got := RedactSecrets(in); got != in {
			t.Errorf("RedactSecrets(%q) = %q, want unchanged (key inside a longer word)", in, got)
		}
		if len(ScanSecrets(in)) != 0 {
			t.Errorf("ScanSecrets(%q) found secrets, want none", in)
		}
	}
	// The bare key still matches.
	for _, in := range []string{
		"token = hunter2secret",
		"token: hunter2secret",
		"api_key=hunter2secret",
		`password="s3cretPassw0rd"`,
	} {
		if got := RedactSecrets(in); !strings.Contains(got, Redacted) {
			t.Errorf("RedactSecrets(%q) = %q, no %q marker", in, got, Redacted)
		}
	}
}

func TestRedactAssignmentShortValues(t *testing.T) {
	// Values of 4+ characters are secrets too.
	for _, in := range []string{
		"password: abcd",
		"password: hunter2",
		"token: s3cr",
		"secret: none",
		"api_key = ab12",
	} {
		if got := RedactSecrets(in); !strings.Contains(got, Redacted) {
			t.Errorf("RedactSecrets(%q) = %q, short value not redacted", in, got)
		}
		if len(ScanSecrets(in)) == 0 {
			t.Errorf("ScanSecrets(%q) found nothing, want a generic finding", in)
		}
	}
	// Values under 4 characters are too short to be a secret and stay.
	for _, in := range []string{
		"password: abc",
		"token: ab",
		"secret: no",
	} {
		if got := RedactSecrets(in); got != in {
			t.Errorf("RedactSecrets(%q) = %q, want unchanged (value too short)", in, got)
		}
	}
}

func TestRedactTelegramTokenLengthRange(t *testing.T) {
	// The tail after the colon is a range, not exactly 35 chars.
	for _, n := range []int{30, 35, 40, 60} {
		tail := strings.Repeat("Ab1", n/3) + strings.Repeat("x", n%3)
		tok := "123456789:" + tail
		if len(tail) != n {
			t.Fatalf("test setup: tail length %d, want %d", len(tail), n)
		}
		in := "bot " + tok
		if got := RedactSecrets(in); strings.Contains(got, tail) {
			t.Errorf("RedactSecrets(%q) = %q, %d-char Telegram tail survived", in, got, n)
		}
		found := false
		for _, f := range ScanSecrets(in) {
			if f.Type == "telegram" {
				found = true
			}
		}
		if !found {
			t.Errorf("ScanSecrets(%q) found no telegram finding", in)
		}
	}
	// A short tail is not a Telegram token.
	in := "bot 123456789:Ab1"
	if got := RedactSecrets(in); got != in {
		t.Errorf("RedactSecrets(%q) = %q, want unchanged (tail too short)", in, got)
	}
}

func TestRedactSlackExtended(t *testing.T) {
	// xoxe (enterprise) tokens are covered, like the other xox* types.
	in := "token xoxe-testfake-1234567890-abcdefghij"
	if got := RedactSecrets(in); !strings.Contains(got, Redacted) {
		t.Errorf("RedactSecrets(%q) = %q, no %q marker", in, got, Redacted)
	}
	// A Slack webhook URL: the service prefix survives, the secret
	// path is redacted.
	hook := "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"
	got := RedactSecrets("post to " + hook)
	if !strings.Contains(got, "https://hooks.slack.com/services/"+Redacted) {
		t.Errorf("RedactSecrets(webhook) = %q, want prefix kept and path redacted", got)
	}
	if strings.Contains(got, "XXXXXXXXXXXXXXXXXXXXXXXX") {
		t.Errorf("RedactSecrets(webhook) = %q, webhook secret survived", got)
	}
	found := false
	for _, f := range ScanSecrets("post to " + hook) {
		if f.Type == "slack" {
			found = true
		}
	}
	if !found {
		t.Error("ScanSecrets(webhook) found no slack finding")
	}
}

func TestRedactGenericHighEntropyToken(t *testing.T) {
	// A bare high-entropy token (mixed case + digits, 32+ chars) is
	// redacted even with no key or scheme around it.
	tok := "aB3dEfGhIjKlMnOpQrStUvWxYz0123456789"
	if len(tok) < 32 {
		t.Fatalf("test setup: token length %d, want >= 32", len(tok))
	}
	if got := RedactSecrets("leaked " + tok + " end"); strings.Contains(got, tok) {
		t.Errorf("RedactSecrets(bare token) = %q, token survived", got)
	}
	found := false
	for _, f := range ScanSecrets("leaked " + tok + " end") {
		if f.Type == "generic_token" {
			found = true
		}
	}
	if !found {
		t.Error("ScanSecrets(bare token) found no generic_token finding")
	}
}

func TestRedactGenericTokenCleanTextUnchanged(t *testing.T) {
	clean := []string{
		// The regression case from the audit: ordinary prose about
		// training must never be redacted.
		"Basic training starts on Monday.",
		// A hex digest has no uppercase letters: not a token.
		"commit a94a8fe5ccb19ba61c4c0873d391e987982fbbd3 landed",
		// A UUID has no uppercase letters either.
		"id 123e4567-e89b-12d3-a456-426614174000 was returned",
		// A long lowercase word has no digits: not a token.
		"antidisestablishmentarianism is a long word indeed",
		// A long document ID that is a URL path segment is left alone.
		"See https://docs.example.com/d/AbCdEfGhIjKlMnOpQrStUvWxYz0123456789/edit for the doc.",
		"",
	}
	for _, in := range clean {
		if got := RedactSecrets(in); got != in {
			t.Errorf("RedactSecrets(%q) = %q, want unchanged", in, got)
		}
		if len(ScanSecrets(in)) != 0 {
			t.Errorf("ScanSecrets(%q) found secrets, want none", in)
		}
	}
}

func TestScanSecretsTelegramTailNotDoubleReported(t *testing.T) {
	// The tail of a Telegram token is itself high-entropy, but it
	// must be reported once as "telegram", not again as a generic
	// token.
	findings := ScanSecrets("bot " + fakeTelegramLong)
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", findings)
	}
	if findings[0].Type != "telegram" {
		t.Fatalf("finding type = %q, want telegram", findings[0].Type)
	}
}

// ---------------------------------------------------------------------
// Audit log — P2-17 (enforce modes on open) and P2-18 (torn tail,
// RWMutex).
// ---------------------------------------------------------------------

func followupFileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return fi.Mode().Perm()
}

// TestAuditLogEnforcesModesOnOpen (P2-17): a pre-existing
// world-readable log file (and directory) must be tightened when the
// log is opened, not only when it is created.
func TestAuditLogEnforcesModesOnOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	log, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	if mode := followupFileMode(t, path); mode != 0o600 {
		t.Errorf("pre-existing log file mode = %o, want 0600 (tightened on open)", mode)
	}
	if mode := followupFileMode(t, dir); mode != 0o700 {
		t.Errorf("pre-existing log dir mode = %o, want 0700 (tightened on open)", mode)
	}
	// The log must still work after the tightening.
	if _, err := log.Append("agent", "test", "entry"); err != nil {
		t.Fatalf("Append after mode enforcement: %v", err)
	}
	if err := log.Verify(); err != nil {
		t.Fatalf("Verify after mode enforcement: %v", err)
	}
}

// TestAuditLogTornTailRecoveredOnOpen (P2-18): a crash mid-Append
// leaves a partially written last line with no trailing newline. The
// log must open, truncate the torn tail, verify, and keep working.
func TestAuditLogTornTailRecoveredOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	log, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	for _, d := range []string{"first entry", "second entry", "third entry"} {
		if _, err := log.Append("agent", "test-action", d); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Simulate the crash: a partial fourth line, no trailing newline.
	torn := append(bytes.Clone(raw),
		[]byte(`{"seq":4,"timestamp":"2026-11-13T02:00:00Z","actor":"age`)...)
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatalf("WriteFile torn log: %v", err)
	}

	reopened, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog on torn log: %v (want recovery, not an error)", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after recovery: %v", err)
	}
	if !bytes.Equal(after, raw) {
		t.Fatalf("torn tail was not truncated: file differs from the pre-crash bytes")
	}
	if err := reopened.Verify(); err != nil {
		t.Fatalf("Verify after recovery: %v", err)
	}
	entries, err := reopened.Entries()
	if err != nil {
		t.Fatalf("Entries after recovery: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries after recovery = %d, want 3", len(entries))
	}
	// The recovered log keeps working: the next append is seq 4.
	e, err := reopened.Append("agent", "test-action", "fourth entry")
	if err != nil {
		t.Fatalf("Append after recovery: %v", err)
	}
	if e.Seq != 4 {
		t.Fatalf("appended seq = %d, want 4", e.Seq)
	}
	if err := reopened.Verify(); err != nil {
		t.Fatalf("Verify after append on recovered log: %v", err)
	}
}

// TestAuditLogCompleteEntryMissingNewline: an entry whose JSON was
// fully written but whose trailing newline was lost is still a valid
// entry, not a torn tail — it must survive the reopen.
func TestAuditLogCompleteEntryMissingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	log, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	if _, err := log.Append("agent", "test-action", "only entry"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatal("test setup: log does not end with a newline")
	}
	if err := os.WriteFile(path, raw[:len(raw)-1], 0o600); err != nil {
		t.Fatalf("strip trailing newline: %v", err)
	}
	reopened, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog on newline-less entry: %v", err)
	}
	entries, err := reopened.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 1 || entries[0].Detail != "only entry" {
		t.Fatalf("entries = %v, want the one complete entry", entries)
	}
	if err := reopened.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestAuditLogConcurrentAppendVerify (P2-18): Verify/Entries take the
// read lock, so concurrent appends and verifications must not stall
// each other — and a Verify that races an in-flight Append must not
// fail on the partially written last line.
func TestAuditLogConcurrentAppendVerify(t *testing.T) {
	dir := t.TempDir()
	log, err := NewAuditLog(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < 25; i++ {
					if _, err := log.Append("agent", "act", fmt.Sprintf("w%d-%d", w, i)); err != nil {
						t.Errorf("Append: %v", err)
						return
					}
				}
			}(w)
		}
		for v := 0; v < 2; v++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 25; i++ {
					if err := log.Verify(); err != nil {
						t.Errorf("Verify during concurrent appends: %v", err)
						return
					}
					if _, err := log.Entries(); err != nil {
						t.Errorf("Entries during concurrent appends: %v", err)
						return
					}
				}
			}()
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent Append/Verify stalled (deadlock or livelock)")
	}
	if err := log.Verify(); err != nil {
		t.Fatalf("Verify after concurrent appends: %v", err)
	}
	entries, err := log.Entries()
	if err != nil {
		t.Fatalf("Entries after concurrent appends: %v", err)
	}
	if len(entries) != 100 {
		t.Fatalf("entries = %d, want 100", len(entries))
	}
}
