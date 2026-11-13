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
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
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
//
// Proxy variables get special treatment (P2-O6): a proxy URL often
// carries credentials (https://user:password@proxy:8080), and the
// plain host:port form is needed for egress to work at all, so proxy
// variables are stripped ONLY when they carry credentials — either
// the name says so (PROXY_USER, PROXY_PASSWORD, ...) or the value
// has userinfo before the host. A credential-less proxy (plain
// http://proxy:8080, or a NO_PROXY host list) is kept, so proxied
// egress keeps working.
//
// The scrub is two-legged (Audit 6, F-A6-P2-20): name-based (the
// rules above — a variable whose NAME looks like a credential is
// dropped) AND value-shape-based (a variable whose VALUE looks
// like a known secret shape is dropped, whatever its name is —
// MY_NOTE=sk-... must not reach a child just because the name is
// innocuous). The value patterns are the well-known provider
// prefixes from the secret scanner (security/scanner.go), kept as
// a focused list on purpose: a generic high-entropy detector
// would also drop legitimate long values (base64 config blobs,
// document IDs), so only shapes that are secrets by construction
// are matched. Dropping is strictly redacting: no variable the
// old scrub kept is newly kept, and a dropped variable is gone
// entirely — no partial secret can leak through a surrounding
// value.
func scrubEnv() []string {
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		key, value, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "ORION_") ||
			strings.Contains(upper, "API_KEY") ||
			strings.Contains(upper, "TOKEN") ||
			strings.Contains(upper, "SECRET") ||
			strings.Contains(upper, "PASSWORD") ||
			strings.Contains(upper, "PASSWD") ||
			strings.Contains(upper, "KEY") ||
			strings.Contains(upper, "CREDENTIAL") ||
			strings.Contains(upper, "AUTH") ||
			strings.Contains(upper, "BEARER") {
			continue
		}
		if strings.Contains(upper, "PROXY") && proxyCarriesCredentials(upper, value) {
			continue
		}
		if valueLooksLikeSecret(value) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// secretValuePatterns are secret shapes matched against an
// environment variable's VALUE, whatever the variable is named
// (Audit 6, F-A6-P2-20). Each pattern is a provider prefix that
// identifies a credential by construction, mirroring the
// detectors in security/scanner.go: OpenAI/Anthropic API keys,
// Stripe keys, GitHub tokens, AWS access keys, Slack tokens,
// Telegram bot tokens, JWTs, and private-key blocks. The match is
// a substring match — a secret embedded in a longer value (a note
// containing a pasted key) still drops the variable.
var secretValuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`(?i)(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{8,}`),
	regexp.MustCompile(`(?:ghp_|gho_|ghu_|ghs_|ghr_)[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`),
	regexp.MustCompile(`xox[baprse]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\b\d{8,10}:[A-Za-z0-9_-]{30,}\b`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
}

// valueLooksLikeSecret reports whether an environment variable
// value contains a known secret shape (see
// secretValuePatterns). It is the value leg of the ScrubEnv
// scrub: a secret-shaped value under an innocuous name is still a
// secret, and must not reach a child process.
func valueLooksLikeSecret(value string) bool {
	for _, re := range secretValuePatterns {
		if re.MatchString(value) {
			return true
		}
	}
	return false
}

// proxyCarriesCredentials reports whether a proxy environment variable
// carries credentials: either the name itself names a credential
// (PROXY_USER, PROXY_PASS, ...) or the value is a proxy URL with
// userinfo (scheme://user[:password]@host). A bare host list
// (NO_PROXY) or a credential-less URL has none.
func proxyCarriesCredentials(upperKey, value string) bool {
	for _, marker := range []string{"USER", "PASS", "TOKEN", "SECRET", "CREDENTIAL", "AUTH"} {
		if strings.Contains(upperKey, marker) {
			return true
		}
	}
	// userinfo is the part between "://" and the first "@" before
	// the first "/" after the scheme. A "@" anywhere else (a path
	// segment, a bare hostname) is not credentials.
	rest := value
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	} else {
		return false
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return strings.Contains(rest, "@")
}

