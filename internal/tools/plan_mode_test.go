package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/permissions"
)

// --- fakes ---------------------------------------------------------------

type planFakeSend struct {
	chatID   int64
	text     string
	keyboard InlineKeyboard
}

type planFakeEdit struct {
	chatID int64
	msgID  int64
	html   string
}

type planFakeAnswer struct {
	callbackID string
	text       string
}

// planFakeGateway records everything the plan tool sends through
// the transport. Thread-safe: Execute blocks in one goroutine
// while the test drives decisions from another.
type planFakeGateway struct {
	mu        sync.Mutex
	sends     []planFakeSend
	edits     []planFakeEdit
	answers   []planFakeAnswer
	nextMsgID int64
	sendErr   error
}

func (f *planFakeGateway) SendText(ctx context.Context, chatID int64, text string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextMsgID++
	f.sends = append(f.sends, planFakeSend{chatID: chatID, text: text})
	return f.nextMsgID, nil
}

func (f *planFakeGateway) SendKeyboard(ctx context.Context, chatID int64, text string, keyboard InlineKeyboard) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	f.nextMsgID++
	f.sends = append(f.sends, planFakeSend{chatID: chatID, text: text, keyboard: keyboard})
	return f.nextMsgID, nil
}

func (f *planFakeGateway) EditTextRemoveKeyboard(ctx context.Context, chatID, messageID int64, htmlText string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, planFakeEdit{chatID: chatID, msgID: messageID, html: htmlText})
	return nil
}

func (f *planFakeGateway) AnswerCallback(ctx context.Context, callbackID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, planFakeAnswer{callbackID: callbackID, text: text})
	return nil
}

func (f *planFakeGateway) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

func (f *planFakeGateway) lastSend() planFakeSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sends[len(f.sends)-1]
}

func (f *planFakeGateway) lastEdit() planFakeEdit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.edits[len(f.edits)-1]
}

func (f *planFakeGateway) lastAnswer() planFakeAnswer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.answers[len(f.answers)-1]
}

func (f *planFakeGateway) editCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.edits)
}

type planFakeCallbackQuery struct {
	data   string
	id     string
	sender int64
	chat   int64
	msg    int64
}

func (q planFakeCallbackQuery) CallbackData() string { return q.data }
func (q planFakeCallbackQuery) CallbackID() string   { return q.id }
func (q planFakeCallbackQuery) SenderID() int64      { return q.sender }
func (q planFakeCallbackQuery) HasMessage() bool     { return q.msg != 0 }
func (q planFakeCallbackQuery) MessageChatID() int64 { return q.chat }
func (q planFakeCallbackQuery) MessageID() int64     { return q.msg }

type planFakeReplyUpdate struct {
	chat    int64
	sender  int64
	text    string
	command bool
}

func (u planFakeReplyUpdate) ChatID() int64   { return u.chat }
func (u planFakeReplyUpdate) SenderID() int64 { return u.sender }
func (u planFakeReplyUpdate) Text() string    { return u.text }
func (u planFakeReplyUpdate) IsCommand() bool { return u.command }

// --- helpers -------------------------------------------------------------

// planTestEnv is a plan-mode tool pair built the real way (via
// the factories in a DefaultRegistryWithDeps), with a fake
// gateway installed as the transport. Requester 100, owner 999,
// chat 100 unless a test says otherwise.
type planTestEnv struct {
	reg   *Registry
	enter *enterPlanModeTool
	exit  *exitPlanModeTool
	gw    *planFakeGateway
	store *PlanningStore
}

func newPlanTestEnv(t *testing.T) *planTestEnv {
	t.Helper()
	store := NewPlanningStore("")
	reg, err := DefaultRegistryWithDeps(ToolDeps{PlanningStore: store})
	if err != nil {
		t.Fatalf("DefaultRegistryWithDeps: %v", err)
	}
	enterTool, ok := reg.Get("enter_plan_mode")
	if !ok {
		t.Fatal("registry has no enter_plan_mode")
	}
	enter, ok := enterTool.(*enterPlanModeTool)
	if !ok {
		t.Fatalf("enter_plan_mode is %T, want *enterPlanModeTool", enterTool)
	}
	exitTool, ok := reg.Get("exit_plan_mode")
	if !ok {
		t.Fatal("registry has no exit_plan_mode")
	}
	exit, ok := exitTool.(*exitPlanModeTool)
	if !ok {
		t.Fatalf("exit_plan_mode is %T, want *exitPlanModeTool", exitTool)
	}
	gw := &planFakeGateway{nextMsgID: 41}
	if err := enter.InstallTransport(&ToolHost{Gateway: gw, OwnerID: 999}); err != nil {
		t.Fatalf("InstallTransport: %v", err)
	}
	return &planTestEnv{reg: reg, enter: enter, exit: exit, gw: gw, store: store}
}

func planCtx(chatID, senderID int64) context.Context {
	return permissions.WithApprovalContext(context.Background(), chatID, senderID)
}

func planArgs(goal string, steps []string, timeoutSeconds float64) json.RawMessage {
	m := map[string]any{"goal": goal, "steps": steps}
	if timeoutSeconds != 0 {
		m["timeout_seconds"] = timeoutSeconds
	}
	data, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return data
}

type enterResult struct {
	res string
	err error
}

func runEnter(t *testing.T, enter *enterPlanModeTool, ctx context.Context, args json.RawMessage) chan enterResult {
	t.Helper()
	ch := make(chan enterResult, 1)
	go func() {
		res, err := enter.Execute(ctx, args)
		ch <- enterResult{res: res, err: err}
	}()
	return ch
}

