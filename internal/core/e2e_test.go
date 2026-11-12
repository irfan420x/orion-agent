// End-to-end tests for the Orion agent loop (internal/core).
//
// These tests drive the REAL agent loop (Agent.Run) against a REAL
// OpenAI-compatible provider (model.NewProvider, "openai") pointed at a
// local httptest mock server — no real API keys, no network, no live
// model. The mock first returns a tool call, then (on the next request,
// after the tool has run for real) a final text answer. The tool
// executes for real through tools.DefaultRegistry, and conversation
// history is asserted against the REAL memory.FileStore (including a
// reopen from disk, proving persistence).
//
// Safety: the only shell command any mock ever requests is
// "echo hello" (harmless). File tools only ever touch t.TempDir().
// Blocked-command coverage uses IsBlockedCommand on strings only and
// the loop's denial path — a destructive command is never executed.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// --- Mock OpenAI-compatible server ---------------------------------------

// mockRequest is one request the mock server received, decoded loosely
// so tests can assert on what the provider actually sent.
type mockRequest struct {
	Path    string
	Auth    string
	Body    map[string]any
	RawBody []byte
}

// mockOpenAIServer is a scripted OpenAI-compatible endpoint. The script
// decides each reply by 1-based call number; every request is recorded.
type mockOpenAIServer struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	calls    atomic.Int32
	requests []mockRequest
	script   func(call int, req mockRequest) (status int, body string)
}

func newMockOpenAIServer(t *testing.T, script func(call int, req mockRequest) (int, string)) *mockOpenAIServer {
	t.Helper()
	m := &mockOpenAIServer{t: t, script: script}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		call := int(m.calls.Add(1))
		req := mockRequest{Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Body: body, RawBody: raw}
		m.mu.Lock()
		m.requests = append(m.requests, req)
		m.mu.Unlock()
		status, respBody := m.script(call, req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, respBody)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockOpenAIServer) URL() string { return m.srv.URL }

func (m *mockOpenAIServer) CallCount() int { return int(m.calls.Load()) }

func (m *mockOpenAIServer) Requests() []mockRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mockRequest(nil), m.requests...)
}

// --- Mock wire responses (built with encoding/json, never hand-escaped) ---

type mockToolCallFunction struct {
	Name      string `json:"name"`
	Arguments any    `json:"arguments"` // string (standard) or object (some compatible servers)
}

type mockToolCall struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function mockToolCallFunction `json:"function"`
}

type mockChatMessage struct {
	Role      string         `json:"role"`
	Content   any            `json:"content"` // string, or nil -> JSON null
	ToolCalls []mockToolCall `json:"tool_calls,omitempty"`
}

