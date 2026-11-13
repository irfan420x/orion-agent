// Package messaging provides Orion's messaging channels.
//
// Telegram is the only remote messaging channel. There is deliberately no
// WhatsApp / Baileys support in this package.
//
// The Telegram gateway talks to the Bot API over plain HTTP with no
// external dependencies, so it can be tested against an httptest server
// by overriding the base URL (see WithBaseURL / SetDefaultBaseURL).
package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/security"
)

// DefaultBaseURL is the Telegram Bot API endpoint. It is read by
// NewTelegramGateway and may be changed with SetDefaultBaseURL —
// typically by tests pointing gateways at a fake server, or embedders
// that need a proxy. The value lives in an atomic.Value so a
// concurrent SetDefaultBaseURL and NewTelegramGateway can never race;
// production code should leave it alone.
var defaultBaseURL atomic.Value // string

func init() {
	defaultBaseURL.Store("https://api.telegram.org")
}

// DefaultBaseURLValue returns the current default Bot API base URL.
func DefaultBaseURLValue() string {
	if v, ok := defaultBaseURL.Load().(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimRight(strings.TrimSpace(v), "/")
	}
	return "https://api.telegram.org"
}

// SetDefaultBaseURL changes the default Bot API base URL used by
// gateways created without an explicit WithBaseURL option. It is safe
// for concurrent use with NewTelegramGateway. An empty value resets
// the default to https://api.telegram.org.
func SetDefaultBaseURL(baseURL string) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.telegram.org"
	}
	defaultBaseURL.Store(baseURL)
}

// MaxMessageLength is Telegram's per-message text limit. Longer texts are
// split into multiple messages by SendMessage.
const MaxMessageLength = 4096

// defaultHTTPTimeout bounds a single Bot API request. GetUpdates long-polls
// for 30s, so the timeout must exceed that.
const defaultHTTPTimeout = 35 * time.Second

// getUpdatesTimeout is the long-poll timeout requested from Telegram.
const getUpdatesTimeout = 30

// TGUser is a Telegram user.
type TGUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// TGChat is a Telegram chat.
type TGChat struct {
	ID int64 `json:"id"`
}

// TGMessage is a Telegram message.
type TGMessage struct {
	MessageID int64   `json:"message_id"`
	Chat      TGChat  `json:"chat"`
	Text      string  `json:"text"`
	From      *TGUser `json:"from,omitempty"`
}

// Update is a single Telegram update.
type Update struct {
	UpdateID int64      `json:"update_id"`
	Message  *TGMessage `json:"message,omitempty"`
}

// TelegramGateway is a client for the Telegram Bot API.
//
// It is safe for concurrent use: the getUpdates offset is guarded by a
// mutex. A gateway is created with NewTelegramGateway; the token is never
// logged.
//
// Sender authorisation: when an allowlist is set (WithAllowedChats),
// Poll drops messages from any other chat before the handler runs.
// Anyone who discovers the bot username could otherwise drive the OS
// agent, including its shell tool.
type TelegramGateway struct {
	token   string
	baseURL string
	client  *http.Client
	offset  int64
	mu      sync.Mutex
	// fetchMu serialises GetUpdates calls. The offset alone is not
	// enough: two concurrent GetUpdates calls could both read the
	// same offset before either advanced it, and Telegram would
	// return the same updates to both. One poller at a time also
	// matches the Bot API's own rule (409 Conflict when a second
	// getUpdates is in flight).
	fetchMu sync.Mutex
	// allowedChats is nil when no allowlist is set (all chats pass).
	allowedChats map[int64]bool
	// logger, when set via WithLogger, receives polling-health notes:
	// transient getUpdates failures and the recovery after them. It is
	// nil by default (silent). Only redacted errors are ever passed
	// to it — call() wraps every token-bearing error — so the token
	// can never appear in a log line. Fatal poll errors are NOT
	// logged here; PollStream returns them to the caller.
	logger func(format string, args ...any)
}

// Gateway is an alias for TelegramGateway.
type Gateway = TelegramGateway