// cappedWriter collects output up to limit bytes and discards the
// rest, remembering that it truncated. The old code buffered without
// limit and only capped after Run returned, so a flooding command
// (e.g. `yes`) could exhaust memory before the timeout fired; with
// this writer the buffer never grows past the cap. The command itself
// is left to run to completion (or its timeout): a command that
// produces more than the cap is a truncated success, not an error,
// so legitimate large output still reaches the model with a marker.
// total counts every byte the command produced, including discarded
// ones, so the marker can report the true output size.
type cappedWriter struct {
	buf      *bytes.Buffer
	limit    int
	exceeded bool
	total    int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.total += len(p)
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
// additional layer, never a replacement for it. Sandboxing is ON
// by default in production: the registry gets cfg.Sandbox from
// config.Load, whose default enables it, and only an explicit
// operator opt-out (sandbox.enabled=false, or ORION_SANDBOX=off)
// turns it off. A zero config means sandboxing is off — that is
// the unconfigured value used by tests and by direct registry
// construction, never a production default; do not rely on it to
// run unsandboxed in production.
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
	// Sandbox mode (ON by default via config; see
	// sandbox.DefaultConfig): confine the command to the namespace
	// chroot. The denylist above has already run, so this is a
	// second layer, not a substitute. Fail-closed by default:
	// when the sandbox is enabled but cannot be established
	// (mechanism unavailable, or setup failed), the command is
	// REFUSED rather than run unsandboxed — an operator who opted
	// into isolation must not silently lose it. Only an explicit
	// opt-in (sandbox.allow_unsandboxed_fallback=true) restores
	// the old warn-and-run behaviour.
	//
	// A missing seccomp filter is a DIFFERENT degradation from a
	// sandbox that cannot be established at all, and it is governed
	// by a different opt-in (sandbox.allow_without_seccomp), never
	// by allow_unsandboxed_fallback: the fallback opt-in covers
	// sandbox-establishment failure, and letting it silently run
	// the command fully unsandboxed when only the filter is
	// missing would be a worse degradation than sandboxed without
	// a filter — and would make the filter opt-in meaningless.
	// So a filter-absence error (sandbox.ErrSeccompUnavailable)
	// is ALWAYS refused here, even with
	// AllowUnsandboxedFallback set; only AllowWithoutSeccomp (or
	// DisableSeccomp) lets a filterless run happen, and that
	// decision is made in Prepare, not here.
	sandboxWarn := ""
	if t.sandboxCfg.Enabled {
		var sandboxErr error
		if !sandbox.Available() {
			if availErr := sandbox.AvailabilityError(); availErr != nil {
				sandboxErr = fmt.Errorf("sandbox unavailable: %w", availErr)
			} else {
				sandboxErr = errors.New("sandbox unavailable: unknown reason")
			}
		} else if err := t.sandboxCfg.Prepare(cmd, cmdStr); err != nil {
			sandboxErr = fmt.Errorf("sandbox setup failed: %w", err)
		}
		if sandboxErr != nil {
			if errors.Is(sandboxErr, sandbox.ErrSeccompUnavailable) {
				// The wrapped error already names the missing
				// launcher, the refusal, and the opt-in; repeat
				// none of it here.
				msg := sandboxErr.Error()
				log.Printf("orion: shell_exec: %s", msg)
				return "", &ToolError{Tool: t.Name(), Kind: "denied", Err: errors.New(msg)}
			}
			if t.sandboxCfg.AllowUnsandboxedFallback {
				sandboxWarn = fmt.Sprintf("%v; ran without sandbox isolation", sandboxErr)
				log.Printf("orion: shell_exec: %s", sandboxWarn)
			} else {
				msg := fmt.Sprintf("%v; refusing to run unsandboxed (sandbox is enabled) — set sandbox.allow_unsandboxed_fallback=true to allow unsandboxed fallback", sandboxErr)
				log.Printf("orion: shell_exec: %s", msg)
				return "", &ToolError{Tool: t.Name(), Kind: "denied", Err: errors.New(msg)}
			}
		}
	} else {
		// Sandbox disabled: with the default now ON, a disabled
		// sandbox in production means the operator explicitly
		// opted out (sandbox.enabled=false, or
		// ORION_SANDBOX=off). Log every unsandboxed run, so the
		// opt-out — or a config that silently lost its sandbox
		// setting — is visible in the log, not just in the file.
		// (A zero sandbox.Config in tests hits this branch too;
		// the log line is the same fact: no isolation.)
		log.Printf("orion: shell_exec: sandbox disabled; running command without sandbox isolation (set sandbox.enabled=true to confine shell_exec)")
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
		// Keep the "[truncated]" marker (existing callers and tests
		// match on it) and add the true output size, which the
		// writer counted even for discarded bytes.
		sb.WriteString(fmt.Sprintf("\n[truncated] (output exceeded %d bytes; total output was %d bytes)", maxShellOutput, out.total))
	}
	if sandboxWarn != "" {
		sb.WriteString("\n[warning: " + sandboxWarn + "]")
	}
	return sb.String(), nil
}
