// Telegram slash-command framework.
//
// A Registry maps "/name" to a Command. Every command is real: its
// handler calls into the app through CommandEnv (small function
// fields, so messaging never imports app and there is no import
// cycle), and every field must be wired to a real implementation —
// no nil placeholders. Adding a command is a ~20-line job:
//
//  1. Add a Command literal in DefaultRegistry (name, description,
//     handler) — the handler closes over env and returns the reply
//     text; expected failures (bad argument, nothing to undo) are
//     returned as reply text, not errors, so the user sees them
//     instead of a generic "Sorry, I hit an error".
//  2. If the command needs a capability CommandEnv does not have,
//     add a function field to CommandEnv, wire it in
//     cmd/oriond/buildCommandEnv, and use it in the handler.
//  3. Add a test (Dispatch the command through a stub env and
//     assert the reply and the side effect).
//
// /help lists the registry, so a new command shows up in the menu
// and in /help automatically; BotCommands feeds the same list to
// Telegram's setMyCommands, so the "/" menu never goes stale either.
package messaging

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/permissions"
)

// Command is one slash command.
type Command struct {
	// Name without the leading "/" (e.g. "help").
	Name string
	// Description is shown in /help and in Telegram's "/" menu.
	Description string
	// Handler runs the command. args is the text after the command
	// name ("" when none). The returned string is sent back to the
	// chat; a non-nil error is reported as a handler error instead.
	Handler func(ctx context.Context, env *CommandEnv, args string) (string, error)
}

