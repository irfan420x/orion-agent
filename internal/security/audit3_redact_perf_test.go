// Audit 3, Workstream 1 — regression tests for the RedactSecrets
// fast path (internal/security/scanner.go).
//
// RedactSecrets runs on EVERY tool result the agent loop handles, so
// its cost is loop cost: before the marker pre-filter, redacting
// 1 MiB of secret-free text took ~1.5 s (~30 s under -race) because
// Go's regexp engine charges 50–400 ms per pattern per MiB. The
// pre-filter skips each detector unless a literal marker that MUST
// be present for a match is found. These tests pin both halves:
// secret-free large inputs must pass through byte-identical (and
// fast), and every secret type embedded in a large secret-free
// filler must still be redacted (a marker must never skip a real
// secret).
package security

import (
	"strings"
	"testing"
	"time"
)

func TestAudit3RedactLargeSecretFreeInputUnchanged(t *testing.T) {
	inputs := map[string]string{
		"repeated-rune": strings.Repeat("w", 1024*1024),
		"normal-text":   strings.Repeat("hello world, this is normal text. ", 30000),
		// Deliberately near-miss text: words that contain marker
		// substrings without being secrets.
		"near-misses": strings.Repeat("the token effort is password-protected by secret agents; bearer of bad news; basic training; ", 10000),
	}
	for name, in := range inputs {
		t0 := time.Now()
		out := RedactSecrets(in)
		dur := time.Since(t0)
		if out != in {
			t.Errorf("%s: RedactSecrets changed secret-free input (%d bytes -> %d bytes)", name, len(in), len(out))
		}
		t.Logf("AUDIT3 REDACT %s: 1 MiB secret-free in %v", name, dur)
	}
}

func TestAudit3RedactSecretsStillFoundInLargeInput(t *testing.T) {
	filler := strings.Repeat("ordinary filler text with no secrets. ", 20000) // ~780 KiB
	secrets := map[string]string{
		"openai":    "sk-proj-abcdefghij1234567890ABCDEFGH",
		"anthropic": "sk-ant-api03-abcdefghij1234567890",
		"stripe":    "sk_live_abcdefghij1234567890",
		"github":    "ghp_abcdefghij1234567890ABCD",
		"aws":       "AKIA1234567890ABCDEF",
		"telegram":  "123456789:AAEabcdefghij1234567890ABCDEFGHIJKLMN",
		"slack":     "xoxb-123456789012-abcdefghij",
		"jwt":       "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
		"bearer":    "Bearer abcdefghij1234567890TOKEN",
		"basic":     "Basic dXNlcjpwYXNzd29yZA==",
		"url":       "https://user:hunter2secret@example.com/path",
		"assign":    "api_key = hunter2secretvalue",
		"generic":   "Ab3dEf6hIjKlMnOpQrStUvWxYz0123456789",
		"private":   "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA7\n-----END RSA PRIVATE KEY-----",
	}
	for name, secret := range secrets {
		in := filler + "\n" + secret + "\n" + filler
		out := RedactSecrets(in)
		if strings.Contains(out, secret) {
			t.Errorf("%s: secret survived redaction in a large input", name)
		}
		if !strings.Contains(out, Redacted) {
			t.Errorf("%s: no %q marker in redacted output", name, Redacted)
		}
	}
}

func TestAudit3RedactMarkerHelpers(t *testing.T) {
	if !hasLongTokenRun(strings.Repeat("a", 32)) {
		t.Error("hasLongTokenRun: 32-char run not detected")
	}
	if hasLongTokenRun(strings.Repeat("a", 31)) {
		t.Error("hasLongTokenRun: 31-char run falsely detected")
	}
	if hasLongTokenRun("short words only here") {
		t.Error("hasLongTokenRun: short words falsely detected")
	}
	// Telegram shape: exactly 8–10 digits then ':'.
	if !hasTelegramTokenShape("id 12345678:abcdef") {
		t.Error("hasTelegramTokenShape: 8-digit run + ':' not detected")
	}
	if !hasTelegramTokenShape("id 1234567890:abcdef") {
		t.Error("hasTelegramTokenShape: 10-digit run + ':' not detected")
	}
	if hasTelegramTokenShape("id 1234567:abcdef") {
		t.Error("hasTelegramTokenShape: 7-digit run falsely detected")
	}
	if hasTelegramTokenShape("id 12345678901:abcdef") {
		t.Error("hasTelegramTokenShape: 11-digit run falsely detected (regex cannot match it)")
	}
	if hasTelegramTokenShape("no digits here: at all") {
		t.Error("hasTelegramTokenShape: no-digit text falsely detected")
	}
}
