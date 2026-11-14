// Package tools — web tools: web_fetch and web_search.
//
// web_fetch performs a plain HTTP GET with redirect and size caps
// and returns EXTRACTED text (HTML → clean text, JSON → compact
// summary), never a raw dump — a raw 68KB API response cost
// ~17,900 tokens in one turn (rc23 fetch-cost fix).
// web_search is a real search: it queries the DuckDuckGo Instant Answer
// API and falls back to the DuckDuckGo lite HTML results page.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
)

// The factories for this file's tools (framework auto-discovery):
// each tool self-registers here, so adding a tool is one file.
func init() {
	RegisterFactory("web_fetch", func(deps ToolDeps) (Tool, error) { return newWebFetchTool(deps.TrustedEgressCIDRs) })
	RegisterFactory("web_search", func(deps ToolDeps) (Tool, error) { return newWebSearchTool(deps.TrustedEgressCIDRs) })
}

const (
	// defaultFetchMaxBytes is the default raw-fetch cap: 32KB.
	// It was 256KB (rc22), which let one fetch dump up to ~64k
	// tokens of raw body into the model context — a 68KB
	// repos.json cost ~17,900 tokens in a single turn and tripped
	// the daily budget (F-A17-23 / queued user item). 32KB of raw
	// input, extracted (see extractFetchBody) and capped at
	// maxBytes/4 output chars, costs ~2k tokens by default; the
	// agent raises max_bytes explicitly when it needs the full
	// page.
	defaultFetchMaxBytes = 32 * 1024 // 32KB
	maxFetchMaxBytes     = 10 * 1024 * 1024
	fetchTimeout         = 30 * time.Second
	searchTimeout        = 20 * time.Second
	userAgent            = "Orion/0.1"
)

// searchAPIBase and searchLiteBase are package-level vars so tests can
// point them at an httptest server. Production defaults are DuckDuckGo.
var (
	searchAPIBase  = "https://api.duckduckgo.com"
	searchLiteBase = "https://lite.duckduckgo.com"
)

// searchHTTPClient is a dedicated client for DuckDuckGo searches, with
// its own timeout. Search must not use http.DefaultClient: it has no
// timeout and is shared process-wide, so a hung search could stall
// unrelated callers, and any other code mutating DefaultClient would
// silently change search behaviour.
//
// NOTE (Audit 4 P1-4): search no longer uses this client for requests:
// searchViaAPI/searchViaLite route through a fetchSession (the same
// pinned-dialer + redirect validation web_fetch uses), so a search
// result server cannot 302-redirect the search onto a private
// address (SSRF). The client is kept so existing callers/tests that
// assert a dedicated, timed-out search client keep compiling; it is
// not used for live search traffic.
var searchHTTPClient = &http.Client{Timeout: searchTimeout}

// ipResolver is the minimal DNS interface web_fetch needs. It exists
// so tests can substitute a scripted resolver (notably one that
// changes its answer between calls — a DNS-rebinding attack) without
// touching real DNS. Production uses net.DefaultResolver.
type ipResolver interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

// fetchProxyFunc selects the proxy for a web_fetch request. It
// defaults to http.ProxyFromEnvironment. It is a variable (like
// fetchResolver) so tests can select a proxy deterministically:
// http.ProxyFromEnvironment reads the process environment exactly
// once (sync.Once) and caches the result for the life of the
// process, so per-test proxy changes via t.Setenv would otherwise
// be silently ignored after the first fetch in the process.
// Production behaviour is unchanged: the environment is fixed for
// the life of the daemon, so the cached value is the right one.
var fetchProxyFunc = http.ProxyFromEnvironment

// fetchResolver resolves fetch hostnames. Production code must leave
// it at net.DefaultResolver; tests swap it and restore it.
var fetchResolver ipResolver = net.DefaultResolver

// allowPrivateFetchHosts is test-only: tests point web_fetch at httptest
// servers on 127.0.0.1. Production code must leave it false, or the
// SSRF guard below is disabled.
var allowPrivateFetchHosts = false

