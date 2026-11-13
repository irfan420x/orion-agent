// Seccomp filtering for the sandbox.
//
// Feasibility survey (this VM, 2026-11-13):
//
//   - The kernel supports seccomp: /proc/self/status reports
//     "Seccomp: 2" (filter mode) for the agent itself, and the
//     seccomp(2) syscall works (proven by the prototype in
//     /tmp/secprobe/proto.go and by TestSeccompLauncherBlocksSyscalls).
//   - libseccomp2 2.5.5 is installed as a runtime library, but the
//     -dev headers (/usr/include/seccomp.h) are absent, and the
//     module has no Go dependencies at all, so neither
//     github.com/seccomp/libseccomp-golang (cgo + libseccomp-dev)
//     nor golang.org/x/sys/unix is available without adding a
//     dependency and a build-time header requirement.
//   - Neither is needed: the filter is built with the standard
//     library only — a hand-rolled classic-BPF program, installed
//     with raw seccomp(2) and prctl(PR_SET_NO_NEW_PRIVS) syscalls.
//     No cgo, no new dependency, nothing to install on the host.
//
// Design:
//
//   - Default ALLOW with an explicit blocklist of escape-relevant
//     syscalls (see blockedSyscalls). A default-deny allowlist was
//     considered and rejected: the sandbox must run arbitrary user
//     tools (/bin/sh, coreutils, python3, ...), and enumerating
//     every syscall any tool might need is fragile — one missing
//     number silently breaks a legitimate command. The dangerous
//     set, by contrast, is small and enumerable: mount/namespace
//     escapes, kernel-module and reboot calls, keyring and BPF
//     access, host clock/hostname changes, and cross-process
//     memory access. The namespace + chroot + read-only mounts
//     remain the primary containment; seccomp is defense in depth
//     against the sandboxed process reaching the kernel directly.
//   - Blocked syscalls fail with EPERM, except clone3 which fails
//     with ENOSYS: glibc's posix_spawn (and clone fallback paths)
//     retry with clone(2) on ENOSYS, so tools that spawn processes
//     through posix_spawn keep working; EPERM would break them.
//     clone(2) itself is allowed, but only when its flags carry no
//     namespace-creation bit (CLONE_NEWNS/NEWUSER/NEWNET/...): the
//     flags are a syscall argument, so classic BPF can inspect
//     them. clone3's flags live in a pointed-to struct, which BPF
//     cannot dereference, so clone3 is denied outright.
//   - mknod/mknodat are deliberately NOT blocked: the kernel already
//     refuses device-node creation inside the sandbox (EPERM for
//     mknod of a character device on both /work and the /tmp tmpfs,
//     probed 2026-11-13), while fifos (mkfifo) need mknod and are
//     legitimate. Blocking mknod would break mkfifo for no gain.
//   - A syscall in an unexpected architecture (e.g. a 32-bit compat
//     binary, whose numbers differ) fails with ENOSYS: the blocklist
//     numbers are only meaningful for the native architecture.
//   - The filter is installed by a tiny helper binary, orion-seccomp
//     (internal/sandbox/cmd/launcher), which installs the filter on
//     itself and then execs the target command: filters persist
//     across execve, and Go's os/exec has no pre-exec hook, so the
//     filter cannot be installed from the parent. The launcher is
//     copied into the chroot by Prepare and exec'd by the wrapper
//     after the chroot; if the launcher is unavailable the command
//     still runs (graceful fallback) and a warning is logged once.
//   - Supported architectures: amd64 and arm64, with per-arch
//     syscall tables verified against the kernel headers
//     (asm/unistd_64.h, asm-generic/unistd.h) and cross-checked
//     against Go's syscall package where the constants exist (see
//     TestSeccompTableMatchesStdlib). Any other GOARCH fails closed:
//     InstallSeccompFilter returns an error and the launcher exits
//     126 instead of running the command unfiltered.
package sandbox

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Classic-BPF opcodes (linux/filter.h) and seccomp return values
// (linux/seccomp.h). Defined here because Go's syscall package does
// not export SYS_SECCOMP or PR_SET_NO_NEW_PRIVS.
const (
	bpfLdAbs  = 0x20 // BPF_LD  | BPF_W   | BPF_ABS
	bpfJmpEq  = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfJmpSet = 0x45 // BPF_JMP | BPF_JSET | BPF_K
	bpfRetK   = 0x06 // BPF_RET | BPF_K

	seccompRetAllow = 0x7fff0000 // SECCOMP_RET_ALLOW
	seccompRetErrno = 0x00050000 // SECCOMP_RET_ERRNO

	seccompSetFilter = 1  // SECCOMP_SET_FILTER
	prSetNoNewPrivs  = 38 // PR_SET_NO_NEW_PRIVS
)

