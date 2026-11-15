// Package tools — cron: scheduled jobs (cron_schedule / cron_list /
// cron_cancel) and the scheduler that fires them.
//
// A job is a prompt the agent runs as a turn, on a 5-field cron
// expression, in the chat that created it. The three tools are
// deliberately separate (matching enter_plan_mode / exit_plan_mode)
// instead of one mega-tool: each has one job, one schema, and one
// risk story, and the model cannot confuse "list" arguments with
// "schedule" arguments.
//
// Design:
//
//   - Persistence: jobs live in the shared PlanningStore, in
//     <DataDir>/chats/<chatID>/cron.json (0600, atomic write,
//     flocked load-modify-save, symlink refusal — the exact
//     todo/task/plans pattern), so a daemon restart reloads them
//     with no extra work. In-memory buckets (no data dir) keep a
//     per-bucket copy, like todos. There is no per-chat timezone
//     anywhere in the codebase, so all times are stored and shown
//     in UTC and the tools say so.
//   - Ownership: the chat comes from the turn context, exactly as
//     for plans — a job is created in the caller's bucket, and
//     list/cancel resolve the same bucket, so chat B physically
//     cannot see or cancel chat A's jobs (there is no cross-chat
//     lookup to refuse; the bucket is the boundary). Scheduling
//     requires a Telegram chat: a CLI run has no chat to deliver
//     a scheduled turn to, so cron_schedule fails cleanly there
//     instead of creating an undeliverable job.
//   - Approval: the MANAGEMENT tools are Safe — they only write
//     Orion's own job files. The scheduled TURN is a normal agent
//     turn on the chat's agent, through the same gate machinery
//     as an interactive turn (the daemon's runner sets the
//     approval context to the job's chat and requester, and the
//     turn runs on the chat's agent with the installed gate) —
//     but the turn is UNATTENDED (F-C3), so it is NOT approved
//     like an interactive turn: persisted "always allow" rules
//     are IGNORED in a scheduled turn, and a destructive or
//     network action inside it is DENIED PROMPTLY, without a
//     prompt card and without waiting the gate's approval
//     window — nobody is present to approve, so waiting would
//     only burn the tick. The denial is fed back to the model
//     as a tool result and is visible in the reply the scheduler
//     sends to the chat, and a turn in which EVERY tool call was
//     gate-denied counts toward the job's consecutive failures
//     (F-C8) instead of recording "ok" — a job that needs a
//     gated tool backs off and is disabled after 5 such runs,
//     instead of firing the same doomed turn forever. A turn
//     refused for the chat's daily token budget (F-C7) is a third
//     outcome: it records last_result "budget-exceeded", leaves
//     the failure count alone, and waits for the UTC-midnight
//     reset instead of the next cron slot — never "ok", never a
//     failure, never a minutely re-fire until midnight. Safe tools
//     still run freely in a scheduled turn, exactly as they do
//     interactively.
//   - Safety: a job that fails repeatedly backs off exponentially
//     (1, 2, 4, 8, 16 minutes) and is DISABLED after 5 consecutive
//     failures, so the backoff never grows past 16 minutes — never
//     a hot loop. A corrupt job never poisons the chat (F-C6):
//     each entry is validated on its own at load, and a bad one
//     is quarantined — skipped, listed in cron_list with its
//     reason, removable with cron_cancel — while the valid jobs
//     keep running. Only a file that is not valid JSON at all
//     still fails the whole file, since per-job quarantine needs
//     a parseable file.
//     A chat may hold at most 20 jobs. A disabled job never
//     fires; it stays listed until cancelled, so the owner can
//     see what stopped and re-create it.
//   - Missed runs: a run missed by more than 10 minutes (the
//     daemon was down at the scheduled time) is SKIPPED, not run
//     late — a "remind me at 9am" job firing at 6pm after a day
//     of downtime is worse than no run, and a run-late policy
//     would fire a backlog of stale turns on every restart. The
//     skip is recorded in last_result and does NOT count as a
//     failure (downtime is not the job failing).
//   - Scheduler: CronScheduler scans the store's chat buckets on
//     a tick (30s) and runs due jobs through an injected
//     CronRunner — the tools package cannot import core/app (it
//     would cycle), so the daemon installs the runner, exactly
//     like the TransportAware pattern. The runner runs the turn
//     OUTSIDE the store lock (a turn takes minutes; the lock is
//     held only for load and for the ID-scoped outcome update),
//     and turns for one chat are serialised against user turns
//     by the per-chat turn lock in app.runChatTurn, so a
//     scheduled turn can never overlap a user turn in the same
//     chat. Chats run in PARALLEL, bounded by a worker pool
//     (cronMaxWorkers, F-C2): one chat's slow turn never delays
//     another chat's scan or run, and a tick never double-fires
//     a job — each chat is claimed per tick (inFlight), so an
//     overlapping tick skips only the chats still running, never
//     the whole tick. Within one chat, jobs still run strictly
//     one after another. Every scheduled turn gets a deadline
//     (cronTurnTimeout, F-C2): a hung provider or stuck tool
//     ends the turn as a failure (backoff/disable accounting),
//     never holds a worker forever. A job is judged against the
//     missed window at the time it is actually processed, not
//     the tick start — a job that crossed the window while
//     queued behind a slow chat is skipped, not run late.
//     Claim-before-run (F-C4): a due job's NextRun is advanced
//     in the store BEFORE its turn runs, so a daemon crash or
//     restart mid-turn cannot re-fire the same slot — the slot
//     is consumed even if the turn never finishes, and an
//     outcome-save failure is retried once with the failure
//     counted instead of silently dropping the outcome and
//     re-firing. A turn the owner stops deliberately (/stop, or
//     cron_cancel on the running job — F-C5) records "stopped",
//     consumes the slot, and does NOT count toward the 5-strike
//     disable: a stop is not a failure.
package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
)

// cronJobsFile is the per-chat jobs file, next to todos.json,
// tasks.json and plans.json.
const cronJobsFile = "cron.json"

const (
	// cronMaxJobsPerChat bounds one chat's jobs. 20 recurring
	// turns is already a lot of unattended agent work; the cap
	// keeps one chat from scheduling a denial-of-service on its
	// own budget.
	cronMaxJobsPerChat = 20
	// cronMaxFailures is the consecutive-failure count at which
	// a job is disabled (never a hot loop).
	cronMaxFailures = 5
	// cronMaxBackoff caps the exponential failure backoff.
	cronMaxBackoff = 60 * time.Minute
	// cronMissedWindow is how late a run may be before it is
	// skipped instead of run (see the header: missed runs are
	// skipped, not run late).
	cronMissedWindow = 10 * time.Minute
	// cronMaxPromptRunes bounds one job's prompt.
	cronMaxPromptRunes = 2000
	// cronMaxJobIDRunes bounds a job id argument.
	cronMaxJobIDRunes = 64
	// cronDefaultTick is the scheduler's scan interval. Jobs
	// fire at minute granularity, so 30s keeps the worst firing
	// delay under a minute without busy-scanning.
	cronDefaultTick = 30 * time.Second
	// cronMaxWorkers bounds how many chats' scheduled turns run
	// at once (F-C2), across all concurrent ticks: the pool is
	// scheduler-level, not per tick, so overlapping ticks share
	// one bound. A tick with more due chats than this queues
	// the rest behind the pool — bounded, so a burst of due
	// jobs cannot spawn an unbounded pile of agent turns, and
	// one chat's long turn delays other chats only up to the
	// pool bound. 8 is far above the chats a daemon normally
	// serves at once, and far below any resource concern.
	cronMaxWorkers = 8
	// cronTurnTimeout bounds one scheduled turn (F-C2). A turn
	// that has not finished within this long ends as a failure
	// (backoff/disable accounting), so a hung provider or a
	// stuck tool cannot hold a worker — or a chat — forever.
	// 10 minutes is five times the agent's per-step timeout
	// (120s, internal/core/agent.go) and the same length as the
	// missed window: a healthy multi-step turn finishes in a
	// few minutes, while a turn that has run longer than the
	// missed window is already too late by the scheduler's own
	// definition. The gate's 90s approval window never applies
	// here — unattended turns deny gated tools promptly (F-C3)
	// instead of waiting for approval.
	cronTurnTimeout = 10 * time.Minute
	// cronSearchYears bounds the next-run search: a schedule
	// that cannot fire within 5 years (e.g. Feb 29 on a
	// non-leap cycle the search somehow misses) is an error,
	// not an unbounded loop.
	cronSearchYears = 5
)

// ---------------------------------------------------------------- expression

// cronFieldNames names the five fields, in order, for error
// messages that say WHICH field is bad.
var cronFieldNames = [5]string{"minute", "hour", "day-of-month", "month", "day-of-week"}

// cronFieldBounds are the valid ranges per field. Day-of-week
// accepts 0-7 (7 is Sunday, normalised to 0); the rest are the
// standard ranges.
var cronFieldBounds = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}

// cronSchedule is a parsed 5-field expression: one bitmask per
// field (bit n set = value n matches) plus whether the
// day-of-month / day-of-week fields were unrestricted ("*") —
// the OR semantics for those two fields need to know that.
type cronSchedule struct {
	minute, hour, dom, month, dow uint64
	domStar, dowStar              bool
}

// parseCronExpression parses a 5-field cron expression
// ("minute hour day-of-month month day-of-week"). Supported per
// field: "*" (any), lists ("1,2,5"), ranges ("1-5"), steps
// ("*/15", "1-30/5", "2/10"). Month/day names (JAN, MON) and
// @-shorthands (@daily) are NOT supported — they are rejected
// with an error naming the field, so a model that tries one
// learns the supported syntax instead of silently scheduling
// something else. Numbers must be in range; a step must be >= 1.
func parseCronExpression(expr string) (*cronSchedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron expression %q must have exactly 5 fields (minute hour day-of-month month day-of-week), got %d", expr, len(fields))
	}
	s := &cronSchedule{}
	for i, f := range fields {
		mask, star, err := parseCronField(f, cronFieldNames[i], cronFieldBounds[i][0], cronFieldBounds[i][1])
		if err != nil {
			return nil, err
		}
		switch i {
		case 0:
			s.minute = mask
		case 1:
			s.hour = mask
		case 2:
			s.dom, s.domStar = mask, star
		case 3:
			s.month = mask
		case 4:
			s.dow, s.dowStar = mask, star
		}
	}
	// Day-of-week 7 is Sunday: fold it onto bit 0 so matching
	// only ever tests 0-6.
	if s.dow&(1<<7) != 0 {
		s.dow |= 1 << 0
		s.dow &^= 1 << 7
	}
	return s, nil
}

