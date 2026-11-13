// Command oriond is Orion's daemon: it wires the app and serves the
// Telegram gateway until interrupted. CLI mode lives in cmd/orion.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/irfan/orion/internal/app"
	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/messaging"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func usage() {
	fmt.Printf(`oriond — Orion daemon (v%s)

Usage:
  oriond                  run Telegram polling until interrupted
  oriond --help           show this help
  oriond --version        print version

Environment:
  ORION_TELEGRAM_BASE_URL override the Telegram Bot API endpoint
                          (default https://api.telegram.org); for
                          self-hosted Bot API servers and local tests
`, version.Version)
}

// run executes the daemon and returns the process exit code, so the
// argument handling is testable without spawning a process.
func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "--help", "-h":
			if len(args) > 1 {
				fmt.Fprintln(os.Stderr, "oriond: --help takes no arguments")
				return 2
			}
			usage()
			return 0
		case "--version", "-v", "version":
			if len(args) > 1 {
				fmt.Fprintln(os.Stderr, "oriond: --version takes no arguments")
				return 2
			}
			fmt.Printf("oriond %s (go, model-agnostic, telegram-only)\n", version.String())
			return 0
		default:
			fmt.Fprintf(os.Stderr, "oriond: unknown argument %q\n", args[0])
			usage()
			return 2
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.Build("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "oriond: %v\n", err)
		return 1
	}
	defer a.Close()

	if a.Config.TelegramToken == "" {
		fmt.Fprintln(os.Stderr, "oriond: no telegram token configured (set ORION_TELEGRAM_TOKEN or config telegram_token)")
		return 1
	}
	// ORION_TELEGRAM_BASE_URL overrides the Bot API endpoint (default
	// https://api.telegram.org). It exists for self-hosted Bot API
	// servers, and it is what lets the daemon be exercised end to end
	// against a local fake Bot API without a real token or network
	// (see TestDaemonStartStopRestart in main_audit3_test.go). It
	// never carries a secret — only the endpoint — so it is safe to
	// set in a test or staging environment.
	if baseURL := os.Getenv("ORION_TELEGRAM_BASE_URL"); baseURL != "" {
		messaging.SetDefaultBaseURL(baseURL)
	}
	// P2-O5: LOUD warning when the allow-all opt-in is set.
	if w := a.Config.AllowAllTelegramWarning(); w != "" {
		fmt.Fprintf(os.Stderr, "oriond: %s\n", w)
	}
	gwOpts := []messaging.GatewayOption{
		messaging.WithAllowedChats(a.Config.TelegramAllowedChats...),
		// Polling-health notes (transient getUpdates failures and
		// the recovery after them) go to stderr, where the rest of
		// the daemon's lifecycle logging already goes. Without this
		// a network outage is invisible in the log: the process
		// looks alive while polling is down.
		messaging.WithLogger(func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "oriond: "+format+"\n", args...)
		}),
	}
	if a.Config.AllowAllTelegramChats {
		// P2-O5: the opt-in is time-boxed — IsChatAllowed refuses
		// every chat once the window (from daemon start) elapses.
		gwOpts = append(gwOpts, messaging.WithAllowAll(a.Config.AllowAllWindow()))
	}
	gw, err := messaging.NewTelegramGateway(a.Config.TelegramToken, gwOpts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "oriond: %v\n", err)
		return 1
	}
	if err := messaging.CheckTelegramAllowlist(a.Config.TelegramAllowedChats, a.Config.AllowAllTelegramChats); err != nil {
		fmt.Fprintf(os.Stderr, "oriond: %v\n", err)
		return 1
	}
	username, err := gw.GetMe(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "oriond: telegram getMe: %v\n", err)
		return 1
	}
	active, err := a.Config.ActiveProvider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "oriond: %v\n", err)
		return 1
	}
	fmt.Printf("oriond running as @%s (provider %s, model %s). Ctrl+C to stop.\n",
		username, a.Provider.Name(), active.Model)

	// --- Phase C: slash-command framework -------------------------
	// Sessions persist under the data dir; the footer default comes
	// from config (telegram_footer nil = on).
	footerDefault := true
	if a.Config.TelegramFooter != nil {
		footerDefault = *a.Config.TelegramFooter
	}
	store := messaging.NewSessionStore(filepath.Join(a.Config.DataDir, "sessions.json"), footerDefault)
	if err := store.LoadError(); err != nil {
		fmt.Fprintf(os.Stderr, "oriond: sessions file unusable, starting with defaults: %v\n", err)
	}
	// Persist sessions on every exit path (SIGTERM, /restart, fatal
	// poll error): mutations already save immediately, but a daemon
	// that ran with zero turns would otherwise leave no sessions
	// file at all, and the next start could not tell "no sessions
	// yet" from "sessions lost". Best-effort: a failed save is
	// logged, never a reason to change the exit code.
	defer func() {
		if err := store.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "oriond: save sessions on shutdown: %v\n", err)
		}
	}()

	// /restart signals here (buffered, so the signal is never lost
	// when nothing is selecting yet). The signal is delayed 500ms so
	// PollStream can send the "Restarting…" reply before the run
	// loop exits and cancels the polling context.
	restartCh := make(chan struct{}, 1)
	restart := func() {
		go func() {
			time.Sleep(500 * time.Millisecond)
			select {
			case restartCh <- struct{}{}:
			default:
			}
		}()
	}

	// --- P1-O2 permission gate + P1-O4 owner tier -------------------
	// The owner is the FIRST allowlisted chat (see
	// messaging.OwnerIDFromAllowlist): the operator who installed
	// the bot. Daemon-wide operations (/restart) are owner-only;
	// per-chat operations are safe for any allowlisted chat once
	// isolated (P1-O3). There is deliberately no config field for
	// the owner — the allowlist already names them.
	ownerID := messaging.OwnerIDFromAllowlist(a.Config.TelegramAllowedChats)
	if ownerID != 0 {
		fmt.Fprintf(os.Stderr, "oriond: owner chat %d (first telegram_allowed_chats entry); /restart is owner-only\n", ownerID)
	} else {
		fmt.Fprintln(os.Stderr, "oriond: no owner (empty allowlist with allow-all opt-in); /restart is refused for everyone")
	}
	// Permission gate (P1-O2) + approval cards: built through the
	// shared constructor (app.NewPermissionGate, F-A7-P2-2) with
	// the shared Telegram prompter/expiry hook from
	// internal/messaging — the same wiring `orion daemon` and
	// `orion telegram poll` get from wireGate, so the three
	// Telegram loops cannot drift apart again. Rules persist
	// under the data dir (permissions.json, 0600); a load error
	// is logged by NewPermissionGate and the gate starts with no
	// rules rather than refusing startup. See buildApprovalGate.
	gate, cards, err := buildApprovalGate(a, gw, ownerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "oriond: %v\n", err)
		return 1
	}
	messaging.InstallApprovalCallbackHandler(gw, gate, ownerID, cards, "oriond")

	allowed := make(map[int64]bool, len(a.Config.TelegramAllowedChats))
	for _, id := range a.Config.TelegramAllowedChats {
		allowed[id] = true
	}
	retry := func(ctx context.Context, chatID int64) (string, *core.TurnStats, error) {
		last := store.Get(chatID).LastUserInput
		if last == "" {
			return "", nil, errors.New("nothing to retry yet — send a message first")
		}
		return runTurn(a, store, ctx, chatID, last, nil)
	}
	env := buildCommandEnv(a, store, username, allowed, ownerID, restart, retry)
	registry := messaging.DefaultRegistry(env)
	// A menu failure must not kill the daemon: log and keep polling.
	if err := gw.SetMyCommands(ctx, registry.BotCommands()); err != nil {
		fmt.Fprintf(os.Stderr, "oriond: warning: setMyCommands failed (the / menu will be stale): %v\n", err)
	}
	turnFn := buildTurnHandler(a, store, registry, env, ownerID, username)

	pollErr := make(chan error, 1)
	go func() { pollErr <- gw.PollStream(ctx, turnFn) }()
	select {
	case err := <-pollErr:
		if err != nil {
			fmt.Fprintf(os.Stderr, "oriond: %v\n", err)
			return 1
		}
		return 0
	case <-restartCh:
		fmt.Fprintln(os.Stderr, "oriond: restart requested; draining active runs (up to 10s)...")
		if !a.Drain(10 * time.Second) {
			fmt.Fprintln(os.Stderr, "oriond: restart: some runs still active after 10s; exiting anyway")
		}
		// Exit 0 so a supervisor (systemd, oriond.service with
		// Restart=on-success) restarts the daemon. Without a
		// supervisor the daemon simply exits and must be started
		// manually — see /restart's reply and ARCHITECTURE.md.
		return 0
	case <-ctx.Done():
		// SIGINT/SIGTERM: let in-flight turns unwind and persist
		// before Close saves and closes the memory store — the
		// /restart path drains the same way. Exiting immediately
		// could close the store under a turn that is still
		// recording its last step. Turns are already cancelled
		// (their contexts derive from ctx), so this is prompt
		// unless a run is stuck in a non-cancellable operation;
		// the 10s bound keeps shutdown from hanging either way.
		if !a.Drain(10 * time.Second) {
			fmt.Fprintln(os.Stderr, "oriond: shutdown: some runs still active after 10s; exiting anyway")
		}
		return 0
	}
}

