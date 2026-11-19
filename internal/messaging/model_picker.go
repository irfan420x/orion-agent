// Package messaging — the /model button picker.
//
// /model with no arguments shows a Telegram inline keyboard
// instead of the text list: tap a provider, tap a model, confirm
// when the switch is pricey. The picker is stateless: the route
// and the requester travel in callback_data (Telegram caps it at
// 64 bytes), so a daemon restart voids live picker cards exactly
// like it voids question cards — a tap on one is answered
// honestly, never silently dropped. No picker cache is kept
// anywhere: every tap re-reads the live state (current provider,
// priced models, cost warning) from the app side, so a stale card
// can never switch to a provider that has since been removed or
// repriced.
//
// The handler is messaging-owned (like the question transport):
// the app side is a struct of small funcs (ModelPickerDeps) wired
// once in cmd/oriond, so messaging never imports app. The picker
// switches the chat that opened it (per-chat, persisted — the same
// scope as a typed /model <route>); --once and --global stay
// typed-only.
//
// Wire format (prefix "mdl:", registered on the callback router):
//
//	mdl:p:<provider>:<requester>  provider chosen -> model list
//	mdl:m:<route>:<requester>     model chosen -> cost check / switch
//	mdl:y:<route>:<requester>     confirm an expensive switch
//	mdl:x:<requester>             cancel
//	mdl:b:<requester>             back to the provider list
//	mdl:r:<requester>             reset to the daemon default
//
// The requester is the user id that sent /model (from ctx at show
// time); a tap is allowed only for the requester or the owner, the
// same rule as question taps. The requester is the digits after
// the LAST ":" — a route may itself contain ":" (an Ollama tag
// such as llama3.1:latest) and "/" (a vendor/model id), so the
// split is last-colon, never first-colon.
//
// Routes: a model button carries the /model route that selects it.
// ResolveRoute rejects a model part containing "/", so a
// vendor/model id (openrouter's deepseek/deepseek-chat) cannot
// travel in a route: it is addressable only as its provider's
// configured model, i.e. the bare provider route — and only when
// it IS that model. Models with no valid route are not offered.
//
// A repeated tap re-runs the same step: the picker keeps no
// consumed set, so a double-tap on "Switch anyway" switches
// twice. That is safe because switching is idempotent — the same
// route twice leaves the same provider installed, at the cost of
// one redundant agent rebuild — and the stale card's Cancel still
// reports the true current route.
package messaging

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/irfan/orion/internal/tools"
)

// modelPickerCallbackPrefix is the callback_data prefix the picker
// owns on the callback router. It must end with ":", be at most
// 16 bytes and not collide with another prefix (the router
// enforces all three at registration).
const modelPickerCallbackPrefix = "mdl:"

// ModelPickerDeps is the app-side surface the model picker needs.
// Every field is a small func so messaging never imports app (the
// daemon wires both). A nil field is a wiring gap: the picker
// answers "not fully wired" instead of panicking — except
// ModelInfo, which Show needs to send anything at all, so Show
// returns a wiring error for it.
type ModelPickerDeps struct {
	// ModelInfo returns the chat's current provider/model and
	// the known provider names.
	ModelInfo func(chatID int64) (provider, model string, providers []string)
	// ModelsFor returns the priced models for a provider (the
	// cost table's list): the models the picker may offer.
	ModelsFor func(provider string) []string
	// ConfiguredModel returns the provider's configured model
	// (its config entry), for the slashed-model route rule
	// (see modelPickerRoute).
	ConfiguredModel func(provider string) (string, bool)
	// Detail renders the alias/fallback lines for the picker
	// message (App.ModelDetail). Nil means no detail block.
	Detail func() string
	// CostWarning returns the cost warning for a route ("" =
	// no warning), or an error when the route cannot be
	// resolved. It is the same gate the /model text command
	// applies, so the two can never drift.
	CostWarning func(chatID int64, route string) (string, error)
	// Switch switches the chat to a route (per-chat, persisted).
	Switch func(chatID int64, route string) error
	// Reset clears the chat's provider override.
	Reset func(chatID int64) error
}

// ModelPicker is the /model button picker: it sends the provider
// step (Show) and resolves taps (HandleCallback, via the callback
// router under "mdl:"). It holds no per-picker state.
type ModelPicker struct {
	deps      ModelPickerDeps
	ownerID   int64
	logPrefix string
}

// NewModelPicker builds a picker over deps. ownerID is the owner
// chat/user id (0 = no owner: only the requester may tap).
func NewModelPicker(deps ModelPickerDeps, ownerID int64, logPrefix string) *ModelPicker {
	return &ModelPicker{deps: deps, ownerID: ownerID, logPrefix: logPrefix}
}

