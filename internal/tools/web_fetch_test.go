// Audit 22 fix-batch failure-mode tests for web_fetch (WEB worker).
//
// Every silent path Audit 22 filed against web_fetch gets an
// httptest case here: empty body, JS-only/empty extraction, binary
// bodies, DNS-as-denied, and truncation with counts + continuation.
// The rc23/ssrf tests cover the happy paths and the SSRF guard;
// these cover the failures.
package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// webFetchTestServer serves body with contentType for one fetch test.
func webFetchTestServer(t *testing.T, contentType string, body []byte) *httptest.Server {
	t.Helper()
	allowPrivateFetchHosts = true
	t.Cleanup(func() { allowPrivateFetchHosts = false })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func webFetchToolError(t *testing.T, err error) *ToolError {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var te *ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error %v is not a *ToolError", err)
	}
	return te
}

// TestWebFetchEmptyBody200IsError (F-A22-1): a 200 with an empty
// body is an error, never a bare "" success.
func TestWebFetchEmptyBody200IsError(t *testing.T) {
	for _, body := range []string{"", "  \n\t "} {
		srv := webFetchTestServer(t, "text/html", []byte(body))
		f := &webFetchTool{}
		out, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL}))
		if err == nil {
			t.Fatalf("body %q: expected error, got success %q", body, out)
		}
		if !strings.Contains(err.Error(), "empty body") {
			t.Errorf("body %q: error = %v, want it to say the body is empty", body, err)
		}
		if out != "" {
			t.Errorf("body %q: output %q, want empty output with the error", body, out)
		}
	}
}

// TestWebFetchJSOnlyPageReturnsMarker (F-A22-1): a non-empty page
// whose visible text is empty (a JS app shell) returns the marker
// string, never a bare "".
func TestWebFetchJSOnlyPageReturnsMarker(t *testing.T) {
	body := `<!DOCTYPE html><html><head><script src="/app.js"></script></head>` +
		`<body><div id="app"></div><script>window.renderApp();</script></body></html>`
	// Note: the fixture carries no <title> text on purpose —
	// extractHTMLText includes title text, so a page whose only
	// text is its title still returns the title (a known residual,
	// not a silent empty); the audit's probe page had no title.
	srv := webFetchTestServer(t, "text/html; charset=utf-8", []byte(body))
	f := &webFetchTool{}
	out, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatalf("JS-only page: unexpected error %v", err)
	}
	if out == "" {
		t.Fatal("JS-only page: got bare empty success, want the no-visible-text marker")
	}
	if !strings.Contains(out, "[no visible text extracted — the page may require JavaScript]") {
		t.Errorf("JS-only page output = %q, want the marker", out)
	}
}

// TestWebFetchImageOnlyPageReturnsMarker (F-A22-1): a non-empty
// body with no extractable text but no script either still gets
// the marker — the fetch worked, there is just nothing to show.
func TestWebFetchImageOnlyPageReturnsMarker(t *testing.T) {
	body := `<html><body><svg><circle cx="5" cy="5" r="4"/></svg><img src="/x.png" alt=""></body></html>`
	srv := webFetchTestServer(t, "text/html", []byte(body))
	f := &webFetchTool{}
	out, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatalf("image-only page: unexpected error %v", err)
	}
	if !strings.Contains(out, "[no visible text extracted") {
		t.Errorf("image-only page output = %q, want the marker", out)
	}
}

// TestWebFetchBinaryKinds (F-A22-22): magic-byte detection names
// the kind and errors — even when Content-Type lies — and a NUL
// byte with no known magic is still binary, not text.
func TestWebFetchBinaryKinds(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        []byte
		wantKind    string
	}{
		{"pdf", "application/pdf", []byte("%PDF-1.4\x00\xff\xfex"), "PDF document"},
		{"pdf-lying-content-type", "text/plain", []byte("%PDF-1.7\n"), "PDF document"},
		{"png", "image/png", append([]byte("\x89PNG\r\n\x1a\n"), 0, 0, 0, 0), "PNG image"},
		{"png-lying-content-type", "text/plain", []byte("\x89PNG\r\n\x1a\n"), "PNG image"},
		{"jpeg", "image/jpeg", []byte("\xff\xd8\xff\xe0\x00\x10JFIF"), "JPEG image"},
		{"gif", "image/gif", []byte("GIF89a\x01\x00\x01\x00"), "GIF image"},
		{"zip", "application/zip", []byte("PK\x03\x04\x14\x00\x00\x00"), "ZIP archive"},
		{"gzip", "application/gzip", []byte("\x1f\x8b\x08\x00\x00\x00\x00\x00"), "gzip-compressed file"},
		{"sqlite", "application/octet-stream", []byte("SQLite format 3\x00\x10\x00"), "SQLite database"},
		{"elf", "application/octet-stream", []byte("\x7fELF\x02\x01\x01\x00"), "ELF executable"},
		{"webp", "image/webp", []byte("RIFF\x10\x00\x00\x00WEBPVP8 "), "WebP image"},
		{"unknown-nul", "text/plain", []byte("hello\x00world"), "binary data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := webFetchTestServer(t, tc.contentType, tc.body)
			f := &webFetchTool{}
			out, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL}))
			te := webFetchToolError(t, err)
			if !strings.Contains(err.Error(), tc.wantKind) {
				t.Errorf("error = %v, want it to name the kind %q", err, tc.wantKind)
			}
			if te.Kind != "execution" {
				t.Errorf("error Kind = %q, want \"execution\"", te.Kind)
			}
			if strings.Contains(out, "%PDF") || strings.Contains(out, "SQLite format") {
				t.Errorf("binary bytes leaked into output %q", out)
			}
		})
	}
}

