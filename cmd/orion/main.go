// Command orion is Orion's CLI: run, chat, config, status, tools,
// memory, skills, subagent, telegram, version.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/irfan/orion/internal/app"
	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/debug"
	"github.com/irfan/orion/internal/messaging"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	// F-A16-5: the signal is captured explicitly (not
	// signal.NotifyContext) so a long-running command interrupted
	// by SIGINT/SIGTERM can log WHICH signal stopped it (below,
	// before exit). Before this, an interrupted `orion daemon` /
	// `orion telegram poll` exited 0 silently — the log could not
	// tell a stop from a crash. SIGHUP is NOT captured here: the
	// long-running Telegram loops ignore it in pollTelegram, and
	// short-lived commands keep the default SIGHUP behaviour.
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
			// time the command returns, the name is here.
			shutdownSig <- sig
			cancel()
		case <-ctx.Done():
		}
	}()

	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "chat":
		err = cmdChat(ctx, os.Args[2:])
	case "config":
		err = cmdConfig(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "tools":
		err = cmdTools(ctx, os.Args[2:])
	case "memory":
		err = cmdMemory(os.Args[2:])
	case "skills":
		err = cmdSkills(os.Args[2:])
	case "subagent":
		err = cmdSubagent(ctx, os.Args[2:])
	case "agents":
		err = cmdAgents(ctx, os.Args[2:])
	case "telegram":
		err = cmdTelegram(ctx, os.Args[2:])
	case "daemon":
		err = cmdDaemon(ctx, os.Args[2:])
	case "sessions":
		err = cmdSessions(os.Args[2:])
	case "backup":
		err = cmdBackup(os.Args[2:])
	case "version", "--version", "-v":
		if len(os.Args) > 2 {
			fmt.Fprintf(os.Stderr, "version: takes no arguments\n")
			os.Exit(2)
		}
		fmt.Printf("orion %s (go, model-agnostic, telegram-only)\n", version.String())
	case "help", "--help", "-h":
		if len(os.Args) > 2 {
			fmt.Fprintf(os.Stderr, "help: takes no arguments (try: orion <command> --help)\n")
			os.Exit(2)
		}
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usageTo(os.Stderr)
		os.Exit(2)
	}
	// F-A16-5: log which signal stopped the command, when a signal
	// did. (Non-blocking: a command that ran to completion logs
	// nothing here.)
	select {
	case sig := <-shutdownSig:
		fmt.Fprintf(os.Stderr, "orion: shutting down on %s...\n", signalName(sig))
	default:
	}
	if err != nil {
		// A command interrupted by SIGINT/SIGTERM is a clean exit:
		// the user asked to stop, so there is nothing to report and
		// the exit code matches the daemon, which already returns 0
		// on cancel. Anything else is printed and exits 1.
		if exitCodeFor(err) != 0 {
			fmt.Fprintf(os.Stderr, "orion: %v\n", err)
		}
		os.Exit(exitCodeFor(err))
	}
}

// exitCodeFor maps a command error to a process exit code. A nil error
// is 0; an interruption (context.Canceled, e.g. Ctrl-C during chat, a
// run, or a subagent wait) is also 0 and silent; a refused second
// poller (errAlreadyRunning) is 2, like oriond's exitAlreadyRunning,
// so a supervisor — and the journal — can tell it apart from a
// crash; any other error is 1.
func exitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, context.Canceled) {
		return 0
	}
	if errors.Is(err, errAlreadyRunning) {
		return 2
	}
	return 1
}

// errAlreadyRunning marks an instance-lock failure caused by another
// live poller (oriond, `orion daemon`, or `orion telegram poll`
// already holding <dataDir>/oriond.lock) — not a permissions or
// missing-dir failure. exitCodeFor maps it to exit 2.
var errAlreadyRunning = errors.New("another orion instance is already running")

// acquireInstanceLock takes an exclusive, non-blocking flock on
// <dataDir>/oriond.lock (F-A17-3 / F-A18-5) — the SAME lock file
// oriond holds, so `orion daemon`, `orion telegram poll` and oriond
// mutually exclude: a second poller would draw a Telegram 409 and
// flap the good daemon too. This is a copy of the helper in
// cmd/oriond/main.go (package main cannot be imported); keep the two
// in sync — same file, same flock, same "already running" wording —
// or a future change to one side silently reopens the 409 flap. The
// returned file must stay open for the life of the polling loop:
// closing it releases the lock. A lock failure that is not "already
// locked" (permissions, missing dir) is a different error, so it is
// never misreported as a second instance.
func acquireInstanceLock(dataDir string) (*os.File, error) {
	if dataDir == "" {
		return nil, errors.New("orion: cannot take the instance lock: data dir is empty")
	}
	path := filepath.Join(dataDir, "oriond.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("orion: open instance lock %q: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (instance lock %q is held); stop the other daemon or poller first", errAlreadyRunning, path)
		}
		return nil, fmt.Errorf("orion: lock instance lock %q: %w", path, err)
	}
	return f, nil
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

func usage() { usageTo(os.Stdout) }

