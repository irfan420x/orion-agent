// rc23 W4: /budget + /skills reload commands (user order, priority)
// and the F-A17-9/F-A17-10 photo handling in PollStream.
package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
)

// stubBudgetEnv is a CommandEnv stub for /budget and /skills tests.
type stubBudgetEnv struct {
	env *CommandEnv

	limit        int
	used         int
	setCalled    bool
	setLimit     int
	setErr       error
	reloadLoaded int
	reloadErr    error
	reloadCalled bool
}

func newStubBudgetEnv() *stubBudgetEnv {
	s := &stubBudgetEnv{limit: 100000, used: 25000}
	s.env = &CommandEnv{
		ChatID:  7,
		OwnerID: 7,
		BudgetInfo: func(chatID int64) (int, int) {
			return s.limit, s.used
		},
		SetBudget: func(limit int) error {
			s.setCalled = true
			s.setLimit = limit
			if s.setErr != nil {
				return s.setErr
			}
			s.limit = limit
			return nil
		},
		ReloadSkills: func() (int, error) {
			s.reloadCalled = true
			return s.reloadLoaded, s.reloadErr
		},
	}
	return s
}

func dispatchBudget(t *testing.T, s *stubBudgetEnv, ctx context.Context, text string) string {
	t.Helper()
	if ctx == nil {
		ctx = context.Background()
	}
	r := DefaultRegistry(s.env)
	reply, handled, err := r.Dispatch(ctx, s.env, text)
	if err != nil {
		t.Fatalf("Dispatch(%q) error: %v", text, err)
	}
	if !handled {
		t.Fatalf("Dispatch(%q) not handled", text)
	}
	return reply
}

func TestBudgetShow(t *testing.T) {
	s := newStubBudgetEnv()
	reply := dispatchBudget(t, s, nil, "/budget")
	for _, want := range []string{"100,000", "25,000", "75,000", "UTC midnight"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q:\n%s", want, reply)
		}
	}
	if s.setCalled {
		t.Error("show must not call SetBudget")
	}
}

func TestBudgetShowUnlimited(t *testing.T) {
	s := newStubBudgetEnv()
	s.limit = 0
	reply := dispatchBudget(t, s, nil, "/budget")
	if !strings.Contains(reply, "unlimited") {
		t.Errorf("unlimited reply: %q", reply)
	}
	if !strings.Contains(reply, "25,000") {
		t.Errorf("unlimited reply must still show today's usage: %q", reply)
	}
}

func TestBudgetShowOverLimit(t *testing.T) {
	s := newStubBudgetEnv()
	s.used = 150000
	reply := dispatchBudget(t, s, nil, "/budget")
	if !strings.Contains(reply, "Remaining today: 0") {
		t.Errorf("over-limit reply must clamp remaining to 0:\n%s", reply)
	}
}

func TestBudgetSet(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int
	}{
		{"/budget 5000", 5000},
		{"/budget 1000", 1000},
		{"/budget 10000000", 10000000},
		{"/budget infinity", 0},
		{"/budget inf", 0},
		{"/budget unlimited", 0},
		{"/budget INFINITY", 0},
		{"/budget 0", 0},
		{"/budget 5000 extra words", 5000},
	} {
		s := newStubBudgetEnv()
		reply := dispatchBudget(t, s, nil, tc.text)
		if !s.setCalled {
			t.Errorf("%q: SetBudget not called", tc.text)
			continue
		}
		if s.setLimit != tc.want {
			t.Errorf("%q: SetBudget(%d), want %d", tc.text, s.setLimit, tc.want)
		}
		if tc.want == 0 {
			if !strings.Contains(reply, "unlimited") {
				t.Errorf("%q: reply missing unlimited:\n%s", tc.text, reply)
			}
		} else if !strings.Contains(reply, "every chat") {
			t.Errorf("%q: reply must say the limit applies to every chat:\n%s", tc.text, reply)
		}
	}
}

func TestBudgetSetInvalid(t *testing.T) {
	for _, text := range []string{"/budget 999", "/budget 10000001", "/budget -5", "/budget abc", "/budget 12.5"} {
		s := newStubBudgetEnv()
		reply := dispatchBudget(t, s, nil, text)
		if s.setCalled {
			t.Errorf("%q: SetBudget must not be called", text)
		}
		if !strings.Contains(reply, "Usage: /budget") && !strings.Contains(reply, "must be") {
			t.Errorf("%q: reply must explain the valid range:\n%s", text, reply)
		}
	}
}

