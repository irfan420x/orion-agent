// Package sandbox confines shell_exec commands to a private filesystem
// view, so code the denylist cannot see (aliased or dynamic interpreter
// code, a bare interpreter, a script file, stdin) cannot destroy data
// outside the sandbox root.
//
// Mechanism (chosen after surveying this VM on 2026-11-13):
//
//   - bwrap and firejail are NOT installed; systemd-run --user has no
//     D-Bus to talk to ("Failed to connect to bus"). The only
//     container mechanism this host offers is Linux namespaces, and
//     those work: unshare -rm and unshare -rmn both succeed as root.
//   - So the sandbox is built from namespaces directly, with no new
//     dependency: the child is started with CLONE_NEWUSER (uid/gid 0
//     mapped to the agent's own uid/gid), CLONE_NEWNS, and — unless
//     AllowNetwork is set — CLONE_NEWNET (a network namespace whose
//     only interface is a down loopback: no network at all).
//   - Inside the namespaces a small shell wrapper (run before any
//     user code) makes mounts private, then builds a chroot root
//     under <root>/.orion-root: /usr bind-mounted read-only, a
//     minimal /etc copied from a small allowlist (never bind-mounted:
//     the host /etc contains runtime submounts, including a
//     credentials tmpfs, and bind-mounting it is refused in a nested
//     user namespace), the harmless device nodes (/dev/null, zero,
//     full, random, urandom, tty) bind-mounted individually, a tmpfs
//     on /tmp, and <root>/work bind-mounted read-write as /work.
//     The chroot root itself is remounted read-only (via --rbind,
//     which replicates the submounts — a plain self-bind drops them).
//     The command then runs as /bin/sh -c inside the chroot with cwd
//     /work, HOME=/work, TMPDIR=/tmp, and the agent's scrubbed env.
//   - The only writable places inside are /work (which persists on
//     the host at <root>/work) and /tmp (a per-run tmpfs that
//     vanishes with the namespace). /home, /etc, /usr and every other
//     host path are either absent or read-only, so even `rm -rf /`
//     inside can only destroy the sandbox's own contents.
//
// Limits (see SECURITY.md for the full list):
//
//   - Nested uid/gid mapping is blocked for unprivileged users on
//     this VM (the cell itself is a user-namespaced container; writing
//     uid_map as nobody fails with EPERM), so the sandbox only works
//     when the agent runs as root, or on a host that allows
//     unprivileged user namespaces. Available() probes the real
//     mechanism and reports false otherwise; shell_exec then falls
//     back to the denylist alone, with a warning.
//   - The sandbox is an ADDITIONAL layer, never a replacement: the
//     token-based IsBlockedCommand denylist still runs first.
//   - /proc and /sys are empty directories inside: no host process
//     or kernel information leaks in, and tools that need them fail.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Config controls sandbox mode for shell_exec. It is opt-in: Enabled
// defaults to false and behaviour is unchanged unless configured.
type Config struct {
	// Enabled turns sandbox mode on for shell_exec.
	Enabled bool `json:"enabled"`
	// Root is the sandbox root on the host. <root>/work is the
	// command's working directory inside the sandbox (visible as
	// /work); <root>/.orion-root holds the chroot tree. Empty means
	// the default (see Resolve).
	Root string `json:"root"`
	// AllowNetwork, when true, keeps the host network namespace
	// instead of creating an empty one. Default false: no network.
	AllowNetwork bool `json:"allow_network"`
}

// DefaultRoot returns the default sandbox root under dataDir.
func DefaultRoot(dataDir string) string {
	return filepath.Join(dataDir, "sandbox")
}

// Resolve returns c with defaults filled in: an empty Root becomes
// <dataDir>/sandbox when dataDir is set, else ~/.orion/sandbox.
func (c Config) Resolve(dataDir string) Config {
	if strings.TrimSpace(c.Root) != "" {
		return c
	}
	if strings.TrimSpace(dataDir) != "" {
		c.Root = DefaultRoot(dataDir)
		return c
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// No home and no data dir: leave Root empty; Prepare will
		// fail with a clear error instead of guessing a relative path.
		return c
	}
	c.Root = filepath.Join(home, ".orion", "sandbox")
	return c
}

// systemRoots are host trees the sandbox root must never be: a root
// of / would make Prepare create /work and /.orion-root on the host
// root, and a root inside a system tree would put sandbox state
// where the system owns the directory.
var systemRoots = []string{
	"/etc", "/usr", "/bin", "/sbin", "/var", "/opt", "/root",
	"/boot", "/dev", "/proc", "/sys", "/lib", "/lib64", "/run", "/srv",
}

