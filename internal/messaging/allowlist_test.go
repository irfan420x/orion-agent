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
			{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 999}, Text: "hello secret-text"}},
			{UpdateID: 2, Message: &TGMessage{MessageID: 2, Chat: TGChat{ID: 77}, Text: "hello"}},
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