// CommandEnv is the app-side surface commands need. Every field is a
// small function so messaging never imports app (which would cycle:
// app does not import messaging, but the daemon wires both). The
// daemon fills every field in cmd/oriond/buildCommandEnv — a nil
// field is a wiring bug, not a supported state.
type CommandEnv struct {
	// ChatID is the chat the current dispatch is for. Dispatch never
	// sets it: the caller (cmd/oriond's TurnFunc) copies the shared
	// env per turn and sets ChatID on the copy before Dispatch, so
	// concurrent chats never share the value. Handlers read it; they
	// never write it.
	ChatID int64
	// OwnerID is the owner chat/user id: the first
	// telegram_allowed_chats entry (messaging.OwnerIDFromAllowlist).
	// 0 means "no owner" (empty allowlist with allow-all opt-in):
	// owner-only commands are then refused for everyone. The
	// daemon sets it in cmd/oriond/buildCommandEnv; handlers use
	// isOwner to check it (chat match, or the sender from ctx —
	// the owner sending from a group chat — see SenderIDFromContext).
	OwnerID int64
	// Status renders the /status body (provider, model, limits,
	// memory counts, data dir, uptime).
	Status func() string
	// ModelInfo returns the current provider/model and the known
	// provider names, for /model.
	ModelInfo func() (provider, model string, providers []string)
	// Limits returns the per-turn step budget and per-step timeout
	// in seconds, for /usage.
	Limits func() (maxSteps, timeoutSeconds int)
	// SwitchProvider switches the daemon default provider (persisted,
	// effective next turn) for chats without a per-chat override;
	// chats with an override keep their provider. Returns an error
	// naming the problem.
	SwitchProvider func(name string) error
	// Usage returns daemon-wide token/turn totals (all chats).
	Usage func() (tokens int, turns int)
	// UsageForChat renders the per-chat, per-day usage block for
	// /usage (tokens today with the prompt/completion/subagent
	// split, estimated USD cost, daily token budget). It is wired
	// by the daemon to the app's per-chat accounting; nil in
	// hand-built envs, where /usage shows the daemon-wide totals
	// only, exactly as before.
	UsageForChat func(chatID int64) string
	// DeadLetter returns the subagent dead-letter ledger's
	// contents (F-A10-2): the tokens and task count of finished
	// subagent tasks that left the registry through eviction
	// WITHOUT ever being charged (see subagent.Manager).
	// The ledger is daemon-wide and unattributed to any chat, so
	// /usage renders it as its own daemon-wide line, never folded
	// into a chat's totals. The read is non-draining: /usage
	// must never consume the ledger (a future charge consumer
	// drains it with DrainDeadLetter), so the counts stay stable
	// across /usage calls. Nil in hand-built envs, where the
	// line is omitted, exactly as before.
	DeadLetter func() (tokens, tasks int)
	// Session returns a copy of the chat's session state.
	Session func(chatID int64) *Session
	// NewSession drops the conversation history (facts survive) and
	// resets the chat's turn state.
	NewSession func(chatID int64) error
	// StopRun cancels the live turn for the chat; false when nothing
	// was running.
	StopRun func(chatID int64) bool
	// Undo removes the last n user turns from memory and returns
	// their texts.
	Undo func(chatID int64, n int) ([]string, error)
	// Retry re-runs the chat's last user message through the agent.
	Retry func(ctx context.Context, chatID int64) (string, *core.TurnStats, error)
	// Pause / IsPaused control and report the daemon-wide pause.
	Pause    func(on bool)
	IsPaused func() bool
	// Agents renders the active-agents view.
	Agents func() string
	// SetTitle persists the session title.
	SetTitle func(chatID int64, title string) error
	// Compress summarises messages older than the recent window and
	// drops them; returns message counts before/after and the
	// summary length. keepRecent is the recent window to keep.
	Compress func(ctx context.Context, chatID int64, keepRecent int) (before, after, summaryLen int, err error)
	// SaveConversation exports the conversation to Markdown and
	// returns the path and message count.
	SaveConversation func(chatID int64) (path string, count int, err error)
	// Restart signals the daemon to drain active runs and exit 42
	// (a supervisor restarts it).
	Restart func()
	// BudgetInfo returns the per-chat daily token budget for
	// /budget: the configured limit (<= 0 means unlimited) and the
	// tokens this chat has used today (UTC day). The daemon wires
	// it to the app's Budget (see App.BudgetInfoForChat); nil in
	// hand-built envs, where /budget reports that budget info is
	// unavailable instead of inventing numbers.
	BudgetInfo func(chatID int64) (limit int, usedToday int)
	// BudgetAlertThreshold returns the per-chat daily usage
	// alert threshold in tokens (F-A18-11, rc24): 0 means the
	// alert is disabled. /budget shows it next to the limit so
	// the owner can see when they will be notified. Nil in
	// hand-built envs, where the threshold line is omitted
	// instead of invented. The threshold itself is set in the
	// config file (daily_budget_alert_threshold_tokens), not
	// through /budget.
	BudgetAlertThreshold func() int
	// SetBudget persists a new per-chat daily token limit (0 =
	// unlimited, otherwise 1000–10000000, matching config
	// validation) AND applies it to the live budget, so the new
	// limit survives restarts. The daemon wires it to
	// App.SetDailyBudget, which saves the config atomically
	// (0600, like SwitchProvider) before touching the live
	// budget; an in-memory-only SetLimit would be silently lost
	// on the next restart, so a SetBudget that does not persist
	// is a wiring bug. The handler validates the range first and
	// reports bad input as a reply, so this is only called with
	// a valid limit — it still validates defensively and returns
	// an error naming the problem.
	SetBudget func(limit int) error
	// ReloadSkills re-reads the skills directory (F-A17-15) and
	// returns how many skills loaded. New/changed files are
	// picked up, removed files are dropped, and a file that now
	// fails to parse is dropped with the error returned (the
	// previously loaded skills stay live when the directory
	// cannot be read at all). The daemon wires it to
	// App.ReloadSkills, which reloads the registry in place, so
	// the agent and the skill tool keep working on the same
	// registry pointer. Nil in hand-built envs, where /skills
	// reports that reloading is unavailable.
	ReloadSkills func() (loaded int, err error)
	// Whoami renders the access line for the chat.
	Whoami func(chatID int64) string
	// SetFooter persists the per-chat footer preference.
	SetFooter func(chatID int64, on bool) error
	// MemoryInfo renders the memory review.
	MemoryInfo func() (string, error)
	// ForgetFact deletes fact number index (1-based) and returns
	// the deleted key.
	ForgetFact func(index int) (string, error)
	// TypedMemories lists the typed memories (workstream A:
	// memory.TypedStore.List over <dataDir>/memories/*.md). The
	// coordinator wires this in cmd/oriond/buildCommandEnv; until
	// then it is nil and /memory shows facts only, exactly as
	// before.
	TypedMemories func() ([]TypedMemory, error)
	// ForgetTypedMemory deletes the typed memory named name (the
	// file is removed by the memory store — messaging never
	// deletes files directly) and returns the deleted name. The
	// coordinator wires this to memory.TypedStore.Delete; until
	// then it is nil and /memory forget <name> replies that
	// typed-memory forgetting is unavailable, while
	// /memory forget N keeps working for facts.
	ForgetTypedMemory func(name string) (string, error)
	// SaveFeedbackMemory creates a feedback memory from an explicit
	// user confirmation (the /memory feedback command). It is the
	// ONLY way a feedback memory enters the store: the model never
	// calls it, and the store refuses non-user feedback. Nil until
	// the coordinator wires it in cmd/oriond/buildCommandEnv.
	SaveFeedbackMemory func(name, rule, why, how string) (string, error)
	// SessionMemory returns the current session-memory.md content
	// (workstream A: memory.SessionLog.Read, "" when there is
	// none). The coordinator wires this in cmd/oriond; until then
	// it is nil. Read errors are best-effort: /compress proceeds
	// without it and says so.
	SessionMemory func(ctx context.Context) (string, error)
	// ListSessions lists this chat's per-session transcripts,
	// newest first, with the live session id (audit P0 #4). Nil
	// until the daemon wires it; /sessions then replies that
	// sessions are unavailable instead of panicking.
	ListSessions func(chatID int64) ([]SessionInfo, string, error)
	// ResumeSession resumes a named session (full id or unique
	// leading prefix) for this chat: the transcript's messages
	// replace the working history and the live session moves to
	// it. It returns the resolved id and the message count now in
	// working history. Nil until the daemon wires it; /resume
	// then replies that sessions are unavailable.
	ResumeSession func(chatID int64, idOrPrefix string) (string, int, error)
	// LastResumeReport returns what the last /resume actually
	// restored for this chat (corrupt lines skipped, cap applied,
	// summaries merged). Optional: when nil the /resume reply
	// stays in its short form. The daemon wires it to the app's
	// LastResumeReportForChat.
	LastResumeReport func(chatID int64) (ResumeReport, bool)
	// CompressWithSessionMemory is the /compress variant that
	// takes the session-memory text so the saved summary preserves
	// decisions/facts from session memory (not just the raw
	// messages) and cites that it drew on it. Used instead of
	// Compress when set and session memory is non-empty. Nil
	// until the coordinator wires it to the app-side variant
	// (internal/app Compress lives outside messaging); /compress
	// then falls back to plain Compress.
	CompressWithSessionMemory func(ctx context.Context, chatID int64, keepRecent int, sessionMemory string) (before, after, summaryLen int, err error)
	// IsOwner reports whether chatID is the daemon owner (the
	// first allowlisted chat). /rules consults it only as its
	// "wired" signal (nil means the rules wiring is absent and
	// /rules reports unavailability); the ownership decision
	// itself is CommandEnv.isOwner (OwnerID plus the sender
	// from ctx, so the owner in a group chat counts — F-A18-10).
	// The coordinator wires it in cmd/oriond/buildCommandEnv as:
	//   IsOwner: func(chatID int64) bool { return ownerID != 0 && chatID == ownerID },
	// Nil until wired; /rules then replies that it is
	// unavailable instead of guessing.
	IsOwner func(chatID int64) bool
	// ListRules returns all standing permission rules (F-A17-6):
	// legacy global rules (ChatID 0) and per-chat rules, sorted
	// by chat then tool (see permissions.RuleStore.List). The
	// coordinator wires it in cmd/oriond/buildCommandEnv as:
	//   ListRules: func() []permissions.Rule {
	//       g := a.PermissionGate()
	//       if g == nil || g.Rules() == nil { return nil }
	//       return g.Rules().List()
	//   },
	// Nil until wired; /rules then replies that rules are
	// unavailable instead of showing a stale or empty list.
	ListRules func() []permissions.Rule
	// DeleteRule deletes the rule for tool in chatID (0 = the
	// legacy global rule) and persists. The coordinator wires
	// it in cmd/oriond/buildCommandEnv as:
	//   DeleteRule: func(chatID int64, tool string) error {
	//       g := a.PermissionGate()
	//       if g == nil || g.Rules() == nil { return errors.New("rules store unavailable") }
	//       return g.Rules().DeleteForChat(chatID, tool)
	//   },
	// Nil until wired; /rules delete then replies that it is
	// unavailable.
	DeleteRule func(chatID int64, tool string) error
	// SetRule stores the rule for tool in chatID and persists
	// (0 = the legacy global scope). /rules scope uses it to
	// move a legacy global rule onto one chat. The coordinator
	// wires it in cmd/oriond/buildCommandEnv as:
	//   SetRule: func(chatID int64, tool string, action permissions.Action) error {
	//       g := a.PermissionGate()
	//       if g == nil || g.Rules() == nil { return errors.New("rules store unavailable") }
	//       return g.Rules().SetForChat(chatID, tool, action)
	//   },
	// Nil until wired; /rules scope then replies that it is
	// unavailable.
	SetRule func(chatID int64, tool string, action permissions.Action) error
}

