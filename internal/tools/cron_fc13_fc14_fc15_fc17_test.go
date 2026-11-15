// P3-B fix tests: F-C13 (stored LastResult redaction), F-C14 (load
// validation of hand-edited fields), F-C17 (list/cancel need chat
// context), and the P2-D verifier residuals R1 (global cap counts
// valid jobs only), R2 (minimum-interval message derives from the
// constant), R3 (minimum-gap scan covers the full window).
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/permissions"
)

// TestCronStoredLastResultRedactsSecrets (F-C13): a runner error
// carrying a secret must not reach cron.json verbatim. The chat
// notice is redacted in runScheduledTurn (security.RedactSecrets);
// the stored LastResult must be redacted the same way, and the job
// must still record the failure.
func TestCronStoredLastResultRedactsSecrets(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	cronDueJob(t, store, ctx, sched, "*/5 * * * *", "do the thing", now.Add(-time.Minute))

	secret := "ghp_" + strings.Repeat("A", 36)
	runner := &cronFakeRunner{err: errors.New("provider failed, token was " + secret)}
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	got := mustCronList(t, list, ctx)
	if got.Count != 1 {
		t.Fatalf("list = %d jobs, want 1", got.Count)
	}
	j := got.Jobs[0]
	if strings.Contains(j.LastResult, secret) {
		t.Fatalf("stored last result = %q, want the secret redacted", j.LastResult)
	}
	if !strings.Contains(j.LastResult, "[REDACTED]") {
		t.Fatalf("stored last result = %q, want it to carry [REDACTED]", j.LastResult)
	}
	if !strings.HasPrefix(j.LastResult, "error:") {
		t.Fatalf("stored last result = %q, want the failure still recorded as an error outcome", j.LastResult)
	}
	if j.ConsecutiveFailures != 1 || j.Disabled {
		t.Fatalf("job after failed run = %+v, want 1 failure / not disabled", j)
	}
}

// TestCronLoadValidationHandEditedFields (F-C14): a hand-edited
// cron.json must not smuggle in a wrong ChatID, a bogus Requester,
// an unparsable CreatedAt/LastRun, or an over-cap prompt — each
// such entry is quarantined (F-C6 path), never trusted, while the
// valid jobs keep working.
func TestCronLoadValidationHandEditedFields(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	future := now.Add(time.Hour).Format(time.RFC3339)
	overCap := strings.Repeat("x", cronMaxPromptRunes+1)
	cases := []struct {
		name       string
		corrupt    func() *CronJob
		wantReason string
	}{
		{"chat id zero", func() *CronJob {
			j := cronTestJob("cron_bad", "0 9 * * *", "bad chat", future)
			j.ChatID = 0
			return j
		}, "missing chat id"},
		{"chat id mismatch", func() *CronJob {
			j := cronTestJob("cron_bad", "0 9 * * *", "bad chat", future)
			j.ChatID = 222
			return j
		}, "does not match"},
		{"requester zero", func() *CronJob {
			j := cronTestJob("cron_bad", "0 9 * * *", "bad requester", future)
			j.Requester = 0
			return j
		}, "invalid requester"},
		{"requester negative", func() *CronJob {
			j := cronTestJob("cron_bad", "0 9 * * *", "bad requester", future)
			j.Requester = -5
			return j
		}, "invalid requester"},
		{"created missing", func() *CronJob {
			j := cronTestJob("cron_bad", "0 9 * * *", "bad created", future)
			j.CreatedAt = ""
			return j
		}, "missing created time"},
		{"created unparsable", func() *CronJob {
			j := cronTestJob("cron_bad", "0 9 * * *", "bad created", future)
			j.CreatedAt = "not-a-time"
			return j
		}, "unparsable created time"},
		{"last run unparsable", func() *CronJob {
			j := cronTestJob("cron_bad", "0 9 * * *", "bad last run", future)
			j.LastRun = "not-a-time"
			return j
		}, "unparsable last run"},
		{"prompt over cap", func() *CronJob {
			j := cronTestJob("cron_bad", "0 9 * * *", overCap, future)
			return j
		}, "too long"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			valid := cronTestJob("cron_valid", "0 9 * * *", "valid", future)
			writeCronFile(t, dir, 111, []*CronJob{valid, c.corrupt()})
			store := NewPlanningStore(dir)
			_, list, _ := cronTestTools(store)
			got := mustCronListFC6(t, list, cronChatCtx(111, 111))
			if got.Count != 1 || len(got.Jobs) != 1 || got.Jobs[0].ID != "cron_valid" {
				t.Fatalf("list = %+v, want only the valid job", got)
			}
			if len(got.Quarantined) != 1 {
				t.Fatalf("quarantined = %+v, want exactly the corrupt entry", got.Quarantined)
			}
			q := got.Quarantined[0]
			if q.ID != "cron_bad" || !q.Corrupt || !strings.Contains(q.Reason, c.wantReason) {
				t.Fatalf("quarantined entry = %+v, want id cron_bad / corrupt / reason containing %q", q, c.wantReason)
			}
		})
	}
}

