package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func toolErrKind(t *testing.T, err error) string {
	t.Helper()
	var te *ToolError
	if !errors.As(err, &te) {
		t.Fatalf("expected *ToolError, got %T (%v)", err, err)
	}
	return te.Kind
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---------------- shell ----------------

func TestShellExecSuccess(t *testing.T) {
	tool := &shellExecTool{}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "echo hello"}))
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(out, "exit_code: 0") || !strings.Contains(out, "hello") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestShellExecBlocked(t *testing.T) {
	tool := &shellExecTool{}
	for _, cmd := range []string{"rm -rf /", "/bin/rm -rf /", "rm -rf / --no-preserve-root", "sudo rm -rf /"} {
		_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": cmd}))
		if err == nil {
			t.Fatalf("command %q was not blocked", cmd)
		}
		if kind := toolErrKind(t, err); kind != "denied" {
			t.Fatalf("command %q: expected kind denied, got %q (%v)", cmd, kind, err)
		}
	}
}

func TestShellExecTimeout(t *testing.T) {
	tool := &shellExecTool{}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "sleep 5", "timeout_seconds": 1}))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if kind := toolErrKind(t, err); kind != "timeout" {
		t.Fatalf("expected kind timeout, got %q (%v)", kind, err)
	}
}

func TestShellExecInvalidArgs(t *testing.T) {
	tool := &shellExecTool{}
	for _, args := range []json.RawMessage{
		mustJSON(t, map[string]any{"command": ""}),
		mustJSON(t, map[string]any{"timeout_seconds": 1}),
		mustJSON(t, map[string]any{"command": "echo hi", "timeout_seconds": 999}),
		mustJSON(t, map[string]any{"command": "echo hi", "timeout_seconds": 0}),
	} {
		if _, err := tool.Execute(context.Background(), args); err == nil {
			t.Fatalf("args %s: expected error", args)
		} else if kind := toolErrKind(t, err); kind != "invalid_args" {
			t.Fatalf("args %s: expected invalid_args, got %q", args, kind)
		}
	}
}

func TestShellExecCancelledContext(t *testing.T) {
	tool := &shellExecTool{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tool.Execute(ctx, mustJSON(t, map[string]any{"command": "echo hello"}))
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestShellExecOutputCap(t *testing.T) {
	tool := &shellExecTool{}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "yes x | head -c 200000"}))
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(out, "[truncated]") {
		t.Fatalf("expected [truncated] marker, output len %d", len(out))
	}
	if len(out) > 64*1024+256 {
		t.Fatalf("output not capped: %d bytes", len(out))
	}
}

func TestShellExecNonZeroExit(t *testing.T) {
	tool := &shellExecTool{}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "exit 3"}))
	if err != nil {
		t.Fatalf("non-zero exit should not be an error: %v", err)
	}
	if !strings.Contains(out, "exit_code: 3") {
		t.Fatalf("expected exit_code: 3, got %q", out)
	}
}

// ---------------- files ----------------

func TestFileWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/sub/nested/hello.txt"
	w := &fileWriteTool{}
	out, err := w.Execute(context.Background(), mustJSON(t, map[string]any{"path": path, "content": "hello world"}))
	if err != nil {
		t.Fatalf("file_write failed: %v", err)
	}
	if !strings.Contains(out, "wrote 11 bytes") {
		t.Fatalf("unexpected write output: %q", out)
	}
	r := &fileReadTool{}
	got, err := r.Execute(context.Background(), mustJSON(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("file_read failed: %v", err)
	}
	if got != "hello world" {
		t.Fatalf("got %q, want %q", got, "hello world")
	}
}

func TestFileWriteRefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	w := &fileWriteTool{}
	_, err := w.Execute(context.Background(), mustJSON(t, map[string]any{"path": dir, "content": "x"}))
	if err == nil {
		t.Fatal("expected error writing to a directory path")
	}
	if kind := toolErrKind(t, err); kind != "invalid_args" {
		t.Fatalf("expected invalid_args, got %q", kind)
	}
}

func TestFileReadMissingAndEmptyPath(t *testing.T) {
	r := &fileReadTool{}
	_, err := r.Execute(context.Background(), mustJSON(t, map[string]any{"path": "/nonexistent/orion-test-file.txt"}))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	_, err = r.Execute(context.Background(), mustJSON(t, map[string]any{"path": ""}))
	if err == nil {
		t.Fatal("expected error for empty path")
	}
	if kind := toolErrKind(t, err); kind != "invalid_args" {
		t.Fatalf("empty path: expected invalid_args, got %q", kind)
	}
	_, err = r.Execute(context.Background(), mustJSON(t, map[string]any{"path": t.TempDir()}))
	if err == nil {
		t.Fatal("expected error reading a directory")
	}
}

