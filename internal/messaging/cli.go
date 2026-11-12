// Package messaging — CLI channel.
//
// RunCLI is the local, interactive channel: it reads lines from in,
// passes each one to a handler, and prints the reply. It is the channel
// used when Orion runs in a terminal instead of (or alongside) Telegram.
package messaging

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// HandlerFunc handles one line of user input and returns the reply text.
// A non-nil error is reported to the user and the loop continues; it does
// not terminate the session.
type HandlerFunc func(ctx context.Context, input string) (string, error)

// CLIChannel is a terminal channel bound to a HandlerFunc. It is a thin
// wrapper around RunCLI so callers can carry a channel as a value.
type CLIChannel struct {
	handler HandlerFunc
}

// NewCLIChannel returns a CLIChannel using handler. handler may be nil
// here, but Run will then return an error.
func NewCLIChannel(handler HandlerFunc) *CLIChannel {
	return &CLIChannel{handler: handler}
}

// Run runs the interactive loop on in/out with the channel's handler.
// It is equivalent to RunCLI(ctx, in, out, c.handler).
func (c *CLIChannel) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	if c == nil {
		return errors.New("messaging: cli: nil CLIChannel")
	}
	return RunCLI(ctx, in, out, c.handler)
}

// maxLineBytes caps a single input line. Lines longer than this make the
// scanner fail and RunCLI returns an error rather than growing memory
// without bound.
const maxLineBytes = 64 * 1024

// RunCLI runs an interactive prompt loop:
//
//   - It prints "orion> " before processing each line it reads.
//   - Empty lines are ignored (reprompt).
//   - "/quit" or "/exit" (after trimming whitespace), or EOF on in,
//     ends the loop and returns nil. EOF prints no trailing prompt:
//     the prompt is emitted only once a line is known to be available.
//   - Any other line is passed to handler and its reply is printed.
//     A handler error is printed as "error: <err>" and the loop continues.
//
// Lines are read with a bufio.Scanner capped at 64KB per line. ctx
// cancellation is checked between lines and returns ctx.Err(); a scanner
// error is returned wrapped. in, out and handler must be non-nil.
func RunCLI(ctx context.Context, in io.Reader, out io.Writer, handler HandlerFunc) error {
	if in == nil {
		return errors.New("messaging: cli: input must not be nil")
	}
	if out == nil {
		return errors.New("messaging: cli: output must not be nil")
	}
	if handler == nil {
		return errors.New("messaging: cli: handler must not be nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 4096), maxLineBytes)

	// Lines are read in a goroutine so ctx cancellation (Ctrl-C) can
	// interrupt the loop even while it is waiting for input: the old
	// blocking scanner.Scan() ignored ctx until a line arrived or EOF.
	// The reader goroutine may stay blocked on stdin after cancellation
	// until input arrives; the loop itself returns promptly.
	type lineResult struct {
		text string
		err  error
	}
	lines := make(chan lineResult)
	go func() {
		defer close(lines)
		for scanner.Scan() {
			select {
			case lines <- lineResult{text: scanner.Text()}:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case lines <- lineResult{err: err}:
			case <-ctx.Done():
			}
		}
	}()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case res, ok := <-lines:
			if !ok {
				// EOF: the session ends without a trailing prompt.
				return nil
			}
			if res.err != nil {
				return fmt.Errorf("messaging: cli: read input: %w", res.err)
			}
			// The prompt is printed only once a line is known to be
			// available, so EOF ends the session without a trailing
			// "orion> ". (In an interactive terminal this means the
			// prompt appears as each line is processed, rather than
			// while it is being typed; the output is otherwise
			// identical.)
			if _, err := fmt.Fprint(out, "orion> "); err != nil {
				return fmt.Errorf("messaging: cli: write prompt: %w", err)
			}
			line := strings.TrimSpace(res.text)
			if line == "" {
				continue
			}
			if line == "/quit" || line == "/exit" {
				return nil
			}
			reply, err := handler(ctx, line)
			if err != nil {
				if _, werr := fmt.Fprintf(out, "error: %v\n", err); werr != nil {
					return fmt.Errorf("messaging: cli: write error: %w", werr)
				}
				continue
			}
			if _, err := fmt.Fprintln(out, reply); err != nil {
				return fmt.Errorf("messaging: cli: write reply: %w", err)
			}
		}
	}
}
