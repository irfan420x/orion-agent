package tools

// P2 follow-up tests (audit 2026-11-13): file-tool truncation/modes/caps,
// registry guards, calculator parser bounds, web helpers, and search.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
)

// --- P2-23: file_read truncation marker ---

// TestFileReadMarksTruncationWhenFileGrows: a file that grows between
// Stat and ReadFile must not silently truncate to 1 MiB. A FIFO reports
// size 0 at Stat but yields more than 1 MiB of content, exercising
// exactly that race deterministically: the read must be capped and
// marked with [truncated to 1MB].
func TestFileReadMarksTruncationWhenFileGrows(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "growing")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	content := strings.Repeat("x", maxFileRead+100)
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.Write([]byte(content))
	}()

	tool := &fileReadTool{roots: []string{dir}}
	type result struct {
		out string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": fifo}))
		ch <- result{out, err}
	}()
	var res result
	select {
	case res = <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("file_read on the growing FIFO timed out")
	}
	if res.err != nil {
		t.Fatalf("file_read: %v", res.err)
	}
	want := strings.Repeat("x", maxFileRead) + "\n[truncated to 1MB]"
	if res.out != want {
		t.Fatalf("file_read output len = %d, want %d; has marker = %v",
			len(res.out), len(want), strings.HasSuffix(res.out, "\n[truncated to 1MB]"))
	}
}

// TestFileReadTruncationDoesNotSplitUTF8: when the 1 MiB cut lands in
// the middle of a multi-byte rune, the cut backs off to a rune
// boundary instead of emitting invalid UTF-8.
func TestFileReadTruncationDoesNotSplitUTF8(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "growing-utf8")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	// (maxFileRead-1) ASCII bytes, then "é" (2 bytes) straddling the
	// 1 MiB boundary, then more bytes: the cut must drop the partial
	// rune, not half of it.
	content := strings.Repeat("a", maxFileRead-1) + "é" + strings.Repeat("b", 10)
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.Write([]byte(content))
	}()

	tool := &fileReadTool{roots: []string{dir}}
	type result struct {
		out string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": fifo}))
		ch <- result{out, err}
	}()
	var res result
	select {
	case res = <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("file_read on the growing FIFO timed out")
	}
	if res.err != nil {
		t.Fatalf("file_read: %v", res.err)
	}
	if !utf8.ValidString(res.out) {
		t.Fatalf("file_read output is not valid UTF-8 (len %d)", len(res.out))
	}
	want := strings.Repeat("a", maxFileRead-1) + "\n[truncated to 1MB]"
	if res.out != want {
		t.Fatalf("file_read output = %d bytes, want %d (rune-safe cut)", len(res.out), len(want))
	}
}

// --- P2-24: file modes, write cap, atomic edit ---

// TestFileWriteModesAndCap: agent-written files are 0600, parent
// directories created by file_write are 0700, and content over the
// 1 MiB cap is refused (while exactly 1 MiB is accepted).
func TestFileWriteModesAndCap(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tool := &fileWriteTool{roots: []string{dir}}

	nested := filepath.Join(dir, "a", "b", "c.txt")
	if _, err := tool.Execute(ctx, mustJSON(t, map[string]any{"path": nested, "content": "hi"})); err != nil {
		t.Fatalf("file_write nested: %v", err)
	}
	for _, d := range []string{filepath.Join(dir, "a"), filepath.Join(dir, "a", "b")} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatalf("stat %s: %v", d, err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("parent dir %s mode = %o, want 0700", d, info.Mode().Perm())
		}
	}
	info, err := os.Stat(nested)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("written file mode = %o, want 0600", info.Mode().Perm())
	}

	big := filepath.Join(dir, "big.txt")
	if _, err := tool.Execute(ctx, mustJSON(t, map[string]any{"path": big, "content": strings.Repeat("x", maxFileWrite+1)})); err == nil {
		t.Fatal("file_write of 1MiB+1 bytes: expected refusal")
	}
	if _, err := os.Stat(big); !os.IsNotExist(err) {
		t.Fatalf("over-cap write must not create the file (stat err = %v)", err)
	}
	if _, err := tool.Execute(ctx, mustJSON(t, map[string]any{"path": filepath.Join(dir, "exact.txt"), "content": strings.Repeat("x", maxFileWrite)})); err != nil {
		t.Fatalf("file_write of exactly 1MiB: %v, want success", err)
	}
}

