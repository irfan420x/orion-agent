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
//     dependency: the child is started with CLONE_NEWNS,
//     CLONE_NEWIPC (its own SysV IPC namespace: no host shared memory,
//     semaphores or message queues are visible), CLONE_NEWUTS (its own
//     hostname), CLONE_NEWPID (its own PID namespace: the sandboxed
//     process is PID 1, it cannot see or signal host processes), and
//     — unless AllowNetwork is set — CLONE_NEWNET (a network
//     namespace whose only interface is a down loopback: no network
//     at all). The user namespace is deliberately NOT created by the
//     clone: this host's kernel refuses procfs mounts from inside a
//     nested user namespace (proven 2026-11-13: every variant —
//     single clone, two-stage unshare, subset=pid or plain, /proc or
//     an arbitrary mountpoint — fails with EPERM, while the same
//     mount succeeds from the cell's user namespace), and /proc must
//     be mounted for the PID namespace to be observable. So the
//     wrapper mounts /proc first, while still running as the agent's
//     uid in the host user namespace, and only then drops into a
//     fresh user namespace with `unshare --user --map-root-user`
//     (agent uid/gid mapped to root inside) immediately before
//     running user code. The end state is identical to creating the
//     user namespace at clone time — root inside its own user
//     namespace, with no capabilities over any host namespace — but
//     the ordering is what makes the /proc mount possible. See
//     SECURITY.md ("PID namespace") for the full reasoning, the
//     experiments, and the fail-closed rules.
//   - Inside the namespaces a small shell wrapper (run before any
//     user code) makes mounts private, then builds a chroot root
//     under <root>/.orion-root: /usr bind-mounted read-only, a
//     minimal /etc copied from a small allowlist (never bind-mounted:
//     the host /etc contains runtime submounts, including a
//     credentials tmpfs, and bind-mounting it is refused in a nested
//     user namespace), the harmless device nodes (/dev/null, zero,
//     full, random, urandom) bind-mounted individually, a tmpfs on
//     /tmp, and <root>/work bind-mounted read-write as /work. It also
//     mounts a fresh procfs on /proc, tied to the new PID namespace,
//     so `ps` and `ls /proc` show only namespace processes (see the
//     /proc paragraph below). The chroot root itself is remounted
//     read-only (via --rbind, which replicates the submounts — a plain
//     self-bind drops them), and /work is remounted nodev+nosuid so no
//     device node created there can ever be opened. The command then
//     runs as /bin/sh -c inside the chroot with cwd /work, HOME=/work,
//     TMPDIR=/tmp, and the agent's scrubbed env — after the wrapper
//     has dropped into the fresh user namespace (see above), so the
//     user code runs as root inside its own user namespace and never
//     as the agent's uid in the host user namespace.
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
//     vanishes with the namespace, size-bounded to 256M so filling
//     it cannot consume host RAM without bound — Audit 3 found
//     it unbounded before the size=, defaulting to 50% of RAM).
//     /home, /etc, /usr and every other host path are either
//     absent or read-only, so even `rm -rf /` inside can only
//     destroy the sandbox's own contents.
//   - /proc is a real procfs for the new PID namespace, mounted by
//     the wrapper before the pivot (see above for why it must be
//     mounted before the user-namespace switch). It is mounted with
//     subset=pid when the kernel supports it (kernel 5.8+): then
//     /proc contains only per-process entries plus self/thread-self
//     — no /proc/sys, /proc/kcore, /proc/kallsyms or
//     /proc/sysrq-trigger at all. On older kernels the fallback is a
//     plain proc mount with /proc/sys bound read-only. Either way a
//     sandboxed process sees only its own namespace's processes:
//     host PIDs do not exist there (kill -0 on one fails with
//     ESRCH), and the namespace init — the user shell itself, PID 1
//     — reaps orphaned children through its normal wait loop
//     (proven by TestSandboxPID1ReapsOrphans: dash reaps an orphan
//     reparented to it). /sys stays an empty directory: no host
//     kernel information leaks in.
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
//     unprivileged user namespaces. The wrapper does all of its
//     mounts before the user-namespace switch, while running as the
//     agent's uid in the host user namespace, so as a non-root agent
//     the very first mount fails and the probe reports the sandbox
//     unavailable. Available() probes the real mechanism and reports
//     false otherwise; shell_exec then refuses the command
//     (fail-closed) unless the operator explicitly opted into an
//     unsandboxed fallback with sandbox.allow_unsandboxed_fallback=true.
//   - /sys is an empty directory inside: no host kernel information
//     leaks in. /proc, by contrast, is a real procfs for the new PID
//     namespace (see above) — tools that need it (ps, /proc/self/…)
//     work, but only ever see namespace processes.
//   - The sandbox is an ADDITIONAL layer, never a replacement: the
//     token-based IsBlockedCommand denylist still runs first.
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
	// AllowUnsandboxedFallback, when true, lets shell_exec run a
	// command unsandboxed (with a warning) when the sandbox is
	// enabled but cannot be established — unavailable mechanism or
	// a Prepare failure. Default false: fail-closed. With the
	// default, a command is REFUSED outright rather than run
	// without the isolation the operator opted into; opt into the
	// fallback only when that degradation is acceptable.
	AllowUnsandboxedFallback bool `json:"allow_unsandboxed_fallback"`
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
mount -t tmpfs -o nosuid,nodev,noexec,size=256M tmpfs "$NEWROOT/tmp" || exit 126
mount --bind "$ROOT/work" "$NEWROOT/work" || exit 126
mount -o remount,bind,rw,nodev,nosuid "$NEWROOT/work" || exit 126
mount --rbind "$NEWROOT" "$NEWROOT" || exit 126
mount -o remount,bind,ro,nosuid,nodev "$NEWROOT" || exit 126
# /proc for the new PID namespace. The wrapper is PID 1 in the new
# namespace (the clone created it), so a procfs mounted here is tied
# to that namespace and shows only its processes — never host PIDs.
# It MUST be mounted now, before the user-namespace switch below:
# this host's kernel refuses procfs mounts from inside a nested user
# namespace (EPERM, proven every way it can be tried), so once the
# wrapper has dropped into the fresh user namespace the mount is
# impossible. subset=pid (kernel 5.8+) hides everything but
# per-process entries — no /proc/sys, /proc/kcore, /proc/kallsyms or
# /proc/sysrq-trigger. Fall back to a plain proc mount with /proc/sys
# bound read-only on older kernels. If neither mount works, refuse:
# a half-built sandbox never runs user code (exit 126).
if ! mount -t proc -o nosuid,nodev,noexec,subset=pid proc "$NEWROOT/proc" 2>/dev/null; then
  mount -t proc -o nosuid,nodev,noexec proc "$NEWROOT/proc" || exit 126
  mount --bind "$NEWROOT/proc/sys" "$NEWROOT/proc/sys" || exit 126
  mount -o remount,bind,ro "$NEWROOT/proc/sys" || exit 126
