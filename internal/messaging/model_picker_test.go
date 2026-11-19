package messaging

import (
	"context"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/cost"
	"github.com/irfan/orion/internal/tools"
)

// --- recording gateway: records every picker send/edit/answer ----

type pickerSent struct {
	chatID   int64
	text     string
	keyboard tools.InlineKeyboard
}

type pickerEdit struct {
	chatID    int64
	messageID int64
	text      string
}

type recordingPickerGateway struct {
	sent    []pickerSent
	edits   []pickerEdit
	answers map[string]string
}

func (g *recordingPickerGateway) SendText(ctx context.Context, chatID int64, text string) (int64, error) {
	return 0, nil
}

func (g *recordingPickerGateway) SendKeyboard(ctx context.Context, chatID int64, text string, keyboard tools.InlineKeyboard) (int64, error) {
	g.sent = append(g.sent, pickerSent{chatID: chatID, text: text, keyboard: keyboard})
	return int64(len(g.sent)), nil
}

func (g *recordingPickerGateway) EditTextRemoveKeyboard(ctx context.Context, chatID, messageID int64, htmlText string) error {
	g.edits = append(g.edits, pickerEdit{chatID: chatID, messageID: messageID, text: htmlText})
	return nil
}

func (g *recordingPickerGateway) AnswerCallback(ctx context.Context, callbackID, text string) error {
	if g.answers == nil {
		g.answers = map[string]string{}
	}
	g.answers[callbackID] = text
	return nil
}

func (g *recordingPickerGateway) lastSent() pickerSent { return g.sent[len(g.sent)-1] }

// --- stub deps ------------------------------------------------------

type pickerStub struct {
	current    [2]string // provider, model
	providers  []string
	models     map[string][]string
	configured map[string]string
	detail     string
	warnings   map[string]string // route -> warning ("" = none)
	warnErrs   map[string]error  // route -> resolve error
	switches   []string
	resets     []int64
	switchErr  error
	resetErr   error
}

func testPickerStub() *pickerStub {
	return &pickerStub{
		current:   [2]string{"deepseek", "deepseek-flash"},
		providers: []string{"anthropic", "deepseek", "ollama", "openai", "openrouter"},
		models: map[string][]string{
			"deepseek":   {"deepseek-chat", "deepseek-flash", "deepseek-reasoner"},
			"openai":     {"gpt-4o", "gpt-4o-mini"},
			"anthropic":  {"claude-sonnet-4-5"},
			"ollama":     {"llama3.1"},
			"openrouter": {"deepseek/deepseek-chat"},
		},
		configured: map[string]string{
			"deepseek":   "deepseek-flash",
			"openrouter": "deepseek/deepseek-chat",
		},
		detail:   "Fallback chain: deepseek -> openai",
		warnings: map[string]string{},
		warnErrs: map[string]error{},
	}
}

func (s *pickerStub) deps() ModelPickerDeps {
	return ModelPickerDeps{
		ModelInfo: func(chatID int64) (string, string, []string) {
			return s.current[0], s.current[1], s.providers
		},
		ModelsFor: func(provider string) []string { return s.models[provider] },
		ConfiguredModel: func(provider string) (string, bool) {
			m, ok := s.configured[provider]
			return m, ok
		},
		Detail: func() string { return s.detail },
		CostWarning: func(chatID int64, route string) (string, error) {
			if err, ok := s.warnErrs[route]; ok {
				return "", err
			}
			return s.warnings[route], nil
		},
		Switch: func(chatID int64, route string) error {
			if s.switchErr != nil {
				return s.switchErr
			}
			s.switches = append(s.switches, route)
			// The stub tracks the new current route like the
			// app would: provider/model split on the first "/".
			if i := strings.Index(route, "/"); i >= 0 {
				s.current = [2]string{route[:i], route[i+1:]}
			} else {
				s.current = [2]string{route, s.configured[route]}
			}
			return nil
		},
		Reset: func(chatID int64) error {
			if s.resetErr != nil {
				return s.resetErr
			}
			s.resets = append(s.resets, chatID)
			s.current = [2]string{"deepseek", "deepseek-flash"}
			return nil
		},
	}
}

