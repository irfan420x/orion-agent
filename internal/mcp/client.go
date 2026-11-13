// Package mcp implements Orion's MCP client (audit P0 #7): it launches
// configured MCP servers over stdio, speaks MCP (JSON-RPC 2.0, one
// JSON object per line) to them, and exposes each server's tools as
// Orion tools named mcp__<server>__<tool> through the existing
// tools.Tool interface — so MCP tools get the permission gate, the
// central redact/truncate choke point, and the result caps for free.
//
// # Trust and configuration
//
// Servers are configured ONLY in <DataDir>/mcp.json (the user data
// dir, e.g. ~/.orion/mcp.json). The file is never read from the
// repository working directory, any project directory, or any path a
// model or tool result can influence: a cloned repo must never be
// able to smuggle an MCP server into the agent. There is no second
// search path and no environment override. The shape mirrors the
// familiar .mcp.json layout:
//
//	{"mcpServers": {"my-server": {"command": "/usr/bin/my-server",
//	                               "args": ["--flag"],
//	                               "env": {"MY_SERVER_KEY": "…"}}}}
//
// The server command must be an absolute path or a bare name found
// via PATH; a relative path containing a separator is refused, so a
// server can never be resolved against the daemon's working
// directory (which might be a repo). The server child runs with its
// working directory set to the data dir, never the repo cwd.
//
// # Confinement (Audit 5, F-A5-4)
//
// A server runs confined to the Orion sandbox by default (the same
// namespace/chroot + seccomp confinement shell_exec uses — see
// internal/mcp/sandbox.go): it sees /usr read-only and the sandbox
// work dir, and can never read the agent's config, keys, or home
// directory. The server binary must therefore live under /usr (or
// resolve there). A server that needs host access opts out
// explicitly with "sandbox": false in mcp.json, and an opted-out
// server is FULLY TRUSTED code, equivalent to a hook: it runs as
// the daemon uid with full filesystem access. Confinement is
// fail-closed: when it is on but cannot be established, the server
// is refused, never silently run unsandboxed.
//
// # Environment
//
// A server child NEVER inherits the agent's secrets: its environment
// is tools.ScrubEnv (the same scrub shell_exec uses) plus the
// per-server "env" entries from mcp.json, plus ORION_MCP_SERVER.
// The mcp.json "env" entries are the operator's explicit choice —
// that is how a server gets its own API key — but nothing else from
// the agent's environment leaks through. A server that prints a
// secret in a tool result is still caught by the registry's
// redaction, which every MCP result passes through.
//
// # Gate
//
// MCP tools are classified ClassNetwork by the permission gate (see
// internal/permissions): a server is an external process with
// unknown capabilities — a filesystem server's write tool is as
// destructive as a network call — so every mcp__ tool needs approval
// when a gate is installed (Telegram turns). Local CLI runs are
// ungated, as for every other tool.
//
// # Scope
//
// Client only, stdio transport only. MCP server mode (exposing
// Orion's tools to other clients) is out of scope for this phase,
// as are HTTP/SSE transports. A server that never answers is
// bounded by context timeouts; a server that exits is detected and
// its pending calls fail instead of hanging.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/irfan/orion/internal/tools"
	"github.com/irfan/orion/internal/version"
)

