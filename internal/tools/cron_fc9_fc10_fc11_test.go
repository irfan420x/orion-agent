// Tests for the F-C9 (owner-only in groups), F-C10 (minimum
// interval + global cap) and F-C11 (dormant-job warning) cron
// fixes.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// cronScheduleNoteOut decodes a cron_schedule result including
// the note, which cronJobOut (in cron_test.go) does not carry.
type cronScheduleNoteOut struct {
	cronJobOut
	Note string `json:"note"`
}

func mustCronScheduleNote(t *testing.T, tool *cronScheduleTool, ctx context.Context, expr, prompt string) cronScheduleNoteOut {
	t.Helper()
	out, err := tool.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"expression":%q,"prompt":%q}`, expr, prompt)))
	if err != nil {
		t.Fatalf("cron_schedule(%q): %v", expr, err)
	}
	var parsed cronScheduleNoteOut
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("cron_schedule result is not JSON (%q): %v", out, err)
	}
	return parsed
}

// TestCronGroupOwnerOnly (F-C9): in a group chat only the bot
// owner may schedule or cancel; a private chat is unaffected,
// and with no owner configured a group refuses everyone.
func TestCronGroupOwnerOnly(t *testing.T) {
	const owner = int64(777)
	const group = int64(-100123)
	const member = int64(555)

	store := NewPlanningStore("")
	sched, list, cancel := cronTestTools(store)
	if err := sched.InstallTransport(&ToolHost{OwnerID: owner}); err != nil {
		t.Fatalf("schedule InstallTransport: %v", err)
	}
	if err := cancel.InstallTransport(&ToolHost{OwnerID: owner}); err != nil {
		t.Fatalf("cancel InstallTransport: %v", err)
	}
	memberCtx := cronChatCtx(group, member)
	ownerCtx := cronChatCtx(group, owner)

	// A non-owner group member cannot schedule, and the refusal
	// names the owner-only rule.
	if _, err := sched.Execute(memberCtx, json.RawMessage(`{"expression":"0 9 * * *","prompt":"member job"}`)); err == nil {
		t.Fatalf("member schedule in group: got nil error, want owner-only refusal")
	} else if !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("member schedule error = %v, want owner-only", err)
	}

	// The owner can schedule in the same group.
	created := mustCronSchedule(t, sched, ownerCtx, "0 9 * * *", "owner job")

	// Listing stays readable by the member (read-only).
	if got := mustCronList(t, list, memberCtx); got.Count != 1 || got.Jobs[0].ID != created.Job.ID {
		t.Fatalf("member list = %+v, want the owner's 1 job (list is read-only)", got)
	}

	// The member cannot cancel the owner's job — and the job
	// survives the attempt.
	if _, err := cancel.Execute(memberCtx, json.RawMessage(fmt.Sprintf(`{"job_id":%q}`, created.Job.ID))); err == nil {
		t.Fatalf("member cancel in group: got nil error, want owner-only refusal")
	} else if !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("member cancel error = %v, want owner-only", err)
	}
	if got := mustCronList(t, list, ownerCtx); got.Count != 1 {
		t.Fatalf("list after member cancel attempt = %d jobs, want 1 (job must survive)", got.Count)
	}

	// The owner can cancel it.
	if _, err := cancel.Execute(ownerCtx, json.RawMessage(fmt.Sprintf(`{"job_id":%q}`, created.Job.ID))); err != nil {
		t.Fatalf("owner cancel in group: %v", err)
	}

	// A private chat is unaffected: anyone allowlisted may
	// schedule there, even though the owner id is someone else.
	mustCronSchedule(t, sched, cronChatCtx(111, 111), "0 9 * * *", "private job")

	// No owner configured (no InstallTransport): a group
	// refuses everyone, fail closed — while a private chat
	// still works.
	store2 := NewPlanningStore("")
	sched2, _, _ := cronTestTools(store2)
	if _, err := sched2.Execute(memberCtx, json.RawMessage(`{"expression":"0 9 * * *","prompt":"no owner job"}`)); err == nil {
		t.Fatalf("no-owner group schedule: got nil error, want owner-only refusal")
	} else if !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("no-owner group schedule error = %v, want owner-only", err)
	}
	mustCronSchedule(t, sched2, cronChatCtx(222, 222), "0 9 * * *", "no-owner private job")
}

// TestCronMinimumInterval (F-C10): a schedule that fires more
// often than every 5 minutes is rejected at creation with the
// minimum named; a 5-minute (or slower) schedule is accepted.
func TestCronMinimumInterval(t *testing.T) {
	store := NewPlanningStore("")
	sched, _, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)

	reject := []string{
		"* * * * *",       // every minute
		"*/4 * * * *",     // every 4 minutes
		"0,2 * * * *",     // 2-minute gap inside the hour
		"59,0 23,0 * * *", // 1-minute overnight gap (23:59 -> 00:00)
	}
	for _, expr := range reject {
		if _, err := sched.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"expression":%q,"prompt":"too fast"}`, expr))); err == nil {
			t.Errorf("schedule %q: got nil error, want minimum-interval rejection", expr)
		} else if !strings.Contains(err.Error(), "minimum interval of 5 minutes") {
			t.Errorf("schedule %q error = %v, want the 5-minute minimum named", expr, err)
		}
	}

	accept := []string{
		"*/5 * * * *",  // exactly 5 minutes: the floor, not below it
		"0,30 * * * *", // every 30 minutes
		"0 9 * * *",    // daily
		"0 9 * * 1-5",  // weekdays
	}
	for _, expr := range accept {
		if _, err := sched.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"expression":%q,"prompt":"fine"}`, expr))); err != nil {
			t.Errorf("schedule %q: %v, want success", expr, err)
		}
	}
}

// TestCronGlobalJobCap (F-C10): the daemon-wide cap of 200 jobs
// is enforced on top of the per-chat cap of 20.
func TestCronGlobalJobCap(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)

	// Ten chats at the per-chat cap: 200 jobs total.
	for c := 0; c < 10; c++ {
		ctx := cronChatCtx(int64(1000+c), int64(1000+c))
		for i := 0; i < cronMaxJobsPerChat; i++ {
			mustCronSchedule(t, sched, ctx, "0 9 * * *", fmt.Sprintf("chat %d job %d", c, i))
		}
	}
	if got := mustCronList(t, list, cronChatCtx(1000, 1000)); got.Count != cronMaxJobsPerChat {
		t.Fatalf("chat 1000 list = %d jobs, want %d", got.Count, cronMaxJobsPerChat)
	}

	// The 201st job — in a fresh chat, under its per-chat cap —
	// is refused by the global cap.
	ctx := cronChatCtx(2000, 2000)
	if _, err := sched.Execute(ctx, json.RawMessage(`{"expression":"0 9 * * *","prompt":"one too many"}`)); err == nil {
		t.Fatalf("201st job: got nil error, want the global cap error")
	} else if !strings.Contains(err.Error(), "global job limit reached") || !strings.Contains(err.Error(), "200") {
		t.Fatalf("201st job error = %v, want the global cap error naming 200", err)
	}
}

// seedCronJobs writes n valid jobs straight into chatID's bucket
// (the test seam for filling a store without 200 tool calls).
func seedCronJobs(t *testing.T, store *PlanningStore, chatID int64, n int) {
	t.Helper()
	ctx := cronChatCtx(chatID, chatID)
	dir, key := store.resolveBucket(ctx)
	// F-C27: flock before mu, like every cron site — this helper
	// used to take mu first, inverting the order on the same
	// (s.mu, cron-flock) pair.
	release, err := store.lockState(ctx, dir, cronJobsFile)
	if err != nil {
		t.Fatalf("seedCronJobs lock: %v", err)
	}
	defer release()
	store.mu.Lock()
	defer store.mu.Unlock()
	doc, err := loadCronJobs(store, dir, key)
	if err != nil {
		t.Fatalf("seedCronJobs load: %v", err)
	}
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		doc.append(&CronJob{
			ID: fmt.Sprintf("cron_seed_%d_%d", chatID, i), ChatID: chatID, Requester: chatID,
			Expression: "0 9 * * *", Prompt: "seeded",
			CreatedAt: now.Format(time.RFC3339),
			NextRun:   now.Add(time.Hour).Format(time.RFC3339),
		})
	}
	if err := saveCronJobs(store, dir, key, doc); err != nil {
		t.Fatalf("seedCronJobs save: %v", err)
	}
}

// TestCronGlobalJobCapOnDisk (F-C10): the global count covers
// jobs persisted in <dataDir>/chats/<id>/cron.json, not just
// in-memory buckets — 199 seeded on disk, the 200th schedules,
// the 201st is refused.
func TestCronGlobalJobCapOnDisk(t *testing.T) {
	dir := t.TempDir()
	store := NewPlanningStore(dir)
	sched, _, _ := cronTestTools(store)

	for c := 0; c < 9; c++ {
		seedCronJobs(t, store, int64(1000+c), cronMaxJobsPerChat)
	}
	seedCronJobs(t, store, 1999, cronMaxJobsPerChat-1) // 9*20 + 19 = 199

	ctx := cronChatCtx(2000, 2000)
	mustCronSchedule(t, sched, ctx, "0 9 * * *", "the 200th job")
	if _, err := sched.Execute(ctx, json.RawMessage(`{"expression":"0 9 * * *","prompt":"the 201st job"}`)); err == nil {
		t.Fatalf("201st job on disk: got nil error, want the global cap error")
	} else if !strings.Contains(err.Error(), "global job limit reached") {
		t.Fatalf("201st job on disk error = %v, want the global cap error", err)
	}
}

// TestCronScheduleWarnsWithoutScheduler (F-C11): scheduling
// from a process with no scheduler warns that the job will not
// fire until oriond runs.
func TestCronScheduleWarnsWithoutScheduler(t *testing.T) {
	store := NewPlanningStore("") // no NewCronScheduler: no scheduler in this process
	sched, _, _ := cronTestTools(store)
	got := mustCronScheduleNote(t, sched, cronChatCtx(111, 111), "0 9 * * *", "dormant job")
	if !strings.Contains(got.Note, "no cron scheduler is running in this process") {
		t.Fatalf("schedule note = %q, want the no-scheduler warning", got.Note)
	}
	if !strings.Contains(got.Note, "oriond") {
		t.Fatalf("schedule note = %q, want the warning to name oriond", got.Note)
	}
}

// TestCronScheduleNoWarningWithScheduler (F-C11): the same
// schedule from a process WITH a scheduler carries no dormant
// warning.
func TestCronScheduleNoWarningWithScheduler(t *testing.T) {
	store := NewPlanningStore("")
	_ = NewCronScheduler(store) // this process runs a scheduler
	sched, _, _ := cronTestTools(store)
	got := mustCronScheduleNote(t, sched, cronChatCtx(111, 111), "0 9 * * *", "live job")
	if strings.Contains(got.Note, "no cron scheduler is running") {
		t.Fatalf("schedule note = %q, want no dormant warning when a scheduler runs", got.Note)
	}
}