// TypedMemory is one typed memory file (workstream A:
// memory.TypedMemory over <dataDir>/memories/*.md, frontmatter
// name, description, type). Alias, not a shadow type, so the
// coordinator can wire TypedMemories straight to
// memory.TypedStore.List with no conversion.
type TypedMemory = memory.TypedMemory

// SessionInfo is one per-session transcript, for /sessions (the
// messaging-local view of session.Info: messaging never imports
// the session package, so the daemon converts when wiring).
type SessionInfo struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time
	Messages  int
	Summaries int
	Bytes     int64
	Title     string // first user message, truncated
}

// ResumeReport describes what a /resume actually restored, so the
// reply can say so instead of silently dropping it (Audit 5
// F-A5-P2-5). Mirrors internal/app.ResumeReport; the daemon converts
// between the two (messaging must not import app).
type ResumeReport struct {
	SessionID          string
	TranscriptMessages int  // messages in the transcript (pre-cap)
	LoadedMessages     int  // messages now in working history (capped)
	CorruptLines       int  // unparseable transcript lines skipped
	SummariesLoaded    int  // transcript summaries merged into memory
	Capped             bool // transcript held more than the store cap
}

// memoryTypeOrder is the display order for /memory groups; types
// outside this list render after them, sorted by type name.
var memoryTypeOrder = []string{"user", "feedback", "project", "reference"}

// formatAge renders t as an age relative to now ("just now",
// "5m ago", "2h ago", "3d ago"). Future timestamps (clock skew, or
// a memory written a moment ahead) render as "just now", never as a
// negative age.
func formatAge(t time.Time) string { return formatAgeAt(t, time.Now()) }

// formatAgeAt is formatAge against an explicit now, for tests.
func formatAgeAt(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/24/30))
	default:
		return fmt.Sprintf("%dy ago", int(d.Hours()/24/365))
	}
}

// renderTypedMemories renders typed memories grouped by type (user,
// feedback, project, reference, then any other type sorted by
// name), newest first within each group, with an age and the
// description for each entry. mems is not modified.
func renderTypedMemories(mems []TypedMemory, now time.Time) string {
	groups := map[string][]TypedMemory{}
	for _, m := range mems {
		typ := strings.ToLower(strings.TrimSpace(string(m.Type)))
		if typ == "" {
			typ = "other"
		}
		groups[typ] = append(groups[typ], m)
	}
	ordered := make([]string, 0, len(groups))
	for _, typ := range memoryTypeOrder {
		if _, ok := groups[typ]; ok {
			ordered = append(ordered, typ)
		}
	}
	var rest []string
	for typ := range groups {
		known := false
		for _, k := range memoryTypeOrder {
			if typ == k {
				known = true
				break
			}
		}
		if !known {
			rest = append(rest, typ)
		}
	}
	sort.Strings(rest)
	ordered = append(ordered, rest...)

	var b strings.Builder
	b.WriteString("Typed memories:")
	for _, typ := range ordered {
		entries := groups[typ]
		sort.Slice(entries, func(i, j int) bool {
			if !entries[i].UpdatedAt.Equal(entries[j].UpdatedAt) {
				return entries[i].UpdatedAt.After(entries[j].UpdatedAt)
			}
			return entries[i].Name < entries[j].Name
		})
		fmt.Fprintf(&b, "\n  %s (%d):", typ, len(entries))
		for _, m := range entries {
			fmt.Fprintf(&b, "\n    • %s (%s)", m.Name, formatAgeAt(m.UpdatedAt, now))
			if desc := strings.TrimSpace(m.Description); desc != "" {
				fmt.Fprintf(&b, " — %s", truncateShort(desc, 80))
			}
		}
	}
	return b.String()
}

// Registry is a set of commands, sorted by name for /help and the
// Telegram menu.
type Registry struct {
	mu   sync.RWMutex
	cmds map[string]Command
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{cmds: map[string]Command{}}
}

// Register adds (or replaces) a command. The name is lower-cased and
// must not be empty; a command with an empty name or a nil handler
// is ignored (a wiring bug should not take the registry down).
func (r *Registry) Register(cmd Command) {
	name := strings.ToLower(strings.TrimSpace(cmd.Name))
	if name == "" || cmd.Handler == nil {
		return
	}
	cmd.Name = name
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmds == nil {
		r.cmds = map[string]Command{}
	}
	r.cmds[name] = cmd
}

// Commands returns all registered commands, sorted by name.
func (r *Registry) Commands() []Command {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Command, 0, len(r.cmds))
	for _, c := range r.cmds {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// BotCommands returns the registry as Telegram BotCommand entries
// (names without "/", in the same sorted order) for setMyCommands.
// Names the Bot API rejects are left out: a command name must be
// 1–32 characters of [a-z0-9_] (Telegram's rule), so a non-Latin
// alias such as /বাজেট would fail setMyCommands on every startup
// and leave the whole "/" menu stale. Excluded commands still work
// via Dispatch and still appear in /help — only the menu omits them.
func (r *Registry) BotCommands() []BotCommand {
	cmds := r.Commands()
	out := make([]BotCommand, 0, len(cmds))
	for _, c := range cmds {
		if !isValidBotCommandName(c.Name) {
			continue
		}
		out = append(out, BotCommand{Name: c.Name, Description: c.Description})
	}
	return out
}

// isValidBotCommandName reports whether name is a valid Telegram bot
// command name for setMyCommands: 1–32 characters, lowercase Latin
// letters, digits and underscores only.
func isValidBotCommandName(name string) bool {
	if len(name) < 1 || len(name) > 32 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			continue
		}
		return false
	}
	return true
}