// usageTo writes the top-level usage text to w: stdout for --help,
// stderr when it follows an error (unknown command).
func usageTo(w io.Writer) {
	fmt.Fprintf(w, `orion — model-agnostic OS agent (v%s)

Usage:
  orion run [--config <path>] "prompt"   run one prompt through the agent
  orion chat                  interactive CLI chat (same agent, same memory)
  orion config show           show config (secrets masked)
  orion config set-provider <openai|anthropic|ollama|deepseek>
  orion config set-model <model>
  orion config set-api-key <openai|anthropic|ollama|deepseek> <key>
  orion config set-api-key <provider> -   read the key from stdin (avoids shell history / process list)
  orion status                provider, memory, tools, telegram status
  orion tools                 list tools
  orion tools exec <name> '<json args>'
  orion memory list           list remembered facts
  orion memory add <key> <value>
  orion memory get <key>
  orion memory search <query>
  orion skills                list loaded skills
  orion skills search <query>
  orion subagent run "brief"  run a brief as a managed child agent
  orion agents                list subagent tasks (same view as /agents)
  orion telegram test         verify the Telegram bot token (getMe)
  orion telegram poll         start the Telegram gateway (polling)
  orion daemon                run Telegram polling until interrupted
  orion sessions              list CLI sessions (transcripts)
  orion sessions resume <id>  resume a CLI session
  orion sessions new          start a fresh CLI session
  orion backup                snapshot ~/.orion (newest 7 kept)
  orion version               print version
`, version.Version)
}

// isHelpArg reports whether s asks for per-command help.
func isHelpArg(s string) bool { return s == "--help" || s == "-h" }

func buildApp() (*app.App, error) { return app.Build("") }

// applyTelegramBaseURL honours ORION_TELEGRAM_BASE_URL for the CLI's
// Telegram paths (test, poll and daemon), exactly as oriond does:
// it points the Bot API client at a self-hosted Bot API server (or a
// local fake for testing) instead of api.telegram.org. It never
// carries a secret — only the endpoint.
func applyTelegramBaseURL() {
	if baseURL := os.Getenv("ORION_TELEGRAM_BASE_URL"); baseURL != "" {
		messaging.SetDefaultBaseURL(baseURL)
	}
}

// localState is the provider-free slice of the app (see app.Local).
// Local commands build it through the single shared wiring in
// app.BuildLocal, not a second copy here.
type localState = app.Local

func buildLocal() (*localState, error) { return app.BuildLocal("") }

// maskSecret reveals nothing about a secret's value — not even its
// first or last characters (for a 5-6 character secret, those are most
// of the value) and never a byte slice that could split a non-ASCII
// rune. Callers only learn whether a secret is set.
func maskSecret(s string) string {
	if s == "" {
		return "(not set)"
	}
	return "****"
}

// providerKey returns the canonical map key for a provider name,
// matched case-insensitively, so "OpenAI" finds the "openai" entry.
func providerKey(cfg *config.Config, name string) (string, bool) {
	for key := range cfg.Providers {
		if strings.EqualFold(key, name) {
			return key, true
		}
	}
	return "", false
}

