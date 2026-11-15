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
//     instead of firing the same doomed turn forever. Safe tools
//     still run freely in a scheduled turn, exactly as they do
//     interactively.
//   - Safety: a job that fails repeatedly backs off exponentially
//     (1, 2, 4, 8, 16 minutes) and is DISABLED after 5 consecutive
//     failures, so the backoff never grows past 16 minutes — never
//     a hot loop.
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
	// at once in one tick (F-C2). A tick with more due chats
	// than this queues the rest behind the pool — bounded, so a
	// burst of due jobs cannot spawn an unbounded pile of agent
	// turns, and one chat's long turn delays other chats only
	// up to the pool bound. 8 is far above the chats a daemon
	// normally serves at once, and far below any resource
	// concern.
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
// "skipped: ...", "disabled after 5 consecutive failures: ...").
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
func loadCronJobs(s *PlanningStore, dir, key string) ([]*CronJob, error) {
	if dir == "" {
		b := s.bucket(key)
		out := make([]*CronJob, len(b.cronJobs))
		for i, j := range b.cronJobs {
			cp := *j
			out[i] = &cp
		}
		return out, nil
	}
	data, err := readStateFile(filepath.Join(dir, cronJobsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
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
		return nil, nil
	}
	seen := make(map[string]bool, len(doc.Jobs))
	for _, j := range doc.Jobs {
		if j == nil || j.ID == "" || seen[j.ID] {
			return nil, fmt.Errorf("corrupt cron jobs file %q: duplicate or missing job id", filepath.Join(dir, cronJobsFile))
		}
		seen[j.ID] = true
		if _, err := parseCronExpression(j.Expression); err != nil {
			return nil, fmt.Errorf("corrupt cron jobs file %q: job %q has an invalid expression: %v", filepath.Join(dir, cronJobsFile), j.ID, err)
		}
		if strings.TrimSpace(j.Prompt) == "" {
			return nil, fmt.Errorf("corrupt cron jobs file %q: job %q has an empty prompt", filepath.Join(dir, cronJobsFile), j.ID)
		}
		if _, err := j.nextRunTime(); err != nil {
			return nil, fmt.Errorf("corrupt cron jobs file %q: %v", filepath.Join(dir, cronJobsFile), err)
		}
		if j.ConsecutiveFailures < 0 {
			return nil, fmt.Errorf("corrupt cron jobs file %q: job %q has a negative failure count", filepath.Join(dir, cronJobsFile), j.ID)
		}
	}
	return doc.Jobs, nil
}

// saveCronJobs persists jobs in creation order. Call with s.mu
// held; the caller takes the flock (lockState) around the
// load-modify-save, as task_create does.
func saveCronJobs(s *PlanningStore, dir, key string, jobs []*CronJob) error {
	if dir == "" {
		b := s.bucket(key)
		cp := make([]*CronJob, len(jobs))
		for i, j := range jobs {
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
	jobs, err := loadCronJobs(t.store, dir, key)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	if len(jobs) >= cronMaxJobsPerChat {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("job limit reached (%d jobs for this chat); cancel an old job with cron_cancel first", cronMaxJobsPerChat)}
	}
	var id string
	for attempts := 0; attempts < 10; attempts++ {
		cand, err := newCronJobID()
		if err != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
		}
		collides := false
		for _, existing := range jobs {
			if existing.ID == cand {
				collides = true
				break
			}
		}
		if !collides {
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
	jobs = append(jobs, job)
	if err := saveCronJobs(t.store, dir, key, jobs); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	note := "Times are UTC (Orion has no per-chat timezone). The scheduled turn is unattended: persisted \"always allow\" rules never apply to it, and a destructive or network action inside it is denied promptly instead of running."
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
	return "List this chat's scheduled cron jobs (id, expression, prompt, next run, last outcome, failure count, disabled flag). Only this chat's jobs are ever listed. Times are UTC."
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
	jobs, err := loadCronJobs(t.store, dir, key)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	summaries := make([]cronJobSummary, 0, len(jobs))
	for _, j := range jobs {
		summaries = append(summaries, summarizeCronJob(j))
	}
	return encodeCronJob(t.Name(), map[string]any{
		"jobs": summaries, "count": len(summaries), "timezone": "UTC",
	})
}

// ---------------------------------------------------------------- cron_cancel

type cronCancelTool struct{ store *PlanningStore }

func (t *cronCancelTool) Name() string { return "cron_cancel" }

func (t *cronCancelTool) Description() string {
	return "Cancel one scheduled cron job by id (from cron_schedule or cron_list). The job stops firing; its past runs are not undone. Only this chat's jobs can be cancelled."
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
	jobs, err := loadCronJobs(t.store, dir, key)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	kept := jobs[:0]
	found := false
	for _, j := range jobs {
		if j.ID == id {
			found = true
			continue
		}
		kept = append(kept, j)
	}
	if !found {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("unknown job %q: use cron_list to see this chat's jobs", id)}
	}
	if err := saveCronJobs(t.store, dir, key, kept); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	return encodeCronJob(t.Name(), map[string]any{"cancelled": true, "job_id": id})
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
// job's consecutive failures (F-C8). A non-nil error otherwise
// means the turn itself failed, and counts toward the job's
// consecutive failures. The runner runs OUTSIDE the store lock,
// so it may take as long as a turn takes.
type CronRunner func(ctx context.Context, job CronJob) (string, error)

// CronScheduler fires due cron jobs. It scans the store's chat
// buckets on a tick and runs each due job through its runner.
// Construct it with NewCronScheduler, install the runner with
// SetRunner, then run it with Run(ctx) in the daemon.
type CronScheduler struct {
	store *PlanningStore

	mu     sync.Mutex // guards runner, tick, log, turnTimeout, maxWorkers
	runner CronRunner
	tick   time.Duration
	log    *log.Logger
	// turnTimeout is the per-turn deadline; 0 = cronTurnTimeout.
	turnTimeout time.Duration
	// maxWorkers is the worker-pool bound for one tick; 0 =
	// cronMaxWorkers.
	maxWorkers int

	claimMu sync.Mutex // guards inFlight
	// inFlight is the per-chat claim (F-C2): chats whose tick
	// work is still in progress. An overlapping tick skips a
	// claimed chat instead of double-firing its jobs — but only
	// that chat, never the whole tick. The claim is held from
	// the scan until that chat's runs and outcome apply finish.
	inFlight map[int64]struct{}
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

// SetMaxWorkers overrides how many chats' turns run at once in
// one tick (F-C2). A non-positive value restores the default
// (cronMaxWorkers).
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

func (s *CronScheduler) maxWorkerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxWorkers <= 0 {
		return cronMaxWorkers
	}
	return s.maxWorkers
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
	// The pool is per tick: one goroutine per chat bucket, but
	// only maxWorkerCount of them may run turns at once. (In
	// production Run serialises ticks, so the live bound is
	// one pool; overlapping ticks — concurrent ProcessDue calls
	// — each bring their own, still bounded, pool.)
	sem := make(chan struct{}, s.maxWorkerCount())
	var wg sync.WaitGroup
	for _, b := range buckets {
		if err := ctx.Err(); err != nil {
			return err
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
	return nil
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
	jobs, err := loadCronJobs(s.store, b.dir, b.key)
	s.store.mu.Unlock()
	if err != nil {
		return fmt.Errorf("load jobs: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Fast filter at tick time: does this chat have anything
	// due? Chats with nothing due never take a worker slot.
	// The authoritative decision is re-made per job below, at
	// the time it is actually processed — this filter only
	// decides whether the chat needs a slot at all.
	needsWorker := false
	for _, j := range jobs {
		if cronDueAt(j, now) {
			needsWorker = true
			break
		}
	}
	if !needsWorker {
		// Nothing can run this tick, but a job already past
		// the missed window still needs its skip outcome
		// recorded (this chat never waited for a worker, so
		// tick time IS its processing time here).
		var outcomes []cronOutcome
		for _, j := range jobs {
			if kind, _, oc := s.classifyCronJob(j, now, b.chatID); kind == cronJobMissed {
				outcomes = append(outcomes, *oc)
			}
		}
		if len(outcomes) == 0 {
			return nil
		}
		return s.applyOutcomes(ctx, b, outcomes)
	}
	// A chat with work to run takes a worker slot: at most
	// maxWorkerCount chats run turns at once, so one chat's
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
	for _, j := range jobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		at := now.Add(time.Since(wallStart))
		kind, nextAfter, oc := s.classifyCronJob(j, at, b.chatID)
		switch kind {
		case cronJobMissed:
			outcomes = append(outcomes, *oc)
		case cronJobDue:
			outcomes = append(outcomes, *s.runJob(ctx, j, at, nextAfter, runner, b.chatID))
		}
	}
	if len(outcomes) == 0 {
		return nil
	}
	return s.applyOutcomes(ctx, b, outcomes)
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
		// Unreachable for a loaded job (loadCronJobs
		// validates), but a job must never crash the tick.
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

// runJob runs one due job's turn under the per-turn deadline and
// builds its outcome. nextAfter is the next cron occurrence
// after `at`, from classifyCronJob; `at` is the time the job was
// actually processed, and is the base for lastRun and backoff.
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
// F-C4 hook: a claim-before-run step belongs here, before the
// runner is invoked — atomically mark the job running in the
// store (or advance NextRun) so a daemon crash mid-turn cannot
// re-fire it on restart. The per-chat in-flight claim in
// ProcessDue only guards overlapping ticks in this process; it
// does not survive a restart.
func (s *CronScheduler) runJob(ctx context.Context, j *CronJob, at time.Time, nextAfter time.Time, runner CronRunner, chatID int64) *cronOutcome {
	// Due: run the turn. The copy carries the bucket's chat id
	// (see cronBucket): the stored ChatID is never trusted to
	// redirect a run.
	runJob := *j
	runJob.ChatID = chatID
	timeout := s.turnTimeoutDuration()
	turnCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type runResult struct {
		err error
	}
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
		// The wait ended at the deadline (or the parent ctx
		// was cancelled). The runner goroutine is left to
		// finish on its own; its late result is discarded
		// (the buffered channel lets it exit without a reader).
		runErr = ctx.Err()
		if runErr == nil {
			runErr = turnCtx.Err()
		}
	}
	// A turn that hit its deadline — whether the runner
	// reported it or the scheduler stopped waiting — is a
	// timeout failure, with a message that says so.
	if turnCtx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("scheduled turn timed out after %s", timeout)
	}
	if runErr != nil {
		failures := j.ConsecutiveFailures + 1
		disabled := failures >= cronMaxFailures
		lastRes := fmt.Sprintf("error: %v", runErr)
		if disabled {
			lastRes = fmt.Sprintf("disabled after %d consecutive failures: %v", failures, runErr)
		}
		return &cronOutcome{
			jobID:    j.ID,
			nextRun:  backoffNextRun(nextAfter, at, failures),
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

// backoffNextRun applies the exponential failure backoff to the
// next cron occurrence: 2^(failures-1) minutes, capped at
// cronMaxBackoff. A job whose cron would fire again in 30
// seconds after a failure waits at least a minute instead; a
// daily job keeps its daily slot (the backoff is far shorter
// than a day), so backoff only delays, never advances, the next
// cron time.
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
	jobs, err := loadCronJobs(s.store, b.dir, b.key)
	if err != nil {
		return fmt.Errorf("reload jobs: %w", err)
	}
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
	if err := saveCronJobs(s.store, b.dir, b.key, jobs); err != nil {
		return fmt.Errorf("save jobs: %w", err)
	}
	return nil
}
