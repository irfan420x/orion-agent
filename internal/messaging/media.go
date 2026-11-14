package messaging

// Telegram file delivery (MEDIA: tags).
//
// When the agent's reply contains a MEDIA:<path> line, the gateway
// uploads the file to the chat and strips the tag from the visible
// reply. Only explicit MEDIA: tags are extracted — bare paths
// mentioned in prose are never sent. Every extracted tag is either
// delivered or replaced by a visible refusal/failure note: no MEDIA:
// line ever reaches the chat.
//
// The hook is deliverMedia, called from PollStream's handleMessage
// on the model reply BEFORE the footer is appended (see telegram.go).
// Media delivery is not a tool: it has no Register/Classify/gate
// entry and sends nothing the owner did not already have the agent
// do — the risk is bounded by the denylist + allowlist + refusal
// in validateMediaPath.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/irfan/orion/internal/config"
)

// mediaRoute maps a lower-cased extension (with the dot) to the Bot
// API method. It is the ONLY place that decides how a file is sent;
// the upload functions take the method as a parameter and never
// re-derive it. Unknown extensions are NOT in this table — they fall
// through to sendDocument in routeMedia.
var mediaRoute = map[string]string{
	".png": "sendPhoto", ".jpg": "sendPhoto", ".jpeg": "sendPhoto", ".gif": "sendPhoto", ".webp": "sendPhoto",
	".mp4": "sendVideo", ".mov": "sendVideo", ".mkv": "sendVideo", ".webm": "sendVideo", ".avi": "sendVideo",
	".ogg": "sendVoice", ".opus": "sendVoice",
	".mp3": "sendAudio", ".m4a": "sendAudio", ".wav": "sendAudio", ".flac": "sendAudio",
}

// routeMedia returns the Bot API method for a file extension. The
// lookup is case-insensitive; any extension not in mediaRoute —
// including .py, .log, a made-up .zzz, or no extension at all —
// falls through to sendDocument. There is no unroutable extension:
// this is the fix for the Hermes bug where an unknown extension
// left the raw MEDIA: tag visible in the chat and the file unsent.
func routeMedia(ext string) string {
	if m, ok := mediaRoute[strings.ToLower(ext)]; ok {
		return m
	}
	return "sendDocument"
}

// mediaFieldName returns the multipart file-field name for a Bot API
// method: sendDocument -> document, sendPhoto -> photo, and so on.
func mediaFieldName(method string) string {
	return strings.ToLower(strings.TrimPrefix(method, "send"))
}

// mediaTagRe matches one MEDIA: tag line. Line-anchored, one tag per
// line: a MEDIA: in the middle of a sentence is not a tag. The path
// is a single whitespace-free token; paths with spaces are not
// supported in v1 (the system prompt says so), and a spaced path
// matches only its first token — validation then refuses the
// (nonexistent) truncated path with a visible note, loud not silent.
var mediaTagRe = regexp.MustCompile(`^[ \t]*MEDIA:[ \t]*(\S+)[ \t]*$`)

// mediaRequest is one extracted MEDIA: tag: the path exactly as
// written in the reply, unexpanded (expansion happens in validation,
// so the refusal note can name the path the model wrote).
type mediaRequest struct {
	path string
}

// extractMediaTags splits reply into the cleaned text (every tag
// line removed, everything else byte-identical), the first maxFiles
// tag requests in order, and the paths of any tags beyond maxFiles
// (stripped from the text too, and reported as refused by the
// caller — the user must never see a raw MEDIA: line). A tag with
// no path, or a MEDIA: mid-sentence, is not a tag and stays as text.
// maxFiles <= 0 means the default.
func extractMediaTags(reply string, maxFiles int) (cleaned string, reqs []mediaRequest, excess []string) {
	if maxFiles <= 0 {
		maxFiles = config.DefaultMediaMaxFiles
	}
	lines := strings.Split(reply, "\n")
	kept := make([]string, 0, len(lines))
	// Identical raw tags dedupe here (the model sometimes repeats a
	// tag); a repeated tag is dropped silently — the file is still
	// sent once and reported once. Different spellings of one file
	// (a symlink alias, /tmp/./x.csv) dedupe later in deliverMedia
	// on the resolved path.
	seenRaw := make(map[string]bool)
	for _, line := range lines {
		m := mediaTagRe.FindStringSubmatch(line)
		if m == nil {
			kept = append(kept, line)
			continue
		}
		if seenRaw[m[1]] {
			continue
		}
		seenRaw[m[1]] = true
		if len(reqs) < maxFiles {
			reqs = append(reqs, mediaRequest{path: m[1]})
		} else {
			excess = append(excess, m[1])
		}
	}
	return strings.Join(kept, "\n"), reqs, excess
}

