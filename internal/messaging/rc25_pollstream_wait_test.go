// rc25 tests: PollStream waits for in-flight reply handlers —
// regression for the data race found in the rc25 clean-clone -race
// run. TestW4's callback handler logged "context canceled" AFTER
// PollStream returned, racing TestDaemonDebugEndpointEnabled's
// capture() swap of the global os.Stderr (g.logf reads that global
// at call time). PollStream now tracks the reply handlers that log
// via g.logf (callback, photo, media) in a WaitGroup and waits for
// them before returning, so no handler logs after return.
package messaging

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
)

// TestPollStreamWaitsForCallbackHandler: a callback handler that is
// still running when PollStream is canceled must finish before
// PollStream returns. Without the WaitGroup, PollStream returns
// immediately on ctx cancel and the handler logs after return —
// the data race the -race run caught. With the WaitGroup, PollStream
// waits, so no handler logs after return.
func TestPollStreamWaitsForCallbackHandler(t *testing.T) {
	fake := newQueueFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	g.SetCallbackHandler(func(ctx context.Context, q TGCallbackQuery) error {
		close(started)
		<-release
		return nil
	})

	turnHandler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		return "ok", nil, nil
	}
	cancel, errCh := runPollStream(t, g, turnHandler)
	// Cleanup on any failure path: unblock the handler and cancel,
	// so no goroutine is left running after the test.
	defer func() {
		cancel()
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	fake.enqueue(Update{UpdateID: 1, CallbackQuery: &TGCallbackQuery{
		ID:      "cb-wait-1",
		From:    &TGUser{ID: 77},
		Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 77}},
		Data:    "test:wait",
	}})
	// Wait for the handler to start (it blocks on release).
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("callback handler did not start")
	}

	// Cancel PollStream while the handler is still blocked.
	cancel()
	// PollStream must NOT return while the handler is blocked.
	select {
	case err := <-errCh:
		t.Fatalf("PollStream returned %v while the callback handler was still running, want it to wait for the handler", err)
	case <-time.After(300 * time.Millisecond):
		// Still waiting — correct.
	}

	// Release the handler; PollStream must return promptly now.
	close(release)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("PollStream after cancel + handler done: %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PollStream did not return after the callback handler finished")
	}
}
