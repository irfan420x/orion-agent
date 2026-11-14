// ApprovalCards unit tests (F-A20-11): NotePending before the
// send, Peek without removing, Take only for a real card.
package messaging

import "testing"

func TestApprovalCardsNotePending(t *testing.T) {
	cards := NewApprovalCards()
	// Unknown id: nothing tracked.
	if _, ok := cards.Peek("x"); ok {
		t.Error("Peek on an unknown id returned a card")
	}
	if _, ok := cards.Take("x"); ok {
		t.Error("Take on an unknown id returned a card")
	}
	// Pending: the send has started, but no card exists yet —
	// neither Peek nor Take may report one.
	cards.NotePending("r1")
	if _, ok := cards.Peek("r1"); ok {
		t.Error("Peek on a pending (not yet sent) id returned a card")
	}
	if _, ok := cards.Take("r1"); ok {
		t.Error("Take on a pending (not yet sent) id returned a card")
	}
	// The send returns and Note lands the card; Take removes it
	// exactly once, Peek sees it until then.
	cards.Note("r1", 42)
	if msgID, ok := cards.Peek("r1"); !ok || msgID != 42 {
		t.Errorf("Peek after Note = (%d, %v), want (42, true)", msgID, ok)
	}
	if msgID, ok := cards.Take("r1"); !ok || msgID != 42 {
		t.Errorf("Take after Note = (%d, %v), want (42, true)", msgID, ok)
	}
	if _, ok := cards.Take("r1"); ok {
		t.Error("second Take returned a card, want nothing")
	}
	if _, ok := cards.Peek("r1"); ok {
		t.Error("Peek after Take returned a card, want nothing")
	}
	// Forget clears a pending mark; a later Note still lands.
	cards.NotePending("r2")
	cards.Forget("r2")
	if _, ok := cards.Peek("r2"); ok {
		t.Error("Peek after Forget returned a card")
	}
	cards.Note("r2", 7)
	if msgID, ok := cards.Peek("r2"); !ok || msgID != 7 {
		t.Errorf("Peek after Forget+Note = (%d, %v), want (7, true)", msgID, ok)
	}
	// A second NotePending for an already-Noted id must not
	// demote the card back to pending.
	cards.NotePending("r2")
	if msgID, ok := cards.Peek("r2"); !ok || msgID != 7 {
		t.Errorf("Peek after NotePending on a Noted id = (%d, %v), want (7, true)", msgID, ok)
	}
	// Nil-safe: a nil tracker means "no tracking", never a panic.
	var nilCards *ApprovalCards
	nilCards.NotePending("x")
	nilCards.Note("x", 1)
	if _, ok := nilCards.Peek("x"); ok {
		t.Error("nil cards Peek returned a card")
	}
	if _, ok := nilCards.Take("x"); ok {
		t.Error("nil cards Take returned a card")
	}
	nilCards.Forget("x")
}