// isPublicIP reports whether ip is a routable public address. Loopback,
// private, link-local, multicast, and unspecified addresses are not
// public: fetching them would let a model reach the local Ollama
// server, cloud metadata endpoints, or the Docker daemon (SSRF).
// Beyond net.IP's own checks, the ranges it does not cover are
// refused too: 0/8 ("this network"), 100.64/10 (CGNAT), 192.0.0/24
// (IETF protocol assignments), 240/4 (reserved), and the IPv6
// documentation and NAT64 prefixes.
//
// trusted lists deployment CIDRs (config trusted_egress_cidrs) whose
// addresses count as public even though they would otherwise be
// refused — e.g. an egress-gateway range this deployment's DNS maps
// public hosts into. With no trusted nets the check is strict:
// 198.18.0.0/15 (benchmarking) is non-public.
func isPublicIP(ip net.IP, trusted []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	// Normalize IPv4-mapped IPv6 before the range checks.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range trusted {
		if n.Contains(ip) {
			return true
		}
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, n := range nonPublicIPNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// nonPublicIPNets are the address ranges net.IP's IsPrivate /
// IsLoopback / IsLinkLocal* / IsMulticast / IsUnspecified do not
// cover, but which are never public destinations.
var nonPublicIPNets = func() []*net.IPNet {
	var nets []*net.IPNet
	for _, cidr := range []string{
		"0.0.0.0/8",     // "this network" (source addresses only)
		"100.64.0.0/10", // shared address space / CGNAT
		"192.0.0.0/24",  // IETF protocol assignments
		"240.0.0.0/4",   // reserved (includes 255.255.255.255)
		"2001:db8::/32", // IPv6 documentation
		"64:ff9b::/96",  // IPv6 NAT64 well-known prefix
		"198.18.0.0/15", // benchmarking (RFC 2544) — non-public by
		// default. The old global exception for this range (this
		// deployment's egress DNS maps public hosts there) now lives
		// in deployment configuration instead: config
		// trusted_egress_cidrs, which isPublicIP takes as its
		// trusted parameter and checks before this list. With no
		// trusted CIDRs configured, 198.18/15 is refused.
	} {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("orion: bad non-public CIDR " + cidr)
		}
		nets = append(nets, n)
	}
	return nets
}()

// fetchSession is the per-fetch SSRF state: the trusted egress nets
// for this fetch and the host -> validated-IP pins recorded by
// resolveHost. One session serves one Execute call (initial URL plus
// its redirects), so a hostname is resolved exactly once per fetch
// and the dial can only ever land on an address this fetch validated.
// A rebinding host that answers public first and private second is
// therefore harmless: the second answer is never even asked for.
type fetchSession struct {
	trusted []*net.IPNet
	mu      sync.Mutex
	pins    map[string][]net.IP
}

func newFetchSession(trusted []*net.IPNet) *fetchSession {
	return &fetchSession{trusted: trusted, pins: map[string][]net.IP{}}
}

// stripFetchHost returns the bare hostname of a URL host: port
// stripped, IPv6 brackets removed. It is the key resolveHost pins
// under and dialContext looks up.
func stripFetchHost(host string) string {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	return strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
}

func (s *fetchSession) pinned(host string) ([]net.IP, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ips, ok := s.pins[host]
	return ips, ok
}

func (s *fetchSession) setPin(host string, ips []net.IP) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]net.IP, len(ips))
	copy(cp, ips)
	s.pins[host] = cp
}

// resolveHost resolves host ONCE, validates every returned address
// against the public-address rule (plus this session's trusted egress
// nets), and records the validated addresses as the dial pin for the
// host. A second call for the same host returns the recorded pin
// without resolving again, so a rebinding answer can never reach the
// dial: dialContext only ever uses addresses this call validated.
//
// Literal IPs are checked directly and pinned to themselves. When
// allowPrivateFetchHosts is set (tests only), validation is skipped
// but the pin is still recorded, so test fetches to local servers
// dial the resolved address exactly once as well.
// trustedCIDRString renders this session's trusted egress CIDRs
// for an error message: the configured ranges, or "none
// configured" when there are none. resolveHost refusals name
// them (queued user item, rc23) so the model explains the real
// cause — the deployment's egress range is not trusted — instead
// of blaming the user's network.
func (s *fetchSession) trustedCIDRString() string {
	if s == nil || len(s.trusted) == 0 {
		return "none configured"
	}
	parts := make([]string, len(s.trusted))
	for i, n := range s.trusted {
		parts[i] = n.String()
	}
	return strings.Join(parts, ", ")
}

func (s *fetchSession) resolveHost(ctx context.Context, host string) error {
	h := stripFetchHost(host)
	if ips, ok := s.pinned(h); ok && len(ips) > 0 {
		return nil
	}
	if ip := net.ParseIP(h); ip != nil {
		if !allowPrivateFetchHosts && !isPublicIP(ip, s.trusted) {
			return fmt.Errorf("refusing to fetch non-public address %q (trusted egress CIDRs: %s)", h, s.trustedCIDRString())
		}
		s.setPin(h, []net.IP{ip})
		return nil
	}
	ips, err := fetchResolver.LookupIP(ctx, "ip", h)
	if err != nil {
		return fmt.Errorf("cannot resolve host %q: %w", h, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("host %q has no addresses", h)
	}
	if !allowPrivateFetchHosts {
		for _, ip := range ips {
			if !isPublicIP(ip, s.trusted) {
				return fmt.Errorf("host %q resolves to non-public address %s (trusted egress CIDRs: %s)", h, ip, s.trustedCIDRString())
			}
		}
	}
	s.setPin(h, ips)
	return nil
}

// pinnedDialer dials a validated IP. Its per-attempt timeout is
// shorter than the fetch timeout so a blackholed first answer does
// not eat the whole fetch budget before the next validated IP is
// tried; the fetch context's deadline bounds the total.
var pinnedDialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

// proxyFallbackDialer mirrors http.DefaultTransport's dialer for the
// one case dialContext does not pin: the egress proxy's own address.
var proxyFallbackDialer = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}

