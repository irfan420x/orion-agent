package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// F-A18-5 (rc24): `orion daemon` and `orion telegram poll` take the
// same instance lock oriond holds (<dataDir>/oriond.lock), so a
// second CLI poller refuses with exit 2 instead of drawing a
// Telegram 409 and flapping the good poller.

// TestAcquireInstanceLockSecondFails: the flock guard — a second
// holder gets errAlreadyRunning (exit 2), and releasing the first
// lock lets the next instance start (a crash can never leave a stale
// lock behind: flock dies with the fd). A lock failure that is not
// "already locked" (empty data dir) is a different error and exits 1.
func TestAcquireInstanceLockSecondFails(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	second, err := acquireInstanceLock(dir)
	if err == nil {
		_ = second.Close()
		_ = first.Close()
		t.Fatal("second lock: got nil, want an error (already running)")
	}
	if !errors.Is(err, errAlreadyRunning) {
		_ = first.Close()
		t.Fatalf("second lock error = %q, want it to wrap errAlreadyRunning", err.Error())
	}
	if code := exitCodeFor(err); code != 2 {
		_ = first.Close()
		t.Errorf("second lock: exit code %d, want 2", code)
	}
	for _, want := range []string{"already running", "stop the other"} {
		if !strings.Contains(err.Error(), want) {
			_ = first.Close()
			t.Errorf("second lock error = %q, want it to say %q", err.Error(), want)
		}
	}
	// Release: the next instance must be able to start.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	defer next.Close()
	fi, err := os.Stat(dir + "/oriond.lock")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("lock file mode = %o, want 0600", fi.Mode().Perm())
	}
	if _, err := acquireInstanceLock(""); err == nil {
		t.Error("empty data dir: got nil, want an error")
	} else if errors.Is(err, errAlreadyRunning) {
		t.Errorf("empty data dir error = %q, want it NOT to wrap errAlreadyRunning", err.Error())
	} else if code := exitCodeFor(err); code != 1 {
		t.Errorf("empty data dir: exit code %d, want 1", code)
	}
}

// TestSecondCLIPollerExits2: with one `orion daemon` polling over a
// data dir, a second `orion daemon` AND a second `orion telegram
// poll` over the same dir must both refuse fast with
// errAlreadyRunning (exit 2) — before getMe, before polling — and
// the first daemon must be undisturbed (it still stops cleanly on
// context cancel afterwards). The first daemon runs uncaptured: the
// second calls must not swap os.Stdout/os.Stderr while it is still
// writing to them (the rc23 oriond test's race warning).
func TestSecondCLIPollerExits2(t *testing.T) {
	home := scratchHome(t)
	provider := &rc11FakeProvider{}
	provSrv := httptest.NewServer(provider)
	defer provSrv.Close()
	bot := &rc11FakeBot{}
	botSrv := httptest.NewServer(bot)
	defer botSrv.Close()
	rc11WriteDaemonTestConfig(t, home, provSrv.URL)
	t.Setenv("ORION_TELEGRAM_BASE_URL", botSrv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- cmdDaemon(ctx, nil)
	}()
	// Wait until the first daemon has passed getMe: the lock is
	// taken before getMe, so a getMe proves it is polling.
	deadline := time.Now().Add(30 * time.Second)
	for rc21BotGetMe(bot) == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("first daemon did not call getMe within 30s")
		}
		select {
		case err := <-errCh:
			t.Fatalf("first daemon exited early with err %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if _, err := os.Stat(home + "/.orion/oriond.lock"); err != nil {
		t.Errorf("first daemon did not create the instance lock: %v", err)
	}
	// Second `orion daemon`: refuses, exit 2.
	if err := cmdDaemon(context.Background(), nil); err == nil {
		t.Error("second daemon: got nil, want an already-running refusal")
	} else {
		if !errors.Is(err, errAlreadyRunning) {
			t.Errorf("second daemon error = %q, want it to wrap errAlreadyRunning", err.Error())
		}
		if code := exitCodeFor(err); code != 2 {
			t.Errorf("second daemon: exit code %d, want 2", code)
		}
		for _, want := range []string{"already running", "stop the other"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("second daemon error = %q, want it to say %q", err.Error(), want)
			}
		}
	}
	// Second `orion telegram poll`: refuses the same way — the two
	// entrypoints share the one lock file, so neither can flap the
	// daemon (or each other).
	if err := cmdTelegram(context.Background(), []string{"poll"}); err == nil {
		t.Error("second telegram poll: got nil, want an already-running refusal")
	} else {
		if !errors.Is(err, errAlreadyRunning) {
			t.Errorf("second telegram poll error = %q, want it to wrap errAlreadyRunning", err.Error())
		}
		if code := exitCodeFor(err); code != 2 {
			t.Errorf("second telegram poll: exit code %d, want 2", code)
		}
	}
	// First daemon undisturbed: stops cleanly on cancel.
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("first daemon after cancel: err = %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("first daemon did not return after cancelling its context")
	}
}