// TestFileEditAtomicAndSizeCap: file_edit writes via temp+rename (no
// temp files left behind, failed edits leave the file untouched),
// preserves the file's existing mode, and refuses an edit whose result
// would exceed the 1 MiB write cap.
func TestFileEditAtomicAndSizeCap(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	write := &fileWriteTool{roots: []string{dir}}
	edit := &fileEditTool{roots: []string{dir}}

	// A file edited by file_edit keeps the mode it already had
	// (umask may have narrowed the 0644 requested at creation; the
	// point is the edit preserves whatever mode is there).
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("hello world"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat before edit: %v", err)
	}
	if _, err := edit.Execute(ctx, mustJSON(t, map[string]any{"path": p, "old_text": "world", "new_text": "there"})); err != nil {
		t.Fatalf("file_edit: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil || string(data) != "hello there" {
		t.Fatalf("edited content = %q, err = %v", data, err)
	}
	if after, err := os.Stat(p); err != nil || after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("edited file mode = %v, want preserved %v", after.Mode(), before.Mode())
	}
	// No temp files left behind by the atomic rename.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".orion-") {
			t.Fatalf("temp file left behind after edit: %s", e.Name())
		}
	}

	// A failed edit leaves the file untouched.
	if _, err := edit.Execute(ctx, mustJSON(t, map[string]any{"path": p, "old_text": "missing", "new_text": "x"})); err == nil {
		t.Fatal("file_edit with absent old_text: expected error")
	}
	if data, _ := os.ReadFile(p); string(data) != "hello there" {
		t.Fatalf("failed edit changed the file: %q", data)
	}

	// An edit whose result exceeds the write cap is refused, file intact.
	small := filepath.Join(dir, "small.txt")
	if _, err := write.Execute(ctx, mustJSON(t, map[string]any{"path": small, "content": "0123456789"})); err != nil {
		t.Fatalf("file_write small: %v", err)
	}
	if _, err := edit.Execute(ctx, mustJSON(t, map[string]any{"path": small, "old_text": "0", "new_text": strings.Repeat("y", maxFileWrite+1)})); err == nil {
		t.Fatal("file_edit growing past 1MiB: expected refusal")
	}
	if data, _ := os.ReadFile(small); string(data) != "0123456789" {
		t.Fatalf("over-cap edit changed the file: len %d", len(data))
	}
}

// --- P2-25: file_list cap ---

