// Command orion-seccomp is the sandbox's seccomp helper. It installs
// the Orion seccomp filter on itself (see
// internal/sandbox/seccomp.go) and then execs its arguments: the
// filter persists across execve, so the command it execs runs under
// the filter.
//
// It is exec'd by the sandbox wrapper after the chroot, as:
//
//	orion-seccomp /bin/sh -c 'cd /work || exit 126; exec /bin/sh -c "$1"' _ "<command>"
//
// Build it statically (CGO_ENABLED=0) and install it next to the
// orion/oriond binaries; the sandbox discovers it there (see
// Config.findSeccompLauncher). If the filter cannot be installed —
// unsupported architecture, kernel without seccomp — it exits 126
// and the command does not run: a half-filtered sandbox is worse
// than a loud failure.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/irfan/orion/internal/sandbox"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "orion-seccomp: usage: orion-seccomp <command> [args...]")
		os.Exit(2)
	}
	if err := sandbox.InstallSeccompFilter(); err != nil {
		fmt.Fprintf(os.Stderr, "orion-seccomp: %v\n", err)
		os.Exit(126)
	}
	path, err := exec.LookPath(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "orion-seccomp: %v\n", err)
		os.Exit(127)
	}
	if err := syscall.Exec(path, os.Args[1:], os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "orion-seccomp: exec %s: %v\n", path, err)
		os.Exit(127)
	}
}