func testPickerQuery(id, data string, sender, chatID, messageID int64) TGCallbackQuery {
	return TGCallbackQuery{
		ID:   id,
		Data: data,
		From: &TGUser{ID: sender},
		Message: &TGMessage{
			MessageID: messageID,
			Chat:      TGChat{ID: chatID},
		},
	}
}

func newTestPicker(stub *pickerStub, ownerID int64) *ModelPicker {
	return NewModelPicker(stub.deps(), ownerID, "test")
}

// --- parsing ---------------------------------------------------------

func TestParseModelPickerCallback(t *testing.T) {
	cases := []struct {
		data     string
		wantStep string
		wantPay  string
		wantReq  int64
		wantOK   bool
	}{
		{"mdl:p:deepseek:7429947930", "p", "deepseek", 7429947930, true},
		{"mdl:m:deepseek/deepseek-chat:7429947930", "m", "deepseek/deepseek-chat", 7429947930, true},
		{"mdl:m:openrouter:7429947930", "m", "openrouter", 7429947930, true},
		// An Ollama tag carries ":" inside the route: the
		// requester is the digits after the LAST colon.
		{"mdl:m:ollama/llama3.1:latest:7429947930", "m", "ollama/llama3.1:latest", 7429947930, true},
		{"mdl:y:anthropic/claude-sonnet-4-5:7429947930", "y", "anthropic/claude-sonnet-4-5", 7429947930, true},
		{"mdl:x:7429947930", "x", "", 7429947930, true},
		{"mdl:b:7429947930", "b", "", 7429947930, true},
		{"mdl:r:7429947930", "r", "", 7429947930, true},
		// Requester 0 (no sender recorded) parses: the sender
		// check fails closed later, the parse must not.
		{"mdl:p:deepseek:0", "p", "deepseek", 0, true},
		{"ask:abc:0", "", "", 0, false},
		{"mdl:z:deepseek:1", "", "", 0, false},
		{"mdl:p:deepseek", "", "", 0, false},     // no requester
		{"mdl:p:deepseek:abc", "", "", 0, false}, // non-digit requester
		{"mdl:p:deepseek:", "", "", 0, false},    // empty requester
		{"mdl:p::1", "", "", 0, false},           // empty provider
		{"mdl:p:deep/seek:1", "", "", 0, false},  // provider with slash
		{"mdl:m::1", "", "", 0, false},           // empty route
		{"mdl:x:deepseek:1", "", "", 0, false},   // payload on cancel
		{"mdl:p:deepseek:-1", "", "", 0, false},  // negative requester
		{"mdl:", "", "", 0, false},
		{"", "", "", 0, false},
	}
	for _, tc := range cases {
		tap, ok := parseModelPickerCallback(tc.data)
		if ok != tc.wantOK {
			t.Errorf("parse %q: ok = %v, want %v", tc.data, ok, tc.wantOK)
			continue
		}
		if !tc.wantOK {
			continue
		}
		if tap.step != tc.wantStep || tap.payload != tc.wantPay || tap.requester != tc.wantReq {
			t.Errorf("parse %q = %+v, want step %q payload %q requester %d", tc.data, tap, tc.wantStep, tc.wantPay, tc.wantReq)
		}
	}
}

// --- routes ----------------------------------------------------------

