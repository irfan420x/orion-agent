// Web parsing helpers — Audit 22 fix batch (WEB worker).
//
// This file holds the pieces web.go's search/fetch paths need to fail
// honestly instead of silently:
//
//   - a small stdlib-only HTML tokenizer (no golang.org/x/net/html:
//     the module is stdlib-only, so no new dependency). It handles
//     single-, double- and unquoted attribute values, skips comments
//     and raw-text elements (script/style), and never mistakes markup
//     inside a script body for a result link.
//   - DuckDuckGo result parsing (lite + html endpoints) on the token
//     stream, with class-contains matching and per-result isolation:
//     a block whose href is missing is skipped, never fatal.
//   - results-page validation + structural challenge detection
//     (SearXNG's shape: challenge-form presence, missing results
//     container — not a substring search for the English word
//     "challenge").
//   - binary-body detection by magic bytes (Hermes' table shape).
//   - head+tail truncation with exact counts + a stored-copy fetch
//     store, so a cut page can be paged (web_fetch fetch_id+offset).
//
// Nothing here is copied from another project: the mechanisms are
// described in ~/workspace/orion-audit/fixbatch-research/
// OPEN-SOURCE-RESEARCH.md §1 and re-implemented in Go from scratch.

package tools

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------- HTML tokenizer

type htmlTokenKind int

const (
	htmlTokenStartTag htmlTokenKind = iota
	htmlTokenEndTag
	htmlTokenText
)

// htmlToken is one event from tokenizeHTML: a start tag (with its
// attributes), an end tag, or a run of text between tags. Tag and
// attribute names are lower-cased — HTML is case-insensitive, so
// <A CLASS="Result-Link"> is the same anchor as the lowercase form.
type htmlToken struct {
	kind        htmlTokenKind
	name        string // lowercase tag name (start/end tags)
	attrs       map[string]string
	text        string // raw text (text tokens)
	selfClosing bool
}

func isHTMLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r'
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isHTMLNameChar(c byte) bool {
	return isASCIILetter(c) || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == ':' || c == '.'
}

// isRawTextElement reports whether the element's content is raw text
// (no tags inside): script/style bodies routinely contain "<a ...>"
// lookalikes that must never be parsed as result links.
func isRawTextElement(name string) bool {
	switch name {
	case "script", "style", "textarea", "title":
		return true
	}
	return false
}

// parseHTMLStartTag parses "<name attr=...>" starting at s[i] == '<'.
// It returns the lower-cased tag name, the attribute map (first value
// wins on duplicates, per the HTML rule), whether the tag is
// self-closing, the index just past '>', and ok=false when the input
// ends mid-tag (the caller then treats '<' as literal text).
func parseHTMLStartTag(s string, i int) (name string, attrs map[string]string, selfClosing bool, next int, ok bool) {
	j := i + 1
	nameStart := j
	for j < len(s) && isHTMLNameChar(s[j]) {
		j++
	}
	if j == nameStart {
		return "", nil, false, 0, false
	}
	name = strings.ToLower(s[nameStart:j])
	attrs = map[string]string{}
	for {
		for j < len(s) && isHTMLSpace(s[j]) {
			j++
		}
		if j >= len(s) {
			return "", nil, false, 0, false
		}
		if s[j] == '>' {
			return name, attrs, selfClosing, j + 1, true
		}
		if s[j] == '/' {
			if j+1 < len(s) && s[j+1] == '>' {
				return name, attrs, true, j + 2, true
			}
			j++
			continue
		}
		aStart := j
		for j < len(s) && s[j] != '=' && !isHTMLSpace(s[j]) && s[j] != '>' && s[j] != '/' {
			j++
		}
		if j == aStart {
			// Defensive: never spin on an unexpected byte.
			j++
			continue
		}
		aName := strings.ToLower(s[aStart:j])
		for j < len(s) && isHTMLSpace(s[j]) {
			j++
		}
		var val string
		if j < len(s) && s[j] == '=' {
			j++
			for j < len(s) && isHTMLSpace(s[j]) {
				j++
			}
			if j >= len(s) {
				return "", nil, false, 0, false
			}
			switch s[j] {
			case '"', '\'':
				q := s[j]
				end := strings.IndexByte(s[j+1:], q)
				if end < 0 {
					return "", nil, false, 0, false
				}
				val = s[j+1 : j+1+end]
				j = j + 1 + end + 1
			default:
				vStart := j
				for j < len(s) && !isHTMLSpace(s[j]) && s[j] != '>' {
					j++
				}
				val = s[vStart:j]
			}
		}
		if _, exists := attrs[aName]; !exists {
			attrs[aName] = val
		}
	}
}