// expandMediaPath expands a leading ~/ (or a bare ~) to the daemon
// user's home directory. No environment-variable expansion ($HOME,
// ${X}): that is an injection surface.
func expandMediaPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

// mediaWithin reports whether path is dir itself or inside it. It
// is the same check as within() in internal/tools/files.go,
// duplicated because that helper is unexported and messaging must
// not import tools.
func mediaWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

// mediaDenylistDirs returns the directories no media file may ever
// come from, checked on the RESOLVED path: the agent's own state
// dir, the SSH and GPG key dirs, and /etc. It mirrors
// internal/tools sensitiveDirs() (files.go) — which denies ~/.orion,
// ~/.ssh and ~/.gnupg for the file tools — plus /etc. If
// sensitiveDirs() ever gains an entry, this list must gain it too
// (pinned by TestMediaDenylistSupersetOfSensitiveDirs). The
// denylist is absolute: no override, no approval can lift it, not
// even the owner.
func mediaDenylistDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return []string{"/etc"}
	}
	return []string{
		filepath.Join(home, ".orion"),
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".gnupg"),
		"/etc",
	}
}

// mediaBasenameDenied reports whether one path component matches a
// denied basename pattern: credential files (*.env, *.env.*),
// private keys/certificates (*.pem, *.key), private keys that live
// outside ~/.ssh (id_*), and anything with "credentials" in the
// name (which catches .git-credentials even if it is ever moved
// out of ~/.orion).
func mediaBasenameDenied(component string) bool {
	lower := strings.ToLower(component)
	switch {
	case strings.HasSuffix(lower, ".env"):
		return true
	case strings.Contains(lower, ".env."):
		return true
	case strings.HasSuffix(lower, ".pem"), strings.HasSuffix(lower, ".key"):
		return true
	case strings.HasPrefix(lower, "id_"):
		return true
	case strings.Contains(lower, "credentials"):
		return true
	}
	return false
}

// mediaPathDenied reports whether the resolved path is denylisted:
// under a denylist dir, or with any path component matching a
// denied basename pattern. Checked only on the resolved path — a
// symlink inside an allowed dir that points at a denied file is
// caught here because validation resolves symlinks first.
func mediaPathDenied(resolved string) bool {
	for _, d := range mediaDenylistDirs() {
		if mediaWithin(resolved, d) {
			return true
		}
	}
	for _, comp := range strings.Split(resolved, string(filepath.Separator)) {
		if mediaBasenameDenied(comp) {
			return true
		}
	}
	return false
}

// displayMediaDir renders an allowlist dir for the refusal note:
// a dir under the user's home shows with a ~ prefix, so the
// defaults read "(~/orion-media, /tmp)".
func displayMediaDir(dir string) string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if dir == home {
			return "~"
		}
		if strings.HasPrefix(dir, home+string(filepath.Separator)) {
			return "~" + dir[len(home):]
		}
	}
	return dir
}

// mediaAllowlistReason is the refusal reason for a file outside the
// allowlist: it names the file's allowed homes and how to allow
// another directory.
func mediaAllowlistReason(allowDirs []string) string {
	names := make([]string, 0, len(allowDirs))
	for _, d := range allowDirs {
		names = append(names, displayMediaDir(d))
	}
	return fmt.Sprintf("outside the media allowlist (%s). Add the directory to media_allow_dirs in ~/.orion/config.json, or move the file into one of those directories and ask again", strings.Join(names, ", "))
}

// resolveMediaDir resolves one allowlist dir: ~ expansion, then
// EvalSymlinks where the dir exists, else the cleaned absolute
// path. A dir that does not exist yet is still a valid allowlist
// entry — files under it become sendable once it exists.
func resolveMediaDir(dir string) string {
	if expanded, err := expandMediaPath(dir); err == nil {
		dir = expanded
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// resolveMediaAllowDirs resolves the allowlist dirs for a gateway.
// An explicitly set list REPLACES the defaults. The defaults are
// ~/orion-media (the dedicated media dir next to the data dir) and
// os.TempDir(). Deliberately NOT the daemon cwd: on the laptop the
// unit runs with WorkingDirectory=%h, so the cwd is the whole home
// and would make the allowlist vacuous (design Appendix C).
func resolveMediaAllowDirs(dirs []string) []string {
	if len(dirs) > 0 {
		out := make([]string, 0, len(dirs))
		for _, d := range dirs {
			if strings.TrimSpace(d) == "" {
				continue
			}
			out = append(out, resolveMediaDir(d))
		}
		if len(out) > 0 {
			return out
		}
	}
	var out []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out, resolveMediaDir(filepath.Join(home, "orion-media")))
	}
	out = append(out, resolveMediaDir(os.TempDir()))
	return out
}

