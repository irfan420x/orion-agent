// Package messaging — the Telegram callback router and the narrow
// gateway surface the tools framework uses.
//
// The gateway holds ONE callback handler (a second installer would
// overwrite the first). The router is that handler: it dispatches
// taps by callback_data prefix to the tool that owns the prefix
// (tools.CallbackHandler, registered by the framework), and falls
// back to the approval handler for anything else — exactly as the
// old hardcoded dispatch did (ask: taps to questions, everything
// else to approvals). The fallback is the approval handler itself,
// so an unparseable tap is still answered "Unknown button" by the
// same code, with the same log line.
//
// A duplicate prefix is a loud error at startup (fail fast, never
// silently overwrite). "perm:" is reserved for the approval
// fallback: a tool must never claim it.
package messaging

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/tools"
)

// callbackRouter dispatches callback-query taps by prefix.
type callbackRouter struct {
	mu       sync.RWMutex
	handlers map[string]tools.CallbackHandler
	// fallback resolves anything that is not a registered tool
	// prefix — the approval handler (perm: taps and unparseable
	// data alike, exactly as before the router existed).
	fallback CallbackHandler
}

func newCallbackRouter(fallback CallbackHandler) *callbackRouter {
	return &callbackRouter{handlers: map[string]tools.CallbackHandler{}, fallback: fallback}
}

// callbackPrefix extracts the prefix of callback data: the part up
// to and including the first ":". "ask:id:0" -> "ask:",
// "perm:approve:id" -> "perm:", data with no ":" -> "".
func callbackPrefix(data string) string {
	if i := strings.Index(data, ":"); i >= 0 {
		return data[:i+1]
	}
	return ""
}

// register adds a tool callback handler under prefix. The prefix
// must end with ":", be at most 16 bytes (Telegram caps
// callback_data at 64 bytes and the tool's own payload needs the
// rest), and be unique — a duplicate is a loud error, never a
// silent overwrite. "perm:" is reserved for the approval fallback.
func (r *callbackRouter) register(prefix string, h tools.CallbackHandler) error {
	if r == nil {
		return fmt.Errorf("messaging: callback router: nil router")
	}
	if h == nil {
		return fmt.Errorf("messaging: callback router: nil handler for prefix %q", prefix)
	}
	if prefix == "" || !strings.HasSuffix(prefix, ":") {
		return fmt.Errorf("messaging: callback router: prefix %q must end with \":\"", prefix)
	}
	if len(prefix) > 16 {
		return fmt.Errorf("messaging: callback router: prefix %q is %d bytes, max 16", prefix, len(prefix))
	}
	if prefix == permissionCallbackPrefix {
		return fmt.Errorf("messaging: callback router: prefix %q is reserved for approvals", prefix)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.handlers[prefix]; ok {
		return fmt.Errorf("messaging: callback router: duplicate callback prefix %q", prefix)
	}
	r.handlers[prefix] = h
	return nil
}

// setFallback installs the handler for anything that is not a
// registered tool prefix (the approval handler). Replacing it is
// allowed (the last install wins, like SetCallbackHandler).
func (r *callbackRouter) setFallback(h CallbackHandler) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fallback = h
}

// dispatch routes one tap: a registered tool prefix goes to that
// tool's handler; anything else goes to the approval fallback. A
// nil fallback with an unknown prefix is an error (the PollStream
// callback site logs it and answers "Sorry, that tap failed").
func (r *callbackRouter) dispatch(ctx context.Context, gw *TelegramGateway, q TGCallbackQuery) error {
	if r == nil {
		return fmt.Errorf("messaging: callback router: nil router")
	}
	prefix := callbackPrefix(q.Data)
	r.mu.RLock()
	h, ok := r.handlers[prefix]
	fallback := r.fallback
	r.mu.RUnlock()
	if ok {
		return h.HandleCallback(ctx, gw, q)
	}
	if fallback != nil {
		return fallback(ctx, q)
	}
	return fmt.Errorf("messaging: callback router: no handler for prefix %q", prefix)
}

// --- narrow CallbackGateway adapters (tools.CallbackGateway) ---
//
// *TelegramGateway satisfies tools.CallbackGateway through these
// small adapters: they convert the tools-side keyboard to Telegram
// markup and delegate to the existing send/edit/answer methods, so
// a tool's transport can send through the gateway without this
// package's concrete types leaking into internal/tools (import
// cycle: tools -> messaging -> core -> tools).

// SendText sends plain text and returns the message id.
func (g *TelegramGateway) SendText(ctx context.Context, chatID int64, text string) (int64, error) {
	return g.SendMessageReturningID(ctx, chatID, text)
}