// TestFileListCapsEntriesAndReportsTotal: a directory with more than
// 200 entries lists the first 200 and reports the total; a small
// directory lists everything with no truncation line.
func TestFileListCapsEntriesAndReportsTotal(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for i := 0; i < 250; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	tool := &fileListTool{roots: []string{dir}}
	out, err := tool.Execute(ctx, mustJSON(t, map[string]any{"path": dir}))
	if err != nil {
		t.Fatalf("file_list: %v", err)
	}
	if !strings.Contains(out, "(250 total entries, showing first 200)") {
		t.Fatalf("file_list output missing total line: %q", out[len(out)-200:])
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// header + 200 entries + total line
	if len(lines) != 202 {
		t.Fatalf("file_list lines = %d, want 202 (header + 200 + total)", len(lines))
	}
	if !strings.Contains(lines[1], "f000.txt") || !strings.Contains(lines[200], "f199.txt") {
		t.Fatalf("file_list did not show the first 200 entries in order")
	}

	small := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(small, fmt.Sprintf("g%d.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	smallTool := &fileListTool{roots: []string{small}}
	out, err = smallTool.Execute(ctx, mustJSON(t, map[string]any{"path": small}))
	if err != nil {
		t.Fatalf("file_list small: %v", err)
	}
	if strings.Contains(out, "total entries") {
		t.Fatalf("small directory must not report truncation: %q", out)
	}
	if lines := strings.Split(strings.TrimRight(out, "\n"), "\n"); len(lines) != 4 {
		t.Fatalf("small file_list lines = %d, want 4 (header + 3)", len(lines))
	}
}

// --- P2-26: registry guards ---

type namedStubTool struct{ name string }

func (s *namedStubTool) Name() string { return s.name }

// RiskClass returns the gate's class for the tool's name:
// Register requires the declaration to match
// permissions.Classify.
func (s *namedStubTool) RiskClass() permissions.Class {
	return permissions.Classify(s.name)
}
func (s *namedStubTool) Description() string {
	return "stub tool for registry guard tests"
}
func (s *namedStubTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: s.name, Description: s.Description(), Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}
}
func (s *namedStubTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "ok", nil
}

// TestRegistryRegisterGuards: Register(nil) and empty/whitespace tool
// names are rejected with an error, not a panic, and the registry stays
// usable afterwards.
func TestRegistryRegisterGuards(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatal("Register(nil): expected error, got nil")
	}
	for _, name := range []string{"", "   "} {
		if err := r.Register(&namedStubTool{name: name}); err == nil {
			t.Fatalf("Register(name %q): expected error, got nil", name)
		}
	}
	if err := r.Register(&namedStubTool{name: "ok"}); err != nil {
		t.Fatalf("Register(valid tool): %v", err)
	}
	if _, ok := r.Get("ok"); !ok {
		t.Fatal("registered tool not retrievable")
	}
	if err := r.Register(&namedStubTool{name: "ok"}); err == nil {
		t.Fatal("Register(duplicate): expected error, got nil")
	}
}

// --- P2-27: calculator parser bounds + registry recover ---

// TestCalcParserPeekNextBounds: peek/next past the EOF token return the
// EOF token instead of panicking with an index out of range, and
// expressions that run the parser off the end still fail cleanly.
func TestCalcParserPeekNextBounds(t *testing.T) {
	p := &calcParser{toks: []calcToken{{kind: tokEOF}}}
	for i := 0; i < 5; i++ {
		if tok := p.next(); tok.kind != tokEOF {
			t.Fatalf("next() past EOF = kind %v, want EOF", tok.kind)
		}
		if tok := p.peek(); tok.kind != tokEOF {
			t.Fatalf("peek() past EOF = kind %v, want EOF", tok.kind)
		}
	}
	empty := &calcParser{}
	if tok := empty.peek(); tok.kind != tokEOF {
		t.Fatalf("peek() on empty parser = kind %v, want EOF", tok.kind)
	}
	if tok := empty.next(); tok.kind != tokEOF {
		t.Fatalf("next() on empty parser = kind %v, want EOF", tok.kind)
	}
	for _, expr := range []string{"1 +", "sqrt(", "((1)", "2 ^", "1 + * 2", "-"} {
		if _, err := evalExpression(expr); err == nil {
			t.Fatalf("evalExpression(%q): expected error, got nil", expr)
		}
	}
}

type panicTool struct{}

func (panicTool) Name() string { return "panicker" }

// RiskClass declares the tool Safe (Register requires a class
// matching permissions.Classify; "panicker" is not in the gate's maps).
func (panicTool) RiskClass() permissions.Class { return permissions.ClassSafe }
func (panicTool) Description() string {
	return "tool that panics, for the recover test"
}
func (panicTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: "panicker", Description: "panics", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}
}
func (panicTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	panic("boom-test-panic")
}