func waitPlanSend(t *testing.T, gw *planFakeGateway, timeout time.Duration) planFakeSend {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if gw.sendCount() > 0 {
			return gw.lastSend()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the plan message to be sent")
	return planFakeSend{}
}

func waitEnterResult(t *testing.T, ch chan enterResult, timeout time.Duration) enterResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for enter_plan_mode to return")
		return enterResult{}
	}
}

// planIDFromSend extracts the plan id from the Approve button's
// callback data.
func planIDFromSend(t *testing.T, s planFakeSend) string {
	t.Helper()
	for _, row := range s.keyboard.Rows {
		for _, b := range row {
			if d, id, ok := parsePlanCallback(b.CallbackData); ok && d == "approve" {
				return id
			}
		}
	}
	t.Fatalf("no approve button in the plan keyboard: %+v", s.keyboard)
	return ""
}

func decodePlanResult(t *testing.T, res string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		t.Fatalf("plan result %q is not JSON: %v", res, err)
	}
	return out
}

func (e *planTestEnv) storedPlan(t *testing.T, ctx context.Context) *planState {
	t.Helper()
	dir, key := e.store.resolveBucket(ctx)
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	plan, _, err := loadStoredPlan(e.store, dir, key)
	if err != nil {
		t.Fatalf("loadStoredPlan: %v", err)
	}
	return plan
}

// --- decisions -----------------------------------------------------------

func TestEnterPlanModeApproveViaCallback(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	ch := runEnter(t, env.enter, ctx, planArgs("Migrate the database", []string{"Back up the data", "Run the migration", "Verify the rows"}, 0))
	send := waitPlanSend(t, env.gw, 5*time.Second)
	if !strings.Contains(send.text, "Migrate the database") {
		t.Errorf("plan message %q does not contain the goal", send.text)
	}
	if !strings.Contains(send.text, "1. Back up the data") || !strings.Contains(send.text, "3. Verify the rows") {
		t.Errorf("plan message %q does not list the steps", send.text)
	}
	planID := planIDFromSend(t, send)
	if len(send.keyboard.Rows) != 3 {
		t.Errorf("plan keyboard has %d rows, want 3 (approve/reject/revise)", len(send.keyboard.Rows))
	}

	if err := env.enter.HandleCallback(ctx, env.gw, planFakeCallbackQuery{
		data: "plan:approve:" + planID, id: "cb1", sender: 100, chat: 100, msg: 42,
	}); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	if got := env.gw.lastAnswer(); got.text != "Plan approved." {
		t.Errorf("callback toast = %q, want %q", got.text, "Plan approved.")
	}
	r := waitEnterResult(t, ch, 5*time.Second)
	if r.err != nil {
		t.Fatalf("enter_plan_mode: %v", r.err)
	}
	out := decodePlanResult(t, r.res)
	if out["decision"] != "approved" {
		t.Errorf("decision = %v, want approved", out["decision"])
	}
	if out["plan_id"] != planID {
		t.Errorf("plan_id = %v, want %s", out["plan_id"], planID)
	}
	if out["goal"] != "Migrate the database" {
		t.Errorf("goal = %v, want the plan goal", out["goal"])
	}
	if got := env.storedPlan(t, ctx); got != nil {
		t.Errorf("stored plan after approval = %+v, want nil (cleared)", got)
	}
	if env.gw.editCount() != 1 {
		t.Fatalf("edits = %d, want 1", env.gw.editCount())
	}
	edit := env.gw.lastEdit()
	if edit.msgID != 42 || !strings.Contains(edit.html, "Approved") {
		t.Errorf("outcome edit = %+v, want the plan message edited to Approved", edit)
	}
}

func TestEnterPlanModeRejectViaCallback(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	ch := runEnter(t, env.enter, ctx, planArgs("Delete the old logs", []string{"List the files", "Delete them"}, 0))
	send := waitPlanSend(t, env.gw, 5*time.Second)
	planID := planIDFromSend(t, send)
	if err := env.enter.HandleCallback(ctx, env.gw, planFakeCallbackQuery{
		data: "plan:reject:" + planID, id: "cb1", sender: 100, chat: 100, msg: 42,
	}); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	r := waitEnterResult(t, ch, 5*time.Second)
	if r.err != nil {
		t.Fatalf("enter_plan_mode: %v", r.err)
	}
	if out := decodePlanResult(t, r.res); out["decision"] != "rejected" {
		t.Errorf("decision = %v, want rejected", out["decision"])
	}
	if got := env.storedPlan(t, ctx); got != nil {
		t.Errorf("stored plan after rejection = %+v, want nil", got)
	}
	if edit := env.gw.lastEdit(); !strings.Contains(edit.html, "Rejected") {
		t.Errorf("outcome edit %q does not say Rejected", edit.html)
	}
}

func TestEnterPlanModeReviseViaCallback(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	ch := runEnter(t, env.enter, ctx, planArgs("Refactor the cache", []string{"Extract the interface"}, 0))
	send := waitPlanSend(t, env.gw, 5*time.Second)
	planID := planIDFromSend(t, send)
	if err := env.enter.HandleCallback(ctx, env.gw, planFakeCallbackQuery{
		data: "plan:revise:" + planID, id: "cb1", sender: 100, chat: 100, msg: 42,
	}); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	r := waitEnterResult(t, ch, 5*time.Second)
	if r.err != nil {
		t.Fatalf("enter_plan_mode: %v", r.err)
	}
	out := decodePlanResult(t, r.res)
	if out["decision"] != "revise" {
		t.Errorf("decision = %v, want revise", out["decision"])
	}
	if _, ok := out["feedback"]; ok {
		t.Errorf("button revise carries feedback %v, want none (no typed text)", out["feedback"])
	}
	if edit := env.gw.lastEdit(); !strings.Contains(edit.html, "Revision requested") {
		t.Errorf("outcome edit %q does not say Revision requested", edit.html)
	}
}