// validateMediaPath checks one tag path against the media policy and
// returns the resolved absolute path to upload and its size, or ""
// plus a user-facing refusal reason. Order matters (design §4):
//
//  1. expand ~, require an absolute path
//  2. resolve symlinks on the FULL path (the file must exist;
//     a missing file is refused as "not found")
//  3. regular-file check on the resolved path (no dirs, devices,
//     sockets or FIFOs)
//  4. denylist on the resolved path (absolute, non-overridable)
//  5. size against maxBytes (clamped to the 50 MiB Bot API limit)
//  6. allowlist: the resolved path under a resolved allowlist dir
//
// The first failing step wins and its reason is the one shown to
// the user. Symlink resolution before the denylist is the whole
// point: a symlink inside an allowed dir that points at
// ~/.orion/config.json must be caught by the denylist.
func validateMediaPath(path string, allowDirs []string, maxBytes int64) (resolved string, size int64, reason string) {
	if strings.TrimSpace(path) == "" {
		return "", 0, "empty path"
	}
	expanded, err := expandMediaPath(path)
	if err != nil {
		return "", 0, "cannot determine home directory"
	}
	if !filepath.IsAbs(expanded) {
		return "", 0, "path must be absolute"
	}
	resolvedPath, err := filepath.EvalSymlinks(filepath.Clean(expanded))
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, "not found"
		}
		return "", 0, "cannot resolve path"
	}
	info, err := os.Stat(resolvedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, "not found"
		}
		return "", 0, "cannot stat file"
	}
	if info.IsDir() {
		return "", 0, "is a directory, not a file"
	}
	if !info.Mode().IsRegular() {
		return "", 0, "is not a regular file"
	}
	if mediaPathDenied(resolvedPath) {
		return "", 0, "that path is never sendable"
	}
	if maxBytes <= 0 || maxBytes > config.DefaultMediaMaxBytes {
		maxBytes = config.DefaultMediaMaxBytes
	}
	if info.Size() > maxBytes {
		return "", 0, "file is too large (" + formatMediaSize(info.Size()) + "; max " + formatMediaSize(maxBytes) + ")"
	}
	for _, dir := range allowDirs {
		if mediaWithin(resolvedPath, dir) {
			return resolvedPath, info.Size(), ""
		}
	}
	return "", 0, mediaAllowlistReason(allowDirs)
}

// formatMediaSize renders a byte count the way the delivery report
// shows it: "512 B", "12 KB", "50 MB" (whole units, rounded).
func formatMediaSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%d KB", (n+512)>>10)
	default:
		return fmt.Sprintf("%d MB", (n+(1<<19))>>20)
	}
}

// WithMediaDelivery installs the media-delivery config on the
// gateway. A zero MediaDeliveryConfig (or no option at all)
// disables extraction entirely: MEDIA: tags stay as visible text.
// Otherwise the config is resolved (limits clamped, defaults
// filled) and the allowlist dirs are resolved once, at
// construction, so a symlinked allowlist dir cannot be used to
// smuggle a path past the check.
func WithMediaDelivery(mc config.MediaDeliveryConfig) GatewayOption {
	return func(g *TelegramGateway) {
		mc = mc.Resolve()
		g.mediaCfg = mc
		g.mediaAllow = resolveMediaAllowDirs(mc.AllowDirs)
	}
}

// shortUploadError renders an upload error for the user-visible
// failure note: the Telegram description when the server gave one,
// otherwise the (already redacted) error with its call-site prefix
// stripped, one line, capped. It never contains the bot token —
// callMultipart wraps every token-bearing error with g.redact.
func shortUploadError(err error) string {
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.Description != "" {
		return apiErr.Description
	}
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const prefix = "messaging: telegram "
	if strings.HasPrefix(s, prefix) {
		rest := s[len(prefix):]
		if i := strings.Index(rest, ": "); i >= 0 {
			s = rest[i+2:]
		}
	}
	s = strings.TrimPrefix(s, "request: ")
	r := []rune(s)
	if len(r) > 200 {
		s = string(r[:200]) + "..."
	}
	return s
}

