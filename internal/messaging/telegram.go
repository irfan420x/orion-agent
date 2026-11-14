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
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/permissions"
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

// allowedUpdates is the update-type filter Orion subscribes to
// (F-A16-2). Telegram persists allowed_updates server-side per bot:
// the rc8 incident was a stale persisted filter of ["message"],
// which meant Telegram never delivered callback_query and every
// approval button silently died. Sending this filter on the first
// successful getUpdates resets any such stale filter, so the bot
// both receives taps and keeps receiving them. Only "message" and
// "callback_query" are needed — Orion handles nothing else.
var allowedUpdates = []string{"message", "callback_query"}

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

// TGCallbackQuery is a Telegram callback query: a tap on an inline
// keyboard button. Message is the message the button belongs to
// (nil for buttons on messages the bot can no longer see); Data is
// the button's callback_data, which the bot defined when it sent
// the keyboard.
type TGCallbackQuery struct {
	ID      string     `json:"id"`
	From    *TGUser    `json:"from"`
	Message *TGMessage `json:"message,omitempty"`
	Data    string     `json:"data"`
}

// Update is a single Telegram update.
type Update struct {
	UpdateID      int64            `json:"update_id"`
	Message       *TGMessage       `json:"message,omitempty"`
	CallbackQuery *TGCallbackQuery `json:"callback_query,omitempty"`
}

// InlineKeyboardButton is one button in an inline keyboard. Only
// Text and CallbackData are used: tapping the button delivers a
// callback query carrying CallbackData.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// InlineKeyboardMarkup is Telegram's reply_markup for inline
// keyboards: rows of buttons.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// CallbackHandler handles a callback query (an inline-keyboard tap).
// It is invoked by PollStream outside any per-chat lock — a tap
// must be able to resolve a pending approval while the turn that
// requested it is still blocked waiting for the answer, and the
// per-chat lock is held by that turn. A handler error is logged, it
// never kills polling.
type CallbackHandler func(ctx context.Context, q TGCallbackQuery) error

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
//
// The allowlist is ALSO the sender allowlist (P1-O4): a message is
// accepted only when its SENDER's user ID is allowlisted, not merely
// when its chat is. In a private chat the chat ID equals the user
// ID, so existing single-user configs behave exactly as before. In
// a group chat, only allowlisted members may drive the bot — other
// members' messages are dropped even though the group itself is
// allowlisted. A message with no sender (channel posts, anonymous
// admins) is dropped: there is nobody to authorise. See
// IsSenderAllowed; IsChatAllowed is unchanged (another worker owns
// it) and still runs first.
type TelegramGateway struct {
	token   string
	baseURL string
	client  *http.Client
	offset  int64
	mu      sync.Mutex
	// updatesFilterSent records whether a getUpdates call has
	// already carried allowedUpdates (F-A16-2). It is set only
	// after a fetch SUCCEEDS: a failed fetch may never have
	// reached Telegram, so the next fetch retries the filter
	// instead of assuming it landed. It is read and written only
	// inside fetchUpdatesLocked, whose callers all hold fetchMu,
	// so no separate lock is needed. One gateway sends the filter
	// once per lifetime — a gateway is built once per daemon
	// start, so that is once per startup.
	updatesFilterSent bool
	// fetchMu serialises getUpdates fetches. The offset alone is not
	// enough: two concurrent fetches could both read the same
	// offset before either advanced it, and Telegram would return
	// the same updates to both. One poller at a time also matches
	// the Bot API's own rule (409 Conflict when a second
	// getUpdates is in flight). GetUpdates holds it across the
	// fetch and the offset advance (one atomic step); PollStream
	// holds it only for the fetch and confirms per dispatched
	// update afterwards (F-A7-P2-3) — production runs a single
	// poller, so no second fetcher can observe the un-advanced
	// offset mid-batch.
	fetchMu sync.Mutex
	// allowedChats is nil when no allowlist is set (all chats pass).
	allowedChats map[int64]bool
	// allowAll is set by WithAllowAll when the operator explicitly
	// opted into allowing every chat (P2-O5). With no allowlist the
	// opt-in allows all chats — but only inside allowAllWindow from
	// allowAllSince (daemon start); IsChatAllowed is the only
	// enforcement point. allowAllExpiredLogged makes the expiry log
	// line fire once, not on every dropped message.
	allowAll              bool
	allowAllSince         time.Time
	allowAllWindow        time.Duration
	allowAllExpiredLogged bool
	// callbackHandler, when set via SetCallbackHandler, receives
	// callback-query updates (inline-keyboard taps). It is invoked
	// outside any per-chat lock (see CallbackHandler).
	callbackHandler CallbackHandler
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

// logReceivedUpdate records one update GetUpdates returned: its id,
// its kind, and the sender/chat ids. Message text and callback data
// are NEVER logged (the text is user/model content; the data holds a
// pending-request id). The line exists so a lost tap is
// diagnosable: if getUpdates returned the callback query, this line
// is in the log and the loss is downstream of the poll; if it is
// absent, the update never reached this poller at all (another
// getUpdates consumer confirmed it server-side, or Telegram never
// delivered it) — no code downstream can fix that, and guessing
// otherwise wastes the next incident.
func (g *TelegramGateway) logReceivedUpdate(upd Update) {
	switch {
	case upd.CallbackQuery != nil:
		from := int64(0)
		if upd.CallbackQuery.From != nil {
			from = upd.CallbackQuery.From.ID
		}
		g.logf("telegram poll: received update %d callback_query id=%s from=%d", upd.UpdateID, upd.CallbackQuery.ID, from)
	case upd.Message != nil:
		from := int64(0)
		if upd.Message.From != nil {
			from = upd.Message.From.ID
		}
		g.logf("telegram poll: received update %d message chat=%d from=%d", upd.UpdateID, upd.Message.Chat.ID, from)
	default:
		g.logf("telegram poll: received update %d (other type, skipped)", upd.UpdateID)
	}
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

// DefaultAllowAllWindow is the allow-all opt-in window when the
// caller passes no (or a non-positive) window to WithAllowAll: 10
// minutes from gateway creation. It mirrors
// config.DefaultAllowAllTelegramWindowMinutes; keep the two in sync.
const DefaultAllowAllWindow = 10 * time.Minute

// WithAllowAll marks the gateway as running under the explicit
// allow-all opt-in (P2-O5): with no allowlist, IsChatAllowed allows
// every chat — but only for window from gateway creation (daemon
// start). Once the window elapses IsChatAllowed refuses every chat
// until the daemon restarts with a real allowlist. A non-positive
// window means DefaultAllowAllWindow. When an allowlist is also set
// (WithAllowedChats), the allowlist wins: only listed chats pass,
// and the window is irrelevant. The window is measured from
// creation, not from first use, so an idle daemon still closes.
func WithAllowAll(window time.Duration) GatewayOption {
	return func(g *TelegramGateway) {
		if window <= 0 {
			window = DefaultAllowAllWindow
		}
		g.allowAll = true
		g.allowAllSince = time.Now()
		g.allowAllWindow = window
	}
}

// IsChatAllowed reports whether chatID may drive the agent through this
// gateway. With no allowlist set, every chat is allowed. That is the
// per-chat enforcement only: the daemon entry points refuse to start
// polling with an empty allowlist at all (see CheckTelegramAllowlist),
// so a gateway with no allowlist should only ever exist in tests or
// behind an explicit allow-all opt-in.
//
// TIME-BOX (P2-O5): the allow-all opt-in (WithAllowAll) is enforced
// here and nowhere else. Inside the window every chat passes; once
// it elapses every chat is refused, including chats that were
// allowed a moment earlier. The expiry is logged once (via the
// gateway logger, when set) so the daemon log shows why the bot
// went silent instead of failing silently.
func (g *TelegramGateway) IsChatAllowed(chatID int64) bool {
	if len(g.allowedChats) > 0 {
		return g.allowedChats[chatID]
	}
	if !g.allowAll {
		return true
	}
	if g.allowAllSince.IsZero() {
		// Hand-built gateway with no start time recorded: there is
		// no window to measure, so nothing to expire. Gateways
		// built by NewTelegramGateway always set the start time
		// through WithAllowAll.
		return true
	}
	window := g.allowAllWindow
	if window <= 0 {
		window = DefaultAllowAllWindow
	}
	if time.Since(g.allowAllSince) < window {
		return true
	}
	g.mu.Lock()
	logged := g.allowAllExpiredLogged
	g.allowAllExpiredLogged = true
	g.mu.Unlock()
	if !logged {
		g.logf("telegram allow-all opt-in expired after %s: refusing all chats until restart with telegram_allowed_chats set", window)
	}
	return false
}

// IsSenderAllowed reports whether the Telegram USER who sent a
// message may drive the agent (P1-O4). The check is on the sender,
// not the chat: the allowlist holds user IDs (in a private chat the
// user ID equals the chat ID, so existing configs are unaffected),
// and a group chat's allowlisting must never authorise every member
// of the group. A nil sender (channel posts, anonymous admins) is
// never allowed — there is nobody to authorise.
//
// When no allowlist is set, IsChatAllowed's own rule decides: a
// gateway with no allowlist and no allow-all opt-in allows
// everyone (tests and the explicit opt-in path); with the allow-all
// opt-in, the window still applies. A nil sender is refused even
// then.
func (g *TelegramGateway) IsSenderAllowed(from *TGUser) bool {
	if from == nil || from.ID == 0 {
		return false
	}
	if len(g.allowedChats) > 0 {
		return g.allowedChats[from.ID]
	}
	// No allowlist: mirror the chat decision (allow-all window
	// included) so the two checks can never disagree about an
	// open gateway.
	return g.IsChatAllowed(from.ID)
}

// OwnerIDFromAllowlist derives the owner user ID from the Telegram
// allowlist: the FIRST entry is the owner. The owner is the
// operator who installed the bot; their private chat ID is their
// user ID, and the allowlist is ordered with the owner first by
// convention (the daemon logs the derivation at startup so a
// mis-ordered list is visible). There is deliberately no config
// field for the owner: the allowlist already names them, and a
// second source of truth would drift. An empty allowlist has no
// owner (0): daemon-wide operations are then refused for everyone
// until an allowlist exists.
func OwnerIDFromAllowlist(allowedChats []int64) int64 {
	for _, id := range allowedChats {
		if id != 0 {
			return id
		}
	}
	return 0
}

// SetCallbackHandler installs the handler for callback-query
// updates (inline-keyboard taps). A nil handler removes it (taps
// are then answered with "not handled" and dropped). Safe for
// concurrent use with PollStream.
func (g *TelegramGateway) SetCallbackHandler(h CallbackHandler) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.callbackHandler = h
}

// callbackHandlerFor returns the installed callback handler, or nil.
func (g *TelegramGateway) callbackHandlerFor() CallbackHandler {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.callbackHandler
}

// senderIDKey is the context key for the Telegram sender user ID of
// the update being handled.
type senderIDKey struct{}

// WithSenderID returns ctx carrying the Telegram sender user ID
// for the update being handled. PollStream sets it before invoking
// the turn handler, so the turn (and the permission gate, via the
// approval context) knows who asked, not just which chat.
func WithSenderID(ctx context.Context, senderID int64) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, senderIDKey{}, senderID)
}