func TestEnterPlanModeOwnerCanDecideViaCallback(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	ch := runEnter(t, env.enter, ctx, planArgs("Rotate the keys", []string{"Generate new keys"}, 0))
	send := waitPlanSend(t, env.gw, 5*time.Second)
	planID := planIDFromSend(t, send)
	// The owner (999) is not the requester (100) but may decide.
	if err := env.enter.HandleCallback(ctx, env.gw, planFakeCallbackQuery{
		data: "plan:approve:" + planID, id: "cb1", sender: 999, chat: 100, msg: 42,
	}); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	r := waitEnterResult(t, ch, 5*time.Second)
	if r.err != nil {
		t.Fatalf("enter_plan_mode: %v", r.err)
	}
	if out := decodePlanResult(t, r.res); out["decision"] != "approved" {
		t.Errorf("decision = %v, want approved (owner tapped)", out["decision"])
	}
}

func TestEnterPlanModeApproveViaTypedReply(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	ch := runEnter(t, env.enter, ctx, planArgs("Add a health check", []string{"Add the endpoint"}, 0))
	waitPlanSend(t, env.gw, 5*time.Second)
	if !env.enter.TryAnswer(planFakeReplyUpdate{chat: 100, sender: 100, text: "yes"}) {
		t.Fatal("TryAnswer(\"yes\"): not consumed, want the typed approval consumed")
	}
	r := waitEnterResult(t, ch, 5*time.Second)
	if r.err != nil {
		t.Fatalf("enter_plan_mode: %v", r.err)
	}
	if out := decodePlanResult(t, r.res); out["decision"] != "approved" {
		t.Errorf("decision = %v, want approved", out["decision"])
	}
}

func TestEnterPlanModeReviseViaTypedReply(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	ch := runEnter(t, env.enter, ctx, planArgs("Switch the database", []string{"Dump the data", "Import it"}, 0))
	waitPlanSend(t, env.gw, 5*time.Second)
	if !env.enter.TryAnswer(planFakeReplyUpdate{chat: 100, sender: 100, text: "use postgres instead of sqlite"}) {
		t.Fatal("TryAnswer(revision text): not consumed")
	}
	r := waitEnterResult(t, ch, 5*time.Second)
	if r.err != nil {
		t.Fatalf("enter_plan_mode: %v", r.err)
	}
	out := decodePlanResult(t, r.res)
	if out["decision"] != "revise" {
		t.Errorf("decision = %v, want revise", out["decision"])
	}
	if out["feedback"] != "use postgres instead of sqlite" {
		t.Errorf("feedback = %v, want the typed text", out["feedback"])
	}
}

func TestEnterPlanModeRejectViaTypedReply(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	ch := runEnter(t, env.enter, ctx, planArgs("Drop the table", []string{"Drop it"}, 0))
	waitPlanSend(t, env.gw, 5*time.Second)
	if !env.enter.TryAnswer(planFakeReplyUpdate{chat: 100, sender: 100, text: "no"}) {
		t.Fatal("TryAnswer(\"no\"): not consumed")
	}
	r := waitEnterResult(t, ch, 5*time.Second)
	if r.err != nil {
		t.Fatalf("enter_plan_mode: %v", r.err)
	}
	if out := decodePlanResult(t, r.res); out["decision"] != "rejected" {
		t.Errorf("decision = %v, want rejected", out["decision"])
	}
}

// --- interceptor discipline ----------------------------------------------

func TestEnterPlanModeStrangerTapIgnored(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	ch := runEnter(t, env.enter, ctx, planArgs("Wipe the cache", []string{"Flush it"}, 0))
	send := waitPlanSend(t, env.gw, 5*time.Second)
	planID := planIDFromSend(t, send)
	if err := env.enter.HandleCallback(ctx, env.gw, planFakeCallbackQuery{
		data: "plan:approve:" + planID, id: "cb1", sender: 777, chat: 100, msg: 42,
	}); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	if got := env.gw.lastAnswer(); got.text != "This plan can only be decided by the person who asked for it." {
		t.Errorf("stranger toast = %q, want the not-yours toast", got.text)
	}
	// The wait is still live: the requester can still decide.
	select {
	case r := <-ch:
		t.Fatalf("enter_plan_mode returned %v after a stranger tap, want it still waiting", r)
	default:
	}
	if err := env.enter.HandleCallback(ctx, env.gw, planFakeCallbackQuery{
		data: "plan:approve:" + planID, id: "cb2", sender: 100, chat: 100, msg: 42,
	}); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	r := waitEnterResult(t, ch, 5*time.Second)
	if r.err != nil {
		t.Fatalf("enter_plan_mode: %v", r.err)
	}
	if out := decodePlanResult(t, r.res); out["decision"] != "approved" {
		t.Errorf("decision = %v, want approved after the requester tapped", out["decision"])
	}
}