func TestModelPickerRoute(t *testing.T) {
	cases := []struct {
		provider, model, configured string
		configuredOK                bool
		want                        string
	}{
		{"deepseek", "deepseek-chat", "deepseek-flash", true, "deepseek/deepseek-chat"},
		{"openai", "gpt-4o-mini", "", false, "openai/gpt-4o-mini"},
		// A slashed model id is the bare provider route, and
		// only when it IS the configured model.
		{"openrouter", "deepseek/deepseek-chat", "deepseek/deepseek-chat", true, "openrouter"},
		{"openrouter", "deepseek/deepseek-chat", "deepseek/deepseek-chat-v3", true, ""},
		{"openrouter", "deepseek/deepseek-chat", "", false, ""},
		{"", "deepseek-chat", "", false, ""},
		{"deepseek", "", "", false, ""},
	}
	for _, tc := range cases {
		if got := modelPickerRoute(tc.provider, tc.model, tc.configured, tc.configuredOK); got != tc.want {
			t.Errorf("route(%q, %q) = %q, want %q", tc.provider, tc.model, got, tc.want)
		}
	}
}

func TestModelPickerChoices(t *testing.T) {
	choices := modelPickerChoices("openrouter", []string{"deepseek/deepseek-chat"}, "deepseek/deepseek-chat", true)
	if len(choices) != 1 || choices[0].label != "deepseek/deepseek-chat" || choices[0].route != "openrouter" {
		t.Fatalf("openrouter choices = %+v, want one {deepseek/deepseek-chat, openrouter}", choices)
	}
	// A slashed model that is NOT the configured model is not
	// offered: the picker must never build a route ResolveRoute
	// would reject.
	choices = modelPickerChoices("openrouter", []string{"deepseek/deepseek-chat"}, "other/model", true)
	if len(choices) != 0 {
		t.Errorf("unconfigured slashed model offered: %+v", choices)
	}
	choices = modelPickerChoices("deepseek", []string{"deepseek-chat", "deepseek-flash"}, "deepseek-flash", true)
	if len(choices) != 2 || choices[0].route != "deepseek/deepseek-chat" || choices[1].route != "deepseek/deepseek-flash" {
		t.Errorf("deepseek choices = %+v, want provider/model routes", choices)
	}
}

// --- Show ------------------------------------------------------------

func TestModelPickerShow(t *testing.T) {
	stub := testPickerStub()
	picker := newTestPicker(stub, 77)
	gw := &recordingPickerGateway{}
	if err := picker.Show(context.Background(), gw, 111, 7429947930); err != nil {
		t.Fatalf("Show: %v", err)
	}
	if len(gw.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(gw.sent))
	}
	sent := gw.sent[0]
	if sent.chatID != 111 {
		t.Errorf("sent to chat %d, want 111", sent.chatID)
	}
	if !strings.Contains(sent.text, "Current: deepseek / deepseek-flash") {
		t.Errorf("picker text missing current route:\n%s", sent.text)
	}
	if !strings.Contains(sent.text, "Fallback chain: deepseek -> openai") {
		t.Errorf("picker text missing detail block:\n%s", sent.text)
	}
	// One button per provider + one row (Reset, Cancel).
	if len(sent.keyboard.Rows) != len(stub.providers)+1 {
		t.Fatalf("keyboard rows = %d, want %d", len(sent.keyboard.Rows), len(stub.providers)+1)
	}
	// The current provider is marked; every button carries the
	// requester and stays under Telegram's 64-byte cap.
	for i, row := range sent.keyboard.Rows {
		for _, b := range row {
			if !strings.HasSuffix(b.CallbackData, ":7429947930") {
				t.Errorf("button %q data %q missing requester", b.Text, b.CallbackData)
			}
			if len(b.CallbackData) > 64 {
				t.Errorf("button %q data %d bytes, over the 64-byte cap", b.Text, len(b.CallbackData))
			}
		}
		if i < len(stub.providers) && stub.providers[i] == "deepseek" && row[0].Text != "✓ deepseek" {
			t.Errorf("current provider button = %q, want \"✓ deepseek\"", row[0].Text)
		}
	}
	if sent.keyboard.Rows[1][0].Text != "✓ deepseek" {
		t.Errorf("current provider button = %q, want \"✓ deepseek\"", sent.keyboard.Rows[1][0].Text)
	}
	last := sent.keyboard.Rows[len(sent.keyboard.Rows)-1]
	if len(last) != 2 || last[0].Text != "↩ Reset to default" || last[1].Text != "✕ Cancel" {
		t.Errorf("last row = %+v, want Reset + Cancel", last)
	}
}

