// F-A3-2 (rc21): the debug/pprof endpoint must serve the
// standard pprof profiles when enabled, listen on nothing when
// disabled, bind loopback-only, and never serve a secret.
package debug

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// freePort returns a currently-free loopback port: listen on
// 127.0.0.1:0, read the assigned port, close. The caller binds
// it immediately after, so the race window is negligible in
// tests (nothing else on this machine binds random ports).
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("freePort: close: %v", err)
	}
	return port
}

// getUntil polls url until it returns 200 or the deadline passes,
// and returns the status and body of the last attempt. pprof
// requests are served from the moment Start returns, but the
// Serve goroutine starts a hair after Listen, so a single GET
// could race it — poll, don't assert once.
func getUntil(t *testing.T, url string) (int, []byte) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastStatus int
	var lastBody []byte
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			lastStatus, lastBody = resp.StatusCode, body
			if resp.StatusCode == http.StatusOK {
				return lastStatus, lastBody
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return lastStatus, lastBody
}

// getOnce is a single GET with a short client timeout: used where
// the endpoint must NOT answer (disabled/closed), so a hanging
// read would be a failure, not a slow server.
func getOnce(t *testing.T, url string) (*http.Response, []byte, error) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp, body, err
}

func TestStartDisabledReturnsNil(t *testing.T) {
	port := freePort(t)
	srv, err := Start(Config{Enabled: false, Port: port})
	if err != nil {
		t.Fatalf("Start(disabled): err = %v, want nil", err)
	}
	if srv != nil {
		defer srv.Close()
		t.Fatalf("Start(disabled): server = %+v, want nil (no listener)", srv)
	}
	// Nothing listens on the port: we can bind it ourselves.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("Start(disabled): port %d is in use (%v) — disabled Start must not listen", port, err)
	}
	_ = ln.Close()
}

func TestStartEnabledServesPprof(t *testing.T) {
	srv, err := Start(Config{Enabled: true, Port: 0})
	if err != nil {
		t.Fatalf("Start(enabled): %v", err)
	}
	defer func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	base := "http://" + srv.Addr

	status, body := getUntil(t, base+"/debug/pprof/")
	if status != http.StatusOK {
		t.Fatalf("GET /debug/pprof/: status = %d, want 200", status)
	}
	for _, want := range []string{"goroutine", "heap"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("index body does not mention %q:\n%s", want, body)
		}
	}

	status, body = getUntil(t, base+"/debug/pprof/goroutine?debug=1")
	if status != http.StatusOK {
		t.Fatalf("GET /debug/pprof/goroutine?debug=1: status = %d, want 200", status)
	}
	if !strings.Contains(string(body), "goroutine profile") {
		t.Errorf("goroutine body does not contain %q:\n%.500s", "goroutine profile", body)
	}

	status, _ = getUntil(t, base+"/debug/pprof/heap")
	if status != http.StatusOK {
		t.Fatalf("GET /debug/pprof/heap: status = %d, want 200", status)
	}

	status, _ = getUntil(t, base+"/debug/pprof/cmdline")
	if status != http.StatusOK {
		t.Fatalf("GET /debug/pprof/cmdline: status = %d, want 200", status)
	}

	// Only pprof paths are served: anything else is 404, so no
	// other handler (and no config/secret dump) can hide here.
	resp, _, err := getOnce(t, base+"/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /: status = %d, want 404 (only /debug/pprof/ is served)", resp.StatusCode)
	}
}

func TestBindIsLoopbackOnly(t *testing.T) {
	srv, err := Start(Config{Enabled: true, Port: 0})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()
	if strings.HasPrefix(srv.Addr, "0.0.0.0") {
		t.Fatalf("Addr = %q, must never be 0.0.0.0", srv.Addr)
	}
	host, _, err := net.SplitHostPort(srv.Addr)
	if err != nil {
		t.Fatalf("Addr %q: SplitHostPort: %v", srv.Addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		t.Fatalf("Addr host %q is not an IP", host)
	}
	if !ip.IsLoopback() {
		t.Fatalf("Addr = %q, want a loopback address", srv.Addr)
	}
	if ip.IsUnspecified() {
		t.Fatalf("Addr = %q, must never be the unspecified address", srv.Addr)
	}
	// A listener on any other interface would answer on an
	// external address too: the loopback address above is the
	// only one Start ever binds.
}

func TestInvalidPortRejected(t *testing.T) {
	for _, port := range []int{-1, -6060, 65536, 70000} {
		srv, err := Start(Config{Enabled: true, Port: port})
		if err == nil {
			if srv != nil {
				_ = srv.Close()
			}
			t.Errorf("Start(port %d): err = nil, want an error", port)
		}
		if srv != nil {
			_ = srv.Close()
			t.Errorf("Start(port %d): server = %+v, want nil on error", port, srv)
		}
	}
}

func TestStartTwiceSamePort(t *testing.T) {
	port := freePort(t)
	first, err := Start(Config{Enabled: true, Port: port})
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer first.Close()
	second, err := Start(Config{Enabled: true, Port: port})
	if err == nil {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second Start on port %d: err = nil, want a bind error", port)
	}
	if second != nil {
		_ = second.Close()
		t.Errorf("second Start on port %d: server = %+v, want nil on error", port, second)
	}
}

// testSecret is a stand-in for a real secret (API key, Telegram
// token): heap-allocated and reachable for the whole test, the
// way the daemon holds its token and keys in the loaded config.
// The pprof profile formats carry stack traces and sizes, not
// object contents and not a config dump, so no served output may
// contain it. If a future handler ever dumps config or memory,
// this test catches it.
var testSecretHolder = map[string]string{}

func TestNoSecretInServedOutput(t *testing.T) {
	secret := strings.Repeat("x", 8) + "orion-debug-test-secret-9f2c4e"
	testSecretHolder["token"] = secret
	defer delete(testSecretHolder, "token")

	srv, err := Start(Config{Enabled: true, Port: 0})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()
	base := "http://" + srv.Addr

	for _, path := range []string{
		"/debug/pprof/",
		"/debug/pprof/goroutine?debug=1",
		"/debug/pprof/heap",
		"/debug/pprof/cmdline",
		"/debug/pprof/heap?debug=1",
	} {
		status, body := getUntil(t, base+path)
		if status != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200", path, status)
		}
		if bytes.Contains(body, []byte(secret)) {
			t.Errorf("GET %s: served output contains the secret", path)
		}
	}
}

