// Package tools — the tools framework (auto-discovery + capabilities).
//
// A tool is one file in this package. It declares what it needs and
// the framework wires it:
//
//   - Auto-discovery: each tool file self-registers a factory in
//     its init() (RegisterFactory). DefaultRegistryWithDeps iterates
//     the factories — there is no central literal tool list to edit
//     (and to forget). Adding a tool = one new file + a rebuild
//     (Go is compiled; the rebuild is seconds, the wiring is zero).
//   - RiskClasser: the tool declares its permission-gate risk class.
//     A tool that forgets to declare is caught by the test that
//     every built-in tool implements RiskClasser.
//   - QuestionAskerInstaller: the tool needs the Telegram question
//     transport (an Ask func) installed after the gateway exists.
//     The transport itself lives in internal/messaging (this
//     package cannot import it — messaging imports core, core
//     imports tools, so tools -> messaging would cycle); the
//     framework builds it there and installs it here through this
//     interface, never by tool name or concrete type.
//   - TransportAware: the tool needs a runtime transport installed
//     after the gateway exists. InstallTransport receives a
//     ToolHost carrying the narrow gateway surface below. A tool
//     that needs nothing implements nothing.
//   - CallbackHandler / ReplyInterceptor: the tool handles Telegram
//     callback taps with a prefix, or wants typed replies
//     intercepted before the per-chat lock. Both are declared
//     against narrow interfaces defined here (CallbackGateway,
//     CallbackQuery, ReplyUpdate) so the tool never names a
//     messaging type: *messaging.TelegramGateway satisfies
//     CallbackGateway, and messaging's TGCallbackQuery / Update
//     satisfy CallbackQuery / ReplyUpdate. The Telegram gateway's
//     router and reply chain (internal/messaging) hold these
//     interfaces and dispatch by prefix / in order.
package tools

import (
	"context"
	"fmt"
	"sync"

	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/sandbox"
)

// ToolDeps carries the config-derived values a tool factory may
// need. It carries NOTHING runtime (no gateway, no gate): factories
// run at registry construction, before any transport exists.
// A factory that needs no deps ignores the struct. A nil
// PlanningStore gets a fresh in-memory store (see
// DefaultRegistryWithDeps).
type ToolDeps struct {
	SandboxCfg         sandbox.Config
	FileRoots          []string
	TrustedEgressCIDRs []string
	DataDir            string
	PlanningStore      *PlanningStore
}

// Factory builds one tool from deps. It returns an error when the
// tool cannot be built from the given deps (e.g. a malformed
// trusted-egress CIDR for a web tool) — registry construction then
// fails loudly at startup instead of registering a broken tool.
type Factory func(deps ToolDeps) (Tool, error)

type factoryEntry struct {
	name    string
	factory Factory
}

var (
	factoryMu sync.RWMutex
	factories []factoryEntry
)

// RegisterFactory registers a tool factory under name. It is called
// from a tool file's init() — registration is a property of the
// tool file itself, so a file cannot be present and unregistered.
// A duplicate name is a programming error: it is caught loudly at
// registry construction (the second tool with that name fails to
// register), so a duplicate can never silently shadow a tool.
func RegisterFactory(name string, f Factory) {
	if f == nil {
		panic(fmt.Sprintf("tools: RegisterFactory %q: nil factory", name))
	}
	factoryMu.Lock()
	defer factoryMu.Unlock()
	factories = append(factories, factoryEntry{name: name, factory: f})
}

// registeredFactories returns the factories in registration order
// (init() order within the package, deterministic). The order is
// the construction order; Registry.List sorts by name, so it does
// not affect what the model sees.
func registeredFactories() []factoryEntry {
	factoryMu.RLock()
	defer factoryMu.RUnlock()
	return append([]factoryEntry(nil), factories...)
}

