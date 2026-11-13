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
//     mapped to the agent's own uid/gid), CLONE_NEWNS, CLONE_NEWIPC
//     (its own SysV IPC namespace: no host shared memory, semaphores
//     or message queues are visible) and CLONE_NEWUTS (its own
//     hostname), and — unless AllowNetwork is set — CLONE_NEWNET (a
//     network namespace whose only interface is a down loopback: no
//     network at all).
//   - Inside the namespaces a small shell wrapper (run before any
//     user code) makes mounts private, then builds a chroot root
//     under <root>/.orion-root: /usr bind-mounted read-only, a
//     minimal /etc copied from a small allowlist (never bind-mounted:
//     the host /etc contains runtime submounts, including a
//     credentials tmpfs, and bind-mounting it is refused in a nested
//     user namespace), the harmless device nodes (/dev/null, zero,
//     full, random, urandom) bind-mounted individually, a tmpfs on
//     /tmp, and <root>/work bind-mounted read-write as /work. The
//     chroot root itself is remounted read-only (via --rbind, which
//     replicates the submounts — a plain self-bind drops them), and
//     /work is remounted nodev+nosuid so no device node created
//     there can ever be opened. The command then runs as /bin/sh -c
//     inside the chroot with cwd /work, HOME=/work, TMPDIR=/tmp, and
//     the agent's scrubbed env.
//   - A plain chroot is escapable on its own: a process with
//     CAP_SYS_CHROOT (uid 0 in its user namespace) can chroot to a
//     subdirectory while its cwd is outside it, walk up past the
//     old root, and chroot again onto the host filesystem (proven by
//     TestSandboxChrootEscapeBlocked). So the wrapper does not
//     chroot at all: it pivot_roots into the new root and then
//     lazily unmounts the old (host) root with umount2(MNT_DETACH)
//     via python3 ctypes — umount(8) cannot do it, because libmount
//     needs /proc/mounts and /proc is deliberately not mounted.
//     After the pivot and detach, the process root IS the new root
//     and there is no host mount left to walk to, so the escape is
//     structurally closed. (An earlier setpriv capability-dropping
//     plan was abandoned once the pivot was proven to work.) The
//     seccomp filter, when its launcher is installed, blocks
//     chroot/mount as a further layer.
//   - The only writable places inside are /work (which persists on
//     the host at <root>/work) and /tmp (a per-run tmpfs that
//     vanishes with the namespace). /home, /etc, /usr and every other
//     host path are either absent or read-only, so even `rm -rf /`
//     inside can only destroy the sandbox's own contents.
//   - On top of the namespaces, a seccomp filter (see seccomp.go)
//     denies escape-relevant syscalls — mounts, namespace creation,
//     kernel modules, reboot, keyrings, BPF, host clock/hostname
//     changes, cross-process memory access — to the sandboxed
//     command. It is applied by a tiny helper binary, orion-seccomp,
//     exec'd by the wrapper after the pivot_root; when the helper is
//     unavailable the command still runs (namespaces + pivot_root
//     only) and a warning is logged once. Seccomp is a second layer
//     on top of the namespaces, never a replacement for them.
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
	// SeccompLauncher is an explicit path to the orion-seccomp
	// helper binary (internal/sandbox/cmd/launcher). Empty means
	// auto-discovery: $ORION_SECCOMP_LAUNCHER, then orion-seccomp
	// next to the running binary, then PATH.
	SeccompLauncher string `json:"seccomp_launcher"`
	// DisableSeccomp turns the seccomp filter off even when the
	// helper is available. Default false: the filter is applied
	// whenever a usable launcher is found, and the command runs
	// without it (with a warning logged once) when none is.
	DisableSeccomp bool `json:"disable_seccomp"`
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
// /dev/tty is deliberately absent: it is the process's controlling
// terminal, and a sandboxed command with access to it could inject
// input into it (TIOCSTI) when the agent runs attached to a tty.
var devAllowlist = []string{
	"null", "zero", "full", "random", "urandom",
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
mkdir -p "$NEWROOT/usr" "$NEWROOT/etc" "$NEWROOT/dev" "$NEWROOT/tmp" "$NEWROOT/proc" "$NEWROOT/work" "$NEWROOT/.old-root" || exit 126
mount --bind /usr "$NEWROOT/usr" || exit 126
mount -o remount,bind,ro,nosuid,nodev "$NEWROOT/usr" || exit 126
for d in null zero full random urandom; do
  if [ -e "/dev/$d" ]; then
    touch "$NEWROOT/dev/$d" || exit 126
    mount --bind "/dev/$d" "$NEWROOT/dev/$d" || exit 126
  fi
done
mount -t tmpfs -o nosuid,nodev,noexec tmpfs "$NEWROOT/tmp" || exit 126
mount --bind "$ROOT/work" "$NEWROOT/work" || exit 126
mount -o remount,bind,rw,nodev,nosuid "$NEWROOT/work" || exit 126
mount --rbind "$NEWROOT" "$NEWROOT" || exit 126
mount -o remount,bind,ro,nosuid,nodev "$NEWROOT" || exit 126
# Pivot into the new root and detach the old (host) root. A plain
# chroot is escapable: a process with CAP_SYS_CHROOT (uid 0 in its
# user namespace) can chroot to a subdirectory while its cwd is
# outside it, walk up past the old root, and chroot again onto the
# host filesystem. After pivot_root the process root IS the new
# root, and the lazy unmount removes the old root mount from this
# mount namespace entirely, so there is nothing outside left to
# walk to. The pivot must come after the mounts above (the new
# root has to be a mount point) and the unmount after cd / (the
# shell's cwd must not be inside the old root, or it stays busy).
cd "$NEWROOT" || exit 126
pivot_root . .old-root || exit 126
cd / || exit 126
# Detach the old (host) root. The umount(8) CLI cannot do this:
# libmount needs /proc/mounts (or /proc/self/mountinfo) to find the
# mount, and /proc is deliberately not mounted in the sandbox —
# proven by strace (2026-11-13): umount never issues the umount2
# syscall, it fails opening /proc/mounts. So call umount2(2)
# directly via python3 (present in the chroot through the /usr bind
# mount; the number is arch-dependent). If python3 or the arch is
# unavailable, refuse to run: leaving the host root mounted at
# /.old-root would defeat the pivot.
case "$(uname -m)" in
  x86_64) UMOUNT2=166 ;;
  aarch64) UMOUNT2=39 ;;
  *) exit 126 ;;