// Every button the picker can ever build — for every priced model
// of every provider — stays under Telegram's 64-byte callback_data
// cap. A future long model name must fail this test, not a live tap.
func TestModelPickerCallbackDataUnderCap(t *testing.T) {
	tbl := cost.DefaultTable()
	requester := int64(7429947930)
	for _, provider := range []string{"anthropic", "deepseek", "ollama", "openai", "openrouter"} {
		models := tbl.Models(provider)
		configured := ""
		configuredOK := false
		if provider == "openrouter" {
			configured, configuredOK = "deepseek/deepseek-chat", true
		}
		for _, b := range modelPickerProviderKeyboard([]string{provider}, "", requester).Rows {
			for _, btn := range b {
				if len(btn.CallbackData) > 64 {
					t.Errorf("provider button %q data %d bytes", btn.Text, len(btn.CallbackData))
				}
			}
		}
		for _, c := range modelPickerChoices(provider, models, configured, configuredOK) {
			data := modelPickerData("m", c.route, requester)
			if len(data) > 64 {
				t.Errorf("model button %q data %q %d bytes, over cap", c.label, data, len(data))
			}
			data = modelPickerData("y", c.route, requester)
			if len(data) > 64 {
				t.Errorf("confirm button %q data %q %d bytes, over cap", c.label, data, len(data))
			}
		}
	}
}

// --- full flows ------------------------------------------------------

func TestModelPickerCheapSwitchFlow(t *testing.T) {
	stub := testPickerStub()
	picker := newTestPicker(stub, 77)
	gw := &recordingPickerGateway{}
	ctx := context.Background()
	// Step 1: /model shows the picker.
	if err := picker.Show(ctx, gw, 111, 7429947930); err != nil {
		t.Fatalf("Show: %v", err)
	}
	// Step 2: tap deepseek -> model list on a fresh message,
	// keyboard off the provider list.
	q := testPickerQuery("q1", "mdl:p:deepseek:7429947930", 7429947930, 111, 1)
	if err := picker.HandleCallback(ctx, gw, q); err != nil {
		t.Fatalf("provider tap: %v", err)
	}
	if len(gw.sent) != 2 || !strings.Contains(gw.sent[1].text, "Choose a model for deepseek") {
		t.Fatalf("after provider tap, sent = %+v, want a model list message", gw.sent)
	}
	if len(gw.edits) != 1 || !strings.Contains(gw.edits[0].text, "Provider: deepseek") {
		t.Fatalf("edits = %+v, want the provider message edited", gw.edits)
	}
	modelRows := gw.sent[1].keyboard.Rows
	if len(modelRows) != 4 { // 3 models + Back/Cancel row
		t.Fatalf("model keyboard rows = %d, want 4", len(modelRows))
	}
	if modelRows[1][0].Text != "✓ deepseek-flash" {
		t.Errorf("current model button = %q, want \"✓ deepseek-flash\"", modelRows[1][0].Text)
	}
	// Step 3: tap deepseek-chat (no warning) -> switched at once.
	q = testPickerQuery("q2", "mdl:m:deepseek/deepseek-chat:7429947930", 7429947930, 111, 2)
	if err := picker.HandleCallback(ctx, gw, q); err != nil {
		t.Fatalf("model tap: %v", err)
	}
	if len(stub.switches) != 1 || stub.switches[0] != "deepseek/deepseek-chat" {
		t.Fatalf("switches = %v, want [deepseek/deepseek-chat]", stub.switches)
	}
	if len(gw.edits) != 2 || !strings.Contains(gw.edits[1].text, "✅ Switched to deepseek / deepseek-chat") {
		t.Fatalf("edits = %+v, want the switch outcome edit", gw.edits)
	}
	if gw.answers["q2"] != "Switched to deepseek / deepseek-chat." {
		t.Errorf("toast q2 = %q, want the switched toast", gw.answers["q2"])
	}
	// No confirm message was ever sent for a cheap switch.
	if len(gw.sent) != 2 {
		t.Errorf("sent %d messages, want 2 (no confirm step for a cheap switch)", len(gw.sent))
	}
}