// GatewayOption configures a TelegramGateway at construction time.
type GatewayOption func(*TelegramGateway)

// TelegramGatewayOption is an alias for GatewayOption.
type TelegramGatewayOption = GatewayOption

// WithBaseURL overrides the Bot API base URL (default
// https://api.telegram.org). Primarily for tests pointing at a fake
// Telegram server. A trailing slash is stripped.
func WithBaseURL(baseURL string) GatewayOption {
	return func(g *TelegramGateway) {
		if strings.TrimSpace(baseURL) != "" {
			g.baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
		}
	}
}

// WithHTTPClient overrides the HTTP client used for Bot API requests.
// If nil, the option is ignored and the default client is kept.
func WithHTTPClient(client *http.Client) GatewayOption {
	return func(g *TelegramGateway) {
		if client != nil {
			g.client = client
		}
	}
}

// WithLogger sets the polling-health logger: transient getUpdates
// failures (with the backoff about to be waited) and the recovery
// once polling succeeds again. A nil logger is ignored and the
// gateway stays silent. The logger must never receive an unredacted
// error; PollStream only passes it errors that call() already
// redacted, so the bot token can never leak into a log line.
func WithLogger(logf func(format string, args ...any)) GatewayOption {
	return func(g *TelegramGateway) {
		if logf != nil {
			g.logger = logf
		}
	}
}

// logf reports a polling-health note when a logger is set; it is a
// no-op otherwise.
func (g *TelegramGateway) logf(format string, args ...any) {
	if g == nil || g.logger == nil {
		return
	}
	g.logger(format, args...)
}

// WithAllowedChats restricts Poll to messages from the given chat IDs
// (private chats are positive, groups negative). With no IDs the option
// is a no-op; with at least one, every other chat is dropped before the
// handler runs. Use config.TelegramAllowedChats to drive this.
func WithAllowedChats(ids ...int64) GatewayOption {
	return func(g *TelegramGateway) {
		if len(ids) == 0 {
			return
		}
		if g.allowedChats == nil {
			g.allowedChats = map[int64]bool{}
		}
		for _, id := range ids {
			if id != 0 {
				g.allowedChats[id] = true
			}
		}
	}
}

// IsChatAllowed reports whether chatID may drive the agent through this
// gateway. With no allowlist set, every chat is allowed. That is the
// per-chat enforcement only: the daemon entry points refuse to start
// polling with an empty allowlist at all (see CheckTelegramAllowlist),
// so a gateway with no allowlist should only ever exist in tests or
// behind an explicit allow-all opt-in.
func (g *TelegramGateway) IsChatAllowed(chatID int64) bool {
	if len(g.allowedChats) == 0 {
		return true
	}
	return g.allowedChats[chatID]
}

// CheckTelegramAllowlist enforces the daemon startup rule for the
// Telegram allowlist: polling may start only when at least one chat
// is allowlisted, or when allowAll was explicitly opted into. An
// empty allowlist with allowAll false is an error, because with no
// allowlist any Telegram chat could drive the agent (including
// shell_exec). The error tells the operator how to find their chat
// ID and how to fix the config; every polling entry point
// (oriond, `orion telegram poll`, `orion daemon`) reports it and
// exits non-zero.
func CheckTelegramAllowlist(allowedChats []int64, allowAll bool) error {
	if len(allowedChats) > 0 || allowAll {
		return nil
	}
	return errors.New("refusing to start Telegram polling: telegram_allowed_chats is empty, so ANY Telegram chat could drive the agent (including shell_exec); set telegram_allowed_chats in config.json to your chat ID(s), or set allow_all_telegram_chats=true only if you really mean to allow every chat — to find your chat ID, start the daemon once with allow_all_telegram_chats=true and send /whoami to the bot, or check the daemon log for the dropped-chat ID logged when a non-allowlisted chat messages the bot while it is polling")
}

