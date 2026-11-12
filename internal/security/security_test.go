package security

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// NOTE: every "secret" in this file is an obviously fake test value
// (e.g. sk-testfakekey123, AKIAIOSFODNN7EXAMPLE). No real credentials
// appear anywhere in this package.

// ---------------------------------------------------------------------
// IsBlockedCommand — strings only, never executed.
// ---------------------------------------------------------------------

func TestIsBlockedCommand(t *testing.T) {
	blocked := []string{
		"rm -rf /",
		"/bin/rm -rf /",
		"/usr/bin/rm -rf /*",
		"sudo rm -rf /",
		"rm -rf / ; echo hi",
		"mkfs.ext4 /dev/sda1",
		"dd if=/dev/zero of=/dev/sda",
		"shutdown -h now",
		"",
	}
	for _, cmd := range blocked {
		if !IsBlockedCommand(cmd) {
			t.Errorf("IsBlockedCommand(%q) = false, want true", cmd)
		}
	}

	allowed := []string{
		"echo hello",
		"ls -la",
		"rm /tmp/somefile.txt",
	}
	for _, cmd := range allowed {
		if IsBlockedCommand(cmd) {
			t.Errorf("IsBlockedCommand(%q) = true, want false", cmd)
		}
	}
}

// ---------------------------------------------------------------------
// Secret scanner
// ---------------------------------------------------------------------

// All fake test secrets. Each is clearly synthetic and unusable.
const (
	fakeOpenAI    = "sk-testfakekey123"
	fakeAnthropic = "sk-ant-testfakekey123456"
	fakeGitHub    = "ghp_testfakekey1234567890abcdefghij12"
	fakeGitHubOA  = "gho_testfakekey1234567890abcdefghij12"
	fakeGitHubPAT = "github_pat_testfakekey1234567890abcdefghij"
	fakeAWS       = "AKIAIOSFODNN7EXAMPLE"
	fakeTelegram  = "123456789:Abcdefghij1234567890abcdefghij12345"
	fakeBearer    = "Bearer abcdefgh12345678"
)

func TestRedactSecretsEachType(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"openai", "key is " + fakeOpenAI + " end"},
		{"anthropic", "key is " + fakeAnthropic + " end"},
		{"github_ghp", "token " + fakeGitHub},
		{"github_gho", "token " + fakeGitHubOA},
		{"github_pat", "token " + fakeGitHubPAT},
		{"aws", "aws key " + fakeAWS},
		{"telegram", "bot " + fakeTelegram},
		{"bearer", "auth " + fakeBearer},
		{"assignment_equals", "api_key = hunter2secret"},
		{"assignment_colon", "password: s3cretPassw0rd"},
		{"assignment_quoted", `token="supersecrettoken123"`},
		{"private_key", "key:\n-----BEGIN RSA PRIVATE KEY-----\nZmFrZWZha2VmYWtlZmFrZWZha2VmYWtl\n-----END RSA PRIVATE KEY-----\ndone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSecrets(tc.input)
			if !strings.Contains(got, Redacted) {
				t.Errorf("RedactSecrets(%q) = %q, no %q marker", tc.input, got, Redacted)
			}
			// No distinctive fragment of the original secret may survive.
			for _, frag := range []string{fakeOpenAI, fakeAnthropic, fakeGitHub, fakeGitHubOA, fakeGitHubPAT, fakeAWS, fakeTelegram, "abcdefgh12345678", "hunter2secret", "s3cretPassw0rd", "supersecrettoken123", "ZmFrZWZha2VmYWtl"} {
				if strings.Contains(tc.input, frag) && strings.Contains(got, frag) {
					t.Errorf("RedactSecrets(%q) = %q, still contains secret fragment %q", tc.input, got, frag)
				}
			}
		})
	}
}

