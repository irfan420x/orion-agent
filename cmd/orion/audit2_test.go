package main

// Audit2 tests (fresh-eyes audit 2026-11-13): clean Ctrl-C exit code,
// telegram test decoupled from the full app, set-model validation,
// usage routing.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestExitCodeFor(t *testing.T) {
	if got := exitCodeFor(nil); got != 0 {
		t.Errorf("exitCodeFor(nil) = %d, want 0", got)
	}
	if got := exitCodeFor(context.Canceled); got != 0 {
		t.Errorf("exitCodeFor(context.Canceled) = %d, want 0 (Ctrl-C is a clean exit)", got)
	}
	if got := exitCodeFor(fmt.Errorf("interrupted: %w", context.Canceled)); got != 0 {
		t.Errorf("exitCodeFor(wrapped Canceled) = %d, want 0", got)
	}
	if got := exitCodeFor(errors.New("boom")); got != 1 {
		t.Errorf("exitCodeFor(boom) = %d, want 1", got)
	}
	if got := exitCodeFor(context.DeadlineExceeded); got != 1 {
		t.Errorf("exitCodeFor(DeadlineExceeded) = %d, want 1 (a timeout is still an error)", got)
	}
}

// TestCmdTelegramTestSkipsProvider: token verification needs config +
// token only. With a corrupt memory store (which breaks the full app
// build) and no token configured, `telegram test` must report the
// missing token — not a memory error — proving it never builds the
// provider/agent/memory stack.
func TestCmdTelegramTestSkipsProvider(t *testing.T) {
	dir := scratchHome(t)
	if err := os.MkdirAll(dir+"/.orion", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/.orion/memory.json", []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := quiet(func() error { return cmdTelegram(context.Background(), []string{"test"}) })
	if err == nil || !strings.Contains(err.Error(), "no token configured") {
		t.Fatalf("telegram test with corrupt memory and no token: err=%v, want an error about the missing token (not memory)", err)
	}
	if strings.Contains(err.Error(), "memory") {
		t.Fatalf("telegram test with corrupt memory: err=%v, want no mention of memory", err)
	}
}

func TestCmdConfigSetModelRejectsBlank(t *testing.T) {
	scratchHome(t)
	if err := quiet(func() error { return cmdConfig([]string{"set-model", "   "}) }); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Errorf("set-model blank: got err %v, want an error about an empty model name", err)
	}
	if err := quiet(func() error { return cmdConfig([]string{"set-model", "	"}) }); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Errorf("set-model tab-only: got err %v, want an error about an empty model name", err)
	}
}

func TestUsageToWritesToWriter(t *testing.T) {
	var buf bytes.Buffer
	usageTo(&buf)
	got := buf.String()
	for _, want := range []string{"orion run", "orion chat", "orion telegram test", "orion version"} {
		if !strings.Contains(got, want) {
			t.Errorf("usageTo output missing %q:\n%s", want, got)
		}
	}
}