// CallbackPrefix is the callback_data prefix this handler owns.
func (p *ModelPicker) CallbackPrefix() string { return modelPickerCallbackPrefix }

// modelPickerTap is one parsed picker tap.
type modelPickerTap struct {
	step      string // p, m, y, x, b, r
	payload   string // provider (p), route (m/y), "" (x/b/r)
	requester int64
}

// parseModelPickerCallback parses picker callback_data. The
// requester is the digits after the LAST ":" — a route may itself
// contain ":" (an Ollama tag) and "/" (a vendor/model id), so a
// first-colon split would cut inside the route.
func parseModelPickerCallback(data string) (modelPickerTap, bool) {
	var tap modelPickerTap
	if !strings.HasPrefix(data, modelPickerCallbackPrefix) {
		return tap, false
	}
	rest := data[len(modelPickerCallbackPrefix):]
	step, tail, ok := strings.Cut(rest, ":")
	if !ok || step == "" {
		return tap, false
	}
	switch step {
	case "p", "m", "y", "x", "b", "r":
	default:
		return tap, false
	}
	payload, requesterStr := "", tail
	if i := strings.LastIndex(tail, ":"); i >= 0 {
		payload, requesterStr = tail[:i], tail[i+1:]
	}
	req, err := strconv.ParseInt(requesterStr, 10, 64)
	if err != nil || requesterStr == "" || req < 0 {
		return tap, false
	}
	tap.step, tap.payload, tap.requester = step, payload, req
	switch step {
	case "p":
		// A provider name carries no "/" or ":".
		if payload == "" || strings.ContainsAny(payload, "/:") {
			return tap, false
		}
	case "m", "y":
		if payload == "" {
			return tap, false
		}
	case "x", "b", "r":
		if payload != "" {
			return tap, false
		}
	}
	return tap, true
}

// modelPickerRoute returns the /model route selecting model on
// provider, or "" when the model cannot be addressed by a route.
// ResolveRoute rejects a model part containing "/", so a
// vendor/model id (openrouter's deepseek/deepseek-chat) is
// addressable only as its provider's configured model — the bare
// provider route — and only when it IS that model.
func modelPickerRoute(provider, model, configuredModel string, configuredOK bool) string {
	if provider == "" || model == "" {
		return ""
	}
	if strings.Contains(model, "/") {
		if !configuredOK || model != configuredModel {
			return ""
		}
		return provider
	}
	return provider + "/" + model
}

// modelPickerData builds one button's callback_data.
func modelPickerData(step, payload string, requester int64) string {
	if payload == "" {
		return modelPickerCallbackPrefix + step + ":" + strconv.FormatInt(requester, 10)
	}
	return modelPickerCallbackPrefix + step + ":" + payload + ":" + strconv.FormatInt(requester, 10)
}

// modelPickerChoice is one model button: the label shown and the
// /model route it selects.
type modelPickerChoice struct {
	label string
	route string
}

// modelPickerChoices builds the model buttons for provider from
// the priced models: each model becomes its route, except a
// slashed model id, which is offered only as the bare provider
// route when it is the configured model — and skipped otherwise.
// The current model is NOT filtered out: re-tapping it is a
// harmless no-op switch, and hiding it would make the list lie
// about what exists.
func modelPickerChoices(provider string, models []string, configuredModel string, configuredOK bool) []modelPickerChoice {
	var out []modelPickerChoice
	for _, m := range models {
		route := modelPickerRoute(provider, m, configuredModel, configuredOK)
		if route == "" {
			continue
		}
		out = append(out, modelPickerChoice{label: m, route: route})
	}
	return out
}

// modelPickerProviderKeyboard is the provider step: one button per
// provider (the current one marked ✓), then Reset and Cancel.
func modelPickerProviderKeyboard(providers []string, current string, requester int64) tools.InlineKeyboard {
	rows := make([][]tools.InlineButton, 0, len(providers)+1)
	for _, p := range providers {
		text := p
		if p == current {
			text = "✓ " + p
		}
		rows = append(rows, []tools.InlineButton{{Text: text, CallbackData: modelPickerData("p", p, requester)}})
	}
	rows = append(rows, []tools.InlineButton{
		{Text: "↩ Reset to default", CallbackData: modelPickerData("r", "", requester)},
		{Text: "✕ Cancel", CallbackData: modelPickerData("x", "", requester)},
	})
	return tools.InlineKeyboard{Rows: rows}
}