// Protocol constants.
const (
	// protocolVersion is the MCP protocol version the client speaks.
	protocolVersion = "2024-11-05"
	// initializeTimeout bounds the initialize handshake, and
	// listTimeout bounds tools/list: a server that cannot answer
	// either promptly is unusable and must fail fast, not stall
	// daemon startup.
	initializeTimeout = 10 * time.Second
	listTimeout       = 10 * time.Second
	// maxLineBytes caps one NDJSON line from a server (16 MiB): a
	// flooding server must not be able to exhaust memory through
	// one unbounded line. A line over the cap kills the server.
	maxLineBytes = 16 * 1024 * 1024
	// maxDescriptionRunes caps a server tool description handed to
	// the model: a server's description is untrusted text in the
	// provider request, and it must stay short.
	maxDescriptionRunes = 2000
	// maxToolsPerServer caps how many tools one server may
	// advertise (Audit 5, F-A5-P2-9): a server advertising
	// thousands of tools would flood every provider request with
	// definitions. Extras are skipped and logged.
	maxToolsPerServer = 64
	// maxSchemaBytes caps the serialised JSON Schema of one tool
	// (Audit 5, F-A5-P2-9): a multi-MB schema would flood the
	// provider request the same way. An oversize schema is
	// replaced with an empty object schema and logged.
	maxSchemaBytes = 64 * 1024
	// closeGrace is how long Close waits after SIGTERM before
	// SIGKILL: enough for a well-behaved server to exit cleanly.
	closeGrace = 2 * time.Second
)

// namePattern is what server names and tool names must match:
// letters, digits, underscore, hyphen. Anything else is refused
// (server names at config load, tool names at tools/list) so the
// mcp__<server>__<tool> name can never smuggle a separator or a
// second tool name into the registry.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return "mcp: unknown RPC error"
	}
	return fmt.Sprintf("mcp: RPC error %d: %s", e.Code, e.Message)
}

// rpcResponse is one JSON-RPC response from the server.
type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// envelope is the wire shape of one NDJSON line in either
// direction: a request/notification carries Method, a response
// carries neither Method nor (for notifications) an ID.
type envelope struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Params json.RawMessage  `json:"params"`
	Result json.RawMessage  `json:"result"`
	Error  *rpcError        `json:"error"`
}

// Client is one MCP server child process plus its JSON-RPC session.
// Create it with NewClient, start it with Start, and Close it when
// done. It is safe for concurrent use: calls are serialised only
// where the protocol needs it (writes), and responses are matched
// to calls by ID. A client whose server has died reports that on
// every call instead of hanging.
type Client struct {
	name    string
	cfg     ServerConfig
	dataDir string
	logger  *log.Logger

	mu      sync.Mutex // guards started/starting/closed/cmd/onDead
	started bool
	// starting is true between the check in Start and the moment
	// Start records the outcome: two concurrent Start calls must
	// never both launch the server (Audit 5, F-A5-P2-17). The
	// check-and-set of started/starting is atomic under mu; the
	// flag is cleared when Start returns, so a failed Start can
	// be retried.
	starting bool
	closed   bool
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stderr   *cappedBuffer
	// onDead, when set (by the Manager), is called exactly once
	// when the server dies, so the Manager can deregister the
	// server's tools (Audit 5, F-A5-P2-12).
	onDead       func(*Client)
	deadHookOnce sync.Once

	// waitCh is closed once cmd.Wait has reaped the server
	// process; waitOnce guards the single Wait (Audit 5,
	// F-A5-P2-12: a dead server must not be left a zombie).
	// waitCh is created in NewClient so Close can always select
	// on it once a process exists.
	waitCh   chan struct{}
	waitOnce sync.Once

	writeMu sync.Mutex // serialises all writes (calls + server-request replies)

	pendingMu sync.Mutex
	pending   map[int64]chan rpcResponse
	nextID    atomic.Int64

	doneOnce sync.Once
	done     chan struct{} // closed when the reader exits (server died or Close)
	deadMu   sync.Mutex
	deadErr  error
}

// NewClient builds a client for the named server. It does not start
// anything: call Start. A bad name or config is reported by Start,
// not here, so the manager can record per-server failures.
func NewClient(name string, cfg ServerConfig, dataDir string) *Client {
	return &Client{
		name:    name,
		cfg:     cfg,
		dataDir: dataDir,
		logger:  log.New(os.Stderr, "orion/mcp: ", log.LstdFlags),
		pending: map[int64]chan rpcResponse{},
		done:    make(chan struct{}),
		waitCh:  make(chan struct{}),
	}
}

// SetLogger replaces the client logger. A nil logger is ignored.
func (c *Client) SetLogger(l *log.Logger) {
	if l == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logger = l
}

