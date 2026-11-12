//go:build live

package messaging

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/config"
)

// Live-network tests against the real Telegram Bot API.
//
// Run only on purpose, with the test bot token already in the local
// config (~/.orion/config.json):
//
//	ORION_LIVE_TELEGRAM_TEST=1 go test ./internal/messaging/ -tags live -run TestLive -count=1 -v
//
// They are excluded from the default `go test ./...` run (build tag
// `live`) and skipped unless ORION_LIVE_TELEGRAM_TEST=1, so the suite
// stays green offline. The token is loaded through config.Load and is
// never logged; only the bot username and pass/fail are reported.

// liveChatID is the chat of the most recent incoming message, shared
// between the live tests (Go runs tests in a file in source order).
var (
	liveChatMu sync.Mutex
	liveChatID int64
)

// liveGateway loads the local config and builds a gateway for the real
// Telegram API. It skips the test unless live testing is opted into.
func liveGateway(t *testing.T) *TelegramGateway {
	t.Helper()
	if strings.TrimSpace(os.Getenv("ORION_LIVE_TELEGRAM_TEST")) != "1" {
		t.Skip("live Telegram test skipped: set ORION_LIVE_TELEGRAM_TEST=1 to run")
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if strings.TrimSpace(cfg.TelegramToken) == "" {
		t.Skip("live Telegram test skipped: no telegram_token in local config")
	}
	gw, err := NewTelegramGateway(cfg.TelegramToken)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	return gw
}

// TestLiveGetMe verifies the stored test token against the real Bot API.
func TestLiveGetMe(t *testing.T) {
	gw := liveGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	username, err := gw.GetMe(ctx)
	if err != nil {
		t.Fatalf("live GetMe: %v", err)
	}
	if username == "" {
		t.Fatal("live GetMe: empty username")
	}
	t.Logf("live GetMe ok: bot username @%s", username)
}

// TestLiveGetUpdates verifies a real long-poll round-trips and that the
// offset advances past any updates seen (no redelivery on the next call).
// It also records the chat ID of any incoming message for the send test.
// Each long-poll gets its own context: an idle poll waits the full
// 30s server timeout, so one context cannot cover two polls.
func TestLiveGetUpdates(t *testing.T) {
	gw := liveGateway(t)
	before := gw.Offset()
	ctx1, cancel1 := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel1()
	updates, err := gw.GetUpdates(ctx1)
	if err != nil {
		t.Fatalf("live GetUpdates: %v", err)
	}
	t.Logf("live GetUpdates ok: %d update(s), offset %d -> %d", len(updates), before, gw.Offset())
	for _, u := range updates {
		if u.Message != nil {
			t.Logf("live update %d: chat %d text %q", u.UpdateID, u.Message.Chat.ID, u.Message.Text)
			liveChatMu.Lock()
			if liveChatID == 0 {
				liveChatID = u.Message.Chat.ID
			}
			liveChatMu.Unlock()
		}
	}
	if gw.Offset() < before {
		t.Fatalf("live GetUpdates: offset went backwards %d -> %d", before, gw.Offset())
	}
	// A second call must not redeliver the same updates: with no new
	// messages it returns empty (long-poll waits the full server timeout
	// when idle, so this call takes up to ~30s).
	ctx2, cancel2 := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel2()
	updates2, err := gw.GetUpdates(ctx2)
	if err != nil {
		t.Fatalf("live second GetUpdates: %v", err)
	}
	for _, u := range updates2 {
		for _, prev := range updates {
			if u.UpdateID == prev.UpdateID {
				t.Fatalf("live GetUpdates: update %d redelivered on second call", u.UpdateID)
			}
		}
	}
	t.Logf("live second GetUpdates ok: %d new update(s)", len(updates2))
}

// TestLiveSendMessage sends one short test message to the chat that most
// recently messaged the bot (discovered by TestLiveGetUpdates). It is
// skipped when no chat has ever messaged the bot, because there is no
// safe chat_id to send to — a guessed id could message a stranger or
// fail spuriously, so no send is attempted in that case.
func TestLiveSendMessage(t *testing.T) {
	gw := liveGateway(t)
	liveChatMu.Lock()
	chatID := liveChatID
	liveChatMu.Unlock()
	if chatID == 0 {
		// No incoming message was observed; try one fresh drain first.
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		updates, err := gw.GetUpdates(ctx)
		if err != nil {
			t.Fatalf("live GetUpdates (send setup): %v", err)
		}
		for _, u := range updates {
			if u.Message != nil {
				chatID = u.Message.Chat.ID
				break
			}
		}
	}
	if chatID == 0 {
		t.Skip("live SendMessage skipped: no chat has messaged the bot, no safe chat_id to send to")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	text := "Orion v0.1 live gateway test — please ignore. (automated check, no reply needed)"
	if err := gw.SendMessage(ctx, chatID, text); err != nil {
		t.Fatalf("live SendMessage to chat %d: %v", chatID, err)
	}
	t.Logf("live SendMessage ok: 1 message delivered to chat %d", chatID)
}
