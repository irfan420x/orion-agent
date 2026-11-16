package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/model"
)

// mediaFake is a fake Telegram server for the media tests. It
// records every file upload (method, multipart field, filename,
// chat_id, bytes) and every sendMessage text, in call order, and
// serves scripted getUpdates (for the PollStream hook test). A file
// whose name is in failNames fails with a 400 ok:false envelope.
type mediaFake struct {
	mu        sync.Mutex
	calls     []mediaCall
	texts     []string
	failNames map[string]bool
	updates   []Update
	callsN    int
}

type mediaCall struct {
	method   string
	field    string
	filename string
	chatID   int64
	data     []byte
}

func (f *mediaFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/sendDocument"),
		strings.HasSuffix(r.URL.Path, "/sendPhoto"),
		strings.HasSuffix(r.URL.Path, "/sendVideo"),
		strings.HasSuffix(r.URL.Path, "/sendAudio"),
		strings.HasSuffix(r.URL.Path, "/sendVoice"):
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		field, filename, chatID, data, err := parseMultipartForm(r)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"ok":false,"description":"bad multipart"}`))
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, mediaCall{method: method, field: field, filename: filename, chatID: chatID, data: data})
		fail := f.failNames[filename]
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: simulated upload failure"}`))
			return
		}
		w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			ChatID int64  `json:"chat_id"`
			Text   string `json:"text"`
		}
		if err := json.Unmarshal(body, &msg); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"ok":false,"description":"bad json"}`))
			return
		}
		f.mu.Lock()
		f.texts = append(f.texts, msg.Text)
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true,"result":{"message_id":2}}`))
	case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
		w.Write([]byte(`{"ok":true,"result":true}`))
	case strings.HasSuffix(r.URL.Path, "/getUpdates"):
		f.mu.Lock()
		f.callsN++
		call := f.callsN
		f.mu.Unlock()
		var out []Update
		if call == 1 {
			out = f.updates
		}
		data, _ := json.Marshal(map[string]any{"ok": true, "result": out})
		w.Write(data)
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		w.Write([]byte(`{"ok":true,"result":{"id":1,"username":"orion_test_bot","first_name":"Orion"}}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"ok":false,"description":"not found"}`))
	}
}

func (f *mediaFake) snapshot() (calls []mediaCall, texts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mediaCall(nil), f.calls...), append([]string(nil), f.texts...)
}

// parseMultipartForm reads one multipart/form-data upload: the
// chat_id field and the single file part. It returns the file
// field name, the part filename, the chat id, and the file bytes.
func parseMultipartForm(r *http.Request) (field, filename string, chatID int64, data []byte, err error) {
	ct := r.Header.Get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return "", "", 0, nil, err
	}
	if mt != "multipart/form-data" {
		return "", "", 0, nil, fmt.Errorf("content type %q, want multipart/form-data", mt)
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", 0, nil, err
		}
		b, err := io.ReadAll(part)
		if err != nil {
			return "", "", 0, nil, err
		}
		if part.FileName() != "" {
			field = part.FormName()
			filename = part.FileName()
			data = b
		} else if part.FormName() == "chat_id" {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
				chatID = n
			}
		}
		_ = part.Close()
	}
	return field, filename, chatID, data, nil
}

// writeMediaFile creates dir/name with content and returns its path.
func writeMediaFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// mediaGateway builds a gateway against fake pointed at a media
// config that allowlists dir (plus nothing else).
func mediaGateway(t *testing.T, fake *mediaFake, dir string) (*TelegramGateway, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	mc := config.MediaDeliveryConfig{Enabled: true, AllowDirs: []string{dir}, MaxFiles: 5, MaxBytes: 50 << 20}
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithMediaDelivery(mc))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	return g, srv
}

func TestExtractMediaTags(t *testing.T) {
	one := "Done.\nMEDIA:/tmp/a.csv\n"
	if cleaned, reqs, excess := extractMediaTags(one, 5); cleaned != "Done.\n" || len(reqs) != 1 || reqs[0].path != "/tmp/a.csv" || len(excess) != 0 {
		t.Fatalf("one tag: cleaned=%q reqs=%v excess=%v", cleaned, reqs, excess)
	}

	several := "A\nMEDIA:/tmp/a.csv\nB\nMEDIA:/tmp/b.png\nC\n"
	if cleaned, reqs, _ := extractMediaTags(several, 5); cleaned != "A\nB\nC\n" || len(reqs) != 2 {
		t.Fatalf("several tags: cleaned=%q reqs=%v", cleaned, reqs)
	}

	spaced := "A\n  MEDIA:   /tmp/a.csv  \nB\n"
	if cleaned, reqs, _ := extractMediaTags(spaced, 5); cleaned != "A\nB\n" || len(reqs) != 1 || reqs[0].path != "/tmp/a.csv" {
		t.Fatalf("spaced tag: cleaned=%q reqs=%v", cleaned, reqs)
	}

	tilde := "MEDIA:~/work/chart.png\n"
	if _, reqs, _ := extractMediaTags(tilde, 5); len(reqs) != 1 || reqs[0].path != "~/work/chart.png" {
		t.Fatalf("tilde path must be kept as-is for later expansion: reqs=%v", reqs)
	}

	midSentence := "see MEDIA:/tmp/a.csv for details\n"
	if cleaned, reqs, _ := extractMediaTags(midSentence, 5); cleaned != midSentence || len(reqs) != 0 {
		t.Fatalf("mid-sentence MEDIA: must not be extracted: cleaned=%q reqs=%v", cleaned, reqs)
	}

	noPath := "MEDIA:\nMEDIA:   \n"
	if cleaned, reqs, _ := extractMediaTags(noPath, 5); cleaned != noPath || len(reqs) != 0 {
		t.Fatalf("MEDIA: with no path must not be extracted: cleaned=%q reqs=%v", cleaned, reqs)
	}

	dupes := "MEDIA:/tmp/a.csv\nMEDIA:/tmp/a.csv\nMEDIA:/tmp/b.csv\n"
	if _, reqs, _ := extractMediaTags(dupes, 5); len(reqs) != 2 || reqs[0].path != "/tmp/a.csv" || reqs[1].path != "/tmp/b.csv" {
		t.Fatalf("duplicate paths must dedupe to one: reqs=%v", reqs)
	}

	seven := "MEDIA:/tmp/1.csv\nMEDIA:/tmp/2.csv\nMEDIA:/tmp/3.csv\nMEDIA:/tmp/4.csv\nMEDIA:/tmp/5.csv\nMEDIA:/tmp/6.csv\nMEDIA:/tmp/7.csv\n"
	if cleaned, reqs, excess := extractMediaTags(seven, 5); strings.Contains(cleaned, "MEDIA:") || len(reqs) != 5 || len(excess) != 2 || excess[0] != "/tmp/6.csv" || excess[1] != "/tmp/7.csv" {
		t.Fatalf(">5 tags: cleaned=%q reqs=%v excess=%v", cleaned, reqs, excess)
	}

	if cleaned, reqs, excess := extractMediaTags("", 5); cleaned != "" || len(reqs) != 0 || len(excess) != 0 {
		t.Fatalf("empty reply: cleaned=%q reqs=%v excess=%v", cleaned, reqs, excess)
	}

	plain := "just prose, no tags\nsecond line\n"
	if cleaned, reqs, excess := extractMediaTags(plain, 5); cleaned != plain || len(reqs) != 0 || len(excess) != 0 {
		t.Fatalf("no tags: cleaned=%q reqs=%v excess=%v", cleaned, reqs, excess)
	}
}

