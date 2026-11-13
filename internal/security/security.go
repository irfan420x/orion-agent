// Package security is the single canonical place for Orion's safety
// checks. There is exactly ONE IsBlockedCommand implementation in the
// whole codebase, here. Every other package must delegate to it.
//
// SAFETY: this package never executes commands. Tests of this package
// call IsBlockedCommand with strings only — never run a live command.
//
// The denylist is TOKEN-BASED and assumes the command will be run by
// /bin/sh -c, so the full shell language applies: pipes, separators
// (including newlines), subshells, command substitution, quoting,
// wrappers (sudo/su/env/nice/timeout/xargs/...) and shell keywords can
// all hide the real command, so all of them are resolved before the
// binary is checked. Anything that opens an interactive shell or a new
// shell interpreter (sh/bash/...) is refused outright: a shell is an
// indirection layer that defeats token checks, and the agent runs
// commands directly so it never needs to invoke one.
//
// Scripting interpreters are the same kind of indirection when given
// inline code (python3 -c, perl -e, ruby -e, node -e, php -r, lua -e,
// awk '...'): the code argument is analysed for destructive operations
// (deleting files, shelling out to a destructive command, writing to
// or truncating a system path, dumping the environment), while ordinary
// one-liners (print(1+1), awk '{print $1}') stay allowed. A bare
// interpreter or a script file is not inline code and is left to the
// sandboxing layer: this package cannot see what a file contains.
package security

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// blockedBinaries are commands that are never allowed, by basename.
// Token-based: /bin/rm, /usr/bin/rm, ./rm and plain rm all resolve to "rm".
var blockedBinaries = map[string]bool{
	"rm": true,
	// mkfs and every filesystem maker: any mkfs.* is also caught by the
	// prefix rule in blockedSegment, so unlisted variants are covered.
	"mkfs": true, "mkfs.ext4": true, "mkfs.xfs": true, "mkfs.btrfs": true,
	"mkfs.vfat": true, "mkfs.ntfs": true, "mkfs.fat": true, "mkfs.exfat": true,
	"mkfs.f2fs": true, "mkfs.minix": true, "mkfs.jfs": true, "mkfs.reiserfs": true,
	"mkfs.bfs": true, "mkfs.cramfs": true, "mkfs.hfsplus": true,
	"dd": true, "shred": true, "wipe": true, "wipefs": true, "format": true,
	"fdisk": true, "sfdisk": true, "cfdisk": true, "sgdisk": true,
	"parted": true, "blkdiscard": true, "truncate": true, "cryptsetup": true,
	"reboot": true, "shutdown": true, "halt": true, "poweroff": true,
	"init": true, "systemctl": true, "service": true,
	"chmod": true,
	// Host administration: mounting, loop devices, swap, kernel
	// modules, kernel parameters, device-mapper. The agent never
	// needs these, and as root each of them can damage or take over
	// the host (mount/umount a disk, load a module, change a sysctl).
	// "mount" and "losetup" are handled specially in blockedSegment
	// (bare, with no arguments, they only list mounts/devices).
	"umount": true, "swapon": true, "swapoff": true,
	"mkswap": true, "dmsetup": true, "modprobe": true, "insmod": true,
	"rmmod": true, "sysctl": true,
	// Account and credential administration: changing passwords or
	// creating/removing users and groups is persistence, not agent
	// work (a new root-equivalent account survives the agent run).
	"passwd": true, "chpasswd": true,
	"useradd": true, "userdel": true, "usermod": true,
	"groupadd": true, "groupdel": true, "groupmod": true,
	"visudo": true, "adduser": true, "deluser": true,
	// Process killing by name: killall/pkill match by name and can
	// kill the agent, the daemon, or system services. Killing by PID
	// is handled by the "kill" case in blockedSegment (PID 1 and -1
	// are refused); these name-based killers have no safe form.
	"killall": true, "pkill": true,
	// Namespace / privilege / remote-execution wrappers. Each runs
	// an arbitrary command outside the agent's own execution
	// context: nsenter enters another process's namespaces (the
	// host's, as root), unshare runs a command with host filesystem
	// access, systemd-run runs it as a transient service, setpriv
	// and capsh change uid/capabilities, ssh/scp/sftp run commands
	// or copy files on (possibly) another host — and ssh to
	// localhost runs them here. expect and socat both take a
	// command to run (Tcl / EXEC address). None of these is agent
	// work; they are refused outright rather than parsed.
	"nsenter": true, "unshare": true, "systemd-run": true,
	"setpriv": true, "capsh": true, "expect": true, "socat": true,
	"ssh": true, "scp": true, "sftp": true,
	// Shell interpreters: refused outright (see the package comment).
	// The tool layer invokes /bin/sh -c itself and never routes through
	// IsBlockedCommand, so this cannot break the agent's own execution.
	"sh": true, "bash": true, "zsh": true, "dash": true, "fish": true,
	"ksh": true, "csh": true, "tcsh": true,
}

// systemRoots are filesystem trees whose contents the agent must never
// destroy, overwrite or re-permission. /tmp and the workspace are
// deliberately absent: ordinary agent work happens there.
var systemRoots = []string{
	"/etc", "/usr", "/bin", "/sbin", "/var", "/opt", "/home", "/root",
	"/boot", "/dev", "/proc", "/sys", "/lib", "/lib64", "/run", "/srv",
}

// isSystemPath reports whether p is, or is inside, a system tree (or the
// user's home). It is used for chmod/tee/redirection targets, where the
// operation is only dangerous on such paths.
func isSystemPath(p string) bool {
	switch p {
	case "~", "$HOME", "${HOME}", "/", "/*":
		return true
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "$HOME/") || strings.HasPrefix(p, "${HOME}/") {
		return true
	}
	for _, root := range systemRoots {
		if p == root || strings.HasPrefix(p, root+"/") || strings.HasPrefix(p, root+"/*") {
			return true
		}
	}
	return false
}

// isDestructiveTarget reports whether writing to / re-permissioning p
// would damage the system. The harmless pseudo-devices are excluded:
// writing to /dev/null or /dev/stdout destroys nothing.
func isDestructiveTarget(p string) bool {
	switch p {
	case "/dev/null", "/dev/stdout", "/dev/stderr", "/dev/stdin",
		"/dev/tty", "/dev/zero", "/dev/full":
		return false
	}
	return isSystemPath(p)
}

// shellAssignmentRe matches a leading VAR=value assignment token.
var shellAssignmentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// isAssignment reports whether tok is a shell variable assignment.
func isAssignment(tok string) bool { return shellAssignmentRe.MatchString(tok) }

// skipAssignments consumes leading VAR=value tokens.
func skipAssignments(toks []string) []string {
	i := 0
	for i < len(toks) && isAssignment(toks[i]) {
		i++
	}
	return toks[i:]
}

// shellKeywords are reserved words that can lead a segment without being
// the command itself ("then rm -rf /", "do rm -rf /"); they are skipped
// so the real command is checked. "time" is intentionally absent: it is
// a wrapper with flags and is handled by the wrapper loop instead.
var shellKeywords = map[string]bool{
	"if": true, "then": true, "elif": true, "else": true,
	"do": true, "done": true, "fi": true,
	"for": true, "while": true, "until": true,
	"case": true, "esac": true, "select": true, "in": true,
	"!": true, "function": true, "coproc": true,
}

// flagSpec describes the flags of a wrapper command: which short-flag
// letters take a following argument, and which long-flag names (without
// "--") take a following argument when written without "=".
type flagSpec struct {
	shortTakesArg map[byte]bool
	longTakesArg  map[string]bool
}

var (
	sudoSpec = flagSpec{
		shortTakesArg: map[byte]bool{'u': true, 'g': true, 'r': true, 't': true, 'U': true, 'C': true, 'p': true, 'h': true},
		longTakesArg:  map[string]bool{"user": true, "group": true, "role": true, "type": true, "prompt": true, "chdir": true, "close-from": true, "host": true},
	}
	doasSpec = flagSpec{
		shortTakesArg: map[byte]bool{'u': true, 'C': true},
	}
	niceSpec = flagSpec{
		shortTakesArg: map[byte]bool{'n': true},
		longTakesArg:  map[string]bool{"adjustment": true},
	}
	timeSpec = flagSpec{
		shortTakesArg: map[byte]bool{'f': true, 'o': true},
		longTakesArg:  map[string]bool{"format": true, "output": true},
	}
	stdbufSpec = flagSpec{
		shortTakesArg: map[byte]bool{'i': true, 'o': true, 'e': true},
		longTakesArg:  map[string]bool{"input": true, "output": true, "error": true},
	}
	commandSpec = flagSpec{
		shortTakesArg: map[byte]bool{},
	}
	noArgSpec   = flagSpec{} // nohup, setsid: flags take no arguments
	timeoutSpec = flagSpec{
		shortTakesArg: map[byte]bool{'k': true, 's': true},
		longTakesArg:  map[string]bool{"kill-after": true, "signal": true},
	}
	xargsSpec = flagSpec{
		shortTakesArg: map[byte]bool{'a': true, 'd': true, 'E': true, 'I': true, 'L': true, 'n': true, 'P': true, 's': true},
		longTakesArg:  map[string]bool{"arg-file": true, "delimiter": true, "eof": true, "replace": true, "max-args": true, "max-lines": true, "max-procs": true, "process-slot-var": true},
	}
	runuserSpec = flagSpec{
		shortTakesArg: map[byte]bool{'u': true, 'g': true, 's': true},
		longTakesArg:  map[string]bool{"user": true, "group": true, "shell": true, "session-command": true},
	}
	pkexecSpec = flagSpec{
		shortTakesArg: map[byte]bool{'u': true},
		longTakesArg:  map[string]bool{"user": true},
	}
	// execSpec: exec(1) flags. -a takes the argv[0] name.
	execSpec = flagSpec{
		shortTakesArg: map[byte]bool{'a': true},
	}
	// chrootSpec: chroot(1) flags. --userspec and --groups take a value.
	chrootSpec = flagSpec{
		longTakesArg: map[string]bool{"userspec": true, "groups": true},
	}
	// watchSpec: watch(1) flags. -n/--interval takes the interval.
	watchSpec = flagSpec{
		shortTakesArg: map[byte]bool{'n': true},
		longTakesArg:  map[string]bool{"interval": true},
	}
	// copySpec: cp(1)/mv(1)/ln(1) flags. -t/--target-directory takes
	// the destination directory.
	copySpec = flagSpec{
		shortTakesArg: map[byte]bool{'t': true},
		longTakesArg:  map[string]bool{"target-directory": true},
	}
	// chownSpec: chown(1)/chgrp(1) flags. --reference and --from
	// take a value.
	chownSpec = flagSpec{
		longTakesArg: map[string]bool{"reference": true, "from": true},
	}
	// mkdirSpec: mkdir(1) flags. -m/--mode takes a value.
	mkdirSpec = flagSpec{
		shortTakesArg: map[byte]bool{'m': true},
		longTakesArg:  map[string]bool{"mode": true},
	}
)

