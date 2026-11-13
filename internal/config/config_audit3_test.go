package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Audit 3, Workstream 5 (installation and recovery): a config file
// that is only ever *read* — the normal daemon/CLI path — must not
// stay world-readable forever. Save already replaces a lax file with
// a 0600 one, but a hand-edited 0644 config.json (the usual way a
// user adds a key) was never tightened by Load, so the API keys and
// Telegram token it holds leaked to every local user indefinitely.

// TestLoadTightensLaxPerms: a 0644 config holding secrets must come
// back 0600 after a plain Load, with its contents intact.
func TestLoadTightensLaxPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	const body = `{"default_provider":"ollama","providers":{"ollama":{"name":"ollama","base_url":"http://localhost:11434","model":"llama3.1","api_key":"sk-fake-loose-key"}},"telegram_token":"123456:AAAAAAAAAAAAAAAAAAAA"}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Providers["ollama"].APIKey; got != "sk-fake-loose-key" {
		t.Fatalf("API key changed by Load: %q", got)
	}
	if st, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("loaded config mode = %o, want 0600", got)
	}
}

// TestLoadTightensLaxPermsEvenWhenCorrupt: tightening happens before
// parsing, so even a config Load rejects still stops leaking secrets.
func TestLoadTightensLaxPermsEvenWhenCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("corrupt config should error")
	} else if !strings.Contains(err.Error(), "parse") {
		t.Fatalf("corrupt config error = %v, want a parse error", err)
	}
	if st, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("corrupt config mode = %o, want 0600", got)
	}
}

// TestLoadLeavesStrictPermsAlone: an already-0600 file is untouched
// (no mtime churn, no surprises on read-only setups).
func TestLoadLeavesStrictPermsAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"default_provider":"ollama"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != 0o600 || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("strict file changed by Load: mode %o mtime %v (was %v)", after.Mode().Perm(), after.ModTime(), before.ModTime())
	}
}

// TestLoadTightensGroupReadable: 0640 (group-readable) is tightened
// too — only the owner may read a file holding secrets.
func TestLoadTightensGroupReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"default_provider":"ollama"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("loaded config mode = %o, want 0600", got)
	}
}