func TestExtractMediaTagsStripsOnlyTagLines(t *testing.T) {
	reply := "Done — the report is ready.\n\nMEDIA:/tmp/a.csv\n\nSome more prose.\nMEDIA:/tmp/b.png\n— deepseek-chat · 1,240 tokens · 3 steps · 4.2s\n"
	want := "Done — the report is ready.\n\n\nSome more prose.\n— deepseek-chat · 1,240 tokens · 3 steps · 4.2s\n"
	cleaned, reqs, _ := extractMediaTags(reply, 5)
	if cleaned != want {
		t.Fatalf("cleaned=%q, want %q", cleaned, want)
	}
	if len(reqs) != 2 {
		t.Fatalf("reqs=%v, want 2", reqs)
	}
}

func TestMediaRouteTable(t *testing.T) {
	cases := map[string]string{
		".png": "sendPhoto", ".jpg": "sendPhoto", ".jpeg": "sendPhoto", ".gif": "sendPhoto", ".webp": "sendPhoto",
		".mp4": "sendVideo", ".mov": "sendVideo", ".mkv": "sendVideo", ".webm": "sendVideo", ".avi": "sendVideo",
		".ogg": "sendVoice", ".opus": "sendVoice",
		".mp3": "sendAudio", ".m4a": "sendAudio", ".wav": "sendAudio", ".flac": "sendAudio",
		".pdf": "sendDocument", ".txt": "sendDocument", ".md": "sendDocument",
		".csv": "sendDocument", ".json": "sendDocument", ".zip": "sendDocument",
	}
	for ext, want := range cases {
		if got := routeMedia(ext); got != want {
			t.Errorf("routeMedia(%q) = %q, want %q", ext, got, want)
		}
	}
	// Case-insensitive: .PNG is a photo, .Mp4 a video.
	if got := routeMedia(".PNG"); got != "sendPhoto" {
		t.Errorf("routeMedia(%q) = %q, want sendPhoto", ".PNG", got)
	}
	if got := routeMedia(".Mp4"); got != "sendVideo" {
		t.Errorf("routeMedia(%q) = %q, want sendVideo", ".Mp4", got)
	}
}

// TestMediaRouteUnknownExtensionIsDocument is the anti-Hermes-#34517
// test: an unknown extension must still send (as a document), never
// leave the tag visible with the file unsent.
func TestMediaRouteUnknownExtensionIsDocument(t *testing.T) {
	for _, ext := range []string{".py", ".log", ".zzz", ""} {
		if got := routeMedia(ext); got != "sendDocument" {
			t.Errorf("routeMedia(%q) = %q, want sendDocument", ext, got)
		}
	}
}

func TestValidateMediaPathDenylist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	allow := t.TempDir()
	writeMediaFile(t, filepath.Join(home, ".orion"), "config.json", `{"x":1}`)
	writeMediaFile(t, filepath.Join(home, ".ssh"), "id_ed25519", "key")
	writeMediaFile(t, filepath.Join(home, ".gnupg"), "x", "x")

	denied := map[string]string{
		filepath.Join(home, ".orion", "config.json"):      "config.json under ~/.orion",
		filepath.Join(home, ".orion", ".git-credentials"): ".git-credentials under ~/.orion",
		filepath.Join(home, ".ssh", "id_ed25519"):         "private key under ~/.ssh",
		filepath.Join(home, ".gnupg", "pubring.kbx"):      "file under ~/.gnupg",
		"/etc/hostname": "file under /etc",
	}
	writeMediaFile(t, filepath.Join(home, ".orion"), ".git-credentials", "secret")
	writeMediaFile(t, filepath.Join(home, ".gnupg"), "pubring.kbx", "x")
	// Basename patterns, all inside the allowlisted dir.
	writeMediaFile(t, allow, "app.env", "SECRET=1")
	writeMediaFile(t, allow, "app.env.local", "SECRET=1")
	writeMediaFile(t, allow, "cert.pem", "x")
	writeMediaFile(t, allow, "server.key", "x")
	writeMediaFile(t, allow, "id_ecdsa", "x")
	writeMediaFile(t, allow, "my-credentials.json", "x")
	denied[filepath.Join(allow, "app.env")] = "*.env"
	denied[filepath.Join(allow, "app.env.local")] = "*.env.*"
	denied[filepath.Join(allow, "cert.pem")] = "*.pem"
	denied[filepath.Join(allow, "server.key")] = "*.key"
	denied[filepath.Join(allow, "id_ecdsa")] = "id_*"
	denied[filepath.Join(allow, "my-credentials.json")] = "*credentials*"

	for path, why := range denied {
		if _, _, reason := validateMediaPath(path, []string{allow}, 50<<20); reason == "" {
			t.Errorf("validateMediaPath(%s) (%s): allowed, want refusal", path, why)
		} else if reason != "that path is never sendable" {
			t.Errorf("validateMediaPath(%s) (%s): reason=%q, want %q", path, why, reason, "that path is never sendable")
		}
	}

	// A plain file in the allowlisted dir passes.
	p := writeMediaFile(t, allow, "report.csv", "a,b\n1,2\n")
	if resolved, size, reason := validateMediaPath(p, []string{allow}, 50<<20); reason != "" || resolved == "" || size == 0 {
		t.Errorf("validateMediaPath(allowed file): resolved=%q size=%d reason=%q, want success", resolved, size, reason)
	}
}

