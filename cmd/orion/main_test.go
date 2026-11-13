package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/messaging"
)

// scratchHome points HOME at a fresh temp dir so tests never touch the
// real ~/.orion config.
func scratchHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("ORION_TELEGRAM_TOKEN", "")
	t.Setenv("ORION_API_KEY", "")
	t.Setenv("ORION_PROVIDER", "")
	t.Setenv("ORION_MODEL", "")
	t.Setenv("ORION_DATA_DIR", "")
	return dir
}

// quiet runs fn with stdout/stderr discarded so command output does not
// spam the test log; the returned error is fn's.
func quiet(fn func() error) error {
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, _ := os.Pipe()
	os.Stdout, os.Stderr = w, w
	err := fn()
	_ = w.Close()
	_, _ = io.Copy(io.Discard, r)
	os.Stdout, os.Stderr = oldOut, oldErr
	return err
}

func TestMaskSecret(t *testing.T) {
	if got := maskSecret(""); got != "(not set)" {
		t.Errorf("empty: got %q", got)
	}
	// A set secret reveals nothing about its value: no first/last
	// characters (for a 5-6 char secret that is most of the value)
	// and no byte slice that could split a non-ASCII rune.
	for _, s := range []string{"ab", "abcde", "abcdefgh", "pässwörd🔑"} {
		if got := maskSecret(s); got != "****" {
			t.Errorf("maskSecret(%q): got %q, want %q", s, got, "****")
		}
	}
	if got := maskSecret("abcdefgh"); strings.Contains(got, "ab") || strings.Contains(got, "gh") || strings.Contains(got, "cdef") {
		t.Errorf("maskSecret leaked part of the secret: %q", got)
	}
}

func TestIsHelpArg(t *testing.T) {
	if !isHelpArg("--help") || !isHelpArg("-h") {
		t.Error("--help/-h should be help args")
	}
	if isHelpArg("help") || isHelpArg("--version") || isHelpArg("") {
		t.Error("non-help args misclassified")
	}
}

func TestCmdRunValidation(t *testing.T) {
	scratchHome(t)
	ctx := context.Background()
	if err := quiet(func() error { return cmdRun(ctx, nil) }); err == nil || !strings.Contains(err.Error(), "provide a prompt") {
		t.Errorf("no prompt: got err %v", err)
	}
	// --help must print usage, not burn a provider call.
	if err := quiet(func() error { return cmdRun(ctx, []string{"--help"}) }); err != nil {
		t.Errorf("--help: got err %v", err)
	}
	if err := quiet(func() error { return cmdRun(ctx, []string{"-h"}) }); err != nil {
		t.Errorf("-h: got err %v", err)
	}
}

func TestCmdChatValidation(t *testing.T) {
	scratchHome(t)
	ctx := context.Background()
	if err := quiet(func() error { return cmdChat(ctx, []string{"extra"}) }); err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("extra arg: got err %v", err)
	}
	if err := quiet(func() error { return cmdChat(ctx, []string{"--help"}) }); err != nil {
		t.Errorf("--help: got err %v", err)
	}
}

func TestCmdConfigValidation(t *testing.T) {
	scratchHome(t)
	if err := quiet(func() error { return cmdConfig([]string{"--help"}) }); err != nil {
		t.Errorf("--help: got err %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"bogus"}) }); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("bogus: got err %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"show", "extra"}) }); err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("show extra: got err %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"set-provider"}) }); err == nil || !strings.Contains(err.Error(), "provide a provider name") {
		t.Errorf("set-provider missing: got err %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"set-provider", "bogus"}) }); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("set-provider bogus: got err %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"set-provider", "ollama", "extra"}) }); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("set-provider extra: got err %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"set-model"}) }); err == nil || !strings.Contains(err.Error(), "provide a model name") {
		t.Errorf("set-model missing: got err %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"set-model", "a", "b"}) }); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("set-model extra: got err %v", err)
	}
}