fi
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
# Detach the old (host) root. Call umount2(2) directly via python3
# (present in the chroot through the /usr bind mount; the number is
# arch-dependent) instead of the umount(8) CLI: the direct syscall is
# proven and never depends on /proc or libmount — the CLI was
# strace-proven (2026-11-13) to fail opening /proc/mounts when /proc
# was not mounted, and there is no reason to re-prove it now that
# the namespace's own /proc exists. If python3 or the arch is
# unavailable, refuse to run: leaving the host root mounted at
# /.old-root would defeat the pivot.
case "$(uname -m)" in
  x86_64) UMOUNT2=166 ;;
  aarch64) UMOUNT2=39 ;;
  *) exit 126 ;;
esac
python3 -c "import ctypes,sys; libc=ctypes.CDLL('libc.so.6', use_errno=True); sys.exit(0 if libc.syscall($UMOUNT2, b'/.old-root', 2)==0 else 1)" || exit 126
# Drop into a fresh user namespace before any user code runs. The
# wrapper has been running as the agent's uid in the host user
# namespace until now (that is what allowed the mounts and the /proc
# mount above); from here on the command runs as root inside its own
# user namespace, with capabilities only over its own namespaces and
# none over any host namespace — the same end state as creating the
# user namespace at clone time, reached one step later so the /proc
# mount stays possible. unshare --map-root-user maps the agent's
# uid/gid to root inside (it writes the uid/gid maps for itself;
# exec keeps this shell as PID 1 of the namespace, so the user shell
# is PID 1 and reaps orphaned children). If the switch fails,
# unshare exits non-zero and no user code runs.
#
# The mount namespace was created by the clone, so it is owned by
# the host user namespace — after the switch the process would hold
# no capabilities over it and could not mount anything at all, not
# even a tmpfs on /tmp inside its own namespace. That would also
# void the seccomp control test (mounts would be blocked with or
# without the filter, so the filter's blocking would be unprovable).
# So the switch is immediately followed by a fresh mount namespace,
# a copy of the prepared one, owned by the new user namespace: the
# process owns its mount namespace again, exactly as when the user
# namespace was created at clone time, and seccomp — not the lack of
# a capability — is what restricts mounts.
# Seccomp: when Prepare staged the helper into the root, run the
# command through it (it installs the filter, then execs /bin/sh).
# Otherwise run the command directly; the missing filter was
# already warned about on the host side. (The root is already the
# new root after the pivot, so no chroot is needed here.)
if [ -x /orion-seccomp ]; then
  exec unshare --user --map-root-user unshare --mount /orion-seccomp /bin/sh -c 'cd /work || exit 126; exec /bin/sh -c "$1"' _ "$CMD"