// TestValidateMediaPathProcDenied pins F-M1: /proc/self/environ
// (the daemon's live environment, including any secret passed via
// env vars) must be refused, and so must /sys and /dev. Proc files
// report size 0, so the size cap never binds — the denylist is the
// only line of defence here. The /dev and /sys paths are checked
// through mediaPathDenied directly: /dev holds no regular files to
// validate end-to-end, and /sys files are kernel-owned.
func TestValidateMediaPathProcDenied(t *testing.T) {
	allow := t.TempDir()
	for _, p := range []string{"/proc/self/environ", "/proc/version", "/proc/self/cmdline"} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("%s not present in this environment: %v", p, err)
		}
		if _, _, reason := validateMediaPath(p, []string{allow}, 50<<20); reason != "that path is never sendable" {
			t.Errorf("validateMediaPath(%s): reason=%q, want %q", p, reason, "that path is never sendable")
		}
	}
	for _, p := range []string{"/proc/self/environ", "/proc/version", "/sys/kernel/ostype", "/dev/null", "/dev/zero"} {
		if !mediaPathDenied(p) {
			t.Errorf("mediaPathDenied(%q) = false, want true", p)
		}
	}
	// A symlink inside the allowed dir pointing at /proc/self/environ
	// must be refused too: the denylist is checked on the resolved path.
	link := filepath.Join(allow, "env.txt")
	if err := os.Symlink("/proc/self/environ", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, _, reason := validateMediaPath(link, []string{allow}, 50<<20); reason != "that path is never sendable" {
		t.Errorf("symlink to /proc/self/environ: reason=%q, want %q", reason, "that path is never sendable")
	}
}

func TestValidateMediaPathSymlinkEscape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	allow := t.TempDir()
	secret := writeMediaFile(t, filepath.Join(home, ".orion"), "config.json", `{"x":1}`)
	// A symlink inside the allowed dir pointing at a denied file
	// must be refused: the denylist is checked on the RESOLVED path.
	link := filepath.Join(allow, "link.json")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, _, reason := validateMediaPath(link, []string{allow}, 50<<20); reason != "that path is never sendable" {
		t.Fatalf("symlink escape: reason=%q, want %q", reason, "that path is never sendable")
	}
	// A symlink to an allowed file resolves and is sendable.
	real := writeMediaFile(t, allow, "real.csv", "a,b\n")
	good := filepath.Join(allow, "alias.csv")
	if err := os.Symlink(real, good); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if resolved, _, reason := validateMediaPath(good, []string{allow}, 50<<20); reason != "" || resolved != real {
		t.Fatalf("symlink to allowed file: resolved=%q reason=%q, want %q", resolved, reason, real)
	}
}

// TestValidateMediaPathHardlinkRefused pins F-A23W2-6: a hard link
// inside an allowed dir pointing at a denied file must be refused.
// EvalSymlinks cannot see a hard link — the resolved path is the
// allowed alias, not the denied target — so the nlink>1 check is
// the only defence. Repro from the audit: ln
// ~/.orion/config.json ~/Downloads/report.json, then
// MEDIA:~/Downloads/report.json must not send the config.
func TestValidateMediaPathHardlinkRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	allow := t.TempDir()
	secret := writeMediaFile(t, filepath.Join(home, ".orion"), "config.json", `{"token":"x"}`)
	link := filepath.Join(allow, "report.json")
	if err := os.Link(secret, link); err != nil {
		t.Skipf("hard links unavailable in this environment: %v", err)
	}
	if _, _, reason := validateMediaPath(link, []string{allow}, 50<<20); reason != "file has more than one hard link; refusing to send" {
		t.Fatalf("hard link to denied file: reason=%q, want the hard-link refusal", reason)
	}
	// A hard link to an allowed file is refused too: the check is
	// on the link count, not on where the other name lives — the
	// validator cannot know the other name is safe.
	real := writeMediaFile(t, allow, "real.csv", "a,b\n")
	alias := filepath.Join(allow, "alias.csv")
	if err := os.Link(real, alias); err != nil {
		t.Fatalf("hard link to allowed file: %v", err)
	}
	if _, _, reason := validateMediaPath(alias, []string{allow}, 50<<20); reason != "file has more than one hard link; refusing to send" {
		t.Fatalf("hard link to allowed file: reason=%q, want the hard-link refusal", reason)
	}
	// End to end: the tag is replaced by a visible refusal note
	// and nothing reaches the server.
	fake := &mediaFake{}
	g, _ := mediaGateway(t, fake, allow)
	cleaned, report := g.deliverMedia(context.Background(), 77, "Here.\nMEDIA:"+link+"\n")
	calls, _ := fake.snapshot()
	if len(calls) != 0 {
		t.Fatalf("server saw %d file calls, want 0 (hardlinked file must not be sent)", len(calls))
	}
	if strings.Contains(cleaned, "MEDIA:") {
		t.Errorf("cleaned reply still contains a MEDIA: line: %q", cleaned)
	}
	if !strings.Contains(report, "more than one hard link") {
		t.Errorf("report = %q, want the hard-link refusal", report)
	}
}