// sockFilter and sockFprog mirror struct sock_filter / struct
// sock_fprog from linux/filter.h. The kernel reads the program from
// the pointer in sockFprog.
type sockFilter struct {
	Code uint16
	Jt   uint8
	Jf   uint8
	K    uint32
}

type sockFprog struct {
	Len    uint16
	Filter *sockFilter
}

// blockedSyscall is one entry of the per-arch blocklist.
type blockedSyscall struct {
	name string
	nr   uint32
}

// seccompArch is the filter description for one GOARCH. Syscall
// numbers come from the kernel headers (asm/unistd_64.h for amd64,
// asm-generic/unistd.h for arm64); see the tables below.
type seccompArch struct {
	goarch string
	// auditArch is AUDIT_ARCH_* (linux/audit.h): 0xC000003E for
	// x86-64, 0xC00000B7 for aarch64.
	auditArch uint32
	// seccompNr is __NR_seccomp; Go's syscall package does not
	// define SYS_SECCOMP, so the table carries it.
	seccompNr uint32
	cloneNr   uint32 // __NR_clone
	clone3Nr  uint32 // __NR_clone3
	// blocked fail with EPERM.
	blocked []blockedSyscall
	// blockedENOSYS fail with ENOSYS (clone3: see package doc).
	blockedENOSYS []blockedSyscall
}

// cloneNewNSMask is the set of clone(2) flags that create a new
// namespace. A clone carrying any of these is denied; ordinary
// process/thread creation (fork, pthread_create, subprocess) has
// none of them set. Flag values are architecture-independent.
const cloneNewNSMask = 0x7E020080 // CLONE_NEWNS|NEWUTS|NEWIPC|NEWUSER|NEWPID|NEWNET|NEWCGROUP|NEWTIME