// skipFlags consumes leading flag tokens (and the arguments of flags that
// take one) from toks and returns what remains. A "--" token ends flag
// parsing. Parsing stops at the first non-flag token. Unknown flags are
// skipped as flag tokens; only the wrappers with their own parsers
// (sudo, env) treat unknown flags as a refusal.
func skipFlags(toks []string, spec flagSpec) []string {
	i := 0
	for i < len(toks) {
		t := toks[i]
		if t == "--" {
			return toks[i+1:]
		}
		if strings.HasPrefix(t, "--") {
			name := t[2:]
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				i++
				continue
			}
			if spec.longTakesArg[name] {
				i += 2
				continue
			}
			i++
			continue
		}
		if strings.HasPrefix(t, "-") && len(t) > 1 {
			// Short-flag cluster: walk the letters; a letter that takes
			// an argument consumes the rest of the cluster or the next
			// token.
			consumedNext := false
			for j := 1; j < len(t); j++ {
				if spec.shortTakesArg[t[j]] {
					if j+1 >= len(t) {
						consumedNext = true
					}
					break
				}
			}
			i++
			if consumedNext {
				i++
			}
			continue
		}
		break
	}
	if i > len(toks) {
		return nil
	}
	return toks[i:]
}

type sudoVerdict int

const (
	sudoContinue sudoVerdict = iota // the rest is the command to check
	sudoBlocked                     // refused outright (interactive shell)
	sudoAllowed                     // executes nothing (list/edit mode)
)

// resolveSudoLike parses sudo(8)/doas(1) flags. Short-flag letters in
// spec.shortTakesArg (and long flags in spec.longTakesArg) consume a
// following argument; -i/-s open an interactive shell and are refused;
// -l/-e only list or edit and execute nothing.
func resolveSudoLike(toks []string, spec flagSpec) ([]string, sudoVerdict) {
	i := 0
	for i < len(toks) {
		t := toks[i]
		if t == "--" {
			return skipAssignments(toks[i+1:]), sudoContinue
		}
		if strings.HasPrefix(t, "--") {
			name := t[2:]
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name = name[:eq]
				i++
				continue
			}
			switch name {
			case "login", "shell":
				return nil, sudoBlocked
			case "list", "edit", "validate", "version", "help":
				return nil, sudoAllowed
			}
			if spec.longTakesArg[name] {
				i += 2
				continue
			}
			i++
			continue
		}
		if strings.HasPrefix(t, "-") && len(t) > 1 {
			consumedNext := false
			for j := 1; j < len(t); j++ {
				switch t[j] {
				case 'i', 's':
					return nil, sudoBlocked
				case 'l', 'e':
					return nil, sudoAllowed
				}
				if spec.shortTakesArg[t[j]] {
					if j+1 >= len(t) {
						consumedNext = true
					}
					break
				}
			}
			i++
			if consumedNext {
				i++
			}
			continue
		}
		break
	}
	if i > len(toks) {
		return nil, sudoContinue
	}
	return skipAssignments(toks[i:]), sudoContinue
}

// envLongTakesArg are env(1) long flags that take a following argument
// when written without "=".
var envLongTakesArg = map[string]bool{
	"unset": true, "chdir": true, "split-string": true,
}

// resolveEnv parses an env(1) invocation. It returns the remaining
// command tokens and whether the invocation must be refused. Bare env
// (no command) dumps the whole environment — which may hold secrets — so
// it is refused unless -i / --ignore-environment was given, in which
// case the environment is empty and there is nothing to leak. Unknown
// flags are refused rather than guessed at.
func resolveEnv(seg []string, vars map[string][]string) (rest []string, blocked bool) {
	toks := seg[1:]
	ignoreEnv := false
	for len(toks) > 0 {
		t := toks[0]
		if t == "--" {
			toks = toks[1:]
			break
		}
		if strings.HasPrefix(t, "--") {
			name := t[2:]
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				// --split-string=<value>: the value is a command
				// line env splits and runs, so it is checked.
				if name[:eq] == "split-string" && isBlockedCommand(name[eq+1:], vars) {
					return nil, true
				}
				toks = toks[1:]
				continue
			}
			if name == "ignore-environment" {
				ignoreEnv = true
			}
			if envLongTakesArg[name] {
				if len(toks) < 2 {
					return nil, true // flag without its argument: refuse
				}
				// --split-string <value>: check the value.
				if name == "split-string" && isBlockedCommand(toks[1], vars) {
					return nil, true
				}
				toks = toks[2:]
				continue
			}
			toks = toks[1:]
			continue
		}
		if strings.HasPrefix(t, "-") && len(t) > 1 {
			takesArg := false
			unknown := false
			splitValue := ""
			isSplit := false
			for j := 1; j < len(t); j++ {
				switch t[j] {
				case 'i':
					ignoreEnv = true
				case 'u', 'C':
					if j+1 < len(t) {
						j = len(t) // value attached, e.g. -uFOO
					} else {
						takesArg = true
					}
				case 'S':
					// -S <string>: the string is a command line
					// env splits and runs, so it is checked.
					if j+1 < len(t) {
						splitValue = t[j+1:]
						isSplit = true
						j = len(t)
					} else {
						takesArg = true
						isSplit = true
					}
				case '0', 'v':
					// No argument.
				default:
					unknown = true
				}
				if takesArg || unknown {
					break
				}
			}
			if unknown {
				return nil, true
			}
			if isSplit && splitValue != "" && isBlockedCommand(splitValue, vars) {
				return nil, true
			}
			toks = toks[1:]
			if takesArg {
				if len(toks) == 0 {
					return nil, true
				}
				if isSplit && splitValue == "" && isBlockedCommand(toks[0], vars) {
					return nil, true
				}
				toks = toks[1:]
			}
			continue
		}
		break
	}
	toks = skipAssignments(toks)
	if len(toks) == 0 {
		if ignoreEnv {
			return nil, false
		}
		return nil, true
	}
	return toks, false
}

// blockedSu handles `su [user] [-c command]`. Without -c, su opens an
// interactive (root) shell and is refused outright. With -c, only the
// single command string after -c is checked (extra arguments become
// positional parameters of that shell, not part of the command).
func blockedSu(seg []string, vars map[string][]string) bool {
	for i := 1; i < len(seg); i++ {
		if seg[i] == "-c" || seg[i] == "--command" {
			if i+1 >= len(seg) {
				return true
			}
			return isBlockedCommand(seg[i+1], vars)
		}
		if strings.HasPrefix(seg[i], "--command=") {
			return isBlockedCommand(strings.TrimPrefix(seg[i], "--command="), vars)
		}
	}
	return true
}

// blockedRunuser handles runuser(1): -c/--command/--session-command take
// a command string (checked recursively); otherwise flags are skipped
// and the remaining tokens are the command to run.
func blockedRunuser(seg []string, vars map[string][]string) bool {
	for i := 1; i < len(seg); i++ {
		switch {
		case seg[i] == "-c" || seg[i] == "--command" || seg[i] == "--session-command":
			if i+1 >= len(seg) {
				return true
			}
			return isBlockedCommand(seg[i+1], vars)
		case strings.HasPrefix(seg[i], "--command="):
			return isBlockedCommand(strings.TrimPrefix(seg[i], "--command="), vars)
		case strings.HasPrefix(seg[i], "--session-command="):
			return isBlockedCommand(strings.TrimPrefix(seg[i], "--session-command="), vars)
		}
	}
	rest := skipFlags(seg[1:], runuserSpec)
	rest = skipAssignments(rest)
	if len(rest) == 0 {
		return false
	}
	return isBlockedCommand(strings.Join(rest, " "), vars)
}

// blockedExec handles the exec builtin: `exec [-a name] [-c] [-l]
// command [args...]` replaces the shell with the command, so the
// command is checked recursively. With no command, exec only applies
// redirections to the current shell (checked separately by
// blockedRedirect) or is a no-op, so it is allowed.
func blockedExec(seg []string, vars map[string][]string) bool {
	rest := skipFlags(seg[1:], execSpec)
	rest = skipAssignments(rest)
	if len(rest) == 0 {
		return false
	}
	return isBlockedCommand(strings.Join(rest, " "), vars)
}

