package messaging

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Telegram allowlist startup enforcement (ChatGPT rc2 audit,
// Finding 3) ---

// TestCheckTelegramAllowlistEmptyRefuses: an empty allowlist with no
// explicit opt-in must refuse startup, and the error must tell the
// operator how to find their chat ID and how to fix the config.
func TestCheckTelegramAllowlistEmptyRefuses(t *testing.T) {
	err := CheckTelegramAllowlist(nil, false)
	if err == nil {
		t.Fatal("empty allowlist without allow-all: got nil, want an error")
	}
	for _, want := range []string{
		"telegram_allowed_chats",
		"allow_all_telegram_chats",
		"/whoami",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal error %q does not mention %q", err.Error(), want)
		}
	}
	if err := CheckTelegramAllowlist([]int64{}, false); err == nil {
		t.Fatal("empty (non-nil) allowlist without allow-all: got nil, want an error")
	}
}

// TestCheckTelegramAllowlistAllowAllPermits: the explicit opt-in lets
// startup proceed with an empty allowlist.
func TestCheckTelegramAllowlistAllowAllPermits(t *testing.T) {
	if err := CheckTelegramAllowlist(nil, true); err != nil {
		t.Errorf("empty allowlist with allow-all: %v, want nil", err)
	}
}

// TestCheckTelegramAllowlistNonEmptyPermits: a non-empty allowlist
// passes with or without the opt-in.
func TestCheckTelegramAllowlistNonEmptyPermits(t *testing.T) {
	if err := CheckTelegramAllowlist([]int64{42}, false); err != nil {
		t.Errorf("allowlist [42]: %v, want nil", err)
	}
	if err := CheckTelegramAllowlist([]int64{42, -7}, true); err != nil {
		t.Errorf("allowlist [42 -7] with allow-all: %v, want nil", err)
	}
}

// TestP2IsChatAllowedAllowAllWindow: the allow-all opt-in (P2-O5)
// allows every chat inside its window from gateway creation and
// refuses every chat once the window elapses. The window is measured
// from creation, not from first use.
func TestP2IsChatAllowedAllowAllWindow(t *testing.T) {
	g, err := NewTelegramGateway(testToken, WithAllowAll(10*time.Minute))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if !g.IsChatAllowed(12345) {
		t.Error("IsChatAllowed inside the allow-all window = false, want true")
	}
	// 5 minutes in: still allowed.
	g.allowAllSince = time.Now().Add(-5 * time.Minute)
	if !g.IsChatAllowed(12345) {
		t.Error("IsChatAllowed 5m into a 10m window = false, want true")
	}
	// 11 minutes in: the window has elapsed — every chat refused.
	g.allowAllSince = time.Now().Add(-11 * time.Minute)
	if g.IsChatAllowed(12345) {
		t.Error("IsChatAllowed 11m into a 10m window = true, want false (auto-disabled)")
	}
	if g.IsChatAllowed(99999) {
		t.Error("IsChatAllowed for another chat after expiry = true, want false")
	}
}

