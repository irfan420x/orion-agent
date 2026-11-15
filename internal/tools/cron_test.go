package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/permissions"
)

func cronChatCtx(chatID, senderID int64) context.Context {
	return permissions.WithApprovalContext(context.Background(), chatID, senderID)
}

func cronTestTools(store *PlanningStore) (*cronScheduleTool, *cronListTool, *cronCancelTool) {
	if store == nil {
		store = NewPlanningStore("")
	}
	return &cronScheduleTool{store: store}, &cronListTool{store: store}, &cronCancelTool{store: store}
}

type cronJobOut struct {
	Job struct {
		ID                  string `json:"id"`
		Expression          string `json:"expression"`
		Prompt              string `json:"prompt"`
		NextRun             string `json:"next_run_utc"`
		LastRun             string `json:"last_run_utc"`
		LastResult          string `json:"last_result"`
		ConsecutiveFailures int    `json:"consecutive_failures"`
		Disabled            bool   `json:"disabled"`
	} `json:"job"`
	Timezone string `json:"timezone"`
	Note     string `json:"note"`
}

type cronListOut struct {
	Jobs []struct {
		ID                  string `json:"id"`
		Expression          string `json:"expression"`
		Prompt              string `json:"prompt"`
		NextRun             string `json:"next_run_utc"`
		LastRun             string `json:"last_run_utc"`
		LastResult          string `json:"last_result"`
		ConsecutiveFailures int    `json:"consecutive_failures"`
		Disabled            bool   `json:"disabled"`
	} `json:"jobs"`
	Count    int    `json:"count"`
	Timezone string `json:"timezone"`
}

