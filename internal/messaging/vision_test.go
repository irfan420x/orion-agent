package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/model"
)

// visionJPEG is a minimal JPEG (header + a few bytes): enough for
// the MIME sniffer, small enough for tests.
var visionJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x02, 0x03}

// visionGateway builds a gateway against the fake server with a
// fixed (provider, model) for vision decisions.
func visionGateway(t *testing.T, srv *httptest.Server, provider, modelName string) *TelegramGateway {
	t.Helper()
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL),
		WithModelInfo(func(chatID int64) (string, string) { return provider, modelName }))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	return g
}

// visionTurn records the turns a handler receives: text, images,
// and count.
type visionTurn struct {
	mu     sync.Mutex
	texts  []string
	images [][]model.ImageContent
}

func (v *visionTurn) handler(reply string) TurnFunc {
	return func(ctx context.Context, chatID int64, text string, images []model.ImageContent, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		v.mu.Lock()
		v.texts = append(v.texts, text)
		v.images = append(v.images, images)
		v.mu.Unlock()
		return reply, nil, nil
	}
}

func (v *visionTurn) snapshot() (texts []string, images [][]model.ImageContent) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.texts...), append([][]model.ImageContent(nil), v.images...)
}

func TestCaptionIsParsedFromMessageJSON(t *testing.T) {
	var m TGMessage
	if err := json.Unmarshal([]byte(`{"message_id":1,"chat":{"id":77},"caption":"এই ছবিতে কী আছে?","photo":[{"file_id":"big","width":1280,"height":960}]}`), &m); err != nil {
		t.Fatalf("unmarshal TGMessage: %v", err)
	}
	if m.Caption != "এই ছবিতে কী আছে?" {
		t.Errorf("Caption = %q, want the caption text", m.Caption)
	}
	if !m.HasPhoto() {
		t.Error("HasPhoto = false, want true")
	}
}

func TestPhotoWithVisionRunsTurnWithImage(t *testing.T) {
	fake := &streamFake{
		updates:   []Update{photoUpdate(1, 77, 77)},
		fileBytes: map[string][]byte{"big": visionJPEG, "small": visionJPEG},
	}
	// The caption is the user's question about the photo.
	fake.updates[0].Message.Caption = "এই ছবিতে কী আছে?"
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := visionGateway(t, srv, "deepseek", "deepseek-flash")
	turns := &visionTurn{}
	cancel, errCh := runPollStream(t, g, turns.handler("a cat"))
	defer stopPollStream(t, cancel, errCh)

	waitFor(t, "vision turn reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if m.Text == "a cat" {
				return true
			}
		}
		return false
	})
	texts, images := turns.snapshot()
	if len(texts) != 1 || texts[0] != "এই ছবিতে কী আছে?" {
		t.Fatalf("turn texts = %q, want the caption as the text", texts)
	}
	if len(images) != 1 || len(images[0]) != 1 {
		t.Fatalf("turn images = %v, want exactly one image", images)
	}
	if got := images[0][0]; string(got.Data) != string(visionJPEG) || got.MIMEType != "image/jpeg" {
		t.Errorf("image = %d bytes %q, want the JPEG bytes as image/jpeg", len(got.Data), got.MIMEType)
	}
	// The largest size was downloaded, not the small one.
	fake.mu.Lock()
	ids := append([]string(nil), fake.getFileIDs...)
	fake.mu.Unlock()
	if len(ids) != 1 || ids[0] != "big" {
		t.Errorf("getFile file_ids = %v, want [big] (largest size)", ids)
	}
	// No cannot-see reply was sent.
	sent, _, _ := fake.snapshot()
	for _, m := range sent {
		if m.Text == CannotSeeImagesMessage {
			t.Errorf("vision-capable chat got the cannot-see reply")
		}
	}
}

func TestPhotoWithoutCaptionUsesDefaultPrompt(t *testing.T) {
	fake := &streamFake{
		updates:   []Update{photoUpdate(1, 77, 77)},
		fileBytes: map[string][]byte{"big": visionJPEG},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := visionGateway(t, srv, "deepseek", "deepseek-flash")
	turns := &visionTurn{}
	cancel, errCh := runPollStream(t, g, turns.handler("a cat"))
	defer stopPollStream(t, cancel, errCh)

	waitFor(t, "vision turn reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if m.Text == "a cat" {
				return true
			}
		}
		return false
	})
	texts, _ := turns.snapshot()
	if len(texts) != 1 || texts[0] != PhotoDefaultPrompt {
		t.Fatalf("turn text = %q, want the default prompt %q", texts, PhotoDefaultPrompt)
	}
}