// TestP2IsChatAllowedAllowAllExpiryLoggedOnce: the expiry is logged
// once (via the gateway logger), not on every dropped message.
func TestP2IsChatAllowedAllowAllExpiryLoggedOnce(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	g, err := NewTelegramGateway(testToken,
		WithAllowAll(10*time.Minute),
		WithLogger(func(format string, args ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(format, args...))
			mu.Unlock()
		}))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	g.allowAllSince = time.Now().Add(-11 * time.Minute)
	for i := 0; i < 3; i++ {
		if g.IsChatAllowed(int64(100 + i)) {
			t.Errorf("IsChatAllowed after expiry (call %d) = true, want false", i)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	expired := 0
	for _, line := range logs {
		if strings.Contains(line, "expired") {
			expired++
		}
	}
	if expired != 1 {
		t.Errorf("expiry logged %d time(s), want exactly 1; logs: %q", expired, logs)
	}
}

// TestP2IsChatAllowedAllowlistBeatsAllowAll: when an allowlist is
// set, it wins over the allow-all opt-in — a non-listed chat is
// refused even inside the window.
func TestP2IsChatAllowedAllowlistBeatsAllowAll(t *testing.T) {
	g, err := NewTelegramGateway(testToken,
		WithAllowedChats(77),
		WithAllowAll(10*time.Minute))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if !g.IsChatAllowed(77) {
		t.Error("IsChatAllowed(77) with allowlist [77] = false, want true")
	}
	if g.IsChatAllowed(999) {
		t.Error("IsChatAllowed(999) with allowlist [77] + allow-all = true, want false (allowlist wins)")
	}
}

// TestP2IsChatAllowedNoOptInUnchanged: a gateway with no allowlist
// and no opt-in keeps the old behaviour (all chats pass) — the
// time-box only applies to the explicit opt-in.
func TestP2IsChatAllowedNoOptInUnchanged(t *testing.T) {
	g, err := NewTelegramGateway(testToken)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if !g.IsChatAllowed(12345) {
		t.Error("IsChatAllowed with no allowlist and no opt-in = false, want true (unchanged behaviour)")
	}
}

// TestP2WithAllowAllDefaultWindow: a non-positive window means the
// 10-minute default, and the start time is gateway creation.
func TestP2WithAllowAllDefaultWindow(t *testing.T) {
	g, err := NewTelegramGateway(testToken, WithAllowAll(0))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if g.allowAllWindow != DefaultAllowAllWindow {
		t.Errorf("allowAllWindow = %s, want %s (default)", g.allowAllWindow, DefaultAllowAllWindow)
	}
	if g.allowAllSince.IsZero() || time.Since(g.allowAllSince) > time.Minute {
		t.Errorf("allowAllSince = %v, want ~now", g.allowAllSince)
	}
	if !g.IsChatAllowed(1) {
		t.Error("IsChatAllowed with default window at creation = false, want true")
	}
}

// TestPollLogsDroppedChatID: a message from a non-allowlisted chat is
// dropped before the handler (already covered by
// TestPollEnforcesAllowedChats), and the drop is logged with the chat
// ID — that log line is how an operator who has not allowlisted
// themselves yet finds their chat ID to add to
// telegram_allowed_chats. The message text is never logged.
func TestPollLogsDroppedChatID(t *testing.T) {
	fake := &fakeTelegram{
		sendCh: make(chan sentMessage, 10),
		updates: []Update{
			{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 999}, Text: "hello secret-text", From: &TGUser{ID: 999}}},
			{UpdateID: 2, Message: &TGMessage{MessageID: 2, Chat: TGChat{ID: 77}, Text: "hello", From: &TGUser{ID: 77}}},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	var mu sync.Mutex
	var logs []string
	g, err := NewTelegramGateway(testToken,
		WithBaseURL(srv.URL),
		WithAllowedChats(77),
		WithLogger(func(format string, args ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(format, args...))
			mu.Unlock()
		}))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- g.Poll(ctx, func(ctx context.Context, chatID int64, text string) (string, error) {
			return "pong", nil
		})
	}()

	// The allowed chat's reply proves the poll loop processed both
	// updates (the stranger's is first, so its drop is logged before
	// this reply is sent).
	select {
	case sent := <-fake.sendCh:
		if sent.ChatID != 77 || sent.Text != "pong" {
			t.Fatalf("Poll reply = %+v, want {ChatID:77 Text:pong}", sent)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for Poll to reply to the allowed chat")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Poll after cancel: %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not return after ctx cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, line := range logs {
		if strings.Contains(line, "secret-text") {
			t.Fatalf("dropped-chat log leaked message text: %q", line)
		}
		if strings.Contains(line, "999") && strings.Contains(line, "telegram_allowed_chats") {
			return
		}
	}
	t.Fatalf("no dropped-chat log line for chat 999; log lines: %q", logs)
}