func TestEnterPlanModeStrangerTypedReplyIgnored(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	ch := runEnter(t, env.enter, ctx, planArgs("Wipe the cache", []string{"Flush it"}, 0))
	waitPlanSend(t, env.gw, 5*time.Second)
	if env.enter.TryAnswer(planFakeReplyUpdate{chat: 100, sender: 777, text: "yes"}) {
		t.Error("TryAnswer(stranger \"yes\"): consumed, want it ignored")
	}
	select {
	case r := <-ch:
		t.Fatalf("enter_plan_mode returned %v after a stranger reply, want it still waiting", r)
	default:
	}
	if !env.enter.TryAnswer(planFakeReplyUpdate{chat: 100, sender: 100, text: "yes"}) {
		t.Fatal("TryAnswer(requester \"yes\"): not consumed")
	}
	r := waitEnterResult(t, ch, 5*time.Second)
	if r.err != nil || decodePlanResult(t, r.res)["decision"] != "approved" {
		t.Errorf("result = %v / %v, want an approval", r.res, r.err)
	}
}

func TestEnterPlanModeCommandNotAnAnswer(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	ch := runEnter(t, env.enter, ctx, planArgs("Wipe the cache", []string{"Flush it"}, 0))
	waitPlanSend(t, env.gw, 5*time.Second)
	if env.enter.TryAnswer(planFakeReplyUpdate{chat: 100, sender: 100, text: "/start", command: true}) {
		t.Error("TryAnswer(\"/start\"): consumed, want commands never to be answers")
	}
	select {
	case r := <-ch:
		t.Fatalf("enter_plan_mode returned %v after a command, want it still waiting", r)
	default:
	}
	if !env.enter.TryAnswer(planFakeReplyUpdate{chat: 100, sender: 100, text: "approve"}) {
		t.Fatal("TryAnswer(\"approve\"): not consumed")
	}
	r := waitEnterResult(t, ch, 5*time.Second)
	if r.err != nil || decodePlanResult(t, r.res)["decision"] != "approved" {
		t.Errorf("result = %v / %v, want an approval", r.res, r.err)
	}
}

func TestEnterPlanModeOtherChatReplyIgnored(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	ch := runEnter(t, env.enter, ctx, planArgs("Wipe the cache", []string{"Flush it"}, 0))
	waitPlanSend(t, env.gw, 5*time.Second)
	if env.enter.TryAnswer(planFakeReplyUpdate{chat: 200, sender: 100, text: "yes"}) {
		t.Error("TryAnswer(other chat): consumed, want per-chat isolation")
	}
	select {
	case r := <-ch:
		t.Fatalf("enter_plan_mode returned %v after another chat's reply, want it still waiting", r)
	default:
	}
	if !env.enter.TryAnswer(planFakeReplyUpdate{chat: 100, sender: 100, text: "yes"}) {
		t.Fatal("TryAnswer(own chat \"yes\"): not consumed")
	}
	waitEnterResult(t, ch, 5*time.Second)
}

// --- validation / transport ----------------------------------------------

func TestEnterPlanModeValidation(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	cases := []struct {
		name string
		args json.RawMessage
	}{
		{"empty goal", planArgs("", []string{"Do it"}, 0)},
		{"no steps", planArgs("Do a thing", nil, 0)},
		{"empty step", planArgs("Do a thing", []string{""}, 0)},
		{"too many steps", planArgs("Do a thing", make([]string, planMaxSteps+1), 0)},
		{"step too long", planArgs("Do a thing", []string{strings.Repeat("x", planMaxStepRunes+1)}, 0)},
		{"goal too long", planArgs(strings.Repeat("g", planMaxGoalRunes+1), []string{"Do it"}, 0)},
		{"timeout too short", planArgs("Do a thing", []string{"Do it"}, 5)},
		{"timeout too long", planArgs("Do a thing", []string{"Do it"}, 1801)},
		{"message too long", planArgs(strings.Repeat("g", planMaxGoalRunes), repeatSteps(20, planMaxStepRunes), 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.enter.Execute(ctx, tc.args)
			if err == nil {
				t.Fatal("Execute: got nil error, want invalid_args")
			}
			var te *ToolError
			if !errors.As(err, &te) || te.Kind != "invalid_args" {
				t.Errorf("Execute error = %v, want a ToolError with kind invalid_args", err)
			}
		})
	}
	// Too many files / risks are validated the same way.
	files := make([]string, planMaxFiles+1)
	for i := range files {
		files[i] = "f.go"
	}
	raw, _ := json.Marshal(map[string]any{"goal": "Do a thing", "steps": []string{"Do it"}, "files": files})
	if _, err := env.enter.Execute(ctx, raw); err == nil {
		t.Error("Execute(too many files): got nil error, want invalid_args")
	}
}

func repeatSteps(n, runes int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strings.Repeat("s", runes)
	}
	return out
}

func TestEnterPlanModeNeedsChat(t *testing.T) {
	env := newPlanTestEnv(t)
	_, err := env.enter.Execute(context.Background(), planArgs("Do a thing", []string{"Do it"}, 0))
	if err == nil || !strings.Contains(err.Error(), "needs a Telegram chat") {
		t.Errorf("Execute(no chat): err = %v, want the needs-a-chat error", err)
	}
}

func TestEnterPlanModeNeedsTransport(t *testing.T) {
	store := NewPlanningStore("")
	enter := &enterPlanModeTool{store: store, waiters: map[int64]*planWaiter{}}
	// No InstallTransport: no gateway.
	_, err := enter.Execute(planCtx(100, 100), planArgs("Do a thing", []string{"Do it"}, 0))
	if err == nil || !strings.Contains(err.Error(), "no Telegram transport") {
		t.Errorf("Execute(no transport): err = %v, want the no-transport error", err)
	}
}

