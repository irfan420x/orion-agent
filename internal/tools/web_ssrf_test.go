package tools

// Regression tests for the DNS-rebinding SSRF TOCTOU (ChatGPT rc2
// audit, finding 1) and the trusted-egress-CIDR move (finding 5).
//
// The rebinding tests use a scripted resolver instead of real DNS:
// nothing here depends on the network or on this deployment's egress
// DNS mapping public hosts into 198.18.245.0/24.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedResolver answers LookupIP from a script: each host maps to
// a queue of answers, one per call. It records how many times each
// host was resolved, so tests can prove a host was resolved exactly
// once (no second, rebinding resolution ever reached the dial).
type scriptedResolver struct {
	mu      sync.Mutex
	answers map[string][][]net.IP
	calls   map[string]int
}

func (r *scriptedResolver) LookupIP(_ context.Context, _ string, host string) ([]net.IP, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[host]++
	q := r.answers[host]
	if len(q) == 0 {
		return nil, fmt.Errorf("scripted resolver: no answer for %q", host)
	}
	ans := q[0]
	if len(q) > 1 {
		r.answers[host] = q[1:]
	}
	return ans, nil
}

func (r *scriptedResolver) callCount(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[host]
}

// useScriptedResolver swaps in a scripted fetchResolver for the test
// and restores the real one afterwards.
func useScriptedResolver(t *testing.T, r *scriptedResolver) {
	t.Helper()
	old := fetchResolver
	fetchResolver = r
	t.Cleanup(func() { fetchResolver = old })
}

// noFetchProxy disables proxying for the test (and restores the
// ambient setting afterwards). It swaps the fetchProxyFunc seam,
// not the environment: http.ProxyFromEnvironment caches the proxy
// config exactly once per process (sync.Once), so t.Setenv proxy
// changes would be silently ignored after the first fetch in the
// process — the seam is the only deterministic way to select a
// proxy (or none) per test.
func noFetchProxy(t *testing.T) {
	t.Helper()
	old := fetchProxyFunc
	fetchProxyFunc = func(*http.Request) (*url.URL, error) { return nil, nil }
	t.Cleanup(func() { fetchProxyFunc = old })
}

// useFetchProxy routes the test's fetches through the given proxy
// URL (and restores the ambient setting afterwards).
func useFetchProxy(t *testing.T, proxyURL string) {
	t.Helper()
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatalf("parse proxy URL %q: %v", proxyURL, err)
	}
	old := fetchProxyFunc
	fetchProxyFunc = func(*http.Request) (*url.URL, error) { return parsed, nil }
	t.Cleanup(func() { fetchProxyFunc = old })
}

// allowPrivateFetch is the test-only SSRF override, restored after
// the test. Tests that need real validation do not call it.
func allowPrivateFetch(t *testing.T) {
	t.Helper()
	allowPrivateFetchHosts = true
	t.Cleanup(func() { allowPrivateFetchHosts = false })
}

// TestFetchPinsResolvedIPAgainstRebinding: a resolver that answers a
// public IP first and a private IP second must not rebind the fetch.
// The fetch must dial the first answer only (resolver called exactly
// once for the host) and land on the first answer's server.
func TestFetchPinsResolvedIPAgainstRebinding(t *testing.T) {
	noFetchProxy(t)
	allowPrivateFetch(t)

	// Two servers on the SAME port on different loopback addresses:
	// the first answer's server says PUBLIC, the rebinding answer's
	// server says PRIVATE. If the fetch ever dialed the second
	// answer, it would get PRIVATE.
	l1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen 127.0.0.1: %v", err)
	}
	port := l1.Addr().(*net.TCPAddr).Port
	l2, err := net.Listen("tcp", fmt.Sprintf("127.0.0.2:%d", port))
	if err != nil {
		t.Fatalf("listen 127.0.0.2:%d: %v", port, err)
	}
	var gotHost string
	var hostMu sync.Mutex
	publicSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostMu.Lock()
		gotHost = r.Host
		hostMu.Unlock()
		fmt.Fprint(w, "PUBLIC-TARGET")
	}))
	publicSrv.Listener = l1
	publicSrv.Start()
	defer publicSrv.Close()
	privateSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "PRIVATE-TARGET")
	}))
	privateSrv.Listener = l2
	privateSrv.Start()
	defer privateSrv.Close()

	resolver := &scriptedResolver{answers: map[string][][]net.IP{
		"rebind.example": {
			{net.ParseIP("127.0.0.1")},
			{net.ParseIP("127.0.0.2")},
		},
	}, calls: map[string]int{}}
	useScriptedResolver(t, resolver)

	f := &webFetchTool{}
	out, err := f.Execute(context.Background(), mustJSON(t, map[string]any{
		"url": fmt.Sprintf("http://rebind.example:%d/", port),
	}))
	if err != nil {
		t.Fatalf("web_fetch: %v", err)
	}
	if out != "PUBLIC-TARGET" {
		t.Fatalf("got %q, want PUBLIC-TARGET (fetch dialed the rebinding answer)", out)
	}
	if n := resolver.callCount("rebind.example"); n != 1 {
		t.Fatalf("resolver called %d times for rebind.example, want exactly 1 (a second resolution could rebind the dial)", n)
	}
	// The Host header still carries the original hostname, not the
	// pinned IP: dialing a validated IP must not change it.
	hostMu.Lock()
	host := gotHost
	hostMu.Unlock()
	if want := fmt.Sprintf("rebind.example:%d", port); host != want {
		t.Fatalf("Host header = %q, want %q", host, want)
	}
}