// dialContext dials a pinned host by its validated IP(s), in order,
// until one connects. Hosts this session never resolved — notably
// the egress proxy's own address, which the Transport dials instead
// of the target whenever a proxy is configured for the URL — fall
// through to the standard dialer, so proxy-side resolution stays
// the proxy's responsibility (see newClient).
//
// The original hostname is preserved for TLS SNI and the HTTP Host
// header: the Transport derives both from the request URL, not from
// the address DialContext dials, so dialing a validated IP changes
// neither (verified by TestFetchPinnedDialPreservesSNIAndHost).
func (s *fetchSession) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	host = stripFetchHost(host)
	ips, ok := s.pinned(host)
	if !ok || len(ips) == 0 {
		return proxyFallbackDialer.DialContext(ctx, network, address)
	}
	var errs []error
	for _, ip := range ips {
		conn, err := pinnedDialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

// checkRedirect is the http.Client redirect policy for web_fetch: at
// most 5 redirects, and every redirect target is resolved, validated
// and pinned (resolveHost) before the redirect is followed, so a
// public URL must not bounce the fetch onto a private address — and a
// rebinding redirect target cannot swap its address between the
// check and the dial either.
func (s *fetchSession) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("too many redirects (max 5)")
	}
	return s.resolveHost(req.Context(), req.URL.Host)
}

// newClient builds the per-fetch http.Client: a clone of
// http.DefaultTransport (so timeouts, keep-alives and HTTP/2 stay at
// the standard defaults) with the proxy function kept
// (fetchProxyFunc, default http.ProxyFromEnvironment) — a custom
// Transport that drops Proxy breaks this deployment's egress proxy —
// and DialContext replaced by the session's pinned dialer. When a
// proxy is configured for the URL the Transport dials the proxy, not
// the target, so the pinned dialer is not consulted for the target
// and the resolveHost pre-check is the guard; proxy-side resolution
// is the proxy's responsibility.
func (s *fetchSession) newClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = fetchProxyFunc
	tr.DialContext = s.dialContext
	return &http.Client{CheckRedirect: s.checkRedirect, Transport: tr}
}

// checkFetchHost refuses hosts that resolve to a non-public address.
// Literal IPs are checked directly; names are resolved and every
// returned address must be public. Kept for the package-level
// redirect policy used by tests; new code should use
// fetchSession.resolveHost, which additionally pins the validated
// addresses for the dial.
func checkFetchHost(ctx context.Context, host string) error {
	return newFetchSession(nil).resolveHost(ctx, host)
}

// fetchCheckRedirect is the http.Client redirect policy for web_fetch
// with strict defaults (no trusted egress CIDRs): at most 5
// redirects, and every redirect target is re-checked against the
// SSRF guard, so a public URL must not bounce the fetch onto a
// private address. Execute uses fetchSession.checkRedirect, which
// additionally pins each redirect target.
func fetchCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("too many redirects (max 5)")
	}
	if err := checkFetchHost(req.Context(), req.URL.Host); err != nil {
		return err
	}
	return nil
}

// parseTrustedEgressCIDRs parses trusted egress CIDRs; an empty or
// malformed entry is an error, never silently skipped.
func parseTrustedEgressCIDRs(cidrs []string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			return nil, fmt.Errorf("trusted egress CIDR must not be empty")
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted egress CIDR %q: %w", c, err)
		}
		nets = append(nets, n)
	}
	return nets, nil
}

// ----------------------------------------------------------------
// Response extraction (rc23 fetch-cost fix).
//
// web_fetch used to return the RAW body (up to 256KB) straight
// into the model context: a 68KB GitHub repos.json cost ~17,900
// tokens in one turn, and a 212KB GitHub HTML page cost ~50k+
// before trimming. extractFetchBody returns EXTRACTED text
// instead, capped at roughly maxBytes/4 characters (~2k tokens
// at the 32KB default):
//
//   - HTML → visible text only (scripts, styles, comments and
//     tags stripped, entities unescaped, blank lines dropped).
//   - JSON array of objects → one compact line per object
//     (scalar fields only, values truncated, nested values
//     summarised), streamed element-by-element so a body cut off
//     by the raw cap still yields the complete items before the
//     cut instead of failing to parse at all.
//   - JSON object/scalar → minified, then truncated to budget.
//   - Anything else → plain text, truncated to budget.
//
// A body that is not HTML or JSON (or JSON that will not parse
// even tolerantly) falls back to plain text, so extraction can
// never turn a fetch into an error. Every truncation carries a
// "[truncated]" marker naming the remedy (a larger max_bytes),
// and the marker text always contains the literal "[truncated]"
// for the existing cap tests.

