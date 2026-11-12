package security

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// NOTE: every "secret" in this file is an obviously fake test value
// (e.g. sk-proj-testfakekey..., ASIAIOSFODNN7EXAMPLE). No real
// credentials appear anywhere in this package.

// ---------------------------------------------------------------------
// Secret scanner — formats the first pass missed.
// ---------------------------------------------------------------------

// More fake test secrets. Each is clearly synthetic and unusable.
const (
	fakeOpenAIProj    = "sk-proj-testfakekey1234567890abcdefghij"
	fakeOpenAISvc     = "sk-svcacct-testfakekey1234567890abcdefghij"
	fakeStripeLive    = "sk_live_testfake1234567890abcdefghij"
	fakeStripeRK      = "rk_live_testfake1234567890abcdefghij"
	fakeStripeTest    = "sk_test_testfake1234567890abcdefghij"
	fakeSlack         = "xoxb-testfake-1234567890-abcdefghij"
	fakeSlackApp      = "xoxa-testfake-1234567890-abcdefghij"
	fakeAWSsession    = "ASIAIOSFODNN7EXAMPLE"
	fakeGitHubUser    = "ghu_testfakekey1234567890abcdefghij12"
	fakeGitHubServer  = "ghs_testfakekey1234567890abcdefghij12"
	fakeGitHubRefresh = "ghr_testfakekey1234567890abcdefghij12"
	fakeTelegramLong  = "123456789:Abcdefghij1234567890abcdefghij12345EXTRA"
	fakeJWT           = "eyJ0ZXN0ZmFrZWhlYWRlcg.eyJ0ZXN0ZmFrZXBheWxvYWQ.dGVzdGZha2VzaWduYXR1cmU"
	fakeBasic         = "Basic dGVzdGZha2V1c2VyOnRlc3RmYWtlcGFzc3dvcmQ"
	fakeURLCreds      = "https://testuser:TestFakePass123@api.example.com/v1"
)

func TestRedactSecretsNewTypes(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"openai_proj", "key is " + fakeOpenAIProj + " end"},
		{"openai_svcacct", "key is " + fakeOpenAISvc + " end"},
		{"stripe_live", "key " + fakeStripeLive},
		{"stripe_rk", "key " + fakeStripeRK},
		{"stripe_test", "key " + fakeStripeTest},
		{"slack_bot", "token " + fakeSlack},
		{"slack_app", "token " + fakeSlackApp},
		{"aws_session", "aws key " + fakeAWSsession},
		{"github_user", "token " + fakeGitHubUser},
		{"github_server", "token " + fakeGitHubServer},
		{"github_refresh", "token " + fakeGitHubRefresh},
		{"telegram_long", "bot " + fakeTelegramLong},
		{"jwt", "token " + fakeJWT},
		{"basic", "auth " + fakeBasic},
		{"url_creds", "see " + fakeURLCreds},
		{"assignment_access_key", "access_key = hunter2secret"},
		{"assignment_passwd", "passwd: s3cretPassw0rd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSecrets(tc.input)
			if !strings.Contains(got, Redacted) {
				t.Errorf("RedactSecrets(%q) = %q, no %q marker", tc.input, got, Redacted)
			}
			// No distinctive fragment of the original secret may survive.
			for _, frag := range []string{
				fakeOpenAIProj, fakeOpenAISvc, fakeStripeLive, fakeStripeRK, fakeStripeTest,
				fakeSlack, fakeSlackApp, fakeAWSsession,
				fakeGitHubUser, fakeGitHubServer, fakeGitHubRefresh,
				fakeTelegramLong, fakeJWT,
				"dGVzdGZha2V1c2VyOnRlc3RmYWtlcGFzc3dvcmQ",
				"TestFakePass123", "hunter2secret", "s3cretPassw0rd",
			} {
				if strings.Contains(tc.input, frag) && strings.Contains(got, frag) {
					t.Errorf("RedactSecrets(%q) = %q, still contains secret fragment %q", tc.input, got, frag)
				}
			}
		})
	}
}