// TestFetchRedirectToPrivateRefusedWithPinnedDial: initial host
// public (trusted 127.0.0.1/32, so validation is real, not skipped),
// redirect target resolves private (127.0.0.2) — refused, and the
// private server is never dialed.
func TestFetchRedirectToPrivateRefusedWithPinnedDial(t *testing.T) {
	noFetchProxy(t)

	var privateHits int
	var hitsMu sync.Mutex
	privateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsMu.Lock()
		privateHits++
		hitsMu.Unlock()
		fmt.Fprint(w, "SECRET")
	}))
	defer privateSrv.Close()
	_, privatePort, err := net.SplitHostPort(strings.TrimPrefix(privateSrv.URL, "http://"))
	if err != nil {
		t.Fatalf("private server URL %q: %v", privateSrv.URL, err)
	}
	publicSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://private.example:"+privatePort+"/secret", http.StatusFound)
	}))
	defer publicSrv.Close()
	_, publicPort, err := net.SplitHostPort(strings.TrimPrefix(publicSrv.URL, "http://"))
	if err != nil {
		t.Fatalf("public server URL %q: %v", publicSrv.URL, err)
	}

	resolver := &scriptedResolver{answers: map[string][][]net.IP{
		"public.example":  {{net.ParseIP("127.0.0.1")}},
		"private.example": {{net.ParseIP("127.0.0.2")}},
	}, calls: map[string]int{}}
	useScriptedResolver(t, resolver)

	// Trust only 127.0.0.1/32: the initial host passes validation,
	// the redirect target (127.0.0.2, loopback, untrusted) must not.
	tool, err := newWebFetchTool([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatalf("newWebFetchTool: %v", err)
	}
	_, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"url": "http://public.example:" + publicPort + "/",
	}))
	if err == nil {
		t.Fatal("expected refusal of redirect to private address, got nil error")
	}
	if !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("err = %v, want a non-public-address refusal", err)
	}
	if n := resolver.callCount("private.example"); n != 1 {
		t.Fatalf("resolver called %d times for private.example, want exactly 1", n)
	}
	hitsMu.Lock()
	hits := privateHits
	hitsMu.Unlock()
	if hits != 0 {
		t.Fatalf("private server was hit %d times, want 0 (redirect must be refused before the dial)", hits)
	}
}

// selfSignedExampleCert mints a throwaway self-signed certificate for
// example.com, used only to observe the TLS ClientHello (SNI) — the
// client will reject it, which is expected and asserted on.
func selfSignedExampleCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "example.com"},
		DNSNames:     []string{"example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("load key pair: %v", err)
	}
	return cert
}