func TestRedactSecretsKeepsKeyName(t *testing.T) {
	got := RedactSecrets("api_key = hunter2secret")
	if !strings.Contains(got, "api_key") {
		t.Errorf("key name lost: %q", got)
	}
	if !strings.Contains(got, "=") {
		t.Errorf("separator lost: %q", got)
	}
	if got != "api_key = "+Redacted {
		t.Errorf("got %q, want %q", got, "api_key = "+Redacted)
	}

	got = RedactSecrets("Authorization: " + fakeBearer)
	if !strings.Contains(got, "Bearer "+Redacted) {
		t.Errorf("bearer scheme not preserved: %q", got)
	}
}

func TestRedactSecretsCleanTextUnchanged(t *testing.T) {
	clean := []string{
		"Hello world, nothing secret here.",
		"See https://example.com/docs?page=2 for details.",
		"The token was discussed in the meeting, but no value was shared.",
		"My password is hard to remember, I keep forgetting it.",
		"echo hello",
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

func TestScanSecretsFindsTypes(t *testing.T) {
	text := strings.Join([]string{
		"openai " + fakeOpenAI,
		"anthropic " + fakeAnthropic,
		"github " + fakeGitHub,
		"aws " + fakeAWS,
		"telegram " + fakeTelegram,
		fakeBearer,
		"api_key = hunter2secret",
		"-----BEGIN EC PRIVATE KEY-----\nZmFrZQ==\n-----END EC PRIVATE KEY-----",
	}, "\n")

	findings := ScanSecrets(text)
	wantTypes := map[string]bool{
		"openai": true, "anthropic": true, "github": true, "aws": true,
		"telegram": true, "bearer": true, "generic": true, "private_key": true,
	}
	gotTypes := make(map[string]bool)
	for _, f := range findings {
		if f.Redacted != Redacted {
			t.Errorf("finding %+v has Redacted %q, want %q", f, f.Redacted, Redacted)
		}
		gotTypes[f.Type] = true
	}
	for typ := range wantTypes {
		if !gotTypes[typ] {
			t.Errorf("ScanSecrets missing type %q; got types %v", typ, gotTypes)
		}
	}
}

func TestScanSecretsDedupesIdenticalValues(t *testing.T) {
	text := "first " + fakeOpenAI + " and again " + fakeOpenAI + " and " + fakeAWS + " plus " + fakeAWS
	findings := ScanSecrets(text)
	counts := make(map[string]int)
	for _, f := range findings {
		counts[f.Type]++
	}
	if counts["openai"] != 1 {
		t.Errorf("openai findings = %d, want 1 (duplicate value deduped)", counts["openai"])
	}
	if counts["aws"] != 1 {
		t.Errorf("aws findings = %d, want 1 (duplicate value deduped)", counts["aws"])
	}
	if len(findings) != 2 {
		t.Errorf("total findings = %d, want 2", len(findings))
	}
}

func TestScanSecretsEmpty(t *testing.T) {
	if got := ScanSecrets(""); len(got) != 0 {
		t.Errorf("ScanSecrets(\"\") = %v, want empty", got)
	}
	if HasSecrets("nothing here") {
		t.Error("HasSecrets(clean text) = true, want false")
	}
	if !HasSecrets("key " + fakeAWS) {
		t.Error("HasSecrets(text with fake AWS key) = false, want true")
	}
}

// ---------------------------------------------------------------------
// Audit log
// ---------------------------------------------------------------------

func TestAuditLogAppendVerifyEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "audit.log")

	log, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	if st, err := os.Stat(path); err != nil {
		t.Fatalf("log file not created: %v", err)
	} else if st.Mode().Perm() != 0o600 {
		t.Errorf("log file mode = %o, want 0600", st.Mode().Perm())
	}
	if st, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("log dir not created: %v", err)
	} else if st.Mode().Perm() != 0o700 {
		t.Errorf("log dir mode = %o, want 0700", st.Mode().Perm())
	}

	details := []string{"first entry", "second entry", "third entry"}
	var appended []Entry
	for i, d := range details {
		e, err := log.Append("agent", "test-action", d)
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if e.Seq != i+1 {
			t.Errorf("entry %d Seq = %d, want %d", i, e.Seq, i+1)
		}
		if e.Detail != d || e.Actor != "agent" || e.Action != "test-action" {
			t.Errorf("entry %d = %+v, fields mismatch", i, e)
		}
		if e.Hash == "" {
			t.Errorf("entry %d has empty Hash", i)
		}
		appended = append(appended, e)
	}
	// Hash chain linkage.
	if appended[0].PrevHash != "" {
		t.Errorf("genesis PrevHash = %q, want empty", appended[0].PrevHash)
	}
	if appended[1].PrevHash != appended[0].Hash {
		t.Error("entry 2 PrevHash does not equal entry 1 Hash")
	}
	if appended[2].PrevHash != appended[1].Hash {
		t.Error("entry 3 PrevHash does not equal entry 2 Hash")
	}

	if err := log.Verify(); err != nil {
		t.Errorf("Verify after 3 appends: %v", err)
	}

	entries, err := log.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("Entries() = %d entries, want 3", len(entries))
	}
	for i, e := range entries {
		if e.Seq != i+1 || e.Detail != details[i] {
			t.Errorf("Entries()[%d] = %+v, want seq %d detail %q", i, e, i+1, details[i])
		}
		// Recompute the hash independently from the stored fields
		// (v2 entries are HMAC-SHA256 under the log's key).
		if e.Version != auditVersionHMAC {
			t.Errorf("Entries()[%d] Version = %d, want %d (HMAC)", i, e.Version, auditVersionHMAC)
		}
		if want := computeHashHMAC(log.key, e.PrevHash, e.Seq, e.Timestamp, e.Actor, e.Action, e.Detail); e.Hash != want {
			t.Errorf("Entries()[%d] hash mismatch", i)
		}
	}
}

