package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/model"
)

func mustOpen(t *testing.T, root, id string) *Transcript {
	t.Helper()
	tr, err := Open(root, id)
	if err != nil {
		t.Fatalf("Open(%q): %v", id, err)
	}
	return tr
}

func TestTranscriptAppendReadRoundTrip(t *testing.T) {
	root := t.TempDir()
	tr := mustOpen(t, root, NewID())
	msgs := []model.Message{
		{Role: "user", Content: "my favourite number is 17"},
		{Role: "assistant", Content: "noted, 17"},
		{Role: "assistant", Content: "", ToolCalls: []model.ToolCall{{ID: "call-1", Name: "calculator", Arguments: `{"expression":"17*23+5"}`}}},
		{Role: "tool", Content: "396", ToolCallID: "call-1"},
	}
	before := time.Now().UTC().Add(-time.Second)
	for _, m := range msgs {
		if err := tr.AppendMessage(m); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	if got := tr.Count(); got != len(msgs) {
		t.Fatalf("Count = %d, want %d", got, len(msgs))
	}
	recs, err := tr.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(recs) != len(msgs) {
		t.Fatalf("Records = %d, want %d", len(recs), len(msgs))
	}
	for i, r := range recs {
		if r.Kind != KindMessage {
			t.Errorf("record %d kind = %q, want message", i, r.Kind)
		}
		if r.Timestamp.Before(before) || r.Timestamp.After(time.Now().UTC().Add(time.Second)) {
			t.Errorf("record %d timestamp %v out of range", i, r.Timestamp)
		}
		if r.Seq != i+1 {
			t.Errorf("record %d seq = %d, want %d", i, r.Seq, i+1)
		}
		if r.Message == nil || r.Message.Role != msgs[i].Role || r.Message.Content != msgs[i].Content {
			t.Errorf("record %d message = %+v, want %+v", i, r.Message, msgs[i])
		}
	}
	got, err := tr.Messages()
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(got) != len(msgs) {
		t.Fatalf("Messages = %d, want %d", len(got), len(msgs))
	}
	if got[2].ToolCalls[0].Name != "calculator" || got[3].ToolCallID != "call-1" {
		t.Errorf("tool linkage lost in round-trip: %+v", got[2:])
	}
	// File mode 0600, dir 0700.
	if st, err := os.Stat(tr.Path()); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("transcript mode = %v (err %v), want 0600", st.Mode().Perm(), err)
	}
	if st, err := os.Stat(filepath.Dir(tr.Path())); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("sessions dir mode = %v (err %v), want 0700", st.Mode().Perm(), err)
	}
}

func TestTranscriptSurvivesReopen(t *testing.T) {
	root := t.TempDir()
	id := NewID()
	tr := mustOpen(t, root, id)
	if err := tr.AppendMessage(model.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	// Reopen: seq must continue, nothing lost.
	tr2 := mustOpen(t, root, id)
	if err := tr2.AppendMessage(model.Message{Role: "assistant", Content: "hi"}); err != nil {
		t.Fatalf("AppendMessage after reopen: %v", err)
	}
	recs, err := tr2.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(recs) != 2 || recs[1].Seq != 2 {
		t.Fatalf("after reopen: %d records, seqs %v", len(recs), []int{recs[0].Seq, recs[1].Seq})
	}
}

func TestTranscriptSummaryRecord(t *testing.T) {
	root := t.TempDir()
	tr := mustOpen(t, root, NewID())
	if err := tr.AppendSummary(""); err == nil {
		t.Error("AppendSummary(\"\") accepted, want an error")
	}
	if err := tr.AppendSummary("User's favourite number is 17."); err != nil {
		t.Fatalf("AppendSummary: %v", err)
	}
	sums, err := tr.Summaries()
	if err != nil {
		t.Fatalf("Summaries: %v", err)
	}
	if len(sums) != 1 || !strings.Contains(sums[0], "17") {
		t.Errorf("Summaries = %v, want the 17 summary", sums)
	}
	msgs, err := tr.Messages()
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("Messages includes summary records: %v", msgs)
	}
}

func TestTranscriptMigrateFromOnceOnly(t *testing.T) {
	root := t.TempDir()
	tr := mustOpen(t, root, NewID())
	old := []model.Message{
		{Role: "user", Content: "before transcripts"},
		{Role: "assistant", Content: "indeed"},
	}
	importedAt := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	n, err := tr.MigrateFrom(old, importedAt)
	if err != nil || n != 2 {
		t.Fatalf("MigrateFrom = %d, %v; want 2, nil", n, err)
	}
	recs, err := tr.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	for _, r := range recs {
		if !r.Migrated {
			t.Errorf("migrated record not marked Migrated: %+v", r)
		}
		if !r.Timestamp.Equal(importedAt) {
			t.Errorf("migrated timestamp = %v, want %v", r.Timestamp, importedAt)
		}
	}
	// Second import is a no-op — never duplicates turns.
	n, err = tr.MigrateFrom(old, importedAt)
	if err != nil || n != 0 {
		t.Fatalf("second MigrateFrom = %d, %v; want 0, nil", n, err)
	}
	if got := tr.Count(); got != 2 {
		t.Fatalf("Count after second migrate = %d, want 2", got)
	}
	// A fresh transcript over the same history imports again (it is
	// a different session), but the same transcript never does.
	tr2 := mustOpen(t, root, NewID())
	n, err = tr2.MigrateFrom(old, importedAt)
	if err != nil || n != 2 {
		t.Fatalf("fresh transcript MigrateFrom = %d, %v; want 2, nil", n, err)
	}
}