func TestValidateMediaPathNotAFile(t *testing.T) {
	allow := t.TempDir()
	if _, _, reason := validateMediaPath(allow, []string{allow}, 50<<20); reason != "is a directory, not a file" {
		t.Errorf("directory: reason=%q, want %q", reason, "is a directory, not a file")
	}
	missing := filepath.Join(allow, "nope.csv")
	if _, _, reason := validateMediaPath(missing, []string{allow}, 50<<20); reason != "not found" {
		t.Errorf("missing file: reason=%q, want %q", reason, "not found")
	}
	fifo := filepath.Join(allow, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("Mkfifo unavailable in this environment: %v", err)
	}
	if _, _, reason := validateMediaPath(fifo, []string{allow}, 50<<20); reason != "is not a regular file" {
		t.Errorf("FIFO: reason=%q, want %q", reason, "is not a regular file")
	}
}

func TestValidateMediaPathRelative(t *testing.T) {
	allow := t.TempDir()
	for _, p := range []string{"report.csv", "./report.csv", "../x.csv"} {
		if _, _, reason := validateMediaPath(p, []string{allow}, 50<<20); reason != "path must be absolute" {
			t.Errorf("validateMediaPath(%q): reason=%q, want %q", p, reason, "path must be absolute")
		}
	}
}

func TestValidateMediaPathSize(t *testing.T) {
	allow := t.TempDir()
	// A sparse file over 50 MiB: Truncate, so the test writes no
	// 50 MB of bytes.
	big := filepath.Join(allow, "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatalf("create big file: %v", err)
	}
	if err := f.Truncate((50 << 20) + 1); err != nil {
		f.Close()
		t.Fatalf("truncate big file: %v", err)
	}
	f.Close()
	if _, _, reason := validateMediaPath(big, []string{allow}, 50<<20); !strings.HasPrefix(reason, "file is too large") {
		t.Errorf("over-limit file: reason=%q, want too-large refusal", reason)
	}
	small := writeMediaFile(t, allow, "small.csv", "a\n")
	if _, size, reason := validateMediaPath(small, []string{allow}, 50<<20); reason != "" || size != 2 {
		t.Errorf("under-limit file: size=%d reason=%q, want success", size, reason)
	}
}

func TestValidateMediaPathAllowlist(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	work2 := filepath.Join(base, "work2")
	inside := writeMediaFile(t, work, "a.csv", "x")
	if _, _, reason := validateMediaPath(inside, []string{work}, 50<<20); reason != "" {
		t.Errorf("inside allowlist: reason=%q, want success", reason)
	}
	outside := writeMediaFile(t, work2, "b.csv", "x")
	if _, _, reason := validateMediaPath(outside, []string{work}, 50<<20); !strings.Contains(reason, "outside the media allowlist") {
		t.Errorf("outside allowlist: reason=%q, want allowlist refusal", reason)
	}
	// Sibling-dir boundary: /tmp/work2 must not match an allowlist
	// entry of /tmp/work (a shared-prefix match would be a hole).
	if _, _, reason := validateMediaPath(outside, []string{work}, 50<<20); reason == "" {
		t.Errorf("sibling dir %q matched allowlist %q", work2, work)
	}
	// A path under the allowlisted dir itself (the dir, not a
	// child) is inside: the dir entry is its own root.
	if _, _, reason := validateMediaPath(filepath.Join(work, "sub", "c.csv"), []string{work}, 50<<20); reason != "not found" {
		t.Errorf("missing file under allowlist: reason=%q, want not-found (allowlist passed first)", reason)
	}
}

// TestMediaDenylistSupersetOfSensitiveDirs pins that the media
// denylist contains every entry of internal/tools sensitiveDirs()
// (files.go: ~/.orion, ~/.ssh, ~/.gnupg) plus /etc and the virtual
// filesystems /proc, /sys and /dev (F-M1: /proc/self/environ must
// never be sendable). sensitiveDirs()
// is unexported in another package, so the three literals are
// asserted here with a comment pointing at it; if the file-tools
// list ever gains an entry, this test must gain it too.
func TestMediaDenylistSupersetOfSensitiveDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := mediaDenylistDirs()
	want := []string{
		filepath.Join(home, ".orion"), // sensitiveDirs() in internal/tools/files.go
		filepath.Join(home, ".ssh"),   // sensitiveDirs() in internal/tools/files.go
		filepath.Join(home, ".gnupg"), // sensitiveDirs() in internal/tools/files.go
		"/etc",
		"/proc", // F-M1: /proc/self/environ exposes daemon env secrets
		"/sys",  // F-M1: kernel state, same class as /proc
		"/dev",  // F-M1: device nodes, same class as /proc
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("media denylist %v is missing %q", got, w)
		}
	}
}

