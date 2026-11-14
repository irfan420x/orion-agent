// rc24 W-D: F-A18-6 (/skills reload owner-only), F-A18-9 (non-photo
// media gets an honest reply instead of a silent drop) and F-A18-10
// (/rules recognises the owner in a group chat).
package messaging

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/permissions"
)

// --- F-A18-6: /skills reload is owner-only -------------------------------

func TestSkillsReloadNonOwnerRefused(t *testing.T) {
	s := newStubBudgetEnv()
	s.env.ChatID = 8 // allowlisted, but not the owner (owner is 7)
	reply := dispatchBudget(t, s, nil, "/skills reload")
	if !strings.Contains(reply, "owner-only") {
		t.Errorf("non-owner /skills reload = %q, want an owner-only refusal", reply)
	}
	if s.reloadCalled {
		t.Error("non-owner must never reach ReloadSkills")
	}
}

func TestSkillsReloadOwnerViaSender(t *testing.T) {
	s := newStubBudgetEnv()
	s.env.ChatID = 99 // a group chat, not the owner chat
	s.reloadLoaded = 2
	ctx := WithSenderID(context.Background(), 7)
	reply := dispatchBudget(t, s, ctx, "/skills reload")
	if !s.reloadCalled {
		t.Errorf("owner sender in a group chat must be allowed; reply:\n%s", reply)
	}
	if !strings.Contains(reply, "Reloaded 2 skills") {
		t.Errorf("owner-via-sender reload reply: %q", reply)
	}
}

// --- F-A18-10: /rules recognises the owner in a group chat ---------------

func dispatchRulesCtx(t *testing.T, env *CommandEnv, ctx context.Context, text string) string {
	t.Helper()
	r := DefaultRegistry(env)
	reply, handled, err := r.Dispatch(ctx, env, text)
	if err != nil {
		t.Fatalf("Dispatch(%q) error: %v", text, err)
	}
	if !handled {
		t.Fatalf("Dispatch(%q) not handled", text)
	}
	return reply
}

func TestRulesOwnerInGroupRecognised(t *testing.T) {
	const owner = 7429947930
	store := permissions.NewRuleStore("")
	if err := store.Set("shell_exec", permissions.ActionAllow); err != nil {
		t.Fatalf("Set global rule: %v", err)
	}
	env := rc23W3RulesEnv(store, owner)
	// The owner sends from a group chat: the chat is the group's,
	// the sender is the owner.
	env.ChatID = 999
	ctx := WithSenderID(context.Background(), owner)
	reply := dispatchRulesCtx(t, env, ctx, "/rules")
	if strings.Contains(reply, "owner-only") {
		t.Errorf("/rules from the owner in a group = %q, want the rule list, not a refusal", reply)
	}
	if !strings.Contains(reply, "shell_exec") {
		t.Errorf("/rules from the owner in a group = %q, want the shell_exec rule listed", reply)
	}
	// And the owner in the group can delete, too.
	reply = dispatchRulesCtx(t, env, ctx, "/rules delete shell_exec")
	if strings.Contains(reply, "owner-only") {
		t.Errorf("/rules delete from the owner in a group = %q, want the deletion confirmation", reply)
	}
	if _, ok := store.Lookup("shell_exec"); ok {
		t.Error("delete: shell_exec rule still present after the owner deleted it from a group chat")
	}
}

func TestRulesGroupNonOwnerStillRefused(t *testing.T) {
	const owner = 7429947930
	store := permissions.NewRuleStore("")
	if err := store.Set("shell_exec", permissions.ActionAllow); err != nil {
		t.Fatalf("Set global rule: %v", err)
	}
	env := rc23W3RulesEnv(store, owner)
	// A non-owner member of the same group chat is still refused.
	env.ChatID = 999
	ctx := WithSenderID(context.Background(), 555)
	reply := dispatchRulesCtx(t, env, ctx, "/rules")
	if !strings.Contains(reply, "owner-only") {
		t.Errorf("/rules from a non-owner group member = %q, want an owner-only refusal", reply)
	}
	if _, ok := store.Lookup("shell_exec"); !ok {
		t.Error("refused /rules must not change the rule store")
	}
}