// seccompTables is the whole supported set. Add an architecture here
// only with numbers verified against that arch's kernel headers;
// an unverified table is worse than none (a wrong number silently
// un-blocks a syscall or blocks an innocent one).
var seccompTables = []seccompArch{
	{
		goarch:    "amd64",
		auditArch: 0xC000003E,
		seccompNr: 317,
		cloneNr:   56,
		clone3Nr:  435,
		blocked: []blockedSyscall{
			// Filesystem escape: mounts, namespace roots, chroot.
			{"mount", 165}, {"umount2", 166}, {"pivot_root", 155}, {"chroot", 161},
			// The new mount API is mount(2) by another name.
			{"fsopen", 430}, {"fsconfig", 431}, {"fsmount", 432}, {"fspick", 433},
			{"open_tree", 428}, {"move_mount", 429}, {"mount_setattr", 442},
			// Host control: reboot, swap, modules, clock, hostname.
			// The sandbox shares the host UTS namespace and clock,
			// so these would act on the host, not the sandbox.
			{"reboot", 169}, {"swapon", 167}, {"swapoff", 168},
			{"kexec_load", 246}, {"kexec_file_load", 320},
			{"init_module", 175}, {"finit_module", 313}, {"delete_module", 176},
			{"clock_settime", 227}, {"settimeofday", 164}, {"adjtimex", 159}, {"clock_adjtime", 305},
			{"sethostname", 170}, {"setdomainname", 171},
			// Kernel attack surface: keyrings, BPF, tracing, devices.
			{"keyctl", 250}, {"add_key", 248}, {"request_key", 249},
			{"ptrace", 101}, {"bpf", 321}, {"perf_event_open", 298},
			{"userfaultfd", 323}, {"fanotify_init", 300},
			// Chroot escape via file handles.
			{"open_by_handle_at", 304}, {"name_to_handle_at", 303},
			// Namespace escape.
			{"setns", 308}, {"unshare", 272},
			// Host information / control.
			{"syslog", 103}, {"iopl", 172}, {"ioperm", 173},
			{"acct", 163}, {"quotactl", 179}, {"vhangup", 153},
			// Cross-process memory access (the agent runs as the
			// same uid; /proc is empty inside but pids can be guessed).
			{"process_vm_readv", 310}, {"process_vm_writev", 311},
			// The sandboxed process must not install its own filter
			// (a filter can only restrict further, but a second
			// filter would make the effective policy unpredictable).
			{"seccomp", 317},
		},
		blockedENOSYS: []blockedSyscall{
			{"clone3", 435},
		},
	},
	{
		goarch:    "arm64",
		auditArch: 0xC00000B7,
		seccompNr: 277,
		cloneNr:   220,
		clone3Nr:  435,
		blocked: []blockedSyscall{
			{"mount", 40}, {"umount2", 39}, {"pivot_root", 41}, {"chroot", 51},
			{"fsopen", 430}, {"fsconfig", 431}, {"fsmount", 432}, {"fspick", 433},
			{"open_tree", 428}, {"move_mount", 429}, {"mount_setattr", 442},
			{"reboot", 142}, {"swapon", 224}, {"swapoff", 225},
			{"kexec_load", 104}, {"kexec_file_load", 294},
			{"init_module", 105}, {"finit_module", 273}, {"delete_module", 106},
			{"clock_settime", 112}, {"settimeofday", 170}, {"adjtimex", 171}, {"clock_adjtime", 266},
			{"sethostname", 161}, {"setdomainname", 162},
			{"keyctl", 219}, {"add_key", 217}, {"request_key", 218},
			{"ptrace", 117}, {"bpf", 280}, {"perf_event_open", 241},
			{"userfaultfd", 282}, {"fanotify_init", 262},
			{"open_by_handle_at", 265}, {"name_to_handle_at", 264},
			{"setns", 268}, {"unshare", 97},
			{"syslog", 116},
			{"acct", 89}, {"quotactl", 60}, {"vhangup", 58},
			{"process_vm_readv", 270}, {"process_vm_writev", 271},
			{"seccomp", 277},
		},
		blockedENOSYS: []blockedSyscall{
			{"clone3", 435},
		},
	},
}

// seccompTableFor returns the filter table for a GOARCH, or false
// when the architecture has no verified table.
func seccompTableFor(goarch string) (seccompArch, bool) {
	for _, t := range seccompTables {
		if t.goarch == goarch {
			return t, true
		}
	}
	return seccompArch{}, false
}

// bpfAsm is a tiny label-based assembler for the filter program.
// Jumps are recorded by label and resolved after the whole program
// is emitted, so no jump offset is ever hand-computed.
type bpfAsm struct {
	ins    []sockFilter
	labels map[string]int
	fixups []bpfFixup
}

type bpfFixup struct {
	idx    int
	jt, jf string
}

func (a *bpfAsm) emit(code uint16, k uint32) {
	a.ins = append(a.ins, sockFilter{Code: code, K: k})
}

// jmp emits a conditional jump. An empty label means "fall through
// to the next instruction" (offset 0).
func (a *bpfAsm) jmp(code uint16, k uint32, jt, jf string) {
	idx := len(a.ins)
	a.emit(code, k)
	a.fixups = append(a.fixups, bpfFixup{idx: idx, jt: jt, jf: jf})
}

func (a *bpfAsm) label(name string) {
	a.labels[name] = len(a.ins)
}