// Parse splits text into a command name and arguments. It accepts
// "/name args" and "/name@botname args" (the @botname form is what
// Telegram sends in groups). A lone "/" is not a command, and text
// not starting with "/" is not a command. The name is lower-cased.
func Parse(text string) (name, args string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	rest := strings.TrimSpace(text[1:])
	if rest == "" {
		return "", "", false
	}
	// The command token ends at the first space (or the end).
	token := rest
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		token = rest[:i]
		args = strings.TrimSpace(rest[i+1:])
	}
	// Strip a "@botname" suffix from the token.
	if i := strings.IndexByte(token, '@'); i >= 0 {
		token = token[:i]
	}
	name = strings.ToLower(strings.TrimSpace(token))
	if name == "" {
		return "", "", false
	}
	return name, args, true
}

// Dispatch runs text as a command when it is one. It returns
// handled=false for non-commands (the caller then runs the agent).
// An unknown "/foo" returns handled=true with a "unknown command"
// reply, not an error — the user mistyped, the daemon did not fail.
// A nil env is a wiring bug and is returned as an error.
func (r *Registry) Dispatch(ctx context.Context, env *CommandEnv, text string) (reply string, handled bool, err error) {
	name, args, ok := Parse(text)
	if !ok {
		return "", false, nil
	}
	if env == nil {
		return "", true, errors.New("messaging: command env is nil")
	}
	r.mu.RLock()
	cmd, found := r.cmds[name]
	r.mu.RUnlock()
	if !found {
		return fmt.Sprintf("Unknown command /%s. Send /help for the command list.", name), true, nil
	}
	reply, err = cmd.Handler(ctx, env, args)
	return reply, true, err
}

// truncateShort shortens s to at most n runes for display in replies.
func truncateShort(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// parseCount parses an optional integer argument (default def). It
// returns an error message ("" when valid) so handlers can reply
// with it directly.
func parseCount(args string, def int) (int, string) {
	args = strings.TrimSpace(args)
	if args == "" {
		return def, ""
	}
	// Only the first word counts; anything after it is ignored.
	if i := strings.IndexByte(args, ' '); i >= 0 {
		args = args[:i]
	}
	n, err := strconv.Atoi(args)
	if err != nil {
		return 0, fmt.Sprintf("Could not parse %q as a number.", args)
	}
	return n, ""
}

// isOwner reports whether the current dispatch is from the owner:
// the chat itself is the owner, or the sender carried in ctx is
// (the owner sending from a group chat — buildTurnHandler puts the
// sender in ctx, see WithSenderID). No owner configured (OwnerID
// 0) means nobody is the owner: owner-only commands fail closed.
// It is a method on CommandEnv so handlers share one check instead
// of each re-implementing the chat-or-sender rule.
func (env *CommandEnv) isOwner(ctx context.Context) bool {
	if env == nil || env.OwnerID == 0 {
		return false
	}
	if env.ChatID == env.OwnerID {
		return true
	}
	if senderID, ok := SenderIDFromContext(ctx); ok && senderID != 0 && senderID == env.OwnerID {
		return true
	}
	return false
}

// budgetLimit bounds for /budget (and App.SetDailyBudget): 0 is
// unlimited, otherwise the same 1000–10000000 range config
// validation enforces for per_chat_daily_token_budget.
const (
	minDailyBudgetLimit = 1000
	maxDailyBudgetLimit = 10000000
)

// parseBudgetArg parses one /budget argument: "infinity", "inf" or
// "unlimited" (case-insensitive) means unlimited (0); otherwise a
// plain integer. ok=false means the argument is not a limit at all.
func parseBudgetArg(arg string) (limit int, ok bool) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "infinity", "inf", "unlimited":
		return 0, true
	}
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil {
		return 0, false
	}
	return n, true
}

// budgetHandler implements /budget and its /বাজেট alias (owner-only,
// F-A17-9 companion user order): no argument shows the current
// limit and today's usage for the chat; an argument sets the daily
// per-chat limit (persisted by SetBudget, so it survives restarts).
func budgetHandler(ctx context.Context, env *CommandEnv, args string) (string, error) {
	if !env.isOwner(ctx) {
		return "⛔ /budget is owner-only. Your chat can use every other command, but only the owner can view or change the daily token budget.", nil
	}
	if env.BudgetInfo == nil {
		return "Budget info is unavailable (not wired).", nil
	}
	limit, used := env.BudgetInfo(env.ChatID)
	arg := ""
	if fields := strings.Fields(args); len(fields) > 0 {
		arg = fields[0]
	}
	if arg == "" {
		// The alert threshold (F-A18-11) rides along with the
		// limit display: the owner sees both the cap and the
		// point at which they get notified once per day. An
		// unwired threshold (nil func) omits the line.
		thresholdLine := ""
		if env.BudgetAlertThreshold != nil {
			if threshold := env.BudgetAlertThreshold(); threshold > 0 {
				thresholdLine = fmt.Sprintf("\nAlert threshold: %s tokens (owner notified once per day when crossed; set daily_budget_alert_threshold_tokens in config)", formatThousands(threshold))
			} else {
				thresholdLine = "\nAlert threshold: off"
			}
		}
		if limit <= 0 {
			return fmt.Sprintf("💰 Daily token budget (this chat): unlimited\nTokens used today: %s (resets at UTC midnight)%s", formatThousands(used), thresholdLine), nil
		}
		remaining := limit - used
		if remaining < 0 {
			remaining = 0
		}
		return fmt.Sprintf("💰 Daily token budget (this chat): %s tokens per day\nTokens used today: %s\nRemaining today: %s (resets at UTC midnight)%s",
			formatThousands(limit), formatThousands(used), formatThousands(remaining), thresholdLine), nil
	}
	newLimit, ok := parseBudgetArg(arg)
	if !ok {
		return "Usage: /budget [limit|infinity] — limit is a number of tokens per chat per day (1000–10000000), or infinity (or 0) for unlimited. /budget with no argument shows the current limit and today's usage.", nil
	}
	if newLimit != 0 && (newLimit < minDailyBudgetLimit || newLimit > maxDailyBudgetLimit) {
		return fmt.Sprintf("❌ Daily token budget must be 0 (unlimited) or between %d and %d tokens, got %d.", minDailyBudgetLimit, maxDailyBudgetLimit, newLimit), nil
	}
	if env.SetBudget == nil {
		return "Setting the budget is unavailable (not wired).", nil
	}
	if err := env.SetBudget(newLimit); err != nil {
		return fmt.Sprintf("❌ Could not set the budget: %v", err), nil
	}
	// The limit is one value shared by every chat (the per-chat
	// daily cap), not a per-chat override: say so, so the owner
	// does not think only this chat changed. Today's usage is
	// untouched — a lower limit below today's spend simply blocks
	// the next turn until the UTC day rolls over.
	if newLimit == 0 {
		return "✅ Daily token budget set to unlimited for every chat. Today's usage is unchanged.", nil
	}
	return fmt.Sprintf("✅ Daily token budget set to %s tokens per chat per day, for every chat. Today's usage is unchanged.", formatThousands(newLimit)), nil
}