// TestFetchPinnedDialPreservesSNIAndHost: when the dial goes to a
// validated IP, the TLS handshake must still carry the original
// hostname as SNI — not the IP. The server records the SNI from the
// ClientHello via GetCertificate; the fetch itself is expected to
// fail because the client rejects the throwaway self-signed cert,
// which is fine: the SNI was already observed by then.
func TestFetchPinnedDialPreservesSNIAndHost(t *testing.T) {
	noFetchProxy(t)
	allowPrivateFetch(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	cert := selfSignedExampleCert(t)
	var gotSNI string
	var sniMu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		cfg := &tls.Config{
			Certificates: []tls.Certificate{cert},
			GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
				sniMu.Lock()
				gotSNI = chi.ServerName
				sniMu.Unlock()
				return nil, nil // fall back to the configured certificate
			},
		}
		// The handshake will fail on the client side (self-signed
		// cert); ignore the error — SNI is captured before that.
		_ = tls.Server(conn, cfg).Handshake()
	}()

	resolver := &scriptedResolver{answers: map[string][][]net.IP{
		"example.com": {{net.ParseIP("127.0.0.1")}},
	}, calls: map[string]int{}}
	useScriptedResolver(t, resolver)

	f := &webFetchTool{}
	_, err = f.Execute(context.Background(), mustJSON(t, map[string]any{
		"url": fmt.Sprintf("https://example.com:%d/", port),
	}))
	if err == nil {
		t.Fatal("expected fetch error (self-signed cert must be rejected), got nil")
	}
	// The server goroutine ends once the listener is closed (the
	// deferred ln.Close runs even on t.Fatal paths); wait for it so
	// the SNI capture is complete before asserting.
	ln.Close()
	<-done
	sniMu.Lock()
	sni := gotSNI
	sniMu.Unlock()
	if sni != "example.com" {
		t.Fatalf("TLS SNI = %q, want \"example.com\" (dialing the pinned IP must not become the SNI)", sni)
	}
	if n := resolver.callCount("example.com"); n != 1 {
		t.Fatalf("resolver called %d times for example.com, want exactly 1", n)
	}
}

// TestFetchViaProxyKeepsPreCheckAndDoesNotPin: with an egress proxy
// configured, the fetch goes through the proxy (the pinned dialer is
// not consulted for the target — the proxy does the dialing), the
// resolveHost pre-check still runs exactly once, and the target is
// never dialed directly.
func TestFetchViaProxyKeepsPreCheckAndDoesNotPin(t *testing.T) {
	var targetHits int
	var targetMu sync.Mutex
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetMu.Lock()
		targetHits++
		targetMu.Unlock()
		fmt.Fprint(w, "DIRECT")
	}))
	defer target.Close()
	_, targetPort, err := net.SplitHostPort(strings.TrimPrefix(target.URL, "http://"))
	if err != nil {
		t.Fatalf("target URL %q: %v", target.URL, err)
	}

	var gotProxyURL string
	var proxyMu sync.Mutex
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyMu.Lock()
		gotProxyURL = r.URL.String()
		proxyMu.Unlock()
		fmt.Fprint(w, "PROXIED")
	}))
	defer proxy.Close()

	// Route this fetch through the stub proxy (and only this fetch:
	// the seam is restored afterwards). This must go through the
	// fetchProxyFunc seam, not t.Setenv: http.ProxyFromEnvironment
	// caches the proxy config once per process, so env changes
	// would be silently ignored.
	useFetchProxy(t, proxy.URL)

	resolver := &scriptedResolver{answers: map[string][][]net.IP{
		"proxy-target.example": {{net.ParseIP("127.0.0.1")}},
	}, calls: map[string]int{}}
	useScriptedResolver(t, resolver)

	// Trust 127.0.0.1/32 so the pre-check validation is real.
	tool, err := newWebFetchTool([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatalf("newWebFetchTool: %v", err)
	}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"url": "http://proxy-target.example:" + targetPort + "/",
	}))
	if err != nil {
		t.Fatalf("web_fetch via proxy: %v", err)
	}
	if out != "PROXIED" {
		t.Fatalf("got %q, want PROXIED (fetch must go through the proxy, not dial the target directly)", out)
	}
	proxyMu.Lock()
	proxyURL := gotProxyURL
	proxyMu.Unlock()
	if !strings.Contains(proxyURL, "proxy-target.example") {
		t.Fatalf("proxy saw URL %q, want the absolute-form target URL", proxyURL)
	}
	if n := resolver.callCount("proxy-target.example"); n != 1 {
		t.Fatalf("resolver called %d times for proxy-target.example, want exactly 1 (the pre-check)", n)
	}
	targetMu.Lock()
	hits := targetHits
	targetMu.Unlock()
	if hits != 0 {
		t.Fatalf("target server was hit %d times directly, want 0 (proxy path must not dial the target)", hits)
	}
}