func TestModelPickerExpensiveSwitchNeedsConfirm(t *testing.T) {
	stub := testPickerStub()
	stub.warnings["anthropic/claude-sonnet-4-5"] = "switching from deepseek/deepseek-flash ($0.15/M input) to anthropic/claude-sonnet-4-5 ($3.00/M input) costs 20.0x more per input token — reply with --confirm to accept"
	picker := newTestPicker(stub, 77)
	gw := &recordingPickerGateway{}
	ctx := context.Background()
	// Tap the model: no switch yet, a confirm message instead.
	q := testPickerQuery("q1", "mdl:m:anthropic/claude-sonnet-4-5:7429947930", 7429947930, 111, 2)
	if err := picker.HandleCallback(ctx, gw, q); err != nil {
		t.Fatalf("model tap: %v", err)
	}
	if len(stub.switches) != 0 {
		t.Fatalf("switches = %v, want none before confirm", stub.switches)
	}
	if len(gw.sent) != 1 {
		t.Fatalf("sent %d messages, want the confirm message", len(gw.sent))
	}
	sent := gw.sent[0]
	if !strings.Contains(sent.text, "costs 20.0x more") || !strings.Contains(sent.text, "Switch anyway") {
		t.Errorf("confirm text missing warning/pointer:\n%s", sent.text)
	}
	confirmRow := sent.keyboard.Rows[0]
	if len(confirmRow) != 2 || confirmRow[0].Text != "✅ Switch anyway" || confirmRow[1].Text != "✕ Cancel" {
		t.Fatalf("confirm row = %+v, want Switch anyway + Cancel", confirmRow)
	}
	// Tap "Switch anyway": the switch happens now.
	q = testPickerQuery("q2", "mdl:y:anthropic/claude-sonnet-4-5:7429947930", 7429947930, 111, 3)
	if err := picker.HandleCallback(ctx, gw, q); err != nil {
		t.Fatalf("confirm tap: %v", err)
	}
	if len(stub.switches) != 1 || stub.switches[0] != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("switches = %v, want [anthropic/claude-sonnet-4-5]", stub.switches)
	}
	if !strings.Contains(gw.edits[len(gw.edits)-1].text, "✅ Switched to anthropic / claude-sonnet-4-5") {
		t.Errorf("last edit = %+v, want the switch outcome", gw.edits[len(gw.edits)-1])
	}
}

func TestModelPickerCancelLeavesRouteAlone(t *testing.T) {
	stub := testPickerStub()
	picker := newTestPicker(stub, 77)
	gw := &recordingPickerGateway{}
	ctx := context.Background()
	q := testPickerQuery("q1", "mdl:x:7429947930", 7429947930, 111, 1)
	if err := picker.HandleCallback(ctx, gw, q); err != nil {
		t.Fatalf("cancel tap: %v", err)
	}
	if len(stub.switches) != 0 || len(stub.resets) != 0 {
		t.Fatalf("cancel switched/reset: switches %v resets %v", stub.switches, stub.resets)
	}
	if len(gw.edits) != 1 || !strings.Contains(gw.edits[0].text, "Cancelled — still on deepseek / deepseek-flash") {
		t.Fatalf("edits = %+v, want the cancelled-still-on edit", gw.edits)
	}
	if gw.answers["q1"] != "Cancelled." {
		t.Errorf("toast q1 = %q, want \"Cancelled.\"", gw.answers["q1"])
	}
}