func (c *Client) logf(format string, args ...any) {
	c.mu.Lock()
	l := c.logger
	c.mu.Unlock()
	if l != nil {
		l.Printf(format, args...)
	}
}

// Name returns the configured server name.
func (c *Client) Name() string { return c.name }

// Alive reports whether the server process is up (started and the
// reader has not observed its death). It is a snapshot: a server
// can die immediately after. A client that was never started is
// NOT alive (Audit 5, F-A5-P2-15): the old check returned true
// before Start ever ran.
func (c *Client) Alive() bool {
	c.mu.Lock()
	started := c.started
	c.mu.Unlock()
	c.deadMu.Lock()
	defer c.deadMu.Unlock()
	return started && c.deadErr == nil
}

// markDead records the server's death and wakes every waiter: the
// reader calls it on EOF/error, Close calls it after killing the
// process. The first error wins; later ones are dropped. The
// onDead hook (when set) fires exactly once, so the Manager can
// deregister the server's tools.
func (c *Client) markDead(err error) {
	c.deadMu.Lock()
	if c.deadErr == nil {
		if err == nil {
			err = errors.New("mcp: server exited")
		}
		c.deadErr = err
	}
	c.deadMu.Unlock()
	c.doneOnce.Do(func() { close(c.done) })
	c.failPending(err)
	c.deadHookOnce.Do(func() {
		c.mu.Lock()
		hook := c.onDead
		c.mu.Unlock()
		if hook != nil {
			hook(c)
		}
	})
}

// deadError returns the recorded death error, or nil.
func (c *Client) deadError() error {
	c.deadMu.Lock()
	defer c.deadMu.Unlock()
	return c.deadErr
}

// failPending delivers err to every in-flight call. Callers hold no
// locks when calling this (it takes pendingMu).
func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = map[int64]chan rpcResponse{}
	c.pendingMu.Unlock()
	if err == nil {
		err = errors.New("mcp: server exited")
	}
	for _, ch := range pending {
		select {
		case ch <- rpcResponse{Error: &rpcError{Code: -32000, Message: err.Error()}}:
		default:
		}
	}
}

// resolveServerCommand validates a server command the way the
// package doc promises: an absolute path, or a bare name resolved
// through PATH. A relative path with a separator is refused — it
// would resolve against the daemon's working directory, which may
// be a repo.
func resolveServerCommand(command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", errors.New("mcp: server command must not be empty")
	}
	if strings.ContainsRune(command, os.PathSeparator) {
		if !filepath.IsAbs(command) {
			return "", fmt.Errorf("mcp: server command %q is a relative path; use an absolute path or a bare command name", command)
		}
		return command, nil
	}
	p, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("mcp: server command %q not found in PATH: %w", command, err)
	}
	return p, nil
}

// serverEnv builds the server child's environment: the scrubbed
// process environment (no secrets) plus the operator-configured
// per-server entries from mcp.json, plus ORION_MCP_SERVER. The
// explicit entries win over the scrubbed base — they are the
// operator's explicit choice for this server — but the scrubbed
// base never carries an Orion secret in the first place.
func (c *Client) serverEnv() []string {
	env := tools.ScrubEnv()
	keys := make([]string, 0, len(c.cfg.Env))
	for k := range c.cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Build a map so an explicit entry replaces any scrubbed entry
	// with the same key instead of leaving two values (exec would
	// hand the child the last one, but duplicates are a footgun).
	merged := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			merged[k] = v
		}
	}
	for _, k := range keys {
		merged[k] = c.cfg.Env[k]
	}
	out := make([]string, 0, len(merged)+1)
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	out = append(out, "ORION_MCP_SERVER="+c.name)
	sort.Strings(out)
	return out
}