// Local commands must keep working when the default provider is a
// key-requiring one: they never construct a provider, so they stay usable
// even if the provider itself is unreachable. The key below is an
// obviously fake placeholder, following the convention in
// internal/config/config_test.go.
func TestLocalCommandsWithKeyedProvider(t *testing.T) {
	dir := scratchHome(t)
	cfgJSON := `{
  "default_provider": "openai",
  "providers": {
    "openai":    {"name": "openai", "api_key": "test-placeholder-key", "model": "gpt-4o-mini"},
    "anthropic": {"name": "anthropic", "model": "claude-sonnet-4-5"},
    "ollama":    {"name": "ollama", "base_url": "http://localhost:11434", "model": "llama3.1"}
  },
  "model": "",
  "telegram_token": "",
  "data_dir": "` + dir + `/.orion",
  "max_steps": 20,
  "timeout_seconds": 120
}`
	if err := os.MkdirAll(dir+"/.orion", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/.orion/config.json", []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]func() error{
		"config show": func() error { return cmdConfig([]string{"show"}) },
		"status":      func() error { return cmdStatus(nil) },
		"tools":       func() error { return cmdTools(context.Background(), nil) },
		"memory list": func() error { return cmdMemory([]string{"list"}) },
		"skills":      func() error { return cmdSkills(nil) },
	} {
		if err := quiet(fn); err != nil {
			t.Errorf("%s with openai default: %v", name, err)
		}
	}
}

// Switching to a key-requiring provider without a stored key must fail
// with a clear message (config validation), and set-api-key must unblock
// it — otherwise there would be no CLI path to that provider at all.
func TestConfigSetProviderNeedsKey(t *testing.T) {
	scratchHome(t)
	if err := quiet(func() error { return cmdConfig([]string{"set-provider", "openai"}) }); err == nil || !strings.Contains(err.Error(), "requires an API key") {
		t.Errorf("set-provider openai without key: got err %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"set-api-key", "openai", "test-placeholder-key"}) }); err != nil {
		t.Fatalf("set-api-key: %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"set-provider", "openai"}) }); err != nil {
		t.Fatalf("set-provider openai after set-api-key: %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"show"}) }); err != nil {
		t.Fatalf("show after switch: %v", err)
	}
}

func TestCmdConfigSetRoundTrip(t *testing.T) {
	scratchHome(t)
	if err := quiet(func() error { return cmdConfig([]string{"set-api-key", "anthropic", "test-placeholder-key"}) }); err != nil {
		t.Fatalf("set-api-key: %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"set-provider", "anthropic"}) }); err != nil {
		t.Fatalf("set-provider: %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"set-model", "test-model-1"}) }); err != nil {
		t.Fatalf("set-model: %v", err)
	}
	// show must reflect the new values without needing a live provider.
	if err := quiet(func() error { return cmdConfig([]string{"show"}) }); err != nil {
		t.Fatalf("show after set: %v", err)
	}
}

func TestCmdConfigSetAPIKeyValidation(t *testing.T) {
	scratchHome(t)
	if err := quiet(func() error { return cmdConfig([]string{"set-api-key"}) }); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("set-api-key missing args: got err %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"set-api-key", "openai"}) }); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("set-api-key missing key: got err %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"set-api-key", "bogus", "k"}) }); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("set-api-key bogus provider: got err %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"set-api-key", "openai", "k", "extra"}) }); err == nil || !strings.Contains(err.Error(), "exactly") {
		t.Errorf("set-api-key extra arg: got err %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"set-api-key", "openai", "   "}) }); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Errorf("set-api-key blank key: got err %v", err)
	}
}

func TestCmdStatusValidation(t *testing.T) {
	scratchHome(t)
	if err := quiet(func() error { return cmdStatus([]string{"extra"}) }); err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("extra arg: got err %v", err)
	}
	if err := quiet(func() error { return cmdStatus([]string{"--help"}) }); err != nil {
		t.Errorf("--help: got err %v", err)
	}
	if err := quiet(func() error { return cmdStatus(nil) }); err != nil {
		t.Errorf("status: %v", err)
	}
}

func TestCmdToolsValidation(t *testing.T) {
	scratchHome(t)
	if err := quiet(func() error { return cmdTools(context.Background(), []string{"--help"}) }); err != nil {
		t.Errorf("--help: got err %v", err)
	}
	// exec with no tool name must be a usage error, not a silent list.
	if err := quiet(func() error { return cmdTools(context.Background(), []string{"exec"}) }); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("exec no name: got err %v", err)
	}
	if err := quiet(func() error { return cmdTools(context.Background(), []string{"bogus"}) }); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("bogus: got err %v", err)
	}
	if err := quiet(func() error { return cmdTools(context.Background(), []string{"exec", "calculator", "{}", "extra"}) }); err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Errorf("exec extra arg: got err %v", err)
	}
	if err := quiet(func() error { return cmdTools(context.Background(), []string{"exec", "bogus", "{}"}) }); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("exec unknown tool: got err %v", err)
	}
	if err := quiet(func() error { return cmdTools(context.Background(), []string{"exec", "calculator", "not json"}) }); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("exec bad JSON: got err %v", err)
	}
	if err := quiet(func() error { return cmdTools(context.Background(), nil) }); err != nil {
		t.Errorf("tools list: %v", err)
	}
	if err := quiet(func() error {
		return cmdTools(context.Background(), []string{"exec", "calculator", `{"expression":"2+2"}`})
	}); err != nil {
		t.Errorf("tools exec calculator: %v", err)
	}
}