var (
	htmlCommentRe     = regexp.MustCompile(`(?is)<!--.*?-->`)
	htmlScriptStyleRe = regexp.MustCompile(`(?is)<(script|style|noscript|svg|template)[^>]*>.*?</(script|style|noscript|svg|template)>`)
)

// extractOutputBudget is the extracted-text char budget for a
// fetch: a quarter of the raw cap, at least 1024 chars, so the
// agent's single max_bytes knob controls both the download and
// the context cost.
func extractOutputBudget(maxBytes int) int {
	if b := maxBytes / 4; b > 1024 {
		return b
	}
	return 1024
}

// truncateExtracted caps s at budget runes, appending a
// "[truncated]" marker that reports how much was omitted and
// the remedy. A short string comes back unchanged (no marker),
// so small fetches keep their exact text.
func truncateExtracted(s string, budget int) string {
	r := []rune(s)
	if len(r) <= budget {
		return s
	}
	return string(r[:budget]) + fmt.Sprintf("\n[truncated] (showing first %d of %d chars; re-fetch with a larger max_bytes for more)", budget, len(r))
}

// extractFetchBody converts a fetched body into the text the
// model sees. contentType is the response Content-Type header
// (sniffed from the body when it is empty or generic).
func extractFetchBody(body []byte, contentType string, maxBytes int) string {
	budget := extractOutputBudget(maxBytes)
	ct := strings.ToLower(contentType)
	trimmed := bytes.TrimSpace(body)
	isHTML := strings.Contains(ct, "html")
	isJSON := strings.Contains(ct, "json")
	if !isHTML && !isJSON {
		lower := bytes.ToLower(trimmed)
		if bytes.HasPrefix(lower, []byte("<!doctype html")) || bytes.HasPrefix(lower, []byte("<html")) {
			isHTML = true
		} else if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
			isJSON = true
		}
	}
	if isHTML {
		return truncateExtracted(extractHTMLText(body), budget)
	}
	if isJSON {
		if out, ok := compactJSON(trimmed, budget); ok {
			return out
		}
		// Unparseable JSON falls through to plain text.
	}
	return truncateExtracted(string(body), budget)
}