func TestPhotoNonVisionGetsBengaliFallbackWithSuggestion(t *testing.T) {
	fake := &streamFake{updates: []Update{photoUpdate(1, 77, 77)}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	// deepseek-chat is the retired text-only model: no vision.
	g := visionGateway(t, srv, "deepseek", "deepseek-chat")
	turns := 0
	handler := func(ctx context.Context, chatID int64, text string, images []model.ImageContent, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		turns++
		return "turn reply", nil, nil
	}
	cancel, errCh := runPollStream(t, g, handler)
	defer stopPollStream(t, cancel, errCh)

	waitFor(t, "Bengali cannot-see reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if m.Text == CannotSeeImagesMessage {
				return true
			}
		}
		return false
	})
	if turns != 0 {
		t.Errorf("non-vision photo must not run a turn, ran %d", turns)
	}
	// The message is Bengali and names the fix.
	if !strings.Contains(CannotSeeImagesMessage, "দেখতে পারছি না") {
		t.Errorf("cannot-see message is not Bengali: %q", CannotSeeImagesMessage)
	}
	if !strings.Contains(CannotSeeImagesMessage, "deepseek-flash") {
		t.Errorf("cannot-see message must name deepseek-flash as the fix: %q", CannotSeeImagesMessage)
	}
	// No download was attempted for a model that cannot see.
	fake.mu.Lock()
	ids := append([]string(nil), fake.getFileIDs...)
	fake.mu.Unlock()
	if len(ids) != 0 {
		t.Errorf("getFile was called %v for a non-vision model, want no download", ids)
	}
}

func TestPhotoDownloadFailureGetsHonestReply(t *testing.T) {
	fake := &streamFake{
		updates:     []Update{photoUpdate(1, 77, 77)},
		failGetFile: true,
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := visionGateway(t, srv, "deepseek", "deepseek-flash")
	turns := 0
	handler := func(ctx context.Context, chatID int64, text string, images []model.ImageContent, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		turns++
		return "turn reply", nil, nil
	}
	cancel, errCh := runPollStream(t, g, handler)
	defer stopPollStream(t, cancel, errCh)

	waitFor(t, "download-failed reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if m.Text == PhotoDownloadFailedMessage {
				return true
			}
		}
		return false
	})
	if turns != 0 {
		t.Errorf("failed download must not run a turn, ran %d", turns)
	}
}

func TestPhotoTooLargeGetsTooLargeReply(t *testing.T) {
	big := make([]byte, MaxPhotoBytes+1)
	copy(big, visionJPEG)
	fake := &streamFake{
		updates:   []Update{photoUpdate(1, 77, 77)},
		fileBytes: map[string][]byte{"big": big},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := visionGateway(t, srv, "deepseek", "deepseek-flash")
	turns := 0
	handler := func(ctx context.Context, chatID int64, text string, images []model.ImageContent, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		turns++
		return "turn reply", nil, nil
	}
	cancel, errCh := runPollStream(t, g, handler)
	defer stopPollStream(t, cancel, errCh)

	waitFor(t, "too-large reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if m.Text == PhotoTooLargeMessage {
				return true
			}
		}
		return false
	})
	if turns != 0 {
		t.Errorf("oversize photo must not run a turn, ran %d", turns)
	}
}

func TestDownloadPhotoCapsAtMaxPhotoBytes(t *testing.T) {
	// Direct unit test of the cap: a file just over the cap is
	// refused even when getFile succeeds.
	fake := &streamFake{fileBytes: map[string][]byte{"x": make([]byte, MaxPhotoBytes+1)}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if _, err := g.downloadPhoto(context.Background(), "x"); !errors.Is(err, errPhotoTooLarge) {
		t.Fatalf("downloadPhoto over the cap: err = %v, want errPhotoTooLarge", err)
	}
	data, err := g.downloadPhoto(context.Background(), "missing")
	if err != nil {
		t.Fatalf("downloadPhoto of an absent fake file: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("downloadPhoto absent file = %d bytes, want 0", len(data))
	}
}

func TestVisionErrorStillUsesSameBengaliMessage(t *testing.T) {
	// The isVisionError path (provider rejects image input) must
	// use the SAME Bengali message as the photo path — one honest
	// reply for one situation, not two wordings.
	err := errors.New("deepseek: request failed with status 400: image input is not supported for this model")
	if got := userErrorMessage(err); got != CannotSeeImagesMessage {
		t.Errorf("userErrorMessage(vision error) = %q, want CannotSeeImagesMessage", got)
	}
}

func TestVisionSupportedNeedsModelInfo(t *testing.T) {
	g, err := NewTelegramGateway(testToken)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if g.visionSupported(77) {
		t.Error("gateway without modelInfo must report vision unsupported (fail closed)")
	}
	g2, err := NewTelegramGateway(testToken, WithModelInfo(func(chatID int64) (string, string) {
		return "openai", "gpt-4o-mini"
	}))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if !g2.visionSupported(77) {
		t.Error("openai/gpt-4o-mini must report vision supported")
	}
}
