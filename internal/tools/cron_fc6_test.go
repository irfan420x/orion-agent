// F-C6 tests: one corrupt job must not poison the whole chat.
// Each invalid entry is quarantined at load — skipped, listed with
// its reason, removable by id — while the valid jobs keep running
// and cron management keeps working for that chat.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cronQuarantinedOut is one quarantined entry in a cron_list reply.
type cronQuarantinedOut struct {
	ID         string `json:"id"`
	Expression string `json:"expression"`
	Prompt     string `json:"prompt"`
	Reason     string `json:"reason"`
	Corrupt    bool   `json:"corrupt"`
}

// cronListFC6Out is a cron_list reply including quarantined jobs.
type cronListFC6Out struct {
	cronListOut
	Quarantined []cronQuarantinedOut `json:"quarantined"`
}

func mustCronListFC6(t *testing.T, tool *cronListTool, ctx context.Context) cronListFC6Out {
	t.Helper()
	out, err := tool.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("cron_list: %v", err)
	}
	var parsed cronListFC6Out
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("cron_list result is not JSON (%q): %v", out, err)
	}
	return parsed
}

// writeCronFile writes jobs as the chat's cron.json, exactly as a
// hand edit would leave it — the seam for putting corrupt entries
// on disk without going through cron_schedule (which validates).
func writeCronFile(t *testing.T, dir string, chatID int64, jobs []*CronJob) {
	t.Helper()
	chatDir := filepath.Join(dir, "chats", fmt.Sprintf("%d", chatID))
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatalf("writeCronFile mkdir: %v", err)
	}
	data, err := json.MarshalIndent(struct {
		Jobs []*CronJob `json:"jobs"`
	}{Jobs: jobs}, "", "  ")
	if err != nil {
		t.Fatalf("writeCronFile encode: %v", err)
	}
	if err := atomicWriteFile(filepath.Join(chatDir, cronJobsFile), append(data, '\n'), 0o600); err != nil {
		t.Fatalf("writeCronFile: %v", err)
	}
}

// writeCronFileRaw writes raw bytes as the chat's cron.json (for a
// null entry, which a []*CronJob cannot spell except as a nil
// element — used for the missing-id case below).
func writeCronFileRaw(t *testing.T, dir string, chatID int64, data []byte) {
	t.Helper()
	chatDir := filepath.Join(dir, "chats", fmt.Sprintf("%d", chatID))
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatalf("writeCronFileRaw mkdir: %v", err)
	}
	if err := atomicWriteFile(filepath.Join(chatDir, cronJobsFile), data, 0o600); err != nil {
		t.Fatalf("writeCronFileRaw: %v", err)
	}
}

// cronTestJob builds a job as it would sit in cron.json.
func cronTestJob(id, expr, prompt, nextRun string) *CronJob {
	return &CronJob{
		ID: id, ChatID: 111, Requester: 111,
		Expression: expr, Prompt: prompt,
		CreatedAt: "2026-11-15T00:00:00Z",
		NextRun:   nextRun,
	}
}