func (a *bpfAsm) resolve() {
	for _, f := range a.fixups {
		if f.jt != "" {
			a.ins[f.idx].Jt = uint8(a.labels[f.jt] - f.idx - 1)
		}
		if f.jf != "" {
			a.ins[f.idx].Jf = uint8(a.labels[f.jf] - f.idx - 1)
		}
	}
}

// buildSeccompFilter builds the BPF program for a table. Layout:
//
//	LD  arch; JEQ native-arch or ENOSYS
//	LD  nr
//	JEQ <each EPERM-blocked nr> -> deny
//	JEQ clone3 -> denyENOSYS
//	JEQ clone -> fall through to the flag check, else allow
//	LD  args[0]; JSET cloneNewNSMask -> deny
//	allow: RET ALLOW
//	deny:  RET ERRNO(EPERM)
//	denyENOSYS: RET ERRNO(ENOSYS)
//	archDeny:   RET ERRNO(ENOSYS)
//
// struct seccomp_data offsets (linux/seccomp.h): nr is at 0, arch
// at 4, args[0] at 16.
func buildSeccompFilter(t seccompArch) []sockFilter {
	a := &bpfAsm{labels: map[string]int{}}
	a.emit(bpfLdAbs, 4) // arch
	a.jmp(bpfJmpEq, t.auditArch, "", "archDeny")
	a.emit(bpfLdAbs, 0) // nr
	for _, b := range t.blocked {
		a.jmp(bpfJmpEq, b.nr, "deny", "")
	}
	for _, b := range t.blockedENOSYS {
		a.jmp(bpfJmpEq, b.nr, "denyENOSYS", "")
	}
	// clone(2): only the flags matter. A plain fork/clone falls
	// through to the flag check and then to allow; a namespace
	// clone is denied.
	a.jmp(bpfJmpEq, t.cloneNr, "", "allow")
	a.emit(bpfLdAbs, 16) // args[0] = clone flags
	a.jmp(bpfJmpSet, cloneNewNSMask, "deny", "")
	a.label("allow")
	a.emit(bpfRetK, seccompRetAllow)
	a.label("deny")
	a.emit(bpfRetK, seccompRetErrno|uint32(syscall.EPERM))
	a.label("denyENOSYS")
	a.emit(bpfRetK, seccompRetErrno|uint32(syscall.ENOSYS))
	a.label("archDeny")
	a.emit(bpfRetK, seccompRetErrno|uint32(syscall.ENOSYS))
	a.resolve()
	return a.ins
}

// InstallSeccompFilter installs the filter on the calling thread
// (locked to its OS thread: seccomp filters and PR_SET_NO_NEW_PRIVS
// are per-thread, and the launcher execs from that same thread).
// The filter persists across the following execve, which is how the
// sandboxed command inherits it. Call it only in a process that is
// about to exec the sandboxed command: installing it in a
// long-lived process would filter that process itself.
func InstallSeccompFilter() error {
	table, ok := seccompTableFor(runtime.GOARCH)
	if !ok {
		return fmt.Errorf("seccomp: unsupported architecture %q (no verified syscall table)", runtime.GOARCH)
	}
	runtime.LockOSThread()
	if _, _, errno := syscall.Syscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); errno != 0 {
		return fmt.Errorf("seccomp: prctl PR_SET_NO_NEW_PRIVS: %v", errno)
	}
	prog := buildSeccompFilter(table)
	fprog := &sockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if _, _, errno := syscall.Syscall(uintptr(table.seccompNr), seccompSetFilter, 0, uintptr(unsafe.Pointer(fprog))); errno != 0 {
		return fmt.Errorf("seccomp: seccomp(SECCOMP_SET_FILTER): %v", errno)
	}
	return nil
}

// SeccompLauncherEnv overrides the launcher path. Config's
// SeccompLauncher field wins over it.
const SeccompLauncherEnv = "ORION_SECCOMP_LAUNCHER"

// launcherName is the helper binary name. install.sh/Makefile are
// expected to build it (internal/sandbox/cmd/launcher, CGO_ENABLED=0)
// and install it next to the orion/oriond binaries.
const launcherName = "orion-seccomp"

