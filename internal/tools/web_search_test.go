// Audit 22 fix-batch failure-mode tests for web_search (WEB worker).
//
// Every silent path Audit 22 filed against web_search gets an
// httptest case here: empty bodies, markup drift (renamed class,
// single quotes), non-results pages, wordless challenge pages,
// binary-adjacent torn parses, fan-out rescue, all-backends-fail,
// dedupe/corroboration, and the genuine-empty control. The rc23 /
// p2 tests cover the happy paths and the 202 captcha; these cover
// the failures.
package tools

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	// apiEmptyJSON is the Instant Answer API's legitimate empty
	// answer: valid JSON, zero results. Most queries answer this
	// way, so search must not depend on the API alone.
	apiEmptyJSON = `{"AbstractText": "", "RelatedTopics": [], "Results": []}`

	// liteGenuineEmpty is a real empty lite results page: the
	// search form is present, no result links, nothing else.
	liteGenuineEmpty = `<html><body><form action="/lite/" method="post">` +
		`<input name="q" value="obscure topic xyz"></form>` +
		`<p>No more results.</p></body></html>`

	// htmlGenuineEmpty is a real empty html-endpoint results page:
	// the links container and the search form are present.
	htmlGenuineEmpty = `<html><body><div id="links" class="results"></div>` +
		`<form action="/html/" method="post"><input name="q" value="obscure topic xyz"></form></body></html>`
)

// setSearchBases points all three backends at the given servers
// (empty string leaves that backend at its default — tests must
// not do that for Execute, or the call hits the live network).
func setSearchBases(t *testing.T, api, lite, html string) {
	t.Helper()
	allowPrivateFetchHosts = true
	t.Cleanup(func() { allowPrivateFetchHosts = false })
	oldAPI, oldLite, oldHTML := searchAPIBase, searchLiteBase, searchHTMLBase
	searchAPIBase, searchLiteBase, searchHTMLBase = api, lite, html
	t.Cleanup(func() { searchAPIBase, searchLiteBase, searchHTMLBase = oldAPI, oldLite, oldHTML })
}

// searchTestServer serves body for one search backend.
func searchTestServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func searchJSONServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func executeSearch(t *testing.T, query string) (string, error) {
	t.Helper()
	s := &webSearchTool{}
	return s.Execute(context.Background(), mustJSON(t, map[string]any{"query": query}))
}

// TestWebSearchEmptyBodiesAreError (F-A22-2): a 200 with an empty
// body on every backend is a backend failure, never "No results".
func TestWebSearchEmptyBodiesAreError(t *testing.T) {
	api := searchJSONServer(t, apiEmptyJSON)
	lite := searchTestServer(t, 200, "")
	html := searchTestServer(t, 200, "")
	t.Cleanup(lite.Close)
	t.Cleanup(html.Close)
	setSearchBases(t, api.URL, lite.URL, html.URL)

	out, err := executeSearch(t, "obscure topic xyz")
	if err == nil {
		t.Fatalf("empty lite/html bodies: expected error, got success %q", out)
	}
	if strings.Contains(out, "No results") || (err != nil && strings.Contains(err.Error(), "No results found")) {
		t.Errorf("empty bodies must not claim no results: err=%v out=%q", err, out)
	}
}

// TestWebSearchRenamedClassIsError (F-A22-2): the lite page IS a
// results page (search form present) but its result links carry a
// renamed class — the results are there under /l/?uddg= wrappers,
// the parser just does not recognize them. That is markup drift,
// not an empty answer.
func TestWebSearchRenamedClassIsError(t *testing.T) {
	api := searchJSONServer(t, apiEmptyJSON)
	lite := searchTestServer(t, 200, `<html><body>
		<form action="/lite/" method="post"><input name="q" value="x"></form>
		<a class="result-anchor" href="/l/?uddg=https%3A%2F%2Fexample.com%2Fone">First Result</a>
		<a class="result-anchor" href="/l/?uddg=https%3A%2F%2Fexample.com%2Ftwo">Second Result</a>
	</body></html>`)
	html := searchTestServer(t, 200, htmlGenuineEmpty)
	t.Cleanup(lite.Close)
	t.Cleanup(html.Close)
	setSearchBases(t, api.URL, lite.URL, html.URL)

	out, err := executeSearch(t, "drift test")
	if err == nil {
		t.Fatalf("renamed result class: expected error, got success %q", out)
	}
	if !strings.Contains(err.Error(), "could not be parsed") {
		t.Errorf("renamed-class error = %v, want it to name the unparsed result links", err)
	}
}

