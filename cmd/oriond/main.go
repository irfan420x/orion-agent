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
	"strings"
	"syscall"
	"time"

	"github.com/irfan/orion/internal/app"
	"github.com/irfan/orion/internal/backup"
	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/debug"
	"github.com/irfan/orion/internal/messaging"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// Daemon exit codes (F-A17-2, F-A17-3). Exit 1 stays the generic
// startup/poll failure; the codes below let a supervisor — and the
// journal — tell an intentional restart and a refused second instance
// apart from a crash.
const (
	// exitRestartRequested (42) is returned only by the /restart
	// path: the owner asked for a restart, so the process must come
	// back. 42 is not a sysexits.h code, so it can never be confused
	// with a standard failure, and it is deliberately NOT 0: with
	// Restart=always a clean exit 0 and a crash counted identically
	// toward the start-limit burst, so an operator's /restart could
	// consume the last restart the burst allowed and strand the
	// unit. See the /restart case in run for the unit lines.
	exitRestartRequested = 42
	// exitAlreadyRunning (2) is returned when another oriond already
	// holds the instance lock (F-A17-3): a second poller would draw
	// a Telegram 409 and flap the good daemon too, so the second
	// instance refuses to start. 2 matches the existing usage-error
	// exit code — a refusal to start, not a crash.
	exitAlreadyRunning = 2
)

// drainTimeout bounds Drain in both shutdown paths of this binary
// (F-A17-17): /restart and SIGTERM/SIGINT both wait this long for the
// cancelled in-flight turns to wind down (finish writing state, not
// finish replying — their contexts are already cancelled, so the
// replies are lost either way). 30s is the same bound
// cmd/orion/main.go pollTelegram uses: it is the F-A10-5 margin that
// keeps a turn from racing transcript/memory writes during teardown,
// the only reasoned bound in the codebase — the 10s oriond used
// before was never justified. The two entrypoints MUST use the same
// value; if it changes here, change it in cmd/orion/main.go too.
const drainTimeout = 30 * time.Second

