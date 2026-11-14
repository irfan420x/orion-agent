// Package messaging — WireTelegram: the one Telegram wiring call.
//
// Every binary that serves Telegram calls this once, after the
// gateway and the permission gate exist and before polling starts.
// It does ALL the Telegram wiring for every tool, so no binary
// ever wires a tool by hand again:
//
//  1. The question transport: a QuestionRegistry is created, its
//     interceptor is installed on the reply chain
//     (SetQuestionRegistry) and its callback handler under "ask:"
//     on the router, and a TelegramQuestionAsker is built and
//     installed on every tool that implements
//     tools.QuestionAskerInstaller.
//  2. Capability discovery: every tool that implements
//     tools.CallbackHandler gets its declared prefix registered
//     on the router, and every tool that implements
//     tools.ReplyInterceptor is added to the reply chain. The
//     question transport itself stays messaging-owned — its
//     pending-question state (the QuestionRegistry) lives here,
//     so the tool cannot implement these interfaces (import
//     cycle: tools -> messaging -> core -> tools); the
//     messaging-owned questionCallbackHandler /
//     questionReplyInterceptor ARE the implementations, and they
//     are registered directly (step 1). The discovery loop is
//     the generic path for tools whose handler/interceptor state
//     lives in the tool itself.
//  3. Transport install: every tool implementing
//     tools.TransportAware gets InstallTransport with a ToolHost
//     (narrow gateway, owner, log prefix).
//  4. Callback routing: the approval handler is the router's
//     fallback, and the router dispatch is installed as the
//     gateway's callback handler.
//  5. Gate classes: the tools' declared RiskClass() values are
//     installed on the gate (SetToolClasses); the hardcoded maps
//     remain the authority (see permissions.classify).
//
// The "ask:" registration is the fallible step, so it runs first:
// a duplicate (WireTelegram called twice on one gateway) fails
// before any gateway state changes, instead of leaving the asker
// and the handler pointed at different registries. The tool
// callback discovery is fallible too (duplicate prefix, 16-byte
// cap, "perm:" reservation), so it runs next, before the
// fallback/router/question-registry installs — a bad declaration
// fails the whole wiring loudly at startup, never silently.
//
// Cards is required: the approval handler Forgets resolved
// requests from the cards, and a nil cards would leave resolved
// cards answerable. A nil registry, gateway, gate or cards is a
// loud startup error, never a silent half-wiring.
//
// A failed WireTelegram leaves the gateway partially wired (the
// "ask:" handler and any tool handlers registered before the
// failure stay registered). That is acceptable because both
// binaries kill the process on a WireTelegram error — a failed
// wiring never serves traffic — and a second call fails loudly
// on the duplicate "ask:" instead of silently re-wiring.
package messaging

import (
	"fmt"

	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/tools"
)

// WireTelegram installs every interactive capability of every
// registered tool on the gateway and the gate. Call once, after
// the gateway and the gate exist, before polling starts. It is the
// ONLY Telegram wiring any tool ever needs.
func WireTelegram(reg *tools.Registry, gw *TelegramGateway, gate *permissions.Gate, cards *ApprovalCards, ownerID int64, logPrefix string) error {
	if reg == nil {
		return fmt.Errorf("messaging: WireTelegram: nil tool registry")
	}
	if gw == nil {
		return fmt.Errorf("messaging: WireTelegram: nil gateway")
	}
	if gate == nil {
		return fmt.Errorf("messaging: WireTelegram: nil permission gate")
	}
	if cards == nil {
		return fmt.Errorf("messaging: WireTelegram: nil approval cards")
	}
	questions := NewQuestionRegistry(ownerID)
	handler := questionCallbackHandler{questions: questions, logPrefix: logPrefix}
	// The fallible step first (see the package doc): a duplicate
	// "ask:" fails before any gateway state changes.
	if err := gw.RegisterCallbackHandler(questionCallbackPrefix, handler); err != nil {
		return fmt.Errorf("messaging: WireTelegram: %w", err)
	}
	// Callback discovery (F-A20-1): every tool that implements
	// tools.CallbackHandler gets its declared prefix registered
	// on the router. This is fallible too (duplicate prefix,
	// 16-byte cap, "perm:" reservation), so it runs here, before
	// the fallback/router/question-registry installs — a bad
	// declaration fails the whole wiring loudly at startup.
	for _, t := range reg.List() {
		if ch, ok := t.(tools.CallbackHandler); ok {
			if err := gw.RegisterCallbackHandler(ch.CallbackPrefix(), ch); err != nil {
				return fmt.Errorf("messaging: WireTelegram: tool %q callback prefix: %w", t.Name(), err)
			}
		}
	}
	gw.SetApprovalFallback(gate, ownerID, cards, logPrefix)
	gw.InstallCallbackRouter()
	gw.SetQuestionRegistry(questions)
	asker := NewTelegramQuestionAsker(gw, questions, ownerID, logPrefix)
	host := &tools.ToolHost{Gateway: gw, OwnerID: ownerID, LogPrefix: logPrefix}
	for _, t := range reg.List() {
		if qi, ok := t.(tools.QuestionAskerInstaller); ok {
			qi.InstallQuestionAsker(asker.Ask)
		}
		// Reply discovery (F-A20-1): every tool that implements
		// tools.ReplyInterceptor is added to the reply chain.
		// AddReplyInterceptor cannot fail (it appends; a nil
		// interceptor is a no-op). The question interceptor was
		// installed first by SetQuestionRegistry above, so it
		// runs before any later tool's interceptor.
		if ri, ok := t.(tools.ReplyInterceptor); ok {
			gw.AddReplyInterceptor(ri)
		}
		if ta, ok := t.(tools.TransportAware); ok {
			if err := ta.InstallTransport(host); err != nil {
				return fmt.Errorf("messaging: WireTelegram: tool %q InstallTransport: %w", t.Name(), err)
			}
		}
	}
	gate.SetToolClasses(reg.ToolClasses())
	return nil
}