// SendKeyboard sends text with an inline keyboard and returns the
// message id.
func (g *TelegramGateway) SendKeyboard(ctx context.Context, chatID int64, text string, keyboard tools.InlineKeyboard) (int64, error) {
	return g.SendMessageWithKeyboard(ctx, chatID, text, toTelegramMarkup(keyboard))
}

// EditTextRemoveKeyboard edits a message in place with new HTML
// text and REMOVES its keyboard (an empty keyboard: omitting
// reply_markup would leave the old keyboard in place).
func (g *TelegramGateway) EditTextRemoveKeyboard(ctx context.Context, chatID, messageID int64, htmlText string) error {
	return g.EditMessageTextMarkup(ctx, chatID, messageID, htmlText, InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}})
}

// AnswerCallback answers a callback query (clears the button
// spinner, optionally shows a short toast).
func (g *TelegramGateway) AnswerCallback(ctx context.Context, callbackID, text string) error {
	return g.AnswerCallbackQuery(ctx, callbackID, text)
}

// toTelegramMarkup converts a tools-side keyboard to Telegram
// markup (same shape, same button order).
func toTelegramMarkup(kb tools.InlineKeyboard) InlineKeyboardMarkup {
	rows := make([][]InlineKeyboardButton, 0, len(kb.Rows))
	for _, row := range kb.Rows {
		buttons := make([]InlineKeyboardButton, 0, len(row))
		for _, b := range row {
			buttons = append(buttons, InlineKeyboardButton{Text: b.Text, CallbackData: b.CallbackData})
		}
		rows = append(rows, buttons)
	}
	return InlineKeyboardMarkup{InlineKeyboard: rows}
}

// --- narrow CallbackQuery accessors (tools.CallbackQuery) ---
//
// TGCallbackQuery satisfies tools.CallbackQuery through these
// methods. All are nil-safe: a missing From or Message reads as
// zero, matching the old direct field reads guarded by nil checks.

// CallbackData is the button's callback_data payload.
func (q TGCallbackQuery) CallbackData() string { return q.Data }

// CallbackID is the query id, for answering the tap.
func (q TGCallbackQuery) CallbackID() string { return q.ID }

// SenderID is the user who tapped (0 when unknown).
func (q TGCallbackQuery) SenderID() int64 {
	if q.From == nil {
		return 0
	}
	return q.From.ID
}

// HasMessage reports whether the tap belongs to a message the bot
// can still see.
func (q TGCallbackQuery) HasMessage() bool { return q.Message != nil }

// MessageChatID is the chat of that message (0 when none).
func (q TGCallbackQuery) MessageChatID() int64 {
	if q.Message == nil {
		return 0
	}
	return q.Message.Chat.ID
}

// MessageID is that message's id (0 when none).
func (q TGCallbackQuery) MessageID() int64 {
	if q.Message == nil {
		return 0
	}
	return q.Message.MessageID
}

// --- gateway router wiring ---

// callbackRouterFor returns the gateway's callback router,
// creating it on demand (gateways built without NewTelegramGateway
// get one lazily, so the router never needs a constructor change).
func (g *TelegramGateway) callbackRouterFor() *callbackRouter {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.router == nil {
		g.router = newCallbackRouter(nil)
	}
	return g.router
}

// RegisterCallbackHandler registers a tool callback handler under
// prefix on the gateway's router (see callbackRouter.register for
// the prefix rules). It is the framework's only way to add a tap
// handler: the gateway never takes a second installed handler.
func (g *TelegramGateway) RegisterCallbackHandler(prefix string, h tools.CallbackHandler) error {
	if g == nil {
		return fmt.Errorf("messaging: RegisterCallbackHandler: nil gateway")
	}
	return g.callbackRouterFor().register(prefix, h)
}

// SetApprovalFallback installs the approval handler as the
// router's fallback (anything that is not a registered tool
// prefix — perm: taps and unparseable data alike). It closes over
// the gateway, gate, cards and log prefix exactly as
// InstallApprovalCallbackHandler did.
func (g *TelegramGateway) SetApprovalFallback(gate *permissions.Gate, ownerID int64, cards *ApprovalCards, logPrefix string) {
	if g == nil {
		return
	}
	gw := g
	g.callbackRouterFor().setFallback(func(ctx context.Context, q TGCallbackQuery) error {
		return HandleApprovalCallback(ctx, gw, gate, ownerID, q, cards, logPrefix)
	})
}

// InstallCallbackRouter installs the router dispatch as the
// gateway's callback handler, so PollStream taps reach it. It is
// idempotent (re-installing just replaces the same dispatch).
func (g *TelegramGateway) InstallCallbackRouter() {
	if g == nil {
		return
	}
	gw := g
	g.SetCallbackHandler(func(ctx context.Context, q TGCallbackQuery) error {
		return gw.callbackRouterFor().dispatch(ctx, gw, q)
	})
}