func cmdRun(ctx context.Context, args []string) error {
	if len(args) > 0 && isHelpArg(args[0]) {
		fmt.Print(`Usage: orion run [--config <path>] "prompt"

Run one prompt through the agent and print the reply. All arguments
after the flags are joined with spaces into a single prompt.
  --config <path>   load config from <path> instead of ~/.orion/config.json
`)
		return nil
	}
	// --config flag (Audit 10 W4 observation i): the config loads
	// only via $HOME/.orion/config.json by default, so a scratch
	// config needed a scratch HOME. --config makes scratch-config
	// runs work as written. Only leading flags are parsed; the
	// prompt is everything after them. A missing value is a usage
	// error, not a silent default.
	configPath := ""
	for len(args) > 0 {
		if args[0] == "--config" {
			if len(args) < 2 {
				return fmt.Errorf("run: --config needs a path, e.g. orion run --config /tmp/orion-test.json \"prompt\"")
			}
			configPath = args[1]
			args = args[2:]
			continue
		}
		if strings.HasPrefix(args[0], "--config=") {
			configPath = strings.TrimPrefix(args[0], "--config=")
			if configPath == "" {
				return fmt.Errorf("run: --config needs a path, e.g. orion run --config=/tmp/orion-test.json \"prompt\"")
			}
			args = args[1:]
			continue
		}
		break
	}
	if len(args) == 0 {
		return fmt.Errorf("run: provide a prompt, e.g. orion run \"what time is it?\"")
	}
	// An explicitly named config file must exist: config.Load
	// treats a missing file as "use the defaults", which is the
	// right first-run behaviour for the default path — but for an
	// explicit --config it would silently run the LIVE config
	// (defaults resolve to ~/.orion/config.json) when the user
	// meant a scratch one, defeating the flag's whole purpose.
	if configPath != "" {
		if _, err := os.Stat(configPath); err != nil {
			return fmt.Errorf("run: --config %s: %w", configPath, err)
		}
	}
	a, err := app.Build(configPath)
	if err != nil {
		return err
	}
	defer a.Close()
	out, err := a.Handle(ctx, strings.Join(args, " "))
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

func cmdChat(ctx context.Context, args []string) error {
	if len(args) > 0 && isHelpArg(args[0]) {
		fmt.Print(`Usage: orion chat

Interactive CLI chat (same agent, same memory as Telegram).
Type /quit to exit. Takes no arguments.
`)
		return nil
	}
	if len(args) > 0 {
		return fmt.Errorf("chat: takes no arguments (got %q)", strings.Join(args, " "))
	}
	a, err := buildApp()
	if err != nil {
		return err
	}
	defer a.Close()
	fmt.Println("Orion chat — type /quit to exit.")
	return messaging.RunCLI(ctx, os.Stdin, os.Stdout, func(c context.Context, input string) (string, error) {
		return a.Handle(c, input)
	})
}

func configUsage() {
	fmt.Print(`Usage:
  orion config show                          show config (secrets masked)
  orion config set-provider <openai|anthropic|ollama|deepseek>
  orion config set-model <model>
  orion config set-api-key <openai|anthropic|ollama|deepseek> <key>
  orion config set-api-key <provider> -      read the key from stdin (avoids shell history / process list)
`)
}

func cmdConfig(args []string) error {
	if len(args) > 0 && isHelpArg(args[0]) {
		configUsage()
		return nil
	}
	if len(args) == 0 || args[0] == "show" {
		if len(args) > 1 {
			return fmt.Errorf("config show: takes no arguments")
		}
		// Show is a local read: it must work even when the configured
		// provider has no API key, so it never builds the full app.
		l, err := buildLocal()
		if err != nil {
			return err
		}
		defer l.Close()
		cfg := l.Config
		// An ActiveProvider failure is reported, not papered over
		// with an empty model/key: show must never present a broken
		// provider as if it were configured but empty.
		active, err := cfg.ActiveProvider()
		if err != nil {
			return fmt.Errorf("config show: %w", err)
		}
		fmt.Printf("data_dir:        %s\n", cfg.DataDir)
		fmt.Printf("default_provider: %s\n", cfg.DefaultProvider)
		fmt.Printf("model:           %s\n", active.Model)
		fmt.Printf("provider api_key: %s\n", maskSecret(active.APIKey))
		fmt.Printf("provider base_url: %s\n", active.BaseURL)
		fmt.Printf("telegram_token:  %s\n", maskSecret(cfg.TelegramToken))
		fmt.Printf("max_steps:       %d\n", cfg.MaxSteps)
		fmt.Printf("timeout_seconds: %d\n", cfg.TimeoutSeconds)
		fmt.Printf("sandbox_enabled: %v\n", cfg.Sandbox.Enabled)
		fmt.Printf("sandbox_root:    %s\n", cfg.Sandbox.Root)
		fmt.Println("providers:")
		names := make([]string, 0, len(cfg.Providers))
		for n := range cfg.Providers {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p := cfg.Providers[n]
			fmt.Printf("  %s: model=%s base_url=%s api_key=%s\n", n, p.Model, p.BaseURL, maskSecret(p.APIKey))
		}
		return nil
	}
	// Mutating subcommands load the config file directly (no provider
	// construction), so they work even when the current provider has
	// no API key configured yet. The home directory must be known:
	// with HOME unset, DefaultPath fails instead of the config being
	// written into the working directory.
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(defaultPath)
	if err != nil {
		return err
	}
	switch args[0] {
	case "set-provider":
		if len(args) < 2 {
			return fmt.Errorf("config set-provider: provide a provider name (openai|anthropic|ollama|deepseek)")
		}
		if len(args) > 2 {
			return fmt.Errorf("config set-provider: takes exactly one provider name (got %q)", strings.Join(args[1:], " "))
		}
		// Provider names are case-insensitive everywhere else
		// (providerEntry/Validate), so "OpenAI" must work here too.
		// The canonical (lowercase) map key is stored.
		key, ok := providerKey(cfg, args[1])
		if !ok {
			return fmt.Errorf("config set-provider: unknown provider %q", args[1])
		}
		cfg.DefaultProvider = key
		if err := cfg.Save(defaultPath); err != nil {
			return err
		}
		fmt.Printf("default provider set to %s\n", key)
		return nil
	case "set-model":
		if len(args) < 2 {
			return fmt.Errorf("config set-model: provide a model name")
		}
		if len(args) > 2 {
			return fmt.Errorf("config set-model: takes exactly one model name (got %q)", strings.Join(args[1:], " "))
		}
		if strings.TrimSpace(args[1]) == "" {
			return fmt.Errorf("config set-model: model name must not be empty")
		}
		cfg.Model = args[1]
		if err := cfg.Save(defaultPath); err != nil {
			return err
		}
		fmt.Printf("model override set to %s\n", args[1])
		return nil
	case "set-api-key":
		// Key-requiring providers cannot become the default until a key
		// is stored, and ORION_API_KEY only applies to the current
		// default provider, so the CLI needs a way to store a key for
		// a provider before switching to it.
		if len(args) < 3 {
			return fmt.Errorf("config set-api-key: usage: orion config set-api-key <openai|anthropic|ollama|deepseek> <key>")
		}
		if len(args) > 3 {
			return fmt.Errorf("config set-api-key: takes exactly a provider name and a key")
		}
		key, ok := providerKey(cfg, args[1])
		if !ok {
			return fmt.Errorf("config set-api-key: unknown provider %q", args[1])
		}
		// F9: a key passed on the command line lands in shell
		// history and the process list. "-" reads the key from
		// stdin instead (e.g. `orion config set-api-key deepseek -
		// < deepseek-key.txt`); the value is trimmed and never
		// echoed.
		keyValue := args[2]
		if keyValue == "-" {
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("config set-api-key: read key from stdin: %w", err)
			}
			keyValue = strings.TrimSpace(string(raw))
		}
		p := cfg.Providers[key]
		if strings.TrimSpace(keyValue) == "" {
			return fmt.Errorf("config set-api-key: key must not be empty")
		}
		p.APIKey = keyValue
		cfg.Providers[key] = p
		if err := cfg.Save(defaultPath); err != nil {
			return err
		}
		fmt.Printf("api key set for %s\n", key)
		return nil
	default:
		return fmt.Errorf("config: unknown subcommand %q (try: show, set-provider, set-model, set-api-key)", args[0])
	}
}