// parseCronField parses one field into a bitmask. star reports
// whether the field was exactly "*" (unrestricted).
func parseCronField(field, name string, min, max int) (mask uint64, star bool, err error) {
	if field == "" {
		return 0, false, fmt.Errorf("cron expression: %s field is empty", name)
	}
	// A field is a comma-separated list of items; each item is
	// "base" or "base/step".
	for _, item := range strings.Split(field, ",") {
		if item == "" {
			return 0, false, fmt.Errorf("cron expression: %s field %q has an empty list item", name, field)
		}
		base, stepStr, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return 0, false, fmt.Errorf("cron expression: %s field %q has an invalid step %q (want a number >= 1)", name, field, stepStr)
			}
			step = n
		}
		lo, hi := min, max
		if base != "*" {
			if strings.Contains(base, "-") {
				a, b, found := strings.Cut(base, "-")
				if !found || a == "" || b == "" || strings.Contains(b, "-") {
					return 0, false, fmt.Errorf("cron expression: %s field %q has an invalid range %q (want \"low-high\")", name, field, base)
				}
				var err error
				lo, err = parseCronNumber(a, name, field, min, max)
				if err != nil {
					return 0, false, err
				}
				hi, err = parseCronNumber(b, name, field, min, max)
				if err != nil {
					return 0, false, err
				}
				if lo > hi {
					return 0, false, fmt.Errorf("cron expression: %s field %q has a reversed range %q (low must be <= high)", name, field, base)
				}
			} else {
				n, err := parseCronNumber(base, name, field, min, max)
				if err != nil {
					return 0, false, err
				}
				lo, hi = n, n
				// A bare number with a step ("2/10") means
				// n-max/step, as in standard cron: the step
				// runs from n up to the field max, so "2/10"
				// matches 2, 12, 22, ... — not just 2.
				if hasStep {
					hi = max
				}
			}
		} else if item == "*" && !hasStep && len(strings.Split(field, ",")) == 1 {
			star = true
		}
		for v := lo; v <= hi; v += step {
			mask |= 1 << uint(v)
		}
	}
	return mask, star, nil
}

// parseCronNumber parses one number in a field, rejecting
// non-numbers (JAN, MON, "@daily" fragments) and out-of-range
// values with an error naming the field.
func parseCronNumber(s, name, field string, min, max int) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("cron expression: %s field %q has a non-numeric value %q (month/day names and @-shorthands are not supported; use numbers)", name, field, s)
	}
	if n < min || n > max {
		return 0, fmt.Errorf("cron expression: %s field %q has value %d out of range %d-%d", name, field, n, min, max)
	}
	return n, nil
}

// matches reports whether t (in UTC) matches the schedule. The
// day-of-month / day-of-week rule is standard cron OR
// semantics: when both are restricted, the day matches when
// EITHER matches; when one is "*", the other decides; when both
// are "*", every day matches.
func (s *cronSchedule) matches(t time.Time) bool {
	if s.minute&(1<<uint(t.Minute())) == 0 {
		return false
	}
	if s.hour&(1<<uint(t.Hour())) == 0 {
		return false
	}
	if s.month&(1<<uint(t.Month())) == 0 {
		return false
	}
	domMatch := s.dom&(1<<uint(t.Day())) != 0
	dowMatch := s.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dowMatch
	case s.dowStar:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}

// nextAfter returns the next time strictly after `after` that
// matches the schedule, searching minute by minute (the schedule
// is minute-granular, so no finer search is needed). The search
// is bounded: a schedule that cannot fire within
// cronSearchYears is an error, not an unbounded loop. Feb 29 is
// the worst case at ~2.1M iterations, each a few bitmask tests.
func (s *cronSchedule) nextAfter(after time.Time) (time.Time, error) {
	t := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(cronSearchYears, 0, 0)
	for !t.After(limit) {
		if s.matches(t) {
			return t, nil
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("cron expression has no run in the next %d years", cronSearchYears)
}

// nextRunAfter parses expr and returns its next run strictly
// after `after`.
func nextRunAfter(expr string, after time.Time) (time.Time, error) {
	s, err := parseCronExpression(expr)
	if err != nil {
		return time.Time{}, err
	}
	return s.nextAfter(after)
}

// ---------------------------------------------------------------- job + store

// CronJob is one scheduled job. Times are RFC3339 UTC: the
// codebase has no per-chat timezone, so UTC is the stored and
// displayed timezone (see the header). LastResult is a short
// human-readable outcome of the last run/skip ("ok", "error: ...",
// "skipped: ...", "disabled after 5 consecutive failures: ...",
// "budget-exceeded: ..." — the chat's daily token budget was
// exhausted, see ErrBudgetExceeded).
type CronJob struct {
	ID                  string `json:"id"`
	ChatID              int64  `json:"chat_id"`
	Requester           int64  `json:"requester"`
	Expression          string `json:"expression"`
	Prompt              string `json:"prompt"`
	CreatedAt           string `json:"created_at"`
	NextRun             string `json:"next_run"`
	LastRun             string `json:"last_run,omitempty"`
	LastResult          string `json:"last_result,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	Disabled            bool   `json:"disabled"`
}

// nextRunTime parses the job's NextRun. A job that fails this
// never passed load validation; the scheduler treats it as
// corrupt and skips it rather than crashing.
func (j *CronJob) nextRunTime() (time.Time, error) {
	t, err := time.Parse(time.RFC3339, j.NextRun)
	if err != nil {
		return time.Time{}, fmt.Errorf("job %q has an unparsable next run %q: %w", j.ID, j.NextRun, err)
	}
	return t, nil
}

// loadCronJobs returns the chat's jobs in creation order. Call
// with s.mu held (the planning-store convention). A corrupt file
// is an error, never a silent reset: silently dropping a chat's
// scheduled jobs would hide data loss — and a dropped job that
// keeps firing from a stale in-memory copy would be worse.
//
// Corruption is per job, not per file (F-C6): each entry is
// validated on its own, and an entry that fails validation is
// QUARANTINED — skipped, never run, but kept in the file in its
// original position with its reason, so the valid jobs keep
// running and the owner can see the bad one in cron_list and
// remove it with cron_cancel instead of hand-editing cron.json
// on disk. The validation rules are unchanged (missing or
// duplicate id, invalid expression, empty prompt, unparsable
// next run, negative failure count); only the handling changed,
// from whole-file failure to per-job quarantine. The one case
// that still fails the whole file is a file that cannot be
// parsed at all (not valid JSON): per-job quarantine is
// impossible without a parseable file, so that stays a loud
// error, exactly as before.
func loadCronJobs(s *PlanningStore, dir, key string) (*cronJobsDoc, error) {
	var raw []*CronJob
	if dir == "" {
		b := s.bucket(key)
		raw = make([]*CronJob, len(b.cronJobs))
		for i, j := range b.cronJobs {
			if j == nil {
				continue
			}
			cp := *j
			raw[i] = &cp
		}
		return validateCronJobs(raw), nil
	}
	data, err := readStateFile(filepath.Join(dir, cronJobsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return &cronJobsDoc{}, nil
		}
		return nil, err
	}
	var doc struct {
		Jobs []*CronJob `json:"jobs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("corrupt cron jobs file %q: %w (fix or remove the file to recover)", filepath.Join(dir, cronJobsFile), err)
	}
	if doc.Jobs == nil {
		return &cronJobsDoc{}, nil
	}
	return validateCronJobs(doc.Jobs), nil
}

// cronJobEntry is one entry of a chat's cron file, in file
// order. reason is "" for a valid job; a non-empty reason means
// the job is quarantined (F-C6) — it never runs, but the entry
// stays in the file so a later save does not silently delete
// it, and so the owner can inspect and cancel it.
type cronJobEntry struct {
	job    *CronJob // as stored; nil for a null entry
	reason string   // "" = valid; otherwise why the job is quarantined
}

// cronJobsDoc is one chat's loaded cron file: every entry in
// file order, each either valid or quarantined with its reason.
// Callers work with validJobs() and save the whole doc, so a
// quarantined entry survives any load-modify-save round trip
// (schedule, cancel, outcome apply) until the owner cancels it.
type cronJobsDoc struct {
	entries []cronJobEntry
}

// validJobs returns the valid jobs in file order. The returned
// jobs share pointers with the doc's entries, so mutating one
// (an outcome apply) is visible when the doc is saved.
func (d *cronJobsDoc) validJobs() []*CronJob {
	out := make([]*CronJob, 0, len(d.entries))
	for _, e := range d.entries {
		if e.reason == "" {
			out = append(out, e.job)
		}
	}
	return out
}

// quarantined returns the quarantined entries in file order.
func (d *cronJobsDoc) quarantined() []cronQuarantinedJob {
	var out []cronQuarantinedJob
	for _, e := range d.entries {
		if e.reason != "" {
			out = append(out, cronQuarantinedJob{job: e.job, reason: e.reason})
		}
	}
	return out
}

// hasID reports whether any entry (valid or quarantined)
// carries id — a newly minted id must not collide with a
// quarantined one either, or the new job would load as a
// duplicate and be quarantined on the next read.
func (d *cronJobsDoc) hasID(id string) bool {
	for _, e := range d.entries {
		if e.job != nil && e.job.ID == id {
			return true
		}
	}
	return false
}

// append adds a valid job at the end of the file.
func (d *cronJobsDoc) append(job *CronJob) {
	d.entries = append(d.entries, cronJobEntry{job: job})
}

// removeFirstByID removes the first entry (in file order) whose
// id matches, valid or quarantined, and reports whether it
// found one and whether the removed entry was quarantined. A
// quarantined entry is therefore cancellable by id exactly like
// a valid one; when a valid job and a quarantined entry share an
// id, the first in file order goes first. Note a quarantined
// duplicate can promote: removing the earlier twin frees the id,
// so the later entry loads as valid on the next read (it now
// holds the id alone) — a second cancel then removes it as a
// valid job. An entry with no id cannot be addressed this way —
// it stays listed with its reason until the file is fixed by hand.
func (d *cronJobsDoc) removeFirstByID(id string) (wasQuarantined bool, found bool) {
	for i, e := range d.entries {
		if e.job == nil || e.job.ID != id {
			continue
		}
		wasQuarantined = e.reason != ""
		d.entries = append(d.entries[:i], d.entries[i+1:]...)
		return wasQuarantined, true
	}
	return false, false
}

// cronQuarantinedJob is one quarantined entry: the job as stored
// (nil for a null entry) and why it failed validation. It is
// never run and never trusted — only listed and cancellable.
type cronQuarantinedJob struct {
	job    *CronJob
	reason string
}

// id returns the entry's id, or "(no id)" when the entry has
// none (a missing-id entry cannot be cancelled by id).
func (q cronQuarantinedJob) id() string {
	if q.job == nil || q.job.ID == "" {
		return "(no id)"
	}
	return q.job.ID
}

// validateCronJobs applies the load validation rules to each
// entry independently (F-C6): missing or duplicate id, invalid
// expression, empty prompt, unparsable next run, negative
// failure count. A failing entry is quarantined with its reason
// instead of failing the whole file. The first entry carrying an
// id wins it — a later entry with the same id is a duplicate
// even when the first entry was itself quarantined, so ids stay
// unambiguous for cancel.
func validateCronJobs(raw []*CronJob) *cronJobsDoc {
	doc := &cronJobsDoc{entries: make([]cronJobEntry, 0, len(raw))}
	seen := make(map[string]bool, len(raw))
	for _, j := range raw {
		var reason string
		switch {
		case j == nil || j.ID == "":
			reason = "missing job id"
		case seen[j.ID]:
			reason = fmt.Sprintf("duplicate job id %q", j.ID)
		default:
			if _, err := parseCronExpression(j.Expression); err != nil {
				reason = fmt.Sprintf("invalid expression: %v", err)
			} else if strings.TrimSpace(j.Prompt) == "" {
				reason = "empty prompt"
			} else if _, err := j.nextRunTime(); err != nil {
				reason = err.Error()
			} else if j.ConsecutiveFailures < 0 {
				reason = "negative failure count"
			}
		}
		if j != nil && j.ID != "" {
			seen[j.ID] = true
		}
		doc.entries = append(doc.entries, cronJobEntry{job: j, reason: reason})
	}
	return doc
}

// saveCronJobs persists the doc's entries in file order — valid
// and quarantined alike, so a save never silently drops a
// quarantined job. Call with s.mu held; the caller takes the
// flock (lockState) around the load-modify-save, as task_create
// does. For a fully-valid file the written bytes are identical
// to before (same jobs, same order).
func saveCronJobs(s *PlanningStore, dir, key string, doc *cronJobsDoc) error {
	jobs := make([]*CronJob, len(doc.entries))
	for i, e := range doc.entries {
		jobs[i] = e.job
	}
	if dir == "" {
		b := s.bucket(key)
		cp := make([]*CronJob, len(jobs))
		for i, j := range jobs {
			if j == nil {
				continue
			}
			c := *j
			cp[i] = &c
		}
		b.cronJobs = cp
		return nil
	}
	if err := ensurePlanningDir(dir, s.dataDir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		Jobs []*CronJob `json:"jobs"`
	}{Jobs: jobs}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cron jobs: %w", err)
	}
	data = append(data, '\n')
	return atomicWriteFile(filepath.Join(dir, cronJobsFile), data, 0o600)
}