func TestAuditLogReopenVerifiesChain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	log, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	if _, err := log.Append("agent", "start", "first entry"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := log.Append("agent", "work", "second entry"); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Reopening the same file must verify the existing chain and resume
	// sequencing from where the previous handle left off.
	reopened, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog (reopen): %v", err)
	}
	e, err := reopened.Append("agent", "more", "third entry")
	if err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	if e.Seq != 3 {
		t.Errorf("appended Seq after reopen = %d, want 3", e.Seq)
	}
	if err := reopened.Verify(); err != nil {
		t.Errorf("Verify after reopen + append: %v", err)
	}
	entries, err := reopened.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("Entries() = %d, want 3", len(entries))
	}
}

func TestAuditLogTamperDetected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	log, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	for _, d := range []string{"first entry", "second entry", "third entry"} {
		if _, err := log.Append("agent", "test-action", d); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := log.Verify(); err != nil {
		t.Fatalf("Verify before tamper: %v", err)
	}

	// Tamper: change one byte of a stored detail without touching hashes.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	tampered := bytes.Replace(raw, []byte("second entry"), []byte("second entry!"), 1)
	if bytes.Equal(raw, tampered) {
		t.Fatal("tamper step did not change the file")
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := log.Verify(); err == nil {
		t.Error("Verify() after tampering = nil, want error")
	}
	// Opening a tampered log must also fail.
	if _, err := NewAuditLog(path); err == nil {
		t.Error("NewAuditLog(tampered) = nil error, want error")
	}
	if _, err := log.Entries(); err == nil {
		t.Error("Entries() after tampering = nil error, want error")
	}
}

func TestAuditLogEmptyPath(t *testing.T) {
	if _, err := NewAuditLog(""); err == nil {
		t.Error("NewAuditLog(\"\") = nil error, want error")
	}
	if _, err := NewAuditLog("   "); err == nil {
		t.Error("NewAuditLog(whitespace) = nil error, want error")
	}
}