// Validate rejects a sandbox root that is unsafe or unusable. An
// empty Root is allowed (it means "use the default" via Resolve).
func (c Config) Validate() error {
	root := strings.TrimSpace(c.Root)
	if root == "" {
		return nil
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("sandbox: root %q must be an absolute path", c.Root)
	}
	clean := filepath.Clean(root)
	if clean == "/" {
		return errors.New("sandbox: root must not be \"/\"")
	}
	for _, sys := range systemRoots {
		if clean == sys || strings.HasPrefix(clean, sys+"/") {
			return fmt.Errorf("sandbox: root %q must not be inside system tree %q", c.Root, sys)
		}
	}
	return nil
}

// etcAllowlist is the set of host /etc files copied into the sandbox.
// /etc itself is never bind-mounted: it contains runtime submounts
// (including a credentials tmpfs) and the bind is refused inside a
// nested user namespace. The copies are made once, when missing.
var etcAllowlist = []string{
	"passwd", "group", "hosts", "resolv.conf", "nsswitch.conf",
}

// devAllowlist is the set of device nodes bind-mounted individually
// into the sandbox's /dev. /dev itself is never bind-mounted: it is
// a tmpfs with submounts (pts, shm) that refuse the same way.
var devAllowlist = []string{
	"null", "zero", "full", "random", "urandom", "tty",
}

// wrapperScript runs inside the new namespaces, before any user
// code. It is passed to /bin/sh -c with $1 = sandbox root and
// $2 = the user command (as argv, never interpolated, so quoting in
// the command cannot break out of the setup). It exits 126 if any
// setup step fails, so a half-built sandbox never runs user code.
const wrapperScript = `
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ROOT=$1
CMD=$2
mount --make-rprivate / || exit 126
NEWROOT="$ROOT/.orion-root"
mkdir -p "$NEWROOT/usr" "$NEWROOT/etc" "$NEWROOT/dev" "$NEWROOT/tmp" "$NEWROOT/proc" "$NEWROOT/work" || exit 126
mount --bind /usr "$NEWROOT/usr" || exit 126
mount -o remount,bind,ro,nosuid,nodev "$NEWROOT/usr" || exit 126
for d in null zero full random urandom tty; do
  if [ -e "/dev/$d" ]; then
    touch "$NEWROOT/dev/$d" || exit 126
    mount --bind "/dev/$d" "$NEWROOT/dev/$d" || exit 126
  fi
done
mount -t tmpfs -o nosuid,nodev,noexec tmpfs "$NEWROOT/tmp" || exit 126
mount --bind "$ROOT/work" "$NEWROOT/work" || exit 126
mount --rbind "$NEWROOT" "$NEWROOT" || exit 126
mount -o remount,bind,ro,nosuid,nodev "$NEWROOT" || exit 126
exec chroot "$NEWROOT" /bin/sh -c 'cd /work || exit 126; exec /bin/sh -c "$1"' _ "$CMD"
`

var (
	probeOnce sync.Once
	probeErr  error
)

// Available reports whether the sandbox mechanism works on this host.
// It probes the real thing once (a trivial command through the full
// wrapper) and caches the result for the process lifetime.
func Available() bool {
	probeOnce.Do(runProbe)
	return probeErr == nil
}

// AvailabilityError returns nil when the sandbox is available, else
// the reason the probe failed. It is only meaningful after Available
// has been called (or it runs the probe itself).
func AvailabilityError() error {
	probeOnce.Do(runProbe)
	return probeErr
}

func runProbe() {
	dir, err := os.MkdirTemp("", "orion-sandbox-probe-*")
	if err != nil {
		probeErr = fmt.Errorf("sandbox: probe temp dir: %w", err)
		return
	}
	defer os.RemoveAll(dir)
	cfg := Config{Enabled: true, Root: filepath.Join(dir, "root")}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "true")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if err := cfg.Prepare(cmd, "true"); err != nil {
		probeErr = fmt.Errorf("sandbox: probe setup: %w", err)
		return
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		probeErr = fmt.Errorf("sandbox: probe command failed: %w", err)
		return
	}
}