// SenderIDFromContext reads the sender user ID carried by ctx. ok
// is false when ctx carries none.
func SenderIDFromContext(ctx context.Context) (int64, bool) {
	if ctx == nil {
		return 0, false
	}
	id, ok := ctx.Value(senderIDKey{}).(int64)
	return id, ok
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

// SendMessageWithKeyboard sends one plain-text message with an inline
// keyboard to chatID and returns the new message's id. It is the
// approval prompt's transport: the keyboard's buttons carry
// callback_data the callback handler resolves. The text is sent as
// plain text (no parse mode), so approval summaries never need HTML
// escaping. Long texts are sent as-is (approval prompts are short
// by construction); callers must keep them under 4096 runes.
func (g *TelegramGateway) SendMessageWithKeyboard(ctx context.Context, chatID int64, text string, keyboard InlineKeyboardMarkup) (int64, error) {
	result, err := g.call(ctx, "sendMessage", nil, struct {
		ChatID      int64                `json:"chat_id"`
		Text        string               `json:"text"`
		ReplyMarkup InlineKeyboardMarkup `json:"reply_markup"`
	}{ChatID: chatID, Text: text, ReplyMarkup: keyboard})
	if err != nil {
		return 0, err
	}
	var msg sendMessageResult
	if err := json.Unmarshal(result, &msg); err != nil {
		return 0, fmt.Errorf("messaging: telegram sendMessage: decode result: %w", err)
	}
	return msg.MessageID, nil
}

// AnswerCallbackQuery answers a callback query (removes the loading
// spinner on the tapped button, and optionally shows a short toast).
// text is shown as a toast when non-empty (Telegram caps it at 200
// runes). It is best-effort from the caller's view: a failed answer
// must never fail the approval it acknowledges.
func (g *TelegramGateway) AnswerCallbackQuery(ctx context.Context, callbackID, text string) error {
	_, err := g.call(ctx, "answerCallbackQuery", nil, struct {
		CallbackQueryID string `json:"callback_query_id"`
		Text            string `json:"text,omitempty"`
	}{CallbackQueryID: callbackID, Text: text})
	return err
}

// Permission callback data format: "perm:<decision>:<requestID>",
// where decision is approve, deny, always or never. The request ID
// is the gate's opaque pending-request ID (16 hex chars).
const permissionCallbackPrefix = "perm:"

// ApprovalKeyboard builds the inline keyboard for an approval
// prompt: Approve once / Deny on the first row, Always allow /
// Never allow on the second. "Always"/"Never" persist a standing
// rule for the tool, so the user is not asked again.
func ApprovalKeyboard(requestID string) InlineKeyboardMarkup {
	return InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{
			{Text: "✅ Approve once", CallbackData: permissionCallbackPrefix + "approve:" + requestID},
			{Text: "❌ Deny", CallbackData: permissionCallbackPrefix + "deny:" + requestID},
		},
		{
			{Text: "✅ Always allow", CallbackData: permissionCallbackPrefix + "always:" + requestID},
			{Text: "⛔ Never allow", CallbackData: permissionCallbackPrefix + "never:" + requestID},
		},
	}}
}