func TestBudgetSetError(t *testing.T) {
	s := newStubBudgetEnv()
	s.setErr = errors.New("disk exploded")
	reply := dispatchBudget(t, s, nil, "/budget 5000")
	if !strings.Contains(reply, "Could not set the budget") {
		t.Errorf("error reply: %q", reply)
	}
}

func TestBudgetNonOwnerRefused(t *testing.T) {
	s := newStubBudgetEnv()
	s.env.ChatID = 8 // not the owner
	for _, text := range []string{"/budget", "/budget 5000", "/বাজেট", "/বাজেট 5000"} {
		reply := dispatchBudget(t, s, nil, text)
		if !strings.Contains(reply, "owner-only") {
			t.Errorf("%q: non-owner reply must say owner-only:\n%s", text, reply)
		}
	}
	if s.setCalled {
		t.Error("non-owner must never reach SetBudget")
	}
}

func TestBudgetOwnerViaSender(t *testing.T) {
	s := newStubBudgetEnv()
	s.env.ChatID = 99 // a group chat, not the owner chat
	ctx := WithSenderID(context.Background(), 7)
	reply := dispatchBudget(t, s, ctx, "/budget 5000")
	if !s.setCalled || s.setLimit != 5000 {
		t.Errorf("owner sender in a group chat must be allowed; reply:\n%s", reply)
	}
}

func TestBudgetNoOwnerRefusesEveryone(t *testing.T) {
	s := newStubBudgetEnv()
	s.env.OwnerID = 0
	reply := dispatchBudget(t, s, nil, "/budget")
	if !strings.Contains(reply, "owner-only") {
		t.Errorf("no-owner reply must say owner-only:\n%s", reply)
	}
}

func TestBudgetUnwired(t *testing.T) {
	s := newStubBudgetEnv()
	s.env.BudgetInfo = nil
	reply := dispatchBudget(t, s, nil, "/budget")
	if !strings.Contains(reply, "unavailable") {
		t.Errorf("unwired show reply: %q", reply)
	}
	s.env.BudgetInfo = func(chatID int64) (int, int) { return 100000, 0 }
	s.env.SetBudget = nil
	reply = dispatchBudget(t, s, nil, "/budget 5000")
	if !strings.Contains(reply, "unavailable") {
		t.Errorf("unwired set reply: %q", reply)
	}
}

func TestBudgetAliasBajet(t *testing.T) {
	s := newStubBudgetEnv()
	reply := dispatchBudget(t, s, nil, "/বাজেট 5000")
	if !s.setCalled || s.setLimit != 5000 {
		t.Errorf("/বাজেট must dispatch to the budget handler; reply:\n%s", reply)
	}
	// Fresh stub for the show path: the set above changed this
	// stub's limit to 5,000, so it cannot show 100,000 anymore.
	s = newStubBudgetEnv()
	reply = dispatchBudget(t, s, nil, "/বাজেট")
	if !strings.Contains(reply, "100,000") {
		t.Errorf("/বাজেট show reply:\n%s", reply)
	}
}

func TestSkillsReloadCommand(t *testing.T) {
	s := newStubBudgetEnv()
	s.reloadLoaded = 3
	reply := dispatchBudget(t, s, nil, "/skills reload")
	if !s.reloadCalled {
		t.Fatal("ReloadSkills not called")
	}
	if !strings.Contains(reply, "Reloaded 3 skills") {
		t.Errorf("reload reply: %q", reply)
	}
}

func TestSkillsReloadCommandUsage(t *testing.T) {
	s := newStubBudgetEnv()
	reply := dispatchBudget(t, s, nil, "/skills")
	if s.reloadCalled {
		t.Error("bare /skills must not reload")
	}
	if !strings.Contains(reply, "Usage: /skills reload") {
		t.Errorf("bare /skills reply: %q", reply)
	}
}

func TestSkillsReloadCommandPartialError(t *testing.T) {
	s := newStubBudgetEnv()
	s.reloadLoaded = 2
	s.reloadErr = errors.New("bad.md: empty file")
	reply := dispatchBudget(t, s, nil, "/skills reload")
	if !strings.Contains(reply, "Reloaded 2 skill(s), but some skills failed to load") {
		t.Errorf("partial reload reply: %q", reply)
	}
}

func TestSkillsReloadCommandTotalFailure(t *testing.T) {
	s := newStubBudgetEnv()
	s.reloadErr = errors.New(`cannot read directory "x": no such file`)
	reply := dispatchBudget(t, s, nil, "/skills reload")
	if !strings.Contains(reply, "Could not reload skills") {
		t.Errorf("failed reload reply: %q", reply)
	}
}