// Start launches the server and performs the MCP initialize
// handshake (initialize + notifications/initialized). ctx bounds
// the whole start, but the handshake itself is capped at
// initializeTimeout so a hung server cannot stall daemon startup
// past that. On failure the process is killed and Start reports
// the error; the client must not be reused afterwards.
func (c *Client) Start(ctx context.Context) error {
	// The check-and-set of started/starting is atomic under mu:
	// two concurrent Start calls must never both launch the
	// server (Audit 5, F-A5-P2-17). The flag is cleared when
	// Start returns, so a failed Start can be retried; a
	// successful Start leaves started set, and starting is
	// cleared by the same defer.
	c.mu.Lock()
	if c.started || c.starting {
		c.mu.Unlock()
		return errors.New("mcp: client already started")
	}
	if c.closed {
		c.mu.Unlock()
		return errors.New("mcp: client is closed")
	}
	c.starting = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.starting = false
		c.mu.Unlock()
	}()

	if !namePattern.MatchString(c.name) {
		return fmt.Errorf("mcp: invalid server name %q (want letters, digits, _ or -)", c.name)
	}
	if err := c.cfg.Validate(); err != nil {
		return fmt.Errorf("mcp: server %q: %w", c.name, err)
	}
	path, err := resolveServerCommand(c.cfg.Command)
	if err != nil {
		return fmt.Errorf("mcp: server %q: %w", c.name, err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !c.cfg.sandboxEnabled() {
		c.logf("server %q runs UNSANDBOXED (\"sandbox\": false in mcp.json): it is fully trusted code, like a hook, and can read the agent's config and home directory", c.name)
	}

	cmd := exec.Command(path, c.cfg.Args...)
	cmd.Dir = c.dataDir
	cmd.Env = c.serverEnv()
	// Own process group so Close can kill the whole tree, not
	// just the direct child (a server that spawns helpers would
	// otherwise orphan them).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("mcp: server %q: stdin pipe: %w", c.name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("mcp: server %q: stdout pipe: %w", c.name, err)
	}
	c.stderr = &cappedBuffer{limit: 16 * 1024}
	cmd.Stderr = c.stderr
	// Confinement (Audit 5, F-A5-4): by default the server runs
	// under the Orion sandbox (namespace/chroot + seccomp, the
	// same confinement as shell_exec) — see
	// prepareSandboxed. It rewrites cmd to run the sandbox
	// wrapper; the pipes above survive it (Prepare does not
	// touch Stdin/Stdout), so the MCP session still speaks over
	// them. Fail-closed: a sandbox that cannot be established
	// refuses the server, it never silently runs unsandboxed.
	if c.cfg.sandboxEnabled() {
		if err := c.prepareSandboxed(cmd, path); err != nil {
			_ = stdin.Close()
			return err
		}
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("mcp: server %q: start: %w", c.name, err)
	}
	c.mu.Lock()
	c.cmd = cmd
	c.stdin = stdin
	c.started = true
	c.mu.Unlock()
	go c.readLoop(bufio.NewReaderSize(stdout, 64*1024))

	// Handshake under its own timeout: a server that starts but
	// never answers initialize must fail fast.
	hctx, cancel := context.WithTimeout(ctx, initializeTimeout)
	defer cancel()
	var initResult struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := c.Call(hctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "orion", "version": version.Version},
	}, &initResult); err != nil {
		_ = c.Close()
		return fmt.Errorf("mcp: server %q: initialize: %w", c.name, err)
	}
	if initResult.ProtocolVersion != "" && initResult.ProtocolVersion != protocolVersion {
		c.logf("server %q speaks protocol %q (client asked %q); continuing", c.name, initResult.ProtocolVersion, protocolVersion)
	}
	// The initialized notification has no ID and expects no reply.
	_ = c.notify("notifications/initialized", map[string]any{})
	return nil
}

