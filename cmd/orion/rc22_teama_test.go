// F-A16-7 (rc22, Team A): `orion telegram poll` must serve the
// pprof endpoint when config debug.pprof_enabled is on — the same
// wiring as `orion daemon` and oriond — and listen on nothing extra
// when it is off.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestSignalName(t *testing.T) {
	// F-A16-5: the shutdown log must name the signal the way an
	// operator reads it — os.Signal's String() ("terminated")
	// reads like a crash, not a signal.
	for _, tc := range []struct {
		sig  os.Signal
		want string
	}{
		{os.Interrupt, "SIGINT"},
		{syscall.SIGTERM, "SIGTERM"},
		{syscall.SIGHUP, "SIGHUP"},
	} {
		if got := signalName(tc.sig); got != tc.want {
			t.Errorf("signalName(%v) = %q, want %q", tc.sig, got, tc.want)
		}
	}
}

func TestTelegramPollDebugEndpointEnabled(t *testing.T) {
	home := scratchHome(t)
	provider := &rc11FakeProvider{}
	provSrv := httptest.NewServer(provider)
	defer provSrv.Close()
	bot := &rc11FakeBot{}
	botSrv := httptest.NewServer(bot)
	defer botSrv.Close()
	port := rc21FreePort(t)
	rc21WriteDaemonDebugConfig(t, home, provSrv.URL, true, port)
	t.Setenv("ORION_TELEGRAM_BASE_URL", botSrv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		out, err := capture(func() error { return cmdTelegram(ctx, []string{"poll"}) })
		outCh <- out
		errCh <- err
	}()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	status, body := rc21GetUntil(t, base+"/debug/pprof/")
	if status != http.StatusOK {
		t.Fatalf("GET /debug/pprof/: status = %d, want 200 (telegram poll serves no pprof endpoint)", status)
	}
	for _, want := range []string{"goroutine", "heap"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("index body does not mention %q", want)
		}
	}
	status, body = rc21GetUntil(t, base+"/debug/pprof/goroutine?debug=1")
	if status != http.StatusOK {
		t.Fatalf("GET /debug/pprof/goroutine?debug=1: status = %d, want 200", status)
	}
	if !strings.Contains(string(body), "goroutine profile") {
		t.Errorf("goroutine body does not contain %q", "goroutine profile")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("cmdTelegram poll after cancel: err = %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cmdTelegram poll did not return after cancelling its context")
	}
	select {
	case out := <-outCh:
		if !strings.Contains(out, fmt.Sprintf("debug pprof listening on 127.0.0.1:%d", port)) {
			t.Errorf("poll output does not report the pprof listen address; output:\n%s", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("poll output was not delivered")
	}
}

func TestTelegramPollDebugEndpointDisabled(t *testing.T) {
	home := scratchHome(t)
	provider := &rc11FakeProvider{}
	provSrv := httptest.NewServer(provider)
	defer provSrv.Close()
	bot := &rc11FakeBot{}
	// Count getUpdates calls through a wrapper: `telegram poll`
	// never calls getMe, so the first getUpdates is the proof the
	// poll loop is up (and the debug endpoint, which starts before
	// polling, has already made its listen-or-not decision).
	var getUpdates atomic.Int32
	botSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			getUpdates.Add(1)
		}
		bot.ServeHTTP(w, r)
	}))
	defer botSrv.Close()
	// No debug section at all: the off-by-default path.
	rc21WriteDaemonDebugConfig(t, home, provSrv.URL, false, 0)
	t.Setenv("ORION_TELEGRAM_BASE_URL", botSrv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := capture(func() error { return cmdTelegram(ctx, []string{"poll"}) })
		errCh <- err
	}()
	// The poll loop is up once it has called getUpdates; only then
	// is "nothing listens" a meaningful assertion.
	rc11WaitFor(t, "telegram poll getUpdates", func() bool { return getUpdates.Load() > 0 })

	// The default pprof port must refuse: with the endpoint off,
	// telegram poll listens on nothing extra.
	conn, err := net.DialTimeout("tcp", "127.0.0.1:6060", 2*time.Second)
	if err == nil {
		_ = conn.Close()
		t.Errorf("dial 127.0.0.1:6060 succeeded with the endpoint disabled — something is listening on the default pprof port")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("cmdTelegram poll after cancel: err = %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cmdTelegram poll did not return after cancelling its context")
	}
}