esac
python3 -c "import ctypes,sys; libc=ctypes.CDLL('libc.so.6', use_errno=True); sys.exit(0 if libc.syscall($UMOUNT2, b'/.old-root', 2)==0 else 1)" || exit 126
# Seccomp: when Prepare staged the helper into the root, run the
# command through it (it installs the filter, then execs /bin/sh).
# Otherwise run the command directly; the missing filter was
# already warned about on the host side. (The root is already the
# new root after the pivot, so no chroot is needed here.)
if [ -x /orion-seccomp ]; then
  exec /orion-seccomp /bin/sh -c 'cd /work || exit 126; exec /bin/sh -c "$1"' _ "$CMD"
fi
exec /bin/sh -c 'cd /work || exit 126; exec /bin/sh -c "$1"' _ "$CMD"
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
// network namespaces, then the wrapper builds the root and
// pivot_roots into it (see wrapperScript). It creates the sandbox
// root, its work dir, the root tree, the minimal /etc copies and the
// merged-/usr symlinks on the host first; those are plain directories
// and idempotent, so concurrent sandboxed commands can share one root
// safely.
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
		filepath.Join(newRoot, ".old-root"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("sandbox: create %s: %w", d, err)
		}
	}
	// Resolve the root through symlinks and re-validate the real
	// path: a root like /tmp/link where the link points at /etc
	// would otherwise pass the lexical Validate check and put
	// sandbox state (and the chroot tree) inside a system tree.
	// The wrapper is handed the resolved root so $ROOT/work and
	// $ROOT/.orion-root name the same directories it just created.
	if resolved, err := filepath.EvalSymlinks(root); err != nil {
		return fmt.Errorf("sandbox: resolve root %s: %w", root, err)
	} else if resolved != root {
		if err := (Config{Root: resolved}).Validate(); err != nil {
			return err
		}
		root = resolved
		newRoot = filepath.Join(root, ".orion-root")
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

	// Seccomp filter: stage the helper binary into the chroot when
	// one is usable. The wrapper execs through it when present;
	// when absent (or disabled) the command runs without the
	// filter, with a warning logged once by setupSeccomp.
	cfg.setupSeccomp(newRoot)

	cloneFlags := uintptr(syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS)
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
