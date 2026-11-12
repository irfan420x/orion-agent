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
	// allowedChats is nil when no allowlist is set (all chats pass).
	allowedChats map[int64]bool
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
// gateway. With no allowlist set, every chat is allowed.
func (g *TelegramGateway) IsChatAllowed(chatID int64) bool {
	if len(g.allowedChats) == 0 {
		return true
	}
	return g.allowedChats[chatID]
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
		if env.ErrorCode != 0 {
			return nil, fmt.Errorf("messaging: telegram %s: HTTP %d, error %d: %s", method, resp.StatusCode, env.ErrorCode, desc)
		}
		return nil, fmt.Errorf("messaging: telegram %s: HTTP %d: %s", method, resp.StatusCode, desc)
	}
	return env.Result, nil
}

// getMeResult is the subset of the Bot API User object GetMe needs.
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
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
}

// sendMessageOnce sends a single message of at most MaxMessageLength runes.
func (g *TelegramGateway) sendMessageOnce(ctx context.Context, chatID int64, text string) error {
	_, err := g.call(ctx, "sendMessage", nil, sendMessageRequest{ChatID: chatID, Text: text})
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

// Poll loops on GetUpdates until ctx is cancelled, delivering each
// incoming message with non-empty text to handler and sending the
// handler's reply back to the same chat.
//
//   - A handler error is reported to the user as
//     "Sorry, I hit an error: <err>" (split/capped by SendMessage);
//     polling continues.
//   - An empty handler reply sends nothing.
//   - A transient GetUpdates error backs off (1s, doubling to a 5s max)
//     and polling continues.
//   - ctx cancellation returns nil cleanly.
//
// Updates are handled concurrently, bounded by maxPollHandlers, so a
// slow handler for one chat does not stall polling or handling for
// other chats. Ordering is preserved best-effort per chat: handlers
// for the same chat run one at a time (striped locks), in dispatch
// order as far as goroutine scheduling allows. When ctx is cancelled
// Poll returns nil promptly; handlers already running are left to
// finish (their reply sends use the cancelled ctx and fail
// best-effort, as before).
func (g *TelegramGateway) Poll(ctx context.Context, handler func(ctx context.Context, chatID int64, text string) (string, error)) error {
	if handler == nil {
		return errors.New("messaging: telegram Poll: handler must not be nil")
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
		lock := chatLock(upd.Message.Chat.ID)
		lock.Lock()
		defer lock.Unlock()
		reply, herr := handler(ctx, upd.Message.Chat.ID, upd.Message.Text)
		if herr != nil {
			msg := fmt.Sprintf("Sorry, I hit an error: %v", herr)
			// Best effort: a failed apology must not kill polling.
			_ = g.SendMessage(ctx, upd.Message.Chat.ID, msg)
			return
		}
		if reply == "" {
			return
		}
		// Best effort: a failed reply must not kill polling.
		_ = g.SendMessage(ctx, upd.Message.Chat.ID, reply)
	}

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
		backoff = time.Second

		for _, upd := range updates {
			if upd.Message == nil || strings.TrimSpace(upd.Message.Text) == "" {
				continue
			}
			// Sender authorisation: drop messages from chats outside
			// the allowlist before the handler (and the agent, and
			// its tools) ever sees them.
			if !g.IsChatAllowed(upd.Message.Chat.ID) {
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