func TestCronQuarantinedJobDoesNotBlockChat(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	future := now.Add(time.Hour).Format(time.RFC3339)
	// One corrupt job between two valid ones, in file order.
	writeCronFile(t, dir, 111, []*CronJob{
		cronTestJob("cron_good_one", "* * * * *", "good one", future),
		cronTestJob("cron_bad_expr", "99 99 * * *", "bad expression", future),
		cronTestJob("cron_good_two", "0 9 * * *", "good two", future),
	})
	// A fresh store is a daemon restart: the in-memory bucket
	// must not mask the disk file (the old whole-file failure
	// happened exactly on this path).
	store := NewPlanningStore(dir)
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)

	// (i) list works and shows the corrupt job with its reason.
	got := mustCronListFC6(t, list, ctx)
	if got.Count != 2 || len(got.Jobs) != 2 {
		t.Fatalf("list = %d jobs, want the 2 valid ones", got.Count)
	}
	if got.Jobs[0].ID != "cron_good_one" || got.Jobs[1].ID != "cron_good_two" {
		t.Fatalf("list order = %q, %q, want good_one then good_two (file order kept)", got.Jobs[0].ID, got.Jobs[1].ID)
	}
	if len(got.Quarantined) != 1 {
		t.Fatalf("quarantined = %+v, want the 1 corrupt job", got.Quarantined)
	}
	q := got.Quarantined[0]
	if q.ID != "cron_bad_expr" || !q.Corrupt || !strings.Contains(q.Reason, "invalid expression") {
		t.Fatalf("quarantined entry = %+v, want id cron_bad_expr / corrupt / an invalid-expression reason", q)
	}
	if q.Expression != "99 99 * * *" || q.Prompt != "bad expression" {
		t.Fatalf("quarantined entry = %+v, want the stored expression and prompt echoed", q)
	}

	// (iii) cron_schedule keeps working for the chat while the
	// corrupt job is present, and says so in its note.
	out, err := sched.Execute(ctx, json.RawMessage(`{"expression":"0 10 * * *","prompt":"a third job"}`))
	if err != nil {
		t.Fatalf("cron_schedule with a corrupt job present: %v", err)
	}
	var scheduled struct {
		Note string `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &scheduled); err != nil {
		t.Fatalf("schedule result is not JSON: %v", err)
	}
	if !strings.Contains(scheduled.Note, "quarantined") {
		t.Fatalf("schedule note = %q, want it to mention the quarantined job", scheduled.Note)
	}
	got = mustCronListFC6(t, list, ctx)
	if got.Count != 3 || len(got.Quarantined) != 1 {
		t.Fatalf("after schedule list = %d jobs + %d quarantined, want 3 + 1", got.Count, len(got.Quarantined))
	}

	// (i) the scheduler runs the valid due job and never the
	// corrupt one; the tick itself does not fail.
	rewriteCronJob(t, store, ctx, "cron_good_one", func(j *CronJob) {
		j.NextRun = now.Add(-time.Minute).Format(time.RFC3339)
	})
	runner := &cronFakeRunner{}
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue with a corrupt job present: %v (want nil — the tick must not fail)", err)
	}
	if runner.count() != 1 {
		t.Fatalf("runner calls = %d, want 1 (only the valid due job)", runner.count())
	}
	if runner.calls[0].ID != "cron_good_one" || runner.calls[0].Prompt != "good one" {
		t.Fatalf("runner call = %+v, want the valid due job", runner.calls[0])
	}
	// The outcome apply kept the quarantined entry in the file
	// (it must not be silently dropped by a save), and the run
	// recorded normally on the valid job.
	got = mustCronListFC6(t, list, ctx)
	if got.Count != 3 || len(got.Quarantined) != 1 || got.Quarantined[0].ID != "cron_bad_expr" {
		t.Fatalf("after tick list = %d jobs + %+v, want 3 jobs and the quarantined entry still there", got.Count, got.Quarantined)
	}
	for _, j := range got.Jobs {
		if j.ID == "cron_good_one" && j.LastResult != "ok" {
			t.Fatalf("job after run last result = %q, want ok", j.LastResult)
		}
	}
	data, err := readStateFile(dir + "/chats/111/" + cronJobsFile)
	if err != nil {
		t.Fatalf("cron.json not readable after tick: %v", err)
	}
	if !strings.Contains(string(data), "cron_bad_expr") {
		t.Fatalf("cron.json no longer contains the quarantined job after the tick (it was silently dropped)")
	}
}

func TestCronCancelRemovesQuarantinedJob(t *testing.T) {
	dir := t.TempDir()
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	writeCronFile(t, dir, 111, []*CronJob{
		cronTestJob("cron_keeper", "0 9 * * *", "keeper", future),
		cronTestJob("cron_empty_prompt", "0 10 * * *", "   ", future),
	})
	store := NewPlanningStore(dir)
	_, list, cancel := cronTestTools(store)
	ctx := cronChatCtx(111, 111)

	got := mustCronListFC6(t, list, ctx)
	if got.Count != 1 || len(got.Quarantined) != 1 {
		t.Fatalf("list = %d jobs + %d quarantined, want 1 + 1", got.Count, len(got.Quarantined))
	}
	if got.Quarantined[0].ID != "cron_empty_prompt" || !strings.Contains(got.Quarantined[0].Reason, "empty prompt") {
		t.Fatalf("quarantined entry = %+v, want the empty-prompt job with its reason", got.Quarantined[0])
	}

	// (ii) the owner can remove the quarantined job by id, from
	// Telegram, with no disk access — and the result says the
	// removed job was quarantined.
	out, err := cancel.Execute(ctx, json.RawMessage(`{"job_id":"cron_empty_prompt"}`))
	if err != nil {
		t.Fatalf("cron_cancel of the quarantined job: %v", err)
	}
	var cancelled map[string]any
	if err := json.Unmarshal([]byte(out), &cancelled); err != nil {
		t.Fatalf("cancel result is not JSON: %v", err)
	}
	if cancelled["cancelled"] != true || cancelled["job_id"] != "cron_empty_prompt" || cancelled["quarantined"] != true {
		t.Fatalf("cancel result = %s, want {cancelled:true, job_id, quarantined:true}", out)
	}
	got = mustCronListFC6(t, list, ctx)
	if got.Count != 1 || len(got.Quarantined) != 0 {
		t.Fatalf("after cancel list = %d jobs + %d quarantined, want 1 + 0", got.Count, len(got.Quarantined))
	}
	if got.Jobs[0].ID != "cron_keeper" {
		t.Fatalf("after cancel the remaining job is %q, want the valid keeper", got.Jobs[0].ID)
	}
	data, err := readStateFile(dir + "/chats/111/" + cronJobsFile)
	if err != nil {
		t.Fatalf("cron.json not readable after cancel: %v", err)
	}
	if strings.Contains(string(data), "cron_empty_prompt") {
		t.Fatalf("cron.json still contains the cancelled quarantined job")
	}
	if !strings.Contains(string(data), "cron_keeper") {
		t.Fatalf("cron.json lost the valid job when the quarantined one was cancelled")
	}
	// Cancelling it again is an unknown-job error.
	if _, err := cancel.Execute(ctx, json.RawMessage(`{"job_id":"cron_empty_prompt"}`)); err == nil {
		t.Fatalf("second cancel of the quarantined job: got nil error, want unknown job")
	} else if !strings.Contains(err.Error(), "unknown job") {
		t.Fatalf("second cancel error = %v, want unknown job", err)
	}
}

func TestCronQuarantineValidationRules(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	future := now.Add(time.Hour).Format(time.RFC3339)
	valid := func() *CronJob { return cronTestJob("cron_valid", "0 9 * * *", "valid", future) }
	cases := []struct {
		name       string
		corrupt    *CronJob // nil = raw-JSON case below
		raw        []byte   // set instead of corrupt for the null-entry case
		wantReason string
		wantID     string
	}{
		{"invalid expression", cronTestJob("cron_bad", "0 25 * * *", "bad expr", future), nil, "invalid expression", "cron_bad"},
		{"empty prompt", cronTestJob("cron_bad", "0 9 * * *", "   ", future), nil, "empty prompt", "cron_bad"},
		{"unparsable next run", cronTestJob("cron_bad", "0 9 * * *", "bad next", "not-a-time"), nil, "unparsable next run", "cron_bad"},
		{"negative failures", func() *CronJob {
			j := cronTestJob("cron_bad", "0 9 * * *", "bad count", future)
			j.ConsecutiveFailures = -1
			return j
		}(), nil, "negative failure count", "cron_bad"},
		{"duplicate id", cronTestJob("cron_valid", "0 10 * * *", "second with the same id", future), nil, "duplicate job id", "cron_valid"},
		{"missing id", cronTestJob("", "0 9 * * *", "no id", future), nil, "missing job id", ""},
		{"null entry", nil, []byte(`{"jobs":[null,` + mustCronJSON(t, valid()) + `]}`), "missing job id", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.raw != nil {
				writeCronFileRaw(t, dir, 111, c.raw)
			} else {
				writeCronFile(t, dir, 111, []*CronJob{valid(), c.corrupt})
			}
			store := NewPlanningStore(dir)
			sched, list, _ := cronTestTools(store)
			ctx := cronChatCtx(111, 111)
			// The valid job loads; the corrupt one is
			// quarantined with its reason — and list does
			// not fail.
			got := mustCronListFC6(t, list, ctx)
			if got.Count != 1 || len(got.Jobs) != 1 || got.Jobs[0].ID != "cron_valid" {
				t.Fatalf("list = %+v, want only the valid job", got)
			}
			if len(got.Quarantined) != 1 {
				t.Fatalf("quarantined = %+v, want exactly the corrupt entry", got.Quarantined)
			}
			q := got.Quarantined[0]
			if q.ID != c.wantID || !q.Corrupt || !strings.Contains(q.Reason, c.wantReason) {
				t.Fatalf("quarantined entry = %+v, want id %q / corrupt / reason containing %q", q, c.wantID, c.wantReason)
			}
			// A schedule round trip (load-modify-save) must
			// not silently drop the quarantined entry — not
			// even the ones with no id, which cron_cancel
			// cannot address.
			if _, err := sched.Execute(ctx, json.RawMessage(`{"expression":"0 11 * * *","prompt":"another valid job"}`)); err != nil {
				t.Fatalf("cron_schedule with a quarantined job present: %v", err)
			}
			got = mustCronListFC6(t, list, ctx)
			if got.Count != 2 || len(got.Quarantined) != 1 {
				t.Fatalf("after schedule list = %d jobs + %d quarantined, want 2 + 1 (quarantined entry survived the save)", got.Count, len(got.Quarantined))
			}
		})
	}
}

func mustCronJSON(t *testing.T, job *CronJob) string {
	t.Helper()
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("mustCronJSON: %v", err)
	}
	return string(data)
}

func TestCronCancelRemovesFirstMatchingEntry(t *testing.T) {
	// Duplicate ids are ambiguous by construction: cancel
	// removes the first entry in file order with that id, so
	// the owner can clear both with two cancels.
	dir := t.TempDir()
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	writeCronFile(t, dir, 111, []*CronJob{
		cronTestJob("cron_dup", "0 9 * * *", "first", future),
		cronTestJob("cron_dup", "0 10 * * *", "second (duplicate)", future),
	})
	store := NewPlanningStore(dir)
	_, list, cancel := cronTestTools(store)
	ctx := cronChatCtx(111, 111)

	got := mustCronListFC6(t, list, ctx)
	if got.Count != 1 || len(got.Quarantined) != 1 || got.Jobs[0].Prompt != "first" {
		t.Fatalf("list = %+v, want the first entry valid and the duplicate quarantined", got)
	}
	// First cancel takes the valid first entry — and frees the
	// id, so the duplicate loads as valid on the next read (it
	// now holds the id alone).
	if _, err := cancel.Execute(ctx, json.RawMessage(`{"job_id":"cron_dup"}`)); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	got = mustCronListFC6(t, list, ctx)
	if got.Count != 1 || len(got.Quarantined) != 0 || got.Jobs[0].Prompt != "second (duplicate)" {
		t.Fatalf("after first cancel list = %+v, want the duplicate promoted to the valid job", got)
	}
	// Second cancel takes the promoted job (as a valid job now).
	out, err := cancel.Execute(ctx, json.RawMessage(`{"job_id":"cron_dup"}`))
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	var cancelled map[string]any
	if err := json.Unmarshal([]byte(out), &cancelled); err != nil {
		t.Fatalf("cancel result is not JSON: %v", err)
	}
	if cancelled["cancelled"] != true || cancelled["quarantined"] != false {
		t.Fatalf("second cancel result = %s, want {cancelled:true, quarantined:false} (promoted to valid)", out)
	}
	got = mustCronListFC6(t, list, ctx)
	if got.Count != 0 || len(got.Quarantined) != 0 {
		t.Fatalf("after second cancel list = %d jobs + %d quarantined, want 0 + 0", got.Count, len(got.Quarantined))
	}
}