// TestWebSearchSingleQuotedAttrsParsed (F-A22-2): the tokenizer
// handles single-quoted and unquoted attributes — the old
// double-quote-only regex did not.
func TestWebSearchSingleQuotedAttrsParsed(t *testing.T) {
	api := searchJSONServer(t, apiEmptyJSON)
	lite := searchTestServer(t, 200, `<html><body>
		<a class='result-link' href='https://example.com/one'>Single Quoted</a>
		<a class=result-link href=https://example.com/two>Unquoted</a>
	</body></html>`)
	html := searchTestServer(t, 200, htmlGenuineEmpty)
	t.Cleanup(lite.Close)
	t.Cleanup(html.Close)
	setSearchBases(t, api.URL, lite.URL, html.URL)

	out, err := executeSearch(t, "quote test")
	if err != nil {
		t.Fatalf("single/unquoted attrs: unexpected error %v", err)
	}
	if !strings.Contains(out, "Single Quoted") || !strings.Contains(out, "Unquoted") {
		t.Errorf("single/unquoted attrs not parsed: %q", out)
	}
}

// TestWebSearchNonResultsPageIsError (F-A22-2): any non-results
// HTML page (a blog, a homepage, an error page) is a backend
// failure, never "No results".
func TestWebSearchNonResultsPageIsError(t *testing.T) {
	api := searchJSONServer(t, apiEmptyJSON)
	lite := searchTestServer(t, 200, `<html><head><title>Example Blog</title></head><body><h1>Welcome to my blog</h1><p>Nothing to see here.</p></body></html>`)
	html := searchTestServer(t, 200, htmlGenuineEmpty)
	t.Cleanup(lite.Close)
	t.Cleanup(html.Close)
	setSearchBases(t, api.URL, lite.URL, html.URL)

	out, err := executeSearch(t, "blog test")
	if err == nil {
		t.Fatalf("non-results page: expected error, got success %q", out)
	}
	if !strings.Contains(err.Error(), "did not look like a search results page") {
		t.Errorf("non-results error = %v, want the results-page validation wording", err)
	}
}

// TestWebSearchChallengeWithoutWord (F-A22-27): a challenge page
// with NO occurrence of the word "challenge" — localised text, a
// challenge form posting to anomaly.js — is still detected
// structurally and reported as a block, never as "No results".
func TestWebSearchChallengeWithoutWord(t *testing.T) {
	api := searchJSONServer(t, apiEmptyJSON)
	lite := searchTestServer(t, 200, `<html><body>
		<form id="challenge-form" action="//duckduckgo.com/anomaly.js?sv=lite" method="POST">
		<p>Veuillez vérifier que vous n'êtes pas un robot.</p>
		<input type="submit" name="challenge-submit" value="Continuer">
		</form></body></html>`)
	html := searchTestServer(t, 200, htmlGenuineEmpty)
	t.Cleanup(lite.Close)
	t.Cleanup(html.Close)
	setSearchBases(t, api.URL, lite.URL, html.URL)

	out, err := executeSearch(t, "localised challenge test")
	if err != nil {
		t.Fatalf("wordless challenge page: unexpected error %v (want the blocking message)", err)
	}
	if !strings.Contains(out, "blocking automated requests") {
		t.Errorf("wordless challenge reply = %q, want the backend-blocking message", out)
	}
	if strings.Contains(out, "No results") {
		t.Errorf("wordless challenge reply = %q, must not claim no results", out)
	}
}