func cmdStatus(args []string) error {
	if len(args) > 0 && isHelpArg(args[0]) {
		fmt.Print(`Usage: orion status

Show provider, memory, tools and telegram status. Takes no arguments.
`)
		return nil
	}
	if len(args) > 0 {
		return fmt.Errorf("status: takes no arguments (got %q)", strings.Join(args, " "))
	}
	// Status is a local read: it must work even when the configured
	// provider has no API key, so it never builds the full app.
	l, err := buildLocal()
	if err != nil {
		return err
	}
	defer l.Close()
	// Memory and provider errors are reported, not papered over as
	// "0 facts, 0 recent messages" on a broken store.
	active, err := l.Config.ActiveProvider()
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	facts, err := l.Memory.ListFacts()
	if err != nil {
		return fmt.Errorf("status: memory: %w", err)
	}
	msgs, err := l.Memory.RecentMessages(1000)
	if err != nil {
		return fmt.Errorf("status: memory: %w", err)
	}
	fmt.Printf("orion %s\n", version.Version)
	fmt.Printf("provider: %s (model %s, base %s)\n", active.Name, active.Model, active.BaseURL)
	fmt.Printf("data_dir: %s\n", l.Config.DataDir)
	fmt.Printf("tools: %d registered\n", len(l.Tools.List()))
	fmt.Printf("memory: %d facts, %d recent messages\n", len(facts), len(msgs))
	fmt.Printf("skills: %d loaded\n", len(l.Skills.List()))
	if l.Config.TelegramToken != "" {
		fmt.Println("telegram: configured")
	} else {
		fmt.Println("telegram: not configured (set ORION_TELEGRAM_TOKEN or config telegram_token)")
	}
	return nil
}

func toolsUsage() {
	fmt.Print(`Usage:
  orion tools                          list tools
  orion tools exec <name> '<json args>'  run one tool with JSON arguments
`)
}

func cmdTools(ctx context.Context, args []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(args) > 0 && isHelpArg(args[0]) {
		toolsUsage()
		return nil
	}
	// Validate the subcommand before building anything, so bad input
	// fails fast with a usage error instead of silently listing tools.
	isExec := len(args) >= 1 && args[0] == "exec"
	if len(args) > 0 && !isExec {
		return fmt.Errorf("tools: unknown subcommand %q (try: exec)", args[0])
	}
	if isExec {
		if len(args) < 2 {
			return fmt.Errorf("tools exec: usage: orion tools exec <name> '<json args>'")
		}
		if len(args) > 3 {
			return fmt.Errorf("tools exec: takes at most one JSON argument (got %q)", strings.Join(args[3:], " "))
		}
	}
	l, err := buildLocal()
	if err != nil {
		return err
	}
	defer l.Close()
	if isExec {
		name := args[1]
		raw := json.RawMessage("{}")
		if len(args) >= 3 {
			raw = json.RawMessage(args[2])
		}
		out, err := l.Tools.Execute(ctx, name, raw)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	}
	for _, t := range l.Tools.List() {
		fmt.Printf("%s — %s\n", t.Name(), t.Description())
	}
	return nil
}

func memoryUsage() {
	fmt.Print(`Usage:
  orion memory list           list remembered facts
  orion memory add <key> <value>
  orion memory get <key>
  orion memory search <query>
`)
}

func cmdMemory(args []string) error {
	if len(args) > 0 && isHelpArg(args[0]) {
		memoryUsage()
		return nil
	}
	if len(args) == 0 {
		args = []string{"list"}
	}
	// Validate the subcommand and its arguments before building
	// anything, so bad input fails fast with a usage error.
	switch args[0] {
	case "list":
		if len(args) > 1 {
			return fmt.Errorf("memory list: takes no arguments")
		}
	case "add":
		if len(args) < 3 {
			return fmt.Errorf("memory add: usage: orion memory add <key> <value>")
		}
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("memory get: usage: orion memory get <key>")
		}
		if len(args) > 2 {
			return fmt.Errorf("memory get: takes exactly one key (got %q)", strings.Join(args[1:], " "))
		}
	case "search":
		if len(args) < 2 {
			return fmt.Errorf("memory search: usage: orion memory search <query>")
		}
	default:
		return fmt.Errorf("memory: unknown subcommand %q (try: list, add, get, search)", args[0])
	}
	l, err := buildLocal()
	if err != nil {
		return err
	}
	defer l.Close()
	switch args[0] {
	case "list":
		facts, err := l.Memory.ListFacts()
		if err != nil {
			return err
		}
		if len(facts) == 0 {
			fmt.Println("(no facts stored)")
			return nil
		}
		for _, f := range facts {
			fmt.Printf("%s = %s\n", f.Key, f.Value)
		}
	case "add":
		return l.Memory.AddFact(args[1], strings.Join(args[2:], " "))
	case "get":
		f, ok, err := l.Memory.GetFact(args[1])
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("memory get: no fact %q", args[1])
		}
		fmt.Printf("%s = %s\n", f.Key, f.Value)
	case "search":
		facts, err := l.Memory.SearchFacts(strings.Join(args[1:], " "))
		if err != nil {
			return err
		}
		for _, f := range facts {
			fmt.Printf("%s = %s\n", f.Key, f.Value)
		}
	}
	return nil
}

func skillsUsage() {
	fmt.Print(`Usage:
  orion skills                list loaded skills
  orion skills search <query>
`)
}

func cmdSkills(args []string) error {
	if len(args) > 0 && isHelpArg(args[0]) {
		skillsUsage()
		return nil
	}
	// Validate before building, so unknown input fails instead of
	// silently listing skills.
	if len(args) > 0 && args[0] != "search" {
		return fmt.Errorf("skills: unknown subcommand %q (try: search)", args[0])
	}
	if len(args) > 0 && args[0] == "search" && len(args) < 2 {
		return fmt.Errorf("skills search: usage: orion skills search <query>")
	}
	l, err := buildLocal()
	if err != nil {
		return err
	}
	defer l.Close()
	list := l.Skills.List()
	if len(args) >= 2 && args[0] == "search" {
		list = l.Skills.Search(strings.Join(args[1:], " "))
	}
	if len(list) == 0 {
		fmt.Println("(no skills loaded — put .md skill files in ~/.orion/skills/)")
		return nil
	}
	for _, s := range list {
		fmt.Printf("%s — %s\n", s.Name, s.Description)
	}
	return nil
}