// extractHTMLText strips a page down to its visible text:
// comments, script/style/noscript/svg/template blocks and all
// tags go; entities are unescaped; runs of whitespace collapse
// and empty lines drop, so navigation chrome and markup do not
// cost context.
func extractHTMLText(body []byte) string {
	s := htmlCommentRe.ReplaceAllString(string(body), "")
	s = htmlScriptStyleRe.ReplaceAllString(s, "")
	s = htmlTagRe.ReplaceAllString(s, "\n")
	s = htmlUnescape(s)
	s = strings.ReplaceAll(s, "\u00a0", " ")
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// compactJSON renders a JSON body compactly within budget. An
// array of objects becomes one line per object (see
// compactJSONObject); anything else is minified and truncated.
// ok is false when the body is not parseable JSON at all (the
// caller falls back to plain text). A body cut off by the raw
// cap still yields the complete array elements before the cut:
// the decoder streams element-by-element and stops at the
// truncation point instead of failing the whole parse.
func compactJSON(trimmed []byte, budget int) (string, bool) {
	if len(trimmed) == 0 {
		return "", false
	}
	if trimmed[0] != '[' {
		var v any
		if err := json.Unmarshal(trimmed, &v); err != nil {
			return "", false
		}
		min, err := json.Marshal(v)
		if err != nil {
			return "", false
		}
		return truncateExtracted(string(min), budget), true
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('[') {
		return "", false
	}
	var sb strings.Builder
	count := 0
	truncated := false
	for dec.More() {
		var elem any
		if err := dec.Decode(&elem); err != nil {
			// Truncated input (or a bad element): keep the
			// complete elements already rendered.
			truncated = true
			break
		}
		line := compactJSONValue(elem)
		if sb.Len() > 0 && sb.Len()+len(line)+1 > budget {
			truncated = true
			break
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(line)
		count++
	}
	if count == 0 {
		return "", false
	}
	out := sb.String()
	if truncated {
		out += "\n[truncated] (not all items shown; re-fetch with a larger max_bytes for more)"
	}
	return out, true
}

// maxCompactFieldChars bounds one scalar value inside a compacted
// JSON object line, so one huge description cannot eat the whole
// budget.
const maxCompactFieldChars = 120

// compactJSONValue renders one JSON value compactly. An object
// becomes "k=v | k=v" over its scalar fields (keys sorted, so
// the output is deterministic); nested objects and arrays are
// summarised, never expanded; other values are minified.
func compactJSONValue(v any) string {
	obj, ok := v.(map[string]any)
	if !ok {
		min, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return truncateRunesCompact(string(min), maxCompactFieldChars*2)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		val := obj[k]
		switch t := val.(type) {
		case nil:
			parts = append(parts, k+"=null")
		case string, float64, bool:
			parts = append(parts, k+"="+truncateRunesCompact(fmt.Sprint(t), maxCompactFieldChars))
		case map[string]any:
			parts = append(parts, k+"=<object>")
		case []any:
			parts = append(parts, fmt.Sprintf("%s=<%d items>", k, len(t)))
		default:
			parts = append(parts, k+"=<value>")
		}
	}
	return strings.Join(parts, " | ")
}

// truncateRunesCompact caps s at max runes with an ellipsis
// marker; short values are untouched.
func truncateRunesCompact(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

type webFetchTool struct {
	// trusted holds the deployment's trusted egress CIDRs, parsed
	// once at construction (see newWebFetchTool). The zero value is
	// the strict default: no trusted ranges.
	trusted []*net.IPNet
}

// newWebFetchTool builds a web_fetch tool trusting the given egress
// CIDRs (config trusted_egress_cidrs). A malformed CIDR is a
// programmer/configuration error and fails loudly here, at
// construction, rather than being silently ignored per fetch.
func newWebFetchTool(trustedCIDRs []string) (*webFetchTool, error) {
	nets, err := parseTrustedEgressCIDRs(trustedCIDRs)
	if err != nil {
		return nil, fmt.Errorf("tools: web_fetch: %w", err)
	}
	return &webFetchTool{trusted: nets}, nil
}

func (t *webFetchTool) Name() string { return "web_fetch" }

// RiskClass declares the tool network: it fetches remote content.
func (t *webFetchTool) RiskClass() permissions.Class { return permissions.ClassNetwork }

func (t *webFetchTool) Description() string {
	return "Fetch a URL over HTTP(S) and return the extracted text (HTML → clean text, JSON → compact summary; up to 32KB fetched by default, extracted text capped at ~8K chars)."
}

func (t *webFetchTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{"type": "string", "description": "HTTP or HTTPS URL to fetch."},
				"max_bytes": map[string]any{
					"type":        "integer",
					"description": "Maximum response bytes to fetch (default 32768, max 10485760). The returned text is extracted (HTML → clean text, JSON → compact summary) and capped at roughly max_bytes/4 characters; raise max_bytes when the full page is genuinely needed.",
					"minimum":     1,
					"maximum":     10485760,
				},
			},
			"required": []string{"url"},
		},
	}
}

type webFetchArgs struct {
	URL      string `json:"url"`
	MaxBytes *int   `json:"max_bytes,omitempty"`
}

func (t *webFetchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a webFetchArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	raw := strings.TrimSpace(a.URL)
	if raw == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("url must not be empty")}
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("invalid url: %w", err)}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("only http and https URLs are allowed, got scheme %q", parsed.Scheme)}
	}
	if parsed.Host == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("url has no host")}
	}
	maxBytes := defaultFetchMaxBytes
	if a.MaxBytes != nil {
		maxBytes = *a.MaxBytes
	}
	if maxBytes < 1 || maxBytes > maxFetchMaxBytes {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("max_bytes must be between 1 and %d", maxFetchMaxBytes)}
	}

	reqCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	// Resolve once, validate, and pin: the per-fetch session records
	// the validated IPs and the client's pinned dialer only ever
	// dials those, so no second DNS resolution can rebind the host
	// between this check and the dial. When a proxy is configured
	// for the URL the dial goes to the proxy instead and this
	// pre-check is the guard (see fetchSession.newClient).
	session := newFetchSession(t.trusted)
	if err := session.resolveHost(reqCtx, parsed.Host); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "denied", Err: err}
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, raw, nil)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("cannot build request: %w", err)}
	}
	req.Header.Set("User-Agent", userAgent)

	client := session.newClient()
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		if reqCtx.Err() != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: fmt.Errorf("fetch timed out: %w", err)}
		}
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("fetch failed: %w", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read a little of the body for a diagnostic: servers usually
		// explain a 4xx/5xx in the first bytes. The snippet is capped
		// at 512 bytes with invalid UTF-8 dropped, so it stays short
		// and well-formed. The URL is redacted, never raw: a URL with
		// embedded userinfo credentials must not leak into the model
		// context via this error.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		detailStr := strings.TrimSpace(strings.ToValidUTF8(string(detail), ""))
		if detailStr != "" {
			return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("unexpected HTTP status %d %s for %s: %s", resp.StatusCode, resp.Status, parsed.Redacted(), detailStr)}
		}
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("unexpected HTTP status %d %s for %s", resp.StatusCode, resp.Status, parsed.Redacted())}
	}
	limited := io.LimitReader(resp.Body, int64(maxBytes)+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("failed reading response body: %w", err)}
	}
	truncated := len(body) > maxBytes
	if truncated {
		body = body[:maxBytes]
	}
	// Extracted text, not a raw dump (rc23 fetch-cost fix): the
	// model sees clean text / a compact JSON summary, capped at
	// roughly maxBytes/4 chars, instead of up to maxBytes of raw
	// markup or API metadata.
	out := extractFetchBody(body, resp.Header.Get("Content-Type"), maxBytes)
	if truncated {
		out += "\n[truncated] (response exceeded max_bytes; re-fetch with a larger max_bytes for more)"
	}
	return out, nil
}