// readLoop is the reader goroutine: it reads NDJSON lines from the
// server until EOF/error, routing responses to their callers and
// answering server requests. It never blocks on a caller: a
// response with no waiter is dropped, and a request reply is a
// best-effort write. On exit it marks the client dead and reaps
// the server process (Audit 5, F-A5-P2-12: a dead server must not
// be left a zombie).
//
// The line cap is enforced INCREMENTALLY (Audit 5, F-A5-3): the
// old code used ReadBytes('\n') and only checked the length after
// it returned, so a server streaming without a newline grew memory
// unboundedly (bufio's collectFragments accumulates 64 KiB buffers
// with no limit) and the cap never fired. This loop reads with
// ReadSlice, which returns at most one 64 KiB buffer at a time,
// and kills the server the moment the accumulated line exceeds
// maxLineBytes — memory stays bounded by the cap plus one buffer,
// and a newline-less flood dies promptly instead of OOMing the
// daemon.
func (c *Client) readLoop(r *bufio.Reader) {
	defer func() { go c.reap() }()
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		if len(frag) > 0 {
			// Copy: the fragment aliases the reader's buffer
			// and is overwritten by the next ReadSlice.
			line = append(line, frag...)
			if len(line) > maxLineBytes {
				c.markDead(fmt.Errorf("mcp: server %q sent a line over %d bytes; killing it", c.name, maxLineBytes))
				c.killProcess()
				return
			}
		}
		if err == bufio.ErrBufferFull {
			continue // more fragments of the same line coming
		}
		if err != nil {
			if len(line) > 0 {
				// A final line without a trailing newline
				// is still a message: process it, then exit.
				c.handleLine(bytes.TrimSpace(line))
			}
			if err == io.EOF {
				c.markDead(fmt.Errorf("mcp: server %q exited (stderr: %s)", c.name, strings.TrimSpace(c.stderrString())))
			} else {
				c.markDead(fmt.Errorf("mcp: server %q: read: %w", c.name, err))
			}
			return
		}
		// A complete newline-terminated line.
		c.handleLine(bytes.TrimSpace(line))
		line = nil
	}
}

// stderrString returns the captured server stderr.
func (c *Client) stderrString() string {
	if c.stderr == nil {
		return ""
	}
	return c.stderr.String()
}

// handleLine routes one NDJSON line: a server request gets an
// answer, a notification is dropped, a response goes to its
// caller, and anything unparseable is logged and dropped (a
// confused server must not wedge the client).
func (c *Client) handleLine(line []byte) {
	if len(line) == 0 {
		return
	}
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		c.logf("server %q sent unparsable output, dropping %d bytes", c.name, len(line))
		return
	}
	if env.Method != "" {
		if env.ID == nil {
			return // notification from the server: nothing to answer
		}
		c.answerServerRequest(env)
		return
	}
	if env.ID == nil {
		c.logf("server %q sent a response with no id, dropping", c.name)
		return
	}
	var id int64
	if err := json.Unmarshal(*env.ID, &id); err != nil {
		// Our requests always carry numeric IDs; a response
		// with a non-numeric ID cannot be ours.
		return
	}
	c.pendingMu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
	if !ok {
		return // late response to a timed-out call: drop it
	}
	select {
	case ch <- rpcResponse{Result: env.Result, Error: env.Error}:
	default:
	}
}

// answerServerRequest replies to a request the server sent us.
// Only ping and roots/list are supported: sampling (the server
// asking the client to call an LLM) is refused — Orion never lets
// a server drive the model — and anything else is method-not-found.
func (c *Client) answerServerRequest(env envelope) {
	var result any
	var rpcErr *rpcError
	switch env.Method {
	case "ping":
		result = map[string]any{}
	case "roots/list":
		result = map[string]any{"roots": []any{}}
	default:
		rpcErr = &rpcError{Code: -32601, Message: "method not found: " + env.Method}
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(*env.ID)}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	_ = c.writeJSON(resp)
}

// Call performs one JSON-RPC request and decodes its result into
// out (nil out means "ignore the result"). It honours ctx: a
// cancelled or timed-out call returns ctx.Err() and its late
// response is dropped. A dead server returns the death error.
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.deadError(); err != nil {
		return err
	}
	id := c.nextID.Add(1)
	ch := make(chan rpcResponse, 1)
	c.pendingMu.Lock()
	if c.pending == nil {
		c.pending = map[int64]chan rpcResponse{}
	}
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	if err := c.writeJSON(req); err != nil {
		return err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("mcp: server %q: bad %s result: %w", c.name, method, err)
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		if err := c.deadError(); err != nil {
			return err
		}
		return errors.New("mcp: server exited")
	}
}

