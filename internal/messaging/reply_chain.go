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
	// logf receives the consumption audit log (which interceptor
	// consumed which update) and panic reports. It is set by the
	// gateway to its own logf when the chain is created, so the
	// lines land in the daemon log in production; nil means
	// silent (tests set it to a recorder).
	logf func(format string, args ...any)
}

// log writes one audit line when a logger is installed; it is a
// no-op otherwise (and nil-safe).
func (c *replyChain) log(format string, args ...any) {
	if c == nil || c.logf == nil {
		return
	}
	c.logf(format, args...)
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
// returns without starting a turn). Each interceptor runs with
// panic isolation (F-A20-10): a panicking interceptor is logged
// and treated as "did not consume", so one broken tool cannot
// kill the poll loop or swallow the update — the chain continues
// with the next interceptor. Every consumption is audit-logged
// (which interceptor, which chat), so a swallowed update is
// diagnosable instead of silent.
func (c *replyChain) tryAnswer(upd Update) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	interceptors := append([]tools.ReplyInterceptor(nil), c.interceptors...)
	c.mu.RUnlock()
	for i, in := range interceptors {
		if c.tryOne(i, in, upd) {
			return true
		}
	}
	return false
}

// tryOne runs one interceptor with panic isolation: a panic is
// logged (with the interceptor's type, so the broken tool is
// identifiable) and the update is left unconsumed, so the chain
// continues. A consumption is audit-logged the same way.
func (c *replyChain) tryOne(i int, in tools.ReplyInterceptor, upd Update) (consumed bool) {
	defer func() {
		if r := recover(); r != nil {
			c.log("reply chain: interceptor %d (%T) panicked: %v (update not consumed; chain continues)", i, in, r)
			consumed = false
		}
	}()
	if in.TryAnswer(upd) {
		c.log("reply chain: interceptor %d (%T) consumed update for chat %d", i, in, upd.ChatID())
		return true
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
		// The chain audit-logs through the gateway's logger, so
		// consumption and panic lines land in the daemon log in
		// production (F-A20-10). The method value reads
		// g.logger fresh on every call, so a logger installed
		// after the chain exists is still honoured.
		g.replyChain.logf = g.logf
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