// blockedChroot handles `chroot [OPTION] NEWROOT [COMMAND [ARG]...]`.
// The command after NEWROOT is checked recursively; with no command
// chroot runs a shell ($SHELL or /bin/sh) and is refused outright.
func blockedChroot(seg []string, vars map[string][]string) bool {
	rest := skipFlags(seg[1:], chrootSpec)
	if len(rest) == 0 {
		return true // no NEWROOT at all: refuse
	}
	if len(rest) == 1 {
		return true // no command: runs a shell
	}
	return isBlockedCommand(strings.Join(rest[1:], " "), vars)
}

// blockedWatch handles `watch [OPTION] COMMAND`: watch runs its
// arguments as a command (repeating it), so the command is checked
// recursively. With no command, watch is an error and harmless.
func blockedWatch(seg []string, vars map[string][]string) bool {
	rest := skipFlags(seg[1:], watchSpec)
	rest = skipAssignments(rest)
	if len(rest) == 0 {
		return false
	}
	return isBlockedCommand(strings.Join(rest, " "), vars)
}

// blockedScript handles script(1): `script -c 'command' [file]`
// runs the command (via a shell); without -c it opens an interactive
// shell and is refused outright. The -c argument is checked
// recursively; a glued -c'command' is handled too.
func blockedScript(seg []string, vars map[string][]string) bool {
	args := seg[1:]
	for i, a := range args {
		if a == "--" {
			break
		}
		if a == "-c" {
			if i+1 >= len(args) {
				return true
			}
			return isBlockedCommand(args[i+1], vars)
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 && !strings.HasPrefix(a, "--") {
			// Short-flag cluster: look for 'c'; a glued value
			// after it (-c'cmd') is the command.
			for j := 1; j < len(a); j++ {
				if a[j] == 'c' {
					if j+1 < len(a) {
						return isBlockedCommand(a[j+1:], vars)
					}
					if i+1 >= len(args) {
						return true
					}
					return isBlockedCommand(args[i+1], vars)
				}
			}
			continue
		}
		// First non-flag token is the typescript file; no -c seen.
		break
	}
	// No -c: script opens an interactive shell.
	return true
}

// isKillSignalArg reports whether s looks like a signal name or
// number following `kill -l`, so it is skipped rather than read as
// a PID.
func isKillSignalArg(s string) bool {
	if _, err := strconv.Atoi(s); err == nil {
		return true
	}
	name := strings.TrimPrefix(strings.ToUpper(s), "SIG")
	if len(name) == 0 {
		return false
	}
	for _, r := range name {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// blockedKill handles kill(1) and the kill builtin: only killing
// PID 1 (the container init) or a negative PID (a whole process
// group — -1 is every process the caller can signal, and as root
// that is everything) is refused. Other PIDs are the agent's own
// children and are allowed. Only ONE signal spec is consumed at the
// front (-9, -TERM, -s SIG, -n NUM, -q SIG VAL, -l [SIG]); after
// it, a token starting with "-" is a PID, not a signal, so
// `kill -9 -1` is refused. Signal flags are skipped so a signal
// number is never mistaken for a PID; job specs (%1) refer to the
// shell's own jobs and are allowed.
func blockedKill(seg []string) bool {
	args := seg[1:]
	i := 0
	signalSeen := false
	for i < len(args) {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if a == "-l" || a == "--list" {
			i++
			if i < len(args) && isKillSignalArg(args[i]) {
				i++
			}
			continue
		}
		if a == "-s" || a == "--signal" || a == "-n" {
			i += 2
			signalSeen = true
			continue
		}
		if a == "-q" {
			i += 3
			signalSeen = true
			continue
		}
		if strings.HasPrefix(a, "--signal=") || strings.HasPrefix(a, "--list=") {
			i++
			signalSeen = true
			continue
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 && !signalSeen {
			// A signal spec (-9, -TERM, -KILL, -sTERM, -n9):
			// a cluster of letters/digits, never a PID.
			i++
			signalSeen = true
			continue
		}
		break
	}
	for ; i < len(args); i++ {
		pid := args[i]
		if strings.HasPrefix(pid, "%") {
			continue // job spec: the shell's own job
		}
		if n, err := strconv.Atoi(pid); err == nil && (n == 1 || n < 0) {
			return true
		}
	}
	return false
}

// expandTarget expands $var and ${var} in a redirect target or
// file-operation operand using the assignments recorded for this
// command (vars). It reports ok=false when the target contains an
// expansion that cannot be resolved statically — an unknown
// variable, a command substitution $(...) or a backquote — because
// the target cannot then be proven safe: callers must refuse it.
// A resolved value is the recorded words joined with a space (an
// assignment's value is tokenized when recorded, so a multi-word
// value can only come from an explicit multi-word assignment).
func expandTarget(target string, vars map[string][]string) (string, bool) {
	if !strings.Contains(target, "$") && !strings.Contains(target, "`") {
		return target, true
	}
	var b strings.Builder
	for i := 0; i < len(target); {
		c := target[i]
		if c == '`' {
			return "", false // command substitution: unresolvable
		}
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		// '$': must be followed by a variable name, ${name}, or
		// '(' (command substitution, refused).
		if i+1 >= len(target) {
			return "", false
		}
		switch {
		case target[i+1] == '(':
			return "", false
		case target[i+1] == '{':
			end := strings.IndexByte(target[i+2:], '}')
			if end < 0 {
				return "", false
			}
			name := target[i+2 : i+2+end]
			if name == "" {
				return "", false
			}
			for j := 0; j < len(name); j++ {
				if !isVarNameChar(name[j]) {
					return "", false
				}
			}
			val, ok := vars[name]
			if !ok || len(val) == 0 {
				return "", false
			}
			b.WriteString(strings.Join(val, " "))
			i += 2 + end + 1
		case isVarNameChar(target[i+1]):
			j := i + 1
			for j < len(target) && isVarNameChar(target[j]) {
				j++
			}
			val, ok := vars[target[i+1:j]]
			if !ok || len(val) == 0 {
				return "", false
			}
			b.WriteString(strings.Join(val, " "))
			i = j
		default:
			// $$, $?, $#, $1, ... — not a resolvable variable
			// reference; refuse rather than guess.
			return "", false
		}
	}
	return b.String(), true
}

// destructiveTarget reports whether target, after variable
// expansion, must be refused as a write destination. An
// unresolvable expansion is refused outright (see expandTarget).
func destructiveTarget(target string, vars map[string][]string) bool {
	expanded, ok := expandTarget(target, vars)
	if !ok {
		return true
	}
	return isDestructiveTarget(expanded)
}

// blockedFileOp reports whether a file operation must be refused:
// cp/mv/ln writing to a system path, chown/chgrp/mkdir/rmdir
// touching a system path. kind selects which operands count:
// "cp" checks the destination (last operand / -t value), "mv"
// checks every operand (moving a system file away destroys it
// too), "ln" checks the link path (last operand / -t value), and
// "chown"/"chgrp"/"mkdir"/"rmdir" check every operand.
// Operands are expanded through vars first: a destination reached
// through a variable (cp /tmp/x $f with f=/etc/passwd) is checked
// after expansion, and an unresolvable expansion is refused.
func blockedFileOp(args []string, kind string, vars map[string][]string) bool {
	var spec flagSpec
	switch kind {
	case "cp", "mv", "ln":
		spec = copySpec
	case "chown", "chgrp":
		spec = chownSpec
	case "mkdir":
		spec = mkdirSpec
	default: // rmdir: no flag takes an argument
	}
	var operands []string
	var targetDir string
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			operands = append(operands, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "--") {
			name := a[2:]
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				if name[:eq] == "target-directory" {
					targetDir = name[eq+1:]
				}
				i++
				continue
			}
			if spec.longTakesArg[name] {
				if name == "target-directory" {
					if i+1 < len(args) {
						targetDir = args[i+1]
						i += 2
					} else {
						i++
					}
					continue
				}
				i += 2
				continue
			}
			i++
			continue
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			// Short-flag cluster.
			consumedNext := false
			for j := 1; j < len(a); j++ {
				if spec.shortTakesArg[a[j]] {
					if a[j] == 't' {
						if j+1 < len(a) {
							targetDir = a[j+1:]
						} else if i+1 < len(args) {
							targetDir = args[i+1]
							consumedNext = true
						}
					} else if j+1 >= len(a) {
						consumedNext = true
					}
					break
				}
			}
			i++
			if consumedNext {
				i++
			}
			continue
		}
		operands = append(operands, a)
		i++
	}
	switch kind {
	case "cp":
		if targetDir != "" && destructiveTarget(targetDir, vars) {
			return true
		}
		if len(operands) > 0 && destructiveTarget(operands[len(operands)-1], vars) {
			return true
		}
	case "mv":
		if targetDir != "" && destructiveTarget(targetDir, vars) {
			return true
		}
		for _, op := range operands {
			if destructiveTarget(op, vars) {
				return true
			}
		}
	case "ln":
		if targetDir != "" && destructiveTarget(targetDir, vars) {
			return true
		}
		if len(operands) > 0 && destructiveTarget(operands[len(operands)-1], vars) {
			return true
		}
	default: // chown, chgrp, mkdir, rmdir
		for _, op := range operands {
			if destructiveTarget(op, vars) {
				return true
			}
		}
	}
	return false
}

// blockedFind handles find(1): -delete is a bulk delete and is refused
// outright; -exec/-execdir/-ok/-okdir running a blocked binary (rm, dd,
// chmod, a shell, ...) is refused, and the exec'd command is also
// checked recursively so wrappers (sudo, ...) cannot smuggle it through.
func blockedFind(args []string, vars map[string][]string) bool {
	for i, a := range args {
		switch a {
		case "-delete":
			return true
		case "-exec", "-execdir", "-ok", "-okdir":
			if i+1 >= len(args) {
				return true // -exec with no command: refuse
			}
			next := filepath.Base(args[i+1])
			if blockedBinaries[next] || strings.HasPrefix(next, "mkfs") {
				return true
			}
			end := i + 1
			for end < len(args) && args[end] != ";" && args[end] != "+" {
				end++
			}
			// A wrapper around a blocked binary (sudo rm, env rm,
			// nice rm, timeout 5 rm, ...) must also be refused:
			// the recursive check below allows a plain rm of one
			// file, but in -exec it runs once per file found.
			if bin := resolveExecBinary(args[i+1:end], vars); bin != "" && (blockedBinaries[bin] || strings.HasPrefix(bin, "mkfs")) {
				return true
			}
			if isBlockedCommand(strings.Join(args[i+1:end], " "), vars) {
				return true
			}
		}
	}
	return false
}

// resolveExecBinary strips assignments, shell keywords and the
// wrapper commands (sudo/doas/env/nice/time/timeout/nohup/setsid/
// stdbuf/command/pkexec/runuser) from the front of toks and returns
// the basename of the real command, or "" when there is none. It
// mirrors the wrapper-stripping loop in blockedSegment and is used
// by blockedFind, where a wrapped rm is just as destructive as a
// bare one. A variable expansion as the command ($x) cannot be
// resolved here; the recursive check in blockedFind covers it.
func resolveExecBinary(toks []string, vars map[string][]string) string {
	seg := toks
	for len(seg) > 0 {
		seg = skipAssignments(seg)
		if len(seg) == 0 {
			return ""
		}
		if name, ok := varRef(seg[0]); ok {
			if val, ok := vars[name]; ok && len(val) > 0 {
				seg = append(append([]string{}, val...), seg[1:]...)
				continue
			}
		}
		base := filepath.Base(seg[0])
		if shellKeywords[base] {
			seg = seg[1:]
			continue
		}
		switch base {
		case "sudo":
			rest, verdict := resolveSudoLike(seg[1:], sudoSpec)
			if verdict != sudoContinue {
				return ""
			}
			seg = rest
		case "doas":
			rest, verdict := resolveSudoLike(seg[1:], doasSpec)
			if verdict != sudoContinue {
				return ""
			}
			seg = rest
		case "env":
			rest, blocked := resolveEnv(seg, vars)
			if blocked || len(rest) == 0 {
				return ""
			}
			seg = rest
		case "nice":
			seg = skipFlags(seg[1:], niceSpec)
		case "time":
			seg = skipFlags(seg[1:], timeSpec)
		case "nohup", "setsid":
			seg = skipFlags(seg[1:], noArgSpec)
		case "stdbuf":
			seg = skipFlags(seg[1:], stdbufSpec)
		case "command":
			seg = skipFlags(seg[1:], commandSpec)
		case "pkexec":
			seg = skipFlags(seg[1:], pkexecSpec)
		case "timeout":
			rest := skipFlags(seg[1:], timeoutSpec)
			if len(rest) == 0 {
				return ""
			}
			seg = rest[1:]
		case "runuser":
			rest := skipFlags(seg[1:], runuserSpec)
			rest = skipAssignments(rest)
			if len(rest) == 0 {
				return ""
			}
			seg = rest
		default:
			return base
		}
	}
	return ""
}

// varAssignmentRe matches a VAR=value token. The value is the raw
// text after the first '='; tokenize splits it into words when the
// variable is later expanded as a command.
var varAssignmentRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// assignmentKeywords introduce assignments: export/declare/local/
// readonly/typeset (and "let", which takes VAR=value too).
var assignmentKeywords = map[string]bool{
	"export": true, "declare": true, "local": true,
	"readonly": true, "typeset": true, "let": true,
}

// collectSegmentAssignments records the leading VAR=value
// assignments of seg into vars (including ones after an assignment
// keyword like export/declare/local). It is called on each segment
// in order, before that segment is checked, so an assignment only
// affects the segment it precedes (and later ones), matching shell
// semantics.
func collectSegmentAssignments(seg []string, vars map[string][]string) {
	toks := seg
	if len(toks) > 0 && assignmentKeywords[filepath.Base(toks[0])] {
		toks = toks[1:]
		for len(toks) > 0 && strings.HasPrefix(toks[0], "-") && toks[0] != "--" {
			toks = toks[1:]
		}
		if len(toks) > 0 && toks[0] == "--" {
			toks = toks[1:]
		}
	}
	for len(toks) > 0 {
		m := varAssignmentRe.FindStringSubmatch(toks[0])
		if m == nil {
			break
		}
		vars[m[1]] = tokenize(m[2])
		toks = toks[1:]
	}
}

// isVarNameChar reports whether c can appear in a shell variable
// name (after the first character).
func isVarNameChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// varRef reports whether tok is a bare variable expansion ($x or
// ${x}) and returns the variable name. Words that merely contain an
// expansion ("${x}y", "$x.txt") are not a bare expansion: they are
// a longer word, not a command, so they are left alone.
func varRef(tok string) (string, bool) {
	if !strings.HasPrefix(tok, "$") || len(tok) < 2 {
		return "", false
	}
	if tok[1] == '{' {
		i := strings.IndexByte(tok, '}')
		if i < 3 || i != len(tok)-1 {
			return "", false
		}
		name := tok[2:i]
		for j := 0; j < len(name); j++ {
			if !isVarNameChar(name[j]) {
				return "", false
			}
		}
		return name, true
	}
	i := 1
	for i < len(tok) && isVarNameChar(tok[i]) {
		i++
	}
	if i == 1 || i != len(tok) {
		return "", false
	}
	return tok[1:i], true
}

// blockedChmod handles chmod(1): 777/7777 (and the symbolic equivalents)
// are refused on any target, and ANY chmod of a system-path operand
// is refused whatever the mode — chmod 644 /etc/shadow, chmod 4755
// /bin/sh (setuid), chmod 000 /etc/passwd and chmod u+s /bin/bash
// are all host damage, not ordinary file work (mirroring chown,
// which refuses every system-path operand). The mode is the first
// non-flag operand; everything after it is a target. Targets are
// expanded through vars first, and an unresolvable expansion is
// refused. Ordinary chmod of ordinary files is allowed.
func blockedChmod(args []string, vars map[string][]string) bool {
	for _, a := range args {
		switch strings.TrimLeft(a, "0") {
		case "777", "7777", "a+rwx", "a=rwx", "ugo+rwx", "u=rwx,g=rwx,o=rwx":
			return true
		}
	}
	// Parse out the mode (first non-flag operand) and the targets.
	var targets []string
	modeSeen := false
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			i++
			targets = append(targets, args[i:]...)
			break
		}
		if strings.HasPrefix(a, "--") {
			name := a[2:]
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				i++
				continue
			}
			if name == "reference" {
				// --reference takes a value: skip it (it is
				// read, not modified, so it is not a target).
				i += 2
				continue
			}
			i++
			continue
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			i++
			continue
		}
		if !modeSeen {
			modeSeen = true
			i++
			continue
		}
		targets = append(targets, a)
		i++
	}
	for _, t := range targets {
		if destructiveTarget(t, vars) {
			return true
		}
	}
	return false
}