func TestCmdMemoryValidationAndRoundTrip(t *testing.T) {
	scratchHome(t)
	if err := quiet(func() error { return cmdMemory([]string{"--help"}) }); err != nil {
		t.Errorf("--help: got err %v", err)
	}
	if err := quiet(func() error { return cmdMemory([]string{"bogus"}) }); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("bogus: got err %v", err)
	}
	if err := quiet(func() error { return cmdMemory([]string{"add"}) }); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("add missing args: got err %v", err)
	}
	if err := quiet(func() error { return cmdMemory([]string{"get"}) }); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("get missing arg: got err %v", err)
	}
	if err := quiet(func() error { return cmdMemory([]string{"get", "a", "b"}) }); err == nil || !strings.Contains(err.Error(), "exactly one key") {
		t.Errorf("get extra arg: got err %v", err)
	}
	if err := quiet(func() error { return cmdMemory([]string{"search"}) }); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("search missing arg: got err %v", err)
	}
	if err := quiet(func() error { return cmdMemory([]string{"list", "extra"}) }); err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("list extra: got err %v", err)
	}
	if err := quiet(func() error { return cmdMemory([]string{"get", "nosuchkey"}) }); err == nil || !strings.Contains(err.Error(), "no fact") {
		t.Errorf("get missing key: got err %v", err)
	}
	// Round trip: add -> get -> list -> search.
	if err := quiet(func() error { return cmdMemory([]string{"add", "favcolor", "blue"}) }); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := quiet(func() error { return cmdMemory([]string{"get", "favcolor"}) }); err != nil {
		t.Errorf("get: %v", err)
	}
	if err := quiet(func() error { return cmdMemory([]string{"list"}) }); err != nil {
		t.Errorf("list: %v", err)
	}
	if err := quiet(func() error { return cmdMemory([]string{"search", "fav"}) }); err != nil {
		t.Errorf("search: %v", err)
	}
}

func TestCmdSkillsValidation(t *testing.T) {
	scratchHome(t)
	if err := quiet(func() error { return cmdSkills([]string{"--help"}) }); err != nil {
		t.Errorf("--help: got err %v", err)
	}
	if err := quiet(func() error { return cmdSkills([]string{"bogus"}) }); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("bogus: got err %v", err)
	}
	if err := quiet(func() error { return cmdSkills([]string{"search"}) }); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("search no query: got err %v", err)
	}
	if err := quiet(func() error { return cmdSkills(nil) }); err != nil {
		t.Errorf("skills list: %v", err)
	}
	if err := quiet(func() error { return cmdSkills([]string{"search", "nosuchskill"}) }); err != nil {
		t.Errorf("skills search: %v", err)
	}
}

func TestCmdSubagentValidation(t *testing.T) {
	scratchHome(t)
	ctx := context.Background()
	if err := quiet(func() error { return cmdSubagent(ctx, nil) }); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("no args: got err %v", err)
	}
	if err := quiet(func() error { return cmdSubagent(ctx, []string{"bogus"}) }); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("bogus: got err %v", err)
	}
	if err := quiet(func() error { return cmdSubagent(ctx, []string{"run"}) }); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("run no brief: got err %v", err)
	}
	if err := quiet(func() error { return cmdSubagent(ctx, []string{"--help"}) }); err != nil {
		t.Errorf("--help: got err %v", err)
	}
}

func TestCmdTelegramValidationBeforeTokenCheck(t *testing.T) {
	scratchHome(t)
	ctx := context.Background()
	// With no token configured, a bad subcommand must still report the
	// bad subcommand — not the missing token.
	if err := quiet(func() error { return cmdTelegram(ctx, []string{"bogus"}) }); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("bogus: got err %v", err)
	}
	if err := quiet(func() error { return cmdTelegram(ctx, nil) }); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("no args: got err %v", err)
	}
	if err := quiet(func() error { return cmdTelegram(ctx, []string{"test", "extra"}) }); err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("test extra: got err %v", err)
	}
	if err := quiet(func() error { return cmdTelegram(ctx, []string{"--help"}) }); err != nil {
		t.Errorf("--help: got err %v", err)
	}
	// A valid subcommand with no token reports the missing token.
	if err := quiet(func() error { return cmdTelegram(ctx, []string{"test"}) }); err == nil || !strings.Contains(err.Error(), "no token configured") {
		t.Errorf("test no token: got err %v", err)
	}
}

