// orion sessions — list, resume, and start CLI/global sessions.
//
// Sessions are the per-session JSONL transcripts (audit P0 #4):
// one file per session under <DataDir>/sessions/, with the live
// session recorded in sessions/current.json. This command works
// through the provider-free Local wiring (BuildLocal), so listing
// or resuming a session never needs a provider API key and never
// calls a model: resume just loads the transcript's messages into
// memory.json and re-points current.json, and the next `orion run`
// (which builds the full App and wires the live transcript) picks
// the resumed session up. `orion sessions new` rotates to a fresh
// session the same way App.NewSession does, minus the live agent.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/session"
)

func sessionsUsage() {
	fmt.Fprintf(os.Stderr, `Usage:
  orion sessions            list CLI sessions (newest first)
  orion sessions list       list CLI sessions (newest first)
  orion sessions resume <id|prefix>
                            resume a session: its transcript becomes the
                            working history and the live session
  orion sessions new        start a fresh session (history cleared,
                            facts kept; the old transcript stays saved)
`)
}

func cmdSessions(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "list":
			return cmdSessionsList()
		case "resume":
			if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
				sessionsUsage()
				return fmt.Errorf("sessions resume: provide a session id (or a unique prefix of one)")
			}
			if len(args) > 2 {
				sessionsUsage()
				return fmt.Errorf("sessions resume: takes exactly one session id")
			}
			return cmdSessionsResume(args[1])
		case "new":
			if len(args) > 1 {
				sessionsUsage()
				return fmt.Errorf("sessions new: takes no arguments")
			}
			return cmdSessionsNew()
		case "--help", "-h":
			sessionsUsage()
			return nil
		default:
			sessionsUsage()
			return fmt.Errorf("sessions: unknown subcommand %q", args[0])
		}
	}
	return cmdSessionsList()
}

func cmdSessionsList() error {
	cfg, err := config.Load("")
	if err != nil {
		return fmt.Errorf("sessions: %w", err)
	}
	infos, err := session.List(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("sessions: list: %w", err)
	}
	current, _ := session.ReadCurrent(cfg.DataDir)
	if len(infos) == 0 {
		fmt.Println("No saved sessions yet.")
		return nil
	}
	for _, info := range infos {
		live := ""
		if info.ID == current {
			live = " (live)"
		}
		title := ""
		if info.Title != "" {
			title = fmt.Sprintf(" — %q", info.Title)
		}
		fmt.Printf("%s%s — %d message(s), %d summaries, updated %s%s\n",
			info.ID, live, info.Messages, info.Summaries,
			info.UpdatedAt.Format(time.RFC3339), title)
	}
	return nil
}

func cmdSessionsResume(idOrPrefix string) error {
	local, err := buildLocal()
	if err != nil {
		return fmt.Errorf("sessions resume: %w", err)
	}
	defer local.Close()
	cfg := local.Config
	full, err := session.ResolvePrefix(cfg.DataDir, idOrPrefix)
	if err != nil {
		return fmt.Errorf("sessions resume: %w", err)
	}
	tr, err := session.Open(cfg.DataDir, full)
	if err != nil {
		return fmt.Errorf("sessions resume: open transcript: %w", err)
	}
	msgs, err := tr.Messages()
	if err != nil {
		return fmt.Errorf("sessions resume: read transcript: %w", err)
	}
	if err := local.Memory.ReplaceMessages(msgs); err != nil {
		return fmt.Errorf("sessions resume: load history: %w", err)
	}
	if err := session.WriteCurrent(cfg.DataDir, full); err != nil {
		return fmt.Errorf("sessions resume: record current session: %w", err)
	}
	fmt.Printf("Resumed session %s (%d message(s) in history).\n", full, len(msgs))
	return nil
}

func cmdSessionsNew() error {
	local, err := buildLocal()
	if err != nil {
		return fmt.Errorf("sessions new: %w", err)
	}
	defer local.Close()
	cfg := local.Config
	if err := local.Memory.ClearMessages(); err != nil {
		return fmt.Errorf("sessions new: clear history: %w", err)
	}
	id := session.NewID()
	if err := session.WriteCurrent(cfg.DataDir, id); err != nil {
		return fmt.Errorf("sessions new: record current session: %w", err)
	}
	if _, err := session.Open(cfg.DataDir, id); err != nil {
		return fmt.Errorf("sessions new: open transcript: %w", err)
	}
	fmt.Printf("Started new session %s. Conversation history cleared; facts kept.\n", id)
	return nil
}