func subagentUsage() {
	fmt.Print(`Usage: orion subagent run "brief"

Run a brief as a managed child agent and print its output.
`)
}

func cmdSubagent(ctx context.Context, args []string) error {
	if len(args) > 0 && isHelpArg(args[0]) {
		subagentUsage()
		return nil
	}
	if len(args) < 2 || args[0] != "run" {
		return fmt.Errorf("subagent: usage: orion subagent run \"brief\"")
	}
	a, err := buildApp()
	if err != nil {
		return err
	}
	defer a.Close()
	id, err := a.Subagents.Spawn(strings.Join(args[1:], " "), 0)
	if err != nil {
		return err
	}
	res, err := a.Subagents.Wait(ctx, id)
	if err != nil {
		return err
	}
	if res.Err != nil {
		return res.Err
	}
	fmt.Println(res.Output)
	return nil
}

func cmdAgents(ctx context.Context, args []string) error {
	if len(args) > 0 && isHelpArg(args[0]) {
		fmt.Print(`Usage: orion agents

List subagent tasks with status, duration, tokens and result
summary — the same view as Telegram /agents.
`)
		return nil
	}
	if len(args) > 0 {
		return fmt.Errorf("agents: takes no arguments")
	}
	a, err := buildApp()
	if err != nil {
		return err
	}
	defer a.Close()
	fmt.Println(a.AgentsSummary())
	return nil
}

func telegramUsage() {
	fmt.Print(`Usage:
  orion telegram test         verify the Telegram bot token (getMe)
  orion telegram poll         start the Telegram gateway (polling)
`)
}

func cmdTelegram(ctx context.Context, args []string) error {
	if len(args) > 0 && isHelpArg(args[0]) {
		telegramUsage()
		return nil
	}
	// Validate the subcommand before building the app or checking the
	// token, so bad input reports the real problem (a bad subcommand),
	// not a missing token.
	if len(args) == 0 {
		return fmt.Errorf("telegram: usage: orion telegram <test|poll>")
	}
	switch args[0] {
	case "test", "poll":
		if len(args) > 1 {
			return fmt.Errorf("telegram %s: takes no arguments", args[0])
		}
	default:
		return fmt.Errorf("telegram: unknown subcommand %q (try: test, poll)", args[0])
	}
	if args[0] == "test" {
		// Token verification needs config + token only: it must not
		// build the full app (provider, tools, memory, agent), or an
		// unrelated failure — a corrupt memory store, an agent that
		// cannot be constructed — would make a token check fail with
		// the wrong error. (Config load still validates the config
		// file, including the default provider; fully decoupling the
		// token check from provider validation is a config-layer
		// follow-up.)
		cfg, err := config.Load("")
		if err != nil {
			return err
		}
		if cfg.TelegramToken == "" {
			return fmt.Errorf("telegram: no token configured (set ORION_TELEGRAM_TOKEN or config telegram_token)")
		}
		applyTelegramBaseURL()
		gw, err := messaging.NewTelegramGateway(cfg.TelegramToken, messaging.WithAllowedChats(cfg.TelegramAllowedChats...))
		if err != nil {
			return err
		}
		username, err := gw.GetMe(ctx)
		if err != nil {
			return fmt.Errorf("telegram test: %w", err)
		}
		fmt.Printf("telegram: token valid, bot @%s\n", username)
		return nil
	}
	a, err := buildApp()
	if err != nil {
		return err
	}
	defer a.Close()
	// F-A18-5: single-instance guard — the same flock on
	// <dataDir>/oriond.lock oriond holds (see acquireInstanceLock):
	// a second `orion telegram poll` would draw a Telegram 409 and
	// flap the good poller. Taken here, before the token check and
	// gateway setup, so the refusal is fast and never talks to
	// Telegram at all. `telegram test` returned above and never
	// polls, so it takes no lock. Held (defer Close) until this
	// command returns.
	lockFile, err := acquireInstanceLock(a.Config.DataDir)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if a.Config.TelegramToken == "" {
		return fmt.Errorf("telegram: no token configured (set ORION_TELEGRAM_TOKEN or config telegram_token)")
	}
	applyTelegramBaseURL()
	// P2-O5: LOUD warning when the allow-all opt-in is set.
	if w := a.Config.AllowAllTelegramWarning(); w != "" {
		fmt.Fprintf(os.Stderr, "orion: %s\n", w)
	}
	gw, err := newTelegramGateway(a)
	if err != nil {
		return err
	}
	switch args[0] {
	case "poll":
		// An empty allowlist refuses startup (see
		// messaging.CheckTelegramAllowlist): polling with no
		// allowlist would let any Telegram chat drive the agent.
		if err := messaging.CheckTelegramAllowlist(a.Config.TelegramAllowedChats, a.Config.AllowAllTelegramChats); err != nil {
			return err
		}
		// F-A6-3: the gate is wired before polling starts, and
		// the turn runs on the per-chat handler (the same one
		// `orion daemon` uses) — not the global a.Handle,
		// which bypassed per-chat isolation (P1-O3) and the
		// per-chat budget. `orion telegram poll` is now the
		// same gated, isolated loop as `orion daemon`.
		//
		// F-A7-P2-8: this entrypoint is TURN-ONLY — it has no
		// slash-command framework. /restart, /new, /model and
		// the rest go to the agent as plain text here; only
		// oriond dispatches slash commands in full (its registry
		// and CommandEnv live in cmd/oriond, and wiring them here
		// would duplicate ~300 lines of per-turn env, pause,
		// footer, retry and restart plumbing with no shared
		// home for it). The ONE exception is /rules (F-A18-10),
		// dispatched by daemonTurnHandler because the rule store
		// it operates on is already built by wireGate here.
		// /restart owner-only therefore holds vacuously on this
		// path: there is no /restart command to refuse, only a
		// text turn about restarting.
		if _, _, err := wireGate(a, gw); err != nil {
			return err
		}
		// F-A16-7: the same localhost-only pprof endpoint as
		// `orion daemon` and oriond (see startDebugEndpoint):
		// off by default, opt-in via config debug.pprof_enabled.
		// Before rc22 this path polled with no endpoint at all
		// even when enabled, while the other two Telegram
		// polling entrypoints served one.
		dbg := startDebugEndpoint(a)
		if dbg != nil {
			defer dbg.Close()
		}
		fmt.Println("telegram: polling for messages (Ctrl+C to stop)...")
		return pollTelegram(a, gw, ctx)
	}
	return nil
}

