package mcp

// MCP server confinement (Audit 5, F-A5-4).
//
// A server child used to run completely unconfined as the daemon
// uid: a compromised or supply-chain-malicious server could read
// ~/.orion/config.json (the DeepSeek key, the Telegram token),
// ~/.ssh, and exfiltrate over the network — none of which any
// built-in tool can do. Servers now run under the same sandbox
// shell_exec uses (see internal/sandbox): new mount/PID/IPC/UTS
// namespaces, a pivot_root into a chroot holding only /usr
// (read-only) and the sandbox work dir, and the orion-seccomp
// filter — unless the operator explicitly opted the server out
// with "sandbox": false in mcp.json (see ServerConfig.Sandbox).
//
// Consequences of the chroot, which the operator must know when
// configuring a server:
//
//   - The server binary must be visible inside the chroot: it has
//     to live under /usr (or a path that resolves there, e.g.
//     /bin, /sbin, /lib — the merged-/usr symlinks). A binary in
//     ~/.local/bin, /opt, or anywhere else is refused at Start
//     with a clear error: install it under /usr, or opt the
//     server out of the sandbox (fully trusted, like a hook).
//   - The server sees its OWN sandbox work dir
//     (<dataDir>/sandbox-mcp-<name>/work) as /work, and nothing
//     else of the host filesystem. Each server gets a separate
//     sandbox root (Audit 6, F-A6-P2-14): the old shared root
//     (<dataDir>/sandbox, the same root shell_exec uses) let a
//     sandboxed server read files a shell turn left in /work and
//     plant files a later shell_exec might run. A filesystem
//     server can only serve files under its own /work unless it
//     is opted out.
//   - The server gets the host network namespace (AllowNetwork):
//     MCP servers are network-class by design (the permission
//     gate already treats every mcp__ tool as ClassNetwork), and
//     API-backed servers cannot function without egress. The
//     network does not help exfiltrate what the server cannot
//     read: the config, keys, and home directory are outside
//     the chroot.
//   - HOME is /work, TMPDIR is /tmp, and PATH is the sandbox
//     PATH inside: a per-server PATH from mcp.json "env" is
//     overridden by the sandbox, like HOME and TMPDIR.
//
// Fail-closed: when sandboxing is on (the default) but the
// sandbox mechanism is unavailable, the server binary is not
// visible inside the chroot, or the seccomp launcher is missing,
// Start refuses the server — it never silently runs unsandboxed.
// The failure is recorded in the manager's StartErrors, like any
// other start failure, and the operator can opt that server out
// explicitly if the loss of confinement is acceptable.

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/irfan/orion/internal/sandbox"
)

// sanitizeServerName renders a server name safe for use in a
// filesystem path: only letters, digits, underscore and hyphen
// survive, everything else becomes "-". Server names are already
// validated against namePattern before Start reaches the sandbox,
// so this is defence in depth (a name must never smuggle a path
// separator into the sandbox root).
func sanitizeServerName(name string) string {
	if name == "" {
		return "server"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "server"
	}
	return b.String()
}

// sandboxRoot returns this server's sandbox root: a per-server
// root under the data dir (<dataDir>/sandbox-mcp-<name>), so each
// server gets its own /work instead of sharing shell_exec's
// <dataDir>/sandbox/work (Audit 6, F-A6-P2-14). With an empty data
// dir the root is left empty and sandbox.Resolve fills in the
// default from the home directory, as DefaultConfig does.
func (c *Client) sandboxRoot() string {
	if strings.TrimSpace(c.dataDir) == "" {
		return ""
	}
	return filepath.Join(c.dataDir, "sandbox-mcp-"+sanitizeServerName(c.name))
}

// sandboxWorkDir returns the host path of this server's /work:
// <sandboxRoot>/work. Tests use it to plant the probe server
// script where the sandbox will bind-mount it.
func (c *Client) sandboxWorkDir() string {
	if root := c.sandboxRoot(); root != "" {
		return filepath.Join(root, "work")
	}
	return ""
}

