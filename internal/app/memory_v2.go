// Memory v2 wiring: the typed-memory adapter for core's recall v2,
// and the App-level helpers the Telegram /memory and /compress
// commands use.
//
// The adapter maps memory.TypedStore onto the typed-memory source
// interface core's recall v2 expects. The interface is unexported in
// core, so it is satisfied structurally here: the adapter's methods
// match it exactly, and core's SetTypedMemorySource accepts the
// adapter without this package naming the interface.
package app

import (
	"context"
	"fmt"
	"log"

	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
)

// typedStoreAdapter adapts *memory.TypedStore to core's typed-memory
// source. Stale memories are filtered out of ListTypedMemories so
// drift-verified-stale memories are never re-surfaced, even across
// restarts; LoadTypedMemory still loads them (the /memory listing
// shows stale memories until the user forgets them).
type typedStoreAdapter struct {
	ts *memory.TypedStore
}

func newTypedStoreAdapter(ts *memory.TypedStore) *typedStoreAdapter {
	return &typedStoreAdapter{ts: ts}
}

// ListTypedMemories returns all non-stale typed memories, converting
// memory.TypedMemory to core's shape (Type as a plain string; Path is
// unused by recall and left empty).
func (a *typedStoreAdapter) ListTypedMemories() ([]core.TypedMemory, error) {
	if a == nil || a.ts == nil {
		return nil, fmt.Errorf("app: typed memory store unavailable")
	}
	ms, err := a.ts.List()
	if err != nil {
		return nil, err
	}
	out := make([]core.TypedMemory, 0, len(ms))
	for _, m := range ms {
		if m.Stale {
			continue
		}
		out = append(out, core.TypedMemory{
			Name:        m.Name,
			Description: m.Description,
			Type:        string(m.Type),
			Body:        m.Body,
			UpdatedAt:   m.UpdatedAt,
		})
	}
	return out, nil
}

// LoadTypedMemory loads one typed memory by name.
func (a *typedStoreAdapter) LoadTypedMemory(name string) (core.TypedMemory, error) {
	if a == nil || a.ts == nil {
		return core.TypedMemory{}, fmt.Errorf("app: typed memory store unavailable")
	}
	m, found, err := a.ts.Load(name)
	if err != nil {
		return core.TypedMemory{}, err
	}
	if !found {
		return core.TypedMemory{}, fmt.Errorf("app: typed memory %q not found", name)
	}
	return core.TypedMemory{
		Name:        m.Name,
		Description: m.Description,
		Type:        string(m.Type),
		Body:        m.Body,
		UpdatedAt:   m.UpdatedAt,
	}, nil
}

// MarkTypedMemoryStale persists the drift-verified stale marker for a
// memory. core's Agent calls this (best-effort) through the same
// interface check when drift verification condemns a memory.
func (a *typedStoreAdapter) MarkTypedMemoryStale(name string) error {
	if a == nil || a.ts == nil {
		return fmt.Errorf("app: typed memory store unavailable")
	}
	return a.ts.MarkStale(name)
}

// wireMemoryV2 opens the typed-memory layer (migrating the JSON facts
// into typed files on first open) and the session log, and wires both
// into the agent. Every step is best-effort: a failure degrades to v1
// recall rather than failing startup — the daemon must stay up even
// if the typed layer cannot open. It returns the opened layers (nil
// when unavailable) for the App to keep for the Telegram commands,
// plus the typed-store adapter the parent agent uses — subagent
// children get a read-only view over the same adapter (see
// readOnlyTypedSource in subagent.go), never the adapter itself,
// because the adapter can persist stale markers.
func wireMemoryV2(logger *log.Logger, dataDir string, mem *memory.FileStore, agent *core.Agent) (*memory.TypedStore, *memory.SessionLog, *typedStoreAdapter) {
	var ts *memory.TypedStore
	var typedAdapter *typedStoreAdapter
	if store, err := memory.NewTypedStore(dataDir, memory.WithMigrateOnOpen(mem)); err != nil {
		logger.Printf("memory: typed store with migration failed (%v); retrying without migration", err)
		if store2, err2 := memory.NewTypedStore(dataDir); err2 != nil {
			logger.Printf("memory: typed store unavailable (%v); typed recall disabled", err2)
		} else {
			ts = store2
		}
	} else {
		ts = store
	}
	if ts != nil {
		typedAdapter = newTypedStoreAdapter(ts)
		agent.SetTypedMemorySource(typedAdapter)
	}
	var slog *memory.SessionLog
	if l, err := memory.NewSessionLog(dataDir); err != nil {
		logger.Printf("memory: session log unavailable (%v); session extraction disabled", err)
	} else {
		slog = l
		agent.SetSessionMemoryAppender(slog)
	}
	return ts, slog, typedAdapter
}

// TypedMemories lists the typed memories (nil store degrades to an
// empty list, so hand-built Apps without Memory v2 wiring still work).
func (a *App) TypedMemories() ([]memory.TypedMemory, error) {
	if a == nil || a.typedStore == nil {
		return nil, nil
	}
	return a.typedStore.List()
}

// ForgetTypedMemory deletes the typed memory named name and returns
// the name. A missing typed store is an error (unlike the listing,
// forgetting must not silently pretend it worked).
func (a *App) ForgetTypedMemory(name string) (string, error) {
	if a == nil || a.typedStore == nil {
		return "", fmt.Errorf("app: typed memory store unavailable")
	}
	if err := a.typedStore.Delete(name); err != nil {
		return "", err
	}
	return name, nil
}

// SessionMemoryText returns the current session-memory.md content
// ("" when absent or unwired), for /compress to draw on.
func (a *App) SessionMemoryText(ctx context.Context) (string, error) {
	if a == nil || a.sessionLog == nil {
		return "", nil
	}
	return a.sessionLog.Read()
}