func TestMediaConfigValidation(t *testing.T) {
	zero := config.MediaDeliveryConfig{}.Resolve()
	if zero.MaxFiles != config.DefaultMediaMaxFiles || zero.MaxBytes != config.DefaultMediaMaxBytes {
		t.Errorf("zero config Resolve: MaxFiles=%d MaxBytes=%d, want %d/%d", zero.MaxFiles, zero.MaxBytes, config.DefaultMediaMaxFiles, config.DefaultMediaMaxBytes)
	}
	if zero.Enabled {
		t.Errorf("zero config Resolve: Enabled=true, want false (a zero config stays disabled)")
	}
	huge := config.MediaDeliveryConfig{Enabled: true, MaxFiles: 100, MaxBytes: 500 << 20}.Resolve()
	if huge.MaxFiles != config.MaxMediaFiles {
		t.Errorf("MaxFiles=100: resolved to %d, want %d", huge.MaxFiles, config.MaxMediaFiles)
	}
	if huge.MaxBytes != config.DefaultMediaMaxBytes {
		t.Errorf("MaxBytes=500MiB: resolved to %d, want %d", huge.MaxBytes, config.DefaultMediaMaxBytes)
	}
	def := config.DefaultMediaDeliveryConfig()
	if !def.Enabled || def.MaxFiles != 5 || def.MaxBytes != 50<<20 || len(def.AllowDirs) != 0 {
		t.Errorf("DefaultMediaDeliveryConfig = %+v, want enabled, 5 files, 50 MiB, no allow dirs", def)
	}
	// Enabled=false disables extraction: deliverMedia returns the
	// reply untouched and no file is sent.
	fake := &mediaFake{}
	g, _ := mediaGateway(t, fake, t.TempDir())
	g.mediaCfg.Enabled = false
	reply := "Done.\nMEDIA:/tmp/a.csv\n"
	if cleaned, report := g.deliverMedia(context.Background(), 77, reply); cleaned != reply || report != "" {
		t.Errorf("disabled deliverMedia: cleaned=%q report=%q, want reply untouched", cleaned, report)
	}
	// No option at all also disables extraction.
	g2, err := NewTelegramGateway(testToken)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if cleaned, report := g2.deliverMedia(context.Background(), 77, reply); cleaned != reply || report != "" {
		t.Errorf("no-option deliverMedia: cleaned=%q report=%q, want reply untouched", cleaned, report)
	}
}

// TestWithMediaDeliveryDefaultIsDenylistOnly REPLACES
// TestWithMediaDeliveryDefaultAllowlist: the default allowlist
// ([~/orion-media, /tmp]) was removed by the user's order
// 2026-11-15 09:35 — delivery is denylist-only by default, so a
// file may be sent from anywhere on the machine (here ~/Downloads
// and ~/Documents) except the denylist. An explicitly configured
// media_allow_dirs still restricts (see
// TestDeliverMediaRefusalIsVisible, which uses an explicit list).
func TestWithMediaDeliveryDefaultIsDenylistOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	song := writeMediaFile(t, filepath.Join(home, "Downloads"), "Akash.mp3", "fake-audio")
	notes := writeMediaFile(t, filepath.Join(home, "Documents"), "notes.csv", "a,b\n")
	secret := writeMediaFile(t, filepath.Join(home, ".orion"), "config.json", "x")
	fake := &mediaFake{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithMediaDelivery(config.DefaultMediaDeliveryConfig()))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if len(g.mediaAllow) != 0 {
		t.Fatalf("default mediaAllow = %v, want empty (denylist-only, no default dirs)", g.mediaAllow)
	}
	reply := "Done.\nMEDIA:" + song + "\nMEDIA:" + notes + "\nMEDIA:" + secret + "\n"
	cleaned, report := g.deliverMedia(context.Background(), 77, reply)
	calls, _ := fake.snapshot()
	if len(calls) != 2 {
		t.Fatalf("server saw %d file calls, want 2 (Downloads + Documents sent, denylisted file refused)", len(calls))
	}
	if calls[0].method != "sendAudio" || calls[0].filename != "Akash.mp3" {
		t.Errorf("first call = %+v, want sendAudio Akash.mp3", calls[0])
	}
	if calls[1].method != "sendDocument" || calls[1].filename != "notes.csv" {
		t.Errorf("second call = %+v, want sendDocument notes.csv", calls[1])
	}
	if strings.Contains(cleaned, "MEDIA:") {
		t.Errorf("cleaned reply still contains a MEDIA: line: %q", cleaned)
	}
	if !strings.Contains(report, "📎 Sent Akash.mp3") {
		t.Errorf("report = %q, want a 📎 Sent line for Akash.mp3", report)
	}
	if !strings.Contains(report, "📎 Sent notes.csv") {
		t.Errorf("report = %q, want a 📎 Sent line for notes.csv", report)
	}
	if !strings.Contains(report, "that path is never sendable") {
		t.Errorf("report = %q, want the denylist refusal for config.json", report)
	}
}

func TestCallMultipartShape(t *testing.T) {
	methods := []struct {
		method string
		send   func(g *TelegramGateway, path string) error
		field  string
	}{
		{"sendDocument", func(g *TelegramGateway, path string) error { return g.SendDocument(context.Background(), 77, path) }, "document"},
		{"sendPhoto", func(g *TelegramGateway, path string) error { return g.SendPhoto(context.Background(), 77, path) }, "photo"},
		{"sendVideo", func(g *TelegramGateway, path string) error { return g.SendVideo(context.Background(), 77, path) }, "video"},
		{"sendAudio", func(g *TelegramGateway, path string) error { return g.SendAudio(context.Background(), 77, path) }, "audio"},
		{"sendVoice", func(g *TelegramGateway, path string) error { return g.SendVoice(context.Background(), 77, path) }, "voice"},
	}
	for _, m := range methods {
		t.Run(m.method, func(t *testing.T) {
			fake := &mediaFake{}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			g := newTestGateway(t, srv)
			dir := t.TempDir()
			p := writeMediaFile(t, dir, "report.csv", "a,b\n1,2\n")
			if err := m.send(g, p); err != nil {
				t.Fatalf("%s: %v", m.method, err)
			}
			calls, _ := fake.snapshot()
			if len(calls) != 1 {
				t.Fatalf("server saw %d calls, want 1", len(calls))
			}
			c := calls[0]
			if c.method != m.method {
				t.Errorf("method = %q, want %q", c.method, m.method)
			}
			if c.field != m.field {
				t.Errorf("field = %q, want %q", c.field, m.field)
			}
			if c.filename != "report.csv" {
				t.Errorf("filename = %q, want basename %q (never a full path)", c.filename, "report.csv")
			}
			if c.chatID != 77 {
				t.Errorf("chat_id = %d, want 77", c.chatID)
			}
			if string(c.data) != "a,b\n1,2\n" {
				t.Errorf("file bytes = %q, want exact file contents", c.data)
			}
		})
	}
}