// notify sends a JSON-RPC notification (no ID, no reply expected).
func (c *Client) notify(method string, params any) error {
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		req["params"] = params
	}
	return c.writeJSON(req)
}

// writeJSON marshals v as one NDJSON line and writes it, holding
// writeMu so a call and a server-request reply can never interleave
// half-lines on the same pipe.
func (c *Client) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("mcp: encode request: %w", err)
	}
	data = append(data, '\n')
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errors.New("mcp: server stdin is closed")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := stdin.Write(data); err != nil {
		return fmt.Errorf("mcp: write to server %q: %w", c.name, err)
	}
	return nil
}

// ToolInfo is one tool a server advertises via tools/list.
type ToolInfo struct {
	Name        string
	Description string
	// InputSchema is the tool's JSON-Schema input (always an
	// object schema after normalisation; see ListTools).
	InputSchema map[string]any
}

// listToolsResult is the wire shape of a tools/list result.
type listToolsResult struct {
	Tools []struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"inputSchema"`
	} `json:"tools"`
}

// ListTools asks the server for its tools. Tools with invalid names
// are skipped (and logged); the rest are normalised (description
// capped, input schema defaulted to an empty object schema when
// missing or malformed) so a sloppy server can never hand the model
// a broken tool definition. Two floods are capped (Audit 5,
// F-A5-P2-9): at most maxToolsPerServer tools are kept (extras are
// skipped and logged), and a tool whose serialised schema exceeds
// maxSchemaBytes gets an empty object schema instead — a server
// must not be able to inject thousands of definitions, or one
// multi-MB schema, into every provider request.
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	lctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	var res listToolsResult
	if err := c.Call(lctx, "tools/list", map[string]any{}, &res); err != nil {
		return nil, fmt.Errorf("mcp: server %q: tools/list: %w", c.name, err)
	}
	out := make([]ToolInfo, 0, len(res.Tools))
	for _, t := range res.Tools {
		if len(out) >= maxToolsPerServer {
			c.logf("server %q advertised %d tools; keeping the first %d and skipping the rest", c.name, len(res.Tools), maxToolsPerServer)
			break
		}
		if !namePattern.MatchString(t.Name) {
			c.logf("server %q advertised tool with invalid name %q; skipping", c.name, t.Name)
			continue
		}
		schema := normalizeInputSchema(t.InputSchema)
		if b, err := json.Marshal(schema); err != nil || len(b) > maxSchemaBytes {
			c.logf("server %q tool %q has an oversize schema (%d bytes); replacing with an empty object schema", c.name, t.Name, len(b))
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, ToolInfo{
			Name:        t.Name,
			Description: truncateRunes(strings.TrimSpace(t.Description), maxDescriptionRunes),
			InputSchema: schema,
		})
	}
	return out, nil
}

// callToolResult is the wire shape of a tools/call result.
type callToolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

// CallTool calls one server tool with args (decoded JSON object)
// and returns its text result. A server-reported error (isError)
// is returned as an error carrying the server's text. Non-text
// content blocks are summarised, never passed through raw: only
// text (and, when there is no text at all, structuredContent as
// JSON) reaches the model.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !namePattern.MatchString(name) {
		return "", fmt.Errorf("mcp: invalid tool name %q", name)
	}
	if args == nil {
		args = map[string]any{}
	}
	var res callToolResult
	if err := c.Call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return "", fmt.Errorf("mcp: server %q tool %q: %w", c.name, name, err)
	}
	var texts []string
	for _, block := range res.Content {
		switch block.Type {
		case "text":
			texts = append(texts, block.Text)
		case "":
			// A block with no type but text is still text.
			if block.Text != "" {
				texts = append(texts, block.Text)
			}
		default:
			texts = append(texts, fmt.Sprintf("[%s content from MCP tool %q]", block.Type, name))
		}
	}
	text := strings.Join(texts, "\n")
	if text == "" && len(res.StructuredContent) > 0 {
		text = string(res.StructuredContent)
	}
	if res.IsError {
		if text == "" {
			text = "tool reported an error with no message"
		}
		return "", fmt.Errorf("mcp: server %q tool %q reported an error: %s", c.name, name, text)
	}
	return text, nil
}

// killProcess kills the server's whole process group, if it is
// running. Best-effort: errors are ignored (the process may
// already be gone).
func (c *Client) killProcess() {
	c.mu.Lock()
	cmd := c.cmd
	c.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// reap waits for the server process to be reaped, exactly once:
// cmd.Wait may run only once, so waitOnce guards it, and waitCh
// closes when the wait has finished. Callers (readLoop on death,
// Close on shutdown) may all call reap; only one of them actually
// waits, the rest return once the wait is done. A server that dies
// mid-turn is reaped here, not left a zombie until daemon
// shutdown (Audit 5, F-A5-P2-12).
func (c *Client) reap() {
	c.mu.Lock()
	cmd := c.cmd
	c.mu.Unlock()
	if cmd == nil {
		return
	}
	c.waitOnce.Do(func() {
		_ = cmd.Wait()
		close(c.waitCh)
	})
}

// Close terminates the server: close stdin (a polite EOF), SIGTERM
// the process group, wait up to closeGrace, then SIGKILL. It waits
// for the process to be reaped so no zombie is left, and marks the
// client dead so in-flight calls fail instead of hanging. It is
// safe to call more than once, and on a client that never started.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	cmd := c.cmd
	stdin := c.stdin
	c.mu.Unlock()

	c.markDead(errors.New("mcp: client closed"))
	if cmd == nil {
		return nil
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	// SIGTERM the group, then reap with a grace deadline. The
	// reap is shared with readLoop (same waitOnce/waitCh): if
	// the server already died, its reader is reaping it and
	// this select just waits for that reap to land.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	go c.reap()
	select {
	case <-c.waitCh:
		return nil
	case <-time.After(closeGrace):
		c.killProcess()
		<-c.waitCh
		return nil
	}
}

// truncateRunes shortens s to at most n runes, appending "…" when it
// truncates. Truncating by runes (not bytes) keeps the result valid
// UTF-8.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// normalizeInputSchema returns a usable object schema for a tool
// definition: the server's schema when it is an object schema (or
// has no type at all — permissive servers omit it), else an empty
// object schema. A non-object "properties" is dropped rather than
// handed to the model.
func normalizeInputSchema(schema map[string]any) map[string]any {
	def := map[string]any{"type": "object", "properties": map[string]any{}}
	if schema == nil {
		return def
	}
	if t, ok := schema["type"]; ok {
		if ts, ok := t.(string); ok && ts != "object" {
			return def
		}
	}
	out := map[string]any{"type": "object"}
	if props, ok := schema["properties"]; ok {
		if pm, ok := props.(map[string]any); ok {
			out["properties"] = pm
		} else {
			out["properties"] = map[string]any{}
		}
	} else {
		out["properties"] = map[string]any{}
	}
	if req, ok := schema["required"]; ok {
		out["required"] = req
	}
	if desc, ok := schema["description"]; ok {
		if ds, ok := desc.(string); ok {
			out["description"] = truncateRunes(ds, maxDescriptionRunes)
		}
	}
	return out
}

// cappedBuffer collects output up to a limit and discards the rest.
type cappedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.limit <= 0 {
		w.limit = 16 * 1024
	}
	if w.buf.Len() >= w.limit {
		return len(p), nil
	}
	room := w.limit - w.buf.Len()
	if len(p) > room {
		p = p[:room]
	}
	return w.buf.Write(p)
}

func (w *cappedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}