func TestTranscriptCorruptLineTolerated(t *testing.T) {
	root := t.TempDir()
	tr := mustOpen(t, root, NewID())
	if err := tr.AppendMessage(model.Message{Role: "user", Content: "first"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	// Simulate a torn write / hand edit: one bad line in the middle.
	f, err := os.OpenFile(tr.Path(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open for corrupt write: %v", err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatalf("write corrupt line: %v", err)
	}
	f.Close()
	if err := tr.AppendMessage(model.Message{Role: "assistant", Content: "second"}); err != nil {
		t.Fatalf("AppendMessage after corrupt line: %v", err)
	}
	msgs, err := tr.Messages()
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Content != "first" || msgs[1].Content != "second" {
		t.Fatalf("Messages = %v, want [first second]", msgs)
	}
	if got := tr.CorruptLines(); got != 1 {
		t.Errorf("CorruptLines = %d, want 1", got)
	}
}

func TestCurrentPointerRoundTrip(t *testing.T) {
	root := t.TempDir()
	if got, err := ReadCurrent(root); err != nil || got != "" {
		t.Fatalf("ReadCurrent fresh = %q, %v; want \"\", nil", got, err)
	}
	id := NewID()
	if err := WriteCurrent(root, id); err != nil {
		t.Fatalf("WriteCurrent: %v", err)
	}
	got, err := ReadCurrent(root)
	if err != nil || got != id {
		t.Fatalf("ReadCurrent = %q, %v; want %q, nil", got, err, id)
	}
	if err := WriteCurrent(root, "../evil"); err == nil {
		t.Error("WriteCurrent accepted a path-traversal id")
	}
	if err := WriteCurrent(root, ""); err == nil {
		t.Error("WriteCurrent accepted an empty id")
	}
}

func TestListAndResolvePrefix(t *testing.T) {
	root := t.TempDir()
	if infos, err := List(root); err != nil || len(infos) != 0 {
		t.Fatalf("List fresh = %v, %v; want empty, nil", infos, err)
	}
	idA := "20261113-100000-aaaaaaaa"
	idB := "20261113-110000-bbbbbbbb"
	trA := mustOpen(t, root, idA)
	if err := trA.AppendMessage(model.Message{Role: "user", Content: "first session about number seventeen"}); err != nil {
		t.Fatalf("AppendMessage A: %v", err)
	}
	if err := trA.AppendSummary("favourite number 17"); err != nil {
		t.Fatalf("AppendSummary A: %v", err)
	}
	trB := mustOpen(t, root, idB)
	if err := trB.AppendMessage(model.Message{Role: "user", Content: "second session"}); err != nil {
		t.Fatalf("AppendMessage B: %v", err)
	}
	infos, err := List(root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("List = %d sessions, want 2", len(infos))
	}
	// Newest first.
	if infos[0].ID != idB || infos[1].ID != idA {
		t.Errorf("List order = [%s %s], want [%s %s]", infos[0].ID, infos[1].ID, idB, idA)
	}
	if infos[1].Messages != 1 || infos[1].Summaries != 1 {
		t.Errorf("session A info = %+v, want 1 message + 1 summary", infos[1])
	}
	if !strings.Contains(infos[1].Title, "first session") {
		t.Errorf("session A title = %q, want the first user message", infos[1].Title)
	}
	if got, err := ResolvePrefix(root, idA); err != nil || got != idA {
		t.Errorf("ResolvePrefix full = %q, %v; want %q", got, err, idA)
	}
	if got, err := ResolvePrefix(root, "20261113-100000"); err != nil || got != idA {
		t.Errorf("ResolvePrefix unique prefix = %q, %v; want %q", got, err, idA)
	}
	if _, err := ResolvePrefix(root, "20261113"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("ResolvePrefix ambiguous = %v, want an ambiguous error", err)
	}
	if _, err := ResolvePrefix(root, "19990101-000000-ffffffff"); err == nil {
		t.Error("ResolvePrefix unknown id accepted")
	}
	if _, err := ResolvePrefix(root, ""); err == nil {
		t.Error("ResolvePrefix empty accepted")
	}
	// Non-session files in the dir are ignored.
	if err := os.WriteFile(filepath.Join(SessionsDir(root), "notes.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("write stray file: %v", err)
	}
	if infos, err := List(root); err != nil || len(infos) != 2 {
		t.Fatalf("List with stray file = %d, %v; want 2, nil", len(infos), err)
	}
}

func TestValidateID(t *testing.T) {
	if !ValidateID(NewID()) {
		t.Error("NewID failed ValidateID")
	}
	for _, bad := range []string{"", "../x", "abc", "20261113-100000-zzzzzzzz", "20261113 100000 aaaaaaaa", "/etc/passwd"} {
		if ValidateID(bad) {
			t.Errorf("ValidateID(%q) = true, want false", bad)
		}
	}
	if _, err := Open(t.TempDir(), "../evil"); err == nil {
		t.Error("Open accepted a traversal id")
	}
}

// TestOpenCreatesListableFile verifies a freshly opened (still
// empty) session is already listable: /new rotates to a session
// that /sessions must show before its first turn is recorded.
func TestOpenCreatesListableFile(t *testing.T) {
	root := t.TempDir()
	id := NewID()
	tr := mustOpen(t, root, id)
	if tr.Count() != 0 {
		t.Fatalf("fresh transcript count=%d, want 0", tr.Count())
	}
	if st, err := os.Stat(tr.Path()); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("transcript file: stat=%v mode=%v, want an existing 0600 file", err, st.Mode())
	}
	infos, err := List(root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != id || infos[0].Messages != 0 {
		t.Fatalf("List after Open: %+v, want one empty session %s", infos, id)
	}
	if resolved, err := ResolvePrefix(root, id[:20]); err != nil || resolved != id {
		t.Fatalf("ResolvePrefix on an empty session: %q err=%v", resolved, err)
	}
}