// findRawTextEnd finds the end of a raw-text element's content: the
// index just past the matching "</name>" end tag, or -1 when the
// element is unterminated (the rest of the input is raw text).
func findRawTextEnd(s string, from int, name string) int {
	lower := strings.ToLower(s[from:])
	needle := "</" + name
	for {
		idx := strings.Index(lower, needle)
		if idx < 0 {
			return -1
		}
		after := idx + len(needle)
		if after >= len(lower) {
			return -1
		}
		c := lower[after]
		if c == '>' || c == '/' || isHTMLSpace(c) {
			gt := strings.IndexByte(lower[idx:], '>')
			if gt < 0 {
				return -1
			}
			return from + idx + gt + 1
		}
		// "</name..." was a longer tag (e.g. </scriptx>); keep looking.
		lower = lower[idx+len(needle):]
		from += idx + len(needle)
	}
}

// tokenizeHTML turns an HTML document into a flat token stream.
// Comments, doctype and processing instructions are dropped. A '<'
// that does not start a tag, comment or declaration is literal text.
// Raw-text elements (script/style/textarea/title) contribute their
// start and end tags but their content is skipped, so markup-looking
// text inside a script body is never parsed as a result.
func tokenizeHTML(body []byte) []htmlToken {
	s := string(body)
	var toks []htmlToken
	i := 0
	for i < len(s) {
		lt := strings.IndexByte(s[i:], '<')
		if lt < 0 {
			toks = append(toks, htmlToken{kind: htmlTokenText, text: s[i:]})
			break
		}
		if lt > 0 {
			toks = append(toks, htmlToken{kind: htmlTokenText, text: s[i : i+lt]})
		}
		i += lt // s[i] == '<'
		// Comment: <!-- ... -->
		if strings.HasPrefix(s[i:], "<!--") {
			end := strings.Index(s[i+4:], "-->")
			if end < 0 {
				break // unterminated comment: drop the rest
			}
			i += 4 + end + 3
			continue
		}
		// Declaration / processing instruction: <!...> / <?...>
		if i+1 < len(s) && (s[i+1] == '!' || s[i+1] == '?') {
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				break
			}
			i += end + 1
			continue
		}
		// End tag: </name>
		if i+1 < len(s) && s[i+1] == '/' {
			j := i + 2
			nameStart := j
			for j < len(s) && isHTMLNameChar(s[j]) {
				j++
			}
			name := strings.ToLower(s[nameStart:j])
			end := strings.IndexByte(s[j:], '>')
			if end < 0 {
				break
			}
			if name != "" {
				toks = append(toks, htmlToken{kind: htmlTokenEndTag, name: name})
			}
			i = j + end + 1
			continue
		}
		// Start tag: <letter...
		if i+1 < len(s) && isASCIILetter(s[i+1]) {
			name, attrs, selfClosing, next, ok := parseHTMLStartTag(s, i)
			if !ok {
				toks = append(toks, htmlToken{kind: htmlTokenText, text: "<"})
				i++
				continue
			}
			toks = append(toks, htmlToken{kind: htmlTokenStartTag, name: name, attrs: attrs, selfClosing: selfClosing})
			i = next
			if isRawTextElement(name) && !selfClosing {
				end := findRawTextEnd(s, i, name)
				if end < 0 {
					break // unterminated raw-text element: drop the rest
				}
				toks = append(toks, htmlToken{kind: htmlTokenEndTag, name: name})
				i = end
			}
			continue
		}
		// '<' starting nothing: literal text.
		toks = append(toks, htmlToken{kind: htmlTokenText, text: "<"})
		i++
	}
	return toks
}

// anchorInnerText collects the text inside the anchor starting at
// toks[start] (a start-tag token for "a"), up to its matching end
// tag. It returns the collapsed, entity-unescaped text and the
// index just past the end tag (or len(toks) when unterminated).
func anchorInnerText(toks []htmlToken, start int) (string, int) {
	var sb strings.Builder
	depth := 1
	j := start + 1
	for ; j < len(toks); j++ {
		tk := toks[j]
		if tk.kind == htmlTokenStartTag && tk.name == "a" {
			depth++
		}
		if tk.kind == htmlTokenEndTag && tk.name == "a" {
			depth--
			if depth == 0 {
				return strings.TrimSpace(htmlUnescape(collapseWhitespace(sb.String()))), j + 1
			}
		}
		if tk.kind == htmlTokenText {
			sb.WriteString(tk.text)
		}
	}
	return strings.TrimSpace(htmlUnescape(collapseWhitespace(sb.String()))), j
}

