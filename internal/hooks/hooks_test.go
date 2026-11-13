package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeHook writes an executable hook script under dir/<event>/name
// and returns its path. The script is /bin/sh.
func writeHook(t *testing.T, dir string, ev Event, name, body string) string {
	t.Helper()
	d := filepath.Join(dir, "hooks", string(ev))
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatalf("mkdir hooks dir: %v", err)
	}
	p := filepath.Join(d, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write hook %s: %v", p, err)
	}
	return p
}

func TestNewBusEmptyDirHasNoHooks(t *testing.T) {
	b := NewBus(t.TempDir())
	if b.HasHooks() {
		t.Error("empty data dir: HasHooks true, want false")
	}
	if len(b.LoadErrors()) != 0 {
		t.Errorf("empty data dir: LoadErrors = %v, want none", b.LoadErrors())
	}
	if out := b.PreToolUse(context.Background(), "calculator", json.RawMessage(`{}`)); out.Blocked || out.Warning != "" {
		t.Errorf("empty bus PreToolUse = %+v, want continue", out)
	}
	if w := b.PostToolUse(context.Background(), "calculator", json.RawMessage(`{}`), "ok"); w != "" {
		t.Errorf("empty bus PostToolUse warning = %q, want empty", w)
	}
	if w := b.Stop(context.Background(), "completed"); w != "" {
		t.Errorf("empty bus Stop warning = %q, want empty", w)
	}
}

func TestNilBusContinues(t *testing.T) {
	var b *Bus
	if out := b.PreToolUse(context.Background(), "calculator", json.RawMessage(`{}`)); out.Blocked {
		t.Error("nil bus PreToolUse blocked, want continue")
	}
	if b.Count(EventPreToolUse) != 0 || b.HasHooks() {
		t.Error("nil bus reports hooks")
	}
}

func TestPreToolUseExitZeroContinues(t *testing.T) {
	dir := t.TempDir()
	writeHook(t, dir, EventPreToolUse, "allow.sh", "exit 0")
	b := NewBus(dir)
	if b.Count(EventPreToolUse) != 1 {
		t.Fatalf("loaded %d PreToolUse hooks, want 1 (load errs: %v)", b.Count(EventPreToolUse), b.LoadErrors())
	}
	if out := b.PreToolUse(context.Background(), "calculator", json.RawMessage(`{"a":1}`)); out.Blocked || out.Warning != "" {
		t.Errorf("exit-0 hook: %+v, want continue", out)
	}
}

func TestPreToolUseExitTwoBlocksWithReason(t *testing.T) {
	dir := t.TempDir()
	// Block with a JSON decision + reason on stdout.
	writeHook(t, dir, EventPreToolUse, "block.sh", `echo '{"decision":"block","reason":"no shell for you"}'; exit 2`)
	b := NewBus(dir)
	out := b.PreToolUse(context.Background(), "shell_exec", json.RawMessage(`{"command":"id"}`))
	if !out.Blocked {
		t.Fatal("exit-2 hook did not block")
	}
	if out.Reason != "no shell for you" {
		t.Errorf("block reason = %q, want %q", out.Reason, "no shell for you")
	}
}

func TestPreToolUseExitTwoWithoutJSONUsesStderr(t *testing.T) {
	dir := t.TempDir()
	writeHook(t, dir, EventPreToolUse, "block.sh", `echo "forbidden by policy" >&2; exit 2`)
	b := NewBus(dir)
	out := b.PreToolUse(context.Background(), "shell_exec", json.RawMessage(`{}`))
	if !out.Blocked {
		t.Fatal("exit-2 hook did not block")
	}
	if !strings.Contains(out.Reason, "forbidden by policy") {
		t.Errorf("block reason = %q, want stderr text", out.Reason)
	}
}

func TestPreToolUseExitZeroJSONBlockHonoured(t *testing.T) {
	dir := t.TempDir()
	writeHook(t, dir, EventPreToolUse, "block.sh", `echo '{"decision":"block","reason":"json says no"}'; exit 0`)
	b := NewBus(dir)
	out := b.PreToolUse(context.Background(), "file_write", json.RawMessage(`{}`))
	if !out.Blocked || out.Reason != "json says no" {
		t.Errorf("exit-0 JSON block: %+v, want blocked with reason", out)
	}
}