// DefaultRegistryWithDeps constructs a registry with every
// factory-registered tool, built from deps. It is the only
// construction path for the built-in tools: DefaultRegistry,
// DefaultRegistryWithSandbox and
// DefaultRegistryWithSandboxAndTrustedEgress all funnel here.
// A factory error, a tool whose Name() does not match its factory
// name, or a duplicate tool name fails construction loudly.
func DefaultRegistryWithDeps(deps ToolDeps) (*Registry, error) {
	if len(deps.FileRoots) == 0 {
		deps.FileRoots = defaultFileRoots()
	}
	if deps.PlanningStore == nil {
		deps.PlanningStore = NewPlanningStore("")
	}
	r := NewRegistry()
	for _, e := range registeredFactories() {
		t, err := e.factory(deps)
		if err != nil {
			return nil, fmt.Errorf("tools: factory %q: %w", e.name, err)
		}
		if t == nil {
			return nil, fmt.Errorf("tools: factory %q returned a nil tool", e.name)
		}
		if t.Name() != e.name {
			return nil, fmt.Errorf("tools: factory %q returned a tool named %q", e.name, t.Name())
		}
		if err := r.Register(t); err != nil {
			return nil, fmt.Errorf("tools: failed to register %q: %w", t.Name(), err)
		}
	}
	return r, nil
}

// ToolClasses returns the permission-gate risk class each registered
// tool declares, for tools that implement RiskClasser. Tools that
// declare nothing are absent (the gate falls back to its hardcoded
// maps for them). The framework installs this map on the gate at
// startup; a test pins that every built-in tool declares.
func (r *Registry) ToolClasses() map[string]permissions.Class {
	classes := map[string]permissions.Class{}
	if r == nil {
		return classes
	}
	for _, t := range r.List() {
		if rc, ok := t.(RiskClasser); ok {
			classes[t.Name()] = rc.RiskClass()
		}
	}
	return classes
}

// RiskClasser — the tool declares its permission-gate risk class.
// A tool that mutates the machine or the network MUST implement
// this; a tool that forgets is caught by the test that every
// built-in tool declares its class explicitly (a destructive tool
// must not silently slip in as Safe by forgetting to declare).
type RiskClasser interface {
	RiskClass() permissions.Class
}

// QuestionAskerInstaller — the tool needs the Telegram question
// transport (a QuestionAskFunc) installed after the gateway exists.
// The framework installs it; the tool never builds it (the
// transport lives in internal/messaging). This replaces the old
// Registry.SetQuestionAsker name-lookup + concrete-type-assertion
// seam: the framework asks the tool what it needs (this
// interface), never its name or concrete type. A nil fn is a
// no-op (the tool keeps whatever asker it had).
type QuestionAskerInstaller interface {
	InstallQuestionAsker(fn QuestionAskFunc)
}

// InlineButton is one button in an inline keyboard, in the
// tools-side shape. The Telegram gateway converts it to its own
// markup when sending (see the CallbackGateway adapters in
// internal/messaging).
type InlineButton struct {
	Text         string
	CallbackData string
}

// InlineKeyboard is rows of buttons.
type InlineKeyboard struct {
	Rows [][]InlineButton
}

// CallbackGateway is the narrow Telegram surface a tool's transport
// may need. *messaging.TelegramGateway satisfies it (via small
// adapter methods in internal/messaging that convert the
// tools-side keyboard to Telegram markup). The narrow surface is
// deliberate: a tool must never need the full gateway, and this
// package must never import messaging (import cycle).
//
//   - SendText sends plain text and returns the message id.
//   - SendKeyboard sends text with an inline keyboard and returns
//     the message id.
//   - EditTextRemoveKeyboard edits a message in place with new HTML
//     text and REMOVES its keyboard (an empty keyboard).
//   - AnswerCallback answers a callback query (clears the button
//     spinner, optionally shows a short toast).
type CallbackGateway interface {
	SendText(ctx context.Context, chatID int64, text string) (int64, error)
	SendKeyboard(ctx context.Context, chatID int64, text string, keyboard InlineKeyboard) (int64, error)
	EditTextRemoveKeyboard(ctx context.Context, chatID, messageID int64, htmlText string) error
	AnswerCallback(ctx context.Context, callbackID, text string) error
}