// collapseWhitespace folds every run of whitespace to a single space.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// elementInnerText collects the text inside the element starting at
// toks[start] (a start-tag token for tag), up to its matching end
// tag. It returns the collapsed, entity-unescaped text and the index
// just past the end tag.
func elementInnerText(toks []htmlToken, start int, tag string) (string, int) {
	var sb strings.Builder
	depth := 1
	j := start + 1
	for ; j < len(toks); j++ {
		tk := toks[j]
		if tk.kind == htmlTokenStartTag && tk.name == tag {
			depth++
		}
		if tk.kind == htmlTokenEndTag && tk.name == tag {
			depth--
			if depth == 0 {
				return strings.TrimSpace(htmlUnescape(collapseWhitespace(sb.String()))), j + 1
			}
		}
		if tk.kind == htmlTokenText {
			sb.WriteString(tk.text)
		}
	}
	return strings.TrimSpace(htmlUnescape(collapseWhitespace(sb.String()))), j
}

// ---------------------------------------------------------------- DDG page analysis

// ddgPageAnalysis is the structural shape of a DuckDuckGo results
// page (lite or html endpoint), read off the token stream. It is the
// input to both results-page validation and challenge detection.
type ddgPageAnalysis struct {
	hasSearchForm       bool // <input name="q"> — every genuine results page has a search box
	hasLinksContainer   bool // any element with id="links" (html endpoint results container)
	hasResultsContainer bool // any element with class token "results"
	hasForm             bool // any <form> at all
	hasChallengeForm    bool // a form whose id/class/action names a challenge, or posts to anomaly.js
	hasAnomalyMarker    bool // an anomaly/captcha widget marker (id/class/input/script)
	hasVQD              bool // <input name="vqd"> — recorded only; see isDDGChallengeBody
}

func analyzeDDGPage(toks []htmlToken) ddgPageAnalysis {
	var a ddgPageAnalysis
	for _, tk := range toks {
		if tk.kind != htmlTokenStartTag {
			continue
		}
		switch tk.name {
		case "form":
			a.hasForm = true
			id := strings.ToLower(tk.attrs["id"])
			class := strings.ToLower(tk.attrs["class"])
			action := strings.ToLower(tk.attrs["action"])
			if strings.Contains(id, "challenge") || strings.Contains(id, "captcha") ||
				strings.Contains(class, "challenge") || strings.Contains(class, "captcha") ||
				strings.Contains(action, "challenge") || strings.Contains(action, "anomaly") {
				a.hasChallengeForm = true
			}
		case "input":
			name := strings.ToLower(tk.attrs["name"])
			switch name {
			case "q":
				a.hasSearchForm = true
			case "vqd":
				a.hasVQD = true
			}
			if strings.Contains(name, "challenge") || name == "captcha" {
				a.hasAnomalyMarker = true
			}
		case "script":
			src := strings.ToLower(tk.attrs["src"])
			if strings.Contains(src, "hcaptcha") || strings.Contains(src, "recaptcha") ||
				strings.Contains(src, "turnstile") || strings.Contains(src, "challenge-platform") {
				a.hasAnomalyMarker = true
			}
		}
		if id := strings.ToLower(tk.attrs["id"]); id != "" {
			if id == "links" {
				a.hasLinksContainer = true
			}
			if strings.Contains(id, "anomaly") || strings.Contains(id, "challenge") {
				a.hasAnomalyMarker = true
			}
		}
		if class := tk.attrs["class"]; class != "" {
			if hasClass(class, "results") {
				a.hasResultsContainer = true
			}
			lower := strings.ToLower(class)
			if strings.Contains(lower, "anomaly") || strings.Contains(lower, "challenge") {
				a.hasAnomalyMarker = true
			}
		}
	}
	return a
}