func mustCronSchedule(t *testing.T, tool *cronScheduleTool, ctx context.Context, expr, prompt string) cronJobOut {
	t.Helper()
	out, err := tool.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"expression":%q,"prompt":%q}`, expr, prompt)))
	if err != nil {
		t.Fatalf("cron_schedule(%q): %v", expr, err)
	}
	var parsed cronJobOut
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("cron_schedule result is not JSON (%q): %v", out, err)
	}
	return parsed
}

func mustCronList(t *testing.T, tool *cronListTool, ctx context.Context) cronListOut {
	t.Helper()
	out, err := tool.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("cron_list: %v", err)
	}
	var parsed cronListOut
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("cron_list result is not JSON (%q): %v", out, err)
	}
	return parsed
}

// rewriteCronJob loads one chat's jobs, mutates the named job in
// place, and saves — the test seam for putting a job into a due /
// missed / failing state without waiting for real time to pass.
func rewriteCronJob(t *testing.T, store *PlanningStore, ctx context.Context, id string, fn func(*CronJob)) {
	t.Helper()
	dir, key := store.resolveBucket(ctx)
	// F-C27: flock before mu, like every cron site — this helper
	// used to take mu first, inverting the order on the same
	// (s.mu, cron-flock) pair.
	release, err := store.lockState(ctx, dir, cronJobsFile)
	if err != nil {
		t.Fatalf("rewriteCronJob lock: %v", err)
	}
	defer release()
	store.mu.Lock()
	defer store.mu.Unlock()
	jobs, err := loadCronJobs(store, dir, key)
	if err != nil {
		t.Fatalf("rewriteCronJob load: %v", err)
	}
	found := false
	for _, j := range jobs.validJobs() {
		if j.ID == id {
			fn(j)
			found = true
		}
	}
	if !found {
		t.Fatalf("rewriteCronJob: unknown job %q", id)
	}
	if err := saveCronJobs(store, dir, key, jobs); err != nil {
		t.Fatalf("rewriteCronJob save: %v", err)
	}
}

func TestCronParseValid(t *testing.T) {
	for _, expr := range []string{
		"* * * * *",
		"0 9 * * 1-5",
		"*/15 * * * *",
		"0 0 1,15 * *",
		"30 6 1-30/5 * *",
		"0 0 * * 7",        // 7 is Sunday
		"0 0 * * 0,6",      // weekend
		"  0  9  *  *  * ", // extra whitespace is fine
		"0 0 29 2 *",
	} {
		if _, err := parseCronExpression(expr); err != nil {
			t.Errorf("parseCronExpression(%q): unexpected error: %v", expr, err)
		}
	}
}

func TestCronParseInvalid(t *testing.T) {
	cases := []struct {
		expr string
		want string // substring of the error, naming the bad field
	}{
		{"* * * *", "exactly 5 fields"},
		{"* * * * * *", "exactly 5 fields"},
		{"", "exactly 5 fields"},
		{"60 * * * *", "minute"},
		{"-1 * * * *", "minute"},
		{"* 24 * * *", "hour"},
		{"* * 0 * *", "day-of-month"},
		{"* * 32 * *", "day-of-month"},
		{"* * * 0 *", "month"},
		{"* * * 13 *", "month"},
		{"* * * * 8", "day-of-week"},
		{"*/0 * * * *", "minute"},
		{"*/x * * * *", "minute"},
		{"5-1 * * * *", "minute"}, // reversed range
		{"1-2-3 * * * *", "minute"},
		{"JAN * * * *", "minute"},
		{"* * * JAN *", "month"},
		{"* * * * MON", "day-of-week"},
		{"@daily", "exactly 5 fields"},
	}
	for _, c := range cases {
		_, err := parseCronExpression(c.expr)
		if err == nil {
			t.Errorf("parseCronExpression(%q): got nil error, want one mentioning %q", c.expr, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("parseCronExpression(%q): error %q does not mention %q", c.expr, err, c.want)
		}
	}
}

func TestCronParseBareNumberStep(t *testing.T) {
	// A bare number with a step ("2/10") means n-max/step, as in
	// standard cron — not just the number itself.
	cases := []struct {
		field string
		want  []int
	}{
		{"2/10", []int{2, 12, 22, 32, 42, 52}},
		{"5/15", []int{5, 20, 35, 50}},
		{"0/30", []int{0, 30}},
		// Unchanged behaviour: "*" and ranges already ran to max.
		{"*/15", []int{0, 15, 30, 45}},
		{"1-30/5", []int{1, 6, 11, 16, 21, 26}},
		{"2", []int{2}}, // bare number without a step still means just 2
	}
	for _, c := range cases {
		mask, _, err := parseCronField(c.field, "minute", 0, 59)
		if err != nil {
			t.Errorf("parseCronField(%q): unexpected error: %v", c.field, err)
			continue
		}
		if got := cronMaskValues(mask, 59); !cronIntSlicesEqual(got, c.want) {
			t.Errorf("parseCronField(%q): minutes = %v, want %v", c.field, got, c.want)
		}
	}
}

func TestCronParseBareNumberStepOtherFields(t *testing.T) {
	// Same n-max/step rule in other fields (probed by the
	// independent verifier of 3eeccb0): hour 2/6 runs to the
	// 23 max, and a step larger than the remaining range
	// yields just the starting value.
	cases := []struct {
		field string
		min   int
		max   int
		want  []int
	}{
		{"2/6", 0, 23, []int{2, 8, 14, 20}},
		{"20/6", 0, 23, []int{20}},
		{"55/10", 0, 59, []int{55}},
		{"59/10", 0, 59, []int{59}},
	}
	for _, c := range cases {
		mask, _, err := parseCronField(c.field, "field", c.min, c.max)
		if err != nil {
			t.Errorf("parseCronField(%q): unexpected error: %v", c.field, err)
			continue
		}
		if got := cronMaskValues(mask, c.max); !cronIntSlicesEqual(got, c.want) {
			t.Errorf("parseCronField(%q): values = %v, want %v", c.field, got, c.want)
		}
	}
}

func cronMaskValues(mask uint64, max int) []int {
	var out []int
	for v := 0; v <= max; v++ {
		if mask&(1<<uint(v)) != 0 {
			out = append(out, v)
		}
	}
	return out
}

func cronIntSlicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCronNextRunBareNumberStep(t *testing.T) {
	cases := []struct {
		expr  string
		after string
		want  string
	}{
		{"2/10 * * * *", "2026-11-15T10:00:00Z", "2026-11-15T10:02:00Z"},
		{"2/10 * * * *", "2026-11-15T10:02:00Z", "2026-11-15T10:12:00Z"},
		{"2/10 * * * *", "2026-11-15T10:52:00Z", "2026-11-15T11:02:00Z"},
		{"5/15 * * * *", "2026-11-15T10:00:00Z", "2026-11-15T10:05:00Z"},
		{"5/15 * * * *", "2026-11-15T10:05:00Z", "2026-11-15T10:20:00Z"},
		{"0/30 * * * *", "2026-11-15T10:00:00Z", "2026-11-15T10:30:00Z"},
		{"0/30 * * * *", "2026-11-15T10:30:00Z", "2026-11-15T11:00:00Z"},
	}
	for _, c := range cases {
		after, err := time.Parse(time.RFC3339, c.after)
		if err != nil {
			t.Fatalf("bad test time %q: %v", c.after, err)
		}
		got, err := nextRunAfter(c.expr, after)
		if err != nil {
			t.Errorf("nextRunAfter(%q, %s): %v", c.expr, c.after, err)
			continue
		}
		if got.Format(time.RFC3339) != c.want {
			t.Errorf("nextRunAfter(%q, %s) = %s, want %s", c.expr, c.after, got.Format(time.RFC3339), c.want)
		}
	}
}

func TestCronNextRun(t *testing.T) {
	// 2026-11-15 is a Sunday; 2026-11-16 a Monday.
	cases := []struct {
		expr  string
		after string
		want  string
	}{
		{"0 9 * * *", "2026-11-15T08:00:00Z", "2026-11-15T09:00:00Z"},
		{"0 9 * * *", "2026-11-15T09:00:30Z", "2026-11-16T09:00:00Z"}, // 09:00 this minute is past
		{"0 9 * * *", "2026-11-15T09:00:00Z", "2026-11-16T09:00:00Z"}, // strictly after
		{"*/15 * * * *", "2026-11-15T10:07:00Z", "2026-11-15T10:15:00Z"},
		{"*/15 * * * *", "2026-11-15T10:15:00Z", "2026-11-15T10:30:00Z"},
		{"0 9 * * 1-5", "2026-11-15T10:00:00Z", "2026-11-16T09:00:00Z"}, // Sunday -> Monday
		{"0 9 * * 1-5", "2026-11-14T10:00:00Z", "2026-11-16T09:00:00Z"}, // Saturday -> Monday
		{"0 0 1 * *", "2026-11-15T00:00:00Z", "2026-12-01T00:00:00Z"},
		{"0 0 29 2 *", "2026-03-01T00:00:00Z", "2028-02-29T00:00:00Z"}, // next leap Feb 29
		{"0 9 * * 7", "2026-11-14T10:00:00Z", "2026-11-15T09:00:00Z"},  // 7 = Sunday
		{"0 9 * * 0", "2026-11-14T10:00:00Z", "2026-11-15T09:00:00Z"},  // 0 = Sunday
		// dom/dow OR: the 15th OR a Monday, whichever comes first.
		{"0 0 15 * 1", "2026-11-15T01:00:00Z", "2026-11-16T00:00:00Z"}, // Monday beats Dec 15
		{"0 0 15 * 1", "2026-11-16T01:00:00Z", "2026-11-23T00:00:00Z"}, // next Monday
		{"0 0 15 * 1", "2026-11-23T01:00:00Z", "2026-11-30T00:00:00Z"}, // Monday again
		{"30 6 1-30/5 * *", "2026-11-15T06:00:00Z", "2026-11-16T06:30:00Z"},
	}
	for _, c := range cases {
		after, err := time.Parse(time.RFC3339, c.after)
		if err != nil {
			t.Fatalf("bad test time %q: %v", c.after, err)
		}
		got, err := nextRunAfter(c.expr, after)
		if err != nil {
			t.Errorf("nextRunAfter(%q, %s): %v", c.expr, c.after, err)
			continue
		}
		if got.Format(time.RFC3339) != c.want {
			t.Errorf("nextRunAfter(%q, %s) = %s, want %s", c.expr, c.after, got.Format(time.RFC3339), c.want)
		}
	}
}

func TestCronScheduleListCancel(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, cancel := cronTestTools(store)
	ctx := cronChatCtx(111, 111)

	created := mustCronSchedule(t, sched, ctx, "0 9 * * 1-5", "summarise the news")
	if created.Job.ID == "" || !strings.HasPrefix(created.Job.ID, "cron_") {
		t.Fatalf("job id = %q, want a cron_ id", created.Job.ID)
	}
	if created.Job.Expression != "0 9 * * 1-5" || created.Job.Prompt != "summarise the news" {
		t.Fatalf("created job = %+v, want expression/prompt echoed", created.Job)
	}
	if created.Timezone != "UTC" {
		t.Fatalf("timezone = %q, want UTC", created.Timezone)
	}
	next, err := time.Parse(time.RFC3339, created.Job.NextRun)
	if err != nil {
		t.Fatalf("next run %q does not parse: %v", created.Job.NextRun, err)
	}
	if !next.After(time.Now().UTC()) {
		t.Fatalf("next run %s is not in the future", created.Job.NextRun)
	}
	// The next run is a weekday 09:00 UTC.
	if next.Hour() != 9 || next.Minute() != 0 || next.Weekday() == time.Saturday || next.Weekday() == time.Sunday {
		t.Fatalf("next run %s is not a weekday 09:00 UTC", created.Job.NextRun)
	}

	listed := mustCronList(t, list, ctx)
	if listed.Count != 1 || len(listed.Jobs) != 1 || listed.Jobs[0].ID != created.Job.ID {
		t.Fatalf("list = %+v, want the one created job", listed)
	}
	if listed.Timezone != "UTC" {
		t.Fatalf("list timezone = %q, want UTC", listed.Timezone)
	}

	out, err := cancel.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"job_id":%q}`, created.Job.ID)))
	if err != nil {
		t.Fatalf("cron_cancel: %v", err)
	}
	var cancelled map[string]any
	if err := json.Unmarshal([]byte(out), &cancelled); err != nil {
		t.Fatalf("cancel result is not JSON: %v", err)
	}
	if cancelled["cancelled"] != true || cancelled["job_id"] != created.Job.ID {
		t.Fatalf("cancel result = %s, want {cancelled:true, job_id}", out)
	}
	if got := mustCronList(t, list, ctx); got.Count != 0 {
		t.Fatalf("list after cancel = %d jobs, want 0", got.Count)
	}
	// Cancelling again is an unknown-job error, not a silent success.
	if _, err := cancel.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"job_id":%q}`, created.Job.ID))); err == nil {
		t.Fatalf("second cron_cancel: got nil error, want unknown job")
	} else if !strings.Contains(err.Error(), "unknown job") {
		t.Fatalf("second cron_cancel error = %v, want unknown job", err)
	}
}

// TestCronScheduleNoteStatesNextRunUTC (F-C16 mitigation): the
// schedule confirmation must state the concrete consequence of
// UTC-only scheduling — the job's first run in UTC, and how to
// convert it — so an SGT user writing `0 9 * * *` sees the
// 17:00-local trap before the job ever fires. It must not invent
// a local time: there is no per-chat timezone source.
func TestCronScheduleNoteStatesNextRunUTC(t *testing.T) {
	store := NewPlanningStore("")
	sched, _, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)

	created := mustCronSchedule(t, sched, ctx, "0 9 * * 1-5", "morning brief")
	if created.Note == "" {
		t.Fatalf("schedule note is empty, want the UTC consequence stated")
	}
	if !strings.Contains(created.Note, created.Job.NextRun) {
		t.Fatalf("note %q does not state the first run %s", created.Note, created.Job.NextRun)
	}
	if !strings.Contains(created.Note, "no per-chat timezone") {
		t.Fatalf("note %q does not say there is no per-chat timezone", created.Note)
	}
	if !strings.Contains(created.Note, "UTC offset") || !strings.Contains(created.Note, "UTC+8") {
		t.Fatalf("note %q does not give the UTC-offset conversion (want the UTC+8 worked example)", created.Note)
	}
}

func TestCronScheduleValidation(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)

	cases := []struct {
		name string
		args string
		want string
	}{
		{"empty expression", `{"expression":"","prompt":"hi"}`, "expression must not be empty"},
		{"bad expression names field", `{"expression":"0 25 * * *","prompt":"hi"}`, "hour"},
		{"empty prompt", `{"expression":"0 9 * * *","prompt":"  "}`, "prompt must not be empty"},
		{"prompt too long", fmt.Sprintf(`{"expression":"0 9 * * *","prompt":%q}`, strings.Repeat("x", cronMaxPromptRunes+1)), "too long"},
		{"missing prompt", `{"expression":"0 9 * * *"}`, "prompt must not be empty"},
	}
	for _, c := range cases {
		if _, err := sched.Execute(ctx, json.RawMessage(c.args)); err == nil {
			t.Errorf("%s: got nil error", c.name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v does not mention %q", c.name, err, c.want)
		}
	}
	// No chat in the context: scheduling fails cleanly (a CLI run
	// has nowhere to deliver a scheduled turn).
	if _, err := sched.Execute(context.Background(), json.RawMessage(`{"expression":"0 9 * * *","prompt":"hi"}`)); err == nil {
		t.Errorf("no-chat schedule: got nil error")
	} else if !strings.Contains(err.Error(), "needs a Telegram chat") {
		t.Errorf("no-chat schedule error = %v, want a Telegram-chat error", err)
	}
	// No sender: the approval context would have nobody to prompt.
	noSender := permissions.WithApprovalContext(context.Background(), 111, 0)
	if _, err := sched.Execute(noSender, json.RawMessage(`{"expression":"0 9 * * *","prompt":"hi"}`)); err == nil {
		t.Errorf("no-sender schedule: got nil error")
	} else if !strings.Contains(err.Error(), "needs a sender") {
		t.Errorf("no-sender schedule error = %v, want a sender error", err)
	}
	// Listing with no chat fails cleanly (F-C17), like
	// scheduling: a no-chat run must not silently work on the
	// "local" bucket.
	if _, err := list.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Errorf("no-chat list: got nil error, want a Telegram-chat error")
	} else if !strings.Contains(err.Error(), "needs a Telegram chat") {
		t.Errorf("no-chat list error = %v, want a Telegram-chat error", err)
	}
	// No sender either: the approval context would have nobody
	// to attribute the list to.
	noSenderList := permissions.WithApprovalContext(context.Background(), 111, 0)
	if _, err := list.Execute(noSenderList, json.RawMessage(`{}`)); err == nil {
		t.Errorf("no-sender list: got nil error, want a sender error")
	} else if !strings.Contains(err.Error(), "needs a sender") {
		t.Errorf("no-sender list error = %v, want a sender error", err)
	}
}

func TestCronPerChatCap(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	for i := 0; i < cronMaxJobsPerChat; i++ {
		mustCronSchedule(t, sched, ctx, "0 9 * * *", fmt.Sprintf("job %d", i))
	}
	if got := mustCronList(t, list, ctx); got.Count != cronMaxJobsPerChat {
		t.Fatalf("list = %d jobs, want %d", got.Count, cronMaxJobsPerChat)
	}
	_, err := sched.Execute(ctx, json.RawMessage(`{"expression":"0 9 * * *","prompt":"one too many"}`))
	if err == nil {
		t.Fatalf("21st job: got nil error, want the cap error")
	}
	if !strings.Contains(err.Error(), "job limit reached") {
		t.Fatalf("21st job error = %v, want the cap error", err)
	}
}

func TestCronPerChatIsolation(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, cancel := cronTestTools(store)
	ctxA := cronChatCtx(111, 111)
	ctxB := cronChatCtx(222, 222)

	jobA1 := mustCronSchedule(t, sched, ctxA, "0 9 * * *", "A job 1")
	jobA2 := mustCronSchedule(t, sched, ctxA, "0 10 * * *", "A job 2")

	// Chat B sees none of A's jobs.
	if got := mustCronList(t, list, ctxB); got.Count != 0 {
		t.Fatalf("chat B list = %d jobs, want 0 (A has 2)", got.Count)
	}
	// Chat B cannot cancel A's jobs: the id is unknown in B's bucket.
	for _, id := range []string{jobA1.Job.ID, jobA2.Job.ID} {
		if _, err := cancel.Execute(ctxB, json.RawMessage(fmt.Sprintf(`{"job_id":%q}`, id))); err == nil {
			t.Fatalf("chat B cancel of A's job %s: got nil error", id)
		} else if !strings.Contains(err.Error(), "unknown job") {
			t.Fatalf("chat B cancel error = %v, want unknown job", err)
		}
	}
	// A's jobs are untouched.
	if got := mustCronList(t, list, ctxA); got.Count != 2 {
		t.Fatalf("chat A list = %d jobs, want 2", got.Count)
	}
	// Cancelling with a wrong/empty id is an invalid_args error.
	if _, err := cancel.Execute(ctxA, json.RawMessage(`{"job_id":""}`)); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("empty job_id cancel err = %v, want must-not-be-empty", err)
	}
}

func TestCronPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store1 := NewPlanningStore(dir)
	sched, _, _ := cronTestTools(store1)
	ctx := cronChatCtx(111, 111)
	created := mustCronSchedule(t, sched, ctx, "30 7 * * *", "morning brief")
	// The file exists on disk, 0600, next to the other per-chat state.
	data, err := readStateFile(dir + "/chats/111/" + cronJobsFile)
	if err != nil {
		t.Fatalf("cron.json not readable after schedule: %v", err)
	}
	if !strings.Contains(string(data), created.Job.ID) {
		t.Fatalf("cron.json does not contain job %s", created.Job.ID)
	}

	// A fresh store on the same dir is a daemon restart: the job
	// is intact, same id, expression, prompt and next run.
	store2 := NewPlanningStore(dir)
	_, list2, _ := cronTestTools(store2)
	got := mustCronList(t, list2, ctx)
	if got.Count != 1 {
		t.Fatalf("after restart list = %d jobs, want 1", got.Count)
	}
	j := got.Jobs[0]
	if j.ID != created.Job.ID || j.Expression != "30 7 * * *" || j.Prompt != "morning brief" || j.NextRun != created.Job.NextRun {
		t.Fatalf("after restart job = %+v, want %+v", j, created.Job)
	}
}

func TestCronCorruptFile(t *testing.T) {
	dir := t.TempDir()
	store := NewPlanningStore(dir)
	sched, _, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	mustCronSchedule(t, sched, ctx, "0 9 * * *", "will corrupt")
	// Corrupt the file by hand: a silent reset would hide the loss.
	if err := atomicWriteFile(dir+"/chats/111/"+cronJobsFile, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt the file: %v", err)
	}
	// A fresh store reads the corrupt file (the in-memory bucket
	// of `store` still holds the good jobs; the restart path is
	// what must fail loudly).
	store2 := NewPlanningStore(dir)
	_, list2, cancel2 := cronTestTools(store2)
	if _, err := list2.Execute(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatalf("corrupt list: got nil error, want a corrupt-file error")
	} else if !strings.Contains(err.Error(), "corrupt cron jobs file") {
		t.Fatalf("corrupt list error = %v, want corrupt-file", err)
	}
	if _, err := cancel2.Execute(ctx, json.RawMessage(`{"job_id":"cron_whatever"}`)); err == nil {
		t.Fatalf("corrupt cancel: got nil error")
	} else if !strings.Contains(err.Error(), "corrupt cron jobs file") {
		t.Fatalf("corrupt cancel error = %v, want corrupt-file", err)
	}
	// Scheduling over a corrupt file fails loudly too (it loads
	// before appending), instead of overwriting the evidence.
	if _, err := sched.Execute(ctx, json.RawMessage(`{"expression":"0 9 * * *","prompt":"new"}`)); err == nil {
		t.Fatalf("corrupt schedule: got nil error")
	}
}

// --- scheduler ---

type cronFakeRunner struct {
	mu    sync.Mutex
	calls []CronJob
	err   error
	// block, when non-nil, is closed to let a blocked call proceed.
	block chan struct{}
}

func (f *cronFakeRunner) run(ctx context.Context, job CronJob) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, job)
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	return "ran: " + job.Prompt, f.err
}

func (f *cronFakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func cronDueJob(t *testing.T, store *PlanningStore, ctx context.Context, sched *cronScheduleTool, expr, prompt string, nextRun time.Time) string {
	t.Helper()
	created := mustCronSchedule(t, sched, ctx, expr, prompt)
	rewriteCronJob(t, store, ctx, created.Job.ID, func(j *CronJob) {
		j.NextRun = nextRun.Format(time.RFC3339)
	})
	return created.Job.ID
}

func TestCronSchedulerFiresDueJob(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	id := cronDueJob(t, store, ctx, sched, "*/5 * * * *", "do the thing", now.Add(-time.Minute))

	runner := &cronFakeRunner{}
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	if runner.count() != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.count())
	}
	if runner.calls[0].Prompt != "do the thing" || runner.calls[0].ChatID != 111 || runner.calls[0].Requester != 111 {
		t.Fatalf("runner call = %+v, want chat 111 / requester 111 / the prompt", runner.calls[0])
	}
	got := mustCronList(t, list, ctx)
	if got.Count != 1 {
		t.Fatalf("list = %d jobs, want 1", got.Count)
	}
	j := got.Jobs[0]
	if j.ID != id || j.LastResult != "ok" || j.ConsecutiveFailures != 0 || j.Disabled {
		t.Fatalf("job after run = %+v, want ok/0 failures/not disabled", j)
	}
	next, err := time.Parse(time.RFC3339, j.NextRun)
	if err != nil {
		t.Fatalf("next run %q: %v", j.NextRun, err)
	}
	if !next.After(now) {
		t.Fatalf("next run %s is not after the tick time %s", j.NextRun, now.Format(time.RFC3339))
	}
	if j.LastRun == "" {
		t.Fatalf("last run is empty after a run")
	}
	// A second tick at the same time fires nothing (next run is
	// in the future).
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("second ProcessDue: %v", err)
	}
	if runner.count() != 1 {
		t.Fatalf("runner calls after second tick = %d, want 1", runner.count())
	}
}

// TestCronSchedulerFiresDirFuncJob (F-C25): a job scheduled into
// a dirFunc-resolved bucket must be found by the scheduler's
// scan and fired — before the fix, cronBuckets only scanned
// <dataDir>/chats/<id> and the in-memory chat: buckets, so a
// job in a dirFunc dir never fired at all.
func TestCronSchedulerFiresDirFuncJob(t *testing.T) {
	dataDir := t.TempDir()
	store := NewPlanningStore(dataDir)
	custom := filepath.Join(dataDir, "custom-session")
	store.SetDirFunc(func(ctx context.Context) string { return custom })
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	id := cronDueJob(t, store, ctx, sched, "*/5 * * * *", "dirfunc job", now.Add(-time.Minute))

	// Sanity: the job really landed in the dirFunc dir, not
	// in the chat dir the scan already covers.
	if _, err := os.Stat(filepath.Join(custom, "cron.json")); err != nil {
		t.Fatalf("job not in dirFunc dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "chats", "111", "cron.json")); !os.IsNotExist(err) {
		t.Fatalf("job leaked into the chat dir")
	}

	runner := &cronFakeRunner{}
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	if runner.count() != 1 {
		t.Fatalf("runner calls = %d, want 1 (dirFunc job was not found by the scan)", runner.count())
	}
	if runner.calls[0].Prompt != "dirfunc job" || runner.calls[0].ChatID != 111 || runner.calls[0].Requester != 111 {
		t.Fatalf("runner call = %+v, want chat 111 / requester 111 / the prompt", runner.calls[0])
	}
	got := mustCronList(t, list, ctx)
	if got.Count != 1 {
		t.Fatalf("list = %d jobs, want 1", got.Count)
	}
	j := got.Jobs[0]
	if j.ID != id || j.LastResult != "ok" || j.ConsecutiveFailures != 0 || j.Disabled {
		t.Fatalf("job after run = %+v, want ok/0 failures/not disabled", j)
	}
}

func TestCronSchedulerSkipsMissedRun(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	// Due 61 minutes ago: missed by more than the 10-minute window.
	id := cronDueJob(t, store, ctx, sched, "0 9 * * *", "morning brief", now.Add(-61*time.Minute))

	runner := &cronFakeRunner{}
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	if runner.count() != 0 {
		t.Fatalf("runner calls = %d, want 0 (missed run is skipped, not run late)", runner.count())
	}
	got := mustCronList(t, list, ctx)
	j := got.Jobs[0]
	if j.ID != id {
		t.Fatalf("job id = %s, want %s", j.ID, id)
	}
	if !strings.Contains(j.LastResult, "skipped") {
		t.Fatalf("last result = %q, want a skipped note", j.LastResult)
	}
	if j.ConsecutiveFailures != 0 || j.Disabled {
		t.Fatalf("missed run counted as a failure: %+v", j)
	}
	next, err := time.Parse(time.RFC3339, j.NextRun)
	if err != nil {
		t.Fatalf("next run %q: %v", j.NextRun, err)
	}
	if !next.After(now) {
		t.Fatalf("next run %s is not after now", j.NextRun)
	}
}

func TestCronSchedulerBackoffAndDisable(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	id := cronDueJob(t, store, ctx, sched, "*/5 * * * *", "always fails", now.Add(-time.Minute))

	runner := &cronFakeRunner{err: fmt.Errorf("provider exploded")}
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)

	// Five consecutive failing ticks: failures climb 1..5 and the
	// 5th disables the job. Each tick's `now` is just past the
	// job's (backed-off) next run.
	tickNow := now
	for wantFailures := 1; wantFailures <= cronMaxFailures; wantFailures++ {
		if err := s.ProcessDue(ctx, tickNow); err != nil {
			t.Fatalf("ProcessDue #%d: %v", wantFailures, err)
		}
		if runner.count() != wantFailures {
			t.Fatalf("after tick #%d runner calls = %d, want %d", wantFailures, runner.count(), wantFailures)
		}
		got := mustCronList(t, list, ctx)
		j := got.Jobs[0]
		if j.ID != id {
			t.Fatalf("job id = %s, want %s", j.ID, id)
		}
		if j.ConsecutiveFailures != wantFailures {
			t.Fatalf("after tick #%d failures = %d, want %d", wantFailures, j.ConsecutiveFailures, wantFailures)
		}
		if wantFailures < cronMaxFailures && j.Disabled {
			t.Fatalf("after tick #%d job is disabled too early", wantFailures)
		}
		if wantFailures == cronMaxFailures && !j.Disabled {
			t.Fatalf("after %d failures job is not disabled", cronMaxFailures)
		}
		if !strings.Contains(j.LastResult, "error: provider exploded") && !strings.Contains(j.LastResult, "disabled after") {
			t.Fatalf("after tick #%d last result = %q, want the error", wantFailures, j.LastResult)
		}
		// Backoff: the next run is at least a minute out (the
		// first failure backs off 1 minute; later ones more).
		next, err := time.Parse(time.RFC3339, j.NextRun)
		if err != nil {
			t.Fatalf("next run %q: %v", j.NextRun, err)
		}
		if next.Before(tickNow.Add(time.Minute)) {
			t.Fatalf("after tick #%d next run %s is sooner than now+1m (no backoff)", wantFailures, j.NextRun)
		}
		tickNow = next.Add(30 * time.Second)
	}
	// Disabled: no further runs, even when the time comes.
	if err := s.ProcessDue(ctx, tickNow); err != nil {
		t.Fatalf("ProcessDue after disable: %v", err)
	}
	if runner.count() != cronMaxFailures {
		t.Fatalf("runner calls after disable = %d, want %d", runner.count(), cronMaxFailures)
	}
	// A successful run resets the failure count (re-enable by
	// hand here: disable is sticky until the owner re-creates,
	// so prove the reset on a second job instead).
	runner2 := &cronFakeRunner{err: fmt.Errorf("boom once")}
	s2 := NewCronScheduler(store)
	s2.SetRunner(runner2.run)
	id2 := cronDueJob(t, store, ctx, sched, "*/5 * * * *", "fails then works", now.Add(-time.Minute))
	if err := s2.ProcessDue(ctx, now); err != nil {
		t.Fatalf("failing tick: %v", err)
	}
	// The first job is disabled so it never fires; only the
	// second job ran (and failed once).
	if runner2.count() != 1 || runner2.calls[0].ID != id2 {
		t.Fatalf("runner2 calls = %+v, want only the second job", runner2.calls)
	}
	runner2.err = nil
	// Advance past the second job's backed-off next run.
	got := mustCronList(t, list, ctx)
	var next2 time.Time
	for _, j := range got.Jobs {
		if j.ID == id2 {
			next2, _ = time.Parse(time.RFC3339, j.NextRun)
		}
	}
	if err := s2.ProcessDue(ctx, next2.Add(30*time.Second)); err != nil {
		t.Fatalf("recovery tick: %v", err)
	}
	got = mustCronList(t, list, ctx)
	for _, j := range got.Jobs {
		if j.ID == id2 && (j.ConsecutiveFailures != 0 || j.LastResult != "ok" || j.Disabled) {
			t.Fatalf("recovered job = %+v, want 0 failures / ok / enabled", j)
		}
	}
}

func TestCronSchedulerNoRunner(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	id := cronDueJob(t, store, ctx, sched, "*/5 * * * *", "needs a runner", now.Add(-time.Minute))
	s := NewCronScheduler(store)
	// No runner installed.
	if err := s.ProcessDue(ctx, now); err == nil {
		t.Fatalf("ProcessDue without a runner: got nil error")
	} else if !strings.Contains(err.Error(), "no turn runner") {
		t.Fatalf("ProcessDue error = %v, want no-turn-runner", err)
	}
	// The job is untouched: still due, no failure recorded.
	got := mustCronList(t, list, ctx)
	if got.Jobs[0].ID != id || got.Jobs[0].ConsecutiveFailures != 0 || got.Jobs[0].LastResult != "" {
		t.Fatalf("job after no-runner tick = %+v, want untouched", got.Jobs[0])
	}
}

func TestCronSchedulerPerChatIsolation(t *testing.T) {
	store := NewPlanningStore("")
	sched, _, _ := cronTestTools(store)
	ctxA := cronChatCtx(111, 111)
	ctxB := cronChatCtx(222, 222)
	now := time.Now().UTC().Truncate(time.Second)
	cronDueJob(t, store, ctxA, sched, "*/5 * * * *", "A prompt", now.Add(-time.Minute))
	cronDueJob(t, store, ctxB, sched, "*/5 * * * *", "B prompt", now.Add(-time.Minute))
	// A third job, in chat A, not due yet.
	mustCronSchedule(t, sched, ctxA, "0 9 * * *", "A later")

	runner := &cronFakeRunner{}
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	if err := s.ProcessDue(ctxA, now); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	if runner.count() != 2 {
		t.Fatalf("runner calls = %d, want 2 (one per due chat)", runner.count())
	}
	seen := map[int64]string{}
	for _, c := range runner.calls {
		seen[c.ChatID] = c.Prompt
	}
	if seen[111] != "A prompt" || seen[222] != "B prompt" {
		t.Fatalf("runner calls = %v, want each chat's own prompt", seen)
	}
}

func TestCronSchedulerTickOverlapSkipped(t *testing.T) {
	store := NewPlanningStore("")
	sched, _, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	cronDueJob(t, store, ctx, sched, "*/5 * * * *", "slow job", now.Add(-time.Minute))

	release := make(chan struct{})
	runner := &cronFakeRunner{block: release}
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)

	// The first tick blocks inside the runner.
	firstDone := make(chan error, 1)
	go func() { firstDone <- s.ProcessDue(ctx, now) }()
	// Wait until the runner is entered (a call is recorded).
	deadline := time.Now().Add(5 * time.Second)
	for runner.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if runner.count() == 0 {
		close(release)
		t.Fatalf("first tick never entered the runner")
	}
	// A second tick while the first is still running must return
	// promptly (it is skipped, not overlapped) and must not run
	// the job a second time.
	secondDone := make(chan error, 1)
	go func() { secondDone <- s.ProcessDue(ctx, now) }()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("overlapping tick error = %v, want nil (skip)", err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatalf("overlapping tick did not return within 5s (it overlapped the running tick)")
	}
	if runner.count() != 1 {
		t.Fatalf("runner calls = %d, want 1 (no double-run)", runner.count())
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first tick error = %v", err)
	}
}

func TestCronSchedulerReloadAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	ctx := cronChatCtx(111, 111)

	store1 := NewPlanningStore(dir)
	sched1, _, _ := cronTestTools(store1)
	cronDueJob(t, store1, ctx, sched1, "*/5 * * * *", "survives restart", now.Add(-time.Minute))

	// Fresh store + fresh scheduler on the same dir: a restart.
	store2 := NewPlanningStore(dir)
	runner := &cronFakeRunner{}
	s2 := NewCronScheduler(store2)
	s2.SetRunner(runner.run)
	if err := s2.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue after restart: %v", err)
	}
	if runner.count() != 1 || runner.calls[0].Prompt != "survives restart" {
		t.Fatalf("runner calls = %+v, want the reloaded job", runner.calls)
	}
}

func TestCronRegistryWiring(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	for _, name := range []string{"cron_schedule", "cron_list", "cron_cancel"} {
		tool, ok := reg.Get(name)
		if !ok {
			t.Fatalf("registry has no %q", name)
		}
		rc, ok := tool.(RiskClasser)
		if !ok {
			t.Fatalf("%q does not implement RiskClasser", name)
		}
		if rc.RiskClass() != permissions.ClassSafe {
			t.Fatalf("%q risk class = %v, want safe", name, rc.RiskClass())
		}
		if permissions.Classify(name) != permissions.ClassSafe {
			t.Fatalf("gate classifies %q as %v, want safe", name, permissions.Classify(name))
		}
		if _, known := permissions.ClassifyKnown(name); !known {
			t.Fatalf("%q is not explicitly classified", name)
		}
	}
	// End to end through the registry: schedule + list with a
	// chat context, then cancel.
	ctx := cronChatCtx(111, 111)
	out, err := reg.Execute(ctx, "cron_schedule", json.RawMessage(`{"expression":"0 9 * * *","prompt":"via registry"}`))
	if err != nil {
		t.Fatalf("registry cron_schedule: %v", err)
	}
	var created cronJobOut
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("registry schedule result: %v", err)
	}
	out, err = reg.Execute(ctx, "cron_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("registry cron_list: %v", err)
	}
	var listed cronListOut
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		t.Fatalf("registry list result: %v", err)
	}
	if listed.Count != 1 || listed.Jobs[0].ID != created.Job.ID {
		t.Fatalf("registry list = %+v, want the scheduled job", listed)
	}
}