// buildApprovalGate wires the permission gate for the daemon
// through the shared constructor (app.NewPermissionGate, F-A7-P2-2)
// with the shared Telegram prompter and expiry hook from
// internal/messaging — the same wiring `orion daemon` and `orion
// telegram poll` get from wireGate. It returns the gate and the
// card tracker; the callback handler is installed by the caller
// (run installs it right after; tests may install their own
// logging wrapper instead). A nil gate is never returned on
// success. Extracted from run() so the live-shape tests exercise
// exactly the daemon's wiring, not a copy of it.
func buildApprovalGate(a *app.App, gw *messaging.TelegramGateway, ownerID int64) (*permissions.Gate, *messaging.ApprovalCards, error) {
	cards := messaging.NewApprovalCards()
	gate, err := a.NewPermissionGate(messaging.ApprovalPrompter(gw, cards, "oriond"))
	if err != nil {
		return nil, nil, err
	}
	messaging.InstallApprovalExpiry(gate, gw, cards, "oriond")
	return gate, cards, nil
}

// runTurn runs one real agent turn for a chat: it records the input
// for /retry, registers the turn's cancel function so /stop can
// cancel it, runs HandleStreamForChat (the emit is the terminal-card feed),
// and records the turn's token usage in the session — including on
// error, since a failed turn still consumed tokens.
func runTurn(a *app.App, store *messaging.SessionStore, ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
	if a == nil {
		return "", nil, errors.New("oriond: app is nil")
	}
	runCtx, cancel := context.WithCancel(ctx)
	// The turn runs on the CHAT's agent (P1-O3) with the approval
	// context the permission gate reads (P1-O2): the chat the
	// prompt goes to, and the sender who may answer it (P1-O4).
	// The wrapped ctx is also the registered one, so /stop cancels
	// exactly this turn.
	senderID, _ := messaging.SenderIDFromContext(ctx)
	runCtx = permissions.WithApprovalContext(runCtx, chatID, senderID)
	registered := a.RegisterRun(chatID, runCtx, cancel)
	if registered {
		defer a.UnregisterRun(chatID, runCtx)
	}
	defer cancel()
	if store != nil {
		store.SetLastInput(chatID, text)
	}
	// P2-O9 per-chat daily token budget: enforced inside
	// HandleStreamForChat (app.runChatTurn) — the single enforcement
	// point every channel shares, so the check and the post-turn
	// record cannot drift apart or double-count. A budget refusal
	// arrives here as an error wrapping budget.ErrBudgetExceeded
	// and is returned as a plain reply (not a "Sorry, I hit an
	// error" handler error): the turn was refused on purpose, and
	// the message names the usage, the limit and the UTC-midnight
	// reset.
	reply, stats, err := a.HandleStreamForChat(runCtx, chatID, text, emit)
	if err != nil && errors.Is(err, budget.ErrBudgetExceeded) {
		return fmt.Sprintf("⏸ Daily token budget reached for this chat: %v", err), nil, nil
	}
	if stats != nil && store != nil {
		// Best effort: a failed usage write must never fail the
		// turn that just completed. (The budget record itself
		// already happened inside HandleStreamForChat.)
		_ = store.AddUsage(chatID, stats.Tokens)
	}
	if err != nil {
		return "", nil, err
	}
	return reply, stats, nil
}