func cmdDaemon(ctx context.Context, args []string) error {
	if len(args) > 0 && isHelpArg(args[0]) {
		fmt.Print(`Usage: orion daemon

Run Telegram polling until interrupted. Takes no arguments.
`)
		return nil
	}
	if len(args) > 0 {
		return fmt.Errorf("daemon: takes no arguments (got %q)", strings.Join(args, " "))
	}
	a, err := buildApp()
	if err != nil {
		return err
	}
	defer a.Close()
	// F-A18-5: single-instance guard — the same flock on
	// <dataDir>/oriond.lock oriond holds (see acquireInstanceLock):
	// a second `orion daemon` would draw a Telegram 409 and flap
	// the good daemon. Taken here, before getMe and the startup
	// banner, so the refusal is fast and never talks to Telegram
	// at all. Held (defer Close) until this command returns.
	lockFile, err := acquireInstanceLock(a.Config.DataDir)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if a.Config.TelegramToken == "" {
		return fmt.Errorf("daemon: no telegram token configured; run `orion chat` for CLI mode instead")
	}
	applyTelegramBaseURL()
	// P2-O5: LOUD warning when the allow-all opt-in is set.
	if w := a.Config.AllowAllTelegramWarning(); w != "" {
		fmt.Fprintf(os.Stderr, "orion: %s\n", w)
	}
	gw, err := newTelegramGateway(a)
	if err != nil {
		return err
	}
	// An empty allowlist refuses startup (see
	// messaging.CheckTelegramAllowlist): polling with no allowlist
	// would let any Telegram chat drive the agent.
	if err := messaging.CheckTelegramAllowlist(a.Config.TelegramAllowedChats, a.Config.AllowAllTelegramChats); err != nil {
		return err
	}
	// F-A6-3: the permission gate is wired before polling
	// starts — before rc11 this path never called
	// SetPermissionGate, so destructive/network tools ran
	// without approval here while oriond gated them.
	if _, _, err := wireGate(a, gw); err != nil {
		return err
	}
	username, err := gw.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("daemon: telegram getMe: %w", err)
	}
	active, err := a.Config.ActiveProvider()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	fmt.Printf("orion daemon running as @%s (provider %s, model %s). Ctrl+C to stop.\n",
		username, a.Provider.Name(), active.Model)
	// F-A3-2 / F-A16-7: the localhost-only pprof endpoint — the
	// same one oriond and `orion telegram poll` serve (see
	// startDebugEndpoint): off by default, opt-in via config
	// debug.pprof_enabled, binds 127.0.0.1 only. A bind failure
	// is a warning, not a startup failure.
	dbg := startDebugEndpoint(a)
	if dbg != nil {
		defer dbg.Close()
	}
	return pollTelegram(a, gw, ctx)
}

// startDebugEndpoint starts the optional localhost-only pprof/debug
// endpoint (F-A3-2) for the CLI's Telegram loops: `orion daemon`
// and `orion telegram poll` (oriond has the identical wiring in
// cmd/oriond). It returns nil — and listens on nothing — when the
// endpoint is disabled in config (debug.Start returns (nil, nil)
// then), and also on a bind failure, which is a warning, not a
// startup failure: the endpoint is observability, and a port
// conflict must not kill the bot. The caller closes the returned
// server (defer dbg.Close()) so it stops on every exit path.
func startDebugEndpoint(a *app.App) *debug.Server {
	if a == nil || a.Config == nil || !a.Config.Debug.PprofEnabled {
		return nil
	}
	srv, err := debug.Start(debug.Config{Enabled: true, Port: a.Config.DebugPprofPort()})
	if err != nil {
		fmt.Fprintf(os.Stderr, "orion: warning: debug pprof endpoint unavailable: %v (continuing without it)\n", err)
		return nil
	}
	fmt.Fprintf(os.Stderr, "orion: debug pprof listening on %s\n", srv.Addr)
	return srv
}