type mockChatResponse struct {
	Choices []struct {
		Message      mockChatMessage `json:"message"`
		FinishReason string          `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// toolCallReply renders a one-choice assistant reply requesting a tool
// call. content is nil (JSON null), exactly as real OpenAI-compatible
// servers send for tool-call turns.
func toolCallReply(t *testing.T, id, name string, args any, finishReason string) string {
	t.Helper()
	var resp mockChatResponse
	resp.Choices = make([]struct {
		Message      mockChatMessage `json:"message"`
		FinishReason string          `json:"finish_reason"`
	}, 1)
	resp.Choices[0].Message = mockChatMessage{
		Role:      "assistant",
		Content:   nil,
		ToolCalls: []mockToolCall{{ID: id, Type: "function", Function: mockToolCallFunction{Name: name, Arguments: args}}},
	}
	resp.Choices[0].FinishReason = finishReason
	resp.Usage.PromptTokens = 10
	resp.Usage.CompletionTokens = 5
	resp.Usage.TotalTokens = 15
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal tool-call reply: %v", err)
	}
	return string(b)
}

// textReply renders a one-choice final assistant text reply.
func textReply(t *testing.T, text string) string {
	t.Helper()
	var resp mockChatResponse
	resp.Choices = make([]struct {
		Message      mockChatMessage `json:"message"`
		FinishReason string          `json:"finish_reason"`
	}, 1)
	resp.Choices[0].Message = mockChatMessage{Role: "assistant", Content: text}
	resp.Choices[0].FinishReason = "stop"
	resp.Usage.PromptTokens = 10
	resp.Usage.CompletionTokens = 5
	resp.Usage.TotalTokens = 15
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal text reply: %v", err)
	}
	return string(b)
}

// --- Shared harness -------------------------------------------------------

// e2eAgent wires the real loop: real OpenAI provider against the mock,
// real DefaultRegistry tools, real FileStore memory.
func e2eAgent(t *testing.T, mockURL string, dataDir string) (*Agent, *memory.FileStore) {
	t.Helper()
	prov, err := model.NewProvider(model.ProviderConfig{
		Name:    "openai",
		APIKey:  "test-e2e-key", // mock-only key, never a real credential
		BaseURL: mockURL,
		Model:   "mock-model",
	})
	if err != nil {
		t.Fatalf("model.NewProvider(openai, mock): %v", err)
	}
	reg, err := tools.DefaultRegistry()
	if err != nil {
		t.Fatalf("tools.DefaultRegistry: %v", err)
	}
	mem, err := memory.NewFileStore(dataDir)
	if err != nil {
		t.Fatalf("memory.NewFileStore: %v", err)
	}
	cfg := config.Default()
	cfg.MaxSteps = 5
	cfg.TimeoutSeconds = 30
	agent, err := NewAgent(cfg, prov, reg, mem)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	agent.SetLogger(log.New(io.Discard, "", 0))
	return agent, mem
}

// messagesOf decodes the "messages" array of a recorded request body.
func messagesOf(t *testing.T, req mockRequest) []map[string]any {
	t.Helper()
	raw, ok := req.Body["messages"].([]any)
	if !ok {
		t.Fatalf("request has no messages array: %v", req.Body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("message is not an object: %v", m)
		}
		out = append(out, mm)
	}
	return out
}

// findMessage returns the first message with the given role, if any.
func findMessage(msgs []map[string]any, role string) (map[string]any, bool) {
	for _, m := range msgs {
		if m["role"] == role {
			return m, true
		}
	}
	return nil, false
}

// assertMockRequestBasics checks the provider sent a well-formed
// OpenAI-compatible request to the mock.
func assertMockRequestBasics(t *testing.T, req mockRequest) {
	t.Helper()
	if req.Path != "/chat/completions" {
		t.Errorf("request path = %q, want /chat/completions", req.Path)
	}
	if req.Auth != "Bearer test-e2e-key" {
		t.Errorf("Authorization = %q, want Bearer test-e2e-key", req.Auth)
	}
	if req.Body["model"] != "mock-model" {
		t.Errorf("model = %v, want mock-model", req.Body["model"])
	}
	toolsRaw, ok := req.Body["tools"].([]any)
	if !ok || len(toolsRaw) == 0 {
		t.Errorf("request tools missing/empty: %v", req.Body["tools"])
		return
	}
	foundShell := false
	for _, td := range toolsRaw {
		if def, ok := td.(map[string]any); ok {
			if fn, ok := def["function"].(map[string]any); ok && fn["name"] == "shell_exec" {
				foundShell = true
			}
		}
	}
	if !foundShell {
		t.Errorf("request tools do not include shell_exec: %v", req.Body["tools"])
	}
}

// assertMemorySequence checks the FileStore history is exactly the
// expected role sequence with the expected content fragments.
func assertMemorySequence(t *testing.T, mem *memory.FileStore, want []struct {
	role    string
	contain string
}) {
	t.Helper()
	msgs, err := mem.RecentMessages(0)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(msgs) != len(want) {
		t.Fatalf("memory messages = %d, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i, w := range want {
		if msgs[i].Role != w.role {
			t.Errorf("memory[%d].Role = %q, want %q (msg: %+v)", i, msgs[i].Role, w.role, msgs[i])
		}
		if w.contain != "" && !strings.Contains(msgs[i].Content, w.contain) {
			t.Errorf("memory[%d].Content = %q, want it to contain %q", i, msgs[i].Content, w.contain)
		}
	}
}

// --- Tests ----------------------------------------------------------------

// TestEndToEndShellToolLoop is the headline proof: mock returns a
// shell_exec tool call ("echo hello"), the loop executes it for real,
// feeds the real output back, and the mock returns the final answer.
func TestEndToEndShellToolLoop(t *testing.T) {
	dataDir := t.TempDir()
	const finalAnswer = "Done — the command printed hello."

	mock := newMockOpenAIServer(t, func(call int, req mockRequest) (int, string) {
		switch call {
		case 1:
			assertMockRequestBasics(t, req)
			msgs := messagesOf(t, req)
			if len(msgs) < 2 || msgs[0]["role"] != "system" || msgs[len(msgs)-1]["role"] != "user" {
				t.Errorf("call 1 messages = %v, want system first and user last", msgs)
			}
			if last := msgs[len(msgs)-1]["content"]; last != "run echo hello and tell me what it printed" {
				t.Errorf("call 1 user content = %v", last)
			}
			// Arguments as a JSON string (the standard OpenAI shape),
			// content null — both must parse.
			return http.StatusOK, toolCallReply(t, "call-e2e-1", "shell_exec", `{"command":"echo hello"}`, "tool_calls")
		case 2:
			// The second request must carry the assistant tool-call
			// message and the REAL tool result (exit 0, output hello).
			msgs := messagesOf(t, req)
			assistant, ok := findMessage(msgs, "assistant")
			if !ok {
				t.Errorf("call 2 has no assistant message: %v", msgs)
			} else if _, ok := assistant["tool_calls"]; !ok {
				t.Errorf("call 2 assistant message has no tool_calls: %v", assistant)
			}
			toolMsg, ok := findMessage(msgs, "tool")
			if !ok {
				t.Errorf("call 2 has no tool-role message: %v", msgs)
			} else {
				content, _ := toolMsg["content"].(string)
				if !strings.Contains(content, "hello") || !strings.Contains(content, "exit_code: 0") {
					t.Errorf("call 2 tool result = %q, want real echo output with exit_code: 0", content)
				}
				if toolMsg["tool_call_id"] != "call-e2e-1" {
					t.Errorf("call 2 tool_call_id = %v, want call-e2e-1", toolMsg["tool_call_id"])
				}
			}
			return http.StatusOK, textReply(t, finalAnswer)
		default:
			t.Errorf("unexpected mock call %d", call)
			return http.StatusOK, textReply(t, "unexpected")
		}
	})

	agent, mem := e2eAgent(t, mock.URL(), dataDir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := agent.Run(ctx, "run echo hello and tell me what it printed")
	if err != nil {
		t.Fatalf("Agent.Run: %v", err)
	}
	if out != finalAnswer {
		t.Errorf("Run = %q, want %q", out, finalAnswer)
	}
	if got := mock.CallCount(); got != 2 {
		t.Errorf("mock calls = %d, want 2 (tool call + final answer)", got)
	}

	assertMemorySequence(t, mem, []struct {
		role    string
		contain string
	}{
		{"user", "run echo hello"},
		{"assistant", ""}, // tool-call turn: content empty, ToolCalls set
		{"tool", "hello"},
		{"assistant", finalAnswer},
	})
	// The assistant tool-call message must record the actual call.
	msgs, _ := mem.RecentMessages(0)
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Name != "shell_exec" || msgs[1].ToolCalls[0].ID != "call-e2e-1" {
		t.Errorf("memory assistant tool call = %+v, want one shell_exec call-e2e-1", msgs[1].ToolCalls)
	}
	if msgs[2].ToolCallID != "call-e2e-1" {
		t.Errorf("memory tool message ToolCallID = %q, want call-e2e-1", msgs[2].ToolCallID)
	}

	// Persistence: close via Shutdown, reopen the same data dir, and
	// the full 4-message history must still be there.
	if err := agent.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	reopened, err := memory.NewFileStore(dataDir)
	if err != nil {
		t.Fatalf("reopen FileStore: %v", err)
	}
	defer reopened.Close()
	assertMemorySequence(t, reopened, []struct {
		role    string
		contain string
	}{
		{"user", "run echo hello"},
		{"assistant", ""},
		{"tool", "hello"},
		{"assistant", finalAnswer},
	})
}

// TestEndToEndFileToolLoop proves the file-tool path end to end: the
// mock asks for a file_write inside t.TempDir(), the loop writes it for
// real, and the final answer follows. The file on disk is asserted.
func TestEndToEndFileToolLoop(t *testing.T) {
	dataDir := t.TempDir()
	notePath := filepath.Join(t.TempDir(), "note.txt")
	const noteContent = "hello from orion e2e"
	const finalAnswer = "I wrote the note."

	mock := newMockOpenAIServer(t, func(call int, req mockRequest) (int, string) {
		switch call {
		case 1:
			args, _ := json.Marshal(map[string]string{"path": notePath, "content": noteContent})
			return http.StatusOK, toolCallReply(t, "call-file-1", "file_write", string(args), "tool_calls")
		case 2:
			msgs := messagesOf(t, req)
			toolMsg, ok := findMessage(msgs, "tool")
			if !ok {
				t.Errorf("call 2 has no tool-role message: %v", msgs)
			} else if content, _ := toolMsg["content"].(string); !strings.Contains(content, "wrote") || !strings.Contains(content, notePath) {
				t.Errorf("call 2 tool result = %q, want file_write confirmation for %s", content, notePath)
			}
			return http.StatusOK, textReply(t, finalAnswer)
		default:
			t.Errorf("unexpected mock call %d", call)
			return http.StatusOK, textReply(t, "unexpected")
		}
	})

	agent, mem := e2eAgent(t, mock.URL(), dataDir)
	agent.SetLogger(log.New(io.Discard, "", 0))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := agent.Run(ctx, "write a note file and confirm")
	if err != nil {
		t.Fatalf("Agent.Run: %v", err)
	}
	if out != finalAnswer {
		t.Errorf("Run = %q, want %q", out, finalAnswer)
	}
	if got := mock.CallCount(); got != 2 {
		t.Errorf("mock calls = %d, want 2", got)
	}
	// The file must really exist on disk with the exact content.
	got, err := os.ReadFile(notePath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", notePath, err)
	}
	if string(got) != noteContent {
		t.Errorf("file content = %q, want %q", string(got), noteContent)
	}
	assertMemorySequence(t, mem, []struct {
		role    string
		contain string
	}{
		{"user", "write a note file"},
		{"assistant", ""},
		{"tool", "wrote"},
		{"assistant", finalAnswer},
	})
}

// TestEndToEndObjectArgumentsToolCall covers a real-world wire variant:
// some OpenAI-compatible servers send tool-call arguments as a JSON
// OBJECT, not a JSON string. The provider must accept both shapes and
// the loop must still execute the tool for real. (Regression test for
// the openai.go tool-call parsing fix.)
func TestEndToEndObjectArgumentsToolCall(t *testing.T) {
	dataDir := t.TempDir()
	const finalAnswer = "Done — the command printed hello."

	mock := newMockOpenAIServer(t, func(call int, req mockRequest) (int, string) {
		switch call {
		case 1:
			// Arguments as a JSON object, not a string.
			return http.StatusOK, toolCallReply(t, "call-obj-1", "shell_exec",
				map[string]any{"command": "echo hello"}, "tool_calls")
		case 2:
			msgs := messagesOf(t, req)
			toolMsg, ok := findMessage(msgs, "tool")
			if !ok {
				t.Errorf("call 2 has no tool-role message: %v", msgs)
			} else if content, _ := toolMsg["content"].(string); !strings.Contains(content, "hello") {
				t.Errorf("call 2 tool result = %q, want real echo output", content)
			}
			return http.StatusOK, textReply(t, finalAnswer)
		default:
			t.Errorf("unexpected mock call %d", call)
			return http.StatusOK, textReply(t, "unexpected")
		}
	})

	agent, mem := e2eAgent(t, mock.URL(), dataDir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := agent.Run(ctx, "run echo hello and tell me what it printed")
	if err != nil {
		t.Fatalf("Agent.Run with object-form arguments: %v", err)
	}
	if out != finalAnswer {
		t.Errorf("Run = %q, want %q", out, finalAnswer)
	}
	if got := mock.CallCount(); got != 2 {
		t.Errorf("mock calls = %d, want 2", got)
	}
	assertMemorySequence(t, mem, []struct {
		role    string
		contain string
	}{
		{"user", "run echo hello"},
		{"assistant", ""},
		{"tool", "hello"},
		{"assistant", finalAnswer},
	})
}

// TestEndToEndBlockedCommandFedBack proves the denylist holds inside
// the loop: the mock asks for "rm -rf /", the tool refuses it (the
// command is never executed — the check is string-only), the denial is
// fed back to the model as the tool result, and the loop still reaches
// a final answer.
func TestEndToEndBlockedCommandFedBack(t *testing.T) {
	// String-only denylist check, per the safety rule: never run a
	// live destructive command in a test.
	if !IsBlockedCommand("rm -rf /") {
		t.Fatal("IsBlockedCommand(\"rm -rf /\") = false, want true")
	}
	dataDir := t.TempDir()
	const finalAnswer = "That command is blocked, so I did not run it."

	mock := newMockOpenAIServer(t, func(call int, req mockRequest) (int, string) {
		switch call {
		case 1:
			return http.StatusOK, toolCallReply(t, "call-block-1", "shell_exec", `{"command":"rm -rf /"}`, "tool_calls")
		case 2:
			msgs := messagesOf(t, req)
			toolMsg, ok := findMessage(msgs, "tool")
			if !ok {
				t.Errorf("call 2 has no tool-role message: %v", msgs)
			} else if content, _ := toolMsg["content"].(string); !strings.Contains(strings.ToLower(content), "blocked") {
				t.Errorf("call 2 tool result = %q, want it to report the denial", content)
			}
			return http.StatusOK, textReply(t, finalAnswer)
		default:
			t.Errorf("unexpected mock call %d", call)
			return http.StatusOK, textReply(t, "unexpected")
		}
	})

	agent, mem := e2eAgent(t, mock.URL(), dataDir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := agent.Run(ctx, "delete everything")
	if err != nil {
		t.Fatalf("Agent.Run: %v", err)
	}
	if out != finalAnswer {
		t.Errorf("Run = %q, want %q", out, finalAnswer)
	}
	assertMemorySequence(t, mem, []struct {
		role    string
		contain string
	}{
		{"user", "delete everything"},
		{"assistant", ""},
		{"tool", "blocked"},
		{"assistant", finalAnswer},
	})
}

// TestOllamaSmokeIfAvailable smoke-tests the Ollama provider path when
// a local Ollama is actually installed and reachable. On this machine
// Ollama is absent, so the test records a skip instead of failing.
// It never starts or installs Ollama itself.
func TestOllamaSmokeIfAvailable(t *testing.T) {
	if _, err := exec.LookPath("ollama"); err != nil {
		t.Skip("ollama binary not installed on this machine; Ollama provider path not smoke-tested")
	}
	// Ollama is present: check the daemon answers before trying a chat.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://localhost:11434/api/tags")
	if err != nil {
		t.Skipf("ollama binary present but daemon not reachable at http://localhost:11434: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("ollama daemon returned status %d for /api/tags; skipping live chat smoke test", resp.StatusCode)
	}
	prov, err := model.NewProvider(model.ProviderConfig{Name: "ollama", BaseURL: "http://localhost:11434", Model: "llama3.1"})
	if err != nil {
		t.Fatalf("model.NewProvider(ollama): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	chatResp, err := prov.Chat(ctx, model.ChatRequest{
		Messages: []model.Message{{Role: "user", Content: "Reply with exactly: ollama-ok"}},
	})
	if err != nil {
		t.Skipf("ollama Chat failed (no suitable model may be pulled): %v", err)
	}
	if chatResp == nil || chatResp.Message.Content == "" {
		t.Fatalf("ollama Chat returned empty response: %+v", chatResp)
	}
	t.Logf("ollama smoke test reply: %q", chatResp.Message.Content)
}
