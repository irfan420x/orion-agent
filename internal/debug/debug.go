// Package debug serves Orion's optional pprof/debug endpoint
// (F-A3-2). Before rc21 the daemon exposed no debug endpoint at
// all, so goroutine count, heap allocation and GC behaviour were
// not observable on a running daemon (the perf audit could only
// proxy them with OS thread counts and scratch benchmarks).
//
// The endpoint is OFF by default and opt-in only: Start returns
// (nil, nil) — no listener — unless the operator enables it in
// config (debug.pprof_enabled). When enabled it binds
// 127.0.0.1 only — LoopbackHost is a constant, not a config
// value, so no configuration can make it listen on 0.0.0.0 or
// any other interface — and serves ONLY the standard
// net/http/pprof handlers under /debug/pprof/ (index, goroutine,
// heap, threadcreate, block, mutex, cmdline, profile, symbol,
// trace). There is no other route: a request outside
// /debug/pprof/ gets 404, and no config, secret, or application
// data is ever served.
//
// A note on what pprof can show: the profiles are runtime
// diagnostics (stack traces, allocation sizes, GC stats). The
// profile format carries no object contents and no config dump,
// but a goroutine stack can name functions and arguments that
// are in flight at the moment of the dump. The endpoint is
// therefore localhost-only and opt-in, for use on a machine the
// operator trusts — it is not a remote diagnostic interface.
// Enabling it does not weaken the sandbox: the server runs in
// the daemon process, outside any shell_exec sandbox, and touches
// no sandbox state.
package debug

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"time"
)

// DefaultPprofPort is the conventional pprof port, used when the
// operator enables the endpoint without choosing a port (config
// debug.pprof_port 0 — see config.Config.DebugPprofPort).
const DefaultPprofPort = 6060

// LoopbackHost is the only interface the debug endpoint ever
// binds. It is a constant, not a config value: no configuration
// can make the endpoint listen on 0.0.0.0 or any other interface.
const LoopbackHost = "127.0.0.1"

// Config is the debug endpoint's configuration.
type Config struct {
	Enabled bool
	// Port is the TCP port to listen on. 0 means "let the OS pick
	// an ephemeral loopback port" — the daemon resolves 0 to
	// DefaultPprofPort before calling Start, so a daemon always
	// listens on a known port; 0 is the test-friendly form.
	Port int
}

// Server is a running debug endpoint. Addr is the actual listen
// address (for example "127.0.0.1:6060"); it is loopback-only by
// construction.
type Server struct {
	Addr string
	srv  *http.Server
	ln   net.Listener
}

// Start launches the debug endpoint. When !cfg.Enabled it returns
// (nil, nil): no listener is created and nothing listens. When
// enabled it binds LoopbackHost:cfg.Port (0 = ephemeral) and
// serves only the standard net/http/pprof handlers. An out-of
// range port or a bind failure (for example, the port is already
// in use) returns an error and leaves nothing listening.
func Start(cfg Config) (*Server, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.Port < 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("debug: port %d is out of range (0-65535)", cfg.Port)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/debug/pprof", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/debug/pprof/", http.StatusFound)
	})
	ln, err := net.Listen("tcp", net.JoinHostPort(LoopbackHost, strconv.Itoa(cfg.Port)))
	if err != nil {
		return nil, fmt.Errorf("debug: listen %s:%d: %w", LoopbackHost, cfg.Port, err)
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	s := &Server{Addr: ln.Addr().String(), srv: srv, ln: ln}
	go func() {
		// Serve returns http.ErrServerClosed on Close; any other
		// error means the listener died, and there is nothing to
		// do about it here — the daemon keeps polling.
		_ = srv.Serve(ln)
	}()
	return s, nil
}

// Close shuts the endpoint down, waiting up to 5s for in-flight
// pprof requests to finish. It is safe to call on a nil *Server,
// and safe to call more than once. After Close returns, nothing
// listens on the address anymore.
func (s *Server) Close() error {
	if s == nil || s.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.srv.Shutdown(ctx)
	// Shutdown closes the listener once Serve is running, but if
	// Close races the Serve goroutine's start the listener may
	// never have been registered with the server — close it
	// directly so no socket is left behind either way. Closing
	// an already-closed listener is an error we ignore.
	if s.ln != nil {
		_ = s.ln.Close()
	}
	if err != nil {
		_ = s.srv.Close()
		return err
	}
	return nil
}
