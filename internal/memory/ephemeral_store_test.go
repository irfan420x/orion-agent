package memory

import (
	"testing"

	"github.com/irfan/orion/internal/model"
)

// EphemeralStore must satisfy the full Store contract without ever
// touching disk: facts, messages, caps, and the closed behaviour.

func TestEphemeralStoreFacts(t *testing.T) {
	s := NewEphemeralStore()
	if err := s.AddFact(" k ", "v1"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	f, ok, err := s.GetFact("k")
	if err != nil || !ok || f.Value != "v1" {
		t.Fatalf("GetFact = %+v, %v, %v; want value v1", f, ok, err)
	}
	if err := s.AddFact("", "x"); err != ErrEmptyKey {
		t.Fatalf("AddFact empty key = %v, want ErrEmptyKey", err)
	}
	if err := s.AddFact("k", "v2"); err != nil {
		t.Fatalf("AddFact update: %v", err)
	}
	f, _, _ = s.GetFact("k")
	if f.Value != "v2" {
		t.Fatalf("updated value = %q, want v2", f.Value)
	}
	list, err := s.ListFacts()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListFacts = %d, %v; want 1", len(list), err)
	}
	found, err := s.SearchFacts("V2")
	if err != nil || len(found) != 1 {
		t.Fatalf("SearchFacts = %d, %v; want 1 (case-insensitive)", len(found), err)
	}
	if err := s.DeleteFact("k"); err != nil {
		t.Fatalf("DeleteFact: %v", err)
	}
	if _, ok, _ := s.GetFact("k"); ok {
		t.Fatal("fact still present after DeleteFact")
	}
}

func TestEphemeralStoreMessagesAndCap(t *testing.T) {
	s := NewEphemeralStore()
	for i := 0; i < MaxMessages+5; i++ {
		if err := s.AddMessage(model.Message{Role: "user", Content: "m"}); err != nil {
			t.Fatalf("AddMessage %d: %v", i, err)
		}
	}
	all, err := s.RecentMessages(0)
	if err != nil || len(all) != MaxMessages {
		t.Fatalf("RecentMessages(0) = %d, %v; want %d (cap enforced)", len(all), err, MaxMessages)
	}
	last, err := s.RecentMessages(2)
	if err != nil || len(last) != 2 {
		t.Fatalf("RecentMessages(2) = %d, %v; want 2", len(last), err)
	}
	if err := s.ReplaceMessages([]model.Message{{Role: "user", Content: "only"}}); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}
	all, _ = s.RecentMessages(0)
	if len(all) != 1 || all[0].Content != "only" {
		t.Fatalf("after ReplaceMessages: %+v", all)
	}
	if err := s.ClearMessages(); err != nil {
		t.Fatalf("ClearMessages: %v", err)
	}
	all, _ = s.RecentMessages(0)
	if len(all) != 0 {
		t.Fatalf("after ClearMessages: %d messages", len(all))
	}
}

func TestEphemeralStoreClose(t *testing.T) {
	s := NewEphemeralStore()
	if err := s.Save(); err != nil {
		t.Fatalf("Save on open store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v, want nil (idempotent)", err)
	}
	if err := s.AddMessage(model.Message{Role: "user", Content: "x"}); err != ErrClosed {
		t.Fatalf("AddMessage after Close = %v, want ErrClosed", err)
	}
	if err := s.Save(); err != ErrClosed {
		t.Fatalf("Save after Close = %v, want ErrClosed", err)
	}
}