// Prepare rewrites cmd (a /bin/sh -c <command> invocation) to run
// inside the sandbox: new user, mount and (unless AllowNetwork)
// network namespaces, then the wrapper builds the chroot and runs
// the command there. It creates the sandbox root, its work dir, the
// chroot tree, the minimal /etc copies and the merged-/usr symlinks
// on the host first; those are plain directories and idempotent, so
// concurrent sandboxed commands can share one root safely.
//
// cmd.Env should already hold the caller's scrubbed environment when
// Prepare is called; Prepare overrides HOME, TMPDIR and PATH to the
// in-sandbox values. The caller's SysProcAttr (e.g. Setpgid) is
// preserved and extended with the namespace clone flags.
func (c Config) Prepare(cmd *exec.Cmd, command string) error {
	if !c.Enabled {
		return errors.New("sandbox: Prepare called on a disabled config")
	}
	cfg := c.Resolve("")
	if err := cfg.Validate(); err != nil {
		return err
	}
	root := cfg.Root
	if root == "" {
		return errors.New("sandbox: no root configured and no home directory to default from")
	}
	newRoot := filepath.Join(root, ".orion-root")
	dirs := []string{
		filepath.Join(root, "work"),
		filepath.Join(newRoot, "usr"),
		filepath.Join(newRoot, "etc"),
		filepath.Join(newRoot, "dev"),
		filepath.Join(newRoot, "tmp"),
		filepath.Join(newRoot, "proc"),
		filepath.Join(newRoot, "work"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("sandbox: create %s: %w", d, err)
		}
	}
	// Minimal /etc, copied once when missing (see etcAllowlist).
	for _, name := range etcAllowlist {
		dst := filepath.Join(newRoot, "etc", name)
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/etc", name))
		if err != nil {
			continue // file absent on this host; not fatal
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return fmt.Errorf("sandbox: copy /etc/%s: %w", name, err)
		}
	}
	// Symlinks for the merged-/usr layout, so /bin, /sbin, /lib and
	// /lib64 resolve inside the chroot. Created here (not in the
	// wrapper) so concurrent runs cannot race on them.
	for _, link := range []struct{ name, target string }{
		{"bin", "usr/bin"},
		{"sbin", "usr/sbin"},
		{"lib", "usr/lib"},
		{"lib64", "usr/lib64"},
	} {
		if err := ensureSymlink(filepath.Join(newRoot, link.name), link.target); err != nil {
			return fmt.Errorf("sandbox: symlink %s: %w", link.name, err)
		}
	}
	for _, lib := range []string{"lib32", "libx32"} {
		if _, err := os.Stat(filepath.Join("/usr", lib)); err != nil {
			continue
		}
		if err := ensureSymlink(filepath.Join(newRoot, lib), "usr/"+lib); err != nil {
			return fmt.Errorf("sandbox: symlink %s: %w", lib, err)
		}
	}

	cloneFlags := uintptr(syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS)
	if !cfg.AllowNetwork {
		cloneFlags |= uintptr(syscall.CLONE_NEWNET)
	}
	attr := cmd.SysProcAttr
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}
	attr.Cloneflags |= cloneFlags
	// Map the agent's own uid/gid to root inside the namespace:
	// that is what grants the namespaced process the capabilities
	// (mount, chroot) it needs, and only inside its own namespaces.
	attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
	attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
	cmd.SysProcAttr = attr

	// The user command travels as argv, never inside the script
	// string, so no quoting in it can alter the setup.
	cmd.Path = "/bin/sh"
	cmd.Args = []string{"/bin/sh", "-c", wrapperScript, "orion-sandbox", root, command}
	// No Dir: the host cwd is meaningless after chroot; the wrapper
	// cds to /work inside.
	cmd.Dir = ""
	cmd.Env = setEnv(cmd.Env, "HOME", "/work")
	cmd.Env = setEnv(cmd.Env, "TMPDIR", "/tmp")
	cmd.Env = setEnv(cmd.Env, "PATH", "/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin")
	return nil
}

// ensureSymlink creates a symlink at path pointing at target, leaving
// a correct one alone and replacing a stale one. Concurrent Prepares
// sharing one root can race here (both see the link missing, one
// Symlink gets EEXIST), so an EEXIST retries the check instead of
// failing.
func ensureSymlink(path, target string) error {
	for attempt := 0; attempt < 3; attempt++ {
		if fi, err := os.Lstat(path); err == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				if got, err := os.Readlink(path); err == nil && got == target {
					return nil
				}
			}
			if err := os.Remove(path); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Symlink(target, path); err == nil {
			return nil
		} else if !os.IsExist(err) {
			return err
		}
		// Lost a race with another Prepare creating the same link:
		// loop and re-check what is there now.
	}
	return fmt.Errorf("symlink %s: could not settle after retries", path)
}

// setEnv returns env with key set to value, replacing any existing
// entry (there must be exactly one: a duplicated HOME would leave
// the value to exec's whim).
func setEnv(env []string, key, value string) []string {
	kv := key + "=" + value
	for i, e := range env {
		if k, _, ok := strings.Cut(e, "="); ok && k == key {
			env[i] = kv
			return env
		}
	}
	return append(env, kv)
}