fi
exec unshare --user --map-root-user unshare --mount /bin/sh -c 'cd /work || exit 126; exec /bin/sh -c "$1"' _ "$CMD"
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
	// The probe checks namespace/chroot availability only. The
	// seccomp filter is a separate layer (validated by
	// probeSeccompLauncher at setup time), so disable it here:
	// otherwise every process that calls Available() logs a
	// misleading "seccomp filter unavailable" warning even when
	// the launcher is present and working for real commands.
	cfg := Config{Enabled: true, Root: filepath.Join(dir, "root"), DisableSeccomp: true}
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

// resolveRootCanonical returns the canonical (symlink-free) form of
// root without creating anything. The root itself may not exist yet
// (first run), so it resolves the longest existing ancestor with
// EvalSymlinks and re-joins the not-yet-existing remainder. The
// result is absolute and Clean.
func resolveRootCanonical(root string) (string, error) {
	rest := []string{}
	cur := filepath.Clean(root)
	for {
		if _, err := os.Lstat(cur); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("sandbox: stat %s: %w", cur, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// The filesystem root always exists, so reaching here
			// means something stranger than a missing path.
			return "", fmt.Errorf("sandbox: no existing ancestor of %s", root)
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
	resolved, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve %s: %w", cur, err)
	}
	for _, comp := range rest {
		resolved = filepath.Join(resolved, comp)
	}
	return resolved, nil
}

// Prepare rewrites cmd (a /bin/sh -c <command> invocation) to run
// inside the sandbox: new mount, PID and (unless AllowNetwork)
// network namespaces, then the wrapper builds the root and
// pivot_roots into it (see wrapperScript). It creates the sandbox
// root, its work dir, the root tree, the minimal /etc copies and the
// merged-/usr symlinks on the host first; those are plain directories
// and idempotent, so concurrent sandboxed commands can share one root
// safely.
//
// The user namespace is NOT created here. The wrapper mounts /proc
// first — the kernel on this host refuses procfs mounts from inside
// a nested user namespace — and only then drops into a fresh user
// namespace with `unshare --user --map-root-user` (agent uid/gid
// mapped to root inside). The clone therefore requests the PID
// namespace but no user namespace, and sets no uid/gid mappings:
// the child starts as the agent's uid in the host user namespace
// and the wrapper switches it before any user code runs. The PID
// namespace is requested atomically with the other namespaces in
// the single clone: if its creation fails the clone fails, the child
// never starts, and the command never runs unconfined — there is no
// path where the PID namespace is silently missing. See SECURITY.md
// ("PID namespace") for the fail-closed rules.
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
	// Canonicalize + validate BEFORE creating anything. The root
	// may be a symlink into a system tree (e.g. /tmp/link -> /etc)
	// that passes the lexical Validate above; if MkdirAll ran
	// first it would create sandbox directories under the target
	// before validation could stop it. The root itself may not
	// exist yet on first run, so resolve the longest existing
	// ancestor and re-join the not-yet-existing remainder.
	resolved, err := resolveRootCanonical(root)
	if err != nil {
		return err
	}
	if err := (Config{Root: resolved}).Validate(); err != nil {
		return err
	}
	root = resolved
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
	// Re-resolve after creation and re-validate as a second check:
	// the root could have been swapped for a symlink between the
	// canonicalization above and the MkdirAll (a concurrent
	// replacement). The wrapper is handed the resolved root so
	// $ROOT/work and $ROOT/.orion-root name the same directories
	// it just created.
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

	cloneFlags := uintptr(syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID)
	if !cfg.AllowNetwork {
		cloneFlags |= uintptr(syscall.CLONE_NEWNET)
	}
	attr := cmd.SysProcAttr
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}
	attr.Cloneflags |= cloneFlags
	// No UidMappings/GidMappings: the user namespace is created by
	// the wrapper (after the /proc mount), not by the clone — see
	// the wrapperScript comment and SECURITY.md ("PID namespace").
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