func TestModelPickerReset(t *testing.T) {
	stub := testPickerStub()
	stub.current = [2]string{"openai", "gpt-4o-mini"}
	picker := newTestPicker(stub, 77)
	gw := &recordingPickerGateway{}
	q := testPickerQuery("q1", "mdl:r:7429947930", 7429947930, 111, 1)
	if err := picker.HandleCallback(context.Background(), gw, q); err != nil {
		t.Fatalf("reset tap: %v", err)
	}
	if len(stub.resets) != 1 || stub.resets[0] != 111 {
		t.Fatalf("resets = %v, want [111]", stub.resets)
	}
	if !strings.Contains(gw.edits[0].text, "✅ Provider reset to the daemon default: deepseek / deepseek-flash") {
		t.Errorf("edit = %+v, want the reset outcome", gw.edits[0])
	}
}

func TestModelPickerBackReshowsProviders(t *testing.T) {
	stub := testPickerStub()
	picker := newTestPicker(stub, 77)
	gw := &recordingPickerGateway{}
	q := testPickerQuery("q1", "mdl:b:7429947930", 7429947930, 111, 2)
	if err := picker.HandleCallback(context.Background(), gw, q); err != nil {
		t.Fatalf("back tap: %v", err)
	}
	if len(gw.sent) != 1 || !strings.Contains(gw.sent[0].text, "Current: deepseek / deepseek-flash") {
		t.Fatalf("sent = %+v, want the provider list re-sent", gw.sent)
	}
	if len(gw.edits) != 1 || !strings.Contains(gw.edits[0].text, "Back to providers") {
		t.Fatalf("edits = %+v, want the model list edited away", gw.edits)
	}
}

func TestModelPickerOpenRouterSwitchesBare(t *testing.T) {
	// openrouter's only model is a slashed id: the model button
	// must switch the BARE provider route, never
	// "openrouter/deepseek/deepseek-chat" (ResolveRoute rejects it).
	stub := testPickerStub()
	picker := newTestPicker(stub, 77)
	gw := &recordingPickerGateway{}
	ctx := context.Background()
	q := testPickerQuery("q1", "mdl:p:openrouter:7429947930", 7429947930, 111, 1)
	if err := picker.HandleCallback(ctx, gw, q); err != nil {
		t.Fatalf("provider tap: %v", err)
	}
	rows := gw.sent[len(gw.sent)-1].keyboard.Rows
	if len(rows) != 2 || rows[0][0].Text != "deepseek/deepseek-chat" {
		t.Fatalf("openrouter model rows = %+v, want one model button", rows)
	}
	if rows[0][0].CallbackData != "mdl:m:openrouter:7429947930" {
		t.Errorf("openrouter model data = %q, want the bare provider route", rows[0][0].CallbackData)
	}
	q = testPickerQuery("q2", "mdl:m:openrouter:7429947930", 7429947930, 111, 2)
	if err := picker.HandleCallback(ctx, gw, q); err != nil {
		t.Fatalf("model tap: %v", err)
	}
	if len(stub.switches) != 1 || stub.switches[0] != "openrouter" {
		t.Fatalf("switches = %v, want [openrouter]", stub.switches)
	}
}

// --- sender checks ---------------------------------------------------

func TestModelPickerWrongSenderRefused(t *testing.T) {
	stub := testPickerStub()
	picker := newTestPicker(stub, 77) // owner 77, requester 7429947930
	gw := &recordingPickerGateway{}
	// A third user taps: refused, nothing switches, nothing sent.
	q := testPickerQuery("q1", "mdl:p:deepseek:7429947930", 999, 111, 1)
	if err := picker.HandleCallback(context.Background(), gw, q); err != nil {
		t.Fatalf("tap: %v", err)
	}
	if gw.answers["q1"] != "Only the person who opened this picker (or the owner) can use it." {
		t.Errorf("toast = %q, want the refusal", gw.answers["q1"])
	}
	if len(gw.sent) != 0 || len(stub.switches) != 0 {
		t.Errorf("refused tap sent %d messages and switched %v", len(gw.sent), stub.switches)
	}
}