func TestPreToolUseOtherExitContinuesWithWarning(t *testing.T) {
	dir := t.TempDir()
	writeHook(t, dir, EventPreToolUse, "warn.sh", `echo "something odd" >&2; exit 3`)
	b := NewBus(dir)
	out := b.PreToolUse(context.Background(), "calculator", json.RawMessage(`{}`))
	if out.Blocked {
		t.Error("exit-3 hook blocked, want continue-with-warning")
	}
	if !strings.Contains(out.Warning, "warn.sh") || !strings.Contains(out.Warning, "3") {
		t.Errorf("warning = %q, want hook name and exit code", out.Warning)
	}
}

func TestPreToolUseCrashFailsClosed(t *testing.T) {
	dir := t.TempDir()
	// Exit 1 is a clean exit (warning path); a real crash — killed
	// by a signal — must fail closed. `kill -9 $$` dies by signal
	// (SIGKILL) with no exit code.
	writeHook(t, dir, EventPreToolUse, "crash.sh", "kill -9 $$")
	b := NewBus(dir)
	out := b.PreToolUse(context.Background(), "calculator", json.RawMessage(`{}`))
	if !out.Blocked {
		t.Fatalf("signal-killed hook did not block (fail closed): %+v", out)
	}
	if !strings.Contains(out.Reason, "failing closed") {
		t.Errorf("fail-closed reason = %q, want 'failing closed'", out.Reason)
	}
}

func TestPreToolUseTimeoutFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeHook(t, dir, EventPreToolUse, "slow.sh", "sleep 30")
	b := NewBus(dir)
	b.SetTimeout(100 * time.Millisecond)
	out := b.PreToolUse(context.Background(), "calculator", json.RawMessage(`{}`))
	if !out.Blocked {
		t.Fatal("timed-out hook did not block (fail closed)")
	}
}

func TestPreToolUseFirstBlockShortCircuits(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "second-ran")
	writeHook(t, dir, EventPreToolUse, "01-block.sh", `echo '{"decision":"block","reason":"first"}'; exit 2`)
	writeHook(t, dir, EventPreToolUse, "02-second.sh", "touch "+marker+"; exit 0")
	b := NewBus(dir)
	out := b.PreToolUse(context.Background(), "calculator", json.RawMessage(`{}`))
	if !out.Blocked || out.Reason != "first" {
		t.Fatalf("first-block outcome = %+v", out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("second hook ran after the first blocked; want short-circuit")
	}
}

func TestPreToolUsePayloadIsJSONOnStdin(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "payload.json")
	writeHook(t, dir, EventPreToolUse, "see.sh", "cat > "+seen)
	b := NewBus(dir)
	b.PreToolUse(context.Background(), "shell_exec", json.RawMessage(`{"command":"id"}`))
	data, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("hook did not receive stdin: %v", err)
	}
	var p preToolUsePayload
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("payload is not JSON: %v (%s)", err, data)
	}
	if p.Event != "PreToolUse" || p.ToolName != "shell_exec" {
		t.Errorf("payload = %+v, want PreToolUse/shell_exec", p)
	}
	var args map[string]any
	if err := json.Unmarshal(p.Arguments, &args); err != nil || args["command"] != "id" {
		t.Errorf("payload arguments = %s, want command id", p.Arguments)
	}
}

func TestHookEnvIsScrubbed(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "env.txt")
	writeHook(t, dir, EventPreToolUse, "env.sh", "env > "+seen+"; exit 0")
	t.Setenv("ORION_TEST_HOOK_SECRET_API_KEY", "sk-should-never-appear")
	t.Setenv("MY_TELEGRAM_TOKEN", "123456:should-never-appear")
	t.Setenv("HOOK_TEST_PLAIN", "keep-me")
	b := NewBus(dir)
	b.PreToolUse(context.Background(), "calculator", json.RawMessage(`{}`))
	data, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("hook env not captured: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "sk-should-never-appear") || strings.Contains(text, "should-never-appear") {
		t.Errorf("hook env leaked a secret:\n%s", text)
	}
	if strings.Contains(text, "ORION_TEST_HOOK_SECRET_API_KEY") || strings.Contains(text, "MY_TELEGRAM_TOKEN") {
		t.Errorf("hook env leaked a secret variable name:\n%s", text)
	}
	if !strings.Contains(text, "HOOK_TEST_PLAIN=keep-me") {
		t.Errorf("hook env dropped a plain variable:\n%s", text)
	}
	if !strings.Contains(text, "ORION_HOOK_EVENT=PreToolUse") || !strings.Contains(text, "ORION_TOOL_NAME=calculator") {
		t.Errorf("hook env missing Orion-set variables:\n%s", text)
	}
}