// ParsePermissionCallback parses callback data from an approval
// keyboard button. It returns the decision ("approve", "deny",
// "always" or "never") and the pending-request ID. ok is false for
// any other callback data (other buttons, other features).
func ParsePermissionCallback(data string) (decision, requestID string, ok bool) {
	if !strings.HasPrefix(data, permissionCallbackPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(data, permissionCallbackPrefix)
	decision, requestID, found := strings.Cut(rest, ":")
	if !found || requestID == "" {
		return "", "", false
	}
	switch decision {
	case "approve", "deny", "always", "never":
		return decision, requestID, true
	default:
		return "", "", false
	}
}

// --- Shared permission-gate Telegram wiring (F-A7-P2-2) -------------
//
// Before rc12 the approval wiring existed twice — once in cmd/orion
// (wireGate) and once in cmd/oriond (buildApprovalGate) — as two
// near-identical copies of the prompter text, the expiry hook, the
// card tracker and the tap resolver, plus two separate gate
// constructions (oriond called permissions.NewGate directly while
// cmd/orion called app.NewPermissionGate). The security property
// held on both paths, but the copies could drift apart again, and
// the wireGate comment claiming oriond used the shared constructor
// was false. The wiring below is the single implementation both
// binaries use: each binary still builds its gate through the
// shared constructor (app.NewPermissionGate) and passes the
// prompter from ApprovalPrompter, then installs the expiry hook
// and the callback handler from here. The only per-binary
// difference is the log prefix ("orion" vs "oriond"), passed in so
// log lines keep naming the binary that emitted them. Gate
// behaviour is unchanged: same prompt text, same expiry edit,
// same requester-or-owner tap rule.

// ApprovalCards tracks live approval-card message IDs by pending
// request ID. Note records a card when the prompter sends it;
// Take removes and returns it when the request expires (so the
// expiry edit runs once); Forget removes it when the request is
// answered (so a resolved card is never expired later). All
// methods are nil-safe: a nil *ApprovalCards means "no tracking"
// (tests that call HandleApprovalCallback directly pass nil).
type ApprovalCards struct {
	mu sync.Mutex
	m  map[string]int64
}

// NewApprovalCards returns an empty card tracker.
func NewApprovalCards() *ApprovalCards { return &ApprovalCards{m: map[string]int64{}} }

// Note records msgID as the live card for pending request id.
func (c *ApprovalCards) Note(id string, msgID int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]int64{}
	}
	c.m[id] = msgID
}

// Take removes and returns the card for id, reporting whether one
// was tracked.
func (c *ApprovalCards) Take(id string) (int64, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	msgID, ok := c.m[id]
	if ok {
		delete(c.m, id)
	}
	return msgID, ok
}

// Forget removes any tracked card for id without returning it.
func (c *ApprovalCards) Forget(id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
}

// ApprovalPromptText renders the approval-card text for req: what
// is being asked, which chat it is for, and who requested it, plus
// the button instructions. Shared by both binaries so the card a
// user sees never depends on which entrypoint is running.
func ApprovalPromptText(req permissions.ApprovalRequest) string {
	return fmt.Sprintf("⚠️ Approval needed\n\n%s\n\nChat: %d\nRequested by user: %d\n\nTap ✅ to run it once, ❌ to refuse it, or use Always/Never to remember the choice for this tool in this chat only.", req.Summary(), req.ChatID, req.SenderID)
}

// ApprovalPrompter builds the gate prompter for gw: it sends the
// Telegram inline-keyboard approval card (recording its message ID
// in cards so the expiry hook can edit it in place) and logs the
// prompt. A send failure is returned so the gate can treat the
// prompt as undelivered.
func ApprovalPrompter(gw *TelegramGateway, cards *ApprovalCards, logPrefix string) permissions.Prompter {
	// Capture stderr once, here: the prompter runs on turn
	// goroutines, and reading the os.Stderr global on every
	// prompt races with any test (or embedder) that swaps it
	// while the daemon runs (data race found by -race in
	// TestW3DaemonAndTelegramPollGatePrompts, pre-existing on
	// rc12). The captured writer is the same destination in
	// production; only the read moves to setup time.
	stderr := os.Stderr
	return func(ctx context.Context, req permissions.ApprovalRequest) error {
		msgID, err := gw.SendMessageWithKeyboard(ctx, req.ChatID, ApprovalPromptText(req), ApprovalKeyboard(req.ID))
		if err == nil {
			cards.Note(req.ID, msgID)
			fmt.Fprintf(stderr, "%s: approval prompt: tool %q request %s chat %d card %d\n", logPrefix, req.Tool, req.ID, req.ChatID, msgID)
		}
		return err
	}
}

// InstallApprovalExpiry installs gate's expiry hook: when a
// request dies unanswered, its card is edited in place — text
// replaced and keyboard REMOVED, so nobody can tap a dead button.
// The edit runs in a goroutine (the hook must return promptly)
// with a background context — the turn's context may already be
// cancelled, which is exactly why the request expired.
// Best-effort: a failed edit is logged, never fatal.
func InstallApprovalExpiry(gate *permissions.Gate, gw *TelegramGateway, cards *ApprovalCards, logPrefix string) {
	// Capture stderr once, here: the hook and its edit goroutine
	// run on background goroutines, and reading the os.Stderr
	// global there races with any test (or embedder) that swaps
	// it while the daemon runs (data race found by -race in
	// TestW3DaemonAndTelegramPollGatePrompts, pre-existing on
	// rc12: the expiry goroutine's Fprintf read os.Stderr while
	// the test's capture helper restored it). The captured
	// writer is the same destination in production.
	stderr := os.Stderr
	gate.SetOnExpire(func(req permissions.ApprovalRequest) {
		msgID, ok := cards.Take(req.ID)
		if !ok {
			return
		}
		fmt.Fprintf(stderr, "%s: approval expired: request %s chat %d card %d\n", logPrefix, req.ID, req.ChatID, msgID)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// Log an edit failure: a failed expiry edit (e.g.
			// message deleted) must leave a trace instead of
			// being swallowed.
			if err := gw.EditMessageTextMarkup(ctx, req.ChatID, msgID, "⚠️ Approval request: ⏰ Expired — no answer within the approval window. Send the request again to retry.", InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}}); err != nil {
				fmt.Fprintf(stderr, "%s: approval expired: request %s card %d edit failed: %v\n", logPrefix, req.ID, msgID, err)
			}
		}()
	})
}

