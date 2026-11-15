package messaging

// F-V1 (vision report §7, P2): the legacy Poll wrapper adapts
// PollStream's image-carrying TurnFunc down to a text-only handler.
// These tests pin Poll's fail-closed contract: a photo turn must
// never run on its caption alone — the handler is never called
// with the caption, and the chat gets the honest Bengali
// cannot-see reply. Poll is a text-only API; image turns belong
// to PollStream + TurnFunc (see cmd/orion's daemonTurnHandler).
import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestPollVisionPhotoNeverRunsCaptionOnlyTurn (T1) is the F-V1 trap
// test: with a vision-capable model wired, a photo update driven
// through the legacy Poll must NOT invoke the text-only handler —
// the image would otherwise be silently dropped and the turn
// would run on the caption alone. The getFile assertion proves
// the test actually reached the vision branch (the largest size
// was downloaded) and is not passing vacuously via the
// cannot-see-before-download path.
func TestPollVisionPhotoNeverRunsCaptionOnlyTurn(t *testing.T) {
	fake := &streamFake{
		updates:   []Update{photoUpdate(1, 77, 77)},
		fileBytes: map[string][]byte{"big": visionJPEG, "small": visionJPEG},
	}
	// The caption is the user's question about the photo.
	fake.updates[0].Message.Caption = "এই ছবিতে কী আছে?"
	srv := httptest.NewServer(fake)
	defer srv.Close()
	// Vision-capable model, as the CLI path reports it once
	// newTelegramGateway wires WithModelInfo.
	g := visionGateway(t, srv, "deepseek", "deepseek-flash")

	var mu sync.Mutex
	var gotTexts []string
	handler := func(ctx context.Context, chatID int64, text string) (string, error) {
		mu.Lock()
		gotTexts = append(gotTexts, text)
		mu.Unlock()
		return "turn reply", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- g.Poll(ctx, handler) }()

	// Wait until the chat has been answered (the cannot-see
	// reply, once Poll fails closed on image turns).
	waitFor(t, "F-V1 photo reply", func() bool {
		sent, _, _ := fake.snapshot()
		return len(sent) > 0
	})
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Poll after cancel: %v, want nil", err)
		}
	case <-time.After(scaledTimeout(5 * time.Second)):
		t.Fatal("Poll did not return after ctx cancel")
	}

	mu.Lock()
	texts := append([]string(nil), gotTexts...)
	mu.Unlock()
	if len(texts) != 0 {
		t.Errorf("legacy Poll invoked the text-only handler %d time(s) with %q for a photo update: the image was silently dropped and the turn ran on the caption alone", len(texts), texts)
	}
	sent, _, _ := fake.snapshot()
	for _, m := range sent {
		if m.Text == "turn reply" {
			t.Errorf("chat got the confabulated turn reply %q for a photo the handler never saw", m.Text)
		}
	}
	// Fail closed, not silent: the user must get the honest reply.
	foundCannotSee := false
	for _, m := range sent {
		if m.Text == CannotSeeImagesMessage {
			foundCannotSee = true
		}
	}
	if !foundCannotSee {
		t.Errorf("chat got no cannot-see reply; sent = %v, want CannotSeeImagesMessage", sent)
	}
	// The photo WAS downloaded (largest size): the guard fired
	// after the vision branch, not before it.
	fake.mu.Lock()
	ids := append([]string(nil), fake.getFileIDs...)
	fake.mu.Unlock()
	if len(ids) != 1 || ids[0] != "big" {
		t.Errorf("getFile file_ids = %v, want [big] (largest size — the vision branch must have run)", ids)
	}
}

// TestPollNonVisionPhotoStaysCannotSee (T2) is the no-regression
// guard for the non-vision path: a photo from a text-only model
// still gets the cannot-see reply with no download and no turn.
func TestPollNonVisionPhotoStaysCannotSee(t *testing.T) {
	fake := &streamFake{updates: []Update{photoUpdate(1, 77, 77)}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	// deepseek-chat is the retired text-only model: no vision.
	g := visionGateway(t, srv, "deepseek", "deepseek-chat")

	var mu sync.Mutex
	calls := 0
	handler := func(ctx context.Context, chatID int64, text string) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return "turn reply", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- g.Poll(ctx, handler) }()

	waitFor(t, "non-vision cannot-see reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if m.Text == CannotSeeImagesMessage {
				return true
			}
		}
		return false
	})
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Poll after cancel: %v, want nil", err)
		}
	case <-time.After(scaledTimeout(5 * time.Second)):
		t.Fatal("Poll did not return after ctx cancel")
	}

	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 0 {
		t.Errorf("non-vision photo must not run a turn, handler ran %d time(s)", n)
	}
	// No download was attempted for a model that cannot see.
	fake.mu.Lock()
	ids := append([]string(nil), fake.getFileIDs...)
	fake.mu.Unlock()
	if len(ids) != 0 {
		t.Errorf("getFile was called %v for a non-vision model, want no download", ids)
	}
}
