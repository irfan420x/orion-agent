package messaging

import (
	"strings"
	"testing"
)

// TestRedactDisplayTextCredentialPaths verifies the Hermes leak —
// $(cat ~/.hermes/tokens/n8n-api.key) — never appears in rendered
// output, and that credential paths in all their forms are redacted.
func TestRedactDisplayTextCredentialPaths(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"hermes tokens cat", `K-$(cat ~/.hermes/tokens/n8n-api.key)`},
		{"ssh key cat", "cat ~/.ssh/id_rsa"},
		{"ssh key backtick", "`cat ~/.ssh/id_ed25519`"},
		{"ssh path bare", "open ~/.ssh/config now"},
		{"tokens dir", "read /home/irfan/app/tokens/service.key"},
		{"pem file", "openssl x509 -in /etc/ssl/private/server.pem"},
		{"key file", "load /opt/app/certs/tls.key"},
		{"p12 file", "import /home/irfan/certs/bundle.p12"},
		{"pfx file", "import /home/irfan/certs/bundle.pfx"},
		{"gnupg dir", "ls ~/.gnupg/private-keys-v1.d"},
	}
	for _, tc := range cases {
		got := RedactDisplayText(tc.in)
		for _, leak := range []string{"n8n-api.key", "id_rsa", "id_ed25519", "server.pem", "tls.key", "bundle.p12", "bundle.pfx", "private-keys-v1.d", ".ssh", "tokens/"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: RedactDisplayText(%q) = %q, leaks %q", tc.name, tc.in, got, leak)
			}
		}
		if !strings.Contains(got, "[redacted]") {
			t.Errorf("%s: RedactDisplayText(%q) = %q, want [redacted]", tc.name, tc.in, got)
		}
	}
	// The exact Hermes shape becomes $(cat [redacted]).
	if got := RedactDisplayText(`K-$(cat ~/.hermes/tokens/n8n-api.key)`); !strings.Contains(got, "$(cat [redacted])") {
		t.Fatalf("RedactDisplayText = %q, want $(cat [redacted])", got)
	}
}

// TestRedactDisplayTextOrdinaryPaths verifies ordinary work paths
// pass through untouched.
func TestRedactDisplayTextOrdinaryPaths(t *testing.T) {
	for _, in := range []string{
		"chmod +x ~/bin/pulse.sh && ~/bin/pulse.sh",
		"cat /home/irfan/Desktop/orion/README.md",
		"ls /home/irfan/projects/orion-go",
		"echo hello world",
	} {
		if got := RedactDisplayText(in); got != in {
			t.Errorf("RedactDisplayText(%q) = %q, want unchanged", in, got)
		}
	}
}

// TestRedactDisplayTextSecrets verifies RedactSecrets passthrough:
// an API-key-shaped value is redacted, as are header values and
// api_key=/token= assignments that survive it.
func TestRedactDisplayTextSecrets(t *testing.T) {
	key := "sk-" + strings.Repeat("Ab3d", 8) // test placeholder only
	got := RedactDisplayText("leaked " + key + " end")
	if strings.Contains(got, key) {
		t.Fatalf("RedactDisplayText = %q, API key survived", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("RedactDisplayText = %q, want [REDACTED]", got)
	}

	for _, in := range []string{
		"Authorization: Bearer abcdefghijklmnop",
		"curl -H 'Authorization: Basic dXNlcjpwYXNz' https://example.com",
		"api_key=abcdefgh1234",
		"token=abcdefgh1234",
		"API_KEY=abcdefgh1234",
	} {
		got := RedactDisplayText(in)
		if strings.Contains(got, "abcdefgh") && !strings.Contains(got, "[redacted]") {
			t.Errorf("RedactDisplayText(%q) = %q, credential value survived", in, got)
		}
	}
}

// TestRedactDisplayTextCardIntegration verifies the renderer never
// shows the Hermes leak even when the model emits it as a command.
func TestRedactDisplayTextCardIntegration(t *testing.T) {
	var c TerminalCard
	c.Add(started("shell_exec", mustJSONArgs(t, "command", "K-$(cat ~/.hermes/tokens/n8n-api.key)")))
	got := c.Render()
	if strings.Contains(got, "n8n-api.key") || strings.Contains(got, ".hermes/tokens") {
		t.Fatalf("card = %q, credential path leaked into the card", got)
	}
	if !strings.Contains(got, "$(cat [redacted])") {
		t.Fatalf("card = %q, want $(cat [redacted])", got)
	}
}