func TestFileEdit(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/edit.txt"
	w := &fileWriteTool{}
	if _, err := w.Execute(context.Background(), mustJSON(t, map[string]any{"path": path, "content": "alpha beta gamma"})); err != nil {
		t.Fatal(err)
	}
	e := &fileEditTool{}
	if _, err := e.Execute(context.Background(), mustJSON(t, map[string]any{"path": path, "old_text": "beta", "new_text": "BETA"})); err != nil {
		t.Fatalf("file_edit failed: %v", err)
	}
	r := &fileReadTool{}
	got, _ := r.Execute(context.Background(), mustJSON(t, map[string]any{"path": path}))
	if got != "alpha BETA gamma" {
		t.Fatalf("got %q", got)
	}
	// Absent old_text.
	if _, err := e.Execute(context.Background(), mustJSON(t, map[string]any{"path": path, "old_text": "zzz", "new_text": "y"})); err == nil {
		t.Fatal("expected error for absent old_text")
	}
	// Ambiguous old_text.
	if _, err := w.Execute(context.Background(), mustJSON(t, map[string]any{"path": path, "content": "dup dup dup"})); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(context.Background(), mustJSON(t, map[string]any{"path": path, "old_text": "dup", "new_text": "x"})); err == nil {
		t.Fatal("expected error for ambiguous old_text")
	}
}

func TestFileList(t *testing.T) {
	dir := t.TempDir()
	w := &fileWriteTool{}
	for _, name := range []string{"b.txt", "a.txt"} {
		if _, err := w.Execute(context.Background(), mustJSON(t, map[string]any{"path": dir + "/" + name, "content": "x"})); err != nil {
			t.Fatal(err)
		}
	}
	l := &fileListTool{}
	out, err := l.Execute(context.Background(), mustJSON(t, map[string]any{"path": dir}))
	if err != nil {
		t.Fatalf("file_list failed: %v", err)
	}
	// Sorted: a.txt before b.txt.
	if ia, ib := strings.Index(out, "a.txt"), strings.Index(out, "b.txt"); ia < 0 || ib < 0 || ia > ib {
		t.Fatalf("expected sorted listing, got %q", out)
	}
	if !strings.Contains(out, "file") || !strings.Contains(out, "1 bytes") {
		t.Fatalf("expected type/size info, got %q", out)
	}
	_, err = l.Execute(context.Background(), mustJSON(t, map[string]any{"path": dir + "/nope"}))
	if err == nil {
		t.Fatal("expected error for missing directory")
	}
}

// ---------------- web ----------------

func TestWebFetchSuccessAnd404(t *testing.T) {
	// The test server is on 127.0.0.1, which the SSRF guard blocks
	// by default; the override is test-only.
	allowPrivateFetchHosts = true
	t.Cleanup(func() { allowPrivateFetchHosts = false })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			if r.Header.Get("User-Agent") != "Orion/0.1" {
				t.Errorf("unexpected User-Agent %q", r.Header.Get("User-Agent"))
			}
			_, _ = w.Write([]byte("hello web"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	f := &webFetchTool{}
	out, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL + "/ok"}))
	if err != nil {
		t.Fatalf("web_fetch failed: %v", err)
	}
	if out != "hello web" {
		t.Fatalf("got %q", out)
	}
	_, err = f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL + "/missing"}))
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected status in error, got %v", err)
	}
}

func TestWebFetchRejectsNonHTTP(t *testing.T) {
	f := &webFetchTool{}
	for _, u := range []string{"ftp://example.com/x", "file:///etc/passwd", "not a url", ""} {
		if _, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": u})); err == nil {
			t.Fatalf("url %q: expected error", u)
		}
	}
}

func TestWebFetchMaxBytes(t *testing.T) {
	allowPrivateFetchHosts = true
	t.Cleanup(func() { allowPrivateFetchHosts = false })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 1000)))
	}))
	defer srv.Close()
	f := &webFetchTool{}
	out, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL, "max_bytes": 10}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[truncated]") || !strings.HasPrefix(out, "aaaaaaaaaa") {
		t.Fatalf("unexpected capped output: %q", out)
	}
}

func TestWebSearchViaAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"AbstractText": "Go is a programming language.",
			"AbstractURL": "https://go.dev",
			"Heading": "Go",
			"RelatedTopics": [
				{"Text": "Go programming language", "FirstURL": "https://go.dev/doc"},
				{"Text": "Golang tutorials", "FirstURL": "https://example.com/golang"}
			]
		}`))
	}))
	defer srv.Close()

	oldAPI, oldLite := searchAPIBase, searchLiteBase
	searchAPIBase, searchLiteBase = srv.URL, srv.URL
	defer func() { searchAPIBase, searchLiteBase = oldAPI, oldLite }()

	s := &webSearchTool{}
	out, err := s.Execute(context.Background(), mustJSON(t, map[string]any{"query": "go language", "count": 3}))
	if err != nil {
		t.Fatalf("web_search failed: %v", err)
	}
	if !strings.Contains(out, "Go is a programming language") || !strings.Contains(out, "1.") || !strings.Contains(out, "https://go.dev") {
		t.Fatalf("unexpected search output: %q", out)
	}
}

func TestWebSearchLiteFallback(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"AbstractText": "", "RelatedTopics": []}`))
	}))
	defer apiSrv.Close()
	liteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body>
			<a class="result-link" href="https://example.com/one">First Result</a>
			<a class="result-link" href="https://example.com/two">Second Result</a>
		</body></html>`))
	}))
	defer liteSrv.Close()

	oldAPI, oldLite := searchAPIBase, searchLiteBase
	searchAPIBase, searchLiteBase = apiSrv.URL, liteSrv.URL
	defer func() { searchAPIBase, searchLiteBase = oldAPI, oldLite }()

	s := &webSearchTool{}
	out, err := s.Execute(context.Background(), mustJSON(t, map[string]any{"query": "fallback test"}))
	if err != nil {
		t.Fatalf("web_search failed: %v", err)
	}
	if !strings.Contains(out, "First Result") || !strings.Contains(out, "https://example.com/one") {
		t.Fatalf("expected lite fallback results, got %q", out)
	}
}

func TestWebSearchEmptyQuery(t *testing.T) {
	s := &webSearchTool{}
	if _, err := s.Execute(context.Background(), mustJSON(t, map[string]any{"query": ""})); err == nil {
		t.Fatal("expected error for empty query")
	}
}

// ---------------- misc ----------------

func TestCalculatorTable(t *testing.T) {
	tool := &calculatorTool{}
	cases := []struct {
		expr string
		want string // substring of the result
	}{
		{"(2+3)*4", "20"},
		{"2+3*4", "14"},
		{"10/4", "2.5"},
		{"2^10", "1024"},
		{"10%3", "1"},
		{"-5+3", "-2"},
		{"-(2+3)", "-5"},
		{"sqrt(16)", "4"},
		{"abs(-7)", "7"},
		{"sin(0)", "0"},
		{"cos(0)", "1"},
		{"log(100)", "2"},
		{"ln(e)", "1"},
		{"pi", "3.14159265358979"},
		{"2*pi", "6.28318530717959"},
		{"2^3^2", "512"}, // right-associative: 2^(3^2)
	}
	for _, tc := range cases {
		out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"expression": tc.expr}))
		if err != nil {
			t.Errorf("%q: unexpected error %v", tc.expr, err)
			continue
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("%q: got %q, want it to contain %q", tc.expr, out, tc.want)
		}
	}
}

func TestCalculatorErrors(t *testing.T) {
	tool := &calculatorTool{}
	for _, expr := range []string{
		"1/0", "1%0", "sqrt(-1)", "log(0)", "ln(-5)", "2+", "(2+3", "foo(1)", "2 & 3", "",
	} {
		if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"expression": expr})); err == nil {
			t.Errorf("%q: expected error", expr)
		}
	}
}

func TestDatetime(t *testing.T) {
	tool := &datetimeTool{}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"timezone": "Asia/Singapore"}))
	if err != nil {
		t.Fatalf("datetime failed: %v", err)
	}
	if !strings.Contains(out, "rfc3339:") || !strings.Contains(out, "+08:00") {
		t.Fatalf("unexpected datetime output: %q", out)
	}
	out, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{"timezone": "UTC", "format": "2006-01-02"}))
	if err != nil {
		t.Fatalf("datetime with format failed: %v", err)
	}
	if len(out) != 10 || out[4] != '-' {
		t.Fatalf("unexpected formatted date: %q", out)
	}
	_, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{"timezone": "Not/AZone"}))
	if err == nil {
		t.Fatal("expected error for invalid timezone")
	}
	if kind := toolErrKind(t, err); kind != "invalid_args" {
		t.Fatalf("expected invalid_args, got %q", kind)
	}
}

func TestProcessList(t *testing.T) {
	tool := &processListTool{}
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("process_list failed: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("process_list returned empty output")
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 { // header + at least one process
		t.Fatalf("expected header plus processes, got %q", out)
	}
	if !strings.HasPrefix(lines[0], "PID") {
		t.Fatalf("expected PID header, got %q", lines[0])
	}
}

// ---------------- registry ----------------

func TestDefaultRegistry(t *testing.T) {
	r, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry failed: %v", err)
	}
	want := []string{"calculator", "datetime", "file_edit", "file_list", "file_read", "file_write", "process_list", "shell_exec", "web_fetch", "web_search"}
	if got := len(r.List()); got != len(want) {
		t.Fatalf("expected %d tools, got %d", len(want), got)
	}
	for _, name := range want {
		tool, ok := r.Get(name)
		if !ok {
			t.Fatalf("tool %q not registered", name)
		}
		def := tool.Definition()
		if def.Name != name || def.Description == "" || def.Parameters == nil {
			t.Fatalf("tool %q has incomplete definition: %+v", name, def)
		}
	}
	// Execute through the registry end-to-end.
	out, err := r.Execute(context.Background(), "calculator", mustJSON(t, map[string]any{"expression": "6*7"}))
	if err != nil {
		t.Fatalf("registry Execute failed: %v", err)
	}
	if !strings.Contains(out, "42") {
		t.Fatalf("unexpected registry result: %q", out)
	}
	_, err = r.Execute(context.Background(), "no_such_tool", mustJSON(t, map[string]any{}))
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if len(r.Definitions()) != len(want) {
		t.Fatalf("Definitions returned %d entries", len(r.Definitions()))
	}
}
