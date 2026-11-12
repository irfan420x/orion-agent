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
	ClearMessages() error
	// Lifecycle
	Save() error
	Close() error
}