// destructiveRmArgs reports whether rm arguments are destructive. Any
// recursive form — short (-r/-R/-rf) or long (--recursive) — is refused
// outright, whatever the target; --no-preserve-root is refused on its
// own. Flag parsing stops at "--", so operands after it (e.g. a file
// literally named "-rf") are not mistaken for flags. Plain `rm file`
// with an explicit target is allowed.
func destructiveRmArgs(args []string) bool {
	endOfFlags := false
	for _, a := range args {
		if endOfFlags {
			continue
		}
		switch {
		case a == "--":
			endOfFlags = true
		case a == "--no-preserve-root":
			return true
		case a == "--recursive":
			return true
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--"):
			if strings.ContainsAny(a, "rR") {
				return true
			}
		}
	}
	return false
}

// captureParen captures a balanced (...) group starting at index start
// (just after the opening paren). It returns the inner text, the index
// of the closing paren, and whether the group closed. Quotes inside the
// group are honoured, so a paren inside quotes does not count.
func captureParen(r []rune, start int) (string, int, bool) {
	depth := 1
	var inSingle, inDouble bool
	for i := start; i < len(r); i++ {
		c := r[i]
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		if c == '\\' {
			i++
			continue
		}
		switch c {
		case '\'':
			if !inDouble {
				inSingle = true
			}
		case '"':
			inDouble = !inDouble
		case '(':
			if !inDouble {
				depth++
			}
		case ')':
			if !inDouble {
				depth--
				if depth == 0 {
					return string(r[start:i]), i, true
				}
			}
		}
	}
	return "", 0, false
}

// blockedSubstitution reports whether any $(...), <(...) or `...`
// command substitution in cmd is itself a blocked command. Substitutions
// inside single quotes are literal text and are ignored; inside double
// quotes they still execute, so they are still checked. (Unquoted
// substitutions are also caught by the segment check, because ( and )
// are segment separators; this check exists for the quoted cases.)
func blockedSubstitution(cmd string) bool {
	r := []rune(cmd)
	var inSingle, inDouble bool
	for i := 0; i < len(r); i++ {
		c := r[i]
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		if c == '\\' {
			i++ // escaped character cannot start a substitution
			continue
		}
		switch {
		case c == '\'' && !inDouble:
			inSingle = true
		case c == '"':
			inDouble = !inDouble
		case c == '`':
			end := i + 1
			for end < len(r) && r[end] != '`' {
				if r[end] == '\\' {
					end++
				}
				end++
			}
			if end < len(r) {
				if IsBlockedCommand(string(r[i+1 : end])) {
					return true
				}
				i = end
			}
		case c == '$' && i+1 < len(r) && r[i+1] == '(':
			content, end, ok := captureParen(r, i+2)
			if ok {
				if IsBlockedCommand(content) {
					return true
				}
				i = end
			}
		case c == '<' && i+1 < len(r) && r[i+1] == '(':
			content, end, ok := captureParen(r, i+2)
			if ok {
				if IsBlockedCommand(content) {
					return true
				}
				i = end
			}
		}
	}
	return false
}