// looksLikeResultsPage reports whether the page positively looks
// like a DuckDuckGo results page: it carries the search form, the
// html endpoint's links container, or a results container. A page
// with none of these is not a results page, however many bytes it
// has — zero parsed results on such a page is a backend failure,
// never "no results". (A page with ≥1 parsed result is a results
// page by construction; callers check that first.)
func looksLikeResultsPage(toks []htmlToken) bool {
	a := analyzeDDGPage(toks)
	return a.hasSearchForm || a.hasLinksContainer || a.hasResultsContainer
}

// isDDGChallengePage reports whether the analysed page is a
// bot-challenge page, structurally: a challenge form (SearXNG's
// //form[@id='challenge-form'] — verified live 2026-11-15: the DDG
// challenge page carries <form id="challenge-form"
// action="//duckduckgo.com/anomaly.js?..."> with an input named
// "challenge-submit" and id="anomaly-modal"), an anomaly/captcha
// widget marker, or a gate-page shape (forms exist but none is the
// search form and there is no results container).
//
// The English word "challenge" is only a last-resort signal, and it
// fires solely on a page that already fails every structural
// results-page check: a real results page that mentions "challenge"
// in a snippet has results or a search form, so it can never match
// here. A localised or JS-driven challenge page carries no such
// word at all — the structural checks above are what catch it.
//
// The vqd token is deliberately NOT a standalone signal: a genuine
// html-endpoint results page does carry <input name="vqd">
// (verified live 2026-11-15), but whether a genuine LITE page does
// could not be verified (live lite answers every request from this
// VM with 202), so treating a missing vqd as a challenge would risk
// misclassifying genuine empty lite pages. hasVQD is recorded in
// the analysis for a future check once genuine lite markup is
// verified live.
func isDDGChallengePage(a ddgPageAnalysis, body []byte) bool {
	if a.hasChallengeForm || a.hasAnomalyMarker {
		return true
	}
	if a.hasForm && !a.hasSearchForm && !a.hasLinksContainer && !a.hasResultsContainer {
		return true
	}
	if !a.hasSearchForm && !a.hasLinksContainer && !a.hasResultsContainer &&
		strings.Contains(strings.ToLower(string(body)), "challenge") {
		return true
	}
	return false
}

// isDDGChallengeBody reports whether a lite/html results page is a
// bot-challenge page rather than results. Kept as the single entry
// point (it tokenizes once); callers that already tokenized use
// isDDGChallengePage on the analysis.
func isDDGChallengeBody(body []byte) bool {
	return isDDGChallengePage(analyzeDDGPage(tokenizeHTML(body)), body)
}

