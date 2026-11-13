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
	"sort"
	"strings"
	"syscall"

	"github.com/irfan/orion/internal/app"
	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/messaging"
	"github.com/irfan/orion/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	case "telegram":
		err = cmdTelegram(ctx, os.Args[2:])
	case "daemon":
		err = cmdDaemon(ctx, os.Args[2:])
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
// run, or a subagent wait) is also 0 and silent; any other error is 1.
func exitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, context.Canceled) {
		return 0
	}
	return 1
}

func usage() { usageTo(os.Stdout) }

// usageTo writes the top-level usage text to w: stdout for --help,
// stderr when it follows an error (unknown command).
func usageTo(w io.Writer) {
	fmt.Fprintf(w, `orion — model-agnostic OS agent (v%s)

Usage:
  orion run "prompt"          run one prompt through the agent
  orion chat                  interactive CLI chat (same agent, same memory)
  orion config show           show config (secrets masked)
  orion config set-provider <openai|anthropic|ollama>
  orion config set-model <model>
  orion config set-api-key <openai|anthropic|ollama> <key>
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
  orion telegram test         verify the Telegram bot token (getMe)
  orion telegram poll         start the Telegram gateway (polling)
  orion daemon                run Telegram polling until interrupted
  orion version               print version
`, version.Version)
}

// isHelpArg reports whether s asks for per-command help.
func isHelpArg(s string) bool { return s == "--help" || s == "-h" }

func buildApp() (*app.App, error) { return app.Build("") }

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
		fmt.Print(`Usage: orion run "prompt"

Run one prompt through the agent and print the reply. All arguments
are joined with spaces into a single prompt.
`)
		return nil
	}
	if len(args) == 0 {
		return fmt.Errorf("run: provide a prompt, e.g. orion run \"what time is it?\"")
	}
	a, err := buildApp()
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
  orion config set-provider <openai|anthropic|ollama>
  orion config set-model <model>
  orion config set-api-key <openai|anthropic|ollama> <key>
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
			return fmt.Errorf("config set-provider: provide a provider name (openai|anthropic|ollama)")
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
			return fmt.Errorf("config set-api-key: usage: orion config set-api-key <openai|anthropic|ollama> <key>")
		}
		if len(args) > 3 {
			return fmt.Errorf("config set-api-key: takes exactly a provider name and a key")
		}
		key, ok := providerKey(cfg, args[1])
		if !ok {
			return fmt.Errorf("config set-api-key: unknown provider %q", args[1])
		}
		p := cfg.Providers[key]
		if strings.TrimSpace(args[2]) == "" {
			return fmt.Errorf("config set-api-key: key must not be empty")
		}
		p.APIKey = args[2]
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
	if a.Config.TelegramToken == "" {
		return fmt.Errorf("telegram: no token configured (set ORION_TELEGRAM_TOKEN or config telegram_token)")
	}
	gw, err := messaging.NewTelegramGateway(a.Config.TelegramToken,
		messaging.WithAllowedChats(a.Config.TelegramAllowedChats...),
		// Polling-health notes and dropped-chat IDs go to stderr,
		// where the rest of the CLI's lifecycle logging goes.
		messaging.WithLogger(func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "orion: "+format+"\n", args...)
		}))
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
		fmt.Println("telegram: polling for messages (Ctrl+C to stop)...")
		return gw.Poll(ctx, func(c context.Context, chatID int64, text string) (string, error) {
			return a.Handle(c, text)
		})
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
	if a.Config.TelegramToken == "" {
		return fmt.Errorf("daemon: no telegram token configured; run `orion chat` for CLI mode instead")
	}
	gw, err := messaging.NewTelegramGateway(a.Config.TelegramToken,
		messaging.WithAllowedChats(a.Config.TelegramAllowedChats...),
		// Polling-health notes and dropped-chat IDs go to stderr,
		// where the rest of the CLI's lifecycle logging goes.
		messaging.WithLogger(func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "orion: "+format+"\n", args...)
		}))
	if err != nil {
		return err
	}
	// An empty allowlist refuses startup (see
	// messaging.CheckTelegramAllowlist): polling with no allowlist
	// would let any Telegram chat drive the agent.
	if err := messaging.CheckTelegramAllowlist(a.Config.TelegramAllowedChats, a.Config.AllowAllTelegramChats); err != nil {
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
	return gw.Poll(ctx, func(c context.Context, chatID int64, text string) (string, error) {
		return a.Handle(c, text)
	})
}