// buildTurnHandler builds the PollStream TurnFunc: slash commands are
// dispatched (no agent run, no footer, no usage recorded); paused
// chats answer non-command messages with the paused reply; anything
// else runs the chat's agent with the footer gated on the chat's preference
// (read once at turn start). A turn cancelled by /stop returns no
// reply at all — the /stop command already told the chat — instead
// of a second "Sorry, I hit an error" message.
//
// Per-chat overrides (P1-O3/P1-O4): the shared env holds
// daemon-default fallbacks, but every chat-scoped capability is
// rebound per turn here, so a command can only ever act on the
// caller's chat: /model switches only the caller's provider, /status
// and /memory report the caller's state, /pause pauses only the
// caller's chat. /restart is owner-only and is intercepted here
// before Dispatch — the shared env's Restart cannot refuse (it
// returns nothing), so the check lives at this layer.
func buildTurnHandler(a *app.App, store *messaging.SessionStore, registry *messaging.Registry, env *messaging.CommandEnv, ownerID int64, botUsername string) messaging.TurnFunc {
	return func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		senderID, _ := messaging.SenderIDFromContext(ctx)
		if registry != nil && env != nil {
			// Per-turn copy of the env with this chat's ID: the
			// shared env must never carry mutable per-chat state
			// (PollStream handles chats concurrently).
			turnEnv := *env
			turnEnv.ChatID = chatID
			turnEnv.SwitchProvider = func(name string) error {
				return a.SwitchProviderForChat(chatID, name)
			}
			turnEnv.ModelInfo = func() (string, string, []string) {
				return a.ModelInfoForChat(chatID)
			}
			turnEnv.Status = func() string { return a.StatusLineForChat(chatID) }
			turnEnv.MemoryInfo = func() (string, error) { return a.MemorySummaryForChat(chatID) }
			turnEnv.SessionMemory = func(ctx context.Context) (string, error) {
				return a.SessionMemoryTextForChat(ctx, chatID)
			}
			turnEnv.Pause = func(on bool) { _ = store.SetPaused(chatID, on) }
			turnEnv.IsPaused = func() bool { return store.IsPaused(chatID) }
			// /restart is daemon-wide (it restarts the daemon for
			// everyone), so it stays owner-only even though
			// /new and /model are per-chat-safe once isolated:
			// the audit's literal "/new owner-only" suggestion
			// is superseded by isolation (a per-chat /new cannot
			// touch another chat), but nothing isolates a
			// restart. A non-owner is refused with a reply, not
			// a handler error.
			if name, _, ok := messaging.Parse(text); ok && name == "restart" {
				if senderID != ownerID {
					return "⛔ /restart is owner-only. Your chat can use every other command, but only the owner can restart the daemon for everyone.", nil, nil
				}
			}
			// Whoami shows the SENDER's role: in a group chat the
			// sender may be the owner even though the chat ID is
			// the group's. (In a private chat the shared env's
			// Whoami already reports owner when chat == owner.)
			if senderID != 0 && senderID == ownerID && senderID != chatID {
				turnEnv.Whoami = func(id int64) string {
					name := botUsername
					if name == "" {
						name = "unknown"
					}
					return fmt.Sprintf("Chat ID: %d\nSender ID: %d\nBot: @%s\nAllowlisted: yes\nRole: owner", id, senderID, name)
				}
			}
			if reply, handled, err := registry.Dispatch(ctx, &turnEnv, text); handled {
				return reply, nil, err
			}
		}
		if store != nil && store.IsPaused(chatID) {
			return "⏸ Orion is paused for this chat. Send /pause off to resume.", nil, nil
		}
		footerOn := true
		if store != nil {
			footerOn = store.Get(chatID).FooterEnabled
		}
		reply, stats, err := runTurn(a, store, ctx, chatID, text, emit)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return "", nil, nil
			}
			return "", nil, err
		}
		if !footerOn {
			stats = nil
		}
		return reply, stats, nil
	}
}