// TestCronLoadValidationAcceptsBoundaryValues (F-C14 controls): a
// prompt at exactly the cap and a well-formed LastRun load fine —
// the new checks quarantine only what is actually invalid.
func TestCronLoadValidationAcceptsBoundaryValues(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	future := now.Add(time.Hour).Format(time.RFC3339)
	atCap := cronTestJob("cron_cap", "0 9 * * *", strings.Repeat("x", cronMaxPromptRunes), future)
	atCap.LastRun = now.Format(time.RFC3339)
	dir := t.TempDir()
	writeCronFile(t, dir, 111, []*CronJob{atCap})
	store := NewPlanningStore(dir)
	_, list, _ := cronTestTools(store)
	got := mustCronListFC6(t, list, cronChatCtx(111, 111))
	if got.Count != 1 || len(got.Quarantined) != 0 {
		t.Fatalf("list = %+v, want the at-cap job valid and nothing quarantined", got)
	}
}

// TestCronMismatchedChatIDJobNeverRuns (F-C14): a job whose stored
// ChatID names another chat is quarantined at load, so the
// scheduler never runs it — the bucket's chat id, not the file's,
// decides where a job runs (the runJob override), and a foreign
// ChatID cannot even get a turn.
func TestCronMismatchedChatIDJobNeverRuns(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	past := now.Add(-time.Minute).Format(time.RFC3339)
	j := cronTestJob("cron_foreign", "*/5 * * * *", "foreign", past)
	j.ChatID = 222 // the file lives in chat 111's bucket
	writeCronFile(t, dir, 111, []*CronJob{j})
	store := NewPlanningStore(dir)
	ctx := cronChatCtx(111, 111)
	runner := &cronFakeRunner{}
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	if runner.count() != 0 {
		t.Fatalf("runner calls = %d, want 0 (the foreign-ChatID job must never run)", runner.count())
	}
}