// skillsReloadHandler implements /skills reload (F-A17-15): re-read
// the skills directory without restarting the daemon.
func skillsReloadHandler(ctx context.Context, env *CommandEnv, args string) (string, error) {
	// Owner-only (F-A18-6): reloading skills changes what the
	// agent can do for every chat, so it is owner-only like
	// /budget — a non-owner allowlisted chat is refused before
	// ReloadSkills is ever called.
	if !env.isOwner(ctx) {
		return "⛔ /skills reload is owner-only. Your chat can use every other command, but only the owner can reload skills.", nil
	}
	if env.ReloadSkills == nil {
		return "Skills reload is unavailable (not wired).", nil
	}
	loaded, err := env.ReloadSkills()
	if err != nil {
		if loaded > 0 {
			return fmt.Sprintf("⚠️ Reloaded %d skill(s), but some skills failed to load: %v", loaded, err), nil
		}
		return fmt.Sprintf("❌ Could not reload skills: %v", err), nil
	}
	if loaded == 1 {
		return "✅ Reloaded 1 skill from the skills directory.", nil
	}
	return fmt.Sprintf("✅ Reloaded %d skills from the skills directory.", loaded), nil
}

// DefaultRegistry returns a registry with the 23 core commands. env
// is captured by the handlers; /help closes over the registry itself
// so its list can never go stale.
func DefaultRegistry(env *CommandEnv) *Registry {
	r := NewRegistry()

	r.Register(Command{
		Name:        "help",
		Description: "Show available commands",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			var b strings.Builder
			b.WriteString("Orion commands:\n")
			for _, c := range r.Commands() {
				fmt.Fprintf(&b, "/%s — %s\n", c.Name, c.Description)
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	})

	r.Register(Command{
		Name:        "new",
		Description: "Start a new session (fresh session ID + history)",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			if err := env.NewSession(env.ChatID); err != nil {
				return "", err
			}
			return "🆕 Started a new session. Conversation history cleared; facts kept.", nil
		},
	})

	r.Register(Command{
		Name:        "sessions",
		Description: "List saved sessions (transcripts) for this chat",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			if env.ListSessions == nil {
				return "Sessions are unavailable (not wired).", nil
			}
			infos, current, err := env.ListSessions(env.ChatID)
			if err != nil {
				return fmt.Sprintf("❌ Could not list sessions: %v", err), nil
			}
			if len(infos) == 0 {
				return "No saved sessions yet. This chat's conversation is the live session.", nil
			}
			var b strings.Builder
			b.WriteString("📼 Sessions (newest first):\n")
			for _, info := range infos {
				live := ""
				if info.ID == current {
					live = " ★ live"
				}
				title := ""
				if info.Title != "" {
					title = fmt.Sprintf(" — %q", info.Title)
				}
				fmt.Fprintf(&b, "• %s%s — %d message(s), %d summaries, updated %s%s\n",
					info.ID, live, info.Messages, info.Summaries, formatAge(info.UpdatedAt), title)
			}
			b.WriteString("Resume one with /resume <id or prefix>.")
			return b.String(), nil
		},
	})

	r.Register(Command{
		Name:        "resume",
		Description: "Resume a saved session by id (or a unique prefix)",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			args = strings.TrimSpace(args)
			if args == "" {
				return "Usage: /resume <session id or unique prefix>\nList sessions with /sessions.", nil
			}
			if env.ResumeSession == nil {
				return "Sessions are unavailable (not wired).", nil
			}
			id, count, err := env.ResumeSession(env.ChatID, strings.Fields(args)[0])
			if err != nil {
				return fmt.Sprintf("❌ Could not resume session: %v", err), nil
			}
			reply := fmt.Sprintf("✅ Resumed session %s (%d message(s) in history). Facts and memories are shared, so nothing else changed.", id, count)
			if env.LastResumeReport != nil {
				if rep, ok := env.LastResumeReport(env.ChatID); ok {
					var notes []string
					if rep.Capped {
						notes = append(notes, fmt.Sprintf("the transcript held %d messages; working history is capped at %d", rep.TranscriptMessages, rep.LoadedMessages))
					}
					if rep.CorruptLines > 0 {
						notes = append(notes, fmt.Sprintf("%d corrupt line(s) were skipped", rep.CorruptLines))
					}
					if rep.SummariesLoaded > 0 {
						notes = append(notes, fmt.Sprintf("%d summaries were merged into memory", rep.SummariesLoaded))
					}
					if len(notes) > 0 {
						reply += " Note: " + strings.Join(notes, "; ") + "."
					}
				}
			}
			return reply, nil
		},
	})

	r.Register(Command{
		Name:        "stop",
		Description: "Kill all running background processes",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			if env.StopRun(env.ChatID) {
				return "⏹ Stopped the running turn.", nil
			}
			return "Nothing is running for this chat.", nil
		},
	})

	r.Register(Command{
		Name:        "status",
		Description: "Show session, model, token, and context info",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			chatID := env.ChatID
			sess := env.Session(chatID)
			var b strings.Builder
			b.WriteString("📊 Status\n")
			b.WriteString(env.Status())
			fmt.Fprintf(&b, "\nChat: %d", chatID)
			if sess.Title != "" {
				fmt.Fprintf(&b, " (%s)", sess.Title)
			}
			fmt.Fprintf(&b, "\nTokens this chat: %d (%d turn(s))\nFooter: %s\nPaused: %v",
				sess.TotalTokens, sess.Turns, onOff(sess.FooterEnabled), env.IsPaused())
			return b.String(), nil
		},
	})

	r.Register(Command{
		Name:        "model",
		Description: "Show or switch model (switches this chat's model)",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			provider, modelName, providers := env.ModelInfo()
			args = strings.TrimSpace(args)
			if args == "" {
				return fmt.Sprintf("Current: %s / %s\nAvailable providers: %s\n\n/model <name> switches this chat's provider (persisted for this chat, effective from the next turn). Other chats are unaffected.",
					provider, modelName, strings.Join(providers, ", ")), nil
			}
			name := strings.ToLower(strings.Fields(args)[0])
			if err := env.SwitchProvider(name); err != nil {
				return fmt.Sprintf("❌ %v\nAvailable providers: %s", err, strings.Join(providers, ", ")), nil
			}
			provider, modelName, _ = env.ModelInfo()
			return fmt.Sprintf("✅ Switched to %s / %s. This switches the provider for this chat. Other chats are unaffected.", provider, modelName), nil
		},
	})

	r.Register(Command{
		Name:        "usage",
		Description: "Show token usage and rate limits",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			sess := env.Session(env.ChatID)
			tokens, turns := env.Usage()
			maxSteps, timeout := env.Limits()
			var b strings.Builder
			if env.UsageForChat != nil {
				b.WriteString(env.UsageForChat(env.ChatID))
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "Tokens this chat (all time): %d (%d turn(s))\nTokens (all chats): %d (%d turn(s))\nMax steps per turn: %d\nTimeout per step: %ds\nRate limits are set by the provider — Orion does not impose its own.",
				sess.TotalTokens, sess.Turns, tokens, turns, maxSteps, timeout)
			// Dead-letter ledger (F-A10-2): the charged/uncharged
			// split. The per-chat block above shows the tokens
			// that WERE charged to this chat (incl. its subagent
			// sweep); this line shows the subagent tokens that
			// were NEVER charged — finished tasks evicted from
			// the registry before any sweep saw them. It is
			// daemon-wide and unattributed, so it is rendered
			// as its own line, never added to any chat's total,
			// and only when the ledger holds something: an
			// empty ledger is the normal case and needs no line.
			// The read is non-draining (see DeadLetter).
			if env.DeadLetter != nil {
				if dlTokens, dlTasks := env.DeadLetter(); dlTokens > 0 || dlTasks > 0 {
					fmt.Fprintf(&b, "\nEvicted subagent tasks (never charged, daemon-wide): %d task(s), %d tokens — evicted before any sweep charged them; shown separately so they are neither lost nor double-counted.", dlTasks, dlTokens)
				}
			}
			return b.String(), nil
		},
	})

	r.Register(Command{
		Name:        "budget",
		Description: "Show or set the daily token budget (owner-only)",
		Handler:     budgetHandler,
	})

	// /বাজেট is the Bengali alias for /budget: the same handler,
	// the same owner check. It is deliberately NOT advertised in
	// the Telegram "/" menu — BotCommands only lists names the
	// Bot API accepts ([a-z0-9_]), so a Bengali name there would
	// fail setMyCommands on every startup; Dispatch still accepts
	// it, and /help lists it.
	r.Register(Command{
		Name:        "বাজেট",
		Description: "বাজেট দেখুন বা বদলান (owner-only; /budget-এর বাংলা নাম)",
		Handler:     budgetHandler,
	})

	r.Register(Command{
		Name:        "undo",
		Description: "Back up N user turns and re-prompt (default 1)",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			n, msg := parseCount(args, 1)
			if msg != "" {
				return "Usage: /undo [N] — " + msg, nil
			}
			if n < 1 {
				return "Usage: /undo [N] — N must be at least 1.", nil
			}
			undone, err := env.Undo(env.ChatID, n)
			if err != nil {
				return fmt.Sprintf("❌ %v", err), nil
			}
			var b strings.Builder
			fmt.Fprintf(&b, "↩️ Undid %d turn(s):\n", len(undone))
			for _, u := range undone {
				fmt.Fprintf(&b, "  • %s\n", truncateShort(strings.TrimSpace(u), 80))
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	})

	r.Register(Command{
		Name:        "retry",
		Description: "Retry the last message (resend to agent)",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			reply, stats, err := env.Retry(ctx, env.ChatID)
			if err != nil {
				return fmt.Sprintf("❌ %v", err), nil
			}
			if stats != nil && env.Session(env.ChatID).FooterEnabled {
				reply += "\n\n" + FormatFooter(stats)
			}
			return reply, nil
		},
	})

	r.Register(Command{
		Name:        "pause",
		Description: "Pause new work in this chat; '/pause off' resumes",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			switch strings.ToLower(strings.TrimSpace(args)) {
			case "", "on":
				env.Pause(true)
				return "⏸ Paused — new messages will get a paused reply until /pause off. Commands still work.", nil
			case "off":
				env.Pause(false)
				return "▶️ Resumed — new work will run again.", nil
			default:
				return "Usage: /pause [on|off]", nil
			}
		},
	})

	r.Register(Command{
		Name:        "agents",
		Description: "Show active agents and running tasks",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			return "🤖 " + env.Agents(), nil
		},
	})

	r.Register(Command{
		Name:        "title",
		Description: "Set a title for the current session",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			chatID := env.ChatID
			title := strings.TrimSpace(args)
			if title == "" {
				if cur := env.Session(chatID).Title; cur != "" {
					return fmt.Sprintf("Session title: %s", cur), nil
				}
				return "This session has no title yet. Use /title <name> to set one.", nil
			}
			if err := env.SetTitle(chatID, title); err != nil {
				return "", err
			}
			return fmt.Sprintf("✏️ Session title set to %q.", title), nil
		},
	})

	r.Register(Command{
		Name:        "compress",
		Description: "Compress conversation context (summary draws on session memory when available)",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			keep, msg := parseCount(args, 10)
			if msg != "" {
				return "Usage: /compress [N] — " + msg + " N is how many recent messages to keep (default 10).", nil
			}
			if keep < 1 {
				return "Usage: /compress [N] — N must be at least 1.", nil
			}
			// Session memory feeds the summary so compression
			// preserves decisions/facts from session memory, not
			// just the raw messages. Best-effort: a read failure
			// never blocks compression, it just falls back to
			// messages only (and says so). The session-memory
			// variant is used only when it is wired and there is
			// session memory to draw on.
			sessionMem := ""
			sessionNote := ""
			if env.CompressWithSessionMemory != nil && env.SessionMemory != nil {
				sm, err := env.SessionMemory(ctx)
				if err != nil {
					sessionNote = " (Note: session memory could not be read, so the summary drew on messages only.)"
				} else {
					sessionMem = strings.TrimSpace(sm)
				}
			}
			if sessionMem != "" {
				before, after, summaryLen, err := env.CompressWithSessionMemory(ctx, env.ChatID, keep, sessionMem)
				if err != nil {
					return "", err
				}
				if before <= after {
					return fmt.Sprintf("Nothing old enough to compress — only %d message(s) in history (recent window is %d).", before, keep), nil
				}
				return fmt.Sprintf("🗜 Compressed: %d → %d messages. Summary (%d chars) saved as a memory fact, drawing on session memory.", before, after, summaryLen), nil
			}
			before, after, summaryLen, err := env.Compress(ctx, env.ChatID, keep)
			if err != nil {
				return "", err
			}
			if before <= after {
				return fmt.Sprintf("Nothing old enough to compress — only %d message(s) in history (recent window is %d).", before, keep), nil
			}
			return fmt.Sprintf("🗜 Compressed: %d → %d messages. Summary (%d chars) saved as a memory fact.%s", before, after, summaryLen, sessionNote), nil
		},
	})

	r.Register(Command{
		Name:        "save",
		Description: "Export the current conversation",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			path, count, err := env.SaveConversation(env.ChatID)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("💾 Saved %d message(s) to %s", count, path), nil
		},
	})

	r.Register(Command{
		Name:        "restart",
		Description: "Gracefully restart the gateway after draining active runs",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			env.Restart()
			return "🔄 Restarting… Active runs drain first (up to 30s); if no supervisor (systemd) is running the daemon, start oriond again manually.", nil
		},
	})

	// /rules (F-A17-6): the owner-only migration path for legacy
	// global rules. Rules written before rc22 carry no chat, so
	// they apply to every chat forever — and until this command
	// there was no way to list or delete one. /rules lists them
	// (global rules flagged as legacy), /rules delete <tool>
	// deletes a legacy global rule, /rules delete <chat>:<tool>
	// deletes one chat's rule, and /rules scope <tool> <chat>
	// moves a global rule onto a single chat. Owner-only like
	// /restart: the check lives in the handler (isOwner — chat or
	// sender, F-A18-10), so it is testable through Dispatch with
	// a stub env; a nil IsOwner still means the rules wiring is
	// absent and the command reports unavailability.
	r.Register(Command{
		Name:        "rules",
		Description: "Owner-only: list permission rules; '/rules delete <tool>' deletes a legacy global rule, '/rules delete <chat>:<tool>' deletes a per-chat rule, '/rules scope <tool> <chat>' moves a global rule to one chat",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			const usage = "Usage: /rules — list rules; /rules delete <tool> — delete a legacy global rule; /rules delete <chat>:<tool> — delete a per-chat rule; /rules scope <tool> <chat> — move a global rule to one chat."
			if env.IsOwner == nil {
				return "Rules are unavailable (not wired).", nil
			}
			// Owner-only (F-A18-10): isOwner checks the chat OR
			// the sender from ctx, so the owner sending from a
			// group chat is recognised — the old IsOwner(ChatID)
			// check refused them even though they are the owner.
			if !env.isOwner(ctx) {
				return "⛔ /rules is owner-only. Only the owner can list, delete, or scope permission rules.", nil
			}
			fields := strings.Fields(args)
			if len(fields) == 0 {
				if env.ListRules == nil {
					return "Rules are unavailable (not wired).", nil
				}
				rules := env.ListRules()
				if len(rules) == 0 {
					return "No standing permission rules. Gated tools will prompt every time until you tap “Always” or “Never” on an approval card.", nil
				}
				var b strings.Builder
				b.WriteString("📜 Permission rules:\n")
				for _, r := range rules {
					scope := fmt.Sprintf("chat %d", r.ChatID)
					if r.ChatID == 0 {
						scope = "GLOBAL (legacy — applies to every chat)"
					}
					fmt.Fprintf(&b, "• %s — %s — %s\n", r.Tool, r.Action, scope)
				}
				b.WriteString("Delete a legacy global rule with /rules delete <tool>; move one to a single chat with /rules scope <tool> <chat>.")
				return strings.TrimRight(b.String(), "\n"), nil
			}
			switch strings.ToLower(fields[0]) {
			case "delete":
				if len(fields) != 2 {
					return usage, nil
				}
				if env.DeleteRule == nil {
					return "Rules are unavailable (not wired).", nil
				}
				target := fields[1]
				chatID := int64(0)
				tool := target
				if i := strings.Index(target, ":"); i > 0 {
					id, err := strconv.ParseInt(target[:i], 10, 64)
					if err != nil || strings.TrimSpace(target[i+1:]) == "" {
						return usage, nil
					}
					chatID, tool = id, target[i+1:]
				}
				if err := env.DeleteRule(chatID, tool); err != nil {
					return fmt.Sprintf("❌ Could not delete the rule: %v", err), nil
				}
				if chatID == 0 {
					return fmt.Sprintf("🗑 Deleted the legacy global rule for %q. It no longer applies to any chat.", tool), nil
				}
				return fmt.Sprintf("🗑 Deleted the rule for %q in chat %d.", tool, chatID), nil
			case "scope":
				if len(fields) != 3 {
					return usage, nil
				}
				if env.ListRules == nil || env.SetRule == nil || env.DeleteRule == nil {
					return "Rules are unavailable (not wired).", nil
				}
				tool := fields[1]
				chatID, err := strconv.ParseInt(fields[2], 10, 64)
				if err != nil || chatID == 0 {
					return "Usage: /rules scope <tool> <chat> — <chat> must be a non-zero chat id.", nil
				}
				var action permissions.Action
				found := false
				for _, r := range env.ListRules() {
					if r.ChatID == 0 && r.Tool == tool {
						action, found = r.Action, true
					}
				}
				if !found {
					return fmt.Sprintf("No legacy global rule for %q to scope.", tool), nil
				}
				if err := env.SetRule(chatID, tool, action); err != nil {
					return fmt.Sprintf("❌ Could not scope the rule: %v", err), nil
				}
				if err := env.DeleteRule(0, tool); err != nil {
					return fmt.Sprintf("⚠️ The rule is now set for chat %d, but the legacy global rule could not be deleted: %v — it still applies to every chat, so delete it with /rules delete %s.", chatID, err, tool), nil
				}
				return fmt.Sprintf("✅ Moved the %q rule (%s) from global to chat %d only. Other chats will be prompted again for %q.", tool, action, chatID, tool), nil
			default:
				return usage, nil
			}
		},
	})

	r.Register(Command{
		Name:        "whoami",
		Description: "Show your slash-command access (admin / user)",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			return env.Whoami(env.ChatID), nil
		},
	})

	r.Register(Command{
		Name:        "footer",
		Description: "Toggle the runtime footer on final replies (includes memory usage when memories were used)",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			chatID := env.ChatID
			switch strings.ToLower(strings.TrimSpace(args)) {
			case "":
				state := onOff(env.Session(chatID).FooterEnabled)
				return fmt.Sprintf("Footer: %s.\nThe footer is the line appended to final replies: “— <model> · <tokens> · <steps> · <duration>”, plus “· <N> memories” when typed memories were used in that turn (e.g. “· 3 memories”; omitted when none were used).\nUse /footer on or /footer off to change it.", state), nil
			case "on", "off":
				on := args == "on"
				if err := env.SetFooter(chatID, on); err != nil {
					return "", err
				}
				return fmt.Sprintf("Footer: %s.", onOff(on)), nil
			default:
				return "Usage: /footer [on|off]", nil
			}
		},
	})

	r.Register(Command{
		Name:        "memory",
		Description: "Review memory: facts and typed memories; '/memory forget N' deletes a fact, '/memory forget <name>' deletes a typed memory, '/memory feedback <name> :: <rule> :: <why> :: <how>' saves standing guidance",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			fields := strings.Fields(args)
			if len(fields) >= 2 && strings.EqualFold(fields[0], "feedback") {
				// Explicit user confirmation is the only way a
				// feedback memory is created (Audit 4 A P1-3):
				// the model cannot save feedback, and the store
				// refuses non-user feedback. Parts are separated
				// by "::" so rules may contain spaces.
				if env.SaveFeedbackMemory == nil {
					return "Feedback saving is not wired up yet — the memory store does not expose it.", nil
				}
				rest := strings.TrimSpace(args[len(fields[0]):])
				parts := strings.Split(rest, "::")
				if len(parts) != 4 {
					return "Usage: /memory feedback <name> :: <rule> :: <why> :: <how> — all four parts are required.", nil
				}
				for i := range parts {
					parts[i] = strings.TrimSpace(parts[i])
					if parts[i] == "" {
						return "Usage: /memory feedback <name> :: <rule> :: <why> :: <how> — all four parts are required.", nil
					}
				}
				name, err := env.SaveFeedbackMemory(parts[0], parts[1], parts[2], parts[3])
				if err != nil {
					return fmt.Sprintf("❌ %v", err), nil
				}
				return fmt.Sprintf("✅ Saved feedback memory %q. It will be injected as standing guidance on future turns.", name), nil
			}
			if len(fields) >= 2 && strings.EqualFold(fields[0], "forget") {
				// A number always refers to the fact list (as
				// before); anything else is a typed-memory name.
				if n, err := strconv.Atoi(fields[1]); err == nil {
					if n < 1 {
						return "Usage: /memory forget N — N is the fact number from /memory.", nil
					}
					key, err := env.ForgetFact(n)
					if err != nil {
						return fmt.Sprintf("❌ %v", err), nil
					}
					return fmt.Sprintf("🗑 Forgot fact %d (%s).", n, key), nil
				}
				name := strings.Join(fields[1:], " ")
				if env.ForgetTypedMemory == nil {
					return "Typed-memory forgetting is not wired up yet — the memory store does not expose it. /memory forget N still works for facts.", nil
				}
				deleted, err := env.ForgetTypedMemory(name)
				if err != nil {
					return fmt.Sprintf("❌ %v", err), nil
				}
				return fmt.Sprintf("🗑 Forgot typed memory %q.", deleted), nil
			}
			if len(fields) > 0 {
				return "Usage: /memory — review memory; /memory forget N — delete fact N; /memory forget <name> — delete the typed memory named <name>; /memory feedback <name> :: <rule> :: <why> :: <how> — save standing guidance.", nil
			}
			info, err := env.MemoryInfo()
			if err != nil {
				return "", err
			}
			// Typed memories (workstream A) render alongside the
			// facts, grouped by type with ages. Nil/unwired or
			// empty means facts only, exactly as before; a list
			// failure degrades to a note, it never hides the
			// facts.
			if env.TypedMemories != nil {
				typed, terr := env.TypedMemories()
				if terr != nil {
					info += fmt.Sprintf("\n(Typed memories could not be listed: %v)", terr)
				} else if len(typed) > 0 {
					info += "\n\n" + renderTypedMemories(typed, time.Now())
				}
			}
			return info, nil
		},
	})

	r.Register(Command{
		Name:        "skills",
		Description: "Reload skills from the skills directory ('/skills reload')",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			if strings.ToLower(strings.TrimSpace(args)) != "reload" {
				return "Usage: /skills reload — re-reads the skills directory (picks up new, changed and removed skill files) without restarting the daemon.", nil
			}
			return skillsReloadHandler(ctx, env, args)
		},
	})

	return r
}

// onOff renders a bool as "on"/"off".
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
