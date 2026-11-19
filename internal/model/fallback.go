// Fallback chain state: the shared, daemon-wide ordered list of
// providers a failed turn may continue on, plus the per-provider
// cooldown table.
//
// One instance is shared by every agent (global + per-chat +
// subagent children): a 429 in one chat benches that provider for
// the next chat too, because every chat draws on the same API key
// pool — per-chat cooldowns would let two chats hammer the same
// dead provider in parallel. The chain itself is dumb on purpose:
// it knows the order, the factory that builds a provider by name,
// and which providers are benched until when. The agent loop owns
// the per-turn part (which providers this turn already tried, the
// hop record, the usage split).
//
// Selection rules (where Orion deliberately differs from Hermes):
//
//   - The chain order is the operator's explicit priority from
//     config (default first, then fallback_providers in order).
//     There is no monotonic "fallback index" carried across turns
//     (Hermes keeps one in the session): every turn recomputes
//     from the live cooldown table, so a recovered primary is
//     picked up by the very next turn instead of waiting for the
//     session's index to walk back.
//   - A provider is skipped while benched; when every provider is
//     benched the chain reports nothing available and the turn
//     fails honestly instead of trying a provider it just benched.
//   - The factory is the only way providers are built here, so a
//     chain entry that cannot be built (missing key, unknown name)
//     fails at selection time with a clear error — never silently.
package model

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// FallbackHop is one provider switch inside a turn: which provider
// failed, which one took over, and why. It rides on the TurnFinished
// event so the caller can report the switch (and meter it) honestly.
type FallbackHop struct {
	From  string
	To    string
	Class FailoverClass
	// Reason is the short human-readable cause (the class name plus
	// the provider's own error, trimmed).
	Reason string
}

// FailoverChain is the shared chain state for one daemon.
type FailoverChain struct {
	mu sync.Mutex
	// order is the full chain, head first (the daemon default
	// first, then the configured fallbacks in order). A copy is
	// returned by Order; the chain never mutates it.
	order []string
	// build constructs a provider by canonical (lowercase) name.
	// Nil means "nothing can be built" (a hand-built chain with
	// no factory): Enabled reports false and Build errors.
	build func(name string) (Provider, error)
	// cooledUntil benches a provider until a time. A missing key
	// means "not benched".
	cooledUntil map[string]time.Time
	// now is the clock (time.Now in production). Tests set it to
	// drive cooldown expiry without waiting.
	now func() time.Time
}

// NewFailoverChain builds a chain from the configured order (head
// first) and a factory. A nil/empty order or a nil factory makes a
// disabled chain: Enabled reports false and the agent never fails
// over. The order slice is copied.
func NewFailoverChain(order []string, build func(name string) (Provider, error)) *FailoverChain {
	return &FailoverChain{
		order:       append([]string(nil), order...),
		build:       build,
		cooledUntil: map[string]time.Time{},
		now:         time.Now,
	}
}

// Enabled reports whether the chain can fail over at all: it needs
// at least one provider AND a factory to build them. A single
// provider with a factory is enabled (it still benches that
// provider after a failure, so the next turn skips it while
// cooled) — the chain only needs >1 entry to actually switch.
func (c *FailoverChain) Enabled() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.order) > 0 && c.build != nil
}

// Order returns a copy of the chain order (head first).
func (c *FailoverChain) Order() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.order...)
}

// SetOrder replaces the chain's provider order, keeping the
// cooldown table: a provider benched under the old order stays
// benched under the new one. Used when the daemon default changes
// — the chain head follows the new default without forgetting
// that a provider just failed. Nil-safe: a chain that was never
// built has nothing to reorder.
func (c *FailoverChain) SetOrder(names []string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[string]bool{}
	order := make([]string, 0, len(names))
	for _, n := range names {
		key := strings.ToLower(strings.TrimSpace(n))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		order = append(order, key)
	}
	c.order = order
}

// Build constructs the provider named by name through the factory.
func (c *FailoverChain) Build(name string) (Provider, error) {
	if c == nil {
		return nil, errors.New("model: failover chain is nil")
	}
	c.mu.Lock()
	build := c.build
	c.mu.Unlock()
	if build == nil {
		return nil, fmt.Errorf("model: failover chain has no provider factory (cannot build %q)", name)
	}
	return build(name)
}

// MarkFailed benches name for its class's cooldown, starting now.
// A class with no cooldown (FailoverNone) benches nothing: the
// failure was not a recognised failover trigger. A later failure
// re-benches from its own failure time with its own class's
// duration — the freshest evidence wins, so a provider that fails
// again while benched (e.g. via a manual switch) gets a new bench
// from the latest failure instead of keeping a stale one.
func (c *FailoverChain) MarkFailed(name string, class FailoverClass) {
	if c == nil {
		return
	}
	d := class.Cooldown()
	if d <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cooledUntil[name] = c.now().Add(d)
}

// IsCooled reports whether name is benched right now.
func (c *FailoverChain) IsCooled(name string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.cooledUntil[name]
	return ok && c.now().Before(until)
}

// CooledUntil returns the bench end for name, if it is benched.
func (c *FailoverChain) CooledUntil(name string) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.cooledUntil[name]
	if !ok || !c.now().Before(until) {
		return time.Time{}, false
	}
	return until, true
}

// FirstAvailable returns the first provider in chain order that is
// neither benched nor in exclude (nil excludes nothing). It is the
// turn-start selection: the primary when it is healthy, else the
// first healthy fallback.
func (c *FailoverChain) FirstAvailable(exclude map[string]bool) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, name := range c.order {
		if exclude[name] {
			continue
		}
		if until, ok := c.cooledUntil[name]; ok && c.now().Before(until) {
			continue
		}
		return name, true
	}
	return "", false
}

// NextAvailable is FirstAvailable over the per-turn attempted set:
// the first provider this turn has not already tried and that is
// not benched. It is the mid-turn selection after a failure.
func (c *FailoverChain) NextAvailable(attempted map[string]bool) (string, bool) {
	return c.FirstAvailable(attempted)
}
