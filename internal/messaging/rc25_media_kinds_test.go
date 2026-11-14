// rc25 tests: F-A19-1 — the remaining media kinds (animation,
// audio, video_note, contact, poll, venue, dice, game) must not be
// silently dropped. Audit 19 found TGMessage had no fields for
// them, so MediaKind() returned "" and the silent-drop branch ate
// them; a captioned animation was worse — the turn ran on the text
// while the model never got the GIF. Each kind now decodes from a
// real Bot API update, reports its kind, and gets the same honest
// per-kind reply (and no turn) as the F-A18-9 kinds.
package messaging

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/core"
)

// remainingMediaKinds is every kind F-A19-1 adds, in MediaKind's
// fixed precedence order.
var remainingMediaKinds = []string{
	"animation", "audio", "video_note", "contact",
	"poll", "venue", "dice", "game",
}

func TestTGMessageRemainingMediaJSONDecodes(t *testing.T) {
	// Real Bot API shapes: each update carries the kind's object
	// and no "text", and MediaKind must name it.
	raw := map[string]string{
		"animation":  `{"update_id":21,"message":{"message_id":3,"chat":{"id":77},"from":{"id":77},"animation":{"file_id":"a1","file_name":"funny.gif","mime_type":"video/mp4","width":320,"height":240,"duration":5}}}`,
		"audio":      `{"update_id":22,"message":{"message_id":4,"chat":{"id":77},"from":{"id":77},"audio":{"file_id":"au1","duration":180,"performer":"singer","title":"song","mime_type":"audio/mpeg"}}}`,
		"video_note": `{"update_id":23,"message":{"message_id":5,"chat":{"id":77},"from":{"id":77},"video_note":{"file_id":"vn1","length":360,"duration":10}}}`,
		"contact":    `{"update_id":24,"message":{"message_id":6,"chat":{"id":77},"from":{"id":77},"contact":{"phone_number":"+1234567890","first_name":"Irfan"}}}`,
		"poll":       `{"update_id":25,"message":{"message_id":7,"chat":{"id":77},"from":{"id":77},"poll":{"id":"p1","question":"which one?","total_voter_count":3,"is_closed":false}}}`,
		"venue":      `{"update_id":26,"message":{"message_id":8,"chat":{"id":77},"from":{"id":77},"venue":{"location":{"longitude":103.85,"latitude":1.29},"title":"Marina Bay","address":"Singapore"}}}`,
		"dice":       `{"update_id":27,"message":{"message_id":9,"chat":{"id":77},"from":{"id":77},"dice":{"emoji":"🎲","value":4}}}`,
		"game":       `{"update_id":28,"message":{"message_id":10,"chat":{"id":77},"from":{"id":77},"game":{"title":"Chess","description":"A game"}}}`,
	}
	for _, kind := range remainingMediaKinds {
		t.Run(kind, func(t *testing.T) {
			var upd Update
			if err := json.Unmarshal([]byte(raw[kind]), &upd); err != nil {
				t.Fatalf("unmarshal %s update: %v", kind, err)
			}
			if got := upd.Message.MediaKind(); got != kind {
				t.Errorf("MediaKind = %q, want %q", got, kind)
			}
			if !upd.Message.HasMedia() {
				t.Errorf("HasMedia must be true for a %s update", kind)
			}
			if upd.Message.Text != "" {
				t.Errorf("%s update text = %q, want empty", kind, upd.Message.Text)
			}
		})
	}
	// Spot-check the decoded fields, not just the kind.
	var anim Update
	if err := json.Unmarshal([]byte(raw["animation"]), &anim); err != nil {
		t.Fatalf("unmarshal animation: %v", err)
	}
	if anim.Message.Animation == nil || anim.Message.Animation.FileName != "funny.gif" || anim.Message.Animation.Duration != 5 {
		t.Errorf("animation: %+v", anim.Message.Animation)
	}
	var contact Update
	if err := json.Unmarshal([]byte(raw["contact"]), &contact); err != nil {
		t.Fatalf("unmarshal contact: %v", err)
	}
	if contact.Message.Contact == nil || contact.Message.Contact.PhoneNumber != "+1234567890" || contact.Message.Contact.FirstName != "Irfan" {
		t.Errorf("contact: %+v", contact.Message.Contact)
	}
	var venue Update
	if err := json.Unmarshal([]byte(raw["venue"]), &venue); err != nil {
		t.Fatalf("unmarshal venue: %v", err)
	}
	if venue.Message.Venue == nil || venue.Message.Venue.Title != "Marina Bay" || venue.Message.Venue.Location.Latitude != 1.29 {
		t.Errorf("venue: %+v", venue.Message.Venue)
	}
	var dice Update
	if err := json.Unmarshal([]byte(raw["dice"]), &dice); err != nil {
		t.Fatalf("unmarshal dice: %v", err)
	}
	if dice.Message.Dice == nil || dice.Message.Dice.Value != 4 {
		t.Errorf("dice: %+v", dice.Message.Dice)
	}
}

func TestCannotProcessMediaMessageRemainingKinds(t *testing.T) {
	for _, kind := range remainingMediaKinds {
		reply := cannotProcessMediaMessage(kind)
		if !strings.Contains(reply, "can't process") {
			t.Errorf("cannotProcessMediaMessage(%q) = %q, want an honest cannot-process reply", kind, reply)
		}
	}
}

func TestRemainingMediaGetsHonestReply(t *testing.T) {
	for _, kind := range remainingMediaKinds {
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

func TestCaptionedAnimationStillHonestReply(t *testing.T) {
	// The audit's worst case: an animation WITH a text caption.
	// Before F-A19-1 the turn ran on the caption while the model
	// never got the GIF — a misleading answer. Media takes
	// precedence over text, exactly as with a captioned video.
	upd := mediaUpdate(1, 77, 77, "animation")
	upd.Message.Text = "what is in this gif?"
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

	want := cannotProcessMediaMessage("animation")
	waitFor(t, "animation honest reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if m.Text == want {
				return true
			}
		}
		return false
	})
	if turns != 0 {
		t.Errorf("captioned animation must not run a turn, ran %d", turns)
	}
}
