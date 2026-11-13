package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/hooks"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// --- Hook bus integration tests (rc9, audit P0 #6) ----------------------
//
// These prove the wiring, not the bus itself (see internal/hooks):
// a blocking PreToolUse hook must prevent the tool from executing,
// an allowing hook must let it run, and PostToolUse/Stop hooks must
// fire exactly once per turn — including turns that end in error.

// writeHookFile writes an executable /bin/sh hook under
// dir/hooks/<event>/name.
func writeHookFile(t *testing.T, dir string, ev hooks.Event, name, body string) string {
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

// hookAgent builds an agent whose provider issues one call to the
// stub tool on the first turn and answers "done" on the second.
func hookAgent(t *testing.T, stub *gateStubTool, bus *hooks.Bus) (*Agent, *fakeStore) {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatal(err)
	}
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("call-1", stub.Name(), `{}`), nil
			}
			return textResponse("done"), nil
		},
	}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(5), prov, reg, mem)
	a.SetHooks(bus)
	return a, mem
}

func TestHooksPreToolUseBlocksToolCall(t *testing.T) {
	dir := t.TempDir()
	writeHookFile(t, dir, hooks.EventPreToolUse, "block.sh", `echo '{"decision":"block","reason":"no echo for you"}'; exit 2`)
	bus := hooks.NewBus(dir)
	stub := &gateStubTool{name: "echo"}
	a, mem := hookAgent(t, stub, bus)
	out, err := a.Run(context.Background(), "run echo")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "done" {
		t.Fatalf("Run = %q, want %q (the loop must continue after a hook block, not crash)", out, "done")
	}
	if got := stub.callCount(); got != 0 {
		t.Errorf("echo executions = %d, want 0 (blocked before Execute)", got)
	}
	// The block must reach the model as a tool result, worded as a
	// block, so it can explain instead of retrying blindly.
	found := false
	for _, m := range mem.allMessages() {
		if m.Role == "tool" && strings.Contains(m.Content, "blocked by hook") && strings.Contains(m.Content, "no echo for you") {
			found = true
		}
	}
	if !found {
		t.Errorf("no 'blocked by hook' tool result in memory: %+v", mem.allMessages())
	}
}

func TestHooksPreToolUseAllowRunsTool(t *testing.T) {
	dir := t.TempDir()
	writeHookFile(t, dir, hooks.EventPreToolUse, "allow.sh", "exit 0")
	bus := hooks.NewBus(dir)
	stub := &gateStubTool{name: "echo"}
	a, _ := hookAgent(t, stub, bus)
	out, err := a.Run(context.Background(), "run echo")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "done" {
		t.Fatalf("Run = %q, want %q", out, "done")
	}
	if got := stub.callCount(); got != 1 {
		t.Errorf("echo executions = %d, want 1 (allowing hook lets it run)", got)
	}
}

func TestHooksPostToolUseAndStopFire(t *testing.T) {
	dir := t.TempDir()
	postMarker := filepath.Join(dir, "post-ran")
	stopMarker := filepath.Join(dir, "stop-ran")
	writeHookFile(t, dir, hooks.EventPostToolUse, "post.sh", "touch "+postMarker)
	writeHookFile(t, dir, hooks.EventStop, "stop.sh", "touch "+stopMarker)
	bus := hooks.NewBus(dir)
	stub := &gateStubTool{name: "echo"}
	a, _ := hookAgent(t, stub, bus)
	if _, err := a.Run(context.Background(), "run echo"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(postMarker); err != nil {
		t.Errorf("PostToolUse hook did not run: %v", err)
	}
	if _, err := os.Stat(stopMarker); err != nil {
		t.Errorf("Stop hook did not run: %v", err)
	}
	if got := stub.callCount(); got != 1 {
		t.Errorf("echo executions = %d, want 1", got)
	}
}

func TestHooksStopFiresOnErrorTurn(t *testing.T) {
	dir := t.TempDir()
	stopMarker := filepath.Join(dir, "stop-ran")
	stopPayload := filepath.Join(dir, "stop-payload.json")
	writeHookFile(t, dir, hooks.EventStop, "stop.sh", "cat > "+stopPayload+"; touch "+stopMarker)
	bus := hooks.NewBus(dir)
	reg := tools.NewRegistry()
	prov2 := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			return nil, context.DeadlineExceeded
		},
	}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(5), prov2, reg, mem)
	a.SetHooks(bus)
	_, err := a.Run(context.Background(), "boom")
	if err == nil {
		t.Fatal("Run succeeded, want a provider error")
	}
	if _, err := os.Stat(stopMarker); err != nil {
		t.Errorf("Stop hook did not run on an error turn: %v", err)
	}
	data, err := os.ReadFile(stopPayload)
	if err != nil {
		t.Fatalf("Stop hook received no payload: %v", err)
	}
	if !strings.Contains(string(data), `"event":"Stop"`) || !strings.Contains(string(data), "error:") {
		t.Errorf("Stop payload = %s, want event Stop with an error reason", data)
	}
}
