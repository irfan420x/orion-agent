// cloneprobe tests whether clone(2) with the given flags is allowed
// under the sandbox's seccomp filter. Usage:
//
//	cloneprobe <flags-decimal>
//
// The child exits 42 immediately. A plain clone (SIGCHLD only, 17)
// must succeed; a namespace clone (e.g. CLONE_NEWNS|SIGCHLD,
// 131089) must fail with EPERM when the filter is installed.
//
// The child is created without CLONE_VM, so it is an ordinary
// fork-like child; the only thing being tested is whether the
// clone(2) call itself is permitted.
#define _GNU_SOURCE
#include <sched.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/wait.h>
#include <unistd.h>

static char child_stack[8192];

static int child_fn(void *arg) {
	(void)arg;
	_exit(42);
	return 0;
}

int main(int argc, char **argv) {
	if (argc != 2) {
		fprintf(stderr, "usage: cloneprobe <flags>\n");
		return 2;
	}
	int flags = atoi(argv[1]);
	pid_t pid = clone(child_fn, child_stack + sizeof(child_stack), flags, NULL);
	if (pid == -1) {
		perror("clone");
		return 1;
	}
	int status = 0;
	if (waitpid(pid, &status, 0) == -1) {
		perror("waitpid");
		return 1;
	}
	if (WIFEXITED(status)) {
		printf("clone ok status=%d\n", WEXITSTATUS(status));
		return 0;
	}
	fprintf(stderr, "clone child did not exit normally\n");
	return 1;
}
