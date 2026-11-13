// Package memory provides durable Orion memory: short-term conversation
// history and long-term facts, persisted as JSON under the data dir.
package memory

import (
	"time"

	"github.com/irfan/orion/internal/model"
)

// Fact is one long-term remembered fact.
type Fact struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store is the memory interface used by the core agent loop.
type Store interface {
	// Facts
	AddFact(key, value string) error
	GetFact(key string) (Fact, bool, error)
	DeleteFact(key string) error
	SearchFacts(query string) ([]Fact, error)
	ListFacts() ([]Fact, error)
	// Conversation (short-term)
	AddMessage(msg model.Message) error
	RecentMessages(n int) ([]model.Message, error)
	// ReplaceMessages replaces the whole retained conversation history
	// with msgs (a defensive copy is stored), enforcing the same cap
	// as AddMessage (oldest dropped first). It is the primitive
	// behind /undo (drop the last turns) and /compress (drop the
	// summarised prefix); both need to rewrite history, not just
	// append to it or clear it entirely.
	ReplaceMessages(msgs []model.Message) error
	ClearMessages() error
	// Lifecycle
	Save() error
	Close() error
}