// TestWebFetchPlainTextStillWorks: the binary check must not fire
// on ordinary text (control for TestWebFetchBinaryKinds).
func TestWebFetchPlainTextStillWorks(t *testing.T) {
	srv := webFetchTestServer(t, "text/plain", []byte("hello world\nsecond line\n"))
	f := &webFetchTool{}
	out, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatalf("plain text fetch: unexpected error %v", err)
	}
	if out != "hello world\nsecond line\n" {
		t.Errorf("plain text fetch = %q, want the exact text", out)
	}
}

// TestWebFetchDNSFailureKind (F-A22-23): a resolve failure is Kind
// "execution", not "denied". The scripted resolver has no answer
// for the host, so LookupIP fails with no network involved.
func TestWebFetchDNSFailureKind(t *testing.T) {
	noFetchProxy(t)
	useScriptedResolver(t, &scriptedResolver{answers: map[string][][]net.IP{}, calls: map[string]int{}})
	f := &webFetchTool{}
	_, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": "https://nonexistent.invalid/page"}))
	te := webFetchToolError(t, err)
	if te.Kind != "execution" {
		t.Errorf("DNS failure Kind = %q, want \"execution\"", te.Kind)
	}
	if !strings.Contains(err.Error(), "cannot resolve host") {
		t.Errorf("DNS failure error = %v, want it to name the resolve failure", err)
	}
}

// TestWebFetchSSRFRefusalStaysDenied (F-A22-23): the SSRF refusal
// keeps Kind "denied" — "denied" is reserved for the refusal.
func TestWebFetchSSRFRefusalStaysDenied(t *testing.T) {
	noFetchProxy(t)
	f := &webFetchTool{}
	_, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": "http://10.0.0.5/secret"}))
	te := webFetchToolError(t, err)
	if te.Kind != "denied" {
		t.Errorf("SSRF refusal Kind = %q, want \"denied\"", te.Kind)
	}
	if !strings.Contains(err.Error(), "non-public") {
		t.Errorf("SSRF refusal error = %v, want the non-public wording", err)
	}
}

// numberedTextLines builds a plain-text body of n numbered lines.
func numberedTextLines(n int) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "line %04d content here\n", i)
	}
	return sb.String()
}

var fetchIDRe = regexp.MustCompile(`"fetch_id": "([0-9a-f]{8})"`)
var fetchOffsetRe = regexp.MustCompile(`"offset": (\d+)`)
var showingRe = regexp.MustCompile(`showing (\d+)\+(\d+) of (\d+) chars`)