// newRulesCommandEnv builds the CommandEnv for the /rules command
// this binary serves (F-A18-10): the permission gate's rule store
// is already built by wireGate on both Telegram entrypoints, so
// /rules needs no per-turn env, pause, footer, retry or restart
// plumbing — only the gate's rule store and the owner id. The
// closures read the gate lazily (a.PermissionGate) exactly like
// cmd/oriond's buildCommandEnv, so the env stays valid even before
// wireGate installs the gate; a dispatch before the gate exists
// replies that rules are unavailable instead of guessing. The
// owner is the first allowlisted chat, as in wireGate.
func newRulesCommandEnv(a *app.App) *messaging.CommandEnv {
	ownerID := messaging.OwnerIDFromAllowlist(a.Config.TelegramAllowedChats)
	return &messaging.CommandEnv{
		OwnerID: ownerID,
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

// daemonTurnHandler is the turn function for `orion daemon` AND
// `orion telegram poll` (F-A6-3): each chat runs on its own
// per-chat agent through App.HandleForChat, so a chat's history,
// provider and usage are isolated from every other chat (P1-O3) —
// and the per-chat daily token budget is enforced (rc9 follow-up
// 2): the budget gate lives in app.runChatTurn, shared with
// oriond's Telegram turns, so this path refuses an over-budget
// chat exactly the way oriond does. Before rc9 this handler called
// the GLOBAL App.Handle, which had no chat context and no budget
// check at all: any allowlisted chat could spend without limit on
// this path while oriond enforced the budget.
//
// TURN-ONLY (F-A7-P2-8) with one exception, /rules (F-A18-10):
// every slash command except /rules goes to the agent as plain
// text on these entrypoints; only oriond runs the full
// slash-command framework (messaging.Registry + cmd/oriond's
// CommandEnv wiring). /rules is dispatched here because the
// permission-gate rule store it operates on is already built by
// wireGate in this binary, the handler is the shared one from
// messaging.DefaultRegistry (so it cannot drift from oriond's),
// and it needs none of the per-turn env, pause, footer, retry or
// restart plumbing a full registry would. Do not extend this to
// a second command: a partial registry would silently differ
// from oriond's, and the full wiring has no shared home outside
// cmd/oriond.
//
// The turn context carries the approval context (P1-O2/P1-O4):
// the chat the prompt goes to and the sender who may answer it.
// Without it the permission gate would address its approval card
// to chat 0 and accept any approver, and spawned subagents would
// be unattributed (chatID 0, never swept). The sender comes from
// the polling layer (PollStream sets it via WithSenderID).
//
// A budget refusal is returned as a plain reply, not an error:
// the turn was refused on purpose, and the message names the usage,
// the limit and the UTC-midnight reset. Other errors stay errors
// (the gateway reports them as "Sorry, I hit an error: ...").
//
// The turn is registered with the App (F-A10-5, mirroring oriond's
// runTurn): pollTelegram drains registered turns after Poll
// returns, so the command does not exit while a turn is still
// writing its transcript/memory into the data dir.
func daemonTurnHandler(a *app.App) func(context.Context, int64, string) (string, error) {
	// /rules (F-A18-10): the one slash command this binary
	// dispatches — see newRulesCommandEnv. The registry is the
	// shared DefaultRegistry, so the handler is oriond's handler;
	// only /rules is ever dispatched here, and only when the text
	// actually parses as /rules (Parse also strips an @botname
	// suffix, so /rules@bot in a group works).
	rulesEnv := newRulesCommandEnv(a)
	rulesRegistry := messaging.DefaultRegistry(rulesEnv)
	return func(c context.Context, chatID int64, text string) (string, error) {
		runCtx, cancel := context.WithCancel(c)
		registered := a.RegisterRun(chatID, runCtx, cancel)
		if registered {
			defer a.UnregisterRun(chatID, runCtx)
		}
		defer cancel()
		senderID, _ := messaging.SenderIDFromContext(c)
		if name, _, ok := messaging.Parse(text); ok && name == "rules" {
			// Per-turn copy of the env with this chat's ID:
			// the shared env must never carry mutable
			// per-chat state (PollStream handles chats
			// concurrently). The sender stays in ctx, so
			// the handler's isOwner check recognises the
			// owner sending from a group chat (F-A18-10).
			turnEnv := *rulesEnv
			turnEnv.ChatID = chatID
			if reply, handled, err := rulesRegistry.Dispatch(runCtx, &turnEnv, text); handled {
				return reply, err
			}
		}
		c = permissions.WithApprovalContext(runCtx, chatID, senderID)
		reply, err := a.HandleForChat(c, chatID, text)
		if err != nil && errors.Is(err, budget.ErrBudgetExceeded) {
			return fmt.Sprintf("⏸ Daily token budget reached for this chat: %v", err), nil
		}
		return reply, err
	}
}

// drainTimeout bounds the wait for cancelled in-flight turns to wind
// down after polling stops (F-A17-17). It MUST be the same value
// cmd/oriond/main.go uses: the two entrypoints previously disagreed
// (10s vs 30s) for no reason. 30s keeps the F-A10-5 margin — the
// only reasoned bound in the codebase — that keeps a turn from racing
// transcript/memory writes during teardown. If it changes here,
// change it in cmd/oriond/main.go too. (A shared constant would be
// cleaner, but Drain lives in internal/app and both call sites are
// package main in different binaries; the cross-reference comment is
// the guard until a shared home exists.)
const drainTimeout = 30 * time.Second

// pollTelegram runs the Telegram gateway until ctx is cancelled,
// then waits for the cancelled in-flight turns to wind down before
// returning (F-A10-5): Poll returns on ctx cancellation without
// waiting for the turn handlers it spawned, so without the wait the
// command returns while a turn is still writing its transcript/memory
// into the data dir — and a caller that tears the data dir down (a
// test's TempDir cleanup, a supervisor restarting the daemon) races
// the write. The wait is bounded: a turn that ignores its
// (cancelled) context cannot hold shutdown forever, and the command
// then exits anyway, as it did before the wait. Honest language
// (F-A17-17): the turns are already cancelled, so this is wind-down,
// not "draining" live work — their replies are lost either way.
func pollTelegram(a *app.App, gw *messaging.TelegramGateway, ctx context.Context) error {
	// F-A16-5: SIGHUP is ignored on the long-running Telegram loops
	// (`orion daemon` and `orion telegram poll` share this
	// function), mirroring oriond: a closed terminal must not
	// silently kill polling. Short-lived commands never reach here,
	// so their SIGHUP behaviour is unchanged. The ignore is
	// process-wide and logged once so the log shows the choice was
	// deliberate.
	signal.Ignore(syscall.SIGHUP)
	fmt.Fprintln(os.Stderr, "orion: ignoring SIGHUP (terminal close will not stop polling; use SIGTERM/Ctrl+C to stop)")
	err := gw.Poll(ctx, daemonTurnHandler(a))
	if !a.Drain(drainTimeout) {
		fmt.Fprintf(os.Stderr, "orion: poll stopped; cancelled turns had not wound down after %s; exiting anyway (their replies are lost)\n", drainTimeout)
	}
	return err
}

// newTelegramGateway builds the Telegram gateway for the CLI's
// Telegram paths (`orion daemon`, `orion telegram poll`) from the
// app's config: allowlist, polling-health logger on stderr, and
// the time-boxed allow-all opt-in (P2-O5). It is the single
// construction site for both paths, so they cannot drift apart
// again (before rc11 each built its own identical gwOpts block).
func newTelegramGateway(a *app.App) (*messaging.TelegramGateway, error) {
	gwOpts := []messaging.GatewayOption{
		messaging.WithAllowedChats(a.Config.TelegramAllowedChats...),
		// Polling-health notes and dropped-chat IDs go to stderr,
		// where the rest of the CLI's lifecycle logging goes.
		messaging.WithLogger(func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "orion: "+format+"\n", args...)
		}),
	}
	if a.Config.AllowAllTelegramChats {
		// P2-O5 / F-A17-4: the opt-in is time-boxed — IsChatAllowed
		// refuses every chat once the window (from when the window
		// first opened) elapses. The deadline is persisted in the
		// data dir, so a restart resumes the recorded window
		// instead of opening a fresh one.
		gwOpts = append(gwOpts, messaging.WithAllowAllPersisted(a.Config.AllowAllWindow(), a.Config.DataDir))
	}
	return messaging.NewTelegramGateway(a.Config.TelegramToken, gwOpts...)
}