func usage() {
	fmt.Printf(`oriond — Orion daemon (v%s)

Usage:
  oriond                  run Telegram polling until interrupted
  oriond --help           show this help
  oriond --version        print version
  oriond --restore <snapshot>
                          restore ~/.orion from a backup snapshot and exit
                          (F-A17-13; stop the daemon first — a running
                          daemon would undo the restore — verify the
                          snapshot first, then start normally to run)

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
		case "--restore":
			// F-A17-13: restore runs BEFORE the app starts, so no
			// daemon state is loaded from the (possibly damaged)
			// data dir first. The snapshot is verified (every
			// hash) before anything is written; a corrupt
			// snapshot is refused with nothing restored.
			if len(args) != 2 {
				fmt.Fprintln(os.Stderr, "oriond: --restore takes exactly one argument (a backup snapshot path)")
				return 2
			}
			// F-A18-7 (restore half): a corrupt config must not
			// block a restore — the operator restores precisely
			// because the data dir is damaged, and the config
			// file inside it may be damaged too. Load already
			// falls back to the defaults when the file is
			// missing; only a corrupt (or otherwise unloadable)
			// file errors out, so fall back the same way here
			// with a loud warning. The fallback honours
			// ORION_DATA_DIR, the same env override Load
			// applies, so a restore never silently targets
			// ~/.orion while the daemon runs on another dir.
			cfg, err := config.Load("")
			if err != nil {
				cfg = config.Default()
				// F-A19-2 (rc25): the file's data_dir is
				// unknown (the config could not be loaded),
				// so say so plainly — "default data dir" was
				// misleading when ORION_DATA_DIR pointed
				// elsewhere, and the operator restores
				// precisely because the data dir is damaged.
				// The dir named here is the one actually used.
				src := "default data dir"
				if dir := os.Getenv("ORION_DATA_DIR"); dir != "" {
					cfg.DataDir = dir
					src = "data dir from ORION_DATA_DIR"
				}
				fmt.Fprintf(os.Stderr, "oriond: warning: cannot load config (%v); the config file's data_dir is unknown, restoring with %s %s\n", err, src, cfg.DataDir)
			}
			// F-A18-2: take the instance lock BEFORE restoring.
			// A running daemon re-saves sessions + memory on
			// exit, silently undoing the restore; the lock is
			// the only thing that can see it. If the daemon is
			// running the lock fails and restore is refused —
			// the operator stops the daemon first and retries.
			lockFile, err := acquireInstanceLock(cfg.DataDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "oriond: %v\n", err)
				if errors.Is(err, errAlreadyRunning) {
					fmt.Fprintln(os.Stderr, "oriond: stop the daemon first (systemctl stop oriond, or SIGTERM), then retry --restore")
					return exitAlreadyRunning
				}
				return 1
			}
			// Closure, not `defer lockFile.Close()`: the lock
			// file may be replaced by the restore below (see
			// refreshInstanceLock), and the deferred close must
			// release whichever file is held at return time.
			defer func() {
				if lockFile != nil {
					_ = lockFile.Close()
				}
			}()
			if err := backup.Restore(args[1], cfg.DataDir); err != nil {
				fmt.Fprintf(os.Stderr, "oriond: restore failed: %v\n", err)
				return 1
			}
			// F-A18-2 (corrected F-A19-5): Restore never replaces
			// oriond.lock (it skips it in the verify and copy
			// loops), so the path we locked is the same file —
			// but re-check it anyway, in case anything else
			// replaced it mid-restore: flock follows the inode,
			// not the path, so a replaced file would leave us
			// holding a lock nobody else opens, and a daemon
			// could start alongside the rest of the restore.
			// Re-take the lock on the current file if it
			// changed. A failure here means another process
			// took the new file's lock mid-restore; the restore
			// itself already succeeded, so warn loudly instead
			// of failing it.
			if refreshed, rerr := refreshInstanceLock(lockFile, cfg.DataDir); rerr != nil {
				fmt.Fprintf(os.Stderr, "oriond: warning: restore succeeded but the instance lock was taken by another process during the restore (%v); check no daemon started mid-restore before starting one\n", rerr)
				lockFile = nil
			} else {
				lockFile = refreshed
			}
			// F-A18-2 bonus: a snapshot taken while the
			// allow-all opt-in window was open contains a future
			// <dataDir>/allow-all-until, and restoring it would
			// reopen the bot to every chat on the next start —
			// even if the operator has since closed the window.
			// A future deadline is closed (see
			// neutralizeRestoredAllowAll); a write failure is a
			// warning, never a failed restore.
			if neutralized, nerr := neutralizeRestoredAllowAll(cfg.DataDir); nerr != nil {
				fmt.Fprintf(os.Stderr, "oriond: warning: could not close a restored allow-all window (%v); check %s before starting the daemon\n", nerr, filepath.Join(cfg.DataDir, messaging.AllowAllStateFileName))
			} else if neutralized {
				fmt.Fprintf(os.Stderr, "oriond: warning: restored snapshot contained a future allow-all window; it is now closed (delete %s to open a fresh one)\n", filepath.Join(cfg.DataDir, messaging.AllowAllStateFileName))
			}
			fmt.Fprintf(os.Stderr, "oriond: restored data dir %s from snapshot %s; start normally to run\n", cfg.DataDir, args[1])
			return 0
		default:
			fmt.Fprintf(os.Stderr, "oriond: unknown argument %q\n", args[0])
			usage()
			return 2
		}
	}

	// F-A16-5: SIGHUP is ignored — a daemon outlives its terminal,
	// and a closed SSH session / terminal must not kill the bot
	// (previously SIGHUP killed it silently: default action,
	// terminate, with no log line). Stopping the daemon is
	// SIGINT/SIGTERM (or /restart, or systemctl stop). The ignore
	// is process-wide and logged once at startup so the log shows
	// the choice was deliberate.
	signal.Ignore(syscall.SIGHUP)
	fmt.Fprintln(os.Stderr, "oriond: ignoring SIGHUP (terminal close will not stop the daemon)")

	// F-A16-5: the signal is captured explicitly (not
	// signal.NotifyContext) so the shutdown path can log WHICH
	// signal stopped the daemon. Before this, SIGTERM/SIGINT
	// exited 0 with no log line at all — a dead daemon's log
	// could not tell a stop from a crash.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shutdownSig := make(chan os.Signal, 1)
	go func() {
		select {
		case sig := <-sigCh:
			// Buffered, so this send cannot block: by the
			// time ctx.Done fires, the name is already here.
			shutdownSig <- sig
			cancel()
		case <-ctx.Done():
		}
	}()

	a, err := app.Build("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "oriond: %v\n", err)
		return 1
	}
	defer a.Close()

	// F-A17-3: single-instance guard — an exclusive flock on
	// <dataDir>/oriond.lock, held for the life of the process. A
	// second oriond polling with the same token draws Telegram 409
	// Conflict, which is fatal: both instances flap, and the good
	// daemon dies with the bad one. The lock is taken before
	// getMe/polling starts, so the second instance exits 2 with a
	// clear log line and never talks to Telegram at all. flock is
	// released when the process dies (fd close), so a crash can
	// never leave a stale lock behind; the lock file itself stays
	// on disk, empty, which is fine — the lock is the flock, not
	// the file's existence.
	lockFile, err := acquireInstanceLock(a.Config.DataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "oriond: %v\n", err)
		return exitAlreadyRunning
	}
	defer lockFile.Close()

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
		// P2-O5 / F-A17-4: the opt-in is time-boxed — IsChatAllowed
		// refuses every chat once the window elapses. The deadline
		// is persisted in the data dir, so a restart resumes the
		// recorded window instead of opening a fresh one.
		gwOpts = append(gwOpts, messaging.WithAllowAllPersisted(a.Config.AllowAllWindow(), a.Config.DataDir))
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
	// F-A17-16: one startup line carrying everything the next
	// incident needs — version, PID, data dir, config path — so a
	// journal line can tell which build a dead daemon was. The
	// "oriond running as @..." prefix is kept verbatim: the
	// Audit 3 start/stop test waits for it.
	configPath, _ := config.DefaultPath()
	if configPath == "" {
		configPath = "unknown"
	}
	fmt.Printf("oriond running as @%s (provider %s, model %s); version %s; pid %d; data dir %s; config %s\n",
		username, a.Provider.Name(), active.Model, version.String(), os.Getpid(), a.Config.DataDir, configPath)

	// F-A3-2: optional localhost-only pprof/debug endpoint. Off by
	// default (config debug.pprof_enabled); when enabled it binds
	// 127.0.0.1 only and serves only the standard pprof handlers —
	// goroutine count, heap and GC become observable on a running
	// daemon without exposing config or secrets. A bind failure is
	// a loud warning, not a startup failure: the endpoint is
	// observability, and a port conflict must not kill the bot. An
	// invalid port never reaches here — Config.Validate rejects it
	// at load. The deferred Close runs on every exit path (poll
	// error, /restart, SIGTERM).
	var dbg *debug.Server
	if a.Config.Debug.PprofEnabled {
		srv, err := debug.Start(debug.Config{Enabled: true, Port: a.Config.DebugPprofPort()})
		if err != nil {
			fmt.Fprintf(os.Stderr, "oriond: warning: debug pprof endpoint unavailable: %v (continuing without it)\n", err)
		} else {
			dbg = srv
			fmt.Fprintf(os.Stderr, "oriond: debug pprof listening on %s\n", srv.Addr)
		}
	}
	if dbg != nil {
		defer dbg.Close()
	}

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

	// Daily budget threshold alert (F-A18-11, rc24): when a
	// chat's usage today reaches the configured threshold, the
	// owner (the first allowlisted chat) is notified once for
	// the day. The App decides when an alert fires and formats
	// it; this closure only delivers it through the gateway.
	// With no owner configured there is nobody to notify, so
	// the delivery fails with ErrBudgetAlertNoOwner (F-A19-6):
	// the App logs it once per chat and does not retry it,
	// instead of silently dropping the alert.
	a.SetBudgetAlertNotifier(func(ctx context.Context, chatID int64, text string) error {
		if ownerID == 0 {
			return fmt.Errorf("oriond: %w", app.ErrBudgetAlertNoOwner)
		}
		return gw.SendMessage(ctx, ownerID, text)
	})

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
		// F-A17-17: honest language — the turn contexts are already
		// cancelled (their contexts derive from ctx), so Drain only
		// waits for the cancelled turns to wind down and finish
		// writing state; it does not let them finish replying.
		fmt.Fprintf(os.Stderr, "oriond: restart requested; waiting up to %s for cancelled in-flight turns to wind down...\n", drainTimeout)
		if !a.Drain(drainTimeout) {
			fmt.Fprintf(os.Stderr, "oriond: restart: some turns had not wound down after %s; exiting anyway (their replies are lost)\n", drainTimeout)
		}
		// F-A17-2: /restart exits 42 (exitRestartRequested), NOT 0.
		// Choice: (a) distinct exit code + RestartForceExitStatus —
		// verified against the systemd.service man page, which
		// states plainly that a restart "is subject to unit start
		// rate limiting": RestartForceExitStatus does NOT bypass
		// StartLimitBurst, so (a) cannot by itself stop a
		// persistent fault from stranding the unit — the burst
		// limit stays the circuit breaker for genuine faults, by
		// design. What (a) DOES fix is the classification: with
		// Restart=always every exit — clean 0, intentional
		// restart, crash — counted identically toward the burst,
		// so /restart itself could burn the last restart the burst
		// allowed. With exit 42 + Restart=on-failure +
		// RestartForceExitStatus=42, an intentional restart is
		// distinguishable in the journal, restarts even under the
		// stricter policy, and a clean exit 0 (SIGTERM shutdown,
		// PollStream returning nil on context cancel — the only
		// nil return in PollStream) no longer restarts at all.
		// Rejected: (b) StartLimitIntervalSec 600s — a rapid loop
		// trips 12-in-600s exactly as it trips 12-in-60s (12
		// starts in ~60s either way), so it adds no self-recovery
		// and only makes slow loops strand sooner; (c) a startup
		// pre-flight check — the daemon already fails fast at
		// startup (config validate, getMe) and a check cannot
		// distinguish a transient from a deterministic fault
		// without error classification, so it duplicates the
		// existing failures without fixing the classification.
		// Unit lines the coordinator applies (W1 owns the unit
		// files — do NOT edit them here):
		//   [Service]
		//   Restart=on-failure
		//   RestartSec=5s
		//   RestartForceExitStatus=42
		// ([Unit] StartLimitIntervalSec=60s / StartLimitBurst=12
		// unchanged.) Without a supervisor the daemon simply exits
		// 42 and must be started manually — see /restart's reply
		// and ARCHITECTURE.md.
		return exitRestartRequested
	case <-ctx.Done():
		// SIGINT/SIGTERM: let in-flight turns unwind and persist
		// before Close saves and closes the memory store — the
		// /restart path drains the same way. Exiting immediately
		// could close the store under a turn that is still
		// recording its last step. Turns are already cancelled
		// (their contexts derive from ctx), so this is prompt
		// unless a run is stuck in a non-cancellable operation;
		// the drainTimeout bound keeps shutdown from hanging either way.
		// F-A16-5: log which signal stopped the daemon, so the
		// log can tell a stop from a crash after the fact.
		// signalName spells it "SIGTERM", not the os.Signal
		// String() "terminated" — the log must be readable.
		sigName := "signal"
		select {
		case sig := <-shutdownSig:
			sigName = signalName(sig)
		default:
		}
		fmt.Fprintf(os.Stderr, "oriond: shutting down on %s...\n", sigName)
		// F-A17-17: same bound as /restart (drainTimeout), and honest
		// language — Drain only waits for the cancelled turns to
		// wind down; their replies are already lost.
		if !a.Drain(drainTimeout) {
			fmt.Fprintf(os.Stderr, "oriond: shutdown: some turns had not wound down after %s; exiting anyway (their replies are lost)\n", drainTimeout)
		}
		fmt.Fprintln(os.Stderr, "oriond: shutdown complete")
		return 0
	}
}

// signalName spells a shutdown signal the way an operator reads
// it ("SIGTERM", "SIGINT"): os.Signal's String() says "terminated"
// / "interrupt", which in a log reads like a crash description,
// not a signal name. Unknown signals fall back to String().
func signalName(sig os.Signal) string {
	switch sig {
	case os.Interrupt:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGHUP:
		return "SIGHUP"
	default:
		return sig.String()
	}
}

// errAlreadyRunning marks an instance-lock failure caused by
// another live instance (not a permissions or missing-dir failure):
// the --restore path uses errors.Is on it to tell the operator to
// stop the daemon first instead of reporting a generic lock error.
// The wrapped message keeps the exact "already running" wording
// the F-A17-3 tests assert on.
var errAlreadyRunning = errors.New("another oriond instance is already running")

// acquireInstanceLock takes an exclusive, non-blocking flock on
// <dataDir>/oriond.lock (F-A17-3). The returned file must stay open
// for the life of the process: closing it releases the lock. A second
// instance gets an error naming the lock, so the caller can refuse to
// start with exit 2. A lock failure that is not "already locked"
// (permissions, missing dir) is a different error, so it is never
// misreported as a second instance.
func acquireInstanceLock(dataDir string) (*os.File, error) {
	if dataDir == "" {
		return nil, errors.New("oriond: cannot take the instance lock: data dir is empty")
	}
	path := filepath.Join(dataDir, "oriond.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("oriond: open instance lock %q: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("oriond: %w (instance lock %q is held); refusing to start a second poller", errAlreadyRunning, path)
		}
		return nil, fmt.Errorf("oriond: lock instance lock %q: %w", path, err)
	}
	return f, nil
}

// refreshInstanceLock re-establishes the instance lock on the current
// lock file after a restore (F-A18-2, corrected F-A19-5): Restore
// never replaces oriond.lock — backup.Restore skips it in both the
// verify and the copy loops, so a snapshot's lock file (if one is
// present at all) is never copied over the live one. The premise
// this comment used to state — that a snapshot carries its own
// lock and Restore copies it via temp file + rename — was false.
// The refresh stays as a defensive guard: if the lock file was
// replaced by anything else between the lock and the restore (an
// operator, or another process), flock follows the inode, not the
// path, so the caller would hold a lock on a file nobody else
// opens, and a daemon starting then could poll alongside the
// restore. When the path still points at the locked file this
// returns f unchanged; when it was replaced it closes f and takes
// the lock on the current file. An error means another process took
// the new file's lock mid-restore: f is already closed then, and
// the caller must not pretend the lock is still held.
func refreshInstanceLock(f *os.File, dataDir string) (*os.File, error) {
	if !instanceLockReplaced(f, dataDir) {
		return f, nil
	}
	_ = f.Close()
	return acquireInstanceLock(dataDir)
}

// instanceLockReplaced reports whether <dataDir>/oriond.lock is no
// longer the file f locks (replaced, deleted, or unreadable —
// conservative: any doubt counts as replaced, so the caller re-takes
// the lock on the current path instead of holding a stale one).
func instanceLockReplaced(f *os.File, dataDir string) bool {
	oldStat, err := f.Stat()
	if err != nil {
		return true
	}
	newStat, err := os.Stat(filepath.Join(dataDir, "oriond.lock"))
	if err != nil {
		return true
	}
	oldSys, ok := oldStat.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	newSys, ok := newStat.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	return oldSys.Ino != newSys.Ino
}

// neutralizeRestoredAllowAll closes a resurrected allow-all window
// after a restore (F-A18-2 bonus): a snapshot taken while the
// allow-all opt-in window was open contains <dataDir>/allow-all-until
// with a future deadline, and restoring it would reopen the bot to
// every chat on the next start — even if the operator has since
// closed the window or set an allowlist. A future deadline is
// overwritten with a past one, so the next start fails closed
// (the gateway refuses all chats until telegram_allowed_chats is
// set; deleting the file starts a fresh window — the same semantics
// as a corrupt state file). A missing, past, or unreadable deadline
// is left alone: missing means no window was open, past is already
// shut, and unreadable already fails closed at the gateway. It
// reports whether it closed a window; a write failure is returned
// so the caller can warn (the restore itself already succeeded).
func neutralizeRestoredAllowAll(dataDir string) (bool, error) {
	path := filepath.Join(dataDir, messaging.AllowAllStateFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, nil
	}
	deadline, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if err != nil {
		return false, nil
	}
	if !deadline.After(time.Now()) {
		return false, nil
	}
	expired := time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
	if err := os.WriteFile(path, []byte(expired+"\n"), 0o600); err != nil {
		return false, err
	}
	return true, nil
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
		// Dead-letter ledger (F-A10-2): the subagent Manager's
		// evicted-never-charged tokens, surfaced in /usage as
		// a daemon-wide line. Nil-safe: a hand-built App with
		// no Manager reports an empty ledger.
		DeadLetter: func() (int, int) {
			if a.Subagents == nil {
				return 0, 0
			}
			return a.Subagents.DeadLetterTokens(), a.Subagents.DeadLetterTasks()
		},
		Session: store.Get,
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
		// OwnerID + budget + skills reload + rules (rc23):
		// /budget and /skills reload (F-A17-15) and the /rules
		// command (F-A17-6) are wired here. The /budget owner
		// check lives in the handler (CommandEnv.isOwner, using
		// OwnerID); /rules takes IsOwner as a function so the
		// handler never guesses when the gate is unavailable.
		OwnerID:              ownerID,
		BudgetInfo:           a.BudgetInfoForChat,
		BudgetAlertThreshold: a.BudgetAlertThreshold,
		SetBudget:            a.SetDailyBudget,
		ReloadSkills:         a.ReloadSkills,
		IsOwner: func(chatID int64) bool {
			return ownerID != 0 && chatID == ownerID
		},
		ListRules: func() []permissions.Rule {
			g := a.PermissionGate()
			if g == nil || g.Rules() == nil {
				return nil
			}
			return g.Rules().List()
		},
		DeleteRule: func(chatID int64, tool string) error {
			g := a.PermissionGate()
			if g == nil || g.Rules() == nil {
				return errors.New("rules store unavailable")
			}
			return g.Rules().DeleteForChat(chatID, tool)
		},
		SetRule: func(chatID int64, tool string, action permissions.Action) error {
			g := a.PermissionGate()
			if g == nil || g.Rules() == nil {
				return errors.New("rules store unavailable")
			}
			return g.Rules().SetForChat(chatID, tool, action)
		},
	}
}
