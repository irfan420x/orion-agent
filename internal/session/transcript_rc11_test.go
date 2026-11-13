// Audit 6 regression tests for transcripts: F-A6-P2-4 (overlong
// >16 MiB lines skipped but never counted as corrupt), F-A6-P2-5
// (migration remainder permanently blocked by a compaction
// summary).
package session

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/model"
)

func TestTranscriptOverlongLineCountedCorrupt(t *testing.T) {
	// F-A6-P2-4: a line over the 16 MiB cap used to be skipped
	// silently — readLine reported it with nil bytes, and the
	// scan callbacks' TrimSpace-then-empty check swallowed it
	// before the overlong check ever ran, so CorruptLines stayed
	// 0. (The Audit 5 test only covered a 5 MiB line, which is
	// under the cap and counted via its JSON failure.)
	root := t.TempDir()
	id := NewID()
	tr := mustOpen(t, root, id)
	if err := tr.AppendMessage(model.Message{Role: "user", Content: "first"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	// A 17 MiB garbage line, written directly (as a torn write or
	// hand edit would leave it).
	f, err := os.OpenFile(tr.Path(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open for overlong write: %v", err)
	}
	if _, err := f.WriteString(strings.Repeat("x", 17*1024*1024) + "\n"); err != nil {
		t.Fatalf("write overlong line: %v", err)
	}
	f.Close()
	if err := tr.AppendMessage(model.Message{Role: "assistant", Content: "second"}); err != nil {
		t.Fatalf("AppendMessage after overlong line: %v", err)
	}
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
	// Open counts it (scanLocked) and Records recounts it.
	if got := tr2.CorruptLines(); got != 1 {
		t.Errorf("CorruptLines after Open = %d, want 1 (the >16 MiB line)", got)
	}
	if _, err := tr2.Records(); err != nil {
		t.Fatalf("Records: %v", err)
	}
	if got := tr2.CorruptLines(); got != 1 {
		t.Errorf("CorruptLines after Records = %d, want 1 (the >16 MiB line)", got)
	}
}

func TestTranscriptMigrateFromAfterSummary(t *testing.T) {
	// F-A6-P2-5: a session whose transcript holds a compaction
	// summary plus a partially migrated remainder used to migrate
	// nothing — MigrateFrom returned (0, nil) on ANY non-migrated
	// record, so one summary permanently blocked the remainder.
	// A summary is a boundary, not a live turn: the remainder
	// still migrates after it.
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
	// Partial migration (as a crashed import would leave it),
	// then a compaction summary lands in the transcript.
	n, err := tr.MigrateFrom(msgs[:2], importedAt)
	if err != nil || n != 2 {
		t.Fatalf("partial MigrateFrom = %d, %v; want 2, nil", n, err)
	}
	if err := tr.AppendSummary("Summary: the user's favourite number is 17."); err != nil {
		t.Fatalf("AppendSummary: %v", err)
	}
	// Retry with the full history: the remainder migrates, past
	// the summary, without duplicating what is already there.
	n, err = tr.MigrateFrom(msgs, importedAt)
	if err != nil || n != 3 {
		t.Fatalf("MigrateFrom after summary = %d, %v; want 3, nil", n, err)
	}
	recs, err := tr.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(recs) != 6 {
		t.Fatalf("records = %d, want 6 (2 migrated + summary + 3 remainder)", len(recs))
	}
	if recs[2].Kind != KindSummary {
		t.Fatalf("record 2 = %+v, want the summary boundary in place", recs[2])
	}
	for i, want := range []string{"turn one", "turn two", "turn three", "turn four", "turn five"} {
		got := recs[i]
		if i >= 2 {
			got = recs[i+1] // skip the summary at index 2
		}
		if got.Kind != KindMessage || got.Message == nil || got.Message.Content != want || !got.Migrated {
			t.Fatalf("record %d = %+v, want migrated message %q", i, got, want)
		}
	}
	// A further attempt imports nothing.
	if n, err := tr.MigrateFrom(msgs, importedAt); err != nil || n != 0 {
		t.Fatalf("third MigrateFrom = %d, %v; want 0, nil", n, err)
	}
	// A live (non-migrated) turn still blocks migration: the
	// boundary rule is for summaries, not for turns the session
	// ran itself.
	if err := tr.AppendMessage(model.Message{Role: "user", Content: "live turn"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if n, err := tr.MigrateFrom(msgs, importedAt); err != nil || n != 0 {
		t.Fatalf("MigrateFrom after live turns = %d, %v; want 0, nil", n, err)
	}
}
