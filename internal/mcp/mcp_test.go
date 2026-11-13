package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// --- Fake MCP server (test-helper process) ----------------------------
//
// The fake server runs when the test binary is re-invoked with
// ORION_TEST_FAKE_MCP_SERVER=1. It speaks NDJSON JSON-RPC on stdin/
// stdout: initialize, tools/list, tools/call, plus a ping request
// back to the client after initialize (to prove the client answers
// server requests). ORION_TEST_FAKE_MCP_MODE selects behaviour:
// "" (normal), "exit" (die immediately), "die" (die after
// initialize), "hang" (never answer initialize), "badname"
// (advertise a tool with an invalid name too).

func TestFakeMCPServerProcess(t *testing.T) {
	if os.Getenv("ORION_TEST_FAKE_MCP_SERVER") != "1" {
		t.Skip("not the fake server process")
	}
	fakeServerMain()
}

func fakeServerMain() {
	mode := os.Getenv("ORION_TEST_FAKE_MCP_MODE")
	if mode == "exit" {
		os.Exit(1)
	}
	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	write := func(v any) {
		data, _ := json.Marshal(v)
		_, _ = out.Write(data)
		_ = out.WriteByte('\n')
		_ = out.Flush()
	}
	pingFile := os.Getenv("ORION_TEST_FAKE_PING_FILE")
	for {
		line, err := in.ReadBytes('\n')
		if len(line) == 0 {
			return
		}
		var env struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
			Params json.RawMessage  `json:"params"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			if err != nil {
				return
			}
		}
		// A response to our ping request: record it so the test
		// can prove the client answered.
		if env.Method == "" && env.ID != nil && pingFile != "" {
			var id int64
			if json.Unmarshal(*env.ID, &id) == nil && id == 99 {
				_ = os.WriteFile(pingFile, []byte("pong"), 0o600)
			}
			if err != nil {
				return
			}
			continue
		}
		if env.ID == nil {
			if err != nil {
				return
			}
			continue // notification: ignore
		}
		reply := func(result any) {
			write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(*env.ID), "result": result})
		}
		replyErr := func(code int, msg string) {
			write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(*env.ID),
				"error": map[string]any{"code": code, "message": msg}})
		}
		switch env.Method {
		case "initialize":
			if mode == "hang" {
				// Never answer: the client's initialize
				// timeout must fire. Sleep until killed.
				time.Sleep(60 * time.Second)
				return
			}
			// Ask the client a question first (ping), then
			// answer initialize. The client must answer the
			// ping while its initialize call is still pending.
			write(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "ping"})
			reply(map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			})
			if mode == "die" {
				return
			}
		case "tools/list":
			toolList := []any{
				map[string]any{"name": "echo", "description": "Echo the text argument back.",
					"inputSchema": map[string]any{"type": "object",
						"properties": map[string]any{"text": map[string]any{"type": "string"}},
						"required":   []string{"text"}}},
				map[string]any{"name": "failer", "description": "Always fails.",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
				map[string]any{"name": "showenv", "description": "Show env values.",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
				map[string]any{"name": "slow", "description": "Sleeps then answers.",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
				map[string]any{"name": "nodesc",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
			}
			if mode == "badname" {
				toolList = append(toolList, map[string]any{"name": "bad name!", "description": "Invalid.",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}})
			}
			reply(map[string]any{"tools": toolList})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(env.Params, &p)
			switch p.Name {
			case "echo":
				text, _ := p.Arguments["text"].(string)
				reply(map[string]any{"content": []any{
					map[string]any{"type": "text", "text": "echo:" + text}}})
			case "failer":
				reply(map[string]any{"content": []any{
					map[string]any{"type": "text", "text": "boom"}}, "isError": true})
			case "showenv":
				reply(map[string]any{"content": []any{
					map[string]any{"type": "text", "text": fmt.Sprintf("secret=%s plain=%s",
						envOrAbsent("MCP_TEST_FAKE_SECRET"), envOrAbsent("MCP_TEST_FAKE_PLAIN"))}}})
			case "slow":
				time.Sleep(30 * time.Second)
				reply(map[string]any{"content": []any{
					map[string]any{"type": "text", "text": "slow-done"}}})
			case "nodesc":
				reply(map[string]any{"content": []any{
					map[string]any{"type": "text", "text": "nodesc-ok"}}})
			default:
				replyErr(-32602, "unknown tool "+p.Name)
			}
		default:
			replyErr(-32601, "method not found: "+env.Method)
		}
		if err != nil {
			return
		}
	}
}

func envOrAbsent(k string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return "(absent)"
}

// --- Test helpers --------------------------------------------------------

// fakeServerBinary returns the command that re-invokes this test
// binary as the fake MCP server, with the given mode. The fake-mode
// variables are passed through the server's explicit config env
// (which overlays the scrubbed base) so the test also exercises the
// env-overlay path.
func fakeServerConfig(t *testing.T, mode string, extraEnv map[string]string) ServerConfig {
	t.Helper()
	env := map[string]string{"ORION_TEST_FAKE_MCP_SERVER": "1"}
	if mode != "" {
		env["ORION_TEST_FAKE_MCP_MODE"] = mode
	}
	for k, v := range extraEnv {
		env[k] = v
	}
	return ServerConfig{Command: os.Args[0], Args: []string{"-test.run=^TestFakeMCPServerProcess$", "-test.count=1"}, Env: env}
}

// startFakeManager writes mcp.json for one fake server into dir and
// starts a manager over it. The caller must Close the manager.
func startFakeManager(t *testing.T, dir, mode string, extraEnv map[string]string) *Manager {
	t.Helper()
	cfg := &Config{MCPServers: map[string]ServerConfig{"fake": fakeServerConfig(t, mode, extraEnv)}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal test config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), data, 0o600); err != nil {
		t.Fatalf("write test mcp.json: %v", err)
	}
	mgr, err := StartManager(dir, nil)
	if err != nil {
		t.Fatalf("StartManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	return mgr
}

// waitForFile polls for a file to appear (the fake server's ping
// marker), up to timeout.
func waitForFile(t *testing.T, path string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// --- Config tests ----------------------------------------------------------

func TestLoadConfigMissingFileIsEmpty(t *testing.T) {
	cfg, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatalf("LoadConfig missing file: %v", err)
	}
	if len(cfg.MCPServers) != 0 {
		t.Errorf("missing file: %d servers, want 0", len(cfg.MCPServers))
	}
}

func TestLoadConfigMalformedIsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir); err == nil {
		t.Error("malformed mcp.json loaded without error")
	}
}

func TestLoadConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"bad server name", Config{MCPServers: map[string]ServerConfig{"bad name!": {Command: "/bin/true"}}}},
		{"empty command", Config{MCPServers: map[string]ServerConfig{"s": {Command: ""}}}},
		{"relative command", Config{MCPServers: map[string]ServerConfig{"s": {Command: "bin/server"}}}},
		{"empty env key", Config{MCPServers: map[string]ServerConfig{"s": {Command: "/bin/true", Env: map[string]string{"": "x"}}}}},
		{"env key with =", Config{MCPServers: map[string]ServerConfig{"s": {Command: "/bin/true", Env: map[string]string{"A=B": "x"}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			data, _ := json.Marshal(tc.cfg)
			if err := os.WriteFile(filepath.Join(dir, "mcp.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(dir); err == nil {
				t.Errorf("config %q loaded without error", tc.name)
			}
		})
	}
}

func TestLoadConfigNeverReadsCwd(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	data, _ := json.Marshal(Config{MCPServers: map[string]ServerConfig{"evil": {Command: "/bin/true"}}})
	if err := os.WriteFile(filepath.Join(work, "mcp.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(t.TempDir()) // a different, empty data dir
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.MCPServers) != 0 {
		t.Error("config loaded from the working directory; want data-dir only")
	}
}

func TestResolveServerCommand(t *testing.T) {
	if _, err := resolveServerCommand("bin/server"); err == nil {
		t.Error("relative command with separator accepted")
	}
	if _, err := resolveServerCommand("definitely-not-a-real-command-xyz"); err == nil {
		t.Error("missing bare command accepted")
	}
	if _, err := resolveServerCommand("/bin/true"); err != nil {
		t.Errorf("absolute command rejected: %v", err)
	}
	if _, err := resolveServerCommand("true"); err != nil {
		t.Errorf("bare command in PATH rejected: %v", err)
	}
}

// --- Manager / client tests -------------------------------------------------

func TestManagerEchoToolEndToEnd(t *testing.T) {
	dir := t.TempDir()
	mgr := startFakeManager(t, dir, "", nil)
	if mgr == nil {
		t.Fatal("StartManager returned nil with a server configured")
	}
	if errs := mgr.StartErrors(); len(errs) != 0 {
		t.Fatalf("StartErrors = %v, want none", errs)
	}
	names := []string{}
	for _, tl := range mgr.Tools() {
		names = append(names, tl.Name())
	}
	want := "mcp__fake__echo"
	found := false
	for _, n := range names {
		if n == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("tools = %v, want %q among them", names, want)
	}
	// Through the registry: the central choke point applies.
	reg := tools.NewRegistry()
	if err := reg.RegisterAll(mgr.Tools()); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	out, err := reg.Execute(context.Background(), "mcp__fake__echo", json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatalf("Execute echo: %v", err)
	}
	if out != "echo:hello" {
		t.Errorf("echo result = %q, want %q", out, "echo:hello")
	}
}

func TestManagerResultGoesThroughRegistryRedaction(t *testing.T) {
	dir := t.TempDir()
	mgr := startFakeManager(t, dir, "", nil)
	reg := tools.NewRegistry()
	if err := reg.RegisterAll(mgr.Tools()); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	// A secret-shaped token echoed back by the server must be
	// redacted by the registry before it reaches the model.
	token := "sk-0123456789abcdef0123456789abcdef0123456789abcdef"
	out, err := reg.Execute(context.Background(), "mcp__fake__echo", json.RawMessage(`{"text":"`+token+`"}`))
	if err != nil {
		t.Fatalf("Execute echo: %v", err)
	}
	if strings.Contains(out, token) {
		t.Errorf("registry did not redact the echoed secret: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("redacted echo = %q, want a [REDACTED] marker", out)
	}
}

func TestManagerFailerToolReturnsError(t *testing.T) {
	dir := t.TempDir()
	mgr := startFakeManager(t, dir, "", nil)
	reg := tools.NewRegistry()
	if err := reg.RegisterAll(mgr.Tools()); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	_, err := reg.Execute(context.Background(), "mcp__fake__failer", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("failer tool succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("failer error = %v, want the server's 'boom' text", err)
	}
}

func TestManagerServerEnvIsScrubbed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCP_TEST_FAKE_SECRET", "sentinel-secret-value")
	t.Setenv("MCP_TEST_FAKE_PLAIN", "plain-value")
	mgr := startFakeManager(t, dir, "", nil)
	// Call the tool DIRECTLY, not through Registry.Execute: the
	// registry's RedactSecrets would redact "secret=(absent)" (the
	// word "secret=" followed by any 4+ char value) and the
	// assertion below could never see the raw server text. The
	// scrub check needs the unredacted result.
	var showenv tools.Tool
	for _, tl := range mgr.Tools() {
		if tl.Name() == "mcp__fake__showenv" {
			showenv = tl
		}
	}
	if showenv == nil {
		t.Fatal("mcp__fake__showenv tool not registered")
	}
	out, err := showenv.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute showenv: %v", err)
	}
	if !strings.Contains(out, "secret=(absent)") {
		t.Errorf("showenv = %q: server saw the secret var; want (absent)", out)
	}
	if strings.Contains(out, "sentinel-secret-value") {
		t.Errorf("showenv = %q: secret value reached the server", out)
	}
	if !strings.Contains(out, "plain=plain-value") {
		t.Errorf("showenv = %q: server did not see the plain var", out)
	}
}

func TestManagerBadToolNameSkipped(t *testing.T) {
	dir := t.TempDir()
	mgr := startFakeManager(t, dir, "badname", nil)
	for _, tl := range mgr.Tools() {
		if strings.Contains(tl.Name(), "bad") {
			t.Errorf("invalid tool name registered: %q", tl.Name())
		}
	}
	found := false
	for _, tl := range mgr.Tools() {
		if tl.Name() == "mcp__fake__echo" {
			found = true
		}
	}
	if !found {
		t.Error("valid echo tool missing when a bad-named tool was skipped")
	}
}

func TestManagerExitServerRecorded(t *testing.T) {
	dir := t.TempDir()
	mgr := startFakeManager(t, dir, "exit", nil)
	if len(mgr.Tools()) != 0 {
		t.Errorf("exiting server produced %d tools, want 0", len(mgr.Tools()))
	}
	if errs := mgr.StartErrors(); len(errs) == 0 || errs["fake"] == nil {
		t.Errorf("StartErrors = %v, want an error for server 'fake'", errs)
	}
}

func TestManagerDieAfterInitRecorded(t *testing.T) {
	dir := t.TempDir()
	mgr := startFakeManager(t, dir, "die", nil)
	if len(mgr.Tools()) != 0 {
		t.Errorf("dying server produced %d tools, want 0", len(mgr.Tools()))
	}
	if errs := mgr.StartErrors(); len(errs) == 0 || errs["fake"] == nil {
		t.Errorf("StartErrors = %v, want an error for server 'fake'", errs)
	}
}

func TestManagerHangInitializeTimesOut(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	mgr := startFakeManager(t, dir, "hang", nil)
	if time.Since(start) > 30*time.Second {
		t.Errorf("hanging server took %v to fail; want the ~10s initialize timeout", time.Since(start))
	}
	if len(mgr.Tools()) != 0 {
		t.Errorf("hanging server produced %d tools, want 0", len(mgr.Tools()))
	}
	if errs := mgr.StartErrors(); len(errs) == 0 || errs["fake"] == nil {
		t.Errorf("StartErrors = %v, want an error for server 'fake'", errs)
	}
}

func TestClientAnswersServerPing(t *testing.T) {
	dir := t.TempDir()
	pingFile := filepath.Join(dir, "ping-marker")
	mgr := startFakeManager(t, dir, "", map[string]string{"ORION_TEST_FAKE_PING_FILE": pingFile})
	if len(mgr.Tools()) == 0 {
		t.Fatal("no tools: server did not start")
	}
	if !waitForFile(t, pingFile, 5*time.Second) {
		t.Error("client never answered the server's ping request")
	}
}

func TestCallToolTimeout(t *testing.T) {
	dir := t.TempDir()
	mgr := startFakeManager(t, dir, "", nil)
	var slow *Tool
	for _, tl := range mgr.Tools() {
		if tl.Name() == "mcp__fake__slow" {
			slow = tl.(*Tool)
		}
	}
	if slow == nil {
		t.Fatal("slow tool not registered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := slow.Execute(ctx, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("slow tool returned without error under a 200ms timeout")
	}
	var toolErr *tools.ToolError
	if !isToolError(err, &toolErr) || toolErr.Kind != "timeout" {
		t.Errorf("slow tool error = %v (kind %q), want a timeout ToolError", err, toolErrKind(err))
	}
}

func isToolError(err error, out **tools.ToolError) bool {
	if err == nil {
		return false
	}
	te, ok := err.(*tools.ToolError)
	if ok {
		*out = te
	}
	return ok
}

func toolErrKind(err error) string {
	if te, ok := err.(*tools.ToolError); ok {
		return te.Kind
	}
	return ""
}

func TestToolDefinitionDefaults(t *testing.T) {
	dir := t.TempDir()
	mgr := startFakeManager(t, dir, "", nil)
	var nodesc *Tool
	for _, tl := range mgr.Tools() {
		if tl.Name() == "mcp__fake__nodesc" {
			nodesc = tl.(*Tool)
		}
	}
	if nodesc == nil {
		t.Fatal("nodesc tool not registered")
	}
	def := nodesc.Definition()
	if def.Name != "mcp__fake__nodesc" {
		t.Errorf("definition name = %q", def.Name)
	}
	if strings.TrimSpace(def.Description) == "" || !strings.Contains(def.Description, "fake") {
		t.Errorf("definition description = %q, want the generated default naming the server", def.Description)
	}
	if def.Parameters == nil || def.Parameters["type"] != "object" {
		t.Errorf("definition parameters = %v, want an object schema", def.Parameters)
	}
	// The tool still executes through the client.
	out, err := nodesc.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || out != "nodesc-ok" {
		t.Errorf("nodesc Execute = %q, %v; want nodesc-ok, nil", out, err)
	}
}

func TestNormalizeInputSchema(t *testing.T) {
	if got := normalizeInputSchema(nil); got["type"] != "object" {
		t.Errorf("nil schema -> %v, want object schema", got)
	}
	if got := normalizeInputSchema(map[string]any{"type": "array"}); got["type"] != "object" {
		t.Errorf("array schema -> %v, want default object schema", got)
	}
	schema := map[string]any{"type": "object",
		"properties": map[string]any{"x": map[string]any{"type": "string"}},
		"required":   []any{"x"}}
	if got := normalizeInputSchema(schema); got["properties"] == nil {
		t.Errorf("object schema lost its properties: %v", got)
	}
	if got := normalizeInputSchema(map[string]any{"type": "object", "properties": "nope"}); got["properties"] == nil {
		t.Errorf("bad properties not replaced with empty map: %v", got)
	}
}

func TestStartManagerNoConfigReturnsNil(t *testing.T) {
	mgr, err := StartManager(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("StartManager with no config: %v", err)
	}
	if mgr != nil {
		t.Error("StartManager with no config returned a manager; want nil")
	}
}

// TestClientCloseIsIdempotentAndKillsServer starts a server, closes
// twice, and checks the client is dead afterwards.
func TestClientCloseKillsServer(t *testing.T) {
	dir := t.TempDir()
	mgr := startFakeManager(t, dir, "", nil)
	c := mgr.Client("fake")
	if c == nil || !c.Alive() {
		t.Fatal("fake client not alive after start")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if c.Alive() {
		t.Error("client still alive after Close")
	}
	if _, err := c.CallTool(context.Background(), "echo", map[string]any{"text": "x"}); err == nil {
		t.Error("CallTool on a closed client succeeded; want an error")
	}
}

// TestFakeServerBinaryRuns ensures the helper-process trick itself
// works in this environment (a bare test-binary invocation).
func TestFakeServerBinaryRuns(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestFakeMCPServerProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ORION_TEST_FAKE_MCP_SERVER=1", "ORION_TEST_FAKE_MCP_MODE=exit")
	if err := cmd.Run(); err == nil {
		t.Error("fake server in exit mode returned success; want exit 1")
	}
}

// --- MCP tool through the agent loop ------------------------------------

// loopProvider is a scriptable model.Provider for the loop test: its
// first reply requests the fake MCP echo tool, its second answers
// "done". It records every request so the test can prove the MCP
// tool result reached the model.
type loopProvider struct {
	mu    sync.Mutex
	calls int
	reqs  []model.ChatRequest
}

func (p *loopProvider) Name() string { return "loop-fake" }

func (p *loopProvider) Chat(_ context.Context, req model.ChatRequest) (*model.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.reqs = append(p.reqs, req)
	if p.calls == 1 {
		return &model.ChatResponse{
			Message: model.Message{
				Role: "assistant",
				ToolCalls: []model.ToolCall{
					{ID: "call-1", Name: "mcp__fake__echo", Arguments: `{"text":"hello"}`},
				},
			},
			FinishReason: "tool_calls",
		}, nil
	}
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: "done"},
		FinishReason: "stop",
	}, nil
}

func (p *loopProvider) Stream(ctx context.Context, req model.ChatRequest, _ func(chunk string) error) (*model.ChatResponse, error) {
	return p.Chat(ctx, req)
}

func (p *loopProvider) requests() []model.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]model.ChatRequest(nil), p.reqs...)
}

// TestManagerToolThroughAgentLoop proves an MCP tool works through
// the real agent loop: the model calls mcp__fake__echo, the loop
// executes it via the registry (permission gate, redaction, caps),
// and the server's answer reaches the model as a tool result.
func TestManagerToolThroughAgentLoop(t *testing.T) {
	dir := t.TempDir()
	mgr := startFakeManager(t, dir, "", nil)
	reg := tools.NewRegistry()
	if err := reg.RegisterAll(mgr.Tools()); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	prov := &loopProvider{}
	cfg := config.Default()
	cfg.MaxSteps = 5
	cfg.TimeoutSeconds = 30
	agent, err := core.NewAgent(cfg, prov, reg, memory.NewEphemeralStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	out, err := agent.Run(context.Background(), "echo hello via MCP")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "done" {
		t.Fatalf("Run = %q, want %q", out, "done")
	}
	reqs := prov.requests()
	if len(reqs) < 2 {
		t.Fatalf("provider saw %d request(s), want at least 2 (tool call, then answer)", len(reqs))
	}
	found := false
	for _, m := range reqs[1].Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "echo:hello") {
			found = true
		}
	}
	if !found {
		t.Errorf("second provider request carries no MCP echo result: %+v", reqs[1].Messages)
	}
}