// ---------------------------------------------------------------- web_search

type webSearchTool struct {
	// trusted holds the deployment's trusted egress CIDRs, parsed
	// once at construction (see newWebSearchTool). The zero value is
	// the strict default: no trusted ranges.
	trusted []*net.IPNet
}

// newWebSearchTool builds a web_search tool trusting the given egress
// CIDRs (config trusted_egress_cidrs). A malformed CIDR fails loudly
// here, at construction, rather than being silently ignored per
// search — mirroring newWebFetchTool.
func newWebSearchTool(trustedCIDRs []string) (*webSearchTool, error) {
	nets, err := parseTrustedEgressCIDRs(trustedCIDRs)
	if err != nil {
		return nil, fmt.Errorf("tools: web_search: %w", err)
	}
	return &webSearchTool{trusted: nets}, nil
}

func (t *webSearchTool) Name() string { return "web_search" }

// RiskClass declares the tool network: it queries a remote search API.
func (t *webSearchTool) RiskClass() permissions.Class { return permissions.ClassNetwork }

func (t *webSearchTool) Description() string {
	return "Search the web (DuckDuckGo) and return numbered results with titles and links."
}

func (t *webSearchTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query."},
				"count": map[string]any{
					"type":        "integer",
					"description": "Maximum number of results (default 5, max 20).",
					"minimum":     1,
					"maximum":     20,
				},
			},
			"required": []string{"query"},
		},
	}
}

type webSearchArgs struct {
	Query string `json:"query"`
	Count *int   `json:"count,omitempty"`
}

type searchResult struct {
	Title string
	URL   string
	Text  string
}

// errDDGCaptcha is the sentinel for a DuckDuckGo bot challenge
// (rc23, queued user item): DDG answers automated requests with
// HTTP 202 and a challenge page instead of results. It is
// detected in searchViaAPI/searchViaLite and reported by Execute
// as "search backend is blocking automated requests right now"
// instead of the misleading "no results found" — the results
// exist, the backend is refusing to serve them to this egress
// IP, and it usually clears on its own.
var errDDGCaptcha = errors.New("search backend is blocking automated requests right now (DuckDuckGo bot challenge)")

// isDDGChallengeBody reports whether a lite results page is a
// bot-challenge page rather than results: a challenge page has
// no result links and names the challenge. Checked only when the
// page parses to zero results, so a real results page that
// happens to mention the word "challenge" in a snippet is never
// misread.
func isDDGChallengeBody(body []byte) bool {
	return strings.Contains(strings.ToLower(string(body)), "challenge")
}

func (t *webSearchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a webSearchArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("query must not be empty")}
	}
	count := 5
	if a.Count != nil {
		count = *a.Count
	}
	if count < 1 || count > 20 {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("count must be between 1 and 20")}
	}

	results, apiErr := t.searchViaAPI(ctx, query, count)
	if len(results) == 0 {
		// Fall back to the lite HTML results page.
		liteResults, liteErr := t.searchViaLite(ctx, query, count)
		if len(liteResults) > 0 {
			results = liteResults
		} else if errors.Is(apiErr, errDDGCaptcha) || errors.Is(liteErr, errDDGCaptcha) {
			// The backend is refusing automated requests
			// (DDG bot challenge, HTTP 202) — say so
			// instead of "no results", which would be a lie:
			// the results exist, this egress IP is blocked
			// from seeing them right now. A plain reply,
			// not an error: it is a transient backend
			// condition, not a bad query.
			return "Search backend is blocking automated requests right now (DuckDuckGo bot challenge). Try again later.", nil
		} else if apiErr != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("web search failed (API: %v; lite fallback: %v)", apiErr, liteErr)}
		} else if liteErr != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("web search failed (lite fallback: %v)", liteErr)}
		} else {
			return fmt.Sprintf("No results found for %q.", query), nil
		}
	}
	if len(results) > count {
		results = results[:count]
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Search results for %q:\n", query)
	for i, r := range results {
		fmt.Fprintf(&sb, "\n%d. %s\n", i+1, r.Title)
		if r.URL != "" {
			fmt.Fprintf(&sb, "   %s\n", r.URL)
		}
		if r.Text != "" && r.Text != r.Title {
			fmt.Fprintf(&sb, "   %s\n", r.Text)
		}
	}
	return sb.String(), nil
}