// HandleApprovalCallback resolves one permission-gate approval tap
// (P1-O2): the decision comes from the button's callback data
// (perm:<decision>:<requestID>); the pending request is looked up
// in the gate, and only the user whose turn requested the tool call
// — or the owner — may answer it. A tap from anyone else is refused
// without touching the pending request, so a stranger can neither
// approve nor deny someone else's tool call. The approval message
// is edited in place to show the outcome, and the query is answered
// to clear the button spinner. A tap on an expired request is told
// so. Every tap is logged (decision, request ID, sender, outcome):
// a tap that never arrives and a tap on a dead card must be
// distinguishable in the log.
func HandleApprovalCallback(ctx context.Context, gw *TelegramGateway, gate *permissions.Gate, ownerID int64, q TGCallbackQuery, cards *ApprovalCards, logPrefix string) error {
	// Capture stderr once, here: this runs on callback-handler
	// goroutines, and reading the os.Stderr global on every tap
	// races with any test (or embedder) that swaps it while the
	// daemon runs (same class as the InstallApprovalExpiry race
	// found by -race in TestW3DaemonAndTelegramPollGatePrompts).
	// The capture is race-free for this flow: no new handler
	// starts after the poll loop exits, so the entry read can
	// never overlap the swap-back that follows it.
	stderr := os.Stderr
	decision, id, ok := ParsePermissionCallback(q.Data)
	if !ok {
		// Log the unparseable tap: without this a tap the
		// handler cannot understand is silent, and a malformed
		// button looks exactly like a dead one. The data is a
		// short callback payload (perm:<decision>:<id>), never
		// free user text, so logging it is safe.
		fmt.Fprintf(stderr, "%s: approval tap: unparseable callback data %q (query %s)\n", logPrefix, q.Data, q.ID)
		return gw.AnswerCallbackQuery(ctx, q.ID, "Unknown button.")
	}
	senderID := int64(0)
	if q.From != nil {
		senderID = q.From.ID
	}
	fmt.Fprintf(stderr, "%s: approval tap: %s request %s from %d\n", logPrefix, decision, id, senderID)
	req, found := gate.Pending(id)
	if !found {
		fmt.Fprintf(stderr, "%s: approval tap: request %s expired (no longer pending)\n", logPrefix, id)
		return gw.AnswerCallbackQuery(ctx, q.ID, "This approval request has expired.")
	}
	if senderID != req.SenderID && senderID != ownerID {
		fmt.Fprintf(stderr, "%s: approval tap: request %s refused for sender %d (requester %d, owner %d)\n", logPrefix, id, senderID, req.SenderID, ownerID)
		return gw.AnswerCallbackQuery(ctx, q.ID, "Only the person who asked can approve this.")
	}
	var persist *permissions.Action
	approved := false
	outcome := ""
	switch decision {
	case "approve":
		approved, outcome = true, "✅ Approved — running it now."
	case "deny":
		approved, outcome = false, "❌ Denied."
	case "always":
		allow := permissions.ActionAllow
		persist, approved, outcome = &allow, true, "✅ Approved — I won't ask again for this tool in this chat."
	case "never":
		deny := permissions.ActionDeny
		persist, approved, outcome = &deny, false, "⛔ Denied — I won't ask again for this tool in this chat."
	}
	if !gate.Resolve(id, approved, persist) {
		fmt.Fprintf(stderr, "%s: approval tap: request %s expired during resolve\n", logPrefix, id)
		return gw.AnswerCallbackQuery(ctx, q.ID, "This approval request has expired.")
	}
	fmt.Fprintf(stderr, "%s: approval tap: request %s resolved approved=%v\n", logPrefix, id, approved)
	cards.Forget(id)
	// Edit the prompt in place so the chat shows the outcome. The
	// edit carries the outcome only (no tool arguments): the
	// prompt's summary is user/model-derived text and
	// EditMessageText sends HTML, so echoing it back could break
	// the markup. The edit uses EditMessageTextMarkup with an EMPTY
	// keyboard: omitting reply_markup leaves the old keyboard in
	// place, so a resolved card would keep live Approve/Deny
	// buttons. An edit failure is logged, never swallowed.
	if q.Message != nil {
		if err := gw.EditMessageTextMarkup(ctx, q.Message.Chat.ID, q.Message.MessageID, "⚠️ Approval request: "+outcome, InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}}); err != nil {
			fmt.Fprintf(stderr, "%s: approval tap: request %s outcome edit failed: %v\n", logPrefix, id, err)
		}
	}
	return gw.AnswerCallbackQuery(ctx, q.ID, outcome)
}