// newCronJobID mints a job id: "cron_" plus 16 hex chars of
// crypto randomness. The separator is "_" on purpose, not "-":
// like task ids, the id must not match the secret scanner's
// patterns as tool results pass through Registry.Execute (see
// newTaskID).
func newCronJobID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate cron job id: %w", err)
	}
	return "cron_" + hex.EncodeToString(b[:]), nil
}

// cronBucket is one chat's job bucket for the scheduler's scan:
// dir is the on-disk bucket ("" = in-memory), key the in-memory
// bucket name, chatID the chat the jobs run in. The scheduler
// fires with the BUCKET's chat id, never the job's stored
// ChatID — a hand-edited file with a wrong ChatID can therefore
// never redirect a run into another chat.
type cronBucket struct {
	dir    string
	key    string
	chatID int64
}

// cronBuckets lists every chat bucket that may hold jobs: the
// <dataDir>/chats/<id> directories when a data dir is set, the
// in-memory "chat:<id>" buckets otherwise. Sorted by chat id, so
// a tick's scan order is deterministic. A missing chats dir (no
// chat has ever persisted state) is an empty list, not an error.
func (s *PlanningStore) cronBuckets() []cronBucket {
	s.mu.Lock()
	dataDir := s.dataDir
	var memKeys []string
	for k := range s.mem {
		memKeys = append(memKeys, k)
	}
	s.mu.Unlock()
	if dataDir != "" {
		entries, err := os.ReadDir(filepath.Join(dataDir, "chats"))
		if err != nil {
			return nil
		}
		out := make([]cronBucket, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			id, err := strconv.ParseInt(e.Name(), 10, 64)
			if err != nil || id == 0 {
				continue
			}
			out = append(out, cronBucket{dir: filepath.Join(dataDir, "chats", e.Name()), chatID: id})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].chatID < out[j].chatID })
		return out
	}
	out := make([]cronBucket, 0, len(memKeys))
	for _, k := range memKeys {
		if !strings.HasPrefix(k, "chat:") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(k, "chat:"), 10, 64)
		if err != nil || id == 0 {
			continue
		}
		out = append(out, cronBucket{key: k, chatID: id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].chatID < out[j].chatID })
	return out
}