func TestModelPickerOwnerCanTap(t *testing.T) {
	stub := testPickerStub()
	picker := newTestPicker(stub, 77)
	gw := &recordingPickerGateway{}
	q := testPickerQuery("q1", "mdl:p:deepseek:7429947930", 77, 111, 1)
	if err := picker.HandleCallback(context.Background(), gw, q); err != nil {
		t.Fatalf("owner tap: %v", err)
	}
	if len(gw.sent) != 1 {
		t.Errorf("owner tap sent %d messages, want the model list", len(gw.sent))
	}
}

func TestModelPickerUnparseableTap(t *testing.T) {
	stub := testPickerStub()
	picker := newTestPicker(stub, 77)
	gw := &recordingPickerGateway{}
	q := testPickerQuery("q1", "mdl:???", 7429947930, 111, 1)
	if err := picker.HandleCallback(context.Background(), gw, q); err != nil {
		t.Fatalf("tap: %v", err)
	}
	if gw.answers["q1"] != "Unknown button." {
		t.Errorf("toast = %q, want \"Unknown button.\"", gw.answers["q1"])
	}
}

func TestModelPickerTapWithoutMessageRefused(t *testing.T) {
	stub := testPickerStub()
	picker := newTestPicker(stub, 77)
	gw := &recordingPickerGateway{}
	q := TGCallbackQuery{ID: "q1", Data: "mdl:p:deepseek:7429947930", From: &TGUser{ID: 7429947930}}
	if err := picker.HandleCallback(context.Background(), gw, q); err != nil {
		t.Fatalf("tap: %v", err)
	}
	if gw.answers["q1"] != "This button has no message to act on." {
		t.Errorf("toast = %q, want the no-message refusal", gw.answers["q1"])
	}
	if len(stub.switches) != 0 {
		t.Errorf("message-less tap switched %v", stub.switches)
	}
}

// --- /model no-args dispatches to the picker -------------------------

func TestModelNoArgsShowsPickerWhenWired(t *testing.T) {
	stub := testPickerStub()
	picker := newTestPicker(stub, 77)
	gw := &recordingPickerGateway{}
	env := &CommandEnv{
		ChatID:  111,
		OwnerID: 77,
		ModelInfo: func() (string, string, []string) {
			return stub.current[0], stub.current[1], stub.providers
		},
		ShowModelPicker: func(ctx context.Context, chatID int64) error {
			senderID, _ := SenderIDFromContext(ctx)
			return picker.Show(ctx, gw, chatID, senderID)
		},
	}
	ctx := WithSenderID(context.Background(), 7429947930)
	r := DefaultRegistry(env)
	reply, handled, err := r.Dispatch(ctx, env, "/model")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !handled {
		t.Fatal("/model was not handled")
	}
	// The picker message IS the reply: Dispatch returns "" (an
	// empty reply sends nothing) and the keyboard went out.
	if reply != "" {
		t.Errorf("reply = %q, want empty (the picker message is the reply)", reply)
	}
	if len(gw.sent) != 1 || !strings.Contains(gw.sent[0].text, "Current: deepseek / deepseek-flash") {
		t.Fatalf("sent = %+v, want the picker message", gw.sent)
	}
	if !strings.HasSuffix(gw.sent[0].keyboard.Rows[0][0].CallbackData, ":7429947930") {
		t.Errorf("picker buttons carry the wrong requester: %+v", gw.sent[0].keyboard.Rows[0])
	}
}
