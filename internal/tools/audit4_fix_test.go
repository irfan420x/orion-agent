// Audit 4 P1-4 regression tests: web_search must not follow a
// redirect onto a private address (SSRF), exactly like web_fetch.
package tools

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestAudit4SearchRedirectToPrivateRefused: a DuckDuckGo stand-in
// that 302-redirects to a local server must not leak the private
// body as a search result. The initial host is trusted
// (127.0.0.1/32, like the web_fetch redirect test); the redirect
// target (127.0.0.2, loopback, untrusted) must be refused before
// any dial, and the private server must never be hit.
func TestAudit4SearchRedirectToPrivateRefused(t *testing.T) {
	noFetchProxy(t)

	var privateHits int
	var hitsMu sync.Mutex
	privateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsMu.Lock()
		privateHits++
		hitsMu.Unlock()
		_, _ = w.Write([]byte("SECRET-PRIVATE-BODY"))
	}))
	defer privateSrv.Close()
	_, privatePort, err := net.SplitHostPort(strings.TrimPrefix(privateSrv.URL, "http://"))
	if err != nil {
		t.Fatalf("private server URL %q: %v", privateSrv.URL, err)
	}
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://private.example:"+privatePort+"/secret", http.StatusFound)
	}))
	defer apiSrv.Close()
	_, apiPort, err := net.SplitHostPort(strings.TrimPrefix(apiSrv.URL, "http://"))
	if err != nil {
		t.Fatalf("api server URL %q: %v", apiSrv.URL, err)
	}

	resolver := &scriptedResolver{answers: map[string][][]net.IP{
		"public.example":  {{net.ParseIP("127.0.0.1")}},
		"private.example": {{net.ParseIP("127.0.0.2")}},
	}, calls: map[string]int{}}
	useScriptedResolver(t, resolver)

	oldAPI, oldLite := searchAPIBase, searchLiteBase
	searchAPIBase, searchLiteBase = "http://public.example:"+apiPort, "http://public.example:"+apiPort
	defer func() { searchAPIBase, searchLiteBase = oldAPI, oldLite }()

	tool, err := newWebSearchTool([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatalf("newWebSearchTool: %v", err)
	}
	// Direct API path: the redirect must be refused.
	if _, err := tool.searchViaAPI(context.Background(), "x", 5); err == nil {
		t.Fatal("searchViaAPI followed a redirect to a private address, want refusal")
	} else if !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("searchViaAPI err = %v, want a non-public-address refusal", err)
	}
	// Direct lite path: same refusal.
	if _, err := tool.searchViaLite(context.Background(), "x", 5); err == nil {
		t.Fatal("searchViaLite followed a redirect to a private address, want refusal")
	} else if !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("searchViaLite err = %v, want a non-public-address refusal", err)
	}
	// End-to-end Execute: the private body must never surface as a
	// result (API fails, lite fails the same way, Execute errors).
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"query": "x"}))
	if err == nil {
		t.Fatalf("web_search Execute succeeded with output %q, want an error", out)
	}
	if strings.Contains(out, "SECRET-PRIVATE-BODY") {
		t.Fatalf("web_search output leaked the private body: %q", out)
	}
	if n := resolver.callCount("private.example"); n < 1 {
		t.Fatalf("resolver called %d times for private.example, want >= 1 (the redirect target must be resolved and validated)", n)
	}
	hitsMu.Lock()
	hits := privateHits
	hitsMu.Unlock()
	if hits != 0 {
		t.Fatalf("private server was hit %d times, want 0 (redirect must be refused before the dial)", hits)
	}
}

// TestAudit4SearchRejectsBadTrustedCIDR: a malformed trusted-egress
// CIDR fails search construction loudly, mirroring web_fetch.
func TestAudit4SearchRejectsBadTrustedCIDR(t *testing.T) {
	if _, err := newWebSearchTool([]string{"not-a-cidr"}); err == nil {
		t.Fatal("newWebSearchTool(bad CIDR): expected error, got nil")
	}
}