// deliverMedia extracts MEDIA: tags from the agent's reply, uploads
// each deliverable file, and returns (cleanedReply, report).
// cleanedReply is the reply with every MEDIA: line removed. report
// is "" when there were no tags, otherwise one line per file:
// "📎 Sent <name> (<size>)" or "⛔ Could not send <path>: <reason>".
// A failure to upload one file never blocks the others or the text
// reply. When extraction is disabled (no option, or Enabled=false)
// the reply is returned untouched and no file is sent.
//
// Invariant: every extracted tag is either delivered or replaced by
// a visible refusal/failure note. No MEDIA: line ever reaches the
// chat.
func (g *TelegramGateway) deliverMedia(ctx context.Context, chatID int64, reply string) (string, string) {
	if g == nil || !g.mediaCfg.Enabled {
		return reply, ""
	}
	cleaned, reqs, excess := extractMediaTags(reply, g.mediaCfg.MaxFiles)
	if len(reqs) == 0 && len(excess) == 0 {
		return reply, ""
	}
	var lines []string
	// Dedupe by resolved path, first occurrence wins: the model
	// sometimes repeats a tag, and double-sending the same file is
	// pure noise. A refused tag dedupes on its cleaned absolute
	// path (its resolved path is unknown).
	seen := make(map[string]bool)
	for _, req := range reqs {
		key := req.path
		if expanded, err := expandMediaPath(req.path); err == nil {
			key = filepath.Clean(expanded)
		}
		resolved, size, reason := validateMediaPath(req.path, g.mediaAllow, g.mediaCfg.MaxBytes)
		if resolved != "" {
			key = resolved
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if reason != "" {
			lines = append(lines, "⛔ Could not send "+req.path+": "+reason)
			g.logf("telegram media: refused %s: %s", req.path, reason)
			continue
		}
		method := routeMedia(filepath.Ext(resolved))
		if err := g.uploadMedia(ctx, chatID, method, resolved); err != nil {
			lines = append(lines, "⛔ Could not send "+filepath.Base(resolved)+": "+shortUploadError(err))
			g.logf("telegram media: failed to send %s via %s: %v", filepath.Base(resolved), method, err)
			continue
		}
		lines = append(lines, "📎 Sent "+filepath.Base(resolved)+" ("+formatMediaSize(size)+")")
		g.logf("telegram media: sent %s via %s (%d bytes)", filepath.Base(resolved), method, size)
	}
	for _, path := range excess {
		if seen[path] {
			continue
		}
		seen[path] = true
		lines = append(lines, "⛔ Could not send "+path+": too many files in one reply — max "+strconv.Itoa(g.mediaCfg.MaxFiles))
		g.logf("telegram media: refused %s: too many files in one reply", path)
	}
	if len(lines) == 0 {
		return cleaned, ""
	}
	return cleaned, strings.Join(lines, "\n")
}

// uploadMedia sends one validated file with the Bot API method the
// routing table chose. resolvedPath is the symlink-resolved path
// validation returned — the upload opens it, never the tag path.
func (g *TelegramGateway) uploadMedia(ctx context.Context, chatID int64, method, resolvedPath string) error {
	switch method {
	case "sendPhoto":
		return g.SendPhoto(ctx, chatID, resolvedPath)
	case "sendVideo":
		return g.SendVideo(ctx, chatID, resolvedPath)
	case "sendAudio":
		return g.SendAudio(ctx, chatID, resolvedPath)
	case "sendVoice":
		return g.SendVoice(ctx, chatID, resolvedPath)
	default:
		return g.SendDocument(ctx, chatID, resolvedPath)
	}
}

// SendDocument uploads filePath to chatID as a Telegram document.
// It is the default for unknown extensions (see routeMedia).
func (g *TelegramGateway) SendDocument(ctx context.Context, chatID int64, filePath string) error {
	_, err := g.callMultipart(ctx, "sendDocument", "document", filePath, chatID)
	return err
}

// SendPhoto uploads filePath to chatID as a Telegram photo.
func (g *TelegramGateway) SendPhoto(ctx context.Context, chatID int64, filePath string) error {
	_, err := g.callMultipart(ctx, "sendPhoto", "photo", filePath, chatID)
	return err
}

// SendVideo uploads filePath to chatID as a Telegram video.
func (g *TelegramGateway) SendVideo(ctx context.Context, chatID int64, filePath string) error {
	_, err := g.callMultipart(ctx, "sendVideo", "video", filePath, chatID)
	return err
}

// SendAudio uploads filePath to chatID as a Telegram audio file.
func (g *TelegramGateway) SendAudio(ctx context.Context, chatID int64, filePath string) error {
	_, err := g.callMultipart(ctx, "sendAudio", "audio", filePath, chatID)
	return err
}

// SendVoice uploads filePath to chatID as a Telegram voice message.
// Telegram requires OGG encoded with Opus for voice messages; the
// router only sends .ogg/.opus here.
func (g *TelegramGateway) SendVoice(ctx context.Context, chatID int64, filePath string) error {
	_, err := g.callMultipart(ctx, "sendVoice", "voice", filePath, chatID)
	return err
}

// mediaUploadTimeout bounds one file upload: a 50 MB upload over a
// slow link can take minutes, far past g.client's 35 s timeout, so
// the media client carries no Timeout and the context is the only
// deadline. The turn context's cancellation (daemon shutdown) still
// aborts the upload through the derived context.
const mediaUploadTimeout = 10 * time.Minute

// callMultipart POSTs a multipart/form-data body to a Bot API
// method. The file is streamed from disk through an io.Pipe into
// the multipart writer: the whole file is never held in memory,
// and the pipe bounds the in-flight chunk. It reuses the endpoint
// construction and token redaction of call() and the same
// apiResponse error handling, but it does NOT use g.client (35 s
// timeout) — it uses a dedicated client with no Timeout, built from
// g.client's Transport (falling back to http.DefaultTransport),
// so proxy behaviour is shared and the context carries the
// 10-minute per-file deadline. filePath must be the resolved path
// validation returned, never the raw tag path.
func (g *TelegramGateway) callMultipart(ctx context.Context, method, fieldName, filePath string, chatID int64) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, mediaUploadTimeout)
	defer cancel()

	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("messaging: telegram %s: open file: %w", method, err)
	}
	defer f.Close()

	// The writer goroutine streams the multipart body into the
	// pipe: chat_id field, then the file part (basename only —
	// the filename parameter must never be a full path), then the
	// closing boundary. A copy error closes the pipe with that
	// error, so the reader side unblocks instead of hanging.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	writeErr := make(chan error, 1)
	go func() {
		err := func() error {
			if err := mw.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
				return err
			}
			part, err := mw.CreateFormFile(fieldName, filepath.Base(filePath))
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, f); err != nil {
				return err
			}
			return mw.Close()
		}()
		if err != nil {
			_ = pw.CloseWithError(err)
		} else {
			_ = pw.Close()
		}
		writeErr <- err
	}()

	// The token is URL-escaped before it is interpolated into the
	// path (see call()): a malformed token containing "/" or "?"
	// must not alter the request path or query.
	endpoint := fmt.Sprintf("%s/bot%s/%s", g.baseURL, url.PathEscape(g.token), method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, pr)
	if err != nil {
		// Safe to log: g.redact returns the token-free wrapper; the
		// raw err must never be logged.
		return nil, g.redact(fmt.Errorf("messaging: telegram %s: create request: %w", method, err))
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")

	transport := http.DefaultTransport
	if g.client != nil && g.client.Transport != nil {
		transport = g.client.Transport
	}
	mediaClient := &http.Client{Transport: transport}
	resp, err := mediaClient.Do(req)
	// The writer goroutine always terminates once the request body
	// is closed (Do closes it on return, success or failure), so
	// waiting for its error cannot hang; a failed copy must
	// surface even when Do itself returned an error first.
	werr := <-writeErr
	if err != nil {
		return nil, g.redact(fmt.Errorf("messaging: telegram %s: request: %w", method, err))
	}
	defer resp.Body.Close()
	if werr != nil {
		return nil, fmt.Errorf("messaging: telegram %s: upload file: %w", method, werr)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("messaging: telegram %s: read response: %w", method, err)
	}

	var env apiResponse
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("messaging: telegram %s: decode response (HTTP %d): %w", method, resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || !env.OK {
		desc := env.Description
		if desc == "" {
			desc = strings.TrimSpace(string(data))
			if desc == "" {
				desc = http.StatusText(resp.StatusCode)
			}
		}
		// Cap the description: the raw-body fallback above can be up
		// to the 10 MiB read cap, and an uncapped body would land in
		// the returned error (and from there in stderr, logs, or a
		// reply sent back to the chat).
		apiErr := &apiError{
			Method:      method,
			StatusCode:  resp.StatusCode,
			Code:        env.ErrorCode,
			Description: truncateErrorDescription(desc),
		}
		return nil, g.redact(fmt.Errorf("messaging: telegram %s: %w", method, apiErr))
	}
	return env.Result, nil
}