// NewTelegramGateway creates a TelegramGateway for the given bot token.
// An empty (or whitespace-only) token is an error.
func NewTelegramGateway(token string, opts ...GatewayOption) (*TelegramGateway, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("messaging: telegram token must not be empty")
	}
	baseURL := DefaultBaseURLValue()
	g := &TelegramGateway{
		token:   token,
		baseURL: baseURL,
		client:  &http.Client{Timeout: defaultHTTPTimeout},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(g)
		}
	}
	if g.client == nil {
		g.client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if g.baseURL == "" {
		g.baseURL = "https://api.telegram.org"
	}
	return g, nil
}

// Offset returns the next update_id GetUpdates will request. It is
// max(seen update_id) + 1, or 0 before any updates have been seen.
func (g *TelegramGateway) Offset() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.offset
}

// redactedError wraps an error whose message may contain the bot token
// (notably *url.Error, which embeds the full request URL including the
// token in the path).
//
// Safety rule: ONLY the wrapped error (the *redactedError itself) is
// safe to log or to format with %v / %+v. Its Error() returns the
// message with the token replaced by "[redacted]". Never log
// errors.Unwrap(err): the raw wrapped error still contains the token,
// and the only safe handle on it is this wrapper.
//
// errors.Is / errors.As keep working without exposing the raw error:
// Is and As forward to the wrapped error's own chain, and Unwrap
// returns a redacted view of the wrapped error's cause (recursively
// redacted), never the raw error itself. A timeout is therefore still
// detectable via errors.As(err, &net.Error) while every level of the
// chain stays token-free.
type redactedError struct {
	err   error
	token string
}

func (e *redactedError) Error() string {
	if e.err == nil {
		return ""
	}
	return strings.ReplaceAll(e.err.Error(), e.token, "[redacted]")
}

// Is forwards errors.Is to the wrapped error, so sentinel matching
// (e.g. context.DeadlineExceeded) works without unwrapping to the raw,
// token-bearing error.
func (e *redactedError) Is(target error) bool {
	if e.err == nil {
		return false
	}
	return errors.Is(e.err, target)
}

// As forwards errors.As to the wrapped error, so type matching (e.g.
// net.Error for a timed-out request) works without unwrapping to the
// raw, token-bearing error.
func (e *redactedError) As(target any) bool {
	if e.err == nil {
		return false
	}
	return errors.As(e.err, target)
}

// Unwrap returns a redacted view of the wrapped error's cause — never
// the wrapped error itself. Code that unwraps one level (fmt %w
// chains, errors.As traversal) therefore still only ever sees
// token-free messages, while the cause chain remains walkable.
func (e *redactedError) Unwrap() error {
	if e.err == nil {
		return nil
	}
	u, ok := e.err.(interface{ Unwrap() error })
	if !ok {
		return nil
	}
	inner := u.Unwrap()
	if inner == nil {
		return nil
	}
	return &redactedError{err: inner, token: e.token}
}

// redact wraps err so the token never appears in its Error() string.
// A nil err stays nil; an empty token is a no-op.
//
// Callers must log, return, and format ONLY the error redact returns.
// The raw err (and errors.Unwrap of the result) still contains the
// token and must never be logged.
func (g *TelegramGateway) redact(err error) error {
	if err == nil || g.token == "" {
		return err
	}
	return &redactedError{err: err, token: g.token}
}

// apiResponse is the envelope every Bot API call returns.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Result      json.RawMessage `json:"result"`
}