func TestSkillsReloadCommandUnwired(t *testing.T) {
	s := newStubBudgetEnv()
	s.env.ReloadSkills = nil
	reply := dispatchBudget(t, s, nil, "/skills reload")
	if !strings.Contains(reply, "unavailable") {
		t.Errorf("unwired reload reply: %q", reply)
	}
}

func TestRegistryCountsAndMenu(t *testing.T) {
	s := newStubBudgetEnv()
	r := DefaultRegistry(s.env)
	if got := len(r.Commands()); got != 23 {
		t.Errorf("registry has %d commands, want 23 (19 + budget + বাজেট + skills + rules)", got)
	}
	// /help lists the new commands, including the Bengali alias.
	reply := dispatchBudget(t, s, nil, "/help")
	for _, want := range []string{"/budget", "/বাজেট", "/skills"} {
		if !strings.Contains(reply, want) {
			t.Errorf("/help missing %s:\n%s", want, reply)
		}
	}
	// The Telegram "/" menu must NOT contain the Bengali alias:
	// the Bot API only accepts [a-z0-9_] names, and one bad entry
	// fails the whole setMyCommands call (stale menu for everyone).
	var names []string
	for _, c := range r.BotCommands() {
		names = append(names, c.Name)
	}
	for _, want := range []string{"budget", "skills"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("BotCommands missing %q: %v", want, names)
		}
	}
	for _, n := range names {
		if n == "বাজেট" {
			t.Errorf("BotCommands must not contain the Bengali alias: %v", names)
		}
	}
}

func TestIsValidBotCommandName(t *testing.T) {
	for name, want := range map[string]bool{
		"budget": true, "skills": true, "a": true, "x_y_2": true,
		"": false, "বাজেট": false, "Budget": false, "a-b": false,
		"a b": false, strings.Repeat("a", 33): false, strings.Repeat("a", 32): true,
	} {
		if got := isValidBotCommandName(name); got != want {
			t.Errorf("isValidBotCommandName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestParseBudgetArg(t *testing.T) {
	for arg, want := range map[string]struct {
		limit int
		ok    bool
	}{
		"5000": {5000, true}, "infinity": {0, true}, "inf": {0, true},
		"unlimited": {0, true}, "Unlimited": {0, true}, "abc": {0, false},
		"12.5": {0, false}, "": {0, false}, " 5000 ": {5000, true},
	} {
		limit, ok := parseBudgetArg(arg)
		if limit != want.limit || ok != want.ok {
			t.Errorf("parseBudgetArg(%q) = (%d, %v), want (%d, %v)", arg, limit, ok, want.limit, want.ok)
		}
	}
}

// --- F-A17-9 / F-A17-10: photos -------------------------------------------

func photoUpdate(id, chatID, fromID int64) Update {
	return Update{
		UpdateID: id,
		Message: &TGMessage{
			MessageID: id,
			Chat:      TGChat{ID: chatID},
			From:      &TGUser{ID: fromID},
			Photo: []TGPhotoSize{
				{FileID: "small", Width: 90, Height: 67},
				{FileID: "big", Width: 1280, Height: 960},
			},
		},
	}
}

func TestPhotoMessageGetsCannotSeeReply(t *testing.T) {
	fake := &streamFake{updates: []Update{photoUpdate(1, 77, 77)}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)
	turns := 0
	handler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		turns++
		return "turn reply", nil, nil
	}
	cancel, errCh := runPollStream(t, g, handler)
	defer stopPollStream(t, cancel, errCh)

	waitFor(t, "cannot-see reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if m.ChatID == 77 && m.Text == CannotSeeImagesMessage {
				return true
			}
		}
		return false
	})
	if turns != 0 {
		t.Errorf("photo must not run a turn, but the handler ran %d time(s)", turns)
	}
}

func TestPhotoFromNonAllowlistedChatGetsNoReply(t *testing.T) {
	fake := &streamFake{updates: []Update{photoUpdate(1, 77, 77)}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(7))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	turns := 0
	handler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		turns++
		return "turn reply", nil, nil
	}
	cancel, errCh := runPollStream(t, g, handler)
	defer stopPollStream(t, cancel, errCh)

	// Give the poll loop time to fetch and drop the update: the
	// drop is synchronous in dispatch, so 500ms after the first
	// fetch a reply would already have landed if one were coming.
	waitFor(t, "update fetched", func() bool {
		return len(fake.getUpdatesQueriesSnapshot()) > 0
	})
	time.Sleep(500 * time.Millisecond)
	if sent, _, _ := fake.snapshot(); len(sent) > 0 {
		t.Fatalf("non-allowlisted photo got a reply: %+v", sent)
	}
	if turns != 0 {
		t.Errorf("non-allowlisted photo must not run a turn, ran %d", turns)
	}
}