// --- F-A18-9: non-photo media -------------------------------------------

// mediaUpdate builds an update carrying one non-photo media kind.
func mediaUpdate(id, chatID, fromID int64, kind string) Update {
	msg := &TGMessage{
		MessageID: id,
		Chat:      TGChat{ID: chatID},
		From:      &TGUser{ID: fromID},
	}
	switch kind {
	case "sticker":
		msg.Sticker = &TGSticker{FileID: "sticker-1", Width: 512, Height: 512}
	case "voice":
		msg.Voice = &TGVoice{FileID: "voice-1", Duration: 7}
	case "video":
		msg.Video = &TGVideo{FileID: "video-1", Width: 640, Height: 480, Duration: 12}
	case "document":
		msg.Document = &TGDocument{FileID: "doc-1", FileName: "notes.pdf", MimeType: "application/pdf"}
	case "location":
		msg.Location = &TGLocation{Longitude: 103.85, Latitude: 1.29}
	case "animation":
		msg.Animation = &TGAnimation{FileID: "anim-1", FileName: "funny.gif", MimeType: "video/mp4", Width: 320, Height: 240, Duration: 5}
	case "audio":
		msg.Audio = &TGAudio{FileID: "audio-1", Duration: 180, Performer: "singer", Title: "song", MimeType: "audio/mpeg"}
	case "video_note":
		msg.VideoNote = &TGVideoNote{FileID: "vn-1", Length: 360, Duration: 10}
	case "contact":
		msg.Contact = &TGContact{PhoneNumber: "+1234567890", FirstName: "Irfan"}
	case "poll":
		msg.Poll = &TGPoll{ID: "poll-1", Question: "which one?", TotalVoterCount: 3}
	case "venue":
		msg.Venue = &TGVenue{Location: TGLocation{Longitude: 103.85, Latitude: 1.29}, Title: "Marina Bay", Address: "Singapore"}
	case "dice":
		msg.Dice = &TGDice{Emoji: "🎲", Value: 4}
	case "game":
		msg.Game = &TGGame{Title: "Chess", Description: "A game"}
	default:
		panic("mediaUpdate: unknown kind " + kind)
	}
	return Update{UpdateID: id, Message: msg}
}