func TestEnterPlanModeSendFailureClearsPlan(t *testing.T) {
	env := newPlanTestEnv(t)
	env.gw.sendErr = fmt.Errorf("telegram down")
	ctx := planCtx(100, 100)
	_, err := env.enter.Execute(ctx, planArgs("Do a thing", []string{"Do it"}, 0))
	if err == nil || !strings.Contains(err.Error(), "send plan message") {
		t.Fatalf("Execute(send fails): err = %v, want the send error", err)
	}
	if got := env.storedPlan(t, ctx); got != nil {
		t.Errorf("stored plan after a send failure = %+v, want nil (rolled back)", got)
	}
}

// --- timeout / cancel ------------------------------------------------------

func TestEnterPlanModeTimeout(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	ch := runEnter(t, env.enter, ctx, planArgs("Do a slow thing", []string{"Wait"}, 10))
	waitPlanSend(t, env.gw, 5*time.Second)
	r := waitEnterResult(t, ch, 30*time.Second)
	if r.err == nil {
		t.Fatalf("enter_plan_mode: got result %q, want a timeout error", r.res)
	}
	var te *ToolError
	if !errors.As(r.err, &te) || te.Kind != "timeout" {
		t.Errorf("enter_plan_mode error = %v, want a ToolError with kind timeout", r.err)
	}
	if !strings.Contains(r.err.Error(), "no decision within") {
		t.Errorf("timeout error = %v, want it to name the missing decision", r.err)
	}
	if got := env.storedPlan(t, ctx); got != nil {
		t.Errorf("stored plan after timeout = %+v, want nil (cleared)", got)
	}
	if edit := env.gw.lastEdit(); !strings.Contains(edit.html, "No decision in time") {
		t.Errorf("timeout edit %q does not say No decision in time", edit.html)
	}
}

func TestEnterPlanModeContextCancel(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx, cancel := context.WithCancel(planCtx(100, 100))
	ch := runEnter(t, env.enter, ctx, planArgs("Do a thing", []string{"Do it"}, 0))
	waitPlanSend(t, env.gw, 5*time.Second)
	cancel()
	r := waitEnterResult(t, ch, 5*time.Second)
	if r.err == nil {
		t.Fatalf("enter_plan_mode: got result %q, want a cancellation error", r.res)
	}
	if !strings.Contains(r.err.Error(), "cancelled") {
		t.Errorf("cancel error = %v, want it to name the cancellation", r.err)
	}
	if got := env.storedPlan(t, planCtx(100, 100)); got != nil {
		t.Errorf("stored plan after cancel = %+v, want nil (cleared)", got)
	}
}

// --- exit / one-plan-per-chat / isolation ---------------------------------

func TestExitPlanMode(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	// No plan pending: idempotent no-op.
	res, err := env.exit.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("exit (no plan): %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		t.Fatalf("exit result %q is not JSON: %v", res, err)
	}
	if out["exited"] != false {
		t.Errorf("exit (no plan) = %v, want exited=false", out)
	}

	// A pending plan: exit clears the store. The live wait is
	// NOT resolved by exit (it cannot be, in the daemon the
	// chat lock makes the overlap impossible) — the blocked
	// EnterPlanMode still resolves when the user decides.
	ch := runEnter(t, env.enter, ctx, planArgs("Do a thing", []string{"Do it"}, 0))
	send := waitPlanSend(t, env.gw, 5*time.Second)
	planID := planIDFromSend(t, send)
	res, err = env.exit.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("exit (pending plan): %v", err)
	}
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		t.Fatalf("exit result %q is not JSON: %v", res, err)
	}
	if out["exited"] != true || out["plan_id"] != planID {
		t.Errorf("exit (pending plan) = %v, want exited=true with plan_id %s", out, planID)
	}
	if got := env.storedPlan(t, ctx); got != nil {
		t.Errorf("stored plan after exit = %+v, want nil", got)
	}
	// The live wait still settles on the user's tap.
	if err := env.enter.HandleCallback(ctx, env.gw, planFakeCallbackQuery{
		data: "plan:approve:" + planID, id: "cb1", sender: 100, chat: 100, msg: 42,
	}); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	r := waitEnterResult(t, ch, 5*time.Second)
	if r.err != nil || decodePlanResult(t, r.res)["decision"] != "approved" {
		t.Errorf("result after exit+approve = %v / %v, want an approval", r.res, r.err)
	}
}

func TestEnterPlanModeSecondEnterRejected(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	ch := runEnter(t, env.enter, ctx, planArgs("First plan", []string{"Step one"}, 0))
	send := waitPlanSend(t, env.gw, 5*time.Second)
	planID := planIDFromSend(t, send)
	_, err := env.enter.Execute(ctx, planArgs("Second plan", []string{"Step one"}, 0))
	if err == nil || !strings.Contains(err.Error(), "already in plan mode") {
		t.Errorf("second Execute: err = %v, want the already-in-plan-mode error", err)
	}
	if err := env.enter.HandleCallback(ctx, env.gw, planFakeCallbackQuery{
		data: "plan:approve:" + planID, id: "cb1", sender: 100, chat: 100, msg: 42,
	}); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	r := waitEnterResult(t, ch, 5*time.Second)
	if r.err != nil || decodePlanResult(t, r.res)["decision"] != "approved" {
		t.Errorf("first result = %v / %v, want an approval", r.res, r.err)
	}
	// After the first plan resolves, entering again works.
	ch2 := runEnter(t, env.enter, ctx, planArgs("Second plan", []string{"Step one"}, 0))
	// Wait for the SECOND send (waitPlanSend would return at once
	// on the first plan's already-recorded send).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && env.gw.sendCount() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if env.gw.sendCount() < 2 {
		t.Fatal("timed out waiting for the second plan message to be sent")
	}
	if !env.enter.TryAnswer(planFakeReplyUpdate{chat: 100, sender: 100, text: "yes"}) {
		t.Fatal("TryAnswer(second plan): not consumed")
	}
	r2 := waitEnterResult(t, ch2, 5*time.Second)
	if r2.err != nil || decodePlanResult(t, r2.res)["decision"] != "approved" {
		t.Errorf("second result = %v / %v, want an approval", r2.res, r2.err)
	}
}

