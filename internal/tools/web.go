// Package tools — web tools: web_fetch and web_search.
//
// web_fetch performs a plain HTTP GET with redirect and size caps.
// web_search is a real search: it queries the DuckDuckGo Instant Answer
// API and falls back to the DuckDuckGo lite HTML results page.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/irfan/orion/internal/model"
)

const (
	defaultFetchMaxBytes = 256 * 1024 // 256KB
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
var searchHTTPClient = &http.Client{Timeout: searchTimeout}

// allowPrivateFetchHosts is test-only: tests point web_fetch at httptest
// servers on 127.0.0.1. Production code must leave it false, or the
// SSRF guard below is disabled.
var allowPrivateFetchHosts = false

// isPublicIP reports whether ip is a routable public address. Loopback,
// private, link-local, multicast, and unspecified addresses are not
// public: fetching them would let a model reach the local Ollama
// server, cloud metadata endpoints, or the Docker daemon (SSRF).
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() && !ip.IsMulticast() && !ip.IsUnspecified()
}

// checkFetchHost refuses hosts that resolve to a non-public address.
// Literal IPs are checked directly; names are resolved and every
// returned address must be public. Redirect targets are checked the
// same way (see CheckRedirect in Execute).
//
// Residual risk: this is check-then-fetch, so a DNS-rebinding attack
// could swap the address between the check and the dial. Fully
// closing that needs dial-time IP pinning; this guard still blocks
// every static SSRF target (localhost, RFC1918, 169.254/16).
func checkFetchHost(ctx context.Context, host string) error {
	if allowPrivateFetchHosts {
		return nil
	}
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	// IPv6 literals arrive bracketed ("[::1]") when no port is given.
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	if ip := net.ParseIP(h); ip != nil {
		if !isPublicIP(ip) {
			return fmt.Errorf("refusing to fetch non-public address %q", h)
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", h)
	if err != nil {
		return fmt.Errorf("cannot resolve host %q: %w", h, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("host %q has no addresses", h)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("host %q resolves to non-public address %s", h, ip)
		}
	}
	return nil
}

// fetchCheckRedirect is the http.Client redirect policy for web_fetch:
// at most 5 redirects, and every redirect target is re-checked against
// the SSRF guard, so a public URL must not bounce the fetch onto a
// private address.
func fetchCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("too many redirects (max 5)")
	}
	if err := checkFetchHost(req.Context(), req.URL.Host); err != nil {
		return err
	}
	return nil
}

type webFetchTool struct{}

func (t *webFetchTool) Name() string { return "web_fetch" }

func (t *webFetchTool) Description() string {
	return "Fetch a URL over HTTP(S) and return the response body as text (up to 256KB by default)."
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
					"description": "Maximum response bytes to return (default 262144, max 10485760).",
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
	if err := checkFetchHost(reqCtx, parsed.Host); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "denied", Err: err}
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, raw, nil)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("cannot build request: %w", err)}
	}
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{CheckRedirect: fetchCheckRedirect}
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
	out := string(body)
	if truncated {
		out += "\n[truncated]"
	}
	return out, nil
}

// ---------------------------------------------------------------- web_search

type webSearchTool struct{}

func (t *webSearchTool) Name() string { return "web_search" }

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

	results, apiErr := searchViaAPI(ctx, query, count)
	if len(results) == 0 {
		// Fall back to the lite HTML results page.
		liteResults, liteErr := searchViaLite(ctx, query, count)
		if len(liteResults) > 0 {
			results = liteResults
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

func searchViaAPI(ctx context.Context, query string, count int) ([]searchResult, error) {
	reqCtx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	u := searchAPIBase + "/?q=" + url.QueryEscape(query) + "&format=json&no_html=1"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := searchHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
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

func searchViaLite(ctx context.Context, query string, count int) ([]searchResult, error) {
	reqCtx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	u := strings.TrimRight(searchLiteBase, "/") + "/lite/?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := searchHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
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
