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
//  3. Add a test in commands_test.go (Dispatch the command through a
//     stub env and assert the reply and the side effect).
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

	"github.com/irfan/orion/internal/core"
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
	// effective next turn) or returns an error naming the problem.
	SwitchProvider func(name string) error
	// Usage returns daemon-wide token/turn totals (all chats).
	Usage func() (tokens int, turns int)
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
	// Restart signals the daemon to drain active runs and exit 0
	// (a supervisor restarts it).
	Restart func()
	// Whoami renders the access line for the chat.
	Whoami func(chatID int64) string
	// SetFooter persists the per-chat footer preference.
	SetFooter func(chatID int64, on bool) error
	// MemoryInfo renders the memory review.
	MemoryInfo func() (string, error)
	// ForgetFact deletes fact number index (1-based) and returns
	// the deleted key.
	ForgetFact func(index int) (string, error)
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
func (r *Registry) BotCommands() []BotCommand {
	cmds := r.Commands()
	out := make([]BotCommand, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, BotCommand{Name: c.Name, Description: c.Description})
	}
	return out
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

// DefaultRegistry returns a registry with the 17 core commands. env
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
		Description: "Show or switch model (switches the daemon default)",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			provider, modelName, providers := env.ModelInfo()
			args = strings.TrimSpace(args)
			if args == "" {
				return fmt.Sprintf("Current: %s / %s\nAvailable providers: %s\n\n/model <name> switches the daemon default provider (persisted, effective from the next turn, for all chats). Session-scoped switching is not implemented yet.",
					provider, modelName, strings.Join(providers, ", ")), nil
			}
			name := strings.ToLower(strings.Fields(args)[0])
			if err := env.SwitchProvider(name); err != nil {
				return fmt.Sprintf("❌ %v\nAvailable providers: %s", err, strings.Join(providers, ", ")), nil
			}
			provider, modelName, _ = env.ModelInfo()
			return fmt.Sprintf("✅ Switched to %s / %s. This switches the daemon default — it applies to all chats from the next turn.", provider, modelName), nil
		},
	})

	r.Register(Command{
		Name:        "usage",
		Description: "Show token usage and rate limits",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			sess := env.Session(env.ChatID)
			tokens, turns := env.Usage()
			maxSteps, timeout := env.Limits()
			return fmt.Sprintf("Tokens this chat: %d (%d turn(s))\nTokens (all chats): %d (%d turn(s))\nMax steps per turn: %d\nTimeout per step: %ds\nRate limits are set by the provider — Orion does not impose its own.",
				sess.TotalTokens, sess.Turns, tokens, turns, maxSteps, timeout), nil
		},
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
		Description: "Pause new work globally; '/pause off' resumes",
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
		Description: "Compress conversation context",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			keep, msg := parseCount(args, 10)
			if msg != "" {
				return "Usage: /compress [N] — " + msg + " N is how many recent messages to keep (default 10).", nil
			}
			if keep < 1 {
				return "Usage: /compress [N] — N must be at least 1.", nil
			}
			before, after, summaryLen, err := env.Compress(ctx, env.ChatID, keep)
			if err != nil {
				return "", err
			}
			if before <= after {
				return fmt.Sprintf("Nothing old enough to compress — only %d message(s) in history (recent window is %d).", before, keep), nil
			}
			return fmt.Sprintf("🗜 Compressed: %d → %d messages. Summary (%d chars) saved as a memory fact.", before, after, summaryLen), nil
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
			return "🔄 Restarting… Active runs drain first (up to 10s); if no supervisor (systemd) is running the daemon, start oriond again manually.", nil
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
		Description: "Toggle the runtime footer on final replies",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			chatID := env.ChatID
			switch strings.ToLower(strings.TrimSpace(args)) {
			case "":
				state := onOff(env.Session(chatID).FooterEnabled)
				return fmt.Sprintf("Footer: %s.\nThe footer is the line appended to final replies: “— <model> · <tokens> · <steps> · <duration>”.\nUse /footer on or /footer off to change it.", state), nil
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
		Description: "Review memory: facts, messages; '/memory forget N' deletes a fact",
		Handler: func(ctx context.Context, env *CommandEnv, args string) (string, error) {
			fields := strings.Fields(args)
			if len(fields) >= 2 && strings.EqualFold(fields[0], "forget") {
				n, err := strconv.Atoi(fields[1])
				if err != nil || n < 1 {
					return "Usage: /memory forget N — N is the fact number from /memory.", nil
				}
				key, err := env.ForgetFact(n)
				if err != nil {
					return fmt.Sprintf("❌ %v", err), nil
				}
				return fmt.Sprintf("🗑 Forgot fact %d (%s).", n, key), nil
			}
			if len(fields) > 0 {
				return "Usage: /memory — review memory; /memory forget N — delete fact N.", nil
			}
			info, err := env.MemoryInfo()
			if err != nil {
				return "", err
			}
			return info, nil
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
