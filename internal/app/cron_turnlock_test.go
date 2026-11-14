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

// TestCronExcludedFromChildRegistry proves subagent children do
// not get the cron tools: scheduling is a parent-chat commitment
// (recurring turns, token spend), so a child running a brief must
// not commit the parent's chat to recurring work or cancel its
// jobs.
func TestCronExcludedFromChildRegistry(t *testing.T) {
	reg, err := newChildRegistry(config.Default())
	if err != nil {
		t.Fatalf("newChildRegistry: %v", err)
	}
	for _, name := range []string{"cron_schedule", "cron_list", "cron_cancel"} {
		if _, ok := reg.Get(name); ok {
			t.Fatalf("child registry has %q, want it excluded", name)
		}
	}
}