// redirectTarget extracts the target of an output-redirection token.
// It handles glued forms (">/etc/passwd", "2>/etc/passwd", "&>file")
// and bare operators (">", "2>>") whose target is the next token; for a
// bare operator it returns ("", true). It returns ("", false) when tok
// is not an output redirection at all.
func redirectTarget(tok string) (string, bool) {
	i := 0
	for i < len(tok) && tok[i] >= '0' && tok[i] <= '9' {
		i++
	}
	if i < len(tok) && tok[i] == '&' {
		i++
	}
	if i >= len(tok) || tok[i] != '>' {
		return "", false
	}
	j := i
	for j < len(tok) && tok[j] == '>' {
		j++
	}
	if j < len(tok) && tok[j] == '|' { // ">|" (noclobber override)
		j++
	}
	return tok[j:], true
}

// blockedRedirect reports whether any output redirection in seg writes
// to a destructive target (a system path or device). Redirection to
// ordinary files, /dev/null and the standard streams is allowed.
// Targets are expanded through vars first: a target reached through
// a variable (f=/etc/passwd; echo x > $f, > ${f}, > $d/passwd) is
// checked after expansion, and an unresolvable $ expansion or a
// command-substitution target (echo x > $(echo /etc/passwd), where
// tokenize splits the substitution into its own tokens) is refused
// outright, because the destination cannot be proven safe.
func blockedRedirect(seg []string, vars map[string][]string) bool {
	for i, t := range seg {
		if target, ok := redirectTarget(t); ok && target != "" {
			if destructiveTarget(target, vars) {
				return true
			}
			continue
		}
		if target, ok := redirectTarget(t); ok && target == "" {
			if i+1 >= len(seg) {
				continue
			}
			next := seg[i+1]
			// A bare operator whose next token opens a command
			// substitution (or is a bare $ left over from one)
			// writes to whatever that substitution prints.
			if next == "(" || next == "$" || strings.HasPrefix(next, "$(") || strings.HasPrefix(next, "`") {
				return true
			}
			if destructiveTarget(next, vars) {
				return true
			}
		}
	}
	return false
}

// tokenize splits a shell command line into tokens, honouring single and
// double quotes and backslash escapes. It is a tokenizer only — it never
// executes anything. Command separators (;, |, &, newline, parentheses
// and braces) are emitted as their own tokens so each command they
// delimit is checked as its own segment.
func tokenize(cmd string) []string {
	var tokens []string
	var cur strings.Builder
	var quote rune
	escaped := false
	ansiSingle := false // the current single quote came from $'...',
	// where backslash is an escape (unlike plain '...')
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	r := []rune(cmd)
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case escaped:
			cur.WriteRune(c)
			escaped = false
		case c == '\\' && (quote != '\'' || ansiSingle):
			escaped = true
		case quote != 0 && c == quote:
			quote = 0
			ansiSingle = false
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
		case quote == 0 && c == '$' && i+1 < len(r) && r[i+1] == '{':
			// ${var} expansion: keep it as one word so the
			// denylist sees the expansion (and can resolve it
			// through vars) instead of '{' / '}' splitting it
			// into segment separators. Consume through the
			// closing '}' (an unterminated ${ is left as-is and
			// refused downstream as unresolvable).
			cur.WriteRune(c)
			cur.WriteRune(r[i+1])
			i += 2
			for i < len(r) && r[i] != '}' {
				cur.WriteRune(r[i])
				i++
			}
			if i < len(r) {
				cur.WriteRune(r[i]) // the '}'
			}
		case quote == 0 && c == '$' && i+1 < len(r) && (r[i+1] == '\'' || r[i+1] == '"'):
			// ANSI-C ($'...') and locale ($"...") quoting: the $
			// is quoting syntax, not part of the word, so it is
			// dropped and the quote opens. Without this, $'rm'
			// tokenizes as "$rm" and evades the binary check.
			quote = r[i+1]
			ansiSingle = quote == '\''
			i++
		case quote == 0 && (c == ' ' || c == '\t'):
			flush()
		case quote == 0 && c == '|' && strings.HasSuffix(cur.String(), ">"):
			// ">|" (noclobber override) is a single redirection operator,
			// not a pipe: keep it glued to the ">" so the redirect check
			// sees the operator and its target together.
			cur.WriteRune(c)
		case quote == 0 && (c == ';' || c == '|' || c == '&' || c == '\n' || c == '\r' || c == '(' || c == ')' || c == '{' || c == '}'):
			// Command separator: start a new segment.
			flush()
			tokens = append(tokens, string(c))
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return tokens
}

// splitSegments splits tokens on separators so each segment's leading
// binary is checked.
func splitSegments(tokens []string) [][]string {
	var segs [][]string
	var cur []string
	for _, t := range tokens {
		switch t {
		case ";", "|", "&", "\n", "\r", "(", ")", "{", "}":
			if len(cur) > 0 {
				segs = append(segs, cur)
				cur = nil
			}
		default:
			cur = append(cur, t)
		}
	}
	if len(cur) > 0 {
		segs = append(segs, cur)
	}
	return segs
}