// TestCallMultipartStreams proves the pipe path works at size: a
// 20 MB sparse-backed file uploads with its exact bytes.
func TestCallMultipartStreams(t *testing.T) {
	fake := &mediaFake{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)
	dir := t.TempDir()
	p := filepath.Join(dir, "big.bin")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(20 << 20); err != nil {
		f.Close()
		t.Fatalf("truncate: %v", err)
	}
	f.Close()
	if err := g.SendDocument(context.Background(), 77, p); err != nil {
		t.Fatalf("SendDocument(20MB): %v", err)
	}
	calls, _ := fake.snapshot()
	if len(calls) != 1 {
		t.Fatalf("server saw %d calls, want 1", len(calls))
	}
	if len(calls[0].data) != 20<<20 {
		t.Fatalf("uploaded %d bytes, want %d", len(calls[0].data), 20<<20)
	}
	for i, b := range calls[0].data {
		if b != 0 {
			t.Fatalf("byte %d = %d, want 0 (sparse file)", i, b)
		}
	}
}

// TestCallMultipartReverifiesAfterSwap pins F-A23W2-12: the upload
// re-verifies the opened file (O_NOFOLLOW + fd-path check +
// denylist/size/hard-link re-checks), so a file swapped between
// validation and open is refused before a byte is read. Each case
// validates first (success), swaps the file, then uploads (must
// fail, with nothing reaching the server).
func TestCallMultipartReverifiesAfterSwap(t *testing.T) {
	t.Run("path swapped to symlink at denied file", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		allow := t.TempDir()
		p := writeMediaFile(t, allow, "report.csv", "a,b\n")
		if _, _, reason := validateMediaPath(p, []string{allow}, 50<<20); reason != "" {
			t.Fatalf("validate before swap: reason=%q, want success", reason)
		}
		secret := writeMediaFile(t, filepath.Join(home, ".orion"), "config.json", `{"token":"x"}`)
		if err := os.Remove(p); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if err := os.Symlink(secret, p); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		fake := &mediaFake{}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		g := newTestGateway(t, srv)
		err := g.SendDocument(context.Background(), 77, p)
		if err == nil {
			t.Fatal("SendDocument(swapped to symlink at denied file): expected error, got nil")
		}
		if !strings.Contains(err.Error(), "symlink") {
			t.Errorf("error = %q, want it to name the symlink refusal", err)
		}
		if calls, _ := fake.snapshot(); len(calls) != 0 {
			t.Fatalf("server saw %d file calls, want 0", len(calls))
		}
	})

	t.Run("parent dir swapped to symlink", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "dir")
		p := writeMediaFile(t, dir, "report.csv", "a,b\n")
		if _, _, reason := validateMediaPath(p, []string{dir}, 50<<20); reason != "" {
			t.Fatalf("validate before swap: reason=%q, want success", reason)
		}
		other := filepath.Join(base, "other")
		writeMediaFile(t, other, "report.csv", "DIFFERENT\n")
		moved := filepath.Join(base, "moved")
		if err := os.Rename(dir, moved); err != nil {
			t.Fatalf("rename: %v", err)
		}
		if err := os.Symlink(other, dir); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		fake := &mediaFake{}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		g := newTestGateway(t, srv)
		err := g.SendDocument(context.Background(), 77, p)
		if err == nil {
			t.Fatal("SendDocument(swapped parent dir): expected error, got nil")
		}
		if !strings.Contains(err.Error(), "changed during upload") {
			t.Errorf("error = %q, want the changed-during-upload refusal", err)
		}
		if calls, _ := fake.snapshot(); len(calls) != 0 {
			t.Fatalf("server saw %d file calls, want 0", len(calls))
		}
	})

	t.Run("hard link added after validation", func(t *testing.T) {
		allow := t.TempDir()
		p := writeMediaFile(t, allow, "report.csv", "a,b\n")
		if _, _, reason := validateMediaPath(p, []string{allow}, 50<<20); reason != "" {
			t.Fatalf("validate before swap: reason=%q, want success", reason)
		}
		if err := os.Link(p, filepath.Join(allow, "second.csv")); err != nil {
			t.Skipf("hard links unavailable in this environment: %v", err)
		}
		fake := &mediaFake{}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		g := newTestGateway(t, srv)
		err := g.SendDocument(context.Background(), 77, p)
		if err == nil {
			t.Fatal("SendDocument(hard link added after validation): expected error, got nil")
		}
		if !strings.Contains(err.Error(), "hard link") {
			t.Errorf("error = %q, want the hard-link refusal", err)
		}
		if calls, _ := fake.snapshot(); len(calls) != 0 {
			t.Fatalf("server saw %d file calls, want 0", len(calls))
		}
	})

	t.Run("direct Send of a symlink is refused", func(t *testing.T) {
		allow := t.TempDir()
		real := writeMediaFile(t, allow, "real.csv", "a,b\n")
		link := filepath.Join(allow, "alias.csv")
		if err := os.Symlink(real, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		fake := &mediaFake{}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		g := newTestGateway(t, srv)
		// No validateMediaPath call here: the exported Send*
		// methods are self-defending at upload time.
		if err := g.SendDocument(context.Background(), 77, link); err == nil {
			t.Fatal("SendDocument(symlink, direct): expected error, got nil")
		}
		if calls, _ := fake.snapshot(); len(calls) != 0 {
			t.Fatalf("server saw %d file calls, want 0", len(calls))
		}
	})
}

