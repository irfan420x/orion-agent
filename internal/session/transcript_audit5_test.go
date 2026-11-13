// Audit 5 regression tests for transcripts: F-A5-P2-3 (one >4 MiB
// line makes the session unloadable), F-A5-P2-4 (migration failure
// mid-way is unrecoverable), F-A5-P2-6 (no locking across handles /
// processes), F-A5-P2-8 (secrets persisted verbatim), F-A5-1
// (dropped-turn spill record).
package session

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/irfan/orion/internal/model"
)

func TestTranscriptOverlongLineSkipped(t *testing.T) {
	// F-A5-P2-3: one >4 MiB line used to make Open() AND Records()
	// fail with "token too long" — the session became unloadable
	// and the agent ran with no transcript at all. Now the
	// overlong line is one corrupt line to skip.
	root := t.TempDir()
	id := NewID()
	tr := mustOpen(t, root, id)
	if err := tr.AppendMessage(model.Message{Role: "user", Content: "first"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	// A 5 MiB garbage line, written directly (as a torn write or
	// hand edit would leave it).
	f, err := os.OpenFile(tr.Path(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open for overlong write: %v", err)
	}
	if _, err := f.WriteString(strings.Repeat("x", 5*1024*1024) + "\n"); err != nil {
		t.Fatalf("write overlong line: %v", err)
	}
	f.Close()
	if err := tr.AppendMessage(model.Message{Role: "assistant", Content: "second"}); err != nil {
		t.Fatalf("AppendMessage after overlong line: %v", err)
	}
	// Reopen from scratch: the session must load.
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	tr2 := mustOpen(t, root, id)
	msgs, err := tr2.Messages()
	if err != nil {
		t.Fatalf("Messages after reopen: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Content != "first" || msgs[1].Content != "second" {
		t.Fatalf("Messages = %v, want [first second]", msgs)
	}
	if got := tr2.CorruptLines(); got != 1 {
		t.Errorf("CorruptLines = %d, want 1 (the overlong line)", got)
	}
}

func TestTranscriptMigrationResumesAfterPartialFailure(t *testing.T) {
	// F-A5-P2-4: a migration that stopped mid-way (here simulated
	// by importing only the first 2 of 5 turns, as a crashed
	// import would leave them) must resume the remainder on the
	// next attempt — never duplicate, never block forever.
	root := t.TempDir()
	tr := mustOpen(t, root, NewID())
	msgs := []model.Message{
		{Role: "user", Content: "turn one"},
		{Role: "assistant", Content: "turn two"},
		{Role: "user", Content: "turn three"},
		{Role: "assistant", Content: "turn four"},
		{Role: "user", Content: "turn five"},
	}
	importedAt := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	n, err := tr.MigrateFrom(msgs[:2], importedAt)
	if err != nil || n != 2 {
		t.Fatalf("partial MigrateFrom = %d, %v; want 2, nil", n, err)
	}
	// Retry with the full history: only the remainder imports.
	n, err = tr.MigrateFrom(msgs, importedAt)
	if err != nil || n != 3 {
		t.Fatalf("resumed MigrateFrom = %d, %v; want 3, nil", n, err)
	}
	recs, err := tr.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(recs) != 5 {
		t.Fatalf("records = %d, want 5 (no duplicates)", len(recs))
	}
	for i, r := range recs {
		if !r.Migrated || r.Message == nil || r.Message.Content != msgs[i].Content {
			t.Fatalf("record %d = %+v, want migrated %q in order", i, r, msgs[i].Content)
		}
	}
	// A third attempt imports nothing.
	if n, err := tr.MigrateFrom(msgs, importedAt); err != nil || n != 0 {
		t.Fatalf("third MigrateFrom = %d, %v; want 0, nil", n, err)
	}
	// Once a live (non-migrated) turn exists, migration never
	// runs again.
	if err := tr.AppendMessage(model.Message{Role: "user", Content: "live turn"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if n, err := tr.MigrateFrom(msgs, importedAt); err != nil || n != 0 {
		t.Fatalf("MigrateFrom after live turns = %d, %v; want 0, nil", n, err)
	}
	if got := tr.Count(); got != 6 {
		t.Fatalf("Count = %d, want 6", got)
	}
}

func TestTranscriptSecondOpenSharesHandleAndSeqs(t *testing.T) {
	// F-A5-P2-6 (audit repro): two Open() handles on the same
	// file, one append each, used to produce seqs 1, 1. One file
	// has one sequence counter: the second open in this process
	// returns the same handle, and appends never reuse a seq.
	root := t.TempDir()
	id := NewID()
	tr1 := mustOpen(t, root, id)
	if err := tr1.AppendMessage(model.Message{Role: "user", Content: "one"}); err != nil {
		t.Fatalf("AppendMessage 1: %v", err)
	}
	tr2 := mustOpen(t, root, id)
	if tr2 != tr1 {
		t.Fatal("second Open of the same transcript returned a different handle")
	}
	if err := tr2.AppendMessage(model.Message{Role: "assistant", Content: "two"}); err != nil {
		t.Fatalf("AppendMessage 2: %v", err)
	}
	recs, err := tr2.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(recs) != 2 || recs[0].Seq != 1 || recs[1].Seq != 2 {
		t.Fatalf("seqs = %v, want [1 2]", []int{recs[0].Seq, recs[1].Seq})
	}
}

func TestTranscriptFlockExcludesOtherProcesses(t *testing.T) {
	// F-A5-P2-6: the flock is held for the handle's life, so a
	// second process cannot open the same transcript for writing
	// (proved here with a raw flock on a separately opened fd —
	// the kernel enforces it regardless of process). Releasing
	// the last reference frees it again.
	root := t.TempDir()
	id := NewID()
	tr := mustOpen(t, root, id)
	other, err := os.OpenFile(tr.Path(), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open second fd: %v", err)
	}
	defer other.Close()
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatal("second flock succeeded while the transcript is open; want EWOULDBLOCK")
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock after Close failed: %v", err)
	}
	syscall.Flock(int(other.Fd()), syscall.LOCK_UN)
	// Reopening after close resumes the sequence.
	tr2 := mustOpen(t, root, id)
	if err := tr2.AppendMessage(model.Message{Role: "user", Content: "after close"}); err != nil {
		t.Fatalf("AppendMessage after reopen: %v", err)
	}
	if recs, err := tr2.Records(); err != nil || len(recs) != 1 || recs[0].Seq != 1 {
		t.Fatalf("records after reopen = %v, %v; want one record seq 1", recs, err)
	}
}

func TestTranscriptAppendAfterCloseFails(t *testing.T) {
	root := t.TempDir()
	tr := mustOpen(t, root, NewID())
	if err := tr.AppendMessage(model.Message{Role: "user", Content: "one"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tr.AppendMessage(model.Message{Role: "user", Content: "two"}); err == nil {
		t.Error("AppendMessage after Close succeeded, want an error")
	}
	// Close is idempotent.
	if err := tr.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestTranscriptAppendScrubsSecrets(t *testing.T) {
	// F-A5-P2-8: a pasted key/token must not survive verbatim in
	// the append-only transcript. The values below are clearly
	// fake test keys (unusable shapes, no real credentials).
	root := t.TempDir()
	tr := mustOpen(t, root, NewID())
	anthropicKey := "sk-ant-fake1234567890abcdef"
	bearerToken := "Bearer abcdefghijklmnopqrstuvwxyz0123456789"
	msg := model.Message{
		Role:    "user",
		Content: "here is my key: " + anthropicKey + " please use it",
		ToolCalls: []model.ToolCall{{
			ID:        "call-1",
			Name:      "shell_exec",
			Arguments: `{"command":"curl -H \"Authorization: ` + bearerToken + `\" https://example.invalid"}`,
		}},
	}
	if err := tr.AppendMessage(msg); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := tr.AppendSummary("the key was " + anthropicKey); err != nil {
		t.Fatalf("AppendSummary: %v", err)
	}
	// Ordinary content is untouched.
	if err := tr.AppendMessage(model.Message{Role: "user", Content: "my favourite number is 17"}); err != nil {
		t.Fatalf("AppendMessage clean: %v", err)
	}
	raw, err := os.ReadFile(tr.Path())
	if err != nil {
		t.Fatalf("read transcript file: %v", err)
	}
	if strings.Contains(string(raw), anthropicKey) {
		t.Error("transcript file contains the pasted Anthropic key verbatim")
	}
	if strings.Contains(string(raw), "abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Error("transcript file contains the pasted bearer token verbatim")
	}
	if !strings.Contains(string(raw), "[REDACTED]") {
		t.Error("transcript file has no [REDACTED] marker")
	}
	if !strings.Contains(string(raw), "my favourite number is 17") {
		t.Error("clean content was altered in the transcript file")
	}
	// The redacted form is what reads back, too: the secret was
	// never persisted.
	recs, err := tr.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	for _, r := range recs {
		if r.Message != nil && strings.Contains(r.Message.Content, anthropicKey) {
			t.Error("read-back message still holds the key verbatim")
		}
		if r.Kind == KindSummary && strings.Contains(r.Text, anthropicKey) {
			t.Error("read-back summary still holds the key verbatim")
		}
	}
}

func TestTranscriptDroppedRecord(t *testing.T) {
	// F-A5-1: the spill record for a turn dropped without a
	// summary is a first-class record — listed, counted, and
	// excluded from messages and summaries.
	root := t.TempDir()
	tr := mustOpen(t, root, NewID())
	if err := tr.AppendDropped(""); err == nil {
		t.Error("AppendDropped(\"\") accepted, want an error")
	}
	if err := tr.AppendDropped("Dropped without summary (auto-compaction unavailable): user: my favourite number is 17"); err != nil {
		t.Fatalf("AppendDropped: %v", err)
	}
	if err := tr.AppendMessage(model.Message{Role: "user", Content: "still here"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	recs, err := tr.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(recs) != 2 || recs[0].Kind != KindDropped || recs[1].Kind != KindMessage {
		t.Fatalf("records = %+v, want [dropped message]", recs)
	}
	if msgs, err := tr.Messages(); err != nil || len(msgs) != 1 {
		t.Fatalf("Messages = %d, %v; want 1 (dropped records excluded)", len(msgs), err)
	}
	if sums, err := tr.Summaries(); err != nil || len(sums) != 0 {
		t.Fatalf("Summaries = %d, %v; want 0", len(sums), err)
	}
	infos, err := List(root)
	if err != nil || len(infos) != 1 || infos[0].Dropped != 1 {
		t.Fatalf("List info = %+v, %v; want 1 dropped", infos, err)
	}
}
