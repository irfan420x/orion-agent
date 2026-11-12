package messaging

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Hardening tests (audit P0-2, P1-14) ---

// TestPollEnforcesAllowedChats: with an allowlist set, messages from a
// stranger chat must never reach the handler (and never get a reply),
// while messages from an allowed chat are delivered normally. Without
// this, anyone who discovers the bot username could drive the OS agent.
func TestPollEnforcesAllowedChats(t *testing.T) {
	fake := &fakeTelegram{
		sendCh: make(chan sentMessage, 10),
		updates: []Update{
			{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 999}, Text: "rm -rf /"}},
			{UpdateID: 2, Message: &TGMessage{MessageID: 2, Chat: TGChat{ID: 77}, Text: "hello"}},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if g.IsChatAllowed(999) {
		t.Fatal("IsChatAllowed(999) = true, want false for a chat outside the allowlist")
	}
	if !g.IsChatAllowed(77) {
		t.Fatal("IsChatAllowed(77) = false, want true for an allowed chat")
	}

	var mu sync.Mutex
	var calls []int64
	handler := func(ctx context.Context, chatID int64, text string) (string, error) {
		mu.Lock()
		calls = append(calls, chatID)
		mu.Unlock()
		return "pong", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- g.Poll(ctx, handler) }()

	// Only the allowed chat's message may produce a reply.
	select {
	case sent := <-fake.sendCh:
		if sent.ChatID != 77 || sent.Text != "pong" {
			t.Fatalf("Poll reply = %+v, want {ChatID:77 Text:pong}", sent)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for Poll to reply to the allowed chat")
	}
	mu.Lock()
	got := append([]int64(nil), calls...)
	mu.Unlock()
	if len(got) != 1 || got[0] != 77 {
		t.Fatalf("handler chats = %v, want [77] (stranger chat 999 must never reach the handler)", got)
	}
	// No second reply (the stranger's message must not be answered).
	select {
	case sent := <-fake.sendCh:
		t.Fatalf("unexpected second reply: %+v (stranger message was answered)", sent)
	case <-time.After(300 * time.Millisecond):
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
}

// TestIsChatAllowedNoAllowlist: with no allowlist configured, every
// chat is allowed (first-run convenience; the daemon warns about it).
func TestIsChatAllowedNoAllowlist(t *testing.T) {
	g, err := NewTelegramGateway(testToken)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if !g.IsChatAllowed(12345) || !g.IsChatAllowed(-678) {
		t.Fatal("with no allowlist, every chat must be allowed")
	}
}

// TestRunCLICancelledWhileWaiting: cancelling ctx while RunCLI is
// blocked waiting for input must return promptly — the old blocking
// scanner.Scan() ignored ctx until a line arrived or EOF.
func TestRunCLICancelledWhileWaiting(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	var out strings.Builder
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunCLI(ctx, pr, &out, func(ctx context.Context, input string) (string, error) {
			return "reply", nil
		})
	}()
	time.AfterFunc(200*time.Millisecond, cancel)
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("RunCLI after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunCLI did not return after ctx cancel while waiting for input")
	}
}