// blockedSegment reports whether one ;- / |- / newline-delimited segment
// must be refused. Wrappers, env assignments and shell keywords are
// stripped first so the real command is what gets checked; some
// constructs (su -c, eval, xargs, busybox, find -exec) carry a nested
// command string, which is checked recursively.
func blockedSegment(seg []string, vars map[string][]string) bool {
	resolved := 0 // guard against cyclic indirection (x=$x)
	for len(seg) > 0 {
		seg = skipAssignments(seg)
		if len(seg) == 0 {
			return false // only assignments, e.g. FOO=1
		}
		// Resolve a leading variable expansion ($x / ${x}) using
		// assignments seen earlier in this command. The value is
		// spliced in and re-checked, so `x=rm; $x -rf /` and
		// `export x=rm; $x -rf /` are refused. An expansion with no
		// recorded assignment cannot be resolved statically and is
		// left to the sandbox.
		if name, ok := varRef(seg[0]); ok {
			if val, ok := vars[name]; ok && len(val) > 0 {
				resolved++
				if resolved > 8 {
					return false // self-referential / cyclic
				}
				seg = append(append([]string{}, val...), seg[1:]...)
				continue
			}
		}
		base := filepath.Base(seg[0])
		if shellKeywords[base] {
			seg = seg[1:]
			continue
		}
		switch base {
		case "sudo":
			rest, verdict := resolveSudoLike(seg[1:], sudoSpec)
			switch verdict {
			case sudoBlocked:
				return true
			case sudoAllowed:
				return false
			default:
				seg = rest
				continue
			}
		case "doas":
			rest, verdict := resolveSudoLike(seg[1:], doasSpec)
			switch verdict {
			case sudoBlocked:
				return true
			case sudoAllowed:
				return false
			default:
				seg = rest
				continue
			}
		case "su":
			return blockedSu(seg, vars)
		case "env":
			rest, blocked := resolveEnv(seg, vars)
			if blocked {
				return true
			}
			if len(rest) == 0 {
				return false
			}
			seg = rest
			continue
		case "nice":
			seg = skipFlags(seg[1:], niceSpec)
			continue
		case "time":
			seg = skipFlags(seg[1:], timeSpec)
			continue
		case "nohup", "setsid":
			seg = skipFlags(seg[1:], noArgSpec)
			continue
		case "stdbuf":
			seg = skipFlags(seg[1:], stdbufSpec)
			continue
		case "command":
			seg = skipFlags(seg[1:], commandSpec)
			continue
		case "pkexec":
			seg = skipFlags(seg[1:], pkexecSpec)
			continue
		case "timeout":
			rest := skipFlags(seg[1:], timeoutSpec)
			if len(rest) == 0 {
				return false
			}
			seg = rest[1:] // skip the duration operand
			continue
		case "builtin":
			// builtin <command>: strip one level and re-check.
			seg = seg[1:]
			continue
		case "exec":
			return blockedExec(seg, vars)
		case "chroot":
			return blockedChroot(seg, vars)
		case "watch":
			return blockedWatch(seg, vars)
		case "script":
			return blockedScript(seg, vars)
		case "kill":
			return blockedKill(seg)
		case "mount", "losetup":
			// Bare (no arguments) they only list mounts / loop
			// devices. With arguments they (un)mount filesystems
			// or configure loop devices: host administration.
			return len(seg) > 1
		case "cp", "mv", "ln", "chown", "chgrp", "mkdir", "rmdir":
			return blockedFileOp(seg[1:], base, vars)
		case "xargs":
			rest := skipFlags(seg[1:], xargsSpec)
			rest = skipAssignments(rest)
			if len(rest) == 0 {
				return false
			}
			return isBlockedCommand(strings.Join(rest, " "), vars)
		case "eval":
			if len(seg) == 1 {
				return false
			}
			return isBlockedCommand(strings.Join(seg[1:], " "), vars)
		case "busybox", "toybox":
			if len(seg) == 1 {
				return false
			}
			return isBlockedCommand(strings.Join(seg[1:], " "), vars)
		case "runuser":
			return blockedRunuser(seg, vars)
		case "find":
			return blockedFind(seg[1:], vars)
		case "tee":
			for _, a := range seg[1:] {
				// Flags (-a) and "-" (stdout) expand to
				// themselves and are not destructive; a
				// variable or substitution target is expanded
				// and refused when it cannot be proven safe.
				if strings.HasPrefix(a, "-") || a == "-" {
					continue
				}
				if destructiveTarget(a, vars) {
					return true
				}
			}
			return false
		case "printenv", "set", "export", "history":
			// With no arguments these dump the whole environment /
			// history, which may hold secrets; with arguments they are
			// ordinary lookups or assignments.
			return len(seg) == 1
		}
		// Interpreters given inline code (python3 -c, perl -e, awk
		// '...', ...): analyse the code argument for destructive
		// operations. A bare interpreter or a script file is not
		// inline code and is left to the sandboxing layer.
		if kind, ok := interpreterKind(base); ok {
			return blockedInterpreter(seg[1:], kind)
		}
		// Not a wrapper: check the binary itself.
		if strings.HasPrefix(base, "mkfs") {
			return true // mkfs.* — any filesystem maker
		}
		if !blockedBinaries[base] {
			return false
		}
		switch base {
		case "rm":
			return destructiveRmArgs(seg[1:])
		case "dd":
			// dd is refused outright: writing to the wrong of= destroys
			// a disk, and there is no safe general case worth allowing.
			return true
		case "chmod":
			return blockedChmod(seg[1:], vars)
		default:
			// mkfs, shred, reboot, shutdown, shells, etc: always blocked.
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------
// Interpreter one-liners (python3 -c, perl -e, ruby -e, node -e,
// php -r, lua -e, Rscript -e, awk '...').
//
// The code argument is analysed, not just the interpreter name: a
// one-liner is refused when the code itself deletes files, shells out
// to a destructive command, writes to or truncates a system path, or
// dumps the whole environment. Two normalized views of the code are
// built by analyzePayload: the skeleton (string contents replaced by
// "", comments removed) shows the code's real structure, so quoted
// text cannot fake a call; the expanded form (quote characters
// removed, string contents kept inline) shows what a shell-out would
// actually run, so a destructive command smuggled inside a string
// argument is still visible, including through one level of
// eval/exec nesting. Sinks are matched in the skeleton and the
// destructive argument in the expanded form, so merely mentioning a
// destructive command in printed text (print("rm -rf /")) is allowed.
//
// Known limits (see the tests): aliased or dynamically dispatched
// calls (o = os; o.system(...), getattr(os, "system")(...)), code
// built by string concatenation, code read from stdin or a script
// file, and %-formats / q() quoting in Perl/Ruby are not analysed.
// Those need sandboxing, not more denylist rules.
// ---------------------------------------------------------------------

// interpreterSpec describes how an interpreter takes inline code:
// codeShort are short-flag letters whose argument is code, codeLong
// are long-flag names whose argument is code, argShort / argLong are
// other flags that take a following argument (their argument must be
// skipped so it is not mistaken for a script file or for code), and
// scriptFlag, when set, means the rest of the command is a script file
// and its arguments, so scanning stops.
type interpreterSpec struct {
	codeShort  string
	codeLong   []string
	argShort   string
	argLong    []string
	scriptFlag byte
}

var interpreterSpecs = map[string]interpreterSpec{
	"python":  {codeShort: "c", argShort: "mWX"},
	"perl":    {codeShort: "eE", argShort: "MIFxi"},
	"ruby":    {codeShort: "eE", argShort: "IrCK"},
	"node":    {codeShort: "ep", codeLong: []string{"eval", "print"}, argShort: "rC", argLong: []string{"require", "import", "conditions"}},
	"php":     {codeShort: "rBE", argShort: "cd", argLong: []string{"define"}, scriptFlag: 'f'},
	"lua":     {codeShort: "e", argShort: "l"},
	"Rscript": {codeShort: "e", argLong: []string{"default-packages"}},
}

// versionedName reports whether base is name or name followed by a
// version suffix (python3, python3.12, python3.12t, perl5.38, php8.1).
func versionedName(base, name string) bool {
	if base == name {
		return true
	}
	if !strings.HasPrefix(base, name) {
		return false
	}
	rest := base[len(name):]
	rest = strings.TrimSuffix(rest, "t") // free-threaded build, e.g. python3.12t
	rest = strings.TrimSuffix(rest, "m")
	if rest == "" {
		return false
	}
	for i := 0; i < len(rest); i++ {
		if (rest[i] < '0' || rest[i] > '9') && rest[i] != '.' {
			return false
		}
	}
	return true
}

// interpreterKind maps a binary basename to its interpreter kind, or
// reports that it is not a scripting interpreter with inline code.
func interpreterKind(base string) (string, bool) {
	switch {
	case versionedName(base, "python") || versionedName(base, "pypy") || versionedName(base, "micropython") || versionedName(base, "jython"):
		return "python", true
	case versionedName(base, "perl"):
		return "perl", true
	case versionedName(base, "ruby"):
		return "ruby", true
	case versionedName(base, "php"):
		return "php", true
	case versionedName(base, "lua"):
		return "lua", true
	case base == "luajit":
		return "lua", true
	case base == "node" || base == "nodejs" || base == "bun":
		return "node", true
	case base == "deno":
		return "deno", true
	case base == "awk" || base == "gawk" || base == "mawk" || base == "nawk":
		return "awk", true
	case base == "Rscript":
		return "Rscript", true
	}
	return "", false
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// extractCodeArgs collects the inline-code arguments of an interpreter
// invocation: the argument of each -c/-e/-r/--eval-style flag, glued
// (-c"code") or separate (-c "code"), and repeated flags (perl -e 'a'
// -e 'b'). Flags that take other arguments are skipped, and scanning
// stops at "--" or at the first non-flag token (the script file: any
// -c after it is a script argument, not a code flag).
func extractCodeArgs(args []string, spec interpreterSpec) []string {
	var payloads []string
	i := 0
	for i < len(args) {
		t := args[i]
		if t == "--" {
			break
		}
		if strings.HasPrefix(t, "--") {
			name := t[2:]
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				if containsString(spec.codeLong, name[:eq]) {
					payloads = append(payloads, name[eq+1:])
				}
				i++
				continue
			}
			if containsString(spec.codeLong, name) {
				if i+1 < len(args) {
					payloads = append(payloads, args[i+1])
					i += 2
				} else {
					i++
				}
				continue
			}
			if containsString(spec.argLong, name) {
				// An argument-taking flag only consumes the next
				// token when it is not itself a flag: "--opt -e"
				// must not swallow the -e.
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i += 2
				} else {
					i++
				}
				continue
			}
			i++
			continue
		}
		if strings.HasPrefix(t, "-") && len(t) > 1 {
			consumedNext := false
			stop := false
			for j := 1; j < len(t); j++ {
				c := t[j]
				if spec.scriptFlag != 0 && c == spec.scriptFlag {
					stop = true
					break
				}
				if strings.IndexByte(spec.codeShort, c) >= 0 {
					// A code flag followed by another code flag
					// (e.g. node -pe) is a flag cluster, not glued
					// code: keep scanning the cluster.
					if j+1 < len(t) && strings.IndexByte(spec.codeShort, t[j+1]) >= 0 {
						continue
					}
					if j+1 < len(t) {
						payloads = append(payloads, t[j+1:])
					} else if i+1 < len(args) {
						payloads = append(payloads, args[i+1])
						consumedNext = true
					}
					break
				}
				if strings.IndexByte(spec.argShort, c) >= 0 {
					if j+1 >= len(t) && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
						consumedNext = true
					}
					break
				}
				// Unknown flag letter: keep scanning the cluster
				// (e.g. -nu in python, -ne in perl).
			}
			if stop {
				break
			}
			i++
			if consumedNext {
				i++
			}
			continue
		}
		// First non-flag token is the script file; the rest are its
		// arguments, not inline code.
		break
	}
	return payloads
}

// blockedAwk analyses an awk program: the first non-flag argument
// (skipping -F / -v arguments). A program read from a file (-f) is
// not inline code and is allowed.
func blockedAwk(args []string) bool {
	i := 0
	for i < len(args) {
		t := args[i]
		if t == "--" {
			i++
			break
		}
		if strings.HasPrefix(t, "--") {
			name := t[2:]
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				i++
				continue
			}
			switch name {
			case "file":
				return false
			case "field-separator", "assign":
				if i+1 < len(args) {
					i += 2
				} else {
					i++
				}
				continue
			}
			i++
			continue
		}
		if strings.HasPrefix(t, "-") && len(t) > 1 {
			if strings.HasPrefix(t, "-f") {
				return false
			}
			if t == "-F" || t == "-v" {
				if i+1 < len(args) {
					i += 2
				} else {
					i++
				}
				continue
			}
			if strings.HasPrefix(t, "-F") || strings.HasPrefix(t, "-v") {
				i++
				continue
			}
			i++
			continue
		}
		break
	}
	if i >= len(args) {
		return false
	}
	return blockedInterpreterPayload(args[i], "awk")
}

// blockedInterpreter reports whether an interpreter invocation with
// inline code must be refused.
func blockedInterpreter(args []string, kind string) bool {
	if kind == "awk" {
		return blockedAwk(args)
	}
	if kind == "deno" {
		// `deno eval <code>`: deno takes no -e/-c flag; the code
		// follows the eval subcommand (possibly after global
		// flags). Deno code is JavaScript, so it is analysed with
		// the node rules.
		for i, a := range args {
			if a == "eval" {
				for _, code := range args[i+1:] {
					if strings.HasPrefix(code, "-") {
						continue // a flag of eval, not the code
					}
					return blockedInterpreterPayload(code, "node")
				}
				return false
			}
		}
		return false
	}
	spec, ok := interpreterSpecs[kind]
	if !ok {
		return false
	}
	for _, code := range extractCodeArgs(args, spec) {
		if blockedInterpreterPayload(code, kind) {
			return true
		}
	}
	return false
}

