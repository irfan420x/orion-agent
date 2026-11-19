// FailoverChain tests: the shared, daemon-wide chain state the agent
// consults when a provider fails mid-turn. Cooldowns are per provider
// (one API key pool serves every chat), so a 429 in one chat benches
// that provider for the next chat too — the chain must never let two
// chats hammer the same dead provider in parallel.
package model

import (
	"errors"
	"testing"
	"time"
)

func fakeChainProvider(name string) (Provider, error) {
	return nil, errors.New("test factory: no provider named " + name)
}

func TestFailoverChainDisabledWhenEmpty(t *testing.T) {
	c := NewFailoverChain(nil, fakeChainProvider)
	if c.Enabled() {
		t.Error("empty chain: Enabled() = true, want false")
	}
	if _, ok := c.NextAvailable(map[string]bool{}); ok {
		t.Error("empty chain: NextAvailable returned a provider, want none")
	}
	if _, ok := c.FirstAvailable(nil); ok {
		t.Error("empty chain: FirstAvailable returned a provider, want none")
	}
	c2 := NewFailoverChain([]string{"deepseek"}, nil)
	if c2.Enabled() {
		t.Error("chain without a factory: Enabled() = true, want false (nothing can be built)")
	}
}

func TestFailoverChainOrderAndSkip(t *testing.T) {
	c := NewFailoverChain([]string{"deepseek", "openai", "ollama"}, fakeChainProvider)
	if got := c.Order(); len(got) != 3 || got[0] != "deepseek" || got[2] != "ollama" {
		t.Errorf("Order() = %v, want [deepseek openai ollama]", got)
	}
	// The first provider not already attempted wins.
	if next, ok := c.NextAvailable(map[string]bool{"deepseek": true}); !ok || next != "openai" {
		t.Errorf("NextAvailable after deepseek = %q,%v, want openai,true", next, ok)
	}
	if next, ok := c.NextAvailable(map[string]bool{"deepseek": true, "openai": true}); !ok || next != "ollama" {
		t.Errorf("NextAvailable after deepseek+openai = %q,%v, want ollama,true", next, ok)
	}
	if _, ok := c.NextAvailable(map[string]bool{"deepseek": true, "openai": true, "ollama": true}); ok {
		t.Error("NextAvailable with all attempted: returned a provider, want none (chain exhausted)")
	}
	// FirstAvailable honours the exclusion set too.
	if first, ok := c.FirstAvailable(map[string]bool{"deepseek": true}); !ok || first != "openai" {
		t.Errorf("FirstAvailable excluding deepseek = %q,%v, want openai,true", first, ok)
	}
}

func TestFailoverChainCooldown(t *testing.T) {
	now := time.Now()
	c := NewFailoverChain([]string{"deepseek", "openai"}, fakeChainProvider)
	c.now = func() time.Time { return now }
	c.MarkFailed("deepseek", FailoverBilling)
	if !c.IsCooled("deepseek") {
		t.Error("deepseek: IsCooled = false right after a billing failure, want true")
	}
	if c.IsCooled("openai") {
		t.Error("openai: IsCooled = true, want false (only the failed provider is benched)")
	}
	// A cooled provider is skipped by selection, never retried.
	if next, ok := c.NextAvailable(map[string]bool{}); !ok || next != "openai" {
		t.Errorf("NextAvailable with deepseek cooled = %q,%v, want openai,true", next, ok)
	}
	if first, ok := c.FirstAvailable(nil); !ok || first != "openai" {
		t.Errorf("FirstAvailable with deepseek cooled = %q,%v, want openai,true", first, ok)
	}
	// A billing cooldown lasts 60 minutes: 59 minutes later the
	// provider is still benched; at 61 it is available again.
	now = now.Add(59 * time.Minute)
	if !c.IsCooled("deepseek") {
		t.Error("deepseek: IsCooled = false at 59m into a 60m billing cooldown, want true")
	}
	now = now.Add(2 * time.Minute)
	if c.IsCooled("deepseek") {
		t.Error("deepseek: IsCooled = true after the 60m billing cooldown expired, want false")
	}
	if next, ok := c.NextAvailable(map[string]bool{}); !ok || next != "deepseek" {
		t.Errorf("NextAvailable after cooldown expiry = %q,%v, want deepseek,true", next, ok)
	}
}

func TestFailoverChainCooldownIsPerClass(t *testing.T) {
	now := time.Now()
	c := NewFailoverChain([]string{"deepseek", "openai"}, fakeChainProvider)
	c.now = func() time.Time { return now }
	// A transient blip benches for 60s only.
	c.MarkFailed("deepseek", FailoverTransient)
	if until, ok := c.CooledUntil("deepseek"); !ok || !until.Equal(now.Add(60*time.Second)) {
		t.Errorf("transient CooledUntil = %v,%v, want %v,true", until, ok, now.Add(60*time.Second))
	}
	// A later, harder failure extends the bench to its own
	// cooldown from the later failure time.
	now = now.Add(30 * time.Second)
	c.MarkFailed("deepseek", FailoverRateLimit)
	if until, ok := c.CooledUntil("deepseek"); !ok || !until.Equal(now.Add(60*time.Minute)) {
		t.Errorf("rate-limit CooledUntil = %v,%v, want %v,true", until, ok, now.Add(60*time.Minute))
	}
	// Marking a failure with no cooldown (FailoverNone) never
	// benches: it is not a failure the classifier recognised.
	c2 := NewFailoverChain([]string{"deepseek"}, fakeChainProvider)
	c2.MarkFailed("deepseek", FailoverNone)
	if c2.IsCooled("deepseek") {
		t.Error("FailoverNone: IsCooled = true, want false")
	}
}

func TestFailoverChainBuild(t *testing.T) {
	built := map[string]bool{}
	c := NewFailoverChain([]string{"deepseek", "openai"}, func(name string) (Provider, error) {
		built[name] = true
		return nil, errors.New("factory refuses " + name)
	})
	if _, err := c.Build("openai"); err == nil || !built["openai"] {
		t.Errorf("Build(openai) err = %v, built = %v, want a factory error through the chain", err, built["openai"])
	}
	// A chain without a factory cannot build: the error says so
	// instead of panicking on a nil call.
	c2 := NewFailoverChain([]string{"deepseek"}, nil)
	if _, err := c2.Build("deepseek"); err == nil {
		t.Error("Build without factory: expected an error, got nil")
	}
}

func TestFailoverChainSetOrderKeepsCooldowns(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	c := NewFailoverChain([]string{"deepseek", "openai"}, fakeChainProvider)
	c.now = func() time.Time { return now }
	c.MarkFailed("deepseek", FailoverBilling)
	// The daemon default moves: the chain head follows it, but
	// the bench on deepseek survives the reorder.
	c.SetOrder([]string{"openai", "deepseek"})
	if got := c.Order(); len(got) != 2 || got[0] != "openai" || got[1] != "deepseek" {
		t.Errorf("Order after SetOrder = %v, want [openai deepseek]", got)
	}
	if !c.IsCooled("deepseek") {
		t.Error("SetOrder cleared the deepseek bench, want it kept")
	}
	if next, ok := c.NextAvailable(map[string]bool{"openai": true}); ok {
		t.Errorf("NextAvailable after SetOrder = %q, want none (deepseek still benched)", next)
	}
	// A nil chain reorders to nothing instead of panicking.
	var nilChain *FailoverChain
	nilChain.SetOrder([]string{"openai"})
}
