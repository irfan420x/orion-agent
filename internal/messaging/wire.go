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
// The "ask:" registration is the fallible step, so its prefix is
// validated first: every callback prefix this call would register
// ("ask:" plus each tool's declared prefix) is pre-validated
// against the router before any is registered, so a duplicate,
// over-long or reserved prefix fails with the router untouched
// instead of leaving the handlers registered before the failure
// behind. The tool callback discovery is fallible too (duplicate
// prefix, 16-byte cap, "perm:" reservation), so it runs next,
// before the fallback/router/question-registry installs — a bad
// declaration fails the whole wiring loudly at startup, never
// silently.
//
// Cards is required: the approval handler Forgets resolved
// requests from the cards, and a nil cards would leave resolved
// cards answerable. A nil registry, gateway, gate or cards is a
// loud startup error, never a silent half-wiring.
//
// A failed WireTelegram leaves no callback handler behind for a
// prefix failure (validate-then-install, F-A21-7): the router is
// untouched, so a retry on the same gateway starts from a clean
// router. A failure in a later step — a tool's InstallTransport —
// can still leave handlers registered, because those steps have
// no inverse (the fallback install deliberately refuses
// replacement, F-A20-6); both binaries kill the process on a
// WireTelegram error, so a failed wiring never serves traffic,
// and a second call fails loudly on the duplicate "ask:" instead
// of silently re-wiring. Rollback was considered and rejected:
// pre-validation is possible here because every prefix rule is a
// pure check, and it needs no new mutation path.
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
	// Validate-then-install (F-A21-7): collect every callback
	// prefix this call would register — "ask:" for the question
	// transport plus each tool's declared prefix — and validate
	// ALL of them before registering any. The within-set check
	// names both tools claiming a prefix; the router check
	// (validatePrefix) applies the same rules register enforces
	// (suffix ":", 16-byte cap, "perm:" reservation, not already
	// installed) without mutating anything. A bad declaration
	// therefore fails here with the router untouched: no handler
	// from the failed attempt survives, and a retry on the same
	// gateway starts clean. WireTelegram runs once at startup
	// before polling, so nothing can register between this
	// validation and the installs below.
	type plannedPrefix struct {
		prefix string
		owner  string // tool name, or "question transport" for ask:
	}
	planned := []plannedPrefix{{prefix: questionCallbackPrefix, owner: "question transport"}}
	for _, t := range reg.List() {
		if ch, ok := t.(tools.CallbackHandler); ok {
			planned = append(planned, plannedPrefix{prefix: ch.CallbackPrefix(), owner: t.Name()})
		}
	}
	seen := make(map[string]string, len(planned))
	router := gw.callbackRouterFor()
	for _, p := range planned {
		if first, dup := seen[p.prefix]; dup {
			return fmt.Errorf("messaging: WireTelegram: tool %q callback prefix %q duplicates tool %q's prefix", p.owner, p.prefix, first)
		}
		seen[p.prefix] = p.owner
		if err := router.validatePrefix(p.prefix); err != nil {
			return fmt.Errorf("messaging: WireTelegram: tool %q callback prefix: %w", p.owner, err)
		}
	}
	// The fallible step first (see the package doc): a duplicate
	// "ask:" fails before any gateway state changes.
	if err := gw.RegisterCallbackHandler(questionCallbackPrefix, handler); err != nil {
		return fmt.Errorf("messaging: WireTelegram: %w", err)
	}
	// Callback discovery (F-A20-1): every tool that implements
	// tools.CallbackHandler gets its declared prefix registered
	// on the router. Every prefix was pre-validated above
	// (F-A21-7), so these registrations cannot fail on a bad
	// declaration — they run here, before the
	// fallback/router/question-registry installs, and a failure
	// here (only a concurrent registration could cause one) is
	// still loud at startup, never silent.
	for _, t := range reg.List() {
		if ch, ok := t.(tools.CallbackHandler); ok {
			if err := gw.RegisterCallbackHandler(ch.CallbackPrefix(), ch); err != nil {
				return fmt.Errorf("messaging: WireTelegram: tool %q callback prefix: %w", t.Name(), err)
			}
		}
	}
	if err := gw.SetApprovalFallback(gate, ownerID, cards, logPrefix); err != nil {
		return fmt.Errorf("messaging: WireTelegram: %w", err)
	}
	if err := gw.InstallCallbackRouter(); err != nil {
		return fmt.Errorf("messaging: WireTelegram: %w", err)
	}
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
		// interceptor is a no-op). Ordering (F-A21-22): the
		// question interceptor was installed first by
		// SetQuestionRegistry above, so it runs before any later
		// tool's interceptor; tool interceptors follow in
		// reg.List() order — alphabetical by tool name,
		// deterministic — and the first returning true wins.
		// There is no priority field: insertion order is the
		// control, and every consumption is audit-logged with
		// the interceptor's type, so a misroute is diagnosable.
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
