package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestCronContendedScheduleDoesNotBlockOtherChat proves F-C27:
// while one chat's cron_schedule is stuck waiting for its cron
// file lock (a contended flock retries for up to 10s), planning
// operations for an UNRELATED chat must still complete —
// cron_schedule/cron_cancel used to hold the store mutex (s.mu)
// across that wait, blocking every chat's planning operation.
//
// The test holds chat 111's cron flock itself (an external
// holder, e.g. another process mid-save), starts a schedule for
// 111 in the background, and then requires a list and a cancel
// for chat 222 to finish promptly. It then releases the flock
// and requires the blocked schedule to complete normally — the
// restructure must not have broken schedule-under-contention.
func TestCronContendedScheduleDoesNotBlockOtherChat(t *testing.T) {
	dataDir := t.TempDir()
	store := NewPlanningStore(dataDir)
	sched, list, cancel := cronTestTools(store)
	ctx111 := cronChatCtx(111, 111)
	ctx222 := cronChatCtx(222, 222)

	// A job in chat 222, so the cancel below has something to do.
	job222 := mustCronSchedule(t, sched, ctx222, "*/5 * * * *", "other chat job")

	// Hold chat 111's cron flock, as a stuck writer would.
	dir111 := filepath.Join(dataDir, "chats", "111")
	if err := os.MkdirAll(dir111, 0o700); err != nil {
		t.Fatalf("mkdir chat 111 dir: %v", err)
	}
	lockFile, err := os.OpenFile(filepath.Join(dir111, cronJobsFile+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open chat 111 lock file: %v", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lockFile.Close()
		t.Fatalf("hold chat 111 flock: %v", err)
	}

	// The contended schedule for chat 111, in the background.
	type schedResult struct {
		out string
		err error
	}
	schedCh := make(chan schedResult, 1)
	go func() {
		out, err := sched.Execute(ctx111, json.RawMessage(`{"expression":"*/5 * * * *","prompt":"contended job"}`))
		schedCh <- schedResult{out, err}
	}()

	// Give the schedule time to reach the flock wait (it cannot
	// complete while the flock is held, so "not done" below is
	// meaningful, not a scheduling artefact).
	time.Sleep(500 * time.Millisecond)
	select {
	case r := <-schedCh:
		lockFile.Close()
		t.Fatalf("contended schedule returned while the flock was held: out=%q err=%v", r.out, r.err)
	default:
	}

	// An unrelated chat's list must complete promptly, not wait
	// behind the contended schedule's store lock.
	type listResult struct {
		out string
		err error
	}
	listCh := make(chan listResult, 1)
	go func() {
		out, err := list.Execute(ctx222, json.RawMessage(`{}`))
		listCh <- listResult{out, err}
	}()
	select {
	case r := <-listCh:
		if r.err != nil {
			lockFile.Close()
			t.Fatalf("cron_list for chat 222 while chat 111's schedule is contended: %v", r.err)
		}
		var parsed cronListOut
		if err := json.Unmarshal([]byte(r.out), &parsed); err != nil {
			lockFile.Close()
			t.Fatalf("cron_list result is not JSON (%q): %v", r.out, err)
		}
		if parsed.Count != 1 {
			lockFile.Close()
			t.Fatalf("cron_list for chat 222 = %d jobs, want 1", parsed.Count)
		}
	case <-time.After(5 * time.Second):
		lockFile.Close()
		t.Fatal("cron_list for chat 222 blocked behind the contended cron_schedule for chat 111 (F-C27: s.mu held across the flock wait)")
	}

	// The same for cron_cancel: another chat's cancel must not
	// wait behind the contended schedule either.
	cancelCh := make(chan error, 1)
	go func() {
		_, err := cancel.Execute(ctx222, json.RawMessage(fmt.Sprintf(`{"job_id":%q}`, job222.Job.ID)))
		cancelCh <- err
	}()
	select {
	case err := <-cancelCh:
		if err != nil {
			lockFile.Close()
			t.Fatalf("cron_cancel for chat 222 while chat 111's schedule is contended: %v", err)
		}
	case <-time.After(5 * time.Second):
		lockFile.Close()
		t.Fatal("cron_cancel for chat 222 blocked behind the contended cron_schedule for chat 111 (F-C27: s.mu held across the flock wait)")
	}

	// The schedule is still stuck behind the flock we hold.
	select {
	case r := <-schedCh:
		lockFile.Close()
		t.Fatalf("contended schedule returned while the flock was still held: out=%q err=%v", r.out, r.err)
	default:
	}

	// Release the flock: the schedule must now complete normally.
	if err := lockFile.Close(); err != nil {
		t.Fatalf("release chat 111 flock: %v", err)
	}
	select {
	case r := <-schedCh:
		if r.err != nil {
			t.Fatalf("contended schedule after flock release: %v", r.err)
		}
		var parsed cronJobOut
		if err := json.Unmarshal([]byte(r.out), &parsed); err != nil {
			t.Fatalf("schedule result is not JSON (%q): %v", r.out, err)
		}
		if parsed.Job.ID == "" {
			t.Fatalf("schedule result has no job id: %q", r.out)
		}
		if got := mustCronList(t, list, ctx111); got.Count != 1 {
			t.Fatalf("cron_list for chat 111 after contended schedule = %d jobs, want 1", got.Count)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("contended schedule did not complete after the flock was released")
	}
}