// TestWebSearchChallengeWordOnlyFallback (F-A22-27): a challenge
// page with no structural markers at all — only prose naming a
// challenge — is still reported as a block when the API is clean
// (the word fires only on a page that already fails every
// structural results-page check).
func TestWebSearchChallengeWordOnlyFallback(t *testing.T) {
	api := searchJSONServer(t, apiEmptyJSON)
	lite := searchTestServer(t, 200, `<html><body><p>Please complete the following challenge to continue.</p></body></html>`)
	html := searchTestServer(t, 200, htmlGenuineEmpty)
	t.Cleanup(lite.Close)
	t.Cleanup(html.Close)
	setSearchBases(t, api.URL, lite.URL, html.URL)

	out, err := executeSearch(t, "word-only challenge test")
	if err != nil {
		t.Fatalf("word-only challenge page: unexpected error %v", err)
	}
	if !strings.Contains(out, "blocking automated requests") {
		t.Errorf("word-only challenge reply = %q, want the backend-blocking message", out)
	}
}

// TestWebSearchGenuineEmptyIsNoResults (F-A22-2 control): all
// three backends answer cleanly with zero results — the search
// form / links container prove the pages are results pages — so
// "No results found" is the honest answer.
func TestWebSearchGenuineEmptyIsNoResults(t *testing.T) {
	api := searchJSONServer(t, apiEmptyJSON)
	lite := searchTestServer(t, 200, liteGenuineEmpty)
	html := searchTestServer(t, 200, htmlGenuineEmpty)
	t.Cleanup(lite.Close)
	t.Cleanup(html.Close)
	setSearchBases(t, api.URL, lite.URL, html.URL)

	out, err := executeSearch(t, "obscure topic xyz")
	if err != nil {
		t.Fatalf("genuine empty search: unexpected error %v", err)
	}
	if !strings.Contains(out, `No results found for "obscure topic xyz".`) {
		t.Errorf("genuine empty search = %q, want the no-results sentence", out)
	}
}

// TestWebSearchFanoutSecondBackendRescues (F-A22-2): the lite
// backend fails outright while html answers — the search succeeds
// with the html results plus a degraded note naming the failed
// backend, instead of failing or claiming no results.
func TestWebSearchFanoutSecondBackendRescues(t *testing.T) {
	api := searchJSONServer(t, apiEmptyJSON)
	lite := searchTestServer(t, 500, "internal error")
	html := searchTestServer(t, 200, `<html><body><div id="links" class="results">
		<div class="result results_links web-result"><div class="links_main links_deep result__body">
		<h2 class="result__title"><a class="result__a" href="https://example.com/html-one">HTML Result One</a></h2>
		<a class="result__snippet" href="https://example.com/html-one">Snippet one.</a>
		</div></div></div></body></html>`)
	t.Cleanup(lite.Close)
	t.Cleanup(html.Close)
	setSearchBases(t, api.URL, lite.URL, html.URL)

	out, err := executeSearch(t, "rescue test")
	if err != nil {
		t.Fatalf("fan-out rescue: unexpected error %v", err)
	}
	if !strings.Contains(out, "HTML Result One") || !strings.Contains(out, "https://example.com/html-one") {
		t.Errorf("fan-out rescue lost the html results: %q", out)
	}
	if !strings.Contains(out, "Note (degraded)") || !strings.Contains(out, "lite backend failed") {
		t.Errorf("fan-out rescue has no degraded note naming lite: %q", out)
	}
	if !strings.Contains(out, "rescued from the html backend") {
		t.Errorf("fan-out rescue does not name the rescuing backend: %q", out)
	}
}

// TestWebSearchAllBackendsFailIsError (F-A22-2): every backend
// failing is an error naming each backend — never a success.
func TestWebSearchAllBackendsFailIsError(t *testing.T) {
	api := searchTestServer(t, 500, "api down")
	lite := searchTestServer(t, 500, "lite down")
	html := searchTestServer(t, 500, "html down")
	t.Cleanup(api.Close)
	t.Cleanup(lite.Close)
	t.Cleanup(html.Close)
	setSearchBases(t, api.URL, lite.URL, html.URL)

	out, err := executeSearch(t, "all down test")
	if err == nil {
		t.Fatalf("all backends down: expected error, got success %q", out)
	}
	for _, name := range []string{"api:", "lite:", "html:"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("all-down error = %v, want it to name backend %q", err, name)
		}
	}
}