// modelPickerModelKeyboard is the model step: one button per model
// (the current one marked ✓), then Back and Cancel.
func modelPickerModelKeyboard(provider string, choices []modelPickerChoice, currentModel string, requester int64) tools.InlineKeyboard {
	rows := make([][]tools.InlineButton, 0, len(choices)+1)
	for _, c := range choices {
		text := c.label
		if c.label == currentModel {
			text = "✓ " + c.label
		}
		rows = append(rows, []tools.InlineButton{{Text: text, CallbackData: modelPickerData("m", c.route, requester)}})
	}
	rows = append(rows, []tools.InlineButton{
		{Text: "← Providers", CallbackData: modelPickerData("b", "", requester)},
		{Text: "✕ Cancel", CallbackData: modelPickerData("x", "", requester)},
	})
	return tools.InlineKeyboard{Rows: rows}
}

// modelPickerConfirmKeyboard is the cost-confirm step: Switch
// anyway (the button form of --confirm) and Cancel.
func modelPickerConfirmKeyboard(route string, requester int64) tools.InlineKeyboard {
	return tools.InlineKeyboard{Rows: [][]tools.InlineButton{{
		{Text: "✅ Switch anyway", CallbackData: modelPickerData("y", route, requester)},
		{Text: "✕ Cancel", CallbackData: modelPickerData("x", "", requester)},
	}}}
}