// TestRegistryExecuteRecoversToolPanic: a tool that panics is reported
// as an execution error and the daemon (and the registry) survive.
func TestRegistryExecuteRecoversToolPanic(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(panicTool{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err := r.Execute(context.Background(), "panicker", json.RawMessage("{}"))
	if err == nil {
		t.Fatal("Execute(panicking tool): expected error, got nil (panic escaped)")
	}
	var te *ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err type = %T, want *ToolError", err)
	}
	if te.Kind != "execution" {
		t.Fatalf("kind = %q, want execution", te.Kind)
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err = %q, want it to mention the panic", err.Error())
	}
	// The registry is still usable after the panic.
	if err := r.Register(&stubTool{output: "alive"}); err != nil {
		t.Fatalf("Register after panic: %v", err)
	}
	out, err := r.Execute(context.Background(), "stub", json.RawMessage("{}"))
	if err != nil || out != "alive" {
		t.Fatalf("Execute after panic: out=%q err=%v", out, err)
	}
}

// --- P2-28: web helpers ---

// TestFirstSentenceRuneSafe: firstSentence never splits a UTF-8 rune,
// whether it truncates at the 120-rune cap or cuts at a sentence end.
func TestFirstSentenceRuneSafe(t *testing.T) {
	long := strings.Repeat("é", 200)
	got := firstSentence(long)
	if !utf8.ValidString(got) {
		t.Fatal("firstSentence of multi-byte text is not valid UTF-8")
	}
	if n := len([]rune(got)); n != 121 { // 120 runes + "…"
		t.Fatalf("firstSentence len = %d runes, want 121", n)
	}
	multi := strings.Repeat("é", 50) + ". rest of the sentence"
	if got := firstSentence(multi); got != strings.Repeat("é", 50)+"." {
		t.Fatalf("firstSentence(%q) = %q, want the first sentence", multi, got)
	}
	if got := firstSentence("short. tail"); got != "short." {
		t.Fatalf("firstSentence(\"short. tail\") = %q, want %q", got, "short.")
	}
	if got := firstSentence("no period here"); got != "no period here" {
		t.Fatalf("firstSentence short input = %q", got)
	}
}

// TestHTMLUnescapeHexAndInvalidEntities: hex entities (&#x41;, &#X41;)
// decode, and an invalid entity is left in place while decoding
// continues past it instead of stopping.
func TestHTMLUnescapeHexAndInvalidEntities(t *testing.T) {
	cases := map[string]string{
		"&#x41;&#X42;&#67;":  "ABC",
		"a&#zz;b&#65;c":      "a&#zz;bAc",
		"&#65":               "&#65",
		"&#x41;&#bad;&#x42;": "A&#bad;B",
		"&amp;&#x26;":        "&&",
		"&#x27;":             "'",
		"plain &lt;text&gt;": "plain <text>",
		"&#x1F600;":          "😀",
		"&#x41;&#x":          "A&#x",
		"&#;":                "&#;",
	}
	for in, want := range cases {
		if got := htmlUnescape(in); got != want {
			t.Errorf("htmlUnescape(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestWebFetchNon2xxIncludesBodyDetailAndRedactsCredentials: a non-2xx
// response carries a diagnostic snippet from the body, and a URL with
// userinfo credentials never leaks the password into the error.
func TestWebFetchNon2xxIncludesBodyDetailAndRedactsCredentials(t *testing.T) {
	allowPrivateFetchHosts = true
	t.Cleanup(func() { allowPrivateFetchHosts = false })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "quota exceeded for this key")
	}))
	defer srv.Close()

	f := &webFetchTool{}
	_, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL + "/nope"}))
	if err == nil {
		t.Fatal("web_fetch 403: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("web_fetch error = %q, want status and body detail", err.Error())
	}

	// Same 403, but the URL carries userinfo credentials.
	withCreds := strings.Replace(srv.URL, "http://", "http://user:fake-password-123@", 1)
	_, err = f.Execute(context.Background(), mustJSON(t, map[string]any{"url": withCreds + "/nope"}))
	if err == nil {
		t.Fatal("web_fetch 403 with userinfo URL: expected error, got nil")
	}
	if strings.Contains(err.Error(), "fake-password-123") {
		t.Fatalf("web_fetch error leaks URL credentials: %q", err.Error())
	}
	if strings.Contains(err.Error(), "http://user:fake-password-123@") {
		t.Fatalf("web_fetch error contains the raw URL: %q", err.Error())
	}
}