// CallbackQuery is the narrow view of a Telegram callback query a
// tool's callback handler may need. messaging.TGCallbackQuery
// satisfies it. All accessors are nil-safe (a missing From or
// Message reads as zero).
type CallbackQuery interface {
	// CallbackData is the button's callback_data payload.
	CallbackData() string
	// CallbackID is the query id, for answering the tap.
	CallbackID() string
	// SenderID is the user who tapped (0 when unknown).
	SenderID() int64
	// HasMessage reports whether the tap belongs to a message the
	// bot can still see.
	HasMessage() bool
	// MessageChatID is the chat of that message (0 when none).
	MessageChatID() int64
	// MessageID is that message's id (0 when none).
	MessageID() int64
}

// CallbackHandler — the tool handles Telegram callback_data with a
// given prefix (e.g. "ask:"). The gateway's router dispatches taps
// by prefix; the tool never touches the gateway's callback
// dispatcher. The prefix must end with ":", be unique, and be at
// most 16 bytes (Telegram caps callback_data at 64 bytes and the
// tool's own payload needs the rest). HandleCallback resolves one
// tap: it must apply the same sender check as approvals (only the
// requester or the owner may act) and return an error only for
// transport failure.
type CallbackHandler interface {
	CallbackPrefix() string
	HandleCallback(ctx context.Context, gw CallbackGateway, q CallbackQuery) error
}

// ReplyUpdate is the narrow view of a Telegram message update a
// reply interceptor may need. messaging.Update satisfies it. All
// accessors are nil-safe (a missing Message reads as zero/empty).
type ReplyUpdate interface {
	// ChatID is the chat the message arrived in (0 when none).
	ChatID() int64
	// SenderID is the user who sent it (0 when unknown).
	SenderID() int64
	// Text is the message text ("" when none).
	Text() string
	// IsCommand reports whether the (trimmed) text is a command
	// (starts with "/") — commands are never answers.
	IsCommand() bool
}

// ReplyInterceptor — the tool wants typed replies intercepted
// before the per-chat lock (the ask_user_question deadlock case:
// the asking turn holds that lock while waiting). The gateway's
// reply chain calls each registered interceptor in registration
// order; the first returning true consumes the update (no turn is
// started). An interceptor MUST return false unless the update is
// definitely its own (same chat, authorised sender, not a
// command) — a greedy interceptor that eats other tools' updates
// or normal turns is a correctness bug.
type ReplyInterceptor interface {
	TryAnswer(upd ReplyUpdate) bool
}

// ToolHost carries the runtime a tool's transport may need. It is
// built once by the framework after the Telegram gateway exists
// and handed to TransportAware tools. Fields are set by the
// framework, never by tools. Gateway is the narrow callback
// surface (never the concrete gateway type); OwnerID is the owner
// (first allowlisted chat); LogPrefix is "orion" | "oriond" — the
// only per-binary difference. There is no DataDir field: no tool
// needs it from the host (the planning store's data dir travels
// in ToolDeps instead).
type ToolHost struct {
	Gateway   CallbackGateway
	OwnerID   int64
	LogPrefix string
}

// TransportAware — the tool needs a runtime transport installed
// after the gateway exists (the two-phase install). The framework
// calls InstallTransport once per tool, after the gateway is
// built, with a ToolHost carrying everything the tool may need. A
// tool that needs nothing implements nothing. InstallTransport is
// called before polling starts, so it cannot race Execute; a nil
// gateway means "no transport" (the tool must then fail cleanly
// at Execute time, never hang).
type TransportAware interface {
	InstallTransport(host *ToolHost) error
}