func TestPostToolUseRunsAndNeverBlocks(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "post-ran")
	writeHook(t, dir, EventPostToolUse, "post.sh", "echo \"$ORION_TOOL_NAME\" > "+marker)
	b := NewBus(dir)
	if w := b.PostToolUse(context.Background(), "calculator", json.RawMessage(`{"a":1}`), "42"); w != "" {
		t.Errorf("clean post hook warning = %q, want empty", w)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("post hook did not run: %v", err)
	}
	if strings.TrimSpace(string(data)) != "calculator" {
		t.Errorf("post hook saw tool %q, want calculator", strings.TrimSpace(string(data)))
	}
}

func TestPostToolUseFailureIsWarningOnly(t *testing.T) {
	dir := t.TempDir()
	writeHook(t, dir, EventPostToolUse, "boom.sh", "exit 1")
	b := NewBus(dir)
	if w := b.PostToolUse(context.Background(), "calculator", json.RawMessage(`{}`), "42"); w == "" {
		t.Error("failing post hook returned no warning")
	}
}

func TestStopHookRunsAndCannotBlock(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "stop-ran")
	writeHook(t, dir, EventStop, "stop.sh", "echo done > "+marker+"; exit 2")
	b := NewBus(dir)
	w := b.Stop(context.Background(), "completed")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("stop hook did not run: %v", err)
	}
	if !strings.Contains(w, "does not support stop prevention") {
		t.Errorf("stop-block warning = %q, want the stop-prevention note", w)
	}
}

func TestNonExecutableHookSkipped(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "hooks", "PreToolUse")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "plain.sh"), []byte("#!/bin/sh\nexit 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := NewBus(dir)
	if b.Count(EventPreToolUse) != 0 {
		t.Error("non-executable hook loaded; want skipped")
	}
	if len(b.LoadErrors()) == 0 {
		t.Error("non-executable hook produced no load error")
	}
	if out := b.PreToolUse(context.Background(), "calculator", json.RawMessage(`{}`)); out.Blocked {
		t.Error("skipped hook blocked a call")
	}
}

func TestSymlinkEscapingHooksDirSkipped(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside.sh")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(dir, "hooks", "PreToolUse")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(d, "evil.sh")); err != nil {
		t.Fatal(err)
	}
	b := NewBus(dir)
	if b.Count(EventPreToolUse) != 0 {
		t.Error("symlink escaping the hooks dir loaded; want skipped")
	}
	if len(b.LoadErrors()) == 0 {
		t.Error("escaping symlink produced no load error")
	}
}

func TestHooksNeverLoadFromCwd(t *testing.T) {
	// A hook sitting in the working directory (e.g. a cloned repo)
	// must never be picked up: the bus only ever reads <dataDir>/hooks.
	work := t.TempDir()
	t.Chdir(work)
	if err := os.WriteFile(filepath.Join(work, "PreToolUse"), []byte("#!/bin/sh\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "hooks", "PreToolUse"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "hooks", "PreToolUse", "evil.sh"), []byte("#!/bin/sh\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	b := NewBus(t.TempDir()) // a different, empty data dir
	if b.HasHooks() {
		t.Error("hooks loaded from the working directory; want data-dir only")
	}
	if out := b.PreToolUse(context.Background(), "calculator", json.RawMessage(`{}`)); out.Blocked {
		t.Error("cwd hook blocked a call")
	}
}

func TestHookReasonIsRedacted(t *testing.T) {
	dir := t.TempDir()
	// A hook that prints something shaped like a secret in its
	// block reason must not get it through to the model.
	writeHook(t, dir, EventPreToolUse, "leak.sh", `echo '{"decision":"block","reason":"key is sk-0123456789abcdef0123456789abcdef0123456789abcdef"}'; exit 2`)
	b := NewBus(dir)
	out := b.PreToolUse(context.Background(), "calculator", json.RawMessage(`{}`))
	if !out.Blocked {
		t.Fatal("leak hook did not block")
	}
	if strings.Contains(out.Reason, "sk-0123456789abcdef0123456789abcdef0123456789abcdef") {
		t.Errorf("block reason leaked a secret-shaped value: %q", out.Reason)
	}
}

func TestHooksRunInFilenameOrder(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "order.log")
	writeHook(t, dir, EventPostToolUse, "b-second.sh", "echo second >> "+log)
	writeHook(t, dir, EventPostToolUse, "a-first.sh", "echo first >> "+log)
	b := NewBus(dir)
	b.PostToolUse(context.Background(), "calculator", json.RawMessage(`{}`), "42")
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("order log missing: %v", err)
	}
	if string(data) != "first\nsecond\n" {
		t.Errorf("hook order = %q, want first then second", string(data))
	}
}