// cronJobSummary is the wire shape of one job (list/schedule
// replies). Times are UTC; the field documents that.
type cronJobSummary struct {
	ID                  string `json:"id"`
	Expression          string `json:"expression"`
	Prompt              string `json:"prompt"`
	NextRun             string `json:"next_run_utc"`
	LastRun             string `json:"last_run_utc,omitempty"`
	LastResult          string `json:"last_result,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	Disabled            bool   `json:"disabled"`
}

func summarizeCronJob(j *CronJob) cronJobSummary {
	return cronJobSummary{
		ID: j.ID, Expression: j.Expression, Prompt: j.Prompt,
		NextRun: j.NextRun, LastRun: j.LastRun, LastResult: j.LastResult,
		ConsecutiveFailures: j.ConsecutiveFailures, Disabled: j.Disabled,
	}
}

// cronQuarantinedSummary is the wire shape of one quarantined
// (corrupt) job in a cron_list reply: the job as stored, why it
// failed validation, and the explicit corrupt flag, so the owner
// can tell at a glance which jobs are not running and why, and
// remove one with cron_cancel.
type cronQuarantinedSummary struct {
	ID         string `json:"id"`
	Expression string `json:"expression,omitempty"`
	Prompt     string `json:"prompt,omitempty"`
	Reason     string `json:"reason"`
	Corrupt    bool   `json:"corrupt"`
}

func summarizeCronQuarantinedJob(q cronQuarantinedJob) cronQuarantinedSummary {
	s := cronQuarantinedSummary{Reason: q.reason, Corrupt: true}
	if q.job != nil {
		s.ID = q.job.ID
		s.Expression = q.job.Expression
		s.Prompt = q.job.Prompt
	}
	return s
}

func encodeCronJob(tool string, v any) (string, error) {
	out, err := json.Marshal(v)
	if err != nil {
		return "", &ToolError{Tool: tool, Kind: "execution", Err: fmt.Errorf("encoding result: %w", err)}
	}
	return string(out), nil
}

// ---------------------------------------------------------------- cron_schedule

type cronScheduleTool struct {
	store *PlanningStore
	// rules, when set (see SetCronScheduleRuleStore), is the
	// permission rule store the tool consults READ-ONLY at
	// schedule time to warn about persisted "always allow"
	// rules for gated tools (F-C3). A nil store means no
	// warning — the job is still created. The daemon wires the
	// live store in when it builds the permission gate; a
	// hand-built registry without a gate simply has none.
	rules *permissions.RuleStore
}

// setRuleStore installs the rule store consulted for the
// schedule-time warning. It is unexported: callers use
// SetCronScheduleRuleStore, which finds the tool on a registry.
func (t *cronScheduleTool) setRuleStore(r *permissions.RuleStore) {
	t.rules = r
}

// SetCronScheduleRuleStore installs store as the permission rule
// store the registry's cron_schedule tool consults (read-only)
// to warn at schedule time about persisted "always allow" rules
// for gated tools (F-C3). It is a no-op when the registry has no
// cron_schedule tool (e.g. a hand-built registry) or the store is
// nil. The daemon calls it from App.NewPermissionGate, so every
// entrypoint that builds a gate wires the same live store; the
// cron tools never write rules, they only read them for the
// warning.
func SetCronScheduleRuleStore(reg *Registry, store *permissions.RuleStore) {
	if reg == nil || store == nil {
		return
	}
	t, ok := reg.Get("cron_schedule")
	if !ok {
		return
	}
	if s, ok := t.(interface{ setRuleStore(*permissions.RuleStore) }); ok {
		s.setRuleStore(store)
	}
}

// unattendedAllowWarning lists the gated (destructive/network)
// tools for which chatID holds a persisted "always allow" rule:
// rules that would previously have auto-allowed an unattended
// execution, and that a scheduled turn now ignores (F-C3). It
// returns nil when the store is nil or no such rule exists. The
// caller warns only — it never refuses to schedule.
//
// The warning iterates the rule store's own entries for the chat,
// not KnownTools(): KnownTools lists only the explicitly classified
// built-ins, so it misses MCP tools (which classify by the mcp__
// prefix rule) and unclassified names — yet SetForChat accepts any
// non-empty name, and the gate denies those promptly when
// unattended. A legacy global rule (chat 0) applies to every chat,
// so it warns here too; another chat's rule never does. Deny rules
// never warn. Names are deduplicated (a tool can hold both a
// global and a per-chat allow rule) and sorted, so the warning is
// stable.
func (t *cronScheduleTool) unattendedAllowWarning(chatID int64) []string {
	if t.rules == nil {
		return nil
	}
	seen := map[string]bool{}
	var warned []string
	for _, rule := range t.rules.List() {
		if rule.Action != permissions.ActionAllow {
			continue
		}
		if rule.ChatID != chatID && rule.ChatID != 0 {
			continue
		}
		if permissions.Classify(rule.Tool) == permissions.ClassSafe {
			continue
		}
		if seen[rule.Tool] {
			continue
		}
		seen[rule.Tool] = true
		warned = append(warned, rule.Tool)
	}
	sort.Strings(warned)
	return warned
}

func (t *cronScheduleTool) Name() string { return "cron_schedule" }

func (t *cronScheduleTool) Description() string {
	return "Schedule a recurring agent turn: run a prompt on a cron expression (5 fields: minute hour day-of-month month day-of-week, UTC). The job runs in this chat until cancelled; the scheduled turn is unattended, so persisted \"always allow\" rules never apply to it and a destructive or network action inside it is denied promptly instead of running. Returns the job id and the next run (UTC). Times are UTC — Orion has no per-chat timezone. Telegram only: needs a chat in the run context."
}

func (t *cronScheduleTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"expression": map[string]any{"type": "string", "description": "5-field cron expression, e.g. \"0 9 * * 1-5\" (weekdays 09:00 UTC). Supports *, lists (1,2), ranges (1-5), steps (*/15, 1-30/5). Month/day names and @-shorthands are not supported."},
				"prompt":     map[string]any{"type": "string", "description": "The prompt the agent runs on each scheduled turn (max 2000 characters)."},
			},
			"required": []string{"expression", "prompt"},
		},
	}
}

// RiskClass declares the tool safe: it persists only Orion's own
// 0600 per-chat job file (cron.json, paths derived from config
// and context — never a user-supplied path), never user files,
// never the shell, never the network. The scheduled turn's own
// actions are gated separately by the turn machinery, not by
// this tool's class.
func (t *cronScheduleTool) RiskClass() permissions.Class { return permissions.ClassSafe }

type cronScheduleArgs struct {
	Expression string `json:"expression"`
	Prompt     string `json:"prompt"`
}

func (t *cronScheduleTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a cronScheduleArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	invalid := func(format string, v ...any) (string, error) {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf(format, v...)}
	}
	expr := strings.TrimSpace(a.Expression)
	if expr == "" {
		return invalid("expression must not be empty")
	}
	if _, err := parseCronExpression(expr); err != nil {
		return invalid("%v", err)
	}
	prompt := strings.TrimSpace(a.Prompt)
	if prompt == "" {
		return invalid("prompt must not be empty")
	}
	if utf8.RuneCountInString(prompt) > cronMaxPromptRunes {
		return invalid("prompt is too long (%d runes, max %d)", utf8.RuneCountInString(prompt), cronMaxPromptRunes)
	}
	chatID, senderID, ok := permissions.ApprovalContextFrom(ctx)
	if !ok || chatID == 0 {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: errors.New("cron_schedule needs a Telegram chat: this run has no chat in its context (a scheduled turn has nowhere to deliver its reply)")}
	}
	if senderID == 0 {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: errors.New("cron_schedule needs a sender: this run has no sender in its context")}
	}
	if t.store == nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: errors.New("cron_schedule: no planning store configured")}
	}
	now := time.Now().UTC()
	next, err := nextRunAfter(expr, now)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
	}
	dir, key := t.store.resolveBucket(ctx)
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	release, err := t.store.lockState(ctx, dir, cronJobsFile)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	defer release()
	doc, err := loadCronJobs(t.store, dir, key)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	valid := doc.validJobs()
	if len(valid) >= cronMaxJobsPerChat {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("job limit reached (%d jobs for this chat); cancel an old job with cron_cancel first", cronMaxJobsPerChat)}
	}
	var id string
	for attempts := 0; attempts < 10; attempts++ {
		cand, err := newCronJobID()
		if err != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
		}
		// The id must not collide with a quarantined entry
		// either: a new job sharing a quarantined id would
		// load as a duplicate and be quarantined itself.
		if !doc.hasID(cand) {
			id = cand
			break
		}
	}
	if id == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: errors.New("could not mint a unique cron job id")}
	}
	job := &CronJob{
		ID: id, ChatID: chatID, Requester: senderID,
		Expression: expr, Prompt: prompt,
		CreatedAt: now.Format(time.RFC3339),
		NextRun:   next.Format(time.RFC3339),
	}
	doc.append(job)
	if err := saveCronJobs(t.store, dir, key, doc); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	note := "Times are UTC (Orion has no per-chat timezone). The scheduled turn is unattended: persisted \"always allow\" rules never apply to it, and a destructive or network action inside it is denied promptly instead of running."
	if quarantined := doc.quarantined(); len(quarantined) > 0 {
		// Surface the corruption the schedule just worked
		// around: the new job is fine, but the chat still
		// holds quarantined jobs that never run.
		note += fmt.Sprintf(" Warning: this chat has %d quarantined (corrupt) job(s) that never run — see cron_list for the reason and remove one with cron_cancel.", len(quarantined))
	}
	if warned := t.unattendedAllowWarning(chatID); len(warned) > 0 {
		// Warn only, never refuse: the user scheduled the job
		// with these rules standing, and the rules used to
		// auto-allow unattended execution — they must know the
		// job will now be denied instead, before the 3am run
		// surprises them. The rules themselves are untouched:
		// they still apply to interactive turns, and the owner
		// can delete them with the /rules command.
		note += fmt.Sprintf(" Warning: this chat has persisted \"always allow\" rules for %s — those rules are ignored in scheduled turns, so if this job needs one of those tools it will be denied every run and the job will back off and be disabled after 5 consecutive failures.", strings.Join(warned, ", "))
	}
	return encodeCronJob(t.Name(), map[string]any{
		"job":      summarizeCronJob(job),
		"timezone": "UTC",
		"note":     note,
	})
}

// ---------------------------------------------------------------- cron_list

type cronListTool struct{ store *PlanningStore }

func (t *cronListTool) Name() string { return "cron_list" }

func (t *cronListTool) Description() string {
	return "List this chat's scheduled cron jobs (id, expression, prompt, next run, last outcome, failure count, disabled flag). Only this chat's jobs are ever listed. Jobs that failed validation are listed separately under \"quarantined\" as corrupt, with the reason they never run; remove one with cron_cancel. Times are UTC."
}

func (t *cronListTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

func (t *cronListTool) RiskClass() permissions.Class { return permissions.ClassSafe }

func (t *cronListTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if t.store == nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: errors.New("cron_list: no planning store configured")}
	}
	dir, key := t.store.resolveBucket(ctx)
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	doc, err := loadCronJobs(t.store, dir, key)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	jobs := doc.validJobs()
	summaries := make([]cronJobSummary, 0, len(jobs))
	for _, j := range jobs {
		summaries = append(summaries, summarizeCronJob(j))
	}
	quarantined := make([]cronQuarantinedSummary, 0, len(doc.entries))
	for _, q := range doc.quarantined() {
		quarantined = append(quarantined, summarizeCronQuarantinedJob(q))
	}
	return encodeCronJob(t.Name(), map[string]any{
		"jobs": summaries, "count": len(summaries), "quarantined": quarantined, "timezone": "UTC",
	})
}

// ---------------------------------------------------------------- cron_cancel

// cronCancelTool removes one job from the chat's cron.json. stopJob,
// when set (SetCronCancelStopJob — the daemon wires the scheduler's
// StopJob), also stops the job's live turn: it is called AFTER the
// job is removed, so a turn that ends after the cancel finds its job
// gone and its outcome is dropped instead of resurrecting it. The
// stop runs while the store lock is still held; that is safe because
// StopJob only takes the scheduler's running lock and cancels a
// context — it never takes the store lock, and the cancelled turn's
// outcome apply waits for this lock. A nil stopJob means cancel is
// future-runs-only (a hand-built registry with no scheduler, or a
// process that runs no scheduled turns).
type cronCancelTool struct {
	store   *PlanningStore
	stopJob func(jobID string) bool
}

// setStopJob installs the live-run stop hook. It is unexported:
// callers use SetCronCancelStopJob, which finds the tool on a registry.
func (t *cronCancelTool) setStopJob(fn func(jobID string) bool) {
	t.stopJob = fn
}

// SetCronCancelStopJob installs fn as the live-run stop the
// registry's cron_cancel tool calls after removing a job: fn is the
// scheduler's StopJob, reporting whether the job's turn was running
// and is now stopped. It is a no-op when the registry has no
// cron_cancel tool or fn is nil. The daemon calls it from run()
// next to the scheduler construction, so the tool and the scheduler
// it stops are the same live pair; a registry without it cancels
// future runs only.
func SetCronCancelStopJob(reg *Registry, fn func(jobID string) bool) {
	if reg == nil || fn == nil {
		return
	}
	t, ok := reg.Get("cron_cancel")
	if !ok {
		return
	}
	if c, ok := t.(interface{ setStopJob(func(string) bool) }); ok {
		c.setStopJob(fn)
	}
}

func (t *cronCancelTool) Name() string { return "cron_cancel" }

func (t *cronCancelTool) Description() string {
	return "Cancel one scheduled cron job by id (from cron_schedule or cron_list). The job stops firing; if its turn is running now, the turn is stopped too. Its past runs are not undone. Only this chat's jobs can be cancelled. A quarantined (corrupt) job listed by cron_list can be cancelled by id the same way, so a bad job can be cleaned up without editing cron.json on disk."
}

func (t *cronCancelTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"job_id": map[string]any{"type": "string", "description": "Job id, e.g. \"cron_...\" from cron_schedule."},
			},
			"required": []string{"job_id"},
		},
	}
}

func (t *cronCancelTool) RiskClass() permissions.Class { return permissions.ClassSafe }

type cronCancelArgs struct {
	JobID string `json:"job_id"`
}

func (t *cronCancelTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a cronCancelArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	id := strings.TrimSpace(a.JobID)
	if id == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("job_id must not be empty")}
	}
	if utf8.RuneCountInString(id) > cronMaxJobIDRunes {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("job_id is over %d characters", cronMaxJobIDRunes)}
	}
	if t.store == nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: errors.New("cron_cancel: no planning store configured")}
	}
	dir, key := t.store.resolveBucket(ctx)
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	release, err := t.store.lockState(ctx, dir, cronJobsFile)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	defer release()
	doc, err := loadCronJobs(t.store, dir, key)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	// First entry in file order with this id, valid or
	// quarantined: a quarantined job is cancellable by id
	// exactly like a valid one (F-C6).
	wasQuarantined, found := doc.removeFirstByID(id)
	if !found {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("unknown job %q: use cron_list to see this chat's jobs", id)}
	}
	if err := saveCronJobs(t.store, dir, key, doc); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	// The job is gone from the store first; only then stop its live
	// turn, if it has one. A stop here can only ever cancel this
	// job's turn: StopJob looks the job id up in the scheduler's
	// running map, so a user turn in the same chat (or any other
	// job's turn) is never cancelled by mistake — and a cancel
	// issued from inside the very turn it would stop is safe, the
	// turn simply ends as stopped.
	stopped := false
	if t.stopJob != nil {
		stopped = t.stopJob(id)
	}
	return encodeCronJob(t.Name(), map[string]any{"cancelled": true, "job_id": id, "stopped_running": stopped, "quarantined": wasQuarantined})
}

func init() {
	RegisterFactory("cron_schedule", func(deps ToolDeps) (Tool, error) {
		store := deps.PlanningStore
		if store == nil {
			store = NewPlanningStore("")
		}
		return &cronScheduleTool{store: store}, nil
	})
	RegisterFactory("cron_list", func(deps ToolDeps) (Tool, error) {
		store := deps.PlanningStore
		if store == nil {
			store = NewPlanningStore("")
		}
		return &cronListTool{store: store}, nil
	})
	RegisterFactory("cron_cancel", func(deps ToolDeps) (Tool, error) {
		store := deps.PlanningStore
		if store == nil {
			store = NewPlanningStore("")
		}
		return &cronCancelTool{store: store}, nil
	})
}

// ---------------------------------------------------------------- scheduler

// ErrCronTurnStopped marks a scheduled turn the owner stopped
// deliberately — /stop during the turn, or cron_cancel on the
// running job. The runner returns it (runScheduledTurn translates
// a cancelled turn context into it) and the scheduler records the
// run as stopped: the slot is consumed, but the stop is not a
// failure and does not count toward the job's 5-strike disable.
var ErrCronTurnStopped = errors.New("cron: scheduled turn stopped by the owner")

// cronRunHandle is one job's live turn: the chat it runs in and
// the cancel function for the scheduler's per-turn context.
type cronRunHandle struct {
	chatID int64
	cancel context.CancelFunc
}

// ErrBudgetExceeded is the sentinel a CronRunner returns when the
// turn was refused because the chat's daily token budget is
// exhausted (F-C7). The scheduler recognises it with errors.Is
// and records a distinct budget-exceeded outcome (see runJob) —
// it is NOT a generic turn failure, and it must not be confused
// with the all-denied error (F-C8): a budget refusal can carry
// stats with denied tool calls (a mid-turn abort), and it still
// counts as budget-exceeded, never as all-denied. The runner
// wraps it around the underlying budget error, e.g.
// fmt.Errorf("%w: %v", tools.ErrBudgetExceeded, err).
var ErrBudgetExceeded = errors.New("cron: daily token budget exceeded")

// CronBudgetExceededResult is the last_result prefix the
// scheduler records for a budget-exceeded run (F-C7). The daemon
// reads it back to decide whether the chat was already told
// about this job's budget exhaustion today, so the spelling is
// part of the runner/scheduler contract — change it in both
// places at once.
const CronBudgetExceededResult = "budget-exceeded"

// CronRunner runs one scheduled turn for job (a copy of the
// stored job): it must run a normal agent turn for job.ChatID
// with job.Prompt, marked UNATTENDED (F-C3) — the gate ignores
// persisted "always allow" rules in that turn and denies gated
// tools promptly instead of prompting — and deliver the reply
// to that chat itself. It returns the reply and a nil error when
// the turn ran (even when a gated tool call inside it was denied
// — a denial is a turn outcome, not a runner failure), EXCEPT
// when EVERY tool call in the turn was gate-denied: that turn
// accomplished nothing, so the runner returns an error (after
// delivering the reply) and the scheduler counts it toward the
// job's consecutive failures (F-C8). A turn refused for the
// chat's daily token budget is a third case: the runner sends
// the budget notice itself (at most once per job per day —
// whether one already went out today is read back from the
// job's stored last_result) and returns an error wrapping
// ErrBudgetExceeded, so the scheduler records a distinct
// budget-exceeded outcome instead of "ok" (F-C7). A non-nil
// error otherwise means the turn itself failed, and counts
// toward the job's consecutive failures. The runner runs OUTSIDE
// the store lock, so it may take as long as a turn takes.
type CronRunner func(ctx context.Context, job CronJob) (string, error)

// CronScheduler fires due cron jobs. It scans the store's chat
// buckets on a tick and runs each due job through its runner.
// Construct it with NewCronScheduler, install the runner with
// SetRunner, then run it with Run(ctx) in the daemon.
type CronScheduler struct {
	store *PlanningStore

	mu     sync.Mutex // guards runner, tick, log, turnTimeout, maxWorkers, sem
	runner CronRunner
	tick   time.Duration
	log    *log.Logger
	// turnTimeout is the per-turn deadline; 0 = cronTurnTimeout.
	turnTimeout time.Duration
	// maxWorkers is the worker-pool bound; 0 = cronMaxWorkers.
	// It is read when the pool is first created (first tick);
	// changing it after ticks have started does not resize
	// the live pool.
	maxWorkers int
	// sem is the scheduler-level worker pool (F-C2), shared by
	// every ProcessDue call: at most cap(sem) chats run turns
	// at once, even across overlapping ticks. Created lazily
	// by workerPool, under mu, at the then-current bound.
	sem chan struct{}

	claimMu sync.Mutex // guards inFlight
	// inFlight is the per-chat claim (F-C2): chats whose tick
	// work is still in progress. An overlapping tick skips a
	// claimed chat instead of double-firing its jobs — but only
	// that chat, never the whole tick. The claim is held from
	// the scan until that chat's runs and outcome apply finish.
	inFlight map[int64]struct{}

	// saveJobs persists jobs for the scheduler's claim and
	// outcome writes; nil means saveCronJobs. Tests replace it
	// to simulate a persist failure (a failing store) — the
	// tools keep calling saveCronJobs directly, so production
	// behaviour is unchanged.
	saveJobs func(s *PlanningStore, dir, key string, doc *cronJobsDoc) error

	runningMu sync.Mutex // guards running, stopped
	// running is the per-job live-turn table (F-C5): job ids
	// whose turn is currently inside runJob, with the cancel
	// for the scheduler's per-turn context. StopJob cancels
	// exactly that job's turn — never a user turn in the same
	// chat, which runs under a different context.
	running map[string]cronRunHandle
	// stopped marks jobs whose turn was deliberately stopped
	// (StopJob) while it was running, so runJob can tell a
	// deliberate stop from a failure even when the runner
	// reports it only as a context cancellation. Entries are
	// removed when the run unregisters.
	stopped map[string]struct{}
}

// NewCronScheduler builds a scheduler over store. A nil store
// fails ProcessDue loudly instead of silently firing nothing.
func NewCronScheduler(store *PlanningStore) *CronScheduler {
	return &CronScheduler{
		store: store,
		tick:  cronDefaultTick,
		log:   log.New(os.Stderr, "orion/cron: ", log.LstdFlags),
	}
}

// SetRunner installs the turn runner. A nil runner means
// ProcessDue refuses to fire (loudly, every tick) instead of
// silently dropping due jobs.
func (s *CronScheduler) SetRunner(r CronRunner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runner = r
}

// SetTickInterval overrides the scan interval. A non-positive
// value restores the default.
func (s *CronScheduler) SetTickInterval(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d <= 0 {
		d = cronDefaultTick
	}
	s.tick = d
}

// SetTurnTimeout overrides the per-turn deadline — how long one
// scheduled turn may run before the scheduler ends it as a
// failure (F-C2). A non-positive value restores the default
// (cronTurnTimeout).
func (s *CronScheduler) SetTurnTimeout(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d <= 0 {
		d = cronTurnTimeout
	}
	s.turnTimeout = d
}

// SetMaxWorkers overrides how many chats' turns run at once
// (F-C2), across all concurrent ticks. A non-positive value
// restores the default (cronMaxWorkers). The bound is read
// when the worker pool is first created on the first tick;
// changing it after ticks have started does not resize the
// live pool.
func (s *CronScheduler) SetMaxWorkers(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 {
		n = cronMaxWorkers
	}
	s.maxWorkers = n
}

func (s *CronScheduler) turnTimeoutDuration() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnTimeout <= 0 {
		return cronTurnTimeout
	}
	return s.turnTimeout
}

// workerPool returns the scheduler-level worker semaphore,
// created once (lazily, at the then-current maxWorkers bound)
// and shared by every ProcessDue call, so the pool bound holds
// across overlapping ticks, not per tick. A per-call pool
// would let two concurrent ticks run 2×maxWorkers turns.
func (s *CronScheduler) workerPool() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sem == nil {
		n := s.maxWorkers
		if n <= 0 {
			n = cronMaxWorkers
		}
		s.sem = make(chan struct{}, n)
	}
	return s.sem
}

// SetLogger replaces the scheduler's diagnostic logger. A nil
// logger is ignored.
func (s *CronScheduler) SetLogger(l *log.Logger) {
	if l == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = l
}

func (s *CronScheduler) logf(format string, args ...any) {
	s.mu.Lock()
	l := s.log
	s.mu.Unlock()
	if l != nil {
		l.Printf(format, args...)
	}
}

func (s *CronScheduler) tickInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tick <= 0 {
		return cronDefaultTick
	}
	return s.tick
}

func (s *CronScheduler) currentRunner() CronRunner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runner
}

// persistJobs is the scheduler's job save: saveCronJobs unless a
// test replaced saveJobs to simulate a persist failure. The whole
// doc is saved, so quarantined entries survive the round trip
// instead of being silently dropped (F-C6).
func (s *CronScheduler) persistJobs(dir, key string, doc *cronJobsDoc) error {
	save := s.saveJobs
	if save == nil {
		save = saveCronJobs
	}
	return save(s.store, dir, key, doc)
}

// registerRun records jobID's turn as live, with the cancel for
// its per-turn context. It is called by runJob before the runner
// is invoked and undone by unregisterRun when the run returns.
func (s *CronScheduler) registerRun(jobID string, chatID int64, cancel context.CancelFunc) {
	s.runningMu.Lock()
	defer s.runningMu.Unlock()
	if s.running == nil {
		s.running = map[string]cronRunHandle{}
	}
	s.running[jobID] = cronRunHandle{chatID: chatID, cancel: cancel}
}

// unregisterRun drops jobID's live-turn record (and any stopped
// mark for it) when its run returns.
func (s *CronScheduler) unregisterRun(jobID string) {
	s.runningMu.Lock()
	defer s.runningMu.Unlock()
	delete(s.running, jobID)
	delete(s.stopped, jobID)
}

// wasStopped reports whether jobID's live turn was deliberately
// stopped (StopJob) while it was running.
func (s *CronScheduler) wasStopped(jobID string) bool {
	s.runningMu.Lock()
	defer s.runningMu.Unlock()
	_, ok := s.stopped[jobID]
	return ok
}

// StopJob cancels the live turn for jobID, if it is running
// (F-C5 — wired to cron_cancel by SetCronCancelStopJob). It
// reports whether a run was running: false when the job is not
// running (already finished, never started, or unknown), so the
// caller knows the cancel was future-runs-only. The cancellation
// is the scheduler's own per-turn context, so it can never cancel
// a user turn in the same chat: a user turn runs under the daemon
// context, not this turn's context — and a runner queued behind
// the per-chat turn lock still ends promptly once the lock frees,
// because its context is already cancelled. The mark is taken
// under the same lock as the register/unregister, and the cancel
// is invoked while holding it, so a run cannot slip from "live"
// to "finished and unregistered" between the check and the
// cancel: context cancellation only closes channels and never
// blocks, so holding the lock across it cannot deadlock.
func (s *CronScheduler) StopJob(jobID string) bool {
	s.runningMu.Lock()
	defer s.runningMu.Unlock()
	h, ok := s.running[jobID]
	if !ok || h.cancel == nil {
		return false
	}
	if s.stopped == nil {
		s.stopped = map[string]struct{}{}
	}
	s.stopped[jobID] = struct{}{}
	h.cancel()
	return true
}

// Run scans once immediately (so a restart fires or skips
// promptly instead of waiting a full tick) and then on every
// tick until ctx is done. A tick error is logged, never fatal:
// one bad chat must not stop the scheduler for every other chat.
func (s *CronScheduler) Run(ctx context.Context) {
	if err := s.ProcessDue(ctx, time.Now().UTC()); err != nil {
		s.logf("initial scan: %v", err)
	}
	ticker := time.NewTicker(s.tickInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			if err := s.ProcessDue(ctx, t.UTC()); err != nil {
				s.logf("tick: %v", err)
			}
		}
	}
}

// cronOutcome is the state update for one job after a tick
// decided it. It is applied ID-scoped against a fresh load, so
// a job cancelled (or re-created) while its turn was running is
// never resurrected or clobbered by a stale outcome.
type cronOutcome struct {
	jobID    string
	nextRun  time.Time
	lastRun  time.Time // zero = not run this tick
	lastRes  string
	failures int
	disabled bool
}

// ProcessDue fires every job due at `now` (UTC) across all
// chats, and skips missed runs per the header policy. It is safe
// for concurrent use. Chats run in parallel, bounded by the
// worker pool (F-C2): one chat's slow turn never delays another
// chat's scan or run, and ProcessDue returns only once every
// dispatched chat's work is done. A chat whose previous tick's
// work is still in flight is skipped by its per-chat claim,
// never double-fired — but only that chat is skipped, never the
// whole tick. Per-chat failures are logged and do not stop other
// chats; a nil store or a missing runner is a returned error (a
// wiring fault the daemon must hear about) and leaves every job
// untouched.
func (s *CronScheduler) ProcessDue(ctx context.Context, now time.Time) error {
	if s.store == nil {
		return errors.New("cron: no planning store configured")
	}
	runner := s.currentRunner()
	if runner == nil {
		return errors.New("cron: no turn runner configured; scheduled jobs will not run")
	}
	now = now.UTC()
	buckets := s.store.cronBuckets()
	if len(buckets) == 0 {
		return nil
	}
	// wallStart anchors the missed-run check: a job is judged
	// against the time it is actually processed (tick time plus
	// the real delay it waited), not the tick start — see
	// processChat.
	wallStart := time.Now()
	// The pool is scheduler-level (see workerPool): one
	// goroutine per chat bucket, but only cap(sem) of them may
	// run turns at once, even across overlapping ticks.
	sem := s.workerPool()
	var wg sync.WaitGroup
	// dispatchErr is a ctx error seen while dispatching: the
	// loop stops dispatching further chats, but ProcessDue
	// still waits for every dispatched chat to finish before
	// returning, so its "returns only once every dispatched
	// chat's work is done" contract holds on the cancel path
	// too.
	var dispatchErr error
	for _, b := range buckets {
		if err := ctx.Err(); err != nil {
			dispatchErr = err
			break
		}
		if !s.tryClaim(b.chatID) {
			s.logf("chat %d: tick skipped: previous tick still running", b.chatID)
			continue
		}
		wg.Add(1)
		go func(b cronBucket) {
			defer wg.Done()
			defer s.releaseClaim(b.chatID)
			if err := s.processChat(ctx, b, now, wallStart, sem, runner); err != nil {
				s.logf("chat %d: %v", b.chatID, err)
			}
		}(b)
	}
	wg.Wait()
	return dispatchErr
}

// tryClaim marks chatID as having a tick's work in progress. It
// returns false when another tick already holds the chat — the
// caller skips the chat instead of double-firing its jobs. The
// claim is per chat, never global: a slow chat blocks only its
// own next tick, never any other chat's.
func (s *CronScheduler) tryClaim(chatID int64) bool {
	s.claimMu.Lock()
	defer s.claimMu.Unlock()
	if s.inFlight == nil {
		s.inFlight = map[int64]struct{}{}
	}
	if _, ok := s.inFlight[chatID]; ok {
		return false
	}
	s.inFlight[chatID] = struct{}{}
	return true
}

// releaseClaim drops the per-chat claim taken by tryClaim. It is
// called after the chat's runs and outcome apply finish, so a
// later tick always sees the applied outcome, never a stale
// next run.
func (s *CronScheduler) releaseClaim(chatID int64) {
	s.claimMu.Lock()
	defer s.claimMu.Unlock()
	delete(s.inFlight, chatID)
}

// processChat loads one chat's jobs, decides each due job, runs
// the due ones (outside the store lock, under the tick's worker
// semaphore), and applies the outcomes ID-scoped against a fresh
// load. Jobs in one chat run strictly one after another, so a
// scheduled turn in a chat never overlaps another scheduled turn
// (or, via app.runChatTurn's per-chat lock, a user turn) in that
// same chat.
func (s *CronScheduler) processChat(ctx context.Context, b cronBucket, now time.Time, wallStart time.Time, sem chan struct{}, runner CronRunner) error {
	s.store.mu.Lock()
	doc, err := loadCronJobs(s.store, b.dir, b.key)
	s.store.mu.Unlock()
	if err != nil {
		return fmt.Errorf("load jobs: %w", err)
	}
	jobs := doc.validJobs()
	// Quarantined entries are skipped, never run — but logged,
	// so a bad job is visible in the daemon log as well as in
	// cron_list, and the tick still runs the valid jobs. (The
	// line repeats once per tick, like the skip logs in
	// classifyCronJob below; the tick itself no longer fails.)
	for _, q := range doc.quarantined() {
		s.logf("chat %d: quarantined corrupt job %q: %s (valid jobs still run; remove it with cron_cancel)", b.chatID, q.id(), q.reason)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Fast filter at processing time: does this chat have
	// anything due? Chats with nothing due never take a worker
	// slot. The authoritative decision is re-made per job
	// below, at the time it is actually processed — this filter
	// only decides whether the chat needs a slot at all. The
	// filter is judged at the same processing time (`at`, tick
	// time plus the load delay) as the no-worker branch below,
	// not at tick time: a job that crossed the missed window
	// during the load is skipped, not silently left for the
	// next tick.
	at := now.Add(time.Since(wallStart))
	needsWorker := false
	for _, j := range jobs {
		if cronDueAt(j, at) {
			needsWorker = true
			break
		}
	}
	if !needsWorker {
		// Nothing can run this tick, but a job already past
		// the missed window still needs its skip outcome
		// recorded, judged at the same processing time `at`
		// (this chat never waited for a worker, so the load
		// delay is its whole wait).
		var outcomes []cronOutcome
		for _, j := range jobs {
			if kind, _, oc := s.classifyCronJob(j, at, b.chatID); kind == cronJobMissed {
				outcomes = append(outcomes, *oc)
			}
		}
		if len(outcomes) == 0 {
			return nil
		}
		// A failed skip-outcome save is retried the same way as
		// a run outcome (the retry leaves a missed skip
		// unchanged — it is not a run, so no failure is
		// counted for it).
		if err := s.applyOutcomes(ctx, b, outcomes); err != nil {
			return s.retryOutcomesAsFailures(ctx, b, outcomes, err)
		}
		return nil
	}
	// A chat with work to run takes a worker slot: at most
	// cap(sem) chats run turns at once, so one chat's
	// long turn delays other chats only up to the pool bound,
	// never indefinitely. A cancelled tick stops waiting for a
	// slot instead of starting work during shutdown.
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	// Authoritative decision, per job, at the time it is
	// actually processed: a job that waited behind the pool —
	// or behind an earlier turn in this same chat — is judged
	// against the time it runs, not the tick start. A job that
	// crossed the missed window while queued is skipped, not
	// run late; a job that became due while queued is run.
	var outcomes []cronOutcome
	var claimErrs []error
	for _, j := range jobs {
		if err := ctx.Err(); err != nil {
			// The tick is ending (shutdown): turns that
			// already ran still deserve their outcomes. The
			// claim already advanced their NextRun, so the
			// slot cannot re-fire — but without this
			// best-effort apply their last-run/failure
			// accounting would be lost with the shutdown.
			// WithoutCancel bounds it to one flocked save:
			// the lock still times out after 10s, so a
			// stuck holder cannot hang shutdown on it.
			if len(outcomes) > 0 {
				if aerr := s.applyOutcomes(context.WithoutCancel(ctx), b, outcomes); aerr != nil {
					s.logf("chat %d: outcome save failed on shutdown: %v", b.chatID, aerr)
				}
			}
			return err
		}
		at = now.Add(time.Since(wallStart))
		kind, nextAfter, oc := s.classifyCronJob(j, at, b.chatID)
		switch kind {
		case cronJobMissed:
			outcomes = append(outcomes, *oc)
		case cronJobDue:
			// Claim-before-run (F-C4): persist the advanced
			// NextRun BEFORE the turn runs. A daemon crash
			// mid-turn then finds the slot already consumed
			// on restart instead of re-firing it. A claim
			// that cannot be persisted is a loud skip, never
			// a run: running without the claim would risk
			// the duplicate the claim exists to prevent.
			// A job cancelled between the tick's load and
			// the claim is simply gone — skipping it is
			// correct, not an error.
			if err := s.claimJob(ctx, b, j, nextAfter); err != nil {
				if errors.Is(err, errCronJobGone) {
					continue
				}
				s.logf("chat %d: job %q not run: claim failed: %v", b.chatID, j.ID, err)
				claimErrs = append(claimErrs, err)
				continue
			}
			outcomes = append(outcomes, *s.runJob(ctx, j, at, nextAfter, runner, b.chatID))
		}
	}
	var applyErr error
	if len(outcomes) > 0 {
		if err := s.applyOutcomes(ctx, b, outcomes); err != nil {
			applyErr = s.retryOutcomesAsFailures(ctx, b, outcomes, err)
		}
	}
	return errors.Join(append(claimErrs, applyErr)...)
}

// cronDueAt is the cheap, quiet "is this job due at `at`"
// check: not disabled, next run parses, next run is not in the
// future, and it is not already past the missed window. It never
// logs: the authoritative classifyCronJob below is the one that
// reports a corrupt job, so a bad job is logged once per tick,
// not once per check.
func cronDueAt(j *CronJob, at time.Time) bool {
	if j.Disabled {
		return false
	}
	next, err := j.nextRunTime()
	if err != nil {
		return false
	}
	if next.After(at) {
		return false
	}
	return at.Sub(next) <= cronMissedWindow
}

// cronJobKind is what classifyCronJob decided for one job at one
// moment. A missed job carries its skip outcome; a due job
// carries no outcome yet — its turn still has to run.
type cronJobKind int

const (
	cronJobNotDue cronJobKind = iota
	cronJobMissed
	cronJobDue
)

// classifyCronJob decides one job at `at` without running it.
// It returns the decision, the next cron occurrence after `at`
// (for missed and due jobs), and — for a missed job — the skip
// outcome. The decision is pure: the caller supplies the
// decision time, so a job queued behind a slow chat is judged
// at its real processing time (see processChat), and a missed
// run is skipped without counting a failure, exactly as before.
func (s *CronScheduler) classifyCronJob(j *CronJob, at time.Time, chatID int64) (cronJobKind, time.Time, *cronOutcome) {
	if j.Disabled {
		return cronJobNotDue, time.Time{}, nil
	}
	next, err := j.nextRunTime()
	if err != nil {
		s.logf("chat %d: skipping job %q: %v", chatID, j.ID, err)
		return cronJobNotDue, time.Time{}, nil
	}
	if next.After(at) {
		return cronJobNotDue, time.Time{}, nil
	}
	sched, err := parseCronExpression(j.Expression)
	if err != nil {
		// Unreachable for a loaded job (load validation
		// quarantines invalid jobs), but a job must never
		// crash the tick.
		s.logf("chat %d: skipping job %q: invalid expression: %v", chatID, j.ID, err)
		return cronJobNotDue, time.Time{}, nil
	}
	nextAfter, err := sched.nextAfter(at)
	if err != nil {
		s.logf("chat %d: skipping job %q: %v", chatID, j.ID, err)
		return cronJobNotDue, time.Time{}, nil
	}
	if at.Sub(next) > cronMissedWindow {
		// Missed run: skip, do not run late, do not count a
		// failure (downtime is not the job failing).
		return cronJobMissed, nextAfter, &cronOutcome{
			jobID:    j.ID,
			nextRun:  nextAfter,
			lastRes:  fmt.Sprintf("skipped: scheduled run at %s was missed by %s (daemon was down or busy); next run %s (UTC)", next.Format(time.RFC3339), at.Sub(next).Round(time.Second), nextAfter.Format(time.RFC3339)),
			failures: j.ConsecutiveFailures,
			disabled: false,
		}
	}
	return cronJobDue, nextAfter, nil
}

// errCronJobGone marks a claim for a job that is absent from a
// fresh load: it was cancelled between the tick's load and the
// claim, so skipping the run is correct, not an error.
var errCronJobGone = errors.New("cron: job no longer exists")

// claimJob persists the advance of job j's NextRun to nextAfter
// BEFORE its turn runs (F-C4). The slot is consumed on disk
// before any token is spent or any reply is sent: a daemon crash
// or restart mid-turn finds NextRun already in the future and
// cannot re-fire the same slot, and the missed window is judged
// from the claimed NextRun on the next tick, not from the stale
// one. The claim loads fresh and updates only this job id, so a
// concurrent cancel (job gone) or another job's claim cannot
// clobber it, and the outcome apply after the run overwrites the
// claimed NextRun with the run's real outcome (same value on
// success, backed off on failure) — the claim never changes what
// a normal run records, it only makes the slot crash-safe.
func (s *CronScheduler) claimJob(ctx context.Context, b cronBucket, j *CronJob, nextAfter time.Time) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	release, err := s.store.lockState(ctx, b.dir, cronJobsFile)
	if err != nil {
		return fmt.Errorf("claim job %q: lock jobs: %w", j.ID, err)
	}
	defer release()
	doc, err := loadCronJobs(s.store, b.dir, b.key)
	if err != nil {
		return fmt.Errorf("claim job %q: reload jobs: %w", j.ID, err)
	}
	found := false
	for _, jj := range doc.validJobs() {
		if jj.ID == j.ID {
			jj.NextRun = nextAfter.Format(time.RFC3339)
			found = true
		}
	}
	if !found {
		return errCronJobGone
	}
	if err := s.persistJobs(b.dir, b.key, doc); err != nil {
		return fmt.Errorf("claim job %q: save jobs: %w", j.ID, err)
	}
	return nil
}

// retryOutcomesAsFailures is the F-C4 surfacing for an outcome
// save failure: the turn ran, but its outcome could not be
// persisted, so the first save's error is retried once with the
// failure counted — a persist failure is itself a job failure for
// backoff purposes, so a full disk backs the job off and disables
// it after 5 instead of re-firing the same slot every 30 seconds.
// A missed-run skip is not a run, so it is retried unchanged with
// no failure counted. A job cancelled while its turn was running
// is absent from the fresh load and its outcome is dropped, as in
// applyOutcomes. When the retry also fails, the error is returned
// (and logged loudly by ProcessDue): the claim already advanced
// NextRun past this slot, so the job still cannot re-fire the
// same slot in a tight loop — the accounting is lost, but the
// duplicate is not.
func (s *CronScheduler) retryOutcomesAsFailures(ctx context.Context, b cronBucket, outcomes []cronOutcome, saveErr error) error {
	s.logf("chat %d: outcome save failed for %d job(s): %v — retrying once with the failure counted", b.chatID, len(outcomes), saveErr)
	retry := make([]cronOutcome, 0, len(outcomes))
	for _, oc := range outcomes {
		if oc.lastRun.IsZero() {
			retry = append(retry, oc)
			continue
		}
		failures := oc.failures + 1
		disabled := failures >= cronMaxFailures
		lastRes := fmt.Sprintf("error: outcome save failed (%v): %s", saveErr, oc.lastRes)
		if disabled {
			lastRes = fmt.Sprintf("disabled after %d consecutive failures: outcome save failed (%v)", failures, saveErr)
		}
		retry = append(retry, cronOutcome{
			jobID:    oc.jobID,
			nextRun:  backoffNextRun(oc.nextRun, oc.lastRun, failures),
			lastRun:  oc.lastRun,
			lastRes:  lastRes,
			failures: failures,
			disabled: disabled,
		})
	}
	if err := s.applyOutcomes(ctx, b, retry); err != nil {
		s.logf("chat %d: outcome save retry failed: %v (original save error: %v)", b.chatID, err, saveErr)
		return fmt.Errorf("save jobs: %w (retry also failed: %v)", saveErr, err)
	}
	return nil
}

// runJob runs one due job's turn under the per-turn deadline and
// builds its outcome. nextAfter is the next cron occurrence
// after `at`, from classifyCronJob; `at` is the time the job was
// actually processed, and is the base for lastRun. The failure
// backoff is NOT based on `at`: it is anchored at the time the
// turn finished (doneAt, below), so a turn that runs long and
// then fails — a timeout, or a 9-minute turn that errors —
// gets a next run at least the backoff after its outcome, never
// a next run already in the past that would re-fire on the next
// tick. Anchoring at `at` would do exactly that: a turn that
// times out ends at at+timeout, while failures=1 backs off only
// at+1min, so the backed-off next run would be ~9 minutes stale
// the moment it was applied, still inside the missed window,
// and the job would re-fire after every timeout until it was
// disabled. A fast turn is unaffected: its outcome time is
// `at` plus milliseconds, and the stored next run (second
// precision) is what it was before. The caller (processChat)
// has already claimed the slot: the job's NextRun is advanced
// in the store before runJob is invoked, so a crash mid-turn
// cannot re-fire it (F-C4).
//
// The turn runs in its own goroutine and the scheduler stops
// waiting at the deadline: a runner that ignores its context
// cannot hold the worker (or the tick) forever — the late result
// is discarded and the job is recorded as a timeout failure
// with backoff, never silently dropped and never re-fired
// immediately. A runner that honours its context (the daemon's
// runScheduledTurn does, through the agent) ends promptly on
// cancellation, so no goroutine is left behind in the normal
// case.
//
// A deliberate stop is not a failure (F-C5): the run is
// registered in the scheduler's live-turn table before the
// runner is invoked, so StopJob (cron_cancel) can cancel exactly
// this job's per-turn context, and the runner returns
// ErrCronTurnStopped (runScheduledTurn translates a cancelled
// turn context into it). Any of the three stop signals — the
// sentinel, a context.Canceled error, or the stopped mark
// StopJob left — records the run as "stopped": the slot is
// consumed and the failure count is untouched. A timeout still
// counts as a failure: the deadline reports DeadlineExceeded,
// never Canceled, and the stopped check runs before the timeout
// override, so a stop racing the deadline is a stop, not a
// timeout failure.
func (s *CronScheduler) runJob(ctx context.Context, j *CronJob, at time.Time, nextAfter time.Time, runner CronRunner, chatID int64) *cronOutcome {
	// Due: run the turn. The copy carries the bucket's chat id
	// (see cronBucket): the stored ChatID is never trusted to
	// redirect a run.
	runJob := *j
	runJob.ChatID = chatID
	timeout := s.turnTimeoutDuration()
	turnCtx, cancel := context.WithTimeout(ctx, timeout)
	s.registerRun(j.ID, chatID, cancel)
	defer s.unregisterRun(j.ID)
	defer cancel()
	type runResult struct {
		err error
	}
	// turnStart anchors the outcome time (below) in real
	// elapsed time: the scheduler's clock is the tick time
	// `at`, which a test (or a delayed tick) may hold ahead of
	// the wall clock, so the outcome time must be `at` plus
	// the real duration of the turn — never time.Now()
	// itself, which would mix the two clocks.
	turnStart := time.Now()
	resCh := make(chan runResult, 1)
	go func() {
		_, err := runner(turnCtx, runJob)
		resCh <- runResult{err: err}
	}()
	var runErr error
	select {
	case res := <-resCh:
		runErr = res.err
	case <-turnCtx.Done():
		// Deadline race: the runner may have returned at the
		// exact instant the deadline fired, so a result may
		// already be sitting in the buffered channel. A result
		// that is there is the turn's real outcome — use it
		// instead of recording a healthy turn as a timeout.
		select {
		case res := <-resCh:
			runErr = res.err
		default:
			// The wait ended at the deadline (or the parent
			// ctx was cancelled). The runner goroutine is
			// left to finish on its own; its late result is
			// discarded (the buffered channel lets it exit
			// without a reader).
			runErr = ctx.Err()
			if runErr == nil {
				runErr = turnCtx.Err()
			}
		}
	}
	// doneAt is the turn's outcome time: the moment the
	// scheduler stopped waiting, whether the runner returned
	// or the deadline fired. It is `at` plus the real time
	// the turn took, so it stays on the tick's clock (see
	// turnStart above) — a wall-clock time.Now() here would
	// mix clocks whenever the tick time runs ahead of real
	// time. The failure backoff is anchored here, not at `at`
	// (see the doc comment above).
	doneAt := at.Add(time.Since(turnStart))
	// A deliberate stop (F-C5) is not a failure: the slot is
	// consumed and the failure count is untouched. The check
	// runs before the timeout override below, so a stop racing
	// the deadline is a stop. The bare context.Canceled case
	// counts as a stop only when the deadline did NOT fire: a
	// runner echoing the deadline returns a context error too,
	// and that is a timeout, not a stop.
	if runErr != nil && (errors.Is(runErr, ErrCronTurnStopped) || s.wasStopped(j.ID) || (errors.Is(runErr, context.Canceled) && turnCtx.Err() != context.DeadlineExceeded)) {
		return &cronOutcome{
			jobID:    j.ID,
			nextRun:  nextAfter,
			lastRun:  at,
			lastRes:  "stopped: scheduled turn stopped by the owner",
			failures: j.ConsecutiveFailures,
			disabled: false,
		}
	}
	// A turn that hit its deadline — and produced no result, or
	// produced only the context error itself — is a timeout
	// failure, with a message that says so. A runner that
	// honours its context returns ctx.Err() when the deadline
	// fires, and that echo of the deadline is not a turn
	// outcome: it is recorded as a timeout, as before. But a
	// result that arrived with the deadline is the turn's real
	// outcome, so a success or a real error is kept as-is and
	// is never rewritten into a timeout.
	if turnCtx.Err() == context.DeadlineExceeded && runErr != nil && (errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled)) {
		runErr = fmt.Errorf("scheduled turn timed out after %s", timeout)
	}
	if runErr != nil {
		if errors.Is(runErr, ErrBudgetExceeded) {
			// F-C7: the chat's daily token budget is exhausted.
			// This is neither a turn failure nor a success: the
			// job did nothing wrong, and the budget resets at
			// UTC midnight — so the outcome is distinct, the
			// failure count is left exactly where it was
			// (neither reset to 0, which would erase a real
			// failure streak, nor incremented, which would
			// disable a healthy job after 5 over-budget days),
			// the job is never disabled on this outcome, and
			// the next run waits for the reset instead of the
			// next cron slot (a minutely job would otherwise
			// re-fire every minute until midnight).
			nextRun := nextBudgetRun(nextAfter, at)
			return &cronOutcome{
				jobID:    j.ID,
				nextRun:  nextRun,
				lastRun:  at,
				lastRes:  fmt.Sprintf("%s: daily token budget reached; next run %s (UTC)", CronBudgetExceededResult, nextRun.Format(time.RFC3339)),
				failures: j.ConsecutiveFailures,
				disabled: false,
			}
		}
		failures := j.ConsecutiveFailures + 1
		disabled := failures >= cronMaxFailures
		lastRes := fmt.Sprintf("error: %v", runErr)
		if disabled {
			lastRes = fmt.Sprintf("disabled after %d consecutive failures: %v", failures, runErr)
		}
		return &cronOutcome{
			jobID:    j.ID,
			nextRun:  backoffNextRun(nextAfter, doneAt, failures),
			lastRun:  at,
			lastRes:  lastRes,
			failures: failures,
			disabled: disabled,
		}
	}
	return &cronOutcome{
		jobID:    j.ID,
		nextRun:  nextAfter,
		lastRun:  at,
		lastRes:  "ok",
		failures: 0,
		disabled: false,
	}
}

// nextBudgetRun advances the next run past the daily budget
// reset (F-C7): the next cron slot, unless that slot falls
// before the next UTC midnight — then the reset itself. A
// minutely job therefore waits until midnight instead of firing
// every minute; a daily job keeps its next-day slot, which is
// already past the reset.
func nextBudgetRun(nextCron, now time.Time) time.Time {
	if reset := nextUTCMidnight(now); reset.After(nextCron) {
		return reset
	}
	return nextCron
}

// nextUTCMidnight returns the UTC midnight after at — the moment
// the per-chat daily token budget resets (internal/budget
// ResetAt computes the same instant from its own clock).
func nextUTCMidnight(at time.Time) time.Time {
	t := at.UTC()
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
}

// backoffNextRun applies the exponential failure backoff to the
// next cron occurrence: 2^(failures-1) minutes, capped at
// cronMaxBackoff. `now` is the turn's OUTCOME time (when the
// scheduler stopped waiting), not the time the turn started:
// anchoring the backoff at the start would put the next run in
// the past for any turn that ran longer than its backoff (a
// timeout, or any long turn that then fails), and the job would
// re-fire on the next tick. A job whose cron would fire again
// in 30 seconds after a failure waits at least a minute
// instead; a daily job keeps its daily slot (the backoff is far
// shorter than a day), so backoff only delays, never advances,
// the next cron time.
func backoffNextRun(nextCron, now time.Time, failures int) time.Time {
	if failures < 1 {
		return nextCron
	}
	shift := failures - 1
	if shift > 6 {
		shift = 6
	}
	backoff := time.Duration(1<<uint(shift)) * time.Minute
	if backoff > cronMaxBackoff {
		backoff = cronMaxBackoff
	}
	if delayed := now.Add(backoff); delayed.After(nextCron) {
		return delayed
	}
	return nextCron
}

// applyOutcomes applies outcomes ID-scoped against a fresh load:
// a job cancelled while its turn was running is simply absent,
// so its outcome is dropped instead of resurrecting it, and a
// job created mid-tick is untouched. The whole apply is one
// flocked load-modify-save.
func (s *CronScheduler) applyOutcomes(ctx context.Context, b cronBucket, outcomes []cronOutcome) error {
	byID := make(map[string]cronOutcome, len(outcomes))
	for _, oc := range outcomes {
		byID[oc.jobID] = oc
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	release, err := s.store.lockState(ctx, b.dir, cronJobsFile)
	if err != nil {
		return fmt.Errorf("lock jobs: %w", err)
	}
	defer release()
	doc, err := loadCronJobs(s.store, b.dir, b.key)
	if err != nil {
		return fmt.Errorf("reload jobs: %w", err)
	}
	jobs := doc.validJobs()
	changed := false
	for _, j := range jobs {
		oc, ok := byID[j.ID]
		if !ok {
			continue
		}
		changed = true
		j.NextRun = oc.nextRun.Format(time.RFC3339)
		if !oc.lastRun.IsZero() {
			j.LastRun = oc.lastRun.Format(time.RFC3339)
		}
		j.LastResult = oc.lastRes
		j.ConsecutiveFailures = oc.failures
		j.Disabled = oc.disabled
	}
	if !changed {
		return nil
	}
	// The doc is saved whole, so quarantined entries survive
	// the outcome apply instead of being silently dropped.
	if err := s.persistJobs(b.dir, b.key, doc); err != nil {
		return fmt.Errorf("save jobs: %w", err)
	}
	return nil
}