func TestCmdDaemonValidation(t *testing.T) {
	scratchHome(t)
	ctx := context.Background()
	if err := quiet(func() error { return cmdDaemon(ctx, []string{"extra"}) }); err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("extra arg: got err %v", err)
	}
	if err := quiet(func() error { return cmdDaemon(ctx, []string{"--help"}) }); err != nil {
		t.Errorf("--help: got err %v", err)
	}
	// No token: daemon reports that, exit path only (no network use).
	if err := quiet(func() error { return cmdDaemon(ctx, nil) }); err == nil || !strings.Contains(err.Error(), "no telegram token") {
		t.Errorf("no token: got err %v", err)
	}
}

// capture runs fn with stdout/stderr captured and returns the output.
func capture(fn func() error) (string, error) {
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, _ := os.Pipe()
	os.Stdout, os.Stderr = w, w
	err := fn()
	_ = w.Close()
	out, _ := io.ReadAll(r)
	os.Stdout, os.Stderr = oldOut, oldErr
	return string(out), err
}

// TestCmdConfigSetProviderCaseInsensitive (P2-42): set-provider and
// set-api-key accept mixed-case provider names like the rest of the
// config layer, and store the canonical lowercase name.
func TestCmdConfigSetProviderCaseInsensitive(t *testing.T) {
	dir := scratchHome(t)
	if err := quiet(func() error { return cmdConfig([]string{"set-api-key", "OpenAI", "test-placeholder-key"}) }); err != nil {
		t.Fatalf("set-api-key OpenAI: %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"set-provider", "OpenAI"}) }); err != nil {
		t.Fatalf("set-provider OpenAI: %v", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"show"}) }); err != nil {
		t.Fatalf("show after mixed-case switch: %v", err)
	}
	raw, err := os.ReadFile(dir + "/.orion/config.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"default_provider": "openai"`) {
		t.Errorf("stored config should use canonical lowercase provider, got:\n%s", raw)
	}
}

// TestCmdConfigHomeUnset (P2-40): with HOME unset, mutating config
// commands (and config show) must fail with a clear home-directory
// error instead of writing config.json into the working directory.
func TestCmdConfigHomeUnset(t *testing.T) {
	scratchHome(t)
	t.Setenv("HOME", "")
	if err := quiet(func() error { return cmdConfig([]string{"set-provider", "ollama"}) }); err == nil || !strings.Contains(err.Error(), "home") {
		t.Errorf("set-provider without HOME: got err %v, want an error mentioning home", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"show"}) }); err == nil || !strings.Contains(err.Error(), "home") {
		t.Errorf("config show without HOME: got err %v, want an error mentioning home", err)
	}
	// Nothing was written into the working directory.
	if _, err := os.Stat("config.json"); !os.IsNotExist(err) {
		t.Errorf("config.json must not be written to the working directory when HOME is unset (stat err: %v)", err)
	}
}