// TestWebSearchDedupeAndCorroboration: the same URL from lite and
// html is ONE result, ranked before single-backend results.
func TestWebSearchDedupeAndCorroboration(t *testing.T) {
	api := searchJSONServer(t, apiEmptyJSON)
	lite := searchTestServer(t, 200, `<html><body>
		<a class="result-link" href="https://example.com/shared">Shared Result</a>
	</body></html>`)
	html := searchTestServer(t, 200, `<html><body><div id="links" class="results">
		<div class="result__body"><h2><a class="result__a" href="https://example.com/shared">Shared Result</a></h2></div>
		<div class="result__body"><h2><a class="result__a" href="https://example.com/html-only">HTML Only</a></h2></div>
	</div></body></html>`)
	t.Cleanup(lite.Close)
	t.Cleanup(html.Close)
	setSearchBases(t, api.URL, lite.URL, html.URL)

	out, err := executeSearch(t, "dedupe test")
	if err != nil {
		t.Fatalf("dedupe search: unexpected error %v", err)
	}
	if strings.Count(out, "https://example.com/shared") != 1 {
		t.Errorf("shared URL appears %d times, want exactly 1 (dedupe): %q", strings.Count(out, "https://example.com/shared"), out)
	}
	sharedAt := strings.Index(out, "Shared Result")
	htmlOnlyAt := strings.Index(out, "HTML Only")
	if sharedAt < 0 || htmlOnlyAt < 0 {
		t.Fatalf("dedupe search lost a result: %q", out)
	}
	if sharedAt > htmlOnlyAt {
		t.Errorf("corroborated result (2 backends) must rank before single-backend result: %q", out)
	}
	if !strings.Contains(out, "1. Shared Result") || !strings.Contains(out, "2. HTML Only") {
		t.Errorf("dedupe ranking wrong: %q", out)
	}
}

// TestWebSearchHTMLParsing: the html endpoint's blocks parse with
// single quotes and extra classes, and a block whose link has no
// href is skipped without losing the others.
func TestWebSearchHTMLParsing(t *testing.T) {
	toks := tokenizeHTML([]byte(`<html><body><div id="links" class="results">
		<div class='result__body extra'><h2><a class='result__a' href='https://example.com/one'>One</a></h2>
		<a class="result__snippet" href="https://example.com/one">First snippet.</a></div>
		<div class="result__body"><h2><a class="result__a">No Href Here</a></h2></div>
		<div class="links_main"><h2><a class="result__a" href="https://example.com/two">Two</a></h2></div>
	</div></body></html>`))
	results := parseHTMLResults(toks, 10)
	if len(results) != 2 {
		t.Fatalf("parseHTMLResults = %+v, want 2 results (missing-href block skipped)", results)
	}
	if results[0].Title != "One" || results[0].URL != "https://example.com/one" || results[0].Text != "First snippet." {
		t.Errorf("first html result = %+v, want One/one/snippet", results[0])
	}
	if results[1].Title != "Two" || results[1].URL != "https://example.com/two" {
		t.Errorf("second html result = %+v, want Two/two (links_main fallback)", results[1])
	}
}

// TestWebSearchLiteMissingHrefSkipped: a result-link anchor with
// no href is skipped; the parse does not fail.
func TestWebSearchLiteMissingHrefSkipped(t *testing.T) {
	toks := tokenizeHTML([]byte(`<html><body>
		<a class="result-link">No Href</a>
		<a class="result-link" href="https://example.com/kept">Kept</a>
	</body></html>`))
	results := parseLiteResults(toks, 10)
	if len(results) != 1 || results[0].Title != "Kept" {
		t.Fatalf("parseLiteResults = %+v, want only the Kept result", results)
	}
}