// hasUnrecognizedResultLinks reports whether the page contains
// links shaped like search results that the parsers did not
// recognize: DuckDuckGo wraps every result link — on both the lite
// and the html endpoint — in /l/?uddg=<target>. A page with such
// wrappers and zero parsed results has drifted markup (e.g. the
// result class was renamed): the results are there, the parser
// just does not recognize them, so this is a backend failure, not
// an empty answer.
//
// Only the wrapper counts, deliberately: a bare external link
// would also match ad links, which a genuine empty results page
// can carry. The wrapper is result-only.
func hasUnrecognizedResultLinks(toks []htmlToken) bool {
	for _, tk := range toks {
		if tk.kind != htmlTokenStartTag || tk.name != "a" {
			continue
		}
		href := strings.TrimSpace(tk.attrs["href"])
		if strings.HasPrefix(href, "/l/?uddg=") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- DDG result parsing

// normalizeDDGResultHref resolves a DuckDuckGo result link: both
// endpoints wrap links in /l/?uddg=<target>, which is unwrapped,
// and protocol-relative URLs gain https:.
func normalizeDDGResultHref(href string) string {
	href = strings.TrimSpace(href)
	if strings.HasPrefix(href, "/l/?") {
		if parsed, err := url.Parse(href); err == nil {
			if target := parsed.Query().Get("uddg"); target != "" {
				href = target
			}
		}
	} else if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	return href
}

// parseLiteResults extracts results from a DuckDuckGo lite page:
// anchors with class token "result-link" (any quote style, any
// attribute order — the tokenizer never sees quotes). A result whose
// href is missing is skipped, never fatal; a following
// "result-snippet" anchor becomes the result's snippet text. At most
// count results are returned.
func parseLiteResults(toks []htmlToken, count int) []searchResult {
	var results []searchResult
	for i := 0; i < len(toks) && len(results) < count; i++ {
		tk := toks[i]
		if tk.kind != htmlTokenStartTag || tk.name != "a" {
			continue
		}
		if !hasClass(tk.attrs["class"], "result-link") {
			continue
		}
		// Attribute values are raw HTML: unescape entities
		// (&amp; in a /l/?uddg= wrapper) before unwrapping.
		href := normalizeDDGResultHref(htmlUnescape(tk.attrs["href"]))
		if href == "" {
			continue // missing href skips this result only
		}
		title, after := anchorInnerText(toks, i)
		if title == "" {
			continue
		}
		r := searchResult{Title: title, URL: href}
		// Snippet: the next result-snippet anchor before the next
		// result-link anchor (bounded scan — a snippet belongs to
		// the result directly above it).
		for k := after; k < len(toks) && k < after+40; k++ {
			nt := toks[k]
			if nt.kind != htmlTokenStartTag || nt.name != "a" {
				continue
			}
			if hasClass(nt.attrs["class"], "result-link") {
				break
			}
			if hasClass(nt.attrs["class"], "result-snippet") {
				if snip, _ := anchorInnerText(toks, k); snip != "" {
					r.Text = snip
				}
				break
			}
		}
		results = append(results, r)
	}
	return results
}

// parseHTMLResults extracts results from a DuckDuckGo html-endpoint
// page: each result lives in a <div> with class token "result__body"
// (or "links_main"), with the title link in an <a> with class token
// "result__a" and the snippet in an element with class token
// "result__snippet". A block with no usable link is skipped, never
// fatal. At most count results are returned.
func parseHTMLResults(toks []htmlToken, count int) []searchResult {
	var results []searchResult
	for i := 0; i < len(toks) && len(results) < count; i++ {
		tk := toks[i]
		if tk.kind != htmlTokenStartTag || tk.name != "div" {
			continue
		}
		if !hasClass(tk.attrs["class"], "result__body") && !hasClass(tk.attrs["class"], "links_main") {
			continue
		}
		// Find the block's end (matching </div>).
		depth := 1
		end := len(toks)
		for j := i + 1; j < len(toks); j++ {
			if toks[j].kind == htmlTokenStartTag && toks[j].name == "div" {
				depth++
			}
			if toks[j].kind == htmlTokenEndTag && toks[j].name == "div" {
				depth--
				if depth == 0 {
					end = j
					break
				}
			}
		}
		// Title link: first <a class="result__a">, else the first
		// <a> with a non-empty href inside the block.
		linkIdx := -1
		fallbackIdx := -1
		for j := i + 1; j < end; j++ {
			if toks[j].kind != htmlTokenStartTag || toks[j].name != "a" {
				continue
			}
			if strings.TrimSpace(toks[j].attrs["href"]) == "" {
				continue
			}
			if fallbackIdx < 0 {
				fallbackIdx = j
			}
			if hasClass(toks[j].attrs["class"], "result__a") {
				linkIdx = j
				break
			}
		}
		if linkIdx < 0 {
			linkIdx = fallbackIdx
		}
		if linkIdx < 0 {
			continue // no usable link: skip this block only
		}
		href := strings.TrimSpace(toks[linkIdx].attrs["href"])
		href = normalizeDDGResultHref(htmlUnescape(href))
		title, _ := anchorInnerText(toks, linkIdx)
		if title == "" || href == "" {
			continue
		}
		r := searchResult{Title: title, URL: href}
		// Snippet: first descendant with class "result__snippet".
		for j := i + 1; j < end; j++ {
			if toks[j].kind == htmlTokenStartTag && hasClass(toks[j].attrs["class"], "result__snippet") {
				if snip, _ := elementInnerText(toks, j, toks[j].name); snip != "" {
					r.Text = snip
				}
				break
			}
		}
		results = append(results, r)
		// Skip past this block so a nested result__body div cannot
		// be parsed twice (defensive: real pages do not nest them).
		if end > i {
			i = end
		}
	}
	return results
}

// dedupeSearchURL is the merge key for search results: scheme and
// host lower-cased, fragment dropped, one trailing slash trimmed.
// Two backends returning the same page under trivially different
// spellings must count as one corroborated result, not two.
func dedupeSearchURL(u string) string {
	parsed, err := url.Parse(strings.TrimSpace(u))
	if err != nil || parsed.Host == "" {
		return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(u), "/"))
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	return parsed.String()
}

// ---------------------------------------------------------------- Binary detection

// binarySignature is one magic-byte rule: kind is the human-readable
// name the error reports. offset is where the magic sits (tar's
// "ustar" lives at byte 257).
type binarySignature struct {
	magic  []byte
	kind   string
	offset int
}

var binarySignatures = []binarySignature{
	{[]byte("%PDF"), "PDF document", 0},
	{[]byte("PK\x03\x04"), "ZIP archive", 0},
	{[]byte("PK\x05\x06"), "ZIP archive", 0},
	{[]byte("PK\x07\x08"), "ZIP archive", 0},
	{[]byte("\x1f\x8b"), "gzip-compressed file", 0},
	{[]byte("BZh"), "bzip2-compressed file", 0},
	{[]byte("\xfd7zXZ\x00"), "XZ-compressed file", 0},
	{[]byte("7z\xbc\xaf\x27\x1c"), "7-Zip archive", 0},
	{[]byte("Rar!\x1a\x07\x00"), "RAR archive", 0},
	{[]byte("Rar!\x1a\x07\x01\x00"), "RAR archive", 0},
	{[]byte("\x89PNG\r\n\x1a\n"), "PNG image", 0},
	{[]byte("\xff\xd8\xff"), "JPEG image", 0},
	{[]byte("GIF87a"), "GIF image", 0},
	{[]byte("GIF89a"), "GIF image", 0},
	{[]byte("II*\x00"), "TIFF image", 0},
	{[]byte("MM\x00*"), "TIFF image", 0},
	{[]byte("\x7fELF"), "ELF executable", 0},
	{[]byte("\xfe\xed\xfa\xce"), "Mach-O executable", 0},
	{[]byte("\xfe\xed\xfa\xcf"), "Mach-O executable", 0},
	{[]byte("\xce\xfa\xed\xfe"), "Mach-O executable", 0},
	{[]byte("\xcf\xfa\xed\xfe"), "Mach-O executable", 0},
	{[]byte("\xca\xfe\xba\xbe"), "Mach-O executable", 0},
	{[]byte("SQLite format 3\x00"), "SQLite database", 0},
	{[]byte("fLaC"), "FLAC audio", 0},
	{[]byte("OggS"), "Ogg media", 0},
	{[]byte("ID3"), "MP3 audio", 0},
	{[]byte("\xff\xfb"), "MP3 audio", 0},
	{[]byte("\x00asm"), "WebAssembly module", 0},
	{[]byte("ustar"), "tar archive", 257},
}

// detectBinaryKind reports whether body is binary content, and which
// kind. Magic bytes win over Content-Type (servers lie); a NUL byte
// in the first 8 KiB is the fallback for kinds with no table entry.
// A text body never matches: no text format starts with these
// signatures or embeds NUL bytes.
func detectBinaryKind(body []byte) (kind string, ok bool) {
	// NUL-stripped, per Hermes: some payloads are NUL-padded.
	trimmed := bytes.TrimLeft(body, "\x00")
	for _, sig := range binarySignatures {
		if sig.offset == 0 {
			if bytes.HasPrefix(trimmed, sig.magic) {
				return sig.kind, true
			}
			continue
		}
		if len(trimmed) > sig.offset+len(sig.magic) &&
			bytes.Equal(trimmed[sig.offset:sig.offset+len(sig.magic)], sig.magic) {
			return sig.kind, true
		}
	}
	// WebP: "RIFF" + 4 size bytes + "WEBP".
	if len(trimmed) >= 12 && string(trimmed[0:4]) == "RIFF" && string(trimmed[8:12]) == "WEBP" {
		return "WebP image", true
	}
	head := body
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return "binary data", true
	}
	return "", false
}