// Show sends the provider step to chatID: the current
// provider/model, the alias/fallback detail when there is any,
// and one button per provider. The requester (the user that sent
// /model) is embedded in every button so only they — or the owner
// — can drive the picker.
func (p *ModelPicker) Show(ctx context.Context, gw tools.CallbackGateway, chatID, requesterID int64) error {
	if p == nil {
		return fmt.Errorf("messaging: model picker: nil picker")
	}
	if gw == nil {
		return fmt.Errorf("messaging: model picker: nil gateway")
	}
	if p.deps.ModelInfo == nil {
		return fmt.Errorf("messaging: model picker: ModelInfo is not wired")
	}
	curProvider, curModel, providers := p.deps.ModelInfo(chatID)
	if len(providers) == 0 {
		return fmt.Errorf("messaging: model picker: no providers known")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Current: %s / %s\n", curProvider, curModel)
	if p.deps.Detail != nil {
		if detail := strings.TrimSpace(p.deps.Detail()); detail != "" {
			b.WriteString("\n")
			b.WriteString(detail)
			b.WriteString("\n")
		}
	}
	b.WriteString("\nChoose a provider to switch to — then pick a model. A pricey switch asks for confirmation first.\nTip: /model <provider> still works typed; /model reset clears the override.")
	_, err := gw.SendKeyboard(ctx, chatID, b.String(), modelPickerProviderKeyboard(providers, curProvider, requesterID))
	return err
}

// canTap reports whether sender may drive a picker opened by
// requester: the requester, or the owner — the same rule as
// question taps. A zero requester (no sender recorded at show
// time) means nobody but the owner may tap; a zero owner means
// nobody but the requester may.
func (p *ModelPicker) canTap(senderID, requester int64) bool {
	if requester != 0 && senderID == requester {
		return true
	}
	if p.ownerID != 0 && senderID == p.ownerID {
		return true
	}
	return false
}

// currentInfo re-reads the chat's current provider/model live
// (never a cached copy): a tap must report the route the chat is
// actually on, including a switch that landed since the card was
// sent.
func (p *ModelPicker) currentInfo(chatID int64) (provider, model string, providers []string) {
	if p.deps.ModelInfo == nil {
		return "", "", nil
	}
	return p.deps.ModelInfo(chatID)
}

// HandleCallback resolves one picker tap.
func (p *ModelPicker) HandleCallback(ctx context.Context, gw tools.CallbackGateway, q tools.CallbackQuery) error {
	if p == nil {
		return fmt.Errorf("messaging: model picker: nil picker")
	}
	if gw == nil {
		return fmt.Errorf("messaging: model picker: nil gateway")
	}
	tap, ok := parseModelPickerCallback(q.CallbackData())
	if !ok {
		fmt.Fprintf(os.Stderr, "%s: model picker tap: unparseable callback data %q (query %s)\n", p.logPrefix, q.CallbackData(), q.CallbackID())
		return gw.AnswerCallback(ctx, q.CallbackID(), "Unknown button.")
	}
	senderID := q.SenderID()
	if !p.canTap(senderID, tap.requester) {
		fmt.Fprintf(os.Stderr, "%s: model picker tap: refused for sender %d (requester %d, owner %d)\n", p.logPrefix, senderID, tap.requester, p.ownerID)
		return gw.AnswerCallback(ctx, q.CallbackID(), "Only the person who opened this picker (or the owner) can use it.")
	}
	if !q.HasMessage() {
		// A tap with no message carries no chat to switch and
		// no card to update: refuse it rather than switching
		// chat 0.
		return gw.AnswerCallback(ctx, q.CallbackID(), "This button has no message to act on.")
	}
	chatID := q.MessageChatID()
	switch tap.step {
	case "p":
		return p.onProvider(ctx, gw, q, chatID, tap)
	case "m":
		return p.onModel(ctx, gw, q, chatID, tap)
	case "y":
		return p.doSwitch(ctx, gw, q, chatID, tap.payload)
	case "x":
		return p.onCancel(ctx, gw, q, chatID)
	case "b":
		return p.onBack(ctx, gw, q, chatID, tap)
	case "r":
		return p.onReset(ctx, gw, q, chatID)
	}
	return gw.AnswerCallback(ctx, q.CallbackID(), "Unknown button.")
}

// onProvider is the provider step: list the provider's models on
// a fresh message and take the keyboard off the provider list.
func (p *ModelPicker) onProvider(ctx context.Context, gw tools.CallbackGateway, q tools.CallbackQuery, chatID int64, tap modelPickerTap) error {
	provider := tap.payload
	if p.deps.ModelsFor == nil {
		return gw.AnswerCallback(ctx, q.CallbackID(), "The model picker is not fully wired.")
	}
	models := p.deps.ModelsFor(provider)
	configured, configuredOK := "", false
	if p.deps.ConfiguredModel != nil {
		configured, configuredOK = p.deps.ConfiguredModel(provider)
	}
	choices := modelPickerChoices(provider, models, configured, configuredOK)
	if len(choices) == 0 {
		return gw.AnswerCallback(ctx, q.CallbackID(), "No models known for "+provider+".")
	}
	_, curModel, _ := p.currentInfo(chatID)
	if err := gw.EditTextRemoveKeyboard(ctx, chatID, q.MessageID(), "Provider: "+escapeHTML(provider)+"\nChoose a model:"); err != nil {
		fmt.Fprintf(os.Stderr, "%s: model picker tap: provider step edit failed: %v\n", p.logPrefix, err)
	}
	if _, err := gw.SendKeyboard(ctx, chatID, "Choose a model for "+provider+":", modelPickerModelKeyboard(provider, choices, curModel, tap.requester)); err != nil {
		return err
	}
	return gw.AnswerCallback(ctx, q.CallbackID(), "Choose a model for "+provider+".")
}

// onModel is the model step: run the same cost gate the /model
// text command applies. A cheap switch happens at once; an
// expensive one asks for confirmation on a fresh message.
func (p *ModelPicker) onModel(ctx context.Context, gw tools.CallbackGateway, q tools.CallbackQuery, chatID int64, tap modelPickerTap) error {
	route := tap.payload
	if p.deps.CostWarning == nil {
		return gw.AnswerCallback(ctx, q.CallbackID(), "The model picker is not fully wired.")
	}
	warning, err := p.deps.CostWarning(chatID, route)
	if err != nil {
		// The route came from the picker's own buttons, so a
		// resolve failure here means the world changed under
		// a live card (provider removed, config reloaded): the
		// toast carries the reason, truncated to the toast cap.
		return gw.AnswerCallback(ctx, q.CallbackID(), truncateRunes("❌ "+err.Error(), 200))
	}
	if warning == "" {
		return p.doSwitch(ctx, gw, q, chatID, route)
	}
	if err := gw.EditTextRemoveKeyboard(ctx, chatID, q.MessageID(), "Switch to "+escapeHTML(route)+"\nConfirm below:"); err != nil {
		fmt.Fprintf(os.Stderr, "%s: model picker tap: confirm step edit failed: %v\n", p.logPrefix, err)
	}
	text := warning + "\n\nTap ✅ Switch anyway to confirm, or ✕ Cancel."
	if _, err := gw.SendKeyboard(ctx, chatID, text, modelPickerConfirmKeyboard(route, tap.requester)); err != nil {
		return err
	}
	return gw.AnswerCallback(ctx, q.CallbackID(), "This switch costs more — confirm it below.")
}

// doSwitch performs the switch and reports the outcome by editing
// the tapped message in place (keyboard removed) and answering
// the tap. It is shared by the model step (cheap switch) and the
// confirm step (expensive switch, already confirmed).
func (p *ModelPicker) doSwitch(ctx context.Context, gw tools.CallbackGateway, q tools.CallbackQuery, chatID int64, route string) error {
	if p.deps.Switch == nil {
		return gw.AnswerCallback(ctx, q.CallbackID(), "The model picker is not fully wired.")
	}
	if err := p.deps.Switch(chatID, route); err != nil {
		if err := gw.EditTextRemoveKeyboard(ctx, chatID, q.MessageID(), "❌ "+escapeHTML(err.Error())); err != nil {
			fmt.Fprintf(os.Stderr, "%s: model picker tap: switch error edit failed: %v\n", p.logPrefix, err)
		}
		return gw.AnswerCallback(ctx, q.CallbackID(), truncateRunes("Switch failed: "+err.Error(), 200))
	}
	provider, modelName, _ := p.currentInfo(chatID)
	text := "✅ Switched to " + route + ". This switches the provider for this chat. Other chats are unaffected."
	if provider != "" {
		text = "✅ Switched to " + provider + " / " + modelName + ". This switches the provider for this chat. Other chats are unaffected."
	}
	if err := gw.EditTextRemoveKeyboard(ctx, chatID, q.MessageID(), escapeHTML(text)); err != nil {
		fmt.Fprintf(os.Stderr, "%s: model picker tap: switch outcome edit failed: %v\n", p.logPrefix, err)
	}
	toast := "Switched to " + route + "."
	if provider != "" {
		toast = "Switched to " + provider + " / " + modelName + "."
	}
	return gw.AnswerCallback(ctx, q.CallbackID(), toast)
}

// onCancel reports the true current route and leaves it switched
// to nothing: the tap changes no state.
func (p *ModelPicker) onCancel(ctx context.Context, gw tools.CallbackGateway, q tools.CallbackQuery, chatID int64) error {
	provider, modelName, _ := p.currentInfo(chatID)
	text := "Cancelled."
	if provider != "" {
		text = "Cancelled — still on " + provider + " / " + modelName + "."
	}
	if err := gw.EditTextRemoveKeyboard(ctx, chatID, q.MessageID(), escapeHTML(text)); err != nil {
		fmt.Fprintf(os.Stderr, "%s: model picker tap: cancel edit failed: %v\n", p.logPrefix, err)
	}
	return gw.AnswerCallback(ctx, q.CallbackID(), "Cancelled.")
}

// onBack re-shows the provider step on a fresh message and takes
// the keyboard off the model list.
func (p *ModelPicker) onBack(ctx context.Context, gw tools.CallbackGateway, q tools.CallbackQuery, chatID int64, tap modelPickerTap) error {
	if err := gw.EditTextRemoveKeyboard(ctx, chatID, q.MessageID(), "Back to providers — choose below:"); err != nil {
		fmt.Fprintf(os.Stderr, "%s: model picker tap: back edit failed: %v\n", p.logPrefix, err)
	}
	if err := p.Show(ctx, gw, chatID, tap.requester); err != nil {
		return err
	}
	return gw.AnswerCallback(ctx, q.CallbackID(), "Choose a provider.")
}

// onReset clears the chat's override and reports the route the
// chat is actually on afterwards.
func (p *ModelPicker) onReset(ctx context.Context, gw tools.CallbackGateway, q tools.CallbackQuery, chatID int64) error {
	if p.deps.Reset == nil {
		return gw.AnswerCallback(ctx, q.CallbackID(), "The model picker is not fully wired.")
	}
	if err := p.deps.Reset(chatID); err != nil {
		if err := gw.EditTextRemoveKeyboard(ctx, chatID, q.MessageID(), "❌ "+escapeHTML(err.Error())); err != nil {
			fmt.Fprintf(os.Stderr, "%s: model picker tap: reset error edit failed: %v\n", p.logPrefix, err)
		}
		return gw.AnswerCallback(ctx, q.CallbackID(), truncateRunes("Reset failed: "+err.Error(), 200))
	}
	provider, modelName, _ := p.currentInfo(chatID)
	text := "✅ Provider reset to the daemon default."
	if provider != "" {
		text = "✅ Provider reset to the daemon default: " + provider + " / " + modelName + "."
	}
	if err := gw.EditTextRemoveKeyboard(ctx, chatID, q.MessageID(), escapeHTML(text)); err != nil {
		fmt.Fprintf(os.Stderr, "%s: model picker tap: reset outcome edit failed: %v\n", p.logPrefix, err)
	}
	return gw.AnswerCallback(ctx, q.CallbackID(), "Provider reset to the daemon default.")
}