// roundTripFunc is an http.RoundTripper implemented as a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestSearchDoesNotUseDefaultClient: with http.DefaultClient replaced
// by a transport that always fails, both search paths must still work
// against httptest servers — proving they use the dedicated client.
// The dedicated client also carries a timeout.
func TestSearchDoesNotUseDefaultClient(t *testing.T) {
	if searchHTTPClient.Timeout <= 0 {
		t.Fatalf("searchHTTPClient.Timeout = %v, want a positive timeout", searchHTTPClient.Timeout)
	}
	// The test servers are on 127.0.0.1, which the search SSRF guard
	// blocks by default; the override is test-only.
	allowPrivateFetchHosts = true
	t.Cleanup(func() { allowPrivateFetchHosts = false })
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("http.DefaultClient must not be used by search")
	})}
	t.Cleanup(func() { http.DefaultClient = old })

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"AbstractText": "Go is a language.", "AbstractURL": "https://go.dev", "Heading": "Go", "RelatedTopics": []}`))
	}))
	defer apiSrv.Close()
	liteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<a class="result-link" href="https://example.com/x">Lite Result</a>`))
	}))
	defer liteSrv.Close()

	oldAPI, oldLite := searchAPIBase, searchLiteBase
	searchAPIBase, searchLiteBase = apiSrv.URL, liteSrv.URL
	defer func() { searchAPIBase, searchLiteBase = oldAPI, oldLite }()

	if results, err := searchViaAPI(context.Background(), "go", 5); err != nil {
		t.Fatalf("searchViaAPI with broken DefaultClient: %v", err)
	} else if len(results) == 0 {
		t.Fatal("searchViaAPI returned no results")
	}
	if results, err := searchViaLite(context.Background(), "go", 5); err != nil {
		t.Fatalf("searchViaLite with broken DefaultClient: %v", err)
	} else if len(results) == 0 || results[0].Title != "Lite Result" {
		t.Fatalf("searchViaLite results = %+v, want the lite result", results)
	}
}

// --- P2-29: lite result-link attribute order ---

// TestSearchViaLiteAttributeOrder: result links are recognised whether
// class comes before href or href comes before class, and non-result
// anchors are ignored.
func TestSearchViaLiteAttributeOrder(t *testing.T) {
	allowPrivateFetchHosts = true
	t.Cleanup(func() { allowPrivateFetchHosts = false })
	liteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body>
			<a class="result-link" href="https://example.com/class-first">Class First</a>
			<a href="https://example.com/href-first" class="result-link">Href First</a>
			<a href="https://example.com/other" class="result-snippet">Not A Result</a>
			<a class="result-link other-class" href="https://example.com/multi">Multi Class</a>
		</body></html>`))
	}))
	defer liteSrv.Close()

	oldLite := searchLiteBase
	searchLiteBase = liteSrv.URL
	defer func() { searchLiteBase = oldLite }()

	results, err := searchViaLite(context.Background(), "x", 10)
	if err != nil {
		t.Fatalf("searchViaLite: %v", err)
	}
	want := map[string]string{
		"https://example.com/class-first": "Class First",
		"https://example.com/href-first":  "Href First",
		"https://example.com/multi":       "Multi Class",
	}
	if len(results) != len(want) {
		t.Fatalf("searchViaLite results = %+v, want %d results", results, len(want))
	}
	for _, r := range results {
		if wantTitle, ok := want[r.URL]; !ok {
			t.Errorf("unexpected result URL %q", r.URL)
		} else if r.Title != wantTitle {
			t.Errorf("result %q title = %q, want %q", r.URL, r.Title, wantTitle)
		}
	}
}