// F-A16-8 (rc22): the debug server must carry explicit
// Read/Write/Idle timeouts, and the write timeout must leave a
// capped profile room to finish (WriteTimeout > MaxProfileSeconds).
func TestServerTimeoutsAreSet(t *testing.T) {
	srv, err := Start(Config{Enabled: true, Port: 0})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()
	if srv.srv.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 5s", srv.srv.ReadHeaderTimeout)
	}
	if srv.srv.ReadTimeout != ServerReadTimeout {
		t.Errorf("ReadTimeout = %v, want ServerReadTimeout %v", srv.srv.ReadTimeout, ServerReadTimeout)
	}
	if srv.srv.WriteTimeout != ServerWriteTimeout {
		t.Errorf("WriteTimeout = %v, want ServerWriteTimeout %v", srv.srv.WriteTimeout, ServerWriteTimeout)
	}
	if srv.srv.IdleTimeout != ServerIdleTimeout {
		t.Errorf("IdleTimeout = %v, want ServerIdleTimeout %v", srv.srv.IdleTimeout, ServerIdleTimeout)
	}
	// The cap is what bounds the expensive work, so the write
	// timeout must clear it with headroom: a capped profile
	// takes MaxProfileSeconds, and pprof extends the write
	// deadline by WriteTimeout + seconds on top of that.
	if ServerWriteTimeout <= time.Duration(MaxProfileSeconds)*time.Second {
		t.Errorf("ServerWriteTimeout %v must exceed MaxProfileSeconds %ds, or a capped profile could be cut off mid-write", ServerWriteTimeout, MaxProfileSeconds)
	}
	if ServerReadTimeout <= 0 || ServerIdleTimeout <= 0 {
		t.Errorf("timeouts must be positive: Read %v Idle %v", ServerReadTimeout, ServerIdleTimeout)
	}
}

// F-A16-8 (rc22): the profile/trace `seconds` parameter is
// clamped to MaxProfileSeconds. Tested against the wrapper
// directly with a recording handler, so a ?seconds=3600 case
// does not need a 30s live request to prove the clamp.
func TestCapProfileSeconds(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"3600", "30"}, // an hour-long profile request is clamped
		{"300", "30"},
		{"31", "30"},
		{"30.5", "30"}, // fractional seconds above the cap too
		{"30", "30"},   // the default / common case is untouched
		{"5", "5"},
		{"1", "1"},
		{"0", "0"},     // invalid for pprof: left for the stdlib default
		{"-5", "-5"},   // same
		{"abc", "abc"}, // unparsable: left for the stdlib default
		{"", ""},       // missing: left for the stdlib default
	}
	for _, tc := range cases {
		var got string
		recorder := func(w http.ResponseWriter, r *http.Request) {
			got = r.FormValue("seconds")
			w.WriteHeader(http.StatusOK)
		}
		url := "/debug/pprof/profile"
		if tc.in != "" {
			url += "?seconds=" + tc.in
		}
		req := httptest.NewRequest(http.MethodGet, url, nil)
		rr := httptest.NewRecorder()
		capProfileSeconds(recorder).ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("seconds=%q: status = %d, want 200", tc.in, rr.Code)
		}
		if got != tc.want {
			t.Errorf("seconds=%q: handler saw %q, want %q", tc.in, got, tc.want)
		}
	}
}

// F-A16-8 (rc22): with the timeouts and the cap in place, a
// normal short profile and trace still complete end to end.
func TestProfileAndTraceComplete(t *testing.T) {
	srv, err := Start(Config{Enabled: true, Port: 0})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()
	base := "http://" + srv.Addr

	status, body := getUntil(t, base+"/debug/pprof/profile?seconds=1")
	if status != http.StatusOK {
		t.Fatalf("GET /debug/pprof/profile?seconds=1: status = %d, want 200", status)
	}
	if len(body) == 0 {
		t.Errorf("GET /debug/pprof/profile?seconds=1: empty body, want a CPU profile")
	}

	status, _ = getUntil(t, base+"/debug/pprof/trace?seconds=1")
	if status != http.StatusOK {
		t.Fatalf("GET /debug/pprof/trace?seconds=1: status = %d, want 200", status)
	}
}

func TestCloseStopsListening(t *testing.T) {
	srv, err := Start(Config{Enabled: true, Port: 0})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	base := "http://" + srv.Addr
	if status, _ := getUntil(t, base+"/debug/pprof/"); status != http.StatusOK {
		t.Fatalf("GET before Close: status = %d, want 200", status)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close is synchronous: the port must refuse immediately.
	_, _, err = getOnce(t, base+"/debug/pprof/")
	if err == nil {
		t.Fatalf("GET after Close: succeeded, want connection refused")
	}
	// Closing twice is safe (daemon shutdown paths may overlap).
	if err := srv.Close(); err != nil {
		t.Errorf("second Close: %v, want nil", err)
	}
	// Closing a nil server is safe too.
	var nilSrv *Server
	if err := nilSrv.Close(); err != nil {
		t.Errorf("nil Close: %v, want nil", err)
	}
}