// TestCallMultipartEnforcesCapWhileStreaming pins F-A23W2-24: a file
// that grows mid-upload must not exceed the size cap on the wire.
// The file starts at 8 MiB under a 10 MiB cap; the server reads
// slowly, so the pipe backpressures and the writer is still copying
// when a background appender grows the file past the cap — the
// upload must then abort, and the server must never receive more
// than the cap in file bytes. (A small file cannot exercise this:
// the writer races ahead through socket buffers and finishes
// before the file grows, in which case no byte over the cap is
// ever sent and the upload legitimately succeeds.)
func TestCallMultipartEnforcesCapWhileStreaming(t *testing.T) {
	const limit = 10 << 20
	const initial = 8 << 20
	dir := t.TempDir()
	p := filepath.Join(dir, "growing.bin")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(initial); err != nil {
		f.Close()
		t.Fatalf("truncate: %v", err)
	}
	f.Close()

	// Slow counting server: reads the file part in 64 KiB chunks
	// with a 10 ms pause between them, counting the file bytes it
	// actually receives; a fully-read upload gets an ok envelope.
	var mu sync.Mutex
	var fileBytes int64
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		w.Header().Set("Content-Type", "application/json")
		mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "multipart/form-data" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		buf := make([]byte, 64<<10)
		for {
			part, err := mr.NextPart()
			if err != nil {
				if err == io.EOF {
					w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
				}
				return
			}
			if part.FileName() == "" {
				_, _ = io.ReadAll(part)
				_ = part.Close()
				continue
			}
			var sinceSleep int64
			for {
				n, rerr := part.Read(buf)
				mu.Lock()
				fileBytes += int64(n)
				mu.Unlock()
				sinceSleep += int64(n)
				if rerr != nil {
					break
				}
				// Throttle by bytes actually read, not per Read
				// call: multipart may return small chunks, and a
				// sleep per call would stall the drain to
				// ~400 KiB/s. ~6.4 MB/s here.
				if sinceSleep >= 64<<10 {
					sinceSleep = 0
					time.Sleep(10 * time.Millisecond)
				}
			}
			_ = part.Close()
		}
	}))
	defer srv.Close()

	// Grow the file far faster than the server can read it.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		af, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return
		}
		defer af.Close()
		chunk := make([]byte, 32<<10)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := af.Write(chunk); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	mc := config.MediaDeliveryConfig{Enabled: true, MaxFiles: 5, MaxBytes: limit}
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithMediaDelivery(mc))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	err = g.SendDocument(context.Background(), 77, p)
	if err == nil {
		t.Fatal("SendDocument(growing file): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "grew past") {
		t.Errorf("error = %q, want the grew-past-the-cap refusal", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("slow server handler did not finish within 10s")
	}
	mu.Lock()
	got := fileBytes
	mu.Unlock()
	if got > limit {
		t.Errorf("server received %d file bytes, want at most the %d-byte cap", got, limit)
	}
	if got < 1<<20 {
		t.Errorf("server received only %d file bytes, want the upload to have been well underway", got)
	}
}

func TestCallMultipartServerError(t *testing.T) {
	// Telegram 400 with an ok:false envelope: the error carries
	// the description and never the token.
	fake := &mediaFake{failNames: map[string]bool{"bad.csv": true}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)
	dir := t.TempDir()
	p := writeMediaFile(t, dir, "bad.csv", "x")
	err := g.SendDocument(context.Background(), 77, p)
	if err == nil {
		t.Fatal("SendDocument(server 400): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "simulated upload failure") {
		t.Errorf("error = %q, want it to contain the server description", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("error contains the bot token: %q", err)
	}

	// HTTP 500 with a non-JSON body: a clean error, no panic, no token.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("not json at all"))
	}))
	defer srv2.Close()
	g2 := newTestGateway(t, srv2)
	if err := g2.SendDocument(context.Background(), 77, p); err == nil {
		t.Fatal("SendDocument(server 500 non-JSON): expected error, got nil")
	} else if strings.Contains(err.Error(), testToken) {
		t.Errorf("error contains the bot token: %q", err)
	}
}