// TestCronListAndCancelRequireChatContext (F-C17): cron_list and
// cron_cancel need a chat in the run context, like cron_schedule
// does — a no-chat run must fail cleanly instead of silently
// working on the "local" bucket. The scheduler's own scan is
// unaffected: it loads jobs directly, never through these tools.
func TestCronListAndCancelRequireChatContext(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, cancel := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	created := mustCronSchedule(t, sched, ctx, "0 9 * * *", "hi")
	// With context, list still works.
	if got := mustCronList(t, list, ctx); got.Count != 1 {
		t.Fatalf("list with chat = %d jobs, want 1", got.Count)
	}
	// No chat: list fails cleanly, same error shape as schedule.
	if _, err := list.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Errorf("no-chat list: got nil error, want a Telegram-chat error")
	} else if !strings.Contains(err.Error(), "needs a Telegram chat") {
		t.Errorf("no-chat list error = %v, want a Telegram-chat error", err)
	}
	// No sender: list fails cleanly, same error shape as schedule.
	noSender := permissions.WithApprovalContext(context.Background(), 111, 0)
	if _, err := list.Execute(noSender, json.RawMessage(`{}`)); err == nil {
		t.Errorf("no-sender list: got nil error, want a sender error")
	} else if !strings.Contains(err.Error(), "needs a sender") {
		t.Errorf("no-sender list error = %v, want a sender error", err)
	}
	// Cancel already required context (F-C9); lock it in: no chat
	// and no sender both fail before the job is even looked up.
	jobArg, _ := json.Marshal(map[string]string{"job_id": created.Job.ID})
	if _, err := cancel.Execute(context.Background(), jobArg); err == nil {
		t.Errorf("no-chat cancel: got nil error, want a Telegram-chat error")
	} else if !strings.Contains(err.Error(), "needs a Telegram chat") {
		t.Errorf("no-chat cancel error = %v, want a Telegram-chat error", err)
	}
	if _, err := cancel.Execute(noSender, jobArg); err == nil {
		t.Errorf("no-sender cancel: got nil error, want a sender error")
	} else if !strings.Contains(err.Error(), "needs a sender") {
		t.Errorf("no-sender cancel error = %v, want a sender error", err)
	}
	// And with context, cancel still works.
	if _, err := cancel.Execute(ctx, jobArg); err != nil {
		t.Fatalf("cancel with chat: %v", err)
	}
}

// TestCronGlobalCapCountsValidJobsOnly (P2-D R1): the global cap
// and the per-chat cap must count the same thing — valid jobs.
// A quarantined entry never fires, so it must not consume a slot
// of the 200-job global cap either.
func TestCronGlobalCapCountsValidJobsOnly(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	future := now.Add(time.Hour).Format(time.RFC3339)
	// On-disk: chat 111 holds one valid job and one quarantined
	// (bad expression) job; chat 222 holds one valid job.
	dir := t.TempDir()
	writeCronFile(t, dir, 111, []*CronJob{
		cronTestJob("cron_good", "0 9 * * *", "good", future),
		cronTestJob("cron_bad", "99 99 * * *", "bad expression", future),
	})
	other := cronTestJob("cron_other", "0 9 * * *", "other", future)
	other.ChatID, other.Requester = 222, 222
	writeCronFile(t, dir, 222, []*CronJob{other})
	store := NewPlanningStore(dir)
	store.mu.Lock()
	total := store.totalCronJobsLocked()
	store.mu.Unlock()
	if total != 2 {
		t.Fatalf("totalCronJobsLocked (disk) = %d, want 2 valid jobs (the quarantined entry must not count)", total)
	}
	// In-memory: a bucket saved with one valid and one
	// quarantined entry counts one.
	store2 := NewPlanningStore("")
	doc := &cronJobsDoc{entries: []cronJobEntry{
		{job: cronTestJob("cron_good", "0 9 * * *", "good", future)},
		{job: cronTestJob("cron_bad", "99 99 * * *", "bad expression", future), reason: "invalid expression: bad"},
	}}
	if err := saveCronJobs(store2, "", "chat:111", doc); err != nil {
		t.Fatalf("saveCronJobs: %v", err)
	}
	store2.mu.Lock()
	total2 := store2.totalCronJobsLocked()
	store2.mu.Unlock()
	if total2 != 1 {
		t.Fatalf("totalCronJobsLocked (memory) = %d, want 1 valid job (the quarantined entry must not count)", total2)
	}
}