func TestPhotoWithTextStillCannotSee(t *testing.T) {
	upd := photoUpdate(1, 77, 77)
	upd.Message.Text = "what is in this photo?"
	fake := &streamFake{updates: []Update{upd}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)
	turns := 0
	handler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		turns++
		return "turn reply", nil, nil
	}
	cancel, errCh := runPollStream(t, g, handler)
	defer stopPollStream(t, cancel, errCh)

	waitFor(t, "cannot-see reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if m.Text == CannotSeeImagesMessage {
				return true
			}
		}
		return false
	})
	if turns != 0 {
		t.Errorf("photo with text must not run a turn, ran %d", turns)
	}
}

func TestVisionErrorTranslatedToCannotSee(t *testing.T) {
	fake := &streamFake{updates: []Update{
		{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 77}, Text: "hi", From: &TGUser{ID: 77}}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)
	handler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		return "", nil, errors.New("core: provider call at step 1: deepseek: status 400: image input is not supported for this model")
	}
	cancel, errCh := runPollStream(t, g, handler)
	defer stopPollStream(t, cancel, errCh)

	waitFor(t, "translated reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if m.Text == CannotSeeImagesMessage {
				return true
			}
		}
		return false
	})
	sent, _, _ := fake.snapshot()
	for _, m := range sent {
		if strings.Contains(m.Text, "status 400") {
			t.Errorf("raw provider 400 leaked to the user: %q", m.Text)
		}
	}
}

func TestNonVisionErrorKeepsSorryForm(t *testing.T) {
	fake := &streamFake{updates: []Update{
		{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 77}, Text: "hi", From: &TGUser{ID: 77}}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)
	handler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		return "", nil, errors.New("core: provider call at step 1: context too long (status 400)")
	}
	cancel, errCh := runPollStream(t, g, handler)
	defer stopPollStream(t, cancel, errCh)

	waitFor(t, "sorry reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if strings.Contains(m.Text, "Sorry, I hit an error") {
				return true
			}
		}
		return false
	})
}

func TestIsVisionError(t *testing.T) {
	for errStr, want := range map[string]bool{
		"status 400: image input is not supported for this model": true,
		"400 Bad Request: this model doesn't support vision":      true,
		"unsupported image type (status 400)":                     true,
		"multimodal input not supported: 400":                     true,
		"invalid photo content: bad request":                      true,
		"provider call: status 400: context too long":             false,
		"image file too large":                                    false,
		"status 500: internal error":                              false,
		"":                                                        false,
	} {
		var err error
		if errStr != "" {
			err = errors.New(errStr)
		}
		if got := isVisionError(err); got != want {
			t.Errorf("isVisionError(%q) = %v, want %v", errStr, got, want)
		}
	}
	if isVisionError(nil) {
		t.Error("isVisionError(nil) must be false")
	}
}

func TestTGMessagePhotoJSONDecodes(t *testing.T) {
	// A real Bot API photo update: caption lives in "caption",
	// not "text", and the photo is a list of size variants.
	raw := `{"update_id":9,"message":{"message_id":3,"chat":{"id":77},"from":{"id":77},"caption":"what is this?","photo":[{"file_id":"a","width":90,"height":67},{"file_id":"b","width":1280,"height":960,"file_size":12345}]}}`
	var upd Update
	if err := json.Unmarshal([]byte(raw), &upd); err != nil {
		t.Fatalf("unmarshal photo update: %v", err)
	}
	if !upd.Message.HasPhoto() {
		t.Fatal("HasPhoto must be true for a photo update")
	}
	if len(upd.Message.Photo) != 2 || upd.Message.Photo[1].FileID != "b" || upd.Message.Photo[1].FileSize != 12345 {
		t.Errorf("photo variants: %+v", upd.Message.Photo)
	}
	if upd.Message.Text != "" {
		t.Errorf("photo update text = %q, want empty (caption is separate)", upd.Message.Text)
	}
	if (&TGMessage{}).HasPhoto() {
		t.Error("empty message must not HasPhoto")
	}
	if (*TGMessage)(nil).HasPhoto() {
		t.Error("nil message must not HasPhoto")
	}
}