// sandboxConfig returns the sandbox configuration for an MCP
// server: the default (sandbox ON) rooted at this server's own
// per-server root — NOT the shared <dataDir>/sandbox root
// shell_exec uses (Audit 6, F-A6-P2-14) — with the host network
// namespace kept, because MCP servers are network-class and
// API-backed servers need egress to function.
func (c *Client) sandboxConfig() sandbox.Config {
	cfg := sandbox.DefaultConfig(c.dataDir)
	cfg.AllowNetwork = true
	if root := c.sandboxRoot(); root != "" {
		cfg.Root = root
	}
	return cfg
}

// serverVisibleInSandbox reports whether the resolved server
// binary path is visible inside the sandbox chroot. The chroot
// holds /usr (bind-mounted read-only) plus the merged-/usr
// symlinks /bin, /sbin, /lib, /lib64 — so a path is visible when
// it resolves (symlinks followed) to somewhere under /usr. A
// path resolving anywhere else (/opt, $HOME, /tmp) is invisible
// inside, and the server would fail to exec with a bare "file
// not found" — so Start refuses it up front with a clear error
// instead.
func serverVisibleInSandbox(path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		// The binary does not exist (or cannot be resolved):
		// not visible. Start will report the exec failure
		// itself if sandboxing is off; for a sandboxed server
		// the visibility refusal names the problem instead.
		return false
	}
	resolved = filepath.Clean(resolved)
	return resolved == "/usr" || strings.HasPrefix(resolved, "/usr/")
}

// shellQuote renders one argv token as a single-quoted shell
// word, so a server path or argument containing spaces, quotes,
// or shell metacharacters cannot break out of the command string
// the sandbox wrapper executes (see prepareSandboxed).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// prepareSandboxed confines cmd (already built with its pipes and
// scrubbed environment) to the sandbox, via
// sandbox.Config.Prepare: the command is rewritten to run the
// sandbox wrapper, which pivot_roots into the chroot and execs
// the server through /bin/sh -c. The server command travels as a
// shell string (each argv token single-quoted), because Prepare's
// interface takes the command as a string and hands it to the
// wrapper as argv — never interpolated into the wrapper script,
// so quoting in the command cannot alter the setup.
//
// Fail-closed: sandbox unavailability, an invisible binary, or a
// missing seccomp launcher all refuse the server with an error
// naming the fix (install the binary under /usr, install the
// launcher next to the oriond binary, or opt the server out with
// "sandbox": false).
func (c *Client) prepareSandboxed(cmd *exec.Cmd, path string) error {
	if !sandbox.Available() {
		err := sandbox.AvailabilityError()
		if err == nil {
			err = errors.New("sandbox mechanism unavailable for an unknown reason")
		}
		return fmt.Errorf("mcp: server %q: sandbox unavailable: %v; refusing to run unsandboxed (set \"sandbox\": false in mcp.json to run this server fully trusted, like a hook)", c.name, err)
	}
	if !serverVisibleInSandbox(path) {
		return fmt.Errorf("mcp: server %q: binary %q is not visible inside the sandbox (only /usr is mounted in the chroot); install the server under /usr, or set \"sandbox\": false in mcp.json to run it fully trusted, like a hook", c.name, path)
	}
	parts := make([]string, 0, len(c.cfg.Args)+2)
	parts = append(parts, "exec", shellQuote(path))
	for _, arg := range c.cfg.Args {
		parts = append(parts, shellQuote(arg))
	}
	if err := c.sandboxConfig().Prepare(cmd, strings.Join(parts, " ")); err != nil {
		if errors.Is(err, sandbox.ErrSeccompUnavailable) {
			return fmt.Errorf("mcp: server %q: sandbox seccomp filter unavailable: %v; install orion-seccomp next to the oriond binary, or set \"sandbox\": false in mcp.json to run this server fully trusted, like a hook", c.name, err)
		}
		return fmt.Errorf("mcp: server %q: sandbox setup failed: %w; refusing to run unsandboxed (set \"sandbox\": false in mcp.json to run this server fully trusted, like a hook)", c.name, err)
	}
	return nil
}