// wireGate is the shared permission-gate wiring for every
// Telegram loop in this binary (F-A6-3): `orion daemon` and
// `orion telegram poll` both call it before polling starts, and
// oriond calls the same shared constructor (app.NewPermissionGate)
// through its own buildApprovalGate — the gate is built exactly
// once per entrypoint, never copied per command, and the prompter,
// expiry hook, card tracker and tap resolver are the shared
// implementations in internal/messaging (F-A7-P2-2), so the two
// binaries cannot drift apart again. It builds the gate (rules
// under the data dir), installs it on the app, wires the prompter
// (Telegram inline-keyboard approval card, tracked so expiry can
// edit it in place) and the expiry hook, and installs the callback
// handler that resolves taps. It returns the gate and the card
// tracker; a nil gate is never returned on success, so callers can
// assert the loop is gated. The owner is the first allowlisted
// chat (see messaging.OwnerIDFromAllowlist): daemon-wide decisions
// are owner-only in oriond, and here the owner is the only
// approver besides the requester.
func wireGate(a *app.App, gw *messaging.TelegramGateway) (*permissions.Gate, *messaging.ApprovalCards, error) {
	cards := messaging.NewApprovalCards()
	ownerID := messaging.OwnerIDFromAllowlist(a.Config.TelegramAllowedChats)
	gate, err := a.NewPermissionGate(messaging.ApprovalPrompter(gw, cards, "orion"))
	if err != nil {
		return nil, nil, err
	}
	messaging.InstallApprovalExpiry(gate, gw, cards, "orion")
	// ask_user_question transport (same as oriond): the question
	// registry tracks pending questions, the gateway consults it
	// before the per-chat lock (a typed reply answers a pending
	// question), the installed callback handler routes ask: taps
	// through it, and the asker is installed on the app's tool
	// registry so per-chat agents can ask the user mid-turn.
	questions := messaging.NewQuestionRegistry(ownerID)
	gw.SetQuestionRegistry(questions)
	messaging.InstallApprovalCallbackHandler(gw, gate, ownerID, cards, questions, "orion")
	a.Tools.SetQuestionAsker(messaging.NewTelegramQuestionAsker(gw, questions, ownerID, "orion").Ask)
	if ownerID != 0 {
		fmt.Fprintf(os.Stderr, "orion: owner chat %d (first telegram_allowed_chats entry)\n", ownerID)
	}
	// Daily budget threshold alert (F-A18-11, rc24): same
	// delivery as oriond — when a chat's usage today reaches
	// the configured threshold, the owner is notified once for
	// the day. The App decides when an alert fires and formats
	// it; this closure only delivers it through the gateway.
	// With no owner configured the delivery fails with
	// ErrBudgetAlertNoOwner (F-A19-6): the App logs it once
	// per chat and does not retry it, instead of silently
	// dropping the alert.
	a.SetBudgetAlertNotifier(func(ctx context.Context, chatID int64, text string) error {
		if ownerID == 0 {
			return fmt.Errorf("orion: %w", app.ErrBudgetAlertNoOwner)
		}
		return gw.SendMessage(ctx, ownerID, text)
	})
	return gate, cards, nil
}