// findSeccompLauncher resolves the helper binary: the explicit
// Config path, $ORION_SECCOMP_LAUNCHER, orion-seccomp next to the
// running binary, then PATH. It returns "" when none is found. An
// explicitly configured path is returned even when the file is
// missing, so the probe can report the problem instead of silently
// falling back.
func (c Config) findSeccompLauncher() string {
	if strings.TrimSpace(c.SeccompLauncher) != "" {
		return strings.TrimSpace(c.SeccompLauncher)
	}
	if v := strings.TrimSpace(os.Getenv(SeccompLauncherEnv)); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), launcherName); fileExists(p) {
			return p
		}
	}
	if p, err := exec.LookPath(launcherName); err == nil {
		return p
	}
	return ""
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// probeResult caches one kernel-probe outcome per launcher path.
type probeResult struct{ err error }

var seccompProbeCache sync.Map // string -> probeResult

// probeSeccompLauncher checks that the launcher can actually install
// the filter on this kernel: it runs the launcher with /bin/true on
// the host (outside any namespace). Exit 0 means the filter
// installed and the trivial command ran under it. Results are cached
// per launcher path for the process lifetime.
func probeSeccompLauncher(launcher string) error {
	if v, ok := seccompProbeCache.Load(launcher); ok {
		return v.(probeResult).err
	}
	err := probeSeccompLauncherOnce(launcher)
	seccompProbeCache.Store(launcher, probeResult{err})
	return err
}

func probeSeccompLauncherOnce(launcher string) error {
	if !fileExists(launcher) {
		return fmt.Errorf("launcher %q not found", launcher)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, launcher, "/bin/true")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("seccomp launcher probe (%s /bin/true): %w", launcher, err)
	}
	return nil
}

// installSeccompLauncher copies the launcher binary into the chroot
// root as /orion-seccomp, so the wrapper can exec it after chroot.
// The copy is atomic (temp file + rename) so concurrent Prepares
// sharing one root cannot interleave, and a stale copy from an older
// launcher build is replaced on every Prepare.
func installSeccompLauncher(launcher, newRoot string) error {
	src, err := os.Open(launcher)
	if err != nil {
		return fmt.Errorf("open launcher %q: %w", launcher, err)
	}
	defer src.Close()
	tmp, err := os.CreateTemp(newRoot, ".orion-seccomp-*")
	if err != nil {
		return fmt.Errorf("stage launcher: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return fmt.Errorf("stage launcher: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return fmt.Errorf("stage launcher: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("stage launcher: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(newRoot, launcherName)); err != nil {
		return fmt.Errorf("install launcher: %w", err)
	}
	return nil
}

var seccompWarnOnce sync.Once

// warnSeccompOnce logs the fallback warning a single time per
// process: the filter is unavailable, so sandboxed commands run
// with namespace/chroot containment only.
func warnSeccompOnce(reason string) {
	seccompWarnOnce.Do(func() {
		log.Printf("sandbox: seccomp filter unavailable (%s); sandboxed commands run without syscall filtering", reason)
	})
}

// setupSeccomp is the Prepare-time step: when the filter is enabled
// (the default) and a usable launcher is found, stage it into the
// chroot and report true so the wrapper execs through it. Otherwise
// log the reason once and report false (graceful fallback: the
// command still runs, contained by namespaces and chroot).
func (c Config) setupSeccomp(newRoot string) bool {
	if c.DisableSeccomp {
		return false
	}
	launcher := c.findSeccompLauncher()
	if launcher == "" {
		warnSeccompOnce("no orion-seccomp launcher binary found; install it next to the orion binary to enable syscall filtering (see SECURITY.md)")
		return false
	}
	if err := probeSeccompLauncher(launcher); err != nil {
		warnSeccompOnce(fmt.Sprintf("launcher %q unusable: %v", launcher, err))
		return false
	}
	if err := installSeccompLauncher(launcher, newRoot); err != nil {
		warnSeccompOnce(fmt.Sprintf("could not stage launcher: %v", err))
		return false
	}
	return true
}
