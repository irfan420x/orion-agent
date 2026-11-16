// Poll multi-log regression tests (F-A23-POLL): while a turn runs,
// the watermark stays behind its update, so every poll re-fetches
// the in-flight batch — and the old code logged every re-fetch, so
// one 5-minute turn produced 639 "received update" lines for 8
// distinct updates (update 181487784 alone 243x). Each update id
// must now be logged at most once, at first receipt.
package messaging

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/model"
)

// countingFake wraps queueFake and counts getUpdates requests, so a
// test can prove the poll loop really did re-fetch an update (the
// test is vacuous without that: zero re-fetches would also log once).
type countingFake struct {
	*queueFake
	getUpdates atomic.Int64
}

func (f *countingFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/getUpdates") {
		f.getUpdates.Add(1)
	}
	f.queueFake.ServeHTTP(w, r)
}

// TestPollLogsRefetchedUpdateAtMostOnce: a turn that blocks keeps
// its update in-flight, so the poll loop re-fetches it on every
// cycle — but the "received update" line for it must appear exactly
// once. It FAILS on the old code (one line per re-fetch).
func TestPollLogsRefetchedUpdateAtMostOnce(t *testing.T) {
	fake := &countingFake{queueFake: newQueueFake()}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	var logMu sync.Mutex
	var logLines []string
	gw, err := NewTelegramGateway(testToken,
		WithBaseURL(srv.URL),
		WithAllowedChats(77),
		WithLogger(func(format string, args ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			logLines = append(logLines, fmt.Sprintf(format, args...))
		}),
	)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	handler := func(ctx context.Context, chatID int64, text string, images []model.ImageContent, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return "", nil, nil
		case <-release:
			return "slow-done", nil, nil
		}
	}
	cancel, errCh := runPollStream(t, gw, handler)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		stopPollStream(t, cancel, errCh)
	}()

	fake.enqueue(busyMsg(1, "slow"))
	select {
	case <-started:
	case <-time.After(scaledTimeout(5 * time.Second)):
		t.Fatal("blocked turn did not start")
	}

	// Prove the re-fetch really happened: the loop must have
	// polled at least 3 times (first fetch + 2 re-fetches of the
	// in-flight update) before the log count means anything.
	waitFor(t, "at least 3 getUpdates fetches", func() bool {
		return fake.getUpdates.Load() >= 3
	})

	countReceived := func(id int64) int {
		needle := fmt.Sprintf("received update %d ", id)
		n := 0
		logMu.Lock()
		defer logMu.Unlock()
		for _, line := range logLines {
			if strings.Contains(line, needle) {
				n++
			}
		}
		return n
	}
	if got := countReceived(1); got != 1 {
		t.Errorf("received-update lines for update 1 = %d after %d fetches, want exactly 1 (first receipt only)",
			got, fake.getUpdates.Load())
	}

	close(release)
	waitFor(t, "offset past the slow turn", func() bool { return gw.Offset() >= 2 })
}