func TestRedactSecretsKeepsURLUserAndSchemes(t *testing.T) {
	got := RedactSecrets("see " + fakeURLCreds)
	if want := "see https://testuser:" + Redacted + "@api.example.com/v1"; got != want {
		t.Errorf("RedactSecrets(url creds) = %q, want %q", got, want)
	}

	got = RedactSecrets("Authorization: " + fakeBasic)
	if !strings.Contains(got, "Basic "+Redacted) {
		t.Errorf("Basic scheme not preserved: %q", got)
	}

	// A JWT presented as a Bearer token keeps the Bearer scheme.
	got = RedactSecrets("Authorization: Bearer " + fakeJWT)
	if !strings.Contains(got, "Bearer "+Redacted) {
		t.Errorf("Bearer scheme not preserved for JWT: %q", got)
	}
	if strings.Contains(got, fakeJWT) {
		t.Errorf("JWT survived Bearer redaction: %q", got)
	}
}

func TestRedactSecretsNewTypesCleanTextUnchanged(t *testing.T) {
	clean := []string{
		"See https://example.com/docs?page=2 for details.",
		"See https://example.com:8080/path for details.",
		"Contact testuser@example.com for access.",
		"See https://testuser@example.com/path (no password).",
		"The version is 1.2.3 of the doc.",
		"The word xoxb was mentioned, but no token was shared.",
		"The sk- prefix was discussed in the meeting.",
		"Basic training starts on Monday.",
		"My auth is handled by the team.",
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

func TestScanSecretsFindsNewTypes(t *testing.T) {
	text := strings.Join([]string{
		"openai-proj " + fakeOpenAIProj,
		"stripe " + fakeStripeLive,
		"slack " + fakeSlack,
		"aws-session " + fakeAWSsession,
		"github-user " + fakeGitHubUser,
		"telegram-long " + fakeTelegramLong,
		"jwt " + fakeJWT,
		"basic " + fakeBasic,
		"url " + fakeURLCreds,
		"access_key = hunter2secret",
	}, "\n")

	findings := ScanSecrets(text)
	wantTypes := map[string]bool{
		"openai": true, "stripe": true, "slack": true, "aws": true,
		"github": true, "telegram": true, "jwt": true, "basic": true,
		"url_password": true, "generic": true,
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
	// The URL password must be reported once (as url_password) and must
	// not also be double-reported as a generic assignment value.
	counts := make(map[string]int)
	for _, f := range findings {
		counts[f.Type]++
	}
	if counts["url_password"] != 1 {
		t.Errorf("url_password findings = %d, want 1", counts["url_password"])
	}
}

func TestScanSecretsAnthropicNotMisclassifiedAsOpenAI(t *testing.T) {
	findings := ScanSecrets("key " + fakeAnthropic)
	types := make(map[string]bool)
	for _, f := range findings {
		types[f.Type] = true
	}
	if !types["anthropic"] {
		t.Errorf("anthropic key not reported as anthropic; got %v", types)
	}
	if types["openai"] {
		t.Errorf("anthropic key also reported as openai; got %v", types)
	}
	if len(findings) != 1 {
		t.Errorf("findings = %d, want 1", len(findings))
	}
}

// ---------------------------------------------------------------------
// Audit log — single-byte tampering.
// ---------------------------------------------------------------------

// flipByteAt returns a copy of raw with the byte at offset flipped.
// The caller must ensure offset is inside raw.
func flipByteAt(raw []byte, offset int) []byte {
	out := bytes.Clone(raw)
	out[offset] ^= 0x01
	return out
}

// tamperAuditLog builds a log with three entries, verifies it, applies
// tamper to the raw file bytes, and returns the path and log handle.
func tamperAuditLog(t *testing.T, tamper func(raw []byte) []byte) (string, *AuditLog) {
	t.Helper()
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
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	tampered := tamper(raw)
	if bytes.Equal(raw, tampered) {
		t.Fatal("tamper step did not change the file")
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path, log
}

// assertTamperDetected fails the test unless every way of reading the
// log now reports the tampering.
func assertTamperDetected(t *testing.T, path string, log *AuditLog) {
	t.Helper()
	if err := log.Verify(); err == nil {
		t.Error("Verify() after tampering = nil, want error")
	}
	if _, err := NewAuditLog(path); err == nil {
		t.Error("NewAuditLog(tampered) = nil error, want error")
	}
	if _, err := log.Entries(); err == nil {
		t.Error("Entries() after tampering = nil error, want error")
	}
}

func TestAuditLogSingleByteFlipInDetailDetected(t *testing.T) {
	path, log := tamperAuditLog(t, func(raw []byte) []byte {
		// Flip exactly one byte inside the second entry's detail.
		off := bytes.Index(raw, []byte("second entry"))
		if off < 0 {
			t.Fatal("detail not found in log file")
		}
		return flipByteAt(raw, off) // 's' -> 'r'
	})
	assertTamperDetected(t, path, log)
}

func TestAuditLogSingleByteFlipInHashDetected(t *testing.T) {
	path, log := tamperAuditLog(t, func(raw []byte) []byte {
		// Flip exactly one hex digit of the second entry's stored hash.
		off := bytes.Index(raw, []byte(`"hash":"`))
		if off < 0 {
			t.Fatal("hash field not found in log file")
		}
		// Second occurrence: the first belongs to the genesis entry.
		second := bytes.Index(raw[off+1:], []byte(`"hash":"`))
		if second < 0 {
			t.Fatal("second hash field not found in log file")
		}
		return flipByteAt(raw, off+1+second+len(`"hash":"`))
	})
	assertTamperDetected(t, path, log)
}

func TestAuditLogSingleByteFlipInTimestampDetected(t *testing.T) {
	path, log := tamperAuditLog(t, func(raw []byte) []byte {
		// Flip one digit of the second entry's timestamp.
		off := bytes.Index(raw, []byte(`"timestamp":"`))
		if off < 0 {
			t.Fatal("timestamp field not found in log file")
		}
		second := bytes.Index(raw[off+1:], []byte(`"timestamp":"`))
		if second < 0 {
			t.Fatal("second timestamp field not found in log file")
		}
		// Offset of a digit inside the timestamp value (the seconds).
		base := off + 1 + second + len(`"timestamp":"`)
		return flipByteAt(raw, base+17)
	})
	assertTamperDetected(t, path, log)
}

func TestAuditLogSequenceTamperDetected(t *testing.T) {
	path, log := tamperAuditLog(t, func(raw []byte) []byte {
		// Change the second entry's seq from 2 to 9 (one byte).
		off := bytes.Index(raw, []byte(`"seq":2`))
		if off < 0 {
			t.Fatal("seq 2 not found in log file")
		}
		out := bytes.Clone(raw)
		out[off+len(`"seq":`)] = '9'
		return out
	})
	assertTamperDetected(t, path, log)
}

func TestAuditLogMiddleLineDeletedDetected(t *testing.T) {
	path, log := tamperAuditLog(t, func(raw []byte) []byte {
		lines := bytes.Split(raw, []byte("\n"))
		// Drop the second entry's line (index 1).
		kept := append([][]byte{}, lines[0])
		kept = append(kept, lines[2:]...)
		return bytes.Join(kept, []byte("\n"))
	})
	assertTamperDetected(t, path, log)
}

func TestAuditLogLinesSwappedDetected(t *testing.T) {
	path, log := tamperAuditLog(t, func(raw []byte) []byte {
		lines := bytes.Split(raw, []byte("\n"))
		lines[0], lines[1] = lines[1], lines[0]
		return bytes.Join(lines, []byte("\n"))
	})
	assertTamperDetected(t, path, log)
}

func TestAuditLogGarbageLineAppendedDetected(t *testing.T) {
	path, log := tamperAuditLog(t, func(raw []byte) []byte {
		return append(bytes.Clone(raw), []byte("this is not a json audit line\n")...)
	})
	assertTamperDetected(t, path, log)
}

func TestAuditLogActorTamperDetected(t *testing.T) {
	path, log := tamperAuditLog(t, func(raw []byte) []byte {
		// Flip one byte of the second entry's actor field.
		off := bytes.Index(raw, []byte(`"actor":"agent"`))
		if off < 0 {
			t.Fatal("actor field not found in log file")
		}
		second := bytes.Index(raw[off+1:], []byte(`"actor":"agent"`))
		if second < 0 {
			t.Fatal("second actor field not found in log file")
		}
		return flipByteAt(raw, off+1+second+len(`"actor":"a`))
	})
	assertTamperDetected(t, path, log)
}