// TestCronMinimumIntervalMessageDerivesFromConstant (P2-D R2): the
// rejection names the floor from cronMinInterval, so the message
// cannot lie if the constant ever changes.
func TestCronMinimumIntervalMessageDerivesFromConstant(t *testing.T) {
	store := NewPlanningStore("")
	sched, _, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	_, err := sched.Execute(ctx, json.RawMessage(`{"expression":"* * * * *","prompt":"too fast"}`))
	if err == nil {
		t.Fatalf("minutely schedule: got nil error, want minimum-interval rejection")
	}
	want := fmt.Sprintf("minimum interval of %d minutes", int(cronMinInterval.Minutes()))
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("rejection = %v, want it to name %q (derived from cronMinInterval)", err, want)
	}
}

// TestCronEnableReenablesDisabledJob (F-C15): a disabled job
// comes back with its id and history intact — only the disabled
// flag clears and the failure count resets — and it fires again.
func TestCronEnableReenablesDisabledJob(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	past := now.Add(-time.Minute).Format(time.RFC3339)
	lastRun := now.Add(-time.Hour).Format(time.RFC3339)
	disabled := cronTestJob("cron_disabled", "0 9 * * *", "morning brief", past)
	disabled.Disabled = true
	disabled.ConsecutiveFailures = 5
	disabled.LastRun = lastRun
	disabled.LastResult = "disabled after 5 consecutive failures: boom"
	writeCronFile(t, dir, 111, []*CronJob{disabled})
	store := NewPlanningStore(dir)
	ctx := cronChatCtx(111, 111)
	enable := &cronEnableTool{store: store}
	out, err := enable.Execute(ctx, json.RawMessage(`{"job_id":"cron_disabled"}`))
	if err != nil {
		t.Fatalf("cron_enable of the disabled job: %v", err)
	}
	var enabled cronEnableOut
	if err := json.Unmarshal([]byte(out), &enabled); err != nil {
		t.Fatalf("cron_enable result is not JSON (%q): %v", out, err)
	}
	if !enabled.Enabled || !enabled.WasDisabled || enabled.JobID != "cron_disabled" {
		t.Fatalf("cron_enable result = %s, want {enabled:true, job_id, was_disabled:true}", out)
	}
	_, list, _ := cronTestTools(store)
	got := mustCronList(t, list, ctx)
	if got.Count != 1 {
		t.Fatalf("list = %d jobs, want 1 (the same job, not a re-created one)", got.Count)
	}
	j := got.Jobs[0]
	if j.ID != "cron_disabled" || j.Disabled || j.ConsecutiveFailures != 0 {
		t.Fatalf("job after enable = %+v, want same id / not disabled / 0 failures", j)
	}
	if j.Prompt != "morning brief" || j.Expression != "0 9 * * *" {
		t.Fatalf("job after enable = %+v, want expression and prompt kept", j)
	}
	if j.LastRun != lastRun {
		t.Fatalf("last run after enable = %q, want %q (history kept)", j.LastRun, lastRun)
	}
	if j.LastResult != "re-enabled by the owner" {
		t.Fatalf("last result after enable = %q, want the re-enable recorded", j.LastResult)
	}
	next, err := time.Parse(time.RFC3339, j.NextRun)
	if err != nil {
		t.Fatalf("next run %q: %v", j.NextRun, err)
	}
	if !next.After(now) {
		t.Fatalf("next run %s is not after now %s (want the next run recomputed from now)", j.NextRun, now.Format(time.RFC3339))
	}
	// The re-enabled job fires again.
	rewriteCronJob(t, store, ctx, "cron_disabled", func(j *CronJob) {
		j.NextRun = now.Add(-time.Minute).Format(time.RFC3339)
	})
	runner := &cronFakeRunner{}
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue after enable: %v", err)
	}
	if runner.count() != 1 {
		t.Fatalf("runner calls = %d, want 1 (the re-enabled job fires)", runner.count())
	}
}

