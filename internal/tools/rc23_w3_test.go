package tools

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rc23W3GitHubHTML builds a realistic GitHub profile page: head
// full of script/style junk, then a list of repositories. rawLen
// is the approximate total size; the page is mostly markup, like
// the real 212KB github.com/irfan420x page.
func rc23W3GitHubHTML(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`<!DOCTYPE html><html><head><title>Irfan Ahmmed (irfan420x) · GitHub</title>`)
	sb.WriteString(`<style>` + strings.Repeat("body{margin:0;padding:0;color:#111;} ", 100) + `</style>`)
	sb.WriteString(`<script>` + strings.Repeat("var SECRET_JUNK_XYZ=1;function f(){return 42;} ", 100) + `</script>`)
	sb.WriteString(`</head><body><header><nav>Pull requests Issues Marketplace Explore</nav></header><main>`)
	for i := 0; i < 25; i++ {
		fmt.Fprintf(&sb, `<div class="repo" id="repo-%d"><h3><a href="/irfan420x/repo-%d">repo-%d</a></h3><p>Repository number %d: a Go OS agent with a Telegram gateway and a CLI, built for stability first.</p><span>Go</span><span>★ %d</span></div>`, i, i, i, i, i)
		sb.WriteString(strings.Repeat(`<div class="filler"><span>chrome</span><a href="#">link</a></div>`, 100))
	}
	sb.WriteString(`</main><footer>© 2026 GitHub, Inc. Terms Privacy Security Status Docs Contact</footer></body></html>`)
	return sb.String()
}

