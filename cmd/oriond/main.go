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
	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/messaging"
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
	gw, err := messaging.NewTelegramGateway(a.Config.TelegramToken,
		messaging.WithAllowedChats(a.Config.TelegramAllowedChats...),
		// Polling-health notes (transient getUpdates failures and
		// the recovery after them) go to stderr, where the rest of
		// the daemon's lifecycle logging already goes. Without this
		// a network outage is invisible in the log: the process
		// looks alive while polling is down.
		messaging.WithLogger(func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "oriond: "+format+"\n", args...)
		}))
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
	env := buildCommandEnv(a, store, username, allowed, restart, retry)
	registry := messaging.DefaultRegistry(env)
	// A menu failure must not kill the daemon: log and keep polling.
	if err := gw.SetMyCommands(ctx, registry.BotCommands()); err != nil {
		fmt.Fprintf(os.Stderr, "oriond: warning: setMyCommands failed (the / menu will be stale): %v\n", err)
	}
	turnFn := buildTurnHandler(a, store, registry, env)

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

// runTurn runs one real agent turn for a chat: it records the input
// for /retry, registers the turn's cancel function so /stop can
// cancel it, runs HandleStream (the emit is the terminal-card feed),
// and records the turn's token usage in the session — including on
// error, since a failed turn still consumed tokens.
func runTurn(a *app.App, store *messaging.SessionStore, ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
	if a == nil {
		return "", nil, errors.New("oriond: app is nil")
	}
	runCtx, cancel := context.WithCancel(ctx)
	registered := a.RegisterRun(chatID, runCtx, cancel)
	if registered {
		defer a.UnregisterRun(chatID, runCtx)
	}
	defer cancel()
	if store != nil {
		store.SetLastInput(chatID, text)
	}
	reply, stats, err := a.HandleStream(runCtx, text, emit)
	if stats != nil && store != nil {
		// Best effort: a failed usage write must never fail the
		// turn that just completed.
		_ = store.AddUsage(chatID, stats.Tokens)
	}
	if err != nil {
		return "", nil, err
	}
	return reply, stats, nil
}

// buildTurnHandler builds the PollStream TurnFunc: slash commands are
// dispatched (no agent run, no footer, no usage recorded); paused
// daemons answer non-command messages with the paused reply; anything
// else runs the agent with the footer gated on the chat's preference
// (read once at turn start). A turn cancelled by /stop returns no
// reply at all — the /stop command already told the chat — instead
// of a second "Sorry, I hit an error" message.
func buildTurnHandler(a *app.App, store *messaging.SessionStore, registry *messaging.Registry, env *messaging.CommandEnv) messaging.TurnFunc {
	return func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		if registry != nil && env != nil {
			// Per-turn copy of the env with this chat's ID: the
			// shared env must never carry mutable per-chat state
			// (PollStream handles chats concurrently).
			turnEnv := *env
			turnEnv.ChatID = chatID
			if reply, handled, err := registry.Dispatch(ctx, &turnEnv, text); handled {
				return reply, nil, err
			}
		}
		if store != nil && store.IsPaused() {
			return "⏸ Orion is paused. Send /pause off to resume.", nil, nil
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
func buildCommandEnv(a *app.App, store *messaging.SessionStore, botUsername string, allowed map[int64]bool, restart func(), retry func(context.Context, int64) (string, *core.TurnStats, error)) *messaging.CommandEnv {
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
		Session: store.Get,
		NewSession: func(chatID int64) error {
			if err := a.NewSession(); err != nil {
				return err
			}
			store.Reset(chatID)
			return nil
		},
		StopRun: a.StopRun,
		Undo: func(chatID int64, n int) ([]string, error) {
			undone, err := a.UndoLastTurns(n)
			if err != nil {
				return nil, err
			}
			store.PushUndo(chatID, undone...)
			return undone, nil
		},
		Retry:    retry,
		Pause:    func(on bool) { _ = store.SetPaused(on) },
		IsPaused: store.IsPaused,
		Agents:   a.AgentsSummary,
		SetTitle: store.SetTitle,
		Compress: func(ctx context.Context, chatID int64, keepRecent int) (int, int, int, error) {
			return a.Compress(ctx, keepRecent)
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
			return a.CompressWithSessionMemory(ctx, keepRecent, sessionMemory)
		},
	}
}