// TestIsPublicIPTrustedEgress: 198.18/15 is non-public by default
// (strict) and public only when listed in the trusted egress CIDRs.
func TestIsPublicIPTrustedEgress(t *testing.T) {
	ip := net.ParseIP("198.18.245.209")
	if isPublicIP(ip, nil) {
		t.Errorf("isPublicIP(198.18.245.209, nil) = true, want false (strict default)")
	}
	if isPublicIP(net.ParseIP("198.19.0.1"), nil) {
		t.Errorf("isPublicIP(198.19.0.1, nil) = true, want false (strict default)")
	}
	trusted, err := parseTrustedEgressCIDRs([]string{"198.18.0.0/15"})
	if err != nil {
		t.Fatalf("parseTrustedEgressCIDRs: %v", err)
	}
	if !isPublicIP(ip, trusted) {
		t.Errorf("isPublicIP(198.18.245.209, trusted 198.18/15) = false, want true")
	}
	if !isPublicIP(net.ParseIP("198.19.0.1"), trusted) {
		t.Errorf("isPublicIP(198.19.0.1, trusted 198.18/15) = false, want true")
	}
	// Unrelated non-public addresses stay refused even with a
	// trusted range configured.
	if isPublicIP(net.ParseIP("10.0.0.1"), trusted) {
		t.Errorf("isPublicIP(10.0.0.1, trusted 198.18/15) = true, want false")
	}
	if isPublicIP(net.ParseIP("127.0.0.1"), trusted) {
		t.Errorf("isPublicIP(127.0.0.1, trusted 198.18/15) = true, want false")
	}
	// A genuinely public address is unaffected either way.
	if !isPublicIP(net.ParseIP("8.8.8.8"), nil) {
		t.Errorf("isPublicIP(8.8.8.8, nil) = false, want true")
	}
}

// TestNewWebFetchToolRejectsBadTrustedCIDR: a malformed trusted CIDR
// fails loudly at construction, never silently ignored.
func TestNewWebFetchToolRejectsBadTrustedCIDR(t *testing.T) {
	for _, bad := range [][]string{
		{"not-a-cidr"},
		{"198.18.0.0/33"},
		{"10.0.0.1"}, // an address, not a CIDR
		{""},
		{"10.0.0.0/8", "junk"},
		{" 198.18.0.0/15 ", "198.18.0.0/15/2"},
	} {
		if _, err := newWebFetchTool(bad); err == nil {
			t.Errorf("newWebFetchTool(%v): expected error, got nil", bad)
		}
	}
	// Whitespace around a valid CIDR is tolerated.
	if _, err := newWebFetchTool([]string{" 198.18.0.0/15 "}); err != nil {
		t.Errorf("newWebFetchTool with padded CIDR: %v", err)
	}
}

// TestFetchTrustedEgressCIDRAllowsFetch: a trusted CIDR actually
// flows into the fetch path — a host resolving into the trusted
// range is fetchable, while the same fetch with the strict default
// tool is refused.
func TestFetchTrustedEgressCIDRAllowsFetch(t *testing.T) {
	noFetchProxy(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "trusted-ok")
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("server URL %q: %v", srv.URL, err)
	}
	resolver := &scriptedResolver{answers: map[string][][]net.IP{
		// Two answers: the host is resolved once per fetch, and this
		// test fetches twice (trusted tool, then strict tool).
		"trusted.example": {{net.ParseIP("127.0.0.1")}, {net.ParseIP("127.0.0.1")}},
	}, calls: map[string]int{}}
	useScriptedResolver(t, resolver)

	url := "http://trusted.example:" + port + "/"
	trustedTool, err := newWebFetchTool([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatalf("newWebFetchTool: %v", err)
	}
	out, err := trustedTool.Execute(context.Background(), mustJSON(t, map[string]any{"url": url}))
	if err != nil {
		t.Fatalf("trusted web_fetch: %v", err)
	}
	if out != "trusted-ok" {
		t.Fatalf("trusted web_fetch got %q, want trusted-ok", out)
	}

	strictTool := &webFetchTool{}
	_, err = strictTool.Execute(context.Background(), mustJSON(t, map[string]any{"url": url}))
	if err == nil {
		t.Fatal("strict web_fetch: expected refusal for loopback host, got nil error")
	}
	if !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("strict web_fetch err = %v, want a non-public-address refusal", err)
	}
}