// TestWebFetchTruncationHeadTailAndContinuation (half-info item):
// a long page is shown head+tail with exact counts, and the model
// can page the omitted middle with fetch_id+offset — no silent
// mid-text cut, and the tail is genuinely the end of the page.
func TestWebFetchTruncationHeadTailAndContinuation(t *testing.T) {
	body := numberedTextLines(1000) // ~21KB of text, under the 32KB raw cap
	srv := webFetchTestServer(t, "text/plain", []byte(body))
	f := &webFetchTool{}
	out, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatalf("long fetch: unexpected error %v", err)
	}
	if !strings.Contains(out, "[TRUNCATED]") {
		t.Fatalf("long fetch output has no [TRUNCATED] banner: %q", out[:min(400, len(out))])
	}
	m := showingRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("long fetch output has no 'showing X+Y of Z chars' footer: tail %q", out[len(out)-300:])
	}
	// The total is the extracted-text length in chars: the plain
	// body is kept verbatim (trailing newline included).
	if want := fmt.Sprint(len([]rune(body))); m[3] != want {
		t.Errorf("footer total = %s, want %s (the extracted length)", m[3], want)
	}
	if !strings.Contains(out, "line 0001 content here") {
		t.Error("head+tail window lost the first line (head)")
	}
	if !strings.Contains(out, "line 1000 content here") {
		t.Error("head+tail window lost the last line (tail)")
	}
	if strings.Contains(out, "line 0500 content here") {
		t.Error("head+tail window contains a middle line that should have been omitted")
	}
	idMatch := fetchIDRe.FindStringSubmatch(out)
	if idMatch == nil {
		t.Fatalf("long fetch output names no fetch_id: tail %q", out[len(out)-300:])
	}
	fetchID := idMatch[1]
	offMatch := fetchOffsetRe.FindStringSubmatch(out)
	if offMatch == nil {
		t.Fatalf("long fetch output names no continuation offset: tail %q", out[len(out)-300:])
	}
	// Page the omitted middle: the continuation window starts at
	// the named offset and names the next one.
	out2, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"fetch_id": fetchID, "offset": atoiForTest(t, offMatch[1])}))
	if err != nil {
		t.Fatalf("continuation fetch: unexpected error %v", err)
	}
	if !strings.Contains(out2, "line 0500 content here") {
		t.Errorf("continuation window should contain the omitted middle (line 0500), got %q", out2[:min(400, len(out2))])
	}
	if !strings.Contains(out2, "chars "+offMatch[1]+"-") {
		t.Errorf("continuation window has no exact char-range header: %q", out2[:min(200, len(out2))])
	}
	// Page to the end: the last window says so.
	total := atoiForTest(t, m[3])
	out3, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"fetch_id": fetchID, "offset": total - 100}))
	if err != nil {
		t.Fatalf("final continuation fetch: unexpected error %v", err)
	}
	if !strings.Contains(out3, "(end of stored text)") {
		t.Errorf("final window = %q, want the end-of-stored-text marker", out3[:min(200, len(out3))])
	}
	if !strings.Contains(out3, "line 1000 content here") {
		t.Error("final window lost the last line")
	}
}

func atoiForTest(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatalf("atoi %q: %v", s, err)
	}
	return n
}

// TestWebFetchContinuationErrors: unknown ids, past-the-end
// offsets, negative offsets and fetch_id+url together are all loud
// invalid_args errors, never silent successes.
func TestWebFetchContinuationErrors(t *testing.T) {
	f := &webFetchTool{}
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"unknown-id", map[string]any{"fetch_id": "deadbeef"}, "unknown fetch_id"},
		{"id-and-url", map[string]any{"fetch_id": "deadbeef", "url": "https://example.com"}, "mutually exclusive"},
		{"negative-offset", map[string]any{"fetch_id": "deadbeef", "offset": -1}, "must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.Execute(context.Background(), mustJSON(t, tc.args))
			te := webFetchToolError(t, err)
			if te.Kind != "invalid_args" {
				t.Errorf("Kind = %q, want \"invalid_args\"", te.Kind)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	// Past-the-end offset: store a fetch first, then ask past it.
	body := numberedTextLines(1000)
	srv := webFetchTestServer(t, "text/plain", []byte(body))
	out, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatalf("setup fetch: %v", err)
	}
	id := fetchIDRe.FindStringSubmatch(out)
	if id == nil {
		t.Fatal("setup fetch named no fetch_id")
	}
	_, err = f.Execute(context.Background(), mustJSON(t, map[string]any{"fetch_id": id[1], "offset": 1000000}))
	te := webFetchToolError(t, err)
	if te.Kind != "invalid_args" || !strings.Contains(err.Error(), "past the end") {
		t.Errorf("past-the-end error = %v, want invalid_args naming past-the-end", err)
	}
}

// TestWebFetchRawCapAndBudgetTruncation: a body past max_bytes AND
// past the extraction budget carries BOTH notes — the raw-cap note
// and the head+tail banner — so neither cut is silent.
func TestWebFetchRawCapAndBudgetTruncation(t *testing.T) {
	body := numberedTextLines(1000)
	srv := webFetchTestServer(t, "text/plain", []byte(body))
	f := &webFetchTool{}
	out, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL, "max_bytes": 4096}))
	if err != nil {
		t.Fatalf("raw-capped fetch: unexpected error %v", err)
	}
	if !strings.Contains(out, "[TRUNCATED]") {
		t.Error("output has no [TRUNCATED] banner")
	}
	if !strings.Contains(out, "response exceeded max_bytes") {
		t.Error("output has no raw-cap note naming max_bytes")
	}
}

// TestWebFetchShortPageHasNoMarker: a short fetch keeps its exact
// text — no banner, no footer, no store.
func TestWebFetchShortPageHasNoMarker(t *testing.T) {
	srv := webFetchTestServer(t, "text/plain", []byte("short page\n"))
	f := &webFetchTool{}
	out, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatalf("short fetch: unexpected error %v", err)
	}
	if out != "short page\n" {
		t.Errorf("short fetch = %q, want the exact text", out)
	}
	if strings.Contains(out, "TRUNCATED") || strings.Contains(out, "fetch_id") {
		t.Errorf("short fetch should carry no truncation marker: %q", out)
	}
}