// call performs one Bot API call and returns the decoded result payload.
// method is the Bot API method name (e.g. "getMe"). query holds URL query
// parameters for GET calls; body, when non-nil, is JSON-encoded and sent
// as a POST. All errors are wrapped with the method name.
//
// Token safety: every error that can carry the token (notably the
// *url.Error from client.Do, which embeds the request URL) leaves this
// function wrapped by g.redact. Only that wrapped error is safe to log;
// the raw error underneath it still contains the token (see
// redactedError).
func (g *TelegramGateway) call(ctx context.Context, method string, query url.Values, body any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// The token is URL-escaped before it is interpolated into the path
	// (see P2-30): a malformed token containing "/" or "?" must not
	// alter the request path or query.
	endpoint := fmt.Sprintf("%s/bot%s/%s", g.baseURL, url.PathEscape(g.token), method)
	u, err := url.Parse(endpoint)
	if err != nil {
		// Safe to log: g.redact returns the token-free wrapper; the
		// raw err must never be logged.
		return nil, g.redact(fmt.Errorf("messaging: telegram %s: invalid endpoint: %w", method, err))
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}

	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("messaging: telegram %s: marshal request: %w", method, err)
		}
		reqBody = bytes.NewReader(data)
	}

	httpMethod := http.MethodGet
	if body != nil {
		httpMethod = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, httpMethod, u.String(), reqBody)
	if err != nil {
		// Safe to log: g.redact returns the token-free wrapper; the
		// raw err must never be logged.
		return nil, g.redact(fmt.Errorf("messaging: telegram %s: create request: %w", method, err))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		// Safe to log: g.redact returns the token-free wrapper. The
		// raw *url.Error embeds the request URL (and the token) and
		// must never be logged or unwrapped for logging.
		return nil, g.redact(fmt.Errorf("messaging: telegram %s: request: %w", method, err))
	}
	defer resp.Body.Close()

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
		// reply sent back to the chat). The message shape is
		// unchanged — apiError formats exactly like the old
		// fmt.Errorf calls did.
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

// apiError is a Bot API error: either a non-2xx HTTP status, a
// response with "ok": false, or both. Code is Telegram's error_code
// (0 when the response carried none); Description is Telegram's
// description, capped so a large error body can never produce a
// multi-megabyte error string (see maxErrorDescriptionRunes).
//
// Poll treats 401/403/409 as fatal (revoked token, blocked bot,
// another poller holding the connection): retrying those forever at
// the backoff cadence looks alive while being dead.
type apiError struct {
	Method      string
	StatusCode  int
	Code        int
	Description string
}

func (e *apiError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("HTTP %d, error %d: %s", e.StatusCode, e.Code, e.Description)
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Description)
}

// isFatalForPoll reports whether this error can never heal by
// retrying: an unauthorised/forbidden token or a conflicting
// getUpdates (another instance polling with the same token). The
// Telegram error_code is checked first; when the response carried
// none (e.g. a proxy error page) the HTTP status decides.
func (e *apiError) isFatalForPoll() bool {
	for _, c := range []int{e.Code, e.StatusCode} {
		if c == http.StatusUnauthorized || c == http.StatusForbidden || c == http.StatusConflict {
			return true
		}
	}
	return false
}

// maxErrorDescriptionRunes caps an error description taken from a Bot
// API failure. The raw body fallback in call can be up to the 10 MiB
// read cap; without a cap that whole body lands in the returned
// error, which callers print to stderr (orion telegram test, oriond)
// or send back to the chat (Poll's handler-error reply).
const maxErrorDescriptionRunes = 512

// truncateErrorDescription shortens s to at most
// maxErrorDescriptionRunes runes, appending "..." when it truncates.
// Rune-based so the result stays valid UTF-8.
func truncateErrorDescription(s string) string {
	runes := []rune(s)
	if len(runes) <= maxErrorDescriptionRunes {
		return s
	}
	return string(runes[:maxErrorDescriptionRunes]) + "..."
}

type getMeResult struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

// GetMe returns the bot's username, verifying the token against the Bot
// API. A non-ok response (e.g. a bad token) is returned as an error
// carrying Telegram's description.
func (g *TelegramGateway) GetMe(ctx context.Context) (string, error) {
	result, err := g.call(ctx, "getMe", nil, nil)
	if err != nil {
		return "", err
	}
	var me getMeResult
	if err := json.Unmarshal(result, &me); err != nil {
		return "", fmt.Errorf("messaging: telegram getMe: decode result: %w", err)
	}
	return me.Username, nil
}