// rc23W3ReposJSON builds a GitHub /users/:user/repos response:
// 13 repos with full API metadata (~5KB each, ~68KB total — the
// shape that cost ~17,900 tokens in one turn as a raw dump).
func rc23W3ReposJSON(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 13; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"id":%d,"node_id":"R_kgDOQ%s","name":"repo-%d","full_name":"irfan420x/repo-%d","private":false,"owner":{"login":"irfan420x","id":196164248,"node_id":"U_kgDOQ%s","avatar_url":"https://avatars.githubusercontent.com/u/196164248?v=4","gravatar_id":"","url":"https://api.github.com/users/irfan420x","html_url":"https://github.com/irfan420x","followers_url":"https://api.github.com/users/irfan420x/followers","following_url":"https://api.github.com/users/irfan420x/following{/other_user}","gists_url":"https://api.github.com/users/irfan420x/gists{/gist_id}","starred_url":"https://api.github.com/users/irfan420x/starred{/owner}{/repo}","subscriptions_url":"https://api.github.com/users/irfan420x/subscriptions","organizations_url":"https://api.github.com/users/irfan420x/orgs","repos_url":"https://api.github.com/users/irfan420x/repos","events_url":"https://api.github.com/users/irfan420x/events{/privacy}","received_events_url":"https://api.github.com/users/irfan420x/received_events","type":"User","site_admin":false},"html_url":"https://github.com/irfan420x/repo-%d","description":"Repository number %d: a Go OS agent with a Telegram gateway and a CLI, built for stability first. %s","fork":false,"url":"https://api.github.com/repos/irfan420x/repo-%d","forks_url":"https://api.github.com/repos/irfan420x/repo-%d/forks","keys_url":"https://api.github.com/repos/irfan420x/repo-%d/keys{/key_id}","collaborators_url":"https://api.github.com/repos/irfan420x/repo-%d/collaborators{/collaborator}","teams_url":"https://api.github.com/repos/irfan420x/repo-%d/teams","hooks_url":"https://api.github.com/repos/irfan420x/repo-%d/hooks","issue_events_url":"https://api.github.com/repos/irfan420x/repo-%d/issues/events{/number}","events_url":"https://api.github.com/repos/irfan420x/repo-%d/events","assignees_url":"https://api.github.com/repos/irfan420x/repo-%d/assignees{/user}","branches_url":"https://api.github.com/repos/irfan420x/repo-%d/branches{/branch}","tags_url":"https://api.github.com/repos/irfan420x/repo-%d/tags","blobs_url":"https://api.github.com/repos/irfan420x/repo-%d/git/blobs{/sha}","git_tags_url":"https://api.github.com/repos/irfan420x/repo-%d/git/tags{/sha}","git_refs_url":"https://api.github.com/repos/irfan420x/repo-%d/git/refs{/sha}","trees_url":"https://api.github.com/repos/irfan420x/repo-%d/git/trees{/sha}","statuses_url":"https://api.github.com/repos/irfan420x/repo-%d/statuses/{sha}","languages_url":"https://api.github.com/repos/irfan420x/repo-%d/languages","stargazers_url":"https://api.github.com/repos/irfan420x/repo-%d/stargazers","contributors_url":"https://api.github.com/repos/irfan420x/repo-%d/contributors","subscribers_url":"https://api.github.com/repos/irfan420x/repo-%d/subscribers","subscription_url":"https://api.github.com/repos/irfan420x/repo-%d/subscription","issues_url":"https://api.github.com/repos/irfan420x/repo-%d/issues{/number}","pulls_url":"https://api.github.com/repos/irfan420x/repo-%d/pulls{/number}","milestones_url":"https://api.github.com/repos/irfan420x/repo-%d/milestones{/number}","notifications_url":"https://api.github.com/repos/irfan420x/repo-%d/notifications{?since,all,participating}","labels_url":"https://api.github.com/repos/irfan420x/repo-%d/labels{/name}","releases_url":"https://api.github.com/repos/irfan420x/repo-%d/releases{/id}","deployments_url":"https://api.github.com/repos/irfan420x/repo-%d/deployments","created_at":"2026-11-13T00:00:00Z","updated_at":"2026-11-14T00:00:00Z","pushed_at":"2026-11-14T09:00:00Z","homepage":"","size":1234,"stargazers_count":%d,"watchers_count":%d,"language":"Go","has_issues":true,"has_projects":true,"has_downloads":true,"has_wiki":true,"has_pages":false,"has_discussions":false,"topics":["agent","go","telegram"],"permissions":{"admin":false,"maintain":false,"push":true,"triage":true,"pull":true},"license":{"key":"mit","name":"MIT License","spdx_id":"MIT","url":"https://api.github.com/licenses/mit"},"default_branch":"main","open_issues":0,"watchers":%d}`,
			1000+i, strings.Repeat("a", 400), i, i, strings.Repeat("b", 400), i, i, strings.Repeat("detailed description text. ", 60), i, i, i, i, i, i, i, i, i, i, i, i, i, i, i, i, i, i, i, i, i, i, i, i, i, i, i, i, i, i, i)
	}
	sb.WriteString("]")
	return sb.String()
}

func rc23W3FetchServer(t *testing.T, contentType, body string) *httptest.Server {
	t.Helper()
	allowPrivateFetchHosts = true
	t.Cleanup(func() { allowPrivateFetchHosts = false })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRC23W3FetchExtractsGitHubHTML (fetch-cost fix): a ~200KB
// GitHub HTML page shrinks to a small fraction of its raw size —
// clean text only, no markup, no scripts — while keeping the
// content that matters.
func TestRC23W3FetchExtractsGitHubHTML(t *testing.T) {
	raw := rc23W3GitHubHTML(t)
	t.Logf("raw HTML page: %d bytes", len(raw))
	srv := rc23W3FetchServer(t, "text/html; charset=utf-8", raw)
	f := &webFetchTool{}
	out, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatalf("web_fetch: %v", err)
	}
	ratio := float64(len(out)) / float64(len(raw))
	t.Logf("extracted output: %d bytes (%.1f%% of raw)", len(out), ratio*100)
	if ratio >= 0.10 {
		t.Errorf("HTML shrink ratio = %.1f%%, want < 10%% (raw %d bytes, out %d bytes)", ratio*100, len(raw), len(out))
	}
	if !strings.Contains(out, "repo-0") {
		t.Errorf("extracted output lost the repository names: %q", out[:min(500, len(out))])
	}
	if strings.Contains(out, "SECRET_JUNK_XYZ") || strings.Contains(out, "body{margin:0") {
		t.Error("extracted output still contains script/style content")
	}
	if strings.Contains(out, "<div") || strings.Contains(out, "</div>") {
		t.Error("extracted output still contains HTML tags")
	}
}

// TestRC23W3FetchCompactsReposJSON (fetch-cost fix): a 68KB
// repos.json — the exact shape that cost ~17,900 tokens raw —
// compacts to a small fraction: one short line per repo, nested
// metadata summarised away.
func TestRC23W3FetchCompactsReposJSON(t *testing.T) {
	raw := rc23W3ReposJSON(t)
	t.Logf("raw repos.json: %d bytes", len(raw))
	srv := rc23W3FetchServer(t, "application/json; charset=utf-8", raw)
	f := &webFetchTool{}
	out, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatalf("web_fetch: %v", err)
	}
	ratio := float64(len(out)) / float64(len(raw))
	t.Logf("compacted output: %d bytes (%.1f%% of raw)", len(out), ratio*100)
	if ratio >= 0.10 {
		t.Errorf("JSON shrink ratio = %.1f%%, want < 10%% (raw %d bytes, out %d bytes)", ratio*100, len(raw), len(out))
	}
	if !strings.Contains(out, "repo-0") {
		t.Errorf("compacted output lost the repo names: %q", out[:min(500, len(out))])
	}
	if strings.Contains(out, "avatar_url") {
		t.Error("compacted output still contains nested owner metadata (avatar_url)")
	}
	if strings.Contains(out, strings.Repeat("a", 400)) {
		t.Error("compacted output still contains an untruncated 400-char node_id value")
	}
	if !strings.Contains(out, "[truncated]") {
		t.Error("compacted output should note that not all 13 repos fit the default budget")
	}
}

// TestRC23W3FetchExplicitMaxBytesGetsMore: raising max_bytes
// genuinely returns more — the default cap is a budget, not a
// wall the agent cannot pass.
func TestRC23W3FetchExplicitMaxBytesGetsMore(t *testing.T) {
	raw := rc23W3ReposJSON(t)
	srv := rc23W3FetchServer(t, "application/json; charset=utf-8", raw)
	f := &webFetchTool{}
	def, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatalf("default web_fetch: %v", err)
	}
	full, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL, "max_bytes": 1048576}))
	if err != nil {
		t.Fatalf("large web_fetch: %v", err)
	}
	defRepos := strings.Count(def, "repo-")
	fullRepos := strings.Count(full, "repo-")
	t.Logf("default: %d repo mentions; max_bytes=1M: %d repo mentions", defRepos, fullRepos)
	if fullRepos <= defRepos {
		t.Errorf("raising max_bytes did not return more repos (%d vs %d)", fullRepos, defRepos)
	}
	if !strings.Contains(full, "repo-12") {
		t.Error("max_bytes=1M output should contain all 13 repos (repo-12 missing)")
	}
}

// TestRC23W3FetchSmallJSONMinified: a small JSON object comes
// back minified, exact — extraction must not mangle small
// payloads.
func TestRC23W3FetchSmallJSONMinified(t *testing.T) {
	srv := rc23W3FetchServer(t, "application/json", `{ "a": 1, "b": [1, 2], "c": "hi" }`)
	f := &webFetchTool{}
	out, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatalf("web_fetch: %v", err)
	}
	if out != `{"a":1,"b":[1,2],"c":"hi"}` {
		t.Errorf("minified JSON = %q, want compact form", out)
	}
}

// TestRC23W3ResolveHostNamesTrustedCIDRs (queued user item): a
// refusal names the configured trusted egress CIDRs, so the
// model explains the real cause instead of blaming the user's
// network — and says "none configured" when there are none.
func TestRC23W3ResolveHostNamesTrustedCIDRs(t *testing.T) {
	nets, err := parseTrustedEgressCIDRs([]string{"198.18.0.0/15"})
	if err != nil {
		t.Fatalf("parseTrustedEgressCIDRs: %v", err)
	}
	s := newFetchSession(nets)
	err = s.resolveHost(context.Background(), "10.0.0.5")
	if err == nil {
		t.Fatal("resolveHost(10.0.0.5): expected a refusal, got nil")
	}
	if !strings.Contains(err.Error(), "198.18.0.0/15") {
		t.Errorf("refusal = %q, want it to name the trusted egress CIDR 198.18.0.0/15", err)
	}
	if !strings.Contains(err.Error(), "non-public") {
		t.Errorf("refusal = %q, want the non-public wording kept", err)
	}

	// No trusted CIDRs: the error says so.
	s2 := newFetchSession(nil)
	err = s2.resolveHost(context.Background(), "10.0.0.5")
	if err == nil {
		t.Fatal("resolveHost with no trusted CIDRs: expected a refusal, got nil")
	}
	if !strings.Contains(err.Error(), "none configured") {
		t.Errorf("refusal = %q, want it to say no trusted CIDRs are configured", err)
	}

	// A hostname resolving into an untrusted private range names
	// the CIDRs too.
	useScriptedResolver(t, &scriptedResolver{
		answers: map[string][][]net.IP{"example.com": {{net.ParseIP("10.1.2.3")}}},
		calls:   map[string]int{},
	})
	err = s.resolveHost(context.Background(), "example.com")
	if err == nil {
		t.Fatal("resolveHost(example.com → 10.1.2.3): expected a refusal, got nil")
	}
	if !strings.Contains(err.Error(), "198.18.0.0/15") {
		t.Errorf("hostname refusal = %q, want it to name the trusted egress CIDR", err)
	}
}

// TestRC23W3SearchDDGCaptcha (queued user item): when DuckDuckGo
// answers both endpoints with the 202 bot challenge, web_search
// reports "search backend is blocking automated requests right
// now" instead of "no results".
func TestRC23W3SearchDDGCaptcha(t *testing.T) {
	allowPrivateFetchHosts = true
	t.Cleanup(func() { allowPrivateFetchHosts = false })
	challenge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("<html><body><form>Please complete the challenge</form></body></html>"))
	}))
	defer challenge.Close()
	oldAPI, oldLite := searchAPIBase, searchLiteBase
	searchAPIBase, searchLiteBase = challenge.URL, challenge.URL
	t.Cleanup(func() { searchAPIBase, searchLiteBase = oldAPI, oldLite })

	s := &webSearchTool{}
	out, err := s.Execute(context.Background(), mustJSON(t, map[string]any{"query": "golang"}))
	if err != nil {
		t.Fatalf("web_search on captcha: unexpected error %v", err)
	}
	if !strings.Contains(out, "blocking automated requests") {
		t.Errorf("web_search captcha reply = %q, want the backend-blocking message", out)
	}
	if strings.Contains(out, "No results") {
		t.Errorf("web_search captcha reply = %q, must not claim no results", out)
	}
}

// TestRC23W3SearchDDGCaptchaLiteBody: a 200 lite page that is a
// challenge page (no results, names the challenge) is detected
// the same way when the API endpoint is unreachable.
func TestRC23W3SearchDDGCaptchaLiteBody(t *testing.T) {
	allowPrivateFetchHosts = true
	t.Cleanup(func() { allowPrivateFetchHosts = false })
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("challenge"))
	}))
	defer api.Close()
	lite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><p>Please complete the following challenge to continue.</p></body></html>"))
	}))
	defer lite.Close()
	oldAPI, oldLite := searchAPIBase, searchLiteBase
	searchAPIBase, searchLiteBase = api.URL, lite.URL
	t.Cleanup(func() { searchAPIBase, searchLiteBase = oldAPI, oldLite })

	s := &webSearchTool{}
	out, err := s.Execute(context.Background(), mustJSON(t, map[string]any{"query": "golang"}))
	if err != nil {
		t.Fatalf("web_search on lite challenge: unexpected error %v", err)
	}
	if !strings.Contains(out, "blocking automated requests") {
		t.Errorf("web_search lite-challenge reply = %q, want the backend-blocking message", out)
	}
}