// buildCommandEnv wires every CommandEnv field to its real
// implementation. No field is left nil: a command that calls a nil
// field would panic (recovered by PollStream as a handler error),
// and a silently missing capability would be a stub.
//
// Chat-scoped fields (NewSession, Undo, Compress, SaveConversation)
// take the chat ID and call the per-chat App methods directly
// (P1-O3). Fields whose signatures carry no chat ID
// (SwitchProvider, ModelInfo, Status, MemoryInfo, SessionMemory,
// Pause, IsPaused) are wired here to daemon-default fallbacks and
// rebound per turn by buildTurnHandler — a command dispatched
// outside a turn (never in production) sees the daemon default,
// never another chat's state. Pause/IsPaused have no daemon
// default (pause is per chat since P1-O3), so their shared-env
// fallback addresses chat 0 — no real chat — and exists only so
// the field is never nil; buildTurnHandler always rebinds them.
// ownerID is the first allowlisted chat; Whoami reports "owner"
// for it, "admin" for other allowlisted chats.
func buildCommandEnv(a *app.App, store *messaging.SessionStore, botUsername string, allowed map[int64]bool, ownerID int64, restart func(), retry func(context.Context, int64) (string, *core.TurnStats, error)) *messaging.CommandEnv {
	return &messaging.CommandEnv{
		Status: a.StatusLine,
		ModelInfo: func() (string, string, []string) {
			return a.ProviderName(), a.ActiveModel(), config.KnownProviderNames()
		},
		Limits: func() (int, int) {
			if a.Config == nil {
				return 0, 0
			}
			return a.Config.MaxSteps, a.Config.TimeoutSeconds
		},
		SwitchProvider: a.SwitchProvider,
		Usage: func() (int, int) {
			return store.Totals()
		},
		UsageForChat: a.UsageLineForChat,
		Session:      store.Get,
		NewSession: func(chatID int64) error {
			if err := a.NewSessionForChat(chatID); err != nil {
				return err
			}
			store.Reset(chatID)
			return nil
		},
		StopRun: a.StopRun,
		Undo: func(chatID int64, n int) ([]string, error) {
			undone, err := a.UndoLastTurnsForChat(chatID, n)
			if err != nil {
				return nil, err
			}
			store.PushUndo(chatID, undone...)
			return undone, nil
		},
		Retry:    retry,
		Pause:    func(on bool) { _ = store.SetPaused(0, on) },
		IsPaused: func() bool { return store.IsPaused(0) },
		Agents:   a.AgentsSummary,
		SetTitle: store.SetTitle,
		Compress: func(ctx context.Context, chatID int64, keepRecent int) (int, int, int, error) {
			return a.CompressForChat(ctx, chatID, keepRecent)
		},
		SaveConversation: func(chatID int64) (string, int, error) {
			sess := store.Get(chatID)
			return a.ExportConversation(chatID, sess.Title, sess.TotalTokens, sess.Turns)
		},
		Restart: restart,
		Whoami: func(chatID int64) string {
			role, allowlisted := "user", "no"
			if allowed[chatID] {
				role, allowlisted = "admin", "yes"
			}
			if ownerID != 0 && chatID == ownerID {
				role = "owner"
			}
			name := botUsername
			if name == "" {
				name = "unknown"
			}
			return fmt.Sprintf("Chat ID: %d\nBot: @%s\nAllowlisted: %s\nRole: %s", chatID, name, allowlisted, role)
		},
		SetFooter:  store.SetFooter,
		MemoryInfo: a.MemorySummary,
		ForgetFact: a.ForgetFact,
		// Memory v2: typed memories, session memory, and the
		// /compress variant that draws on session memory. The App
		// methods degrade gracefully when the typed layer is
		// unavailable (hand-built Apps), so no field is left nil.
		TypedMemories:      a.TypedMemories,
		ForgetTypedMemory:  a.ForgetTypedMemory,
		SaveFeedbackMemory: a.SaveFeedbackMemory,
		SessionMemory:      a.SessionMemoryText,
		CompressWithSessionMemory: func(ctx context.Context, chatID int64, keepRecent int, sessionMemory string) (int, int, int, error) {
			return a.CompressWithSessionMemoryForChat(ctx, chatID, keepRecent, sessionMemory)
		},
		// Per-session transcripts (audit P0 #4): /sessions lists
		// this chat's transcripts, /resume restores one. The
		// messaging-local SessionInfo is a field-for-field copy
		// of session.Info, so no conversion can silently drop a
		// field. Resume also resets the chat's turn bookkeeping
		// (undo stack, last input), which belonged to the
		// previous session's history — exactly as /new does.
		ListSessions: func(chatID int64) ([]messaging.SessionInfo, string, error) {
			infos, current, err := a.ListSessionsForChat(chatID)
			if err != nil {
				return nil, "", err
			}
			out := make([]messaging.SessionInfo, 0, len(infos))
			for _, info := range infos {
				out = append(out, messaging.SessionInfo{
					ID: info.ID, CreatedAt: info.CreatedAt, UpdatedAt: info.UpdatedAt,
					Messages: info.Messages, Summaries: info.Summaries,
					Bytes: info.Bytes, Title: info.Title,
				})
			}
			return out, current, nil
		},
		ResumeSession: func(chatID int64, idOrPrefix string) (string, int, error) {
			id, n, err := a.ResumeSessionForChat(chatID, idOrPrefix)
			if err != nil {
				return "", 0, err
			}
			store.Reset(chatID)
			return id, n, nil
		},
		LastResumeReport: func(chatID int64) (messaging.ResumeReport, bool) {
			rep, ok := a.LastResumeReportForChat(chatID)
			if !ok {
				return messaging.ResumeReport{}, false
			}
			return messaging.ResumeReport{
				SessionID:          rep.SessionID,
				TranscriptMessages: rep.TranscriptMessages,
				LoadedMessages:     rep.LoadedMessages,
				CorruptLines:       rep.CorruptLines,
				SummariesLoaded:    rep.SummariesLoaded,
				Capped:             rep.Capped,
			}, true
		},
	}
}