func TestPlanModePerChatIsolation(t *testing.T) {
	env := newPlanTestEnv(t)
	ctxA := planCtx(100, 100)
	ctxB := planCtx(200, 200)
	chA := runEnter(t, env.enter, ctxA, planArgs("Plan A", []string{"Step A"}, 0))
	sendA := waitPlanSend(t, env.gw, 5*time.Second)
	planIDA := planIDFromSend(t, sendA)
	// Chat B can enter its own plan while A's is pending.
	chB := runEnter(t, env.enter, ctxB, planArgs("Plan B", []string{"Step B"}, 0))
	// Wait for B's send (the second one recorded).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && env.gw.sendCount() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if env.gw.sendCount() != 2 {
		t.Fatalf("sends = %d, want 2 (one per chat)", env.gw.sendCount())
	}
	sendB := env.gw.lastSend()
	if sendB.chatID != 200 || !strings.Contains(sendB.text, "Plan B") {
		t.Errorf("second send = %+v, want chat 200's plan B", sendB)
	}
	planIDB := planIDFromSend(t, sendB)
	if planIDA == planIDB {
		t.Error("both chats got the same plan id, want distinct ids")
	}
	// Decide B first (typed), then A (callback): neither decision
	// touches the other chat's wait.
	if !env.enter.TryAnswer(planFakeReplyUpdate{chat: 200, sender: 200, text: "no"}) {
		t.Fatal("TryAnswer(chat B \"no\"): not consumed")
	}
	rB := waitEnterResult(t, chB, 5*time.Second)
	if rB.err != nil || decodePlanResult(t, rB.res)["decision"] != "rejected" {
		t.Errorf("chat B result = %v / %v, want a rejection", rB.res, rB.err)
	}
	select {
	case r := <-chA:
		t.Fatalf("chat A returned %v while only B decided, want it still waiting", r)
	default:
	}
	if err := env.enter.HandleCallback(ctxA, env.gw, planFakeCallbackQuery{
		data: "plan:approve:" + planIDA, id: "cb1", sender: 100, chat: 100, msg: 42,
	}); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	rA := waitEnterResult(t, chA, 5*time.Second)
	if rA.err != nil || decodePlanResult(t, rA.res)["decision"] != "approved" {
		t.Errorf("chat A result = %v / %v, want an approval", rA.res, rA.err)
	}
}

// --- PlanModeBlocks ---------------------------------------------------------

func TestPlanModeBlocksDestructiveOnly(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	// No plan: nothing blocks.
	if blocked, _ := env.enter.PlanModeBlocks(ctx, "shell_exec"); blocked {
		t.Error("PlanModeBlocks(shell_exec) with no plan = true, want false")
	}
	ch := runEnter(t, env.enter, ctx, planArgs("Do a thing", []string{"Do it"}, 0))
	waitPlanSend(t, env.gw, 5*time.Second)
	if blocked, reason := env.enter.PlanModeBlocks(ctx, "shell_exec"); !blocked {
		t.Error("PlanModeBlocks(shell_exec) with a pending plan = false, want true")
	} else if !strings.Contains(reason, "pending approval") {
		t.Errorf("block reason = %q, want it to name the pending plan", reason)
	}
	for _, tool := range []string{"file_write", "file_edit"} {
		if blocked, _ := env.enter.PlanModeBlocks(ctx, tool); !blocked {
			t.Errorf("PlanModeBlocks(%s) = false, want true (destructive)", tool)
		}
	}
	for _, tool := range []string{"file_read", "ask_user_question", "web_search", "enter_plan_mode", "exit_plan_mode"} {
		if blocked, _ := env.enter.PlanModeBlocks(ctx, tool); blocked {
			t.Errorf("PlanModeBlocks(%s) = true, want false (safe/network, or the plan tools themselves)", tool)
		}
	}
	// Another chat is not blocked.
	if blocked, _ := env.enter.PlanModeBlocks(planCtx(200, 200), "shell_exec"); blocked {
		t.Error("PlanModeBlocks(shell_exec) for another chat = true, want false")
	}
	// After the decision, nothing blocks.
	if !env.enter.TryAnswer(planFakeReplyUpdate{chat: 100, sender: 100, text: "yes"}) {
		t.Fatal("TryAnswer: not consumed")
	}
	waitEnterResult(t, ch, 5*time.Second)
	if blocked, _ := env.enter.PlanModeBlocks(ctx, "shell_exec"); blocked {
		t.Error("PlanModeBlocks(shell_exec) after approval = true, want false")
	}
}