func TestTGMessageMediaJSONDecodes(t *testing.T) {
	// A real Bot API voice update: no "text", a "voice" object.
	raw := `{"update_id":9,"message":{"message_id":3,"chat":{"id":77},"from":{"id":77},"voice":{"file_id":"v1","duration":7}}}`
	var upd Update
	if err := json.Unmarshal([]byte(raw), &upd); err != nil {
		t.Fatalf("unmarshal voice update: %v", err)
	}
	if got := upd.Message.MediaKind(); got != "voice" {
		t.Errorf("MediaKind = %q, want voice", got)
	}
	if !upd.Message.HasMedia() {
		t.Error("HasMedia must be true for a voice update")
	}
	if upd.Message.Voice == nil || upd.Message.Voice.FileID != "v1" || upd.Message.Voice.Duration != 7 {
		t.Errorf("voice: %+v", upd.Message.Voice)
	}
	if upd.Message.Text != "" {
		t.Errorf("voice update text = %q, want empty", upd.Message.Text)
	}
	// A location update carries coordinates, no file_id.
	raw = `{"update_id":10,"message":{"message_id":4,"chat":{"id":77},"from":{"id":77},"location":{"longitude":103.85,"latitude":1.29}}}`
	var locUpd Update
	if err := json.Unmarshal([]byte(raw), &locUpd); err != nil {
		t.Fatalf("unmarshal location update: %v", err)
	}
	if got := locUpd.Message.MediaKind(); got != "location" {
		t.Errorf("MediaKind = %q, want location", got)
	}
	if locUpd.Message.Location == nil || locUpd.Message.Location.Longitude != 103.85 || locUpd.Message.Location.Latitude != 1.29 {
		t.Errorf("location: %+v", locUpd.Message.Location)
	}
	// A document update keeps the file name and MIME type.
	raw = `{"update_id":11,"message":{"message_id":5,"chat":{"id":77},"from":{"id":77},"document":{"file_id":"d1","file_name":"notes.pdf","mime_type":"application/pdf"}}}`
	var docUpd Update
	if err := json.Unmarshal([]byte(raw), &docUpd); err != nil {
		t.Fatalf("unmarshal document update: %v", err)
	}
	if got := docUpd.Message.MediaKind(); got != "document" {
		t.Errorf("MediaKind = %q, want document", got)
	}
	if docUpd.Message.Document == nil || docUpd.Message.Document.FileName != "notes.pdf" {
		t.Errorf("document: %+v", docUpd.Message.Document)
	}
	// Plain text and empty messages carry no media; nil is safe.
	if (&TGMessage{Text: "hi"}).HasMedia() {
		t.Error("text message must not HasMedia")
	}
	if (&TGMessage{}).MediaKind() != "" {
		t.Error("empty message MediaKind must be empty")
	}
	if (*TGMessage)(nil).MediaKind() != "" || (*TGMessage)(nil).HasMedia() {
		t.Error("nil message must not report media")
	}
	// A photo is not "media" — HasPhoto covers it.
	if photoUpdate(1, 77, 77).Message.HasMedia() {
		t.Error("photo message must not HasMedia (photos are covered by HasPhoto)")
	}
}

func TestCannotProcessMediaMessage(t *testing.T) {
	for _, kind := range []string{"sticker", "voice", "video", "document", "location"} {
		reply := cannotProcessMediaMessage(kind)
		if !strings.Contains(reply, "can't process") {
			t.Errorf("cannotProcessMediaMessage(%q) = %q, want an honest cannot-process reply", kind, reply)
		}
	}
	if got := cannotProcessMediaMessage("bogus"); !strings.Contains(got, "can't process") {
		t.Errorf("cannotProcessMediaMessage(bogus) = %q, want the generic cannot-process form", got)
	}
}

func TestNonPhotoMediaGetsHonestReply(t *testing.T) {
	for _, kind := range []string{"sticker", "voice", "video", "document", "location"} {
		t.Run(kind, func(t *testing.T) {
			fake := &streamFake{updates: []Update{mediaUpdate(1, 77, 77, kind)}}
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

			want := cannotProcessMediaMessage(kind)
			waitFor(t, kind+" honest reply", func() bool {
				sent, _, _ := fake.snapshot()
				for _, m := range sent {
					if m.ChatID == 77 && m.Text == want {
						return true
					}
				}
				return false
			})
			if turns != 0 {
				t.Errorf("%s must not run a turn, but the handler ran %d time(s)", kind, turns)
			}
		})
	}
}

func TestNonPhotoMediaFromNonAllowlistedChatGetsNoReply(t *testing.T) {
	fake := &streamFake{updates: []Update{mediaUpdate(1, 77, 77, "voice")}}
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
		t.Fatalf("non-allowlisted voice note got a reply: %+v", sent)
	}
	if turns != 0 {
		t.Errorf("non-allowlisted voice note must not run a turn, ran %d", turns)
	}
}

func TestMediaWithTextStillHonestReply(t *testing.T) {
	// A video WITH a text caption still takes the media branch:
	// answering the text while ignoring the video would mislead,
	// exactly as with a photo (TestPhotoWithTextStillCannotSee).
	upd := mediaUpdate(1, 77, 77, "video")
	upd.Message.Text = "what is in this video?"
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

	want := cannotProcessMediaMessage("video")
	waitFor(t, "video honest reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if m.Text == want {
				return true
			}
		}
		return false
	})
	if turns != 0 {
		t.Errorf("video with text must not run a turn, ran %d", turns)
	}
}