// TestWebSearchCountCapAfterParsing: the count cap applies to
// parsed results, after merging — the page is fully parsed first.
func TestWebSearchCountCapAfterParsing(t *testing.T) {
	api := searchJSONServer(t, apiEmptyJSON)
	lite := searchTestServer(t, 200, `<html><body>
		<a class="result-link" href="https://example.com/1">One</a>
		<a class="result-link" href="https://example.com/2">Two</a>
		<a class="result-link" href="https://example.com/3">Three</a>
		<a class="result-link" href="https://example.com/4">Four</a>
		<a class="result-link" href="https://example.com/5">Five</a>
	</body></html>`)
	html := searchTestServer(t, 200, htmlGenuineEmpty)
	t.Cleanup(lite.Close)
	t.Cleanup(html.Close)
	setSearchBases(t, api.URL, lite.URL, html.URL)

	s := &webSearchTool{}
	out, err := s.Execute(context.Background(), mustJSON(t, map[string]any{"query": "count test", "count": 2}))
	if err != nil {
		t.Fatalf("count-capped search: unexpected error %v", err)
	}
	if !strings.Contains(out, "1. One") || !strings.Contains(out, "2. Two") {
		t.Errorf("count-capped search = %q, want exactly the first 2 results", out)
	}
	if strings.Contains(out, "Three") {
		t.Errorf("count-capped search leaked a 3rd result: %q", out)
	}
}

// TestWebSearchTornPageWarns (half-info item): a lite page past
// the 1 MB safety cap still yields its (early) results, but the
// reply warns the list may be incomplete — partial is never
// presented as complete.
func TestWebSearchTornPageWarns(t *testing.T) {
	api := searchJSONServer(t, apiEmptyJSON)
	var sb strings.Builder
	sb.WriteString(`<html><body>
		<a class="result-link" href="https://example.com/early">Early Result</a>`)
	sb.WriteString(strings.Repeat(`<div class="filler">padding padding padding padding</div>`, 30000))
	sb.WriteString(`<a class="result-link" href="https://example.com/late">Late Result</a></body></html>`)
	if sb.Len() <= searchMaxBodyBytes {
		t.Fatalf("test setup: torn page is %d bytes, want > 1MB", sb.Len())
	}
	lite := searchTestServer(t, 200, sb.String())
	html := searchTestServer(t, 200, htmlGenuineEmpty)
	t.Cleanup(lite.Close)
	t.Cleanup(html.Close)
	setSearchBases(t, api.URL, lite.URL, html.URL)

	out, err := executeSearch(t, "torn test")
	if err != nil {
		t.Fatalf("torn page search: unexpected error %v", err)
	}
	if !strings.Contains(out, "Early Result") {
		t.Errorf("torn page lost the early (parsed) result: %q", out[:min(500, len(out))])
	}
	if !strings.Contains(out, "may be incomplete") {
		t.Errorf("torn page has no incomplete warning: %q", out)
	}
	if !strings.Contains(out, "1 MB safety cap") {
		t.Errorf("torn page warning does not name the safety cap: %q", out)
	}
}

// TestWebSearchTornEmptyIsError (half-info item): a torn page
// with zero parsed results is an error — the cut may have removed
// the results, so "no results" would be a guess.
func TestWebSearchTornEmptyIsError(t *testing.T) {
	api := searchJSONServer(t, apiEmptyJSON)
	var sb strings.Builder
	sb.WriteString(`<html><body><form action="/lite/" method="post"><input name="q" value="x"></form>`)
	sb.WriteString(strings.Repeat(`<div class="filler">padding padding padding padding</div>`, 30000))
	sb.WriteString(`</body></html>`)
	if sb.Len() <= searchMaxBodyBytes {
		t.Fatalf("test setup: torn page is %d bytes, want > 1MB", sb.Len())
	}
	lite := searchTestServer(t, 200, sb.String())
	html := searchTestServer(t, 200, htmlGenuineEmpty)
	t.Cleanup(lite.Close)
	t.Cleanup(html.Close)
	setSearchBases(t, api.URL, lite.URL, html.URL)

	out, err := executeSearch(t, "torn empty test")
	if err == nil {
		t.Fatalf("torn empty page: expected error, got success %q", out)
	}
	if !strings.Contains(err.Error(), "safety cap") {
		t.Errorf("torn empty error = %v, want it to name the safety cap", err)
	}
}