// ddgAPIResponse is the subset of the DuckDuckGo Instant Answer JSON we use.
type ddgAPIResponse struct {
	AbstractText  string `json:"AbstractText"`
	AbstractURL   string `json:"AbstractURL"`
	Heading       string `json:"Heading"`
	RelatedTopics []struct {
		Text     string `json:"Text"`
		FirstURL string `json:"FirstURL"`
		Topics   []struct {
			Text     string `json:"Text"`
			FirstURL string `json:"FirstURL"`
		} `json:"Topics,omitempty"`
	} `json:"RelatedTopics"`
	Results []struct {
		Text     string `json:"Text"`
		FirstURL string `json:"FirstURL"`
	} `json:"Results"`
}

// searchViaAPI queries the DuckDuckGo Instant Answer API. It routes
// through a fetchSession (pinned dialer + redirect validation, the
// same SSRF guard web_fetch uses): the search host is resolved and
// validated once, and every redirect hop is re-validated before it
// is followed, so a search endpoint cannot bounce the request onto a
// private address. The package-level wrapper keeps the old call
// shape for tests; production code goes through the tool (which
// carries the trusted egress CIDRs).
func searchViaAPI(ctx context.Context, query string, count int) ([]searchResult, error) {
	return (&webSearchTool{}).searchViaAPI(ctx, query, count)
}