// sendMessageRequest is the JSON body for sendMessage.
type sendMessageRequest struct {
	ChatID    int64  `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
}

// sendMessageResult is the message object sendMessage returns.
type sendMessageResult struct {
	MessageID int64 `json:"message_id"`
}

// sendMessageRaw posts one sendMessage call and returns the new
// message's id. parseMode is "" for plain text or "HTML" for the
// terminal card.
func (g *TelegramGateway) sendMessageRaw(ctx context.Context, chatID int64, text, parseMode string) (int64, error) {
	result, err := g.call(ctx, "sendMessage", nil, sendMessageRequest{ChatID: chatID, Text: text, ParseMode: parseMode})
	if err != nil {
		return 0, err
	}
	var msg sendMessageResult
	if err := json.Unmarshal(result, &msg); err != nil {
		return 0, fmt.Errorf("messaging: telegram sendMessage: decode result: %w", err)
	}
	return msg.MessageID, nil
}

// sendMessageOnce sends a single message of at most MaxMessageLength runes.
func (g *TelegramGateway) sendMessageOnce(ctx context.Context, chatID int64, text string) error {
	_, err := g.sendMessageRaw(ctx, chatID, text, "")
	return err
}

// SendMessageHTML sends one HTML message to chatID and returns the new
// message's id, so the caller can edit it in place later. Unlike
// SendMessage it does not split: card HTML is already capped at 4000
// runes by TerminalCard.Render.
func (g *TelegramGateway) SendMessageHTML(ctx context.Context, chatID int64, htmlText string) (int64, error) {
	return g.sendMessageRaw(ctx, chatID, htmlText, "HTML")
}

// SendChatAction sends a chat action (e.g. "typing") to chatID, so the
// user sees the bot working while a turn runs.
func (g *TelegramGateway) SendChatAction(ctx context.Context, chatID int64, action string) error {
	_, err := g.call(ctx, "sendChatAction", nil, struct {
		ChatID int64  `json:"chat_id"`
		Action string `json:"action"`
	}{ChatID: chatID, Action: action})
	return err
}

// EditMessageText edits messageID in chatID in place with new HTML
// text. Telegram's "message is not modified" error (the new text is
// identical to the current text) is treated as success: it must not
// fail the turn.
func (g *TelegramGateway) EditMessageText(ctx context.Context, chatID, messageID int64, htmlText string) error {
	_, err := g.call(ctx, "editMessageText", nil, struct {
		ChatID    int64  `json:"chat_id"`
		MessageID int64  `json:"message_id"`
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}{ChatID: chatID, MessageID: messageID, Text: htmlText, ParseMode: "HTML"})
	if err != nil && strings.Contains(err.Error(), "message is not modified") {
		return nil
	}
	return err
}

// BotCommand is one entry of the bot's /commands menu.
type BotCommand struct {
	Name        string `json:"command"`
	Description string `json:"description"`
}

// SetMyCommands replaces the bot's command menu (the list shown when
// the user types "/" in Telegram).
func (g *TelegramGateway) SetMyCommands(ctx context.Context, cmds []BotCommand) error {
	if cmds == nil {
		cmds = []BotCommand{}
	}
	_, err := g.call(ctx, "setMyCommands", nil, struct {
		Commands []BotCommand `json:"commands"`
	}{Commands: cmds})
	return err
}

// splitMessage splits text into chunks of at most MaxMessageLength runes,
// so multi-byte characters are never split mid-rune.
func splitMessage(text string) []string {
	runes := []rune(text)
	if len(runes) <= MaxMessageLength {
		return []string{text}
	}
	chunks := make([]string, 0, len(runes)/MaxMessageLength+1)
	for len(runes) > MaxMessageLength {
		chunks = append(chunks, string(runes[:MaxMessageLength]))
		runes = runes[MaxMessageLength:]
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}

// SendMessage sends text to chatID. Text longer than 4096 characters is
// split into multiple messages, in order. An empty text is a no-op
// returning nil. The first failing chunk aborts the send and its error
// is returned wrapped with the chunk index.
func (g *TelegramGateway) SendMessage(ctx context.Context, chatID int64, text string) error {
	if text == "" {
		return nil
	}
	for i, chunk := range splitMessage(text) {
		if err := g.sendMessageOnce(ctx, chatID, chunk); err != nil {
			return fmt.Errorf("messaging: telegram sendMessage chunk %d: %w", i, err)
		}
	}
	return nil
}

// GetUpdates long-polls for new updates (timeout=30s on the server side,
// 35s client timeout). After a successful call the internal offset is
// advanced to max(update_id) + 1, so the next call only returns newer
// updates. It honours ctx cancellation.
func (g *TelegramGateway) GetUpdates(ctx context.Context) ([]Update, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Serialise whole polls (see fetchMu): the offset read, the
	// request, and the offset advance are one atomic step, so two
	// concurrent callers can never fetch the same offset and deliver
	// the same updates twice. A second caller waits here for the
	// in-flight long poll (up to ~35s) instead of duplicating it.
	g.fetchMu.Lock()
	defer g.fetchMu.Unlock()

	// Bound the long poll even if the caller supplied a client without a
	// timeout. WithTimeout never extends a shorter caller deadline: the
	// earliest deadline wins.
	pollCtx, cancel := context.WithTimeout(ctx, defaultHTTPTimeout)
	defer cancel()

	g.mu.Lock()
	offset := g.offset
	g.mu.Unlock()

	query := url.Values{}
	query.Set("offset", strconv.FormatInt(offset, 10))
	query.Set("timeout", strconv.Itoa(getUpdatesTimeout))

	result, err := g.call(pollCtx, "getUpdates", query, nil)
	if err != nil {
		return nil, err
	}
	var updates []Update
	if err := json.Unmarshal(result, &updates); err != nil {
		return nil, fmt.Errorf("messaging: telegram getUpdates: decode result: %w", err)
	}
	if len(updates) > 0 {
		var maxID int64 = updates[0].UpdateID
		for _, u := range updates[1:] {
			if u.UpdateID > maxID {
				maxID = u.UpdateID
			}
		}
		g.mu.Lock()
		if maxID+1 > g.offset {
			g.offset = maxID + 1
		}
		g.mu.Unlock()
	}
	if updates == nil {
		updates = []Update{}
	}
	return updates, nil
}

// maxPollHandlers bounds how many update handlers run at once. One
// slow handler (an agent run can take minutes) must not stall polling
// for other chats, but unbounded goroutines would let an update flood
// exhaust memory; 8 in flight with per-chat ordering (below) is the
// balance.
const maxPollHandlers = 8

// cardEditThrottle is the minimum interval between in-place edits of
// the terminal card: Telegram rate-limits edits, so bursts of tool
// events are coalesced and only the latest render is sent.
const cardEditThrottle = time.Second

// typingResendInterval re-sends the "typing" chat action this often
// while a turn runs: Telegram shows it for only a few seconds.
const typingResendInterval = 4 * time.Second

// PollStream loops on GetUpdates until ctx is cancelled, delivering
// each incoming message with non-empty text to handler and sending the
// handler's reply back to the same chat, with a live Hermes-style
// terminal card rendered from the handler's progress events.
//
//   - Before the handler runs, a "typing" chat action is sent and
//     re-sent every 4s until the handler returns.
//   - The terminal card is created lazily on the first ToolCallStarted
//     event and edited in place on later events, throttled to at most
//     one edit per second (bursts coalesce; a final edit is always
//     done when the handler returns, if the render changed since the
//     last successful edit). A failed edit falls back to sending a
//     fresh card message and editing that one instead. A turn with no
//     tool calls sends no card at all.
//   - A handler error is reported to the user as
//     "Sorry, I hit an error: <err>" (redacted, split/capped by
//     SendMessage); polling continues. If a card exists, it gets a
//     final edit first so the card shows the failed state.
//   - When the handler returns stats != nil, the footer from
//     FormatFooter(stats) is appended to the reply; nil stats means
//     no footer. An empty reply sends nothing.
//   - A transient GetUpdates error backs off (1s, doubling to a 5s max)
//     and polling continues. A fatal one (401/403/409: revoked token,
//     blocked bot, another poller) returns an error instead.
//   - ctx cancellation returns nil cleanly.
//   - A handler panic is reported to the chat like a handler error
//     and polling continues; it never kills the daemon.
//
// Updates are handled concurrently, bounded by maxPollHandlers, with
// best-effort per-chat ordering (striped locks). When ctx is cancelled
// PollStream returns nil promptly; handlers already running are left
// to finish (their reply sends use the cancelled ctx and fail
// best-effort, as before).
func (g *TelegramGateway) PollStream(ctx context.Context, handler TurnFunc) error {
	if handler == nil {
		return errors.New("messaging: telegram PollStream: handler must not be nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	backoff := time.Second
	const maxBackoff = 5 * time.Second

	// Semaphore bounding concurrent handlers; striped locks serialise
	// handlers for the same chat (best-effort per-chat ordering).
	sem := make(chan struct{}, maxPollHandlers)
	var chatLocks [64]sync.Mutex
	chatLock := func(chatID int64) *sync.Mutex {
		return &chatLocks[uint64(chatID)%uint64(len(chatLocks))]
	}
	handle := func(upd Update) {
		defer func() { <-sem }()
		// A panic in the handler (the agent loop) must not kill the
		// daemon: report it to the chat like any other handler error
		// and keep polling. This defer is registered after the
		// semaphore release, so on a panic it runs first (LIFO) and
		// the slot is still released.
		defer func() {
			if r := recover(); r != nil {
				msg := fmt.Sprintf("Sorry, I hit an error: %v", r)
				// Redact before sending: a handler panic value
				// could carry a provider/config error with a key.
				msg = security.RedactSecrets(msg)
				// Best effort: a failed apology must not kill polling.
				_ = g.SendMessage(ctx, upd.Message.Chat.ID, msg)
			}
		}()
		lock := chatLock(upd.Message.Chat.ID)
		lock.Lock()
		defer lock.Unlock()

		chatID := upd.Message.Chat.ID

		// --- terminal card state (this turn only) ---
		var cardMu sync.Mutex
		card := &TerminalCard{}
		var cardMsgID int64 // 0 until the card is created
		var lastSent string // last rendered text successfully shown
		var lastEdit time.Time
		// syncCard renders the card and, when it changed since the
		// last successful show, pushes it to Telegram: creating the
		// card on first use, editing it in place otherwise, and
		// sending a fresh card when an edit fails. force ignores the
		// throttle (used for the final edit when the handler
		// returns). It is best-effort: every failure is swallowed
		// so a card problem never fails the turn.
		syncCard := func(force bool) {
			cardMu.Lock()
			defer cardMu.Unlock()
			rendered := card.Render()
			if rendered == lastSent {
				return
			}
			// Never create a card for a turn with no tool calls:
			// the final sync after the handler returns would
			// otherwise send a bare header (lastSent starts as ""
			// and differs from the header).
			if cardMsgID == 0 && card.IsEmpty() {
				return
			}
			if !force && !lastEdit.IsZero() && time.Since(lastEdit) < cardEditThrottle {
				return // coalesce bursts; the final edit catches up
			}
			if cardMsgID == 0 {
				id, err := g.SendMessageHTML(ctx, chatID, rendered)
				if err != nil {
					return
				}
				cardMsgID = id
			} else if err := g.EditMessageText(ctx, chatID, cardMsgID, rendered); err != nil {
				// The message may have been deleted: send a fresh
				// card and keep editing that one instead.
				id, err2 := g.SendMessageHTML(ctx, chatID, rendered)
				if err2 != nil {
					return
				}
				cardMsgID = id
			}
			lastSent = rendered
			lastEdit = time.Now()
		}
		emit := func(ev core.ProgressEvent) {
			cardMu.Lock()
			card.Add(ev)
			cardMu.Unlock()
			// Only tool-call events can change the card; other kinds
			// (step/turn) are ignored by TerminalCard.Add anyway.
			if ev.Kind == core.EventToolCallStarted || ev.Kind == core.EventToolCallFinished {
				syncCard(false)
			}
		}

		// "typing" while the turn runs, re-sent every 4s: Telegram
		// shows a chat action for only a few seconds.
		typingDone := make(chan struct{})
		go func() {
			ticker := time.NewTicker(typingResendInterval)
			defer ticker.Stop()
			for {
				select {
				case <-typingDone:
					return
				case <-ticker.C:
					_ = g.SendChatAction(ctx, chatID, "typing")
				}
			}
		}()
		_ = g.SendChatAction(ctx, chatID, "typing")

		reply, stats, herr := handler(ctx, chatID, upd.Message.Text, emit)
		close(typingDone)

		// Final card edit: show the finished state (including any
		// failed tool call) even if the last event was inside the
		// throttle window. syncCard is a no-op when the card was
		// never created or nothing changed.
		syncCard(true)

		if herr != nil {
			msg := fmt.Sprintf("Sorry, I hit an error: %v", herr)
			// Redact before sending: a raw provider/config error
			// could carry an API key or token.
			msg = security.RedactSecrets(msg)
			// Best effort: a failed apology must not kill polling.
			_ = g.SendMessage(ctx, chatID, msg)
			return
		}
		if reply == "" {
			return
		}
		if stats != nil {
			reply += "\n\n" + FormatFooter(stats)
		}
		// Best effort: a failed reply must not kill polling.
		_ = g.SendMessage(ctx, chatID, reply)
	}

	// consecutiveFailures counts back-to-back transient getUpdates
	// failures, so the recovery log can say how long polling was
	// down. It is only ever read by this loop.
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		updates, err := g.GetUpdates(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// A revoked token, a blocked bot, or a second poller
			// holding the connection (401/403/409) can never heal
			// by retrying: fail loudly so the daemon exits (and
			// systemd / the operator notices) instead of looking
			// alive while polling a dead token every few seconds.
			var apiErr *apiError
			if errors.As(err, &apiErr) && apiErr.isFatalForPoll() {
				return fmt.Errorf("messaging: telegram poll: unrecoverable Bot API error: %w", apiErr)
			}
			// Transient failure: log it (when a logger is set) and
			// back off. Without this line a network outage is
			// completely silent in the daemon log — the process
			// looks alive while polling is down. The error is
			// safe to log: call() redacts the token.
			consecutiveFailures++
			g.logf("telegram poll: transient getUpdates error (attempt %d, retry in %s): %v", consecutiveFailures, backoff, err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		if consecutiveFailures > 0 {
			g.logf("telegram poll: recovered after %d failed getUpdates attempt(s)", consecutiveFailures)
			consecutiveFailures = 0
		}
		backoff = time.Second

		for _, upd := range updates {
			if upd.Message == nil || strings.TrimSpace(upd.Message.Text) == "" {
				continue
			}
			// Sender authorisation: drop messages from chats outside
			// the allowlist before the handler (and the agent, and
			// its tools) ever sees them. The drop is logged (when a
			// logger is set) with the chat ID, so an operator who
			// has not allowlisted themselves yet can find their
			// chat ID in the daemon log and add it to
			// telegram_allowed_chats. The text is never logged.
			if !g.IsChatAllowed(upd.Message.Chat.ID) {
				g.logf("telegram poll: dropped message from non-allowlisted chat %d (add it to telegram_allowed_chats to allow it)", upd.Message.Chat.ID)
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			// Acquire a handler slot (or stop if cancelled) before
			// dispatching, so the number of in-flight handlers is
			// bounded even under an update flood.
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return nil
			}
			go handle(upd)
		}
	}
}

// Poll loops on GetUpdates until ctx is cancelled, delivering each
// incoming message with non-empty text to handler and sending the
// handler's reply back to the same chat. It is a thin wrapper over
// PollStream: the handler receives no progress events and no footer
// is appended (stats nil). All PollStream semantics apply — backoff
// on transient GetUpdates errors, fatal 401/403/409, allowlist,
// per-chat ordering, panic recovery, and nil on ctx cancellation.
func (g *TelegramGateway) Poll(ctx context.Context, handler func(ctx context.Context, chatID int64, text string) (string, error)) error {
	if handler == nil {
		return errors.New("messaging: telegram Poll: handler must not be nil")
	}
	return g.PollStream(ctx, func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		reply, err := handler(ctx, chatID, text)
		return reply, nil, err
	})
}
