// Package tools — shell_exec tool.
//
// Runs a shell command via /bin/sh -c under a context timeout, after
// consulting the canonical security denylist (security.IsBlockedCommand).
// Output is capped; the exit code is reported in the result string.
//
// The child process does NOT inherit the agent's secrets: the environment
// is scrubbed of API keys, tokens, and passwords before exec, so
// `shell_exec {"command":"env"}` can never leak them into tool output.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/sandbox"
	"github.com/irfan/orion/internal/security"
)

const (
	defaultShellTimeout = 30
	maxShellTimeout     = 120
	maxShellOutput      = 64 * 1024 // 64KB
)

// scrubEnv returns the process environment with secrets removed, for
// the shell child: anything that looks like a credential never reaches
// a command the model asked to run.
func scrubEnv() []string {
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "ORION_") ||
			strings.Contains(upper, "API_KEY") ||
			strings.Contains(upper, "TOKEN") ||
			strings.Contains(upper, "SECRET") ||
			strings.Contains(upper, "PASSWORD") ||
			strings.Contains(upper, "PASSWD") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// cappedWriter collects output up to limit bytes and discards the
// rest, remembering that it truncated. The old code buffered without
// limit and only capped after Run returned, so a flooding command
// (e.g. `yes`) could exhaust memory before the timeout fired; with
// this writer the buffer never grows past the cap. The command itself
// is left to run to completion (or its timeout): a command that
// produces more than the cap is a truncated success, not an error,
// so legitimate large output still reaches the model with a marker.
type cappedWriter struct {
	buf      *bytes.Buffer
	limit    int
	exceeded bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if w.exceeded {
		return len(p), nil
	}
	room := w.limit - w.buf.Len()
	if room <= 0 {
		w.exceeded = true
		return len(p), nil
	}
	if len(p) > room {
		w.buf.Write(p[:room])
		w.exceeded = true
		return len(p), nil
	}
	return w.buf.Write(p)
}

// shellExecTool implements "shell_exec". sandboxCfg, when enabled,
// confines the command to the sandbox (see internal/sandbox); the
// denylist check below still runs first, so the sandbox is an
// additional layer, never a replacement for it. A zero config means
// sandboxing is off and behaviour is unchanged.
type shellExecTool struct {
	sandboxCfg sandbox.Config
}

func (t *shellExecTool) Name() string { return "shell_exec" }

func (t *shellExecTool) Description() string {
	return "Run a shell command via /bin/sh -c and return its combined stdout/stderr plus the exit code. Blocked destructive commands are refused."
}

func (t *shellExecTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Shell command to run.",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"description": "Timeout in seconds (default 30, max 120).",
					"minimum":     1,
					"maximum":     120,
				},
			},
			"required": []string{"command"},
		},
	}
}

type shellExecArgs struct {
	Command        string `json:"command"`
	TimeoutSeconds *int   `json:"timeout_seconds,omitempty"`
}

func (t *shellExecTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a shellExecArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("invalid JSON arguments: %w", err)}
	}
	cmdStr := strings.TrimSpace(a.Command)
	if cmdStr == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("command must not be empty")}
	}
	timeout := defaultShellTimeout
	if a.TimeoutSeconds != nil {
		timeout = *a.TimeoutSeconds
	}
	if timeout < 1 || timeout > maxShellTimeout {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("timeout_seconds must be between 1 and %d", maxShellTimeout)}
	}

	// Denylist check first — delegate to the canonical implementation.
	if security.IsBlockedCommand(cmdStr) {
		return "", &ToolError{Tool: t.Name(), Kind: "denied", Err: fmt.Errorf("command blocked by security policy")}
	}

	// Never run if the context is already cancelled.
	if err := ctx.Err(); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: fmt.Errorf("context cancelled before execution: %w", err)}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, timeoutSecondsToDuration(timeout))
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "/bin/sh", "-c", cmdStr)
	// The child gets a scrubbed environment: no API keys, tokens, or
	// passwords, so they can never leak into tool output.
	cmd.Env = scrubEnv()
	// Run the command in its own process group so a timeout kills the
	// whole tree (e.g. `sleep` spawned by sh), not just the shell.
	// Without this, cmd.Run can block until a grandchild holding the
	// stdout pipe exits, even after the shell itself is dead.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Sandbox mode (opt-in via config): confine the command to the
	// namespace chroot. The denylist above has already run, so this
	// is a second layer, not a substitute. When the mechanism is
	// unavailable (or setup fails), fall back to running unsandboxed
	// with a warning — the denylist still applies — rather than
	// refusing to run at all.
	sandboxWarn := ""
	if t.sandboxCfg.Enabled {
		if !sandbox.Available() {
			sandboxWarn = fmt.Sprintf("sandbox unavailable (%v); ran without sandbox isolation", sandbox.AvailabilityError())
			log.Printf("orion: shell_exec: %s", sandboxWarn)
		} else if err := t.sandboxCfg.Prepare(cmd, cmdStr); err != nil {
			sandboxWarn = fmt.Sprintf("sandbox setup failed (%v); ran without sandbox isolation", err)
			log.Printf("orion: shell_exec: %s", sandboxWarn)
		}
	}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	// Bound how long Run may wait for pipe-copying goroutines after kill.
	cmd.WaitDelay = 2 * time.Second
	var buf bytes.Buffer
	out := &cappedWriter{buf: &buf, limit: maxShellOutput}
	cmd.Stdout = out
	cmd.Stderr = out

	err := cmd.Run()
	output := buf.String()
	truncated := out.exceeded
	if len(output) > maxShellOutput {
		output = output[:maxShellOutput]
		truncated = true
	}

	// Timeout takes precedence over a plain non-zero exit.
	if timeoutCtx.Err() == context.DeadlineExceeded {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: fmt.Errorf("command timed out after %d seconds", timeout)}
	}
	if ctx.Err() != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: fmt.Errorf("context cancelled during execution: %w", ctx.Err())}
	}

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("failed to run command: %w", err)}
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("exit_code: %d\n", exitCode))
	sb.WriteString("output:\n")
	sb.WriteString(output)
	if truncated {
		sb.WriteString("\n[truncated]")
	}
	if sandboxWarn != "" {
		sb.WriteString("\n[warning: " + sandboxWarn + "]")
	}
	return sb.String(), nil
}