func TestPlanModeBlocksExpiredPlan(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	// Store an already-expired plan directly.
	expired := &planState{
		ID: "plan_expired", Goal: "Old plan", Steps: []string{"Step"},
		Requester: 100,
		CreatedAt: time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339),
		ExpiresAt: time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339),
	}
	dir, key := env.store.resolveBucket(ctx)
	env.store.mu.Lock()
	if err := saveStoredPlan(env.store, ctx, dir, key, expired); err != nil {
		env.store.mu.Unlock()
		t.Fatalf("saveStoredPlan: %v", err)
	}
	env.store.mu.Unlock()
	if blocked, _ := env.enter.PlanModeBlocks(ctx, "shell_exec"); blocked {
		t.Error("PlanModeBlocks(shell_exec) with an expired plan = true, want false")
	}
	// Entering a new plan overwrites the expired one.
	ch := runEnter(t, env.enter, ctx, planArgs("Fresh plan", []string{"Step"}, 0))
	waitPlanSend(t, env.gw, 5*time.Second)
	if blocked, _ := env.enter.PlanModeBlocks(ctx, "shell_exec"); !blocked {
		t.Error("PlanModeBlocks(shell_exec) with a fresh plan = false, want true")
	}
	if !env.enter.TryAnswer(planFakeReplyUpdate{chat: 100, sender: 100, text: "yes"}) {
		t.Fatal("TryAnswer: not consumed")
	}
	waitEnterResult(t, ch, 5*time.Second)
}

func TestPlanModeRegistryDiscovery(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	// The registry auto-discovered the checker (Register
	// installs the first PlanModeChecker it sees): with no plan
	// nothing blocks, with a plan the registry reports it.
	if blocked, _ := env.reg.PlanModeBlocks(ctx, "shell_exec"); blocked {
		t.Error("registry PlanModeBlocks with no plan = true, want false")
	}
	ch := runEnter(t, env.enter, ctx, planArgs("Do a thing", []string{"Do it"}, 0))
	waitPlanSend(t, env.gw, 5*time.Second)
	if blocked, _ := env.reg.PlanModeBlocks(ctx, "shell_exec"); !blocked {
		t.Error("registry PlanModeBlocks with a pending plan = false, want true")
	}
	// A registry without a plan-mode tool never blocks.
	empty := NewRegistry()
	if blocked, _ := empty.PlanModeBlocks(ctx, "shell_exec"); blocked {
		t.Error("empty registry PlanModeBlocks = true, want false")
	}
	var nilReg *Registry
	if blocked, _ := nilReg.PlanModeBlocks(ctx, "shell_exec"); blocked {
		t.Error("nil registry PlanModeBlocks = true, want false")
	}
	if !env.enter.TryAnswer(planFakeReplyUpdate{chat: 100, sender: 100, text: "yes"}) {
		t.Fatal("TryAnswer: not consumed")
	}
	waitEnterResult(t, ch, 5*time.Second)
}

// --- restart orphaning ------------------------------------------------------

func TestPlanModeRestartOrphan(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	// A plan stored by a previous daemon life (no waiter — the
	// restart dropped it), still inside its expiry.
	orphan := &planState{
		ID: "plan_orphan", Goal: "Orphaned plan", Steps: []string{"Step"},
		Requester: 100,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		ExpiresAt: time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339),
	}
	dir, key := env.store.resolveBucket(ctx)
	env.store.mu.Lock()
	if err := saveStoredPlan(env.store, ctx, dir, key, orphan); err != nil {
		env.store.mu.Unlock()
		t.Fatalf("saveStoredPlan: %v", err)
	}
	env.store.mu.Unlock()

	// A fresh tool instance sharing the store (simulating the
	// restarted daemon) still sees and blocks on the plan.
	fresh := &enterPlanModeTool{store: env.store, waiters: map[int64]*planWaiter{}}
	if blocked, _ := fresh.PlanModeBlocks(ctx, "shell_exec"); !blocked {
		t.Error("fresh instance PlanModeBlocks = false, want true (orphaned plan still blocks until expiry)")
	}
	// A tap on the orphaned plan's message gets the honest
	// no-longer-pending notice: there is no waiter to settle.
	gw := &planFakeGateway{nextMsgID: 41}
	if err := fresh.HandleCallback(ctx, gw, planFakeCallbackQuery{
		data: "plan:approve:plan_orphan", id: "cb1", sender: 100, chat: 100, msg: 42,
	}); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	if got := gw.lastAnswer(); got.text != planNoLongerPendingNotice {
		t.Errorf("orphan tap toast = %q, want the no-longer-pending notice", got.text)
	}
	// ExitPlanMode clears the orphan: the chat is never stuck.
	freshExit := &exitPlanModeTool{store: env.store}
	res, err := freshExit.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("exit (orphan): %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		t.Fatalf("exit result %q is not JSON: %v", res, err)
	}
	if out["exited"] != true || out["plan_id"] != "plan_orphan" {
		t.Errorf("exit (orphan) = %v, want exited=true with plan_id plan_orphan", out)
	}
	if blocked, _ := fresh.PlanModeBlocks(ctx, "shell_exec"); blocked {
		t.Error("PlanModeBlocks after exit = true, want false")
	}
}

func TestPlanModeExitExpiredPlan(t *testing.T) {
	env := newPlanTestEnv(t)
	ctx := planCtx(100, 100)
	expired := &planState{
		ID: "plan_old", Goal: "Old plan", Steps: []string{"Step"},
		Requester: 100,
		CreatedAt: time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339),
		ExpiresAt: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
	}
	dir, key := env.store.resolveBucket(ctx)
	env.store.mu.Lock()
	if err := saveStoredPlan(env.store, ctx, dir, key, expired); err != nil {
		env.store.mu.Unlock()
		t.Fatalf("saveStoredPlan: %v", err)
	}
	env.store.mu.Unlock()
	res, err := env.exit.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("exit (expired plan): %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		t.Fatalf("exit result %q is not JSON: %v", res, err)
	}
	if out["exited"] != true {
		t.Errorf("exit (expired plan) = %v, want exited=true", out)
	}
	if note, _ := out["note"].(string); !strings.Contains(note, "already expired") {
		t.Errorf("exit note = %q, want it to say the plan had already expired", note)
	}
	if got := env.storedPlan(t, ctx); got != nil {
		t.Errorf("stored plan after exit = %+v, want nil", got)
	}
}