// ---------------------------------------------------------------- Head+tail truncation + stored fetches

// maxStoredFetchChars caps the full text kept for continuation
// (Hermes' MAX_STORED_TEXT_CHARS is 2,000,000).
const maxStoredFetchChars = 2_000_000

// maxStoredFetches caps how many fetched pages the daemon retains
// for web_fetch continuation.
const maxStoredFetches = 32

// capStoredText caps s at the stored-copy cap, rune-safe, with a
// marker when the cap bites (so the stored copy never silently
// pretends to be the whole page).
func capStoredText(s string) string {
	r := []rune(s)
	if len(r) <= maxStoredFetchChars {
		return s
	}
	return string(r[:maxStoredFetchChars]) +
		"\n[truncated] (stored copy capped at 2,000,000 chars; re-fetch with a larger max_bytes for more)"
}

// storedFetch is one retained fetched page: the full extracted text
// a continuation call pages through.
type storedFetch struct {
	url     string
	text    string
	created time.Time
}

var (
	fetchStoreMu    sync.Mutex
	fetchStore      = map[string]*storedFetch{}
	fetchStoreOrder []string // insertion order; oldest evicted first
)

// storeFetch retains text for continuation and returns its id (8
// hex chars). Oldest entries are evicted past maxStoredFetches, so
// the store is bounded for the daemon's lifetime.
func storeFetch(url, text string) string {
	for {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			// Crypto rand failing is not a fetch failure: fall
			// back to a time-based id (collisions are still
			// checked below).
			now := time.Now().UnixNano()
			b[0], b[1], b[2], b[3] = byte(now), byte(now>>8), byte(now>>16), byte(now>>24)
		}
		id := hex.EncodeToString(b[:])
		fetchStoreMu.Lock()
		if _, exists := fetchStore[id]; exists {
			fetchStoreMu.Unlock()
			continue
		}
		fetchStore[id] = &storedFetch{url: url, text: text, created: time.Now()}
		fetchStoreOrder = append(fetchStoreOrder, id)
		for len(fetchStoreOrder) > maxStoredFetches {
			oldest := fetchStoreOrder[0]
			fetchStoreOrder = fetchStoreOrder[1:]
			delete(fetchStore, oldest)
		}
		fetchStoreMu.Unlock()
		return id
	}
}

