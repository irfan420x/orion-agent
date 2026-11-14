// Package messaging — the typed-reply interceptor chain.
//
// PollStream consults the chain BEFORE the per-chat lock: a typed
// reply that answers a pending question must reach the waiting
// turn without waiting on the lock that turn holds (the
// ask_user_question deadlock case). The chain calls each
// registered interceptor in registration order; the first
// returning true consumes the update (no turn is started).
// An interceptor MUST return false unless the update is definitely
// its own (same chat, authorised sender, not a command).
package messaging

import (
	"strings"
	"sync"

	"github.com/irfan/orion/internal/tools"
)

// replyChain is the ordered list of typed-reply interceptors.
type replyChain struct {
	mu           sync.RWMutex
	interceptors []tools.ReplyInterceptor
}

func newReplyChain() *replyChain { return &replyChain{} }

// add appends an interceptor (nil is a no-op). Interceptors run in
// the order added; the question interceptor is added first by
// SetQuestionRegistry, so it runs before any later tool's.
func (c *replyChain) add(in tools.ReplyInterceptor) {
	if c == nil || in == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.interceptors = append(c.interceptors, in)
}

// setQuestion installs (or replaces) the question interceptor —
// the one interceptor that answers pending ask_user_question
// questions from typed replies. Replacing instead of appending
// makes SetQuestionRegistry idempotent: calling it twice never
// leaves two question interceptors in the chain. A nil interceptor
// removes it (no questions).
func (c *replyChain) setQuestion(in *questionReplyInterceptor) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, existing := range c.interceptors {
		if _, ok := existing.(*questionReplyInterceptor); ok {
			if in == nil {
				c.interceptors = append(c.interceptors[:i], c.interceptors[i+1:]...)
			} else {
				c.interceptors[i] = in
			}
			return
		}
	}
	if in != nil {
		c.interceptors = append(c.interceptors, in)
	}
}

// tryAnswer runs the interceptors in order on upd. It reports
// whether any interceptor consumed the update (the caller then
// returns without starting a turn).
func (c *replyChain) tryAnswer(upd Update) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	interceptors := append([]tools.ReplyInterceptor(nil), c.interceptors...)
	c.mu.RUnlock()
	for _, in := range interceptors {
		if in.TryAnswer(upd) {
			return true
		}
	}
	return false
}

// --- narrow ReplyUpdate accessors (tools.ReplyUpdate) ---
//
// Update satisfies tools.ReplyUpdate through these methods. All
// are nil-safe: a missing Message reads as zero/empty.

// ChatID is the chat the message arrived in (0 when none).
func (u Update) ChatID() int64 {
	if u.Message == nil {
		return 0
	}
	return u.Message.Chat.ID
}

// SenderID is the user who sent it (0 when unknown).
func (u Update) SenderID() int64 {
	if u.Message == nil || u.Message.From == nil {
		return 0
	}
	return u.Message.From.ID
}

// Text is the message text ("" when none).
func (u Update) Text() string {
	if u.Message == nil {
		return ""
	}
	return u.Message.Text
}

// IsCommand reports whether the (trimmed) text is a command
// (starts with "/") — commands are never answers.
func (u Update) IsCommand() bool {
	return strings.HasPrefix(strings.TrimSpace(u.Text()), "/")
}

// --- gateway chain wiring ---

// replyChainFor returns the gateway's reply chain, creating it on
// demand (gateways built without NewTelegramGateway get one
// lazily, so the chain never needs a constructor change).
func (g *TelegramGateway) replyChainFor() *replyChain {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.replyChainForLocked()
}

// replyChainForLocked is replyChainFor for callers that already
// hold g.mu (e.g. SetQuestionRegistry).
func (g *TelegramGateway) replyChainForLocked() *replyChain {
	if g.replyChain == nil {
		g.replyChain = newReplyChain()
	}
	return g.replyChain
}

// AddReplyInterceptor registers a typed-reply interceptor on the
// gateway's chain (see replyChain.add for the ordering contract).
// It is the framework's only way to add an interceptor.
func (g *TelegramGateway) AddReplyInterceptor(in tools.ReplyInterceptor) {
	if g == nil {
		return
	}
	g.replyChainFor().add(in)
}

// replyChainTryAnswer runs the gateway's reply chain on upd (see
// replyChain.tryAnswer). PollStream calls this before the
// per-chat lock.
func (g *TelegramGateway) replyChainTryAnswer(upd Update) bool {
	if g == nil {
		return false
	}
	return g.replyChainFor().tryAnswer(upd)
}