func TestPlanModeBlocksFailOpenOnCorruptFile(t *testing.T) {
	dir := t.TempDir()
	store := NewPlanningStore("")
	if err := store.SetDataDir(dir); err != nil {
		t.Fatalf("SetDataDir: %v", err)
	}
	env := &planTestEnv{store: store}
	env.enter = &enterPlanModeTool{store: store, waiters: map[int64]*planWaiter{}}
	env.exit = &exitPlanModeTool{store: store}
	gw := &planFakeGateway{nextMsgID: 41}
	if err := env.enter.InstallTransport(&ToolHost{Gateway: gw, OwnerID: 999}); err != nil {
		t.Fatalf("InstallTransport: %v", err)
	}
	ctx := planCtx(100, 100)
	// A corrupt plans file (hand-edited, bad JSON).
	chatDir := filepath.Join(dir, "chats", "100")
	if err := os.MkdirAll(chatDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(chatDir, planningPlansFile), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Fail open: a corrupt file must not block the chat forever.
	if blocked, _ := env.enter.PlanModeBlocks(ctx, "shell_exec"); blocked {
		t.Error("PlanModeBlocks with a corrupt plans file = true, want false (fail open)")
	}
	// But the plan tools surface the corruption honestly.
	if _, err := env.enter.Execute(ctx, planArgs("Do a thing", []string{"Do it"}, 0)); err == nil || !strings.Contains(err.Error(), "corrupt plans file") {
		t.Errorf("enter with a corrupt plans file: err = %v, want the corrupt-file error", err)
	}
	if _, err := env.exit.Execute(ctx, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "corrupt plans file") {
		t.Errorf("exit with a corrupt plans file: err = %v, want the corrupt-file error", err)
	}
}

// --- pure helpers -----------------------------------------------------------

func TestParsePlanReply(t *testing.T) {
	cases := []struct {
		text     string
		decision string
		feedback string
	}{
		{"yes", "approved", ""},
		{"YES!", "approved", ""},
		{"approve", "approved", ""},
		{"ok", "approved", ""},
		{"go ahead", "approved", ""},
		{"👍", "approved", ""},
		{"no", "rejected", ""},
		{"NO.", "rejected", ""},
		{"cancel", "rejected", ""},
		{"❌", "rejected", ""},
		{"no, use postgres", "revise", "no, use postgres"},
		{"yes, but change the order", "revise", "yes, but change the order"},
		{"use a different table", "revise", "use a different table"},
	}
	for _, tc := range cases {
		if got := parsePlanReply(tc.text); got.decision != tc.decision || got.feedback != tc.feedback {
			t.Errorf("parsePlanReply(%q) = %+v, want decision=%q feedback=%q", tc.text, got, tc.decision, tc.feedback)
		}
	}
}

func TestParsePlanCallback(t *testing.T) {
	cases := []struct {
		data     string
		decision string
		id       string
		ok       bool
	}{
		{"plan:approve:plan_abc123", "approve", "plan_abc123", true},
		{"plan:reject:plan_abc123", "reject", "plan_abc123", true},
		{"plan:revise:plan_abc123", "revise", "plan_abc123", true},
		{"perm:approve:x", "", "", false},
		{"plan:approve:", "", "", false},
		{"plan:explode:plan_abc123", "", "", false},
		{"plan:approve", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		d, id, ok := parsePlanCallback(tc.data)
		if d != tc.decision || id != tc.id || ok != tc.ok {
			t.Errorf("parsePlanCallback(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.data, d, id, ok, tc.decision, tc.id, tc.ok)
		}
	}
}

func TestRenderPlanMessage(t *testing.T) {
	plan := &planState{
		Goal:  "Move the data",
		Steps: []string{"Dump it", "Load it"},
		Files: []string{"db.sql"},
		Risks: []string{"Downtime"},
	}
	msg := renderPlanMessage(plan, 5*time.Minute)
	for _, want := range []string{"📋 Plan: Move the data", "1. Dump it", "2. Load it", "Files:\n- db.sql", "Risks:\n- Downtime", "You have 5 minutes"} {
		if !strings.Contains(msg, want) {
			t.Errorf("plan message %q does not contain %q", msg, want)
		}
	}
	bare := &planState{Goal: "Do it", Steps: []string{"Step"}}
	if msg := renderPlanMessage(bare, 30*time.Second); strings.Contains(msg, "Files:") || strings.Contains(msg, "Risks:") {
		t.Errorf("bare plan message %q lists empty sections, want them omitted", msg)
	} else if !strings.Contains(msg, "You have 30 seconds") {
		t.Errorf("bare plan message %q does not say 30 seconds", msg)
	}
}

func TestRenderPlanOutcomeEscapesHTML(t *testing.T) {
	plan := &planState{Goal: "Fix <b>bold</b> & 'quotes'"}
	out := renderPlanOutcome(plan, planDecision{decision: "approved"})
	if strings.Contains(out, "<b>") || strings.Contains(out, "& '") {
		t.Errorf("outcome %q contains raw HTML, want it escaped (the edit sends HTML)", out)
	}
	if !strings.Contains(out, "&lt;b&gt;") || !strings.Contains(out, "&amp;") {
		t.Errorf("outcome %q does not escape < and &", out)
	}
	rev := renderPlanOutcome(plan, planDecision{decision: "revise", feedback: "use <x> instead"})
	if !strings.Contains(rev, "&lt;x&gt;") {
		t.Errorf("revision outcome %q does not escape the feedback", rev)
	}
}