// TestWebSearchDNSFailureIsError: all three backends failing DNS
// is an execution error naming the resolve failure.
func TestWebSearchDNSFailureIsError(t *testing.T) {
	noFetchProxy(t)
	useScriptedResolver(t, &scriptedResolver{answers: map[string][][]net.IP{}, calls: map[string]int{}})
	oldAPI, oldLite, oldHTML := searchAPIBase, searchLiteBase, searchHTMLBase
	searchAPIBase, searchLiteBase, searchHTMLBase = "https://api.invalid:1234", "https://lite.invalid:1234", "https://html.invalid:1234"
	t.Cleanup(func() { searchAPIBase, searchLiteBase, searchHTMLBase = oldAPI, oldLite, oldHTML })

	_, err := executeSearch(t, "dns test")
	if err == nil {
		t.Fatal("DNS failure on all backends: expected error, got nil")
	}
	var te *ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error %v is not a *ToolError", err)
	}
	if te.Kind != "execution" {
		t.Errorf("DNS failure Kind = %q, want \"execution\"", te.Kind)
	}
	if !strings.Contains(err.Error(), "cannot resolve host") {
		t.Errorf("DNS failure error = %v, want it to name the resolve failure", err)
	}
}

// TestHTMLTokenizerQuoteStyles: the tokenizer accepts double-,
// single- and unquoted attribute values, and never mistakes a
// '>' inside a quoted value for the end of the tag.
func TestHTMLTokenizerQuoteStyles(t *testing.T) {
	toks := tokenizeHTML([]byte(`<a href="/x?a=1&amp;b=2" class="result-link">A</a>` +
		`<a href='/y' class='result-link'>B</a>` +
		`<a href=/z class=result-link>C</a>` +
		`<a title="a>b" href="/w" class="result-link">D</a>`))
	var anchors []htmlToken
	for _, tk := range toks {
		if tk.kind == htmlTokenStartTag && tk.name == "a" {
			anchors = append(anchors, tk)
		}
	}
	if len(anchors) != 4 {
		t.Fatalf("tokenizer found %d anchors, want 4", len(anchors))
	}
	want := []string{"/x?a=1&amp;b=2", "/y", "/z", "/w"}
	for i, w := range want {
		if anchors[i].attrs["href"] != w {
			t.Errorf("anchor %d href = %q, want %q", i, anchors[i].attrs["href"], w)
		}
		if !hasClass(anchors[i].attrs["class"], "result-link") {
			t.Errorf("anchor %d lost its class: %+v", i, anchors[i].attrs)
		}
	}
}

// TestHTMLTokenizerSkipsScriptBodies: markup-looking text inside
// <script> is not tokenized as tags.
func TestHTMLTokenizerSkipsScriptBodies(t *testing.T) {
	toks := tokenizeHTML([]byte(`<html><head><script>var s = '<a class="result-link" href="https://evil.example">Fake</a>';</script></head>` +
		`<body><a class="result-link" href="https://example.com/real">Real</a></body></html>`))
	results := parseLiteResults(toks, 10)
	if len(results) != 1 || results[0].URL != "https://example.com/real" {
		t.Fatalf("script-body anchor leaked into results: %+v", results)
	}
}

// TestIsDDGChallengeBodyStructural: challenge detection is
// structural — a challenge form with no "challenge" word anywhere
// is caught, and a real results page that mentions "challenge" in
// a snippet is not.
func TestIsDDGChallengeBodyStructural(t *testing.T) {
	wordless := []byte(`<html><body><form id="challenge-form" action="//duckduckgo.com/anomaly.js?x=1">` +
		`<p>Veuillez patienter.</p></form></body></html>`)
	if !isDDGChallengeBody(wordless) {
		t.Error("wordless challenge form not detected")
	}
	resultsPage := []byte(`<html><body><form action="/lite/" method="post"><input name="q"></form>` +
		`<a class="result-link" href="https://example.com/x">How to win the challenge</a></body></html>`)
	if isDDGChallengeBody(resultsPage) {
		t.Error("real results page mentioning 'challenge' in a snippet misread as a challenge page")
	}
	plain := []byte(`<html><body><h1>Hello</h1></body></html>`)
	if isDDGChallengeBody(plain) {
		t.Error("plain non-results page misread as a challenge page")
	}
}