// TestCronEnableAlreadyEnabledIsNoop (F-C15): enabling a job that
// is not disabled succeeds without touching it.
func TestCronEnableAlreadyEnabledIsNoop(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	created := mustCronSchedule(t, sched, ctx, "0 9 * * *", "hi")
	enable := &cronEnableTool{store: store}
	out, err := enable.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"job_id":%q}`, created.Job.ID)))
	if err != nil {
		t.Fatalf("cron_enable of an enabled job: %v", err)
	}
	var enabled cronEnableOut
	if err := json.Unmarshal([]byte(out), &enabled); err != nil {
		t.Fatalf("cron_enable result is not JSON (%q): %v", out, err)
	}
	if !enabled.Enabled || enabled.WasDisabled {
		t.Fatalf("cron_enable result = %s, want {enabled:true, was_disabled:false}", out)
	}
	got := mustCronList(t, list, ctx)
	if got.Count != 1 || got.Jobs[0].NextRun != created.Job.NextRun {
		t.Fatalf("list after no-op enable = %+v, want the job untouched", got)
	}
}

// TestCronEnableUnknownAndQuarantined (F-C15): an unknown id
// errors like cancel does, and a quarantined job errors as
// quarantined instead of being silently "enabled".
func TestCronEnableUnknownAndQuarantined(t *testing.T) {
	dir := t.TempDir()
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	writeCronFile(t, dir, 111, []*CronJob{
		cronTestJob("cron_bad", "99 99 * * *", "bad expression", future),
	})
	store := NewPlanningStore(dir)
	ctx := cronChatCtx(111, 111)
	enable := &cronEnableTool{store: store}
	if _, err := enable.Execute(ctx, json.RawMessage(`{"job_id":"cron_nope"}`)); err == nil {
		t.Errorf("enable unknown job: got nil error, want unknown job")
	} else if !strings.Contains(err.Error(), "unknown job") {
		t.Errorf("enable unknown job error = %v, want unknown job", err)
	}
	if _, err := enable.Execute(ctx, json.RawMessage(`{"job_id":"cron_bad"}`)); err == nil {
		t.Errorf("enable quarantined job: got nil error, want a quarantined error")
	} else if !strings.Contains(err.Error(), "quarantined") {
		t.Errorf("enable quarantined job error = %v, want it to name the quarantine", err)
	}
}

// TestCronEnableOwnerAndContext (F-C15 + F-C17): re-enabling
// needs a chat in context and, in a group, the owner — the same
// rules as cancel, since it recommits the chat to recurring
// turns.
func TestCronEnableOwnerAndContext(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	past := now.Add(-time.Minute).Format(time.RFC3339)
	disabled := cronTestJob("cron_disabled", "0 9 * * *", "group brief", past)
	disabled.ChatID, disabled.Requester = -100, 111
	disabled.Disabled = true
	disabled.ConsecutiveFailures = 5
	writeCronFile(t, dir, -100, []*CronJob{disabled})
	store := NewPlanningStore(dir)
	enable := &cronEnableTool{store: store, ownerID: 111}
	memberCtx := cronChatCtx(-100, 222)
	if _, err := enable.Execute(memberCtx, json.RawMessage(`{"job_id":"cron_disabled"}`)); err == nil {
		t.Errorf("member enable in group: got nil error, want owner-only refusal")
	} else if !strings.Contains(err.Error(), "owner-only") {
		t.Errorf("member enable error = %v, want owner-only refusal", err)
	}
	// The refused enable left the job disabled.
	_, list, _ := cronTestTools(store)
	if got := mustCronList(t, list, cronChatCtx(-100, 111)); got.Count != 1 || !got.Jobs[0].Disabled {
		t.Fatalf("list after refused enable = %+v, want the job still disabled", got)
	}
	// No chat / no sender fail before the owner check.
	if _, err := enable.Execute(context.Background(), json.RawMessage(`{"job_id":"cron_disabled"}`)); err == nil {
		t.Errorf("no-chat enable: got nil error, want a Telegram-chat error")
	} else if !strings.Contains(err.Error(), "needs a Telegram chat") {
		t.Errorf("no-chat enable error = %v, want a Telegram-chat error", err)
	}
	noSender := permissions.WithApprovalContext(context.Background(), -100, 0)
	if _, err := enable.Execute(noSender, json.RawMessage(`{"job_id":"cron_disabled"}`)); err == nil {
		t.Errorf("no-sender enable: got nil error, want a sender error")
	} else if !strings.Contains(err.Error(), "needs a sender") {
		t.Errorf("no-sender enable error = %v, want a sender error", err)
	}
	// The owner can re-enable.
	if _, err := enable.Execute(cronChatCtx(-100, 111), json.RawMessage(`{"job_id":"cron_disabled"}`)); err != nil {
		t.Fatalf("owner enable in group: %v", err)
	}
}

// cronEnableOut is a cron_enable reply.
type cronEnableOut struct {
	Enabled     bool   `json:"enabled"`
	JobID       string `json:"job_id"`
	WasDisabled bool   `json:"was_disabled"`
	Job         struct {
		ID                  string `json:"id"`
		NextRun             string `json:"next_run_utc"`
		LastRun             string `json:"last_run_utc"`
		LastResult          string `json:"last_result"`
		ConsecutiveFailures int    `json:"consecutive_failures"`
		Disabled            bool   `json:"disabled"`
	} `json:"job"`
}

// TestCronMinGapWithinScansFullWindow (P2-D R3): the minimum-gap
// scan used to stop after 64 runs, which misses a sub-5-minute
// gap that only appears later in the 7-day window. This schedule
// fires every 5 minutes through most of the day, but 22:57 →
// 23:00 is a 3-minute gap — the 144th gap after a midnight start,
// past the old cap — so it must be rejected, not accepted.
//
// Counterexample, verified by counting: minutes
// 0,5,…,50,57 give 5-minute gaps inside an hour (50→57 is 7),
// hours 0,2,…,20,22 are non-consecutive, so no hour-boundary
// gap appears before 22:57→23:00; 11 even hours × 12 runs +
// hour 22's 12 runs = 144 runs (143 gaps) precede that 3-minute
// gap, far past 64. A cap that stops early reports a 5-minute
// minimum and accepts a schedule that fires 3 minutes apart.
func TestCronMinGapWithinScansFullWindow(t *testing.T) {
	sched, err := parseCronExpression("0,5,10,15,20,25,30,35,40,45,50,57 0,2,4,6,8,10,12,14,16,18,20,22,23 * * *")
	if err != nil {
		t.Fatalf("parseCronExpression: %v", err)
	}
	from := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	gap, ok := sched.minGapWithin(from)
	if !ok {
		t.Fatalf("minGapWithin: no gap measured, want one")
	}
	if gap != 3*time.Minute {
		t.Fatalf("minGapWithin = %v, want 3m (the 22:57 → 23:00 gap)", gap)
	}
	// End to end: the same schedule must be rejected at creation.
	store := NewPlanningStore("")
	tool, _, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	// Schedule from a fixed clock is not possible through
	// Execute (it uses time.Now), so the unit check above is
	// the deterministic proof; here the creation path must at
	// least agree whenever the wall clock is before the
	// evening gap. Only assert rejection when the gap is
	// reachable from now: it always is within 7 days, and the
	// scan covers the full window, so rejection is expected
	// at any wall-clock time.
	if _, err := tool.Execute(ctx, json.RawMessage(`{"expression":"0,5,10,15,20,25,30,35,40,45,50,57 0,2,4,6,8,10,12,14,16,18,20,22,23 * * *","prompt":"late gap"}`)); err == nil {
		t.Fatalf("schedule with a late 3-minute gap: got nil error, want minimum-interval rejection")
	} else if !strings.Contains(err.Error(), "minimum interval") {
		t.Fatalf("schedule error = %v, want minimum-interval rejection", err)
	}
}
