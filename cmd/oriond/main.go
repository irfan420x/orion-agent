// Command oriond is Orion's daemon: it wires the app and serves the
// Telegram gateway until interrupted. CLI mode lives in cmd/orion.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/irfan/orion/internal/app"
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
	gw, err := messaging.NewTelegramGateway(a.Config.TelegramToken, messaging.WithAllowedChats(a.Config.TelegramAllowedChats...))
	if err != nil {
		fmt.Fprintf(os.Stderr, "oriond: %v\n", err)
		return 1
	}
	if len(a.Config.TelegramAllowedChats) == 0 {
		fmt.Fprintln(os.Stderr, "oriond: WARNING: telegram_allowed_chats is empty — any Telegram chat can drive the agent; set telegram_allowed_chats in config to restrict it")
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
	if err := gw.Poll(ctx, func(c context.Context, chatID int64, text string) (string, error) {
		return a.Handle(c, text)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "oriond: %v\n", err)
		return 1
	}
	return 0
}
