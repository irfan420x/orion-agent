package app

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/model"
)

// cronCountingProvider is a stub provider that sleeps on every
// call and records the maximum concurrent Chat/Stream calls, so
// the turn-serialisation test can prove two turns for one chat
// never overlap inside the provider.
type cronCountingProvider struct {
	stubProvider
	mu          sync.Mutex
	inFlight    int
	maxInFlight int
	calls       int
}

func (p *cronCountingProvider) trackStart() {
	p.mu.Lock()
	p.inFlight++
	if p.inFlight > p.maxInFlight {
		p.maxInFlight = p.inFlight
	}
	p.calls++
	p.mu.Unlock()
}

func (p *cronCountingProvider) trackEnd() {
	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
}

func (p *cronCountingProvider) max() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxInFlight
}

func (p *cronCountingProvider) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *cronCountingProvider) Chat(ctx context.Context, req model.ChatRequest) (*model.ChatResponse, error) {
	p.trackStart()
	defer p.trackEnd()
	time.Sleep(100 * time.Millisecond)
	return p.stubProvider.Chat(ctx, req)
}

func (p *cronCountingProvider) Stream(ctx context.Context, req model.ChatRequest, fn func(chunk string) error) (*model.ChatResponse, error) {
	p.trackStart()
	defer p.trackEnd()
	time.Sleep(100 * time.Millisecond)
	return p.stubProvider.Stream(ctx, req, fn)
}

// TestCronSameChatTurnsSerialise proves the per-chat turn lock in
// runChatTurn: two concurrent turns for ONE chat never overlap
// inside the provider (max in-flight == 1), so a scheduled cron
// turn can never interleave with a user turn in the same chat
// and corrupt that chat's history. It is the lock the scheduler
// relies on — PollStream's per-chat lock does not cover
// scheduler-fired turns.
func TestCronSameChatTurnsSerialise(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	p := &cronCountingProvider{}
	a.Provider = p

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = a.HandleStreamForChat(context.Background(), 111, fmt.Sprintf("turn %d", i), nil)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	if p.total() != 2 {
		t.Fatalf("provider calls = %d, want 2", p.total())
	}
	if got := p.max(); got != 1 {
		t.Fatalf("max concurrent provider calls = %d, want 1 (same-chat turns must serialise)", got)
	}
}

// TestCronDifferentChatTurnsRunInParallel proves the lock is
// per-chat, not global: two turns for DIFFERENT chats both
// complete (the lock for chat 111 never blocks chat 222).
func TestCronDifferentChatTurnsRunInParallel(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	p := &cronCountingProvider{}
	a.Provider = p

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, chatID := range []int64{111, 222} {
		wg.Add(1)
		go func(i int, chatID int64) {
			defer wg.Done()
			_, _, errs[i] = a.HandleStreamForChat(context.Background(), chatID, "hello", nil)
		}(i, chatID)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	if p.total() != 2 {
		t.Fatalf("provider calls = %d, want 2", p.total())
	}
}

// cronBlockingProvider blocks the FIRST provider call (Chat or
// Stream) until released, and passes every later call straight
// through to the stub. It lets a test park one turn inside the
// provider while another channel's turn runs.
type cronBlockingProvider struct {
	stubProvider
	mu      sync.Mutex
	n       int
	started chan struct{}
	release chan struct{}
}

func (p *cronBlockingProvider) gate() {
	p.mu.Lock()
	p.n++
	first := p.n == 1
	p.mu.Unlock()
	if first {
		close(p.started)
		<-p.release
	}
}

func (p *cronBlockingProvider) Chat(ctx context.Context, req model.ChatRequest) (*model.ChatResponse, error) {
	p.gate()
	return p.stubProvider.Chat(ctx, req)
}

func (p *cronBlockingProvider) Stream(ctx context.Context, req model.ChatRequest, fn func(chunk string) error) (*model.ChatResponse, error) {
	p.gate()
	return p.stubProvider.Stream(ctx, req, fn)
}

// TestCronCLIChannelNotSerialisedByTurnLock proves F-C23 is
// intended, not a regression: the per-chat turn lock covers
// per-chat (Telegram/scheduled) turns only. A CLI-channel turn
// (App.Handle — `orion chat` / `orion run`, on the global
// agent) completes while a per-chat turn for a chat is parked
// inside the provider holding that chat's turn lock, so the CLI
// channel never waits behind it. (The CLI channel has no chat
// id and never enters runChatTurn; cmd/orion/main.go:1149 is
// the CLI binary's Telegram path, not the interactive CLI.)
func TestCronCLIChannelNotSerialisedByTurnLock(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	p := &cronBlockingProvider{started: make(chan struct{}), release: make(chan struct{})}
	a.Provider = p

	// Park a per-chat turn inside the provider: it holds chat
	// 111's turn lock for as long as it is parked.
	turnDone := make(chan error, 1)
	go func() {
		_, _, err := a.HandleStreamForChat(context.Background(), 111, "hello", nil)
		turnDone <- err
	}()
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		close(p.release)
		t.Fatal("timed out waiting for the chat turn to reach the provider")
	}

	// The CLI channel must complete while the chat turn is still
	// parked — if App.Handle waited on the turn lock, this would
	// block until the release below.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reply, err := a.Handle(ctx, "hello")
	if err != nil {
		close(p.release)
		t.Fatalf("CLI Handle while a chat turn is parked: %v", err)
	}
	if reply == "" {
		close(p.release)
		t.Fatal("CLI Handle returned an empty reply")
	}

	close(p.release)
	select {
	case err := <-turnDone:
		if err != nil {
			t.Fatalf("parked chat turn: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parked chat turn did not finish after release")
	}
}

// TestCronExcludedFromChildRegistry proves subagent children do
// not get the cron tools: scheduling is a parent-chat commitment
// (recurring turns, token spend), so a child running a brief must
// not commit the parent's chat to recurring work or cancel its
// jobs.
func TestCronExcludedFromChildRegistry(t *testing.T) {
	reg, err := newChildRegistry(config.Default(), nil)
	if err != nil {
		t.Fatalf("newChildRegistry: %v", err)
	}
	for _, name := range []string{"cron_schedule", "cron_list", "cron_cancel", "cron_enable"} {
		if _, ok := reg.Get(name); ok {
			t.Fatalf("child registry has %q, want it excluded", name)
		}
	}
}