// InstallApprovalCallbackHandler installs the gateway callback
// handler that resolves approval taps through gate: only the
// requester or the owner may answer (see
// HandleApprovalCallback). Callers must install this (or an
// equivalent) or approvals can never be answered — the gate
// prompts, but no tap reaches it.
func InstallApprovalCallbackHandler(gw *TelegramGateway, gate *permissions.Gate, ownerID int64, cards *ApprovalCards, logPrefix string) {
	gw.SetCallbackHandler(func(ctx context.Context, q TGCallbackQuery) error {
		return HandleApprovalCallback(ctx, gw, gate, ownerID, q, cards, logPrefix)
	})
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

// EditMessageTextMarkup edits messageID in chatID in place with new
// HTML text AND a replacement inline keyboard. It is EditMessageText
// with a reply_markup: pass an empty InlineKeyboardMarkup to REMOVE
// the keyboard. That is what an expired approval card needs —
// without it the dead card keeps live-looking Approve/Deny buttons,
// and a tap on them (answered only with a transient "expired"
// toast) looks exactly like a broken button (the rc8 incident:
// the user tapped Approve on a card whose request had already
// timed out). The "message is not modified" tolerance applies
// exactly as in EditMessageText.
func (g *TelegramGateway) EditMessageTextMarkup(ctx context.Context, chatID, messageID int64, htmlText string, markup InlineKeyboardMarkup) error {
	_, err := g.call(ctx, "editMessageText", nil, struct {
		ChatID      int64                `json:"chat_id"`
		MessageID   int64                `json:"message_id"`
		Text        string               `json:"text"`
		ParseMode   string               `json:"parse_mode"`
		ReplyMarkup InlineKeyboardMarkup `json:"reply_markup"`
	}{ChatID: chatID, MessageID: messageID, Text: htmlText, ParseMode: "HTML", ReplyMarkup: markup})
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

// fetchUpdatesLocked long-polls for new updates (timeout=30s on the
// server side, 35s client timeout) and returns them WITHOUT
// advancing the offset. The caller holds fetchMu and must advance
// the offset for every update it has decided (completed, dropped,
// or skipped — F-A8-2: completion, not dispatch) — see
// advanceOffsetTo and PollStream's contiguous confirmation. It
// honours ctx cancellation.
//
// The first SUCCESSFUL fetch also carries allowed_updates
// (F-A16-2): Telegram persists that filter server-side, so sending
// it once resets a stale filter left by an earlier run (the rc8
// incident: a persisted ["message"] filter meant callback_query
// was never delivered and approval buttons silently died). The
// filter rides on the ordinary long poll rather than a separate
// pre-loop call: no extra round-trip, no update is consumed by a
// throwaway call (the offset is not advanced by this call either
// way, so a discarded batch would be re-fetched — but there is no
// reason to waste the round-trip), and a failed first fetch simply
// retries the filter on the next fetch, because
// updatesFilterSent is set only on success. A failed filter can
// therefore never fail startup or stop polling: the fetch's own
// error handling (transient backoff in PollStream) already covers
// it, and polling continues with the filter unset until a fetch
// gets through.
//
// Splitting the fetch from the advance is what closes the
// crash-window loss (F-A7-P2-3): GetUpdates used to advance the
// offset the moment updates arrived, before any of them was
// dispatched, so a daemon dying mid-batch confirmed
// fetched-but-undispatched updates server-side and they were lost.
// A caller that never advances re-fetches the same updates on the
// next poll instead — at-least-once, never lost.
func (g *TelegramGateway) fetchUpdatesLocked(ctx context.Context) ([]Update, error) {
	if ctx == nil {
		ctx = context.Background()
	}
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
	// F-A16-2: subscribe to message + callback_query updates on
	// the first fetch (see the method comment). Only while the
	// filter has not yet been sent successfully; every later
	// fetch is a plain long poll.
	sendFilter := !g.updatesFilterSent
	if sendFilter {
		allowed, err := json.Marshal(allowedUpdates)
		if err != nil {
			// Unreachable in practice (a []string always
			// marshals); if it ever happened, poll without
			// the filter rather than failing the fetch.
			g.logf("telegram poll: allowed_updates filter could not be encoded (%v); polling without it", err)
			sendFilter = false
		} else {
			query.Set("allowed_updates", string(allowed))
		}
	}

	result, err := g.call(pollCtx, "getUpdates", query, nil)
	if err != nil {
		return nil, err
	}
	var updates []Update
	if err := json.Unmarshal(result, &updates); err != nil {
		return nil, fmt.Errorf("messaging: telegram getUpdates: decode result: %w", err)
	}
	if updates == nil {
		updates = []Update{}
	}
	if sendFilter {
		// The filter landed: this fetch succeeded, so Telegram
		// has the subscription. Never send it again on this
		// gateway (once per startup).
		g.updatesFilterSent = true
		g.logf("telegram poll: allowed_updates filter set to [message callback_query]")
	}
	return updates, nil
}

// advanceOffsetTo moves the offset to next when next is ahead of it.
// The offset only ever moves forward: a stale or duplicate advance
// is a no-op. It is the single place the offset changes.
func (g *TelegramGateway) advanceOffsetTo(next int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if next > g.offset {
		g.offset = next
	}
}

// advanceOffset moves the offset past every update in updates (to
// max(update_id) + 1), so the next call only returns newer updates.
// It is GetUpdates' whole-batch confirmation: every fetched update
// is decided by the caller at once, so all of them are confirmed
// together.
func (g *TelegramGateway) advanceOffset(updates []Update) {
	if len(updates) == 0 {
		return
	}
	var maxID int64 = updates[0].UpdateID
	for _, u := range updates[1:] {
		if u.UpdateID > maxID {
			maxID = u.UpdateID
		}
	}
	g.advanceOffsetTo(maxID + 1)
}

// GetUpdates long-polls for new updates (timeout=30s on the server side,
// 35s client timeout). After a successful call the internal offset is
// advanced to max(update_id) + 1, so the next call only returns newer
// updates. It honours ctx cancellation.
//
// Serialise whole polls (see fetchMu): the offset read, the
// request, and the offset advance are one atomic step, so two
// concurrent callers can never fetch the same offset and deliver
// the same updates twice. A second caller waits here for the
// in-flight long poll (up to ~35s) instead of duplicating it.
// PollStream does NOT use this method: it fetches with
// fetchUpdatesLocked and confirms per dispatched update instead,
// so a crash mid-batch cannot confirm updates it never dispatched.
func (g *TelegramGateway) GetUpdates(ctx context.Context) ([]Update, error) {
	// Serialise whole polls (see fetchMu): the offset read, the
	// request, and the offset advance are one atomic step, so two
	// concurrent callers can never fetch the same offset and deliver
	// the same updates twice. A second caller waits here for the
	// in-flight long poll (up to ~35s) instead of duplicating it.
	g.fetchMu.Lock()
	defer g.fetchMu.Unlock()

	updates, err := g.fetchUpdatesLocked(ctx)
	if err != nil {
		return nil, err
	}
	g.advanceOffset(updates)
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
//
// Each batch is dispatched in two passes (F-A7-P2-3): all callback
// queries first, then messages. A message earlier in a batch must
// never delay a tap later in that batch behind a turn slot.
// The getUpdates offset advances only as updates complete,
// are dropped, or are skipped — and only past the longest
// contiguous run of decided updates — so a crash mid-turn
// re-delivers the in-flight update instead of confirming and
// losing it (F-A8-2: confirmation happens on handler
// completion, never at spawn).
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
	// cbSem bounds concurrent callback handlers SEPARATELY from
	// turns (rc11 W7): a callback query (an inline-keyboard tap) is
	// a millisecond Resolve, but a turn handler holds its sem slot
	// for the whole turn — including a 90s approval wait and
	// multi-minute model runs. When callbacks shared the turn
	// semaphore, all 8 slots held meant the poll loop blocked
	// acquiring a slot for a tap and the callback was never
	// dispatched: zero tap lines, gate timed out, polling stalled
	// for every chat (the live rc10 incident, cards 245 and 253).
	// Callbacks must never wait behind turns.
	cbSem := make(chan struct{}, maxPollHandlers)
	var chatLocks [64]sync.Mutex
	chatLock := func(chatID int64) *sync.Mutex {
		return &chatLocks[uint64(chatID)%uint64(len(chatLocks))]
	}

	// Offset confirmation tracker (F-A8-2). The offset is a single
	// watermark and advances ONLY when a handler COMPLETES — never
	// at spawn. rc12 confirmed an update the moment its handler
	// goroutine was spawned, so the next getUpdates re-issued with
	// the advanced offset confirmed the in-flight turn
	// server-side; a crash/restart any time during a minutes-long
	// turn then lost the update permanently (never re-delivered,
	// no reply). Four silent VM reboots happened on 2026-11-13,
	// so this was live risk, not theory.
	//
	// Semantics (at-least-once):
	//   - "Decided" means the handler finished (reply sent, or
	//     callback answered, or panic recovered and apology
	//     sent) or the update was deliberately dropped/skipped
	//     (non-allowlisted sender/chat, empty text). An update
	//     whose handler is still running is NOT decided, and the
	//     watermark stays behind it — and behind every later
	//     update too, because the watermark only advances past
	//     the longest contiguous run of decided updates.
	//   - A re-delivered update (after a crash mid-turn Telegram
	//     re-delivers every unconfirmed update for 24h) is
	//     re-dispatched and the turn runs again — but its tools
	//     do NOT re-run blindly (F-A9-1): the handler carries
	//     the update id in its context (see WithUpdateID below)
	//     and the agent loop replays recorded tool results from
	//     the durable tool journal instead of re-executing them
	//     (see core.ToolJournal). Subagent lifecycle tools are
	//     excluded from the journal — their results reference
	//     in-process state that does not survive a restart — so
	//     they re-execute against the live Manager, and the
	//     run-1 child is dead with the old process, so no work
	//     is duplicated. A re-delivered turn may still send a
	//     second reply when the killed run had already replied
	//     but was not yet confirmed (the confirmation window
	//     is microseconds, so this is rare) — the journal
	//     dedups tool execution, not replies. What IS
	//     guaranteed: a turn is never dispatched twice
	//     CONCURRENTLY — inFlight dedupes re-fetched updates
	//     while their handler is still running, and the
	//     per-chat lock serialises a re-run against any turn
	//     for the same chat.
	//   - If a handler blocks forever, the watermark stays
	//     behind it forever: later updates are still fetched
	//     and dispatched (the poll loop does not stall behind
	//     one turn), but they are never confirmed past it. The
	//     loop re-fetches the in-flight update on every poll;
	//     when a fetch makes no progress at all (every update
	//     in it was already in-flight) the loop waits for a
	//     completion signal (up to 1s) instead of spinning a
	//     tight re-fetch loop. This is the at-least-once price,
	//     and it is bounded: at most one extra getUpdates per
	//     second per stuck turn.
	//   - The offset is in-memory, so on daemon restart it
	//     starts from 0 and Telegram re-delivers everything
	//     unconfirmed within its 24h window. A re-delivered
	//     turn that had already completed before the crash is
	//     confirmed again only after its re-run completes —
	//     same semantics as above, no special case.
	var confirmMu sync.Mutex
	nextConfirm := g.Offset()
	decided := make(map[int64]bool)
	inFlight := make(map[int64]bool)
	// completed is signalled (non-blocking, depth 1) on every
	// markDecided, so the poll loop can wake promptly from its
	// no-progress wait when a handler finishes.
	completed := make(chan struct{}, 1)
	notifyCompleted := func() {
		select {
		case completed <- struct{}{}:
		default:
		}
	}
	// markDecided records update id as finished and advances the
	// watermark past the longest contiguous run of decided
	// updates starting at nextConfirm. It is called only from a
	// handler's defer (on completion) or synchronously for a
	// deliberately dropped/skipped update — never at spawn.
	//
	// NOTE: markDecided deliberately does NOT delete decided
	// entries as the watermark passes them. A getUpdates call
	// issued with an older offset can still be in flight (a
	// long poll lasts up to 30s) while handlers complete and
	// the watermark advances; when that stale batch returns,
	// it still contains the already-decided updates, and the
	// dispatch loop must recognise them as decided to skip
	// them instead of running those turns a second time. The
	// entries are pruned only in the poll loop, against the
	// offset the batch being processed was fetched with (see
	// below): an id below that offset can never be returned
	// again, by this batch or any later one.
	markDecided := func(id int64) {
		confirmMu.Lock()
		decided[id] = true
		delete(inFlight, id)
		for decided[nextConfirm] {
			nextConfirm++
		}
		next := nextConfirm
		confirmMu.Unlock()
		g.advanceOffsetTo(next)
		notifyCompleted()
	}
	// isDecided reports whether update id already finished (or
	// was deliberately dropped/skipped) but is still waiting
	// behind the watermark for an earlier in-flight update.
	// Such an update is re-fetched on every poll until the
	// watermark passes it — it must be skipped, not dispatched
	// again, or a fast turn behind a slow one would re-run on
	// every poll cycle.
	isDecided := func(id int64) bool {
		confirmMu.Lock()
		defer confirmMu.Unlock()
		return decided[id]
	}
	// markInFlight claims update id for a handler that is about
	// to be spawned; it returns false when a handler for id is
	// already running (a re-fetch of an in-flight update), in
	// which case the caller must NOT dispatch it again.
	markInFlight := func(id int64) bool {
		confirmMu.Lock()
		defer confirmMu.Unlock()
		if inFlight[id] {
			return false
		}
		inFlight[id] = true
		return true
	}
	// unmarkInFlight releases a claim made by markInFlight when
	// the handler was never spawned (semaphore acquisition lost
	// to ctx cancellation), so the update can be dispatched on
	// the next poll instead of staying claimed forever.
	unmarkInFlight := func(id int64) {
		confirmMu.Lock()
		defer confirmMu.Unlock()
		delete(inFlight, id)
	}
	// noteBatchStart handles the first update of a fetched batch
	// starting above the watermark (a gap that cannot occur on
	// the real API, where update_ids are contiguous): the run
	// starts at that update instead of stalling forever below
	// it — unless an in-flight update sits below it, in which
	// case the watermark must stay behind that update.
	noteBatchStart := func(firstID int64) {
		confirmMu.Lock()
		jumped := false
		if firstID > nextConfirm {
			jumped = true
			for id := range inFlight {
				if id < firstID {
					jumped = false
					break
				}
			}
			if jumped {
				nextConfirm = firstID
			}
		}
		next := nextConfirm
		confirmMu.Unlock()
		// Advancing the offset to nextConfirm is safe: the
		// offset is inclusive (getUpdates returns updates with
		// id >= offset), so pointing it at the lowest undecided
		// update still re-delivers that update — it confirms
		// nothing. It keeps Offset() meaningful (the next
		// unconfirmed update id) while a turn is in-flight,
		// instead of leaving it at its pre-batch value.
		if jumped {
			g.advanceOffsetTo(next)
		}
	}

	handle := func(upd Update) {
		// Registered first, so it runs LAST (LIFO): after the
		// reply/apology is sent, after the panic recovery, and
		// after the semaphore slot is released. The update is
		// confirmed only once this handler has fully completed
		// (F-A8-2) — never at spawn.
		defer markDecided(upd.UpdateID)
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
			// The Add is wrapped so a panic inside it still
			// releases cardMu (F-A9-P2-7): the deferred final
			// card edit must be able to take the lock on the
			// panic path. The unlock stays before syncCard —
			// syncCard takes cardMu itself.
			func() {
				cardMu.Lock()
				defer cardMu.Unlock()
				card.Add(ev)
			}()
			// Tool-call and compaction events can change the card;
			// other kinds (step/turn) are ignored by
			// TerminalCard.Add anyway. A compaction event alone
			// creates the card: a turn that auto-compacts (or
			// warns) with no tool calls still shows the user what
			// happened, via the final syncCard(true) below at the
			// latest.
			if ev.Kind == core.EventToolCallStarted || ev.Kind == core.EventToolCallFinished ||
				ev.Kind == core.EventCompactionWarning || ev.Kind == core.EventAutoCompacted {
				syncCard(false)
			}
		}

		// "typing" while the turn runs, re-sent every 4s: Telegram
		// shows a chat action for only a few seconds.
		//
		// stopTyping closes typingDone and does the final card
		// edit. It is a sync.OnceFunc registered as a defer
		// (F-A9-P2-7): the old code closed typingDone on the
		// normal return path only, so a handler panic unwound
		// past it to the recover defer and the typing goroutine
		// leaked — one goroutine plus one Telegram POST every
		// 4s per panicked turn, until daemon restart. The
		// explicit stopTyping() call after the handler returns
		// runs it on the normal path (before the reply, as
		// before); the defer runs it on the panic path, and
		// the Once makes the second call a no-op.
		typingDone := make(chan struct{})
		stopTyping := sync.OnceFunc(func() {
			close(typingDone)
			// Final card edit: show the finished state
			// (including any failed tool call) even if the
			// last event was inside the throttle window.
			// syncCard is a no-op when the card was never
			// created or nothing changed.
			syncCard(true)
		})
		defer stopTyping()
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

		// The handler runs with the sender in its context (P1-O4):
		// the turn handler reads it for the owner tier and the
		// permission gate's approval context. A nil From cannot
		// reach here — the sender check above drops it.
		senderID := int64(0)
		if upd.Message.From != nil {
			senderID = upd.Message.From.ID
		}
		handlerCtx := WithSenderID(ctx, senderID)
		// The update id travels with the turn (F-A9-1): the
		// agent loop reads it from the context and replays
		// recorded tool results from the durable tool journal
		// instead of re-executing them, so a turn killed
		// mid-execution and re-delivered after a restart runs
		// each tool exactly once. Callbacks carry no turn and
		// get no update id.
		handlerCtx = core.WithUpdateID(handlerCtx, upd.UpdateID)
		reply, stats, herr := handler(handlerCtx, chatID, upd.Message.Text, emit)
		stopTyping()

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

	// handleCallback answers one callback query (an inline-keyboard
	// tap). It runs OUTSIDE the per-chat lock on purpose: a tap
	// resolves a pending approval while the turn that requested it
	// is still blocked waiting for the answer, and that turn holds
	// the chat's lock — handling the tap under the same lock would
	// deadlock the approval round-trip. A handler panic or error is
	// logged and polling continues; the query is always answered
	// (even when no handler is installed) so the button's spinner
	// clears instead of hanging.
	handleCallback := func(q TGCallbackQuery, updateID int64) {
		// Registered first, so it runs LAST (LIFO): after the
		// query is answered (or the tap failed and was logged),
		// after the panic recovery, and after the cbSem slot is
		// released. The update is confirmed only once this
		// handler has fully completed (F-A8-2) — never at spawn.
		defer markDecided(updateID)
		defer func() { <-cbSem }()
		defer func() {
			if r := recover(); r != nil {
				g.logf("telegram poll: callback handler panicked: %v", r)
				_ = g.AnswerCallbackQuery(ctx, q.ID, "Sorry, that tap failed.")
			}
		}()
		senderID := int64(0)
		if q.From != nil {
			senderID = q.From.ID
		}
		cbCtx := WithSenderID(ctx, senderID)
		h := g.callbackHandlerFor()
		if h == nil {
			_ = g.AnswerCallbackQuery(ctx, q.ID, "This button is not handled right now.")
			return
		}
		if err := h(cbCtx, q); err != nil {
			g.logf("telegram poll: callback handler error: %v", err)
			_ = g.AnswerCallbackQuery(ctx, q.ID, "Sorry, that tap failed.")
		}
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

		// Fetch WITHOUT advancing the offset (F-A7-P2-3, F-A8-2):
		// the offset advances only as updates complete below
		// (see markDecided), so a crash mid-batch or mid-turn
		// cannot confirm updates this loop never finished.
		// fetchMu is held only
		// for the fetch itself: the dispatch loop below can block
		// on the turn semaphore, and holding the fetch lock across
		// it would stall any concurrent GetUpdates caller for the
		// same time. Production has a single poller, so no second
		// fetcher can observe the un-advanced offset mid-batch;
		// GetUpdates callers keep their own atomic
		// fetch-and-advance (see GetUpdates).
		// fetchOffset is the offset this batch was requested
		// with (fetchUpdatesLocked reads the same offset
		// immediately after, so the request's offset is >=
		// this value). It bounds the decided-set pruning
		// below: any decided id below it can never be
		// returned again, by this batch or any later one,
		// because the offset only ever moves forward.
		fetchOffset := g.Offset()
		g.fetchMu.Lock()
		updates, err := g.fetchUpdatesLocked(ctx)
		g.fetchMu.Unlock()
		// Prune decided entries that can never be re-fetched
		// (F-A8-2 stale-batch hole): a getUpdates issued with
		// an older offset can return after the watermark has
		// already advanced past some of its updates, so those
		// updates must stay recognisable as decided until a
		// batch fetched with a newer offset is processed —
		// pruning against this batch's own fetch offset is
		// exactly that point. Pruning earlier (e.g. in
		// markDecided as the watermark passed them) let a
		// stale batch re-dispatch already-finished turns.
		confirmMu.Lock()
		for id := range decided {
			if id < fetchOffset {
				delete(decided, id)
			}
		}
		confirmMu.Unlock()
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

		// Every received update is logged first, in arrival order
		// (id + kind + sender / chat ids only — never message text
		// or callback data): the rc10 incident was undiagnosable
		// precisely because nothing recorded whether getUpdates
		// ever RETURNED the tap. With this line the next incident
		// reads either "received update N callback_query" (the
		// loss is downstream of the poll) or nothing at all (the
		// loss is upstream: another poller confirmed it, or
		// Telegram never delivered it).
		for _, upd := range updates {
			g.logReceivedUpdate(upd)
		}

		// Confirmation state (F-A8-2, see the tracker above
		// handle): markDecided runs only when a handler
		// COMPLETES, never at spawn. The old comment here
		// claimed the safe direction held generally — "a turn
		// killed mid-flight by the crash runs again" — but the
		// probe in Audit 8 falsified that: the offset was
		// confirmed at dispatch, so a turn killed mid-flight
		// was NEVER re-delivered. What is true now: a decided
		// update is one whose handler finished, or one this
		// loop dropped or skipped on purpose; an in-flight
		// update is undecided and is re-fetched on the next
		// poll (deduped, not re-dispatched — see
		// markInFlight). A DECIDED update behind the watermark
		// is re-fetched too, and is likewise skipped, not
		// re-dispatched (see isDecided): without that, a fast
		// turn behind a slow one would re-run on every poll.
		// The offset is a single watermark, so
		// it may only pass an update once every earlier update
		// is decided too: a callback later in the batch may
		// complete while an earlier message's turn is still
		// running, but the watermark stays behind that message
		// — a crash now re-delivers the suffix instead of
		// losing it. Telegram delivers update_ids
		// contiguously, so the run always reaches the batch
		// end once every update is decided.
		if len(updates) > 0 {
			noteBatchStart(updates[0].UpdateID)
		}
		// progress counts updates this batch newly decided or
		// newly dispatched. Zero progress with a non-empty batch
		// means every update in it was already in-flight: the
		// loop waits for a completion (below) instead of
		// re-fetching the same in-flight updates in a tight
		// loop.
		progress := 0

		// Pass 1 — callbacks first (F-A7-P2-3, in-batch ordering).
		// Callback queries (inline-keyboard taps) are handled on
		// their own path, outside the per-chat lock (see
		// handleCallback): a tap must be able to resolve a pending
		// approval while the turn that requested it holds the
		// chat's lock. The sender is checked the same way as for
		// messages — a tap from a non-allowlisted user can never
		// approve anything. The callback slot comes from cbSem,
		// NOT the turn semaphore (see above): a tap is dispatched
		// even when every turn slot is held.
		//
		// The passes exist because the loop used to iterate a
		// batch sequentially: a message earlier in the same batch
		// blocked the loop on sem before a callback later in that
		// batch was reached, so a tap could still wait behind a
		// message dispatch while all turn slots were held (up to
		// the 90s approval window — a same-batch tap could still
		// expire). Dispatching every callback before any message
		// closes that ordering hole; the message pass below can
		// block on sem without delaying a tap.
		dispatchCallback := func(upd Update) bool {
			q := upd.CallbackQuery
			// Already finished but still behind the watermark:
			// re-fetched, not new — skip without re-answering.
			if isDecided(upd.UpdateID) {
				return true
			}
			if !g.IsSenderAllowed(q.From) {
				if q.From != nil {
					g.logf("telegram poll: dropped callback query from non-allowlisted user %d", q.From.ID)
				} else {
					g.logf("telegram poll: dropped callback query with no sender")
				}
				markDecided(upd.UpdateID)
				progress++
				return true
			}
			if ctx.Err() != nil {
				return false
			}
			// In-flight dedup (F-A8-2): the offset stays behind
			// a running callback handler, so the next fetch
			// re-delivers it — skip it instead of answering
			// the same tap twice. Not progress: the loop must
			// still wait for this handler to finish.
			if !markInFlight(upd.UpdateID) {
				return true
			}
			select {
			case cbSem <- struct{}{}:
			case <-ctx.Done():
				unmarkInFlight(upd.UpdateID)
				return false
			}
			go handleCallback(*q, upd.UpdateID)
			// No markDecided here: the update is confirmed
			// only when handleCallback completes (its defer).
			progress++
			return true
		}

		// Pass 2 — messages, in batch order. A message with no
		// text is skipped (decided: there is nothing to deliver);
		// anything else needs a turn slot, so this pass is the one
		// that blocks when all slots are held. Blocking here is
		// safe now: every callback in the batch is already
		// dispatched, and the offset still sits behind any
		// undispatched or in-flight message (see markDecided),
		// so a crash or a shutdown mid-pass re-delivers the rest.
		dispatchMessage := func(upd Update) bool {
			// Already finished but still behind the watermark:
			// re-fetched, not new — skip without re-running the
			// turn (a fast turn behind a slow one would
			// otherwise re-run on every poll cycle).
			if isDecided(upd.UpdateID) {
				return true
			}
			if upd.Message == nil || strings.TrimSpace(upd.Message.Text) == "" {
				markDecided(upd.UpdateID)
				progress++
				return true
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
				markDecided(upd.UpdateID)
				progress++
				return true
			}
			// Sender check (P1-O4): the chat being allowlisted is
			// not enough — the SENDER must be allowlisted too. In
			// a group chat this stops non-allowlisted members
			// driving the bot through the group's allowlisting;
			// a message with no sender is dropped outright.
			if !g.IsSenderAllowed(upd.Message.From) {
				if upd.Message.From != nil {
					g.logf("telegram poll: dropped message from non-allowlisted user %d in chat %d", upd.Message.From.ID, upd.Message.Chat.ID)
				} else {
					g.logf("telegram poll: dropped message with no sender in chat %d", upd.Message.Chat.ID)
				}
				markDecided(upd.UpdateID)
				progress++
				return true
			}
			if ctx.Err() != nil {
				return false
			}
			// In-flight dedup (F-A8-2): the offset stays behind
			// a running turn, so the next fetch re-delivers it
			// — skip it instead of running the same turn twice
			// concurrently. Not progress: the loop must still
			// wait for this handler to finish.
			if !markInFlight(upd.UpdateID) {
				return true
			}
			// Acquire a handler slot (or stop if cancelled) before
			// dispatching, so the number of in-flight handlers is
			// bounded even under an update flood.
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				unmarkInFlight(upd.UpdateID)
				return false
			}
			go handle(upd)
			// No markDecided here: the update is confirmed only
			// when handle completes (its defer, F-A8-2).
			progress++
			return true
		}

		for _, upd := range updates {
			if upd.CallbackQuery == nil {
				continue
			}
			if !dispatchCallback(upd) {
				return nil
			}
		}
		for _, upd := range updates {
			if upd.CallbackQuery != nil {
				continue
			}
			if !dispatchMessage(upd) {
				return nil
			}
		}

		// No-progress wait (F-A8-2): every update in this batch
		// was already in-flight, so the next fetch would return
		// the same updates immediately and the loop would spin.
		// Wait for a handler to complete (waking promptly on
		// the completion signal) or up to 1s, whichever comes
		// first, before polling again. A handler that blocks
		// forever holds the watermark behind it — that is the
		// at-least-once semantics — but it costs at most one
		// extra getUpdates per second, and later updates are
		// still fetched and dispatched on each cycle.
		if len(updates) > 0 && progress == 0 {
			select {
			case <-completed:
			case <-time.After(time.Second):
			case <-ctx.Done():
				return nil
			}
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