// TestCmdStatusCorruptMemorySurfacesError (P2-39): a broken memory
// store must make status fail with the real error, not print
// "0 facts, 0 recent messages" as if the store were empty.
func TestCmdStatusCorruptMemorySurfacesError(t *testing.T) {
	dir := scratchHome(t)
	if err := os.MkdirAll(dir+"/.orion", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/.orion/memory.json", []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := capture(func() error { return cmdStatus(nil) })
	if err == nil || !strings.Contains(err.Error(), "memory") {
		t.Fatalf("status with corrupt memory: err=%v, want an error mentioning memory", err)
	}
	if strings.Contains(out, "0 facts") {
		t.Errorf("status printed %q despite a broken memory store", out)
	}
}

// TestBuildLocalPartialSkillsDir (P2-38): the shared local wiring logs
// a partially broken skills dir instead of failing local commands (or
// silently swallowing the error): the good skill still loads.
func TestBuildLocalPartialSkillsDir(t *testing.T) {
	dir := scratchHome(t)
	skillDir := dir + "/.orion/skills"
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillDir+"/helper.md", []byte("# Helper\n\nA helper skill.\n\n## Tools\n\n- exec\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Empty .md file: LoadDir reports it, but the good skill loads.
	if err := os.WriteFile(skillDir+"/broken.md", []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := capture(func() error { return cmdSkills(nil) })
	if err != nil {
		t.Fatalf("skills with a partially broken skills dir: %v", err)
	}
	if !strings.Contains(out, "Helper") {
		t.Errorf("skills output should still list the good skill, got %q", out)
	}
}

// --- Telegram allowlist startup enforcement (ChatGPT rc2 audit,
// Finding 3) ---

// writeAllowlistTestConfig writes a config with a fake-but-valid
// token and the local ollama provider, so polling entry points can
// be exercised without any network. allowlist is written as
// telegram_allowed_chats; allowAll as allow_all_telegram_chats.
func writeAllowlistTestConfig(t *testing.T, home string, allowlist string, allowAll bool) {
	t.Helper()
	body := `{"default_provider":"ollama","providers":{"ollama":{"name":"ollama","base_url":"http://localhost:11434","model":"llama3.1"}},"telegram_token":"123456:AAAAAAAAAAAAAAAAAAAA","telegram_allowed_chats":` + allowlist + `,"allow_all_telegram_chats":` + map[bool]string{true: "true", false: "false"}[allowAll] + `,"data_dir":"` + home + `/.orion"}`
	if err := os.MkdirAll(home+"/.orion", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home+"/.orion/config.json", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCmdDaemonEmptyAllowlistRefuses: `orion daemon` with an empty
// allowlist and no opt-in must fail before any network call (the
// refusal happens before getMe), with an actionable error.
func TestCmdDaemonEmptyAllowlistRefuses(t *testing.T) {
	home := scratchHome(t)
	writeAllowlistTestConfig(t, home, `[]`, false)
	_, err := capture(func() error { return cmdDaemon(context.Background(), nil) })
	if err == nil {
		t.Fatal("daemon with empty allowlist: got nil, want a refusal error")
	}
	for _, want := range []string{"telegram_allowed_chats", "allow_all_telegram_chats", "/whoami"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("daemon refusal %q does not mention %q", err.Error(), want)
		}
	}
}

// TestCmdTelegramPollEmptyAllowlistRefuses: `orion telegram poll`
// refuses the same way; `telegram test` (token check only) is
// unaffected because it never starts polling.
func TestCmdTelegramPollEmptyAllowlistRefuses(t *testing.T) {
	home := scratchHome(t)
	writeAllowlistTestConfig(t, home, `[]`, false)
	_, err := capture(func() error { return cmdTelegram(context.Background(), []string{"poll"}) })
	if err == nil {
		t.Fatal("telegram poll with empty allowlist: got nil, want a refusal error")
	}
	if !strings.Contains(err.Error(), "telegram_allowed_chats") {
		t.Errorf("telegram poll refusal %q does not mention telegram_allowed_chats", err.Error())
	}
}

// TestCmdConfigSetAPIKeyFromStdin verifies the "-" form reads the key
// from stdin (Audit 4 F9): the key never appears on the command line,
// so it cannot land in shell history or the process list.
func TestCmdConfigSetAPIKeyFromStdin(t *testing.T) {
	home := scratchHome(t)
	keyFile, err := os.CreateTemp(t.TempDir(), "deepseek-key-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyFile.WriteString("stdin-test-key\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := keyFile.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = keyFile
	t.Cleanup(func() { os.Stdin = oldStdin })
	if err := quiet(func() error { return cmdConfig([]string{"set-api-key", "deepseek", "-"}) }); err != nil {
		t.Fatalf("set-api-key from stdin: %v", err)
	}
	path := home + "/.orion/config.json"
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load saved config: %v", err)
	}
	if got := cfg.Providers["deepseek"].APIKey; got != "stdin-test-key" {
		t.Errorf("stored deepseek key = %q, want %q", got, "stdin-test-key")
	}
	// An empty stdin is an error, not an empty stored key.
	empty, err := os.CreateTemp(t.TempDir(), "empty-key-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = empty
	if err := quiet(func() error { return cmdConfig([]string{"set-api-key", "deepseek", "-"}) }); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Errorf("set-api-key from empty stdin: got err %v, want a must-not-be-empty error", err)
	}
}

// TestApplyTelegramBaseURL verifies ORION_TELEGRAM_BASE_URL is honoured
// by the CLI's Telegram paths (Audit 4 F7), exactly as oriond honours it.
func TestApplyTelegramBaseURL(t *testing.T) {
	t.Setenv("ORION_TELEGRAM_BASE_URL", "http://127.0.0.1:9/fake-bot-api")
	t.Cleanup(func() { messaging.SetDefaultBaseURL("https://api.telegram.org") })
	applyTelegramBaseURL()
	if got := messaging.DefaultBaseURLValue(); got != "http://127.0.0.1:9/fake-bot-api" {
		t.Errorf("DefaultBaseURL = %q, want the ORION_TELEGRAM_BASE_URL value", got)
	}
}