// payloadAnalysis is the normalized view of one interpreter code
// argument.
type payloadAnalysis struct {
	skeleton  string   // string contents replaced by "", comments removed
	expanded  string   // quote characters removed, string contents kept inline, comments removed
	strings   []string // contents of every string literal, in order
	backticks []string // contents of backtick-quoted literals
}

const (
	payloadStrNone = iota
	payloadStrSingle
	payloadStrDouble
	payloadStrBacktick
	payloadStrTripleSingle
	payloadStrTripleDouble
)

// analyzePayload scans code once and builds both normalized views.
// String literals (single, double, backtick, and python triple-quoted)
// are honoured, so a quote inside a string does not end it, and a
// comment marker inside a string does not start a comment. Comments
// are per-language: # for python/perl/ruby/php/awk/Rscript, // and
// /* */ for node/php, -- (and --[[ ]]) for lua.
func analyzePayload(code, kind string) payloadAnalysis {
	r := []rune(code)
	var sk, ex strings.Builder
	var strs, backs []string
	state := payloadStrNone
	var content strings.Builder
	finishString := func() {
		s := content.String()
		strs = append(strs, s)
		if state == payloadStrBacktick {
			backs = append(backs, s)
		}
		sk.WriteString(`""`)
		ex.WriteString(s)
		content.Reset()
		state = payloadStrNone
	}
	for i := 0; i < len(r); {
		if state == payloadStrNone {
			// Comments (outside strings only).
			if kind != "node" && kind != "lua" && r[i] == '#' {
				for i < len(r) && r[i] != '\n' {
					i++
				}
				sk.WriteByte('\n')
				ex.WriteByte('\n')
				continue
			}
			if kind == "lua" && i+1 < len(r) && r[i] == '-' && r[i+1] == '-' {
				if i+3 < len(r) && r[i+2] == '[' && r[i+3] == '[' {
					i += 4
					for i+1 < len(r) && !(r[i] == ']' && r[i+1] == ']') {
						i++
					}
					i += 2
				} else {
					for i < len(r) && r[i] != '\n' {
						i++
					}
				}
				sk.WriteByte('\n')
				ex.WriteByte('\n')
				continue
			}
			if (kind == "node" || kind == "php") && i+1 < len(r) && r[i] == '/' && r[i+1] == '/' {
				for i < len(r) && r[i] != '\n' {
					i++
				}
				sk.WriteByte('\n')
				ex.WriteByte('\n')
				continue
			}
			if (kind == "node" || kind == "php") && i+1 < len(r) && r[i] == '/' && r[i+1] == '*' {
				i += 2
				for i+1 < len(r) && !(r[i] == '*' && r[i+1] == '/') {
					i++
				}
				i += 2
				sk.WriteByte(' ')
				ex.WriteByte(' ')
				continue
			}
			// Triple-quoted strings (python only).
			if kind == "python" && i+2 < len(r) && r[i] == '\'' && r[i+1] == '\'' && r[i+2] == '\'' {
				state = payloadStrTripleSingle
				i += 3
				continue
			}
			if kind == "python" && i+2 < len(r) && r[i] == '"' && r[i+1] == '"' && r[i+2] == '"' {
				state = payloadStrTripleDouble
				i += 3
				continue
			}
			switch r[i] {
			case '\'':
				state = payloadStrSingle
				i++
				continue
			case '"':
				state = payloadStrDouble
				i++
				continue
			case '`':
				state = payloadStrBacktick
				i++
				continue
			}
			sk.WriteRune(r[i])
			ex.WriteRune(r[i])
			i++
			continue
		}
		// Inside a string.
		switch state {
		case payloadStrTripleSingle:
			if i+2 < len(r) && r[i] == '\'' && r[i+1] == '\'' && r[i+2] == '\'' {
				i += 3
				finishString()
				continue
			}
		case payloadStrTripleDouble:
			if i+2 < len(r) && r[i] == '"' && r[i+1] == '"' && r[i+2] == '"' {
				i += 3
				finishString()
				continue
			}
		case payloadStrSingle:
			if r[i] == '\'' {
				i++
				finishString()
				continue
			}
		case payloadStrDouble:
			if r[i] == '"' {
				i++
				finishString()
				continue
			}
		case payloadStrBacktick:
			if r[i] == '`' {
				i++
				finishString()
				continue
			}
		}
		if r[i] == '\\' && i+1 < len(r) {
			// Escaped character: keep both in the content so an
			// escaped quote does not end the string early.
			content.WriteRune(r[i])
			content.WriteRune(r[i+1])
			i += 2
			continue
		}
		content.WriteRune(r[i])
		i++
	}
	if state != payloadStrNone {
		// Unterminated string: still count its contents.
		finishString()
	}
	return payloadAnalysis{skeleton: sk.String(), expanded: ex.String(), strings: strs, backticks: backs}
}

// interpreterCmdPos matches a position where a shell command can
// start: the start of the string, or just after a separator, an
// opening bracket/paren, or an argument comma, or just after a
// paren-less sink keyword (Perl/Ruby `system 'rm -rf /'`, where the
// command follows the sink name), optionally followed by the quote
// that opens a nested string argument (system("rm -rf /"), where the
// expanded form keeps the inner quotes) and an optional privilege
// wrapper. A destructive binary only counts there, so `echo rm -rf /`
// and `echo "rm -rf /"` (rm as an argument to echo, where the quote
// follows a word rather than a separator) do not count.
const interpreterCmdPos = `(?:^|[;&|(\[,]\s*|\b(?:system|eval|exec|spawn|passthru|shell_exec|popen)\s+)["']?\s*(?:sudo\s+|doas\s+|env\s+)?`

// interpreterDestructiveAPIs match destructive operations in the code
// itself. They need no shell-out: the call itself deletes, truncates
// or forks. Perl/Ruby/PHP builtins are often called without parens
// (unlink 'file', system 'cmd'), so the bare-builtin patterns accept
// either a paren or a following argument; merely naming one (my
// $unlink = 1, sub unlink { }) does not match.
var interpreterDestructiveAPIs = []*regexp.Regexp{
	regexp.MustCompile(`\bos\s*\.\s*(remove|unlink|rmdir|removedirs)\s*\(`),
	regexp.MustCompile(`\bos\s*\.\s*fork\b`),
	regexp.MustCompile(`\bshutil\s*\.\s*rmtree\s*\(`),
	regexp.MustCompile(`\.\s*(unlink|rmdir|unlinkSync|rmSync|rmdirSync|removeSync|rm)\b`),
	regexp.MustCompile(`\bDeno\s*\.\s*remove\b`),
	regexp.MustCompile(`\bunlink(?:\s*\(|\s+["$/@%A-Za-z_])`),
	regexp.MustCompile(`\brmtree(?:\s*\(|\s+["$/@%A-Za-z_])`),
	regexp.MustCompile(`\brmdir(?:\s*\(|\s+["$/@%A-Za-z_])`),
	regexp.MustCompile(`\bFile\s*\.\s*(delete|unlink)\b`),
	regexp.MustCompile(`\bFileUtils\s*\.\s*(rm_rf|rmtree|rm)\b`),
	regexp.MustCompile(`\bfile\.remove\s*\(`),
	regexp.MustCompile(`\bfork\s*\(`),
	regexp.MustCompile(`\bfork\s*\{`),
	regexp.MustCompile(`\bfork\s+do\b`),
	regexp.MustCompile(`\bfork\s*[;,]`),
	regexp.MustCompile(`\bfork\s*$`),
}

// interpreterExecSinks match calls that hand a string (or argument
// list) to another executor: a shell, a subprocess, or eval. A sink
// alone is not refused — only a sink with a destructive argument —
// so os.system("ls") and subprocess.run(["ls"]) stay allowed.
var interpreterExecSinks = []*regexp.Regexp{
	regexp.MustCompile(`\bos\s*\.\s*(system|popen|exec\w*|spawn\w*|execute)\b`),
	regexp.MustCompile(`\bsubprocess\s*\.\s*(call|run|Popen|check_output|check_call|getoutput|getstatusoutput)\s*\(`),
	regexp.MustCompile(`\bcommands\s*\.\s*(getoutput|getstatusoutput)\s*\(`),
	regexp.MustCompile(`\.\s*(system|popen)\s*\(`),
	regexp.MustCompile(`\bsystem(?:\s*\(|\s+["$/@%A-Za-z_])`),
	regexp.MustCompile(`\beval(?:\s*\(|\s+["$/@%A-Za-z_])`),
	regexp.MustCompile(`\bexec(?:\s*\(|\s+["$/@%A-Za-z_])`),
	regexp.MustCompile(`\bexec(Sync|File)\s*\(`),
	regexp.MustCompile(`\bspawn(Sync)?\s*\(`),
	regexp.MustCompile(`\bshell_exec\s*\(`),
	regexp.MustCompile(`\bpassthru\s*\(`),
	regexp.MustCompile(`\bpopen\s*\(`),
	regexp.MustCompile(`\bproc_open\s*\(`),
	regexp.MustCompile(`\bio\s*\.\s*popen\b`),
	regexp.MustCompile(`\bgetattr\s*\(`),
	regexp.MustCompile(`\b(send|public_send|__send__)\s*\(`),
	regexp.MustCompile(`__import__\s*\([^)]*\)\s*\.\s*(system|popen|exec\w*|spawn\w*|remove|unlink|rmdir)\b`),
}