// loadStoredFetch returns the retained page for id, or false when
// the id is unknown (never stored, evicted, or from before a
// daemon restart — the store is in-memory only).
func loadStoredFetch(id string) (storedFetch, bool) {
	fetchStoreMu.Lock()
	defer fetchStoreMu.Unlock()
	sf, ok := fetchStore[id]
	if !ok {
		return storedFetch{}, false
	}
	return *sf, true
}

// truncateHeadTail renders s as a head+tail window of at most budget
// runes (Hermes' shape): ~75% head + ~25% tail, both cuts snapped
// to line boundaries so no line is sliced mid-way. It returns the
// window, the head/tail char counts, the total, and whether s was
// cut at all. The omitted middle starts at headChars, so a
// continuation call with offset=headChars pages it.
func truncateHeadTail(s string, budget int) (window string, headChars, tailChars, totalChars int, truncated bool) {
	r := []rune(s)
	total := len(r)
	if total <= budget {
		return s, total, 0, total, false
	}
	headTarget := budget * 3 / 4
	tailTarget := budget - headTarget
	headEnd := headTarget
	if idx := lastNewlineAtOrBefore(r, headTarget); idx >= 0 {
		headEnd = idx // cut before the newline; the line stays whole
	}
	tailStart := total - tailTarget
	if idx := firstNewlineAtOrAfter(r, tailStart); idx >= 0 && idx+1 < total {
		tailStart = idx + 1 // start after the newline
	}
	headChars = headEnd
	tailChars = total - tailStart
	omitted := tailStart - headEnd
	var sb strings.Builder
	fmt.Fprintf(&sb, "[TRUNCATED] showing %d+%d of %d chars (head+tail cut on line boundaries; the middle was omitted)\n", headChars, tailChars, total)
	sb.WriteString(string(r[:headEnd]))
	fmt.Fprintf(&sb, "\n... [%d chars omitted] ...\n", omitted)
	sb.WriteString(string(r[tailStart:]))
	return sb.String(), headChars, tailChars, total, true
}

func lastNewlineAtOrBefore(r []rune, at int) int {
	if at > len(r) {
		at = len(r)
	}
	for i := at - 1; i >= 0; i-- {
		if r[i] == '\n' {
			return i
		}
	}
	return -1
}

func firstNewlineAtOrAfter(r []rune, at int) int {
	for i := at; i < len(r); i++ {
		if r[i] == '\n' {
			return i
		}
	}
	return -1
}

// continuationHeader renders the header line of a web_fetch
// continuation window: exact char counts plus the next offset (or
// the end marker when this window reaches the end).
func continuationHeader(fetchID string, from, to, total int) string {
	if to >= total {
		return fmt.Sprintf("[stored fetch %s] chars %d-%d of %d (end of stored text)", fetchID, from, to, total)
	}
	return fmt.Sprintf("[stored fetch %s] chars %d-%d of %d; continue with web_fetch {\"fetch_id\": %q, \"offset\": %d}", fetchID, from, to, total, fetchID, to)
}