// TestCallMultipartTokenNeverLogged pins the redaction path: the
// request URL contains the token (as today) but any error
// callMultipart returns never does — including a transport error,
// where *url.Error embeds the full request URL.
func TestCallMultipartTokenNeverLogged(t *testing.T) {
	srv := httptest.NewServer(&mediaFake{})
	srv.Close() // unreachable: client.Do fails with *url.Error
	g := newTestGateway(t, srv)
	dir := t.TempDir()
	p := writeMediaFile(t, dir, "a.csv", "x")
	err := g.SendDocument(context.Background(), 77, p)
	if err == nil {
		t.Fatal("SendDocument(unreachable server): expected error, got nil")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("error contains the bot token: %q", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("error = %q, want the token replaced by [redacted]", err)
	}
}

func TestDeliverMediaEndToEnd(t *testing.T) {
	dir := t.TempDir()
	csv := writeMediaFile(t, dir, "sales.csv", "a,b\n1,2\n3,4\n")
	png := writeMediaFile(t, dir, "chart.png", "\x89PNG\r\n\x1a\n")
	fake := &mediaFake{}
	g, _ := mediaGateway(t, fake, dir)
	reply := "Done — the report is ready.\nMEDIA:" + csv + "\nMEDIA:" + png + "\n"
	cleaned, report := g.deliverMedia(context.Background(), 77, reply)
	calls, texts := fake.snapshot()
	if len(texts) != 0 {
		t.Fatalf("deliverMedia sent %d text messages, want 0 (the caller sends the text)", len(texts))
	}
	if len(calls) != 2 {
		t.Fatalf("server saw %d file calls, want 2", len(calls))
	}
	if calls[0].method != "sendDocument" || calls[0].filename != "sales.csv" {
		t.Errorf("first call = %+v, want sendDocument sales.csv", calls[0])
	}
	if calls[1].method != "sendPhoto" || calls[1].filename != "chart.png" {
		t.Errorf("second call = %+v, want sendPhoto chart.png", calls[1])
	}
	if strings.Contains(cleaned, "MEDIA:") {
		t.Errorf("cleaned reply still contains a MEDIA: line: %q", cleaned)
	}
	if !strings.Contains(cleaned, "Done — the report is ready.") {
		t.Errorf("cleaned reply lost the prose: %q", cleaned)
	}
	if !strings.Contains(report, "📎 Sent sales.csv (12 B)") {
		t.Errorf("report = %q, want a 📎 Sent line for sales.csv", report)
	}
	if !strings.Contains(report, "📎 Sent chart.png (8 B)") {
		t.Errorf("report = %q, want a 📎 Sent line for chart.png", report)
	}
}

func TestDeliverMediaRefusalIsVisible(t *testing.T) {
	dir := t.TempDir()
	fake := &mediaFake{}
	g, _ := mediaGateway(t, fake, dir)
	other := t.TempDir()
	outside := writeMediaFile(t, other, "passport.pdf", "x")
	home := t.TempDir()
	t.Setenv("HOME", home)
	secret := writeMediaFile(t, filepath.Join(home, ".orion"), "config.json", "x")
	reply := "Done.\nMEDIA:" + outside + "\nMEDIA:" + secret + "\n"
	cleaned, report := g.deliverMedia(context.Background(), 77, reply)
	calls, _ := fake.snapshot()
	if len(calls) != 0 {
		t.Fatalf("server saw %d file calls, want 0", len(calls))
	}
	if strings.Contains(cleaned, "MEDIA:") {
		t.Errorf("cleaned reply still contains a MEDIA: line: %q", cleaned)
	}
	if n := strings.Count(report, "⛔"); n != 2 {
		t.Errorf("report = %q, want exactly 2 ⛔ lines", report)
	}
	if !strings.Contains(report, "outside the media allowlist") {
		t.Errorf("report = %q, want the allowlist refusal", report)
	}
	if !strings.Contains(report, "that path is never sendable") {
		t.Errorf("report = %q, want the denylist refusal", report)
	}
}

func TestDeliverMediaOneFailureDoesNotBlockOthers(t *testing.T) {
	dir := t.TempDir()
	first := writeMediaFile(t, dir, "first.csv", "1")
	second := writeMediaFile(t, dir, "second.csv", "2")
	fake := &mediaFake{failNames: map[string]bool{"first.csv": true}}
	g, _ := mediaGateway(t, fake, dir)
	reply := "Here.\nMEDIA:" + first + "\nMEDIA:" + second + "\n"
	cleaned, report := g.deliverMedia(context.Background(), 77, reply)
	calls, _ := fake.snapshot()
	if len(calls) != 2 {
		t.Fatalf("server saw %d file calls, want 2 (the failed one still reached the server)", len(calls))
	}
	if strings.Contains(cleaned, "MEDIA:") {
		t.Errorf("cleaned reply still contains a MEDIA: line: %q", cleaned)
	}
	if !strings.Contains(report, "⛔ Could not send first.csv") {
		t.Errorf("report = %q, want a ⛔ line for first.csv", report)
	}
	if !strings.Contains(report, "📎 Sent second.csv") {
		t.Errorf("report = %q, want a 📎 Sent line for second.csv", report)
	}
}

// TestDeliverMediaDedupesByResolvedPath pins that two spellings of
// one file — the real path and a symlink alias to it — send once.
func TestDeliverMediaDedupesByResolvedPath(t *testing.T) {
	dir := t.TempDir()
	real := writeMediaFile(t, dir, "real.csv", "x")
	alias := filepath.Join(dir, "alias.csv")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	fake := &mediaFake{}
	g, _ := mediaGateway(t, fake, dir)
	reply := "Here.\nMEDIA:" + real + "\nMEDIA:" + alias + "\n"
	_, report := g.deliverMedia(context.Background(), 77, reply)
	calls, _ := fake.snapshot()
	if len(calls) != 1 {
		t.Fatalf("server saw %d file calls, want 1 (same file twice)", len(calls))
	}
	if n := strings.Count(report, "📎 Sent"); n != 1 {
		t.Errorf("report = %q, want exactly one 📎 Sent line", report)
	}
}

// TestPollStreamMediaDelivery pins the hook itself: a turn whose
// reply carries a MEDIA: tag sends the file and delivers one text
// message with the prose, the delivery report, and no MEDIA: line.
// (Poll is a thin wrapper over PollStream, so this covers it too.)
func TestPollStreamMediaDelivery(t *testing.T) {
	dir := t.TempDir()
	csv := writeMediaFile(t, dir, "sales.csv", "a,b\n1,2\n")
	fake := &mediaFake{updates: []Update{
		{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 77}, Text: "send it", From: &TGUser{ID: 77}}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	mc := config.MediaDeliveryConfig{Enabled: true, AllowDirs: []string{dir}, MaxFiles: 5, MaxBytes: 50 << 20}
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithMediaDelivery(mc))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	handler := func(ctx context.Context, chatID int64, text string, images []model.ImageContent, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		return "Here is the file.\nMEDIA:" + csv + "\n", nil, nil
	}
	cancel, errCh := runPollStream(t, g, handler)
	defer stopPollStream(t, cancel, errCh)

	waitFor(t, "media reply", func() bool {
		_, texts := fake.snapshot()
		return len(texts) >= 1
	})
	calls, texts := fake.snapshot()
	if len(calls) != 1 || calls[0].method != "sendDocument" || calls[0].filename != "sales.csv" {
		t.Fatalf("file calls = %+v, want one sendDocument sales.csv", calls)
	}
	if len(texts) != 1 {
		t.Fatalf("text messages = %d, want 1", len(texts))
	}
	if !strings.Contains(texts[0], "Here is the file.") {
		t.Errorf("reply = %q, want the prose", texts[0])
	}
	if strings.Contains(texts[0], "MEDIA:") {
		t.Errorf("reply = %q, want no MEDIA: line", texts[0])
	}
	if !strings.Contains(texts[0], "📎 Sent sales.csv (8 B)") {
		t.Errorf("reply = %q, want the 📎 Sent line", texts[0])
	}
}