// interpreterDestructiveShell matches destructive shell commands in
// an expanded payload (string contents inline) or in a backtick
// string. Destructive binaries only count at a command position (see
// interpreterCmdPos), so `echo dd` or `echo rm -rf /` do not count.
var interpreterDestructiveShell = []*regexp.Regexp{
	// rm with a recursive flag, or rm aimed at / or a system tree.
	regexp.MustCompile(interpreterCmdPos + `\brm\b[^;&|\n]{0,80}?(?:-[a-zA-Z]*[rR]|--recursive|--no-preserve-root)`),
	regexp.MustCompile(interpreterCmdPos + `\brm\b[^;&|\n]{0,80}?(?:\s/(?:\s|$)|/\*|~|\$HOME|/etc(?:/|$)|/usr(?:/|$)|/home(?:/|$)|/var(?:/|$)|/opt(?:/|$)|/root(?:/|$)|/boot(?:/|$)|/sbin(?:/|$)|/bin(?:/|$)|/dev(?:/|$))`),
	// Disk / partition / power tools at a command position.
	regexp.MustCompile(interpreterCmdPos + `\b(dd|mkfs\w*|shred|wipefs|wipe|fdisk|sfdisk|cfdisk|sgdisk|parted|blkdiscard|cryptsetup|shutdown|reboot|poweroff|halt)\b`),
	// truncate / chmod / tee aimed at a system target.
	regexp.MustCompile(interpreterCmdPos + `\btruncate\b[^;&|\n]{0,60}?(?:/etc|/usr|/home|/var|/opt|/root|/boot|/dev|~|\$HOME)`),
	regexp.MustCompile(interpreterCmdPos + `\bchmod\b[^;&|\n]{0,60}?(?:\b7777\b|\b777\b)`),
	regexp.MustCompile(interpreterCmdPos + `\btee\b[^;&|\n]{0,60}?(?:/etc|/usr|/bin|/sbin|/var|/opt|/home|/root|/boot|/dev|~|\$HOME)`),
	// Output redirection onto a system path or device.
	regexp.MustCompile(`>+?\s*(?:/etc|/usr|/bin|/sbin|/var|/opt|/home|/root|/boot|/dev/sd|~)`),
	// Piping anything into a shell.
	regexp.MustCompile(`\|\s*(?:sudo\s+)?(?:\S*/)?(sh|bash|zsh|dash|fish|ksh)\b`),
	// Shell fork bomb.
	regexp.MustCompile(`:\(\)\s*\{`),
}

func hasExecSink(skeleton string) bool {
	for _, re := range interpreterExecSinks {
		if re.MatchString(skeleton) {
			return true
		}
	}
	return false
}

func destructiveShellCommand(s string) bool {
	for _, re := range interpreterDestructiveShell {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// envDumpRes matches whole-environment reads: os.environ, Node's
// process.env, Perl's %ENV. The shell denylist refuses bare env /
// printenv for the same reason (the whole environment may hold
// secrets); a single lookup (os.environ['HOME'], process.env.HOME)
// is allowed and is filtered out by dumpsEnvironment.
var envDumpRes = []*regexp.Regexp{
	regexp.MustCompile(`\bos\s*\.\s*environ\b`),
	regexp.MustCompile(`\bprocess\s*\.\s*env\b`),
	regexp.MustCompile(`%ENV\b`),
}

func dumpsEnvironment(skeleton string) bool {
	for _, re := range envDumpRes {
		for _, loc := range re.FindAllStringIndex(skeleton, -1) {
			rest := strings.TrimLeft(skeleton[loc[1]:], " \t")
			if strings.HasPrefix(rest, ".") || strings.HasPrefix(rest, "[") || strings.HasPrefix(rest, "{") {
				continue // a single lookup, not a dump
			}
			return true
		}
	}
	return false
}

// interpreterWriteSinks are calls that inherently write a file.
var interpreterWriteSinks = regexp.MustCompile(`\b(createWriteStream|appendFile\w*|writeFile\w*|file_put_contents)\b|\.\s*write_(text|bytes)\s*\(`)

// interpreterTruncate matches truncation calls (python os.truncate,
// Perl truncate 'file', 0, Ruby File.truncate). In the skeleton a
// string literal is "", so the Perl form is `truncate "", 0`.
var interpreterTruncate = regexp.MustCompile(`(?:\bos\s*\.\s*truncate\b|\btruncate\s*\(|\btruncate\s+"")`)

// writesSystemPath reports whether the code writes to, or truncates,
// a system path: a system-path string literal combined with a
// write-mode literal ("w", "a", ">", ...), an inherently writing call
// (fs.writeFileSync, file_put_contents, Path.write_text), a truncate
// call, or — awk only — an output redirection (print > "/etc/...").
// Reading a system path (open("/etc/hostname"), file_get_contents)
// has no write evidence and stays allowed.
func writesSystemPath(skeleton string, strs []string, kind string) bool {
	hasSystem := false
	for _, s := range strs {
		if isDestructiveTarget(strings.TrimSpace(s)) {
			hasSystem = true
			break
		}
	}
	if !hasSystem {
		return false
	}
	for _, s := range strs {
		switch strings.TrimSpace(s) {
		case "w", "a", "w+", "a+", "wb", "ab", "w+b", "a+b", "r+", ">", ">>":
			return true
		}
	}
	if interpreterWriteSinks.MatchString(skeleton) || interpreterTruncate.MatchString(skeleton) {
		return true
	}
	if kind == "awk" && strings.Contains(skeleton, ">") {
		return true
	}
	return false
}

// interpreterChmodRe matches permission/ownership-changing calls
// (os.chmod, fs.chmodSync, File.chmod, chmod 0777, ...).
var interpreterChmodRe = regexp.MustCompile(`\b(?:chmod|chown|lchmod|lchown|chmodSync|chownSync|fchmod|fchown)(?:\s*\(|\s+[0-9"'])`)

// chmodsSystemPath reports whether the code changes permissions or
// ownership of a system path: a system-path string literal combined
// with a chmod/chown call. (Reading a system path, or chmod of an
// ordinary file, stays allowed.) Like writesSystemPath this is a
// heuristic: a system path mentioned anywhere in the one-liner plus
// a chmod/chown call anywhere in it is refused.
func chmodsSystemPath(skeleton string, strs []string) bool {
	hasSystem := false
	for _, s := range strs {
		if isDestructiveTarget(strings.TrimSpace(s)) {
			hasSystem = true
			break
		}
	}
	return hasSystem && interpreterChmodRe.MatchString(skeleton)
}

// blockedInterpreterPayload reports whether one interpreter code
// argument must be refused.
func blockedInterpreterPayload(code, kind string) bool {
	a := analyzePayload(code, kind)
	// Destructive operations in the code itself.
	for _, re := range interpreterDestructiveAPIs {
		if re.MatchString(a.skeleton) {
			return true
		}
	}
	// Whole-environment dumps.
	if dumpsEnvironment(a.skeleton) {
		return true
	}
	// Writes / truncation of system paths.
	if writesSystemPath(a.skeleton, a.strings, kind) {
		return true
	}
	// chmod / chown of system paths.
	if chmodsSystemPath(a.skeleton, a.strings) {
		return true
	}
	// Backtick execution (Perl/Ruby/PHP): the backtick string IS a
	// shell command, so a destructive one inside it is refused even
	// with no other sink. Node template literals are not execution,
	// so they are not checked here.
	if kind == "perl" || kind == "ruby" || kind == "php" {
		for _, b := range a.backticks {
			if destructiveShellCommand(b) {
				return true
			}
		}
	}
	// Shelling out is only refused when the command it runs is
	// destructive: the sink is matched in the skeleton (real code, so
	// quoted text cannot fake one) and the command in the expanded
	// form (string contents inline, so a destructive command smuggled
	// inside a string argument is still visible, including through
	// one level of eval/exec nesting).
	if hasExecSink(a.skeleton) {
		if destructiveShellCommand(a.expanded) {
			return true
		}
		for _, re := range interpreterDestructiveAPIs {
			if re.MatchString(a.expanded) {
				return true
			}
		}
	}
	return false
}

// IsBlockedCommand reports whether a shell command string must be refused.
// It is TOKEN-BASED: the binary basename is resolved (so /bin/rm,
// /usr/bin/rm, ./rm all match "rm"), wrappers and shell keywords are
// stripped, command substitution is checked recursively, and destructive
// patterns are checked per segment. Empty commands are blocked.
func IsBlockedCommand(cmd string) bool {
	return isBlockedCommand(cmd, nil)
}

// isBlockedCommand is IsBlockedCommand with the variable assignments
// from an enclosing command (an eval string, a -c argument, ...).
// outer is copied so a nested command cannot pollute its caller's map.
func isBlockedCommand(cmd string, outer map[string][]string) bool {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return true
	}
	// Command / process substitution executes even when it hides inside
	// an otherwise innocent command; check it before tokenizing.
	if blockedSubstitution(trimmed) {
		return true
	}
	tokens := tokenize(trimmed)
	if len(tokens) == 0 {
		return true
	}
	vars := make(map[string][]string, len(outer))
	for k, v := range outer {
		vars[k] = v
	}
	for _, seg := range splitSegments(tokens) {
		if len(seg) == 0 {
			continue
		}
		// Record this segment's leading assignments before checking
		// it, so `x=rm; $x -rf /` resolves while an assignment in a
		// later segment does not affect an earlier one.
		collectSegmentAssignments(seg, vars)
		if blockedSegment(seg, vars) || blockedRedirect(seg, vars) {
			return true
		}
	}
	return false
}

// IsBlocked is an alias kept for readability at call sites.
func IsBlocked(cmd string) bool { return IsBlockedCommand(cmd) }