func (t *webSearchTool) searchViaAPI(ctx context.Context, query string, count int) ([]searchResult, error) {
	var trusted []*net.IPNet
	if t != nil {
		trusted = t.trusted
	}
	reqCtx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	u := searchAPIBase + "/?q=" + url.QueryEscape(query) + "&format=json&no_html=1"
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	session := newFetchSession(trusted)
	if err := session.resolveHost(reqCtx, parsed.Host); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	client := session.newClient()
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		// DuckDuckGo bot challenge (rc23): the API answers
		// automated requests with HTTP 202 and a challenge
		// page instead of JSON. Surface it as the captcha
		// sentinel, never as an empty result set.
		return nil, errDDGCaptcha
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("DuckDuckGo API returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var ddg ddgAPIResponse
	if err := json.Unmarshal(body, &ddg); err != nil {
		return nil, fmt.Errorf("cannot parse DuckDuckGo response: %w", err)
	}
	var results []searchResult
	if ddg.AbstractText != "" {
		title := ddg.Heading
		if title == "" {
			title = ddg.AbstractText
		}
		results = append(results, searchResult{Title: title, URL: ddg.AbstractURL, Text: ddg.AbstractText})
	}
	for _, rt := range ddg.RelatedTopics {
		if len(results) >= count {
			break
		}
		if rt.Text != "" {
			title := firstSentence(rt.Text)
			results = append(results, searchResult{Title: title, URL: rt.FirstURL, Text: rt.Text})
			continue
		}
		for _, sub := range rt.Topics {
			if len(results) >= count {
				break
			}
			if sub.Text != "" {
				results = append(results, searchResult{Title: firstSentence(sub.Text), URL: sub.FirstURL, Text: sub.Text})
			}
		}
	}
	for _, r := range ddg.Results {
		if len(results) >= count {
			break
		}
		if r.Text != "" {
			results = append(results, searchResult{Title: firstSentence(r.Text), URL: r.FirstURL, Text: r.Text})
		}
	}
	return results, nil
}

func firstSentence(s string) string {
	// Rune-based throughout: slicing by bytes could split a multi-byte
	// rune and return invalid UTF-8 to the model.
	runes := []rune(strings.TrimSpace(s))
	for i := 0; i+1 < len(runes); i++ {
		if runes[i] == '.' && runes[i+1] == ' ' {
			if i > 0 && i < 120 {
				return string(runes[:i+1])
			}
			break
		}
	}
	if len(runes) > 120 {
		return string(runes[:120]) + "…"
	}
	return string(runes)
}

var (
	// liteAnchorRe matches any anchor tag; the result-link class and
	// href are extracted per anchor (see liteHrefRe/liteClassRe), so
	// a match does not depend on attribute order — DuckDuckGo markup
	// puts class before href, but href before class is equally valid
	// HTML and must be recognised too.
	liteAnchorRe = regexp.MustCompile(`(?is)<a\b([^>]*)>(.*?)</a>`)
	liteHrefRe   = regexp.MustCompile(`(?i)\bhref="([^"]*)"`)
	liteClassRe  = regexp.MustCompile(`(?i)\bclass="([^"]*)"`)
	htmlTagRe    = regexp.MustCompile(`<[^>]+>`)
)

// hasClass reports whether a class attribute value contains want as a
// whitespace-separated token.
func hasClass(classAttr, want string) bool {
	for _, c := range strings.Fields(classAttr) {
		if strings.EqualFold(c, want) {
			return true
		}
	}
	return false
}

// searchViaLite queries the DuckDuckGo lite HTML results page. Like
// searchViaAPI it routes through a fetchSession (pinned dialer +
// redirect validation), so a lite endpoint cannot bounce the request
// onto a private address. The package-level wrapper keeps the old
// call shape for tests; production code goes through the tool.
func searchViaLite(ctx context.Context, query string, count int) ([]searchResult, error) {
	return (&webSearchTool{}).searchViaLite(ctx, query, count)
}

func (t *webSearchTool) searchViaLite(ctx context.Context, query string, count int) ([]searchResult, error) {
	var trusted []*net.IPNet
	if t != nil {
		trusted = t.trusted
	}
	reqCtx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	u := strings.TrimRight(searchLiteBase, "/") + "/lite/?q=" + url.QueryEscape(query)
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	session := newFetchSession(trusted)
	if err := session.resolveHost(reqCtx, parsed.Host); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	client := session.newClient()
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		// DuckDuckGo bot challenge (rc23): see searchViaAPI.
		return nil, errDDGCaptcha
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("DuckDuckGo lite returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var results []searchResult
	for _, m := range liteAnchorRe.FindAllStringSubmatch(string(body), -1) {
		attrs, inner := m[1], m[2]
		class := liteClassRe.FindStringSubmatch(attrs)
		if class == nil || !hasClass(class[1], "result-link") {
			continue
		}
		hrefM := liteHrefRe.FindStringSubmatch(attrs)
		if hrefM == nil {
			continue
		}
		href := htmlUnescape(hrefM[1])
		title := strings.TrimSpace(htmlTagRe.ReplaceAllString(htmlUnescape(inner), ""))
		if title == "" || href == "" {
			continue
		}
		// Lite links may be wrapped in /l/?uddg=<urlencoded target>.
		if strings.HasPrefix(href, "/l/?") {
			if parsed, err := url.Parse(href); err == nil {
				if target := parsed.Query().Get("uddg"); target != "" {
					href = target
				}
			}
		} else if strings.HasPrefix(href, "//") {
			href = "https:" + href
		}
		results = append(results, searchResult{Title: title, URL: href})
		if len(results) >= count {
			break
		}
	}
	if len(results) == 0 && isDDGChallengeBody(body) {
		// A challenge page with no results (rc23): see
		// searchViaAPI — report the block, not "no results".
		return nil, errDDGCaptcha
	}
	return results, nil
}

func htmlUnescape(s string) string {
	r := strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#x27;", "'", "&#39;", "'",
	)
	s = r.Replace(s)
	// Numeric entities: decimal (&#65;) and hexadecimal (&#x41; / &#X41;).
	// An invalid entity is left verbatim and decoding continues past
	// it — one bad entity must not swallow the valid ones after it.
	var sb strings.Builder
	for {
		i := strings.Index(s, "&#")
		if i < 0 {
			sb.WriteString(s)
			break
		}
		j := strings.Index(s[i:], ";")
		if j < 0 {
			sb.WriteString(s)
			break
		}
		n, err := parseNumericEntity(s[i+2 : i+j])
		if err != nil || n < 0 || n > 0x10FFFF {
			sb.WriteString(s[:i+j+1])
			s = s[i+j+1:]
			continue
		}
		sb.WriteString(s[:i])
		sb.WriteRune(rune(n))
		s = s[i+j+1:]
	}
	return sb.String()
}

// parseNumericEntity parses the digits of a numeric character reference:
// decimal ("65") or hexadecimal with an x/X prefix ("x41"). The leading
// "&#" and trailing ";" are not part of num.
func parseNumericEntity(num string) (int64, error) {
	if num == "" {
		return 0, strconv.ErrSyntax
	}
	if num[0] == 'x' || num[0] == 'X' {
		return strconv.ParseInt(num[1:], 16, 32)
	}
	return strconv.ParseInt(num, 10, 32)
}
