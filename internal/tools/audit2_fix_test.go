package tools

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Audit-2 fixes: file_read on a fifo with no writer must return
// (via the context/timeout) instead of blocking forever; scrubEnv
// must strip more credential-looking variable names; file_list must
// not read an unbounded number of entries into memory; isPublicIP
// must refuse the ranges net.IP's own checks miss; process_list's
// ps fallback must not leak full command lines.

// TestAudit2FileReadFifoWithoutWriterReturns: a fifo with no writer
// used to block file_read forever (os.ReadFile does not watch the
// context). It must now return when the context expires.
func TestAudit2FileReadFifoWithoutWriterReturns(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "f.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	tool := &fileReadTool{roots: []string{dir}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	var out string
	var err error
	go func() {
		out, err = tool.Execute(ctx, mustJSON(t, map[string]any{"path": fifo}))
		close(done)
	}()
	select {
	case <-done:
		t.Logf("file_read on writer-less fifo returned: out=%q err=%v", out, err)
	case <-time.After(5 * time.Second):
		t.Errorf("file_read on fifo BLOCKED (did not return in 5s) — hang vector")
	}
}

// TestAudit2FileEditRefusesNonRegular: editing a fifo (or socket /
// device) would block on read; it is refused outright.
func TestAudit2FileEditRefusesNonRegular(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "f.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	tool := &fileEditTool{roots: []string{dir}}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"path": fifo, "old_text": "a", "new_text": "b",
	}))
	if err == nil {
		t.Fatalf("file_edit on a fifo: want error, got nil")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("file_edit on a fifo: error %q, want 'not a regular file'", err)
	}
}

// TestAudit2ScrubEnvStripsCredentialVars: variable names containing
// KEY, CREDENTIAL, AUTH or BEARER (not just API_KEY/TOKEN/SECRET/
// PASSWORD) must not reach a shell child.
func TestAudit2ScrubEnvStripsCredentialVars(t *testing.T) {
	t.Setenv("PRIVATE_KEY", "fake-private-key-value")
	t.Setenv("SSH_KEY", "fake-ssh-key-value")
	t.Setenv("MY_AUTH_HEADER", "fake-auth-value")
	t.Setenv("CREDENTIALS_FILE", "/tmp/fake-creds")
	t.Setenv("GITHUB_KEY", "fake-github-key")
	t.Setenv("BEARER_TOKEN_X", "fake-bearer-value")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/tmp")
	env := scrubEnv()
	joined := strings.Join(env, "\n")
	for _, leak := range []string{
		"fake-private-key-value", "fake-ssh-key-value", "fake-auth-value",
		"fake-github-key", "fake-bearer-value",
	} {
		if strings.Contains(joined, leak) {
			t.Errorf("scrubEnv leaked %q", leak)
		}
	}
	if strings.Contains(joined, "CREDENTIALS_FILE") {
		t.Errorf("scrubEnv leaked CREDENTIALS_FILE")
	}
	if !strings.Contains(joined, "PATH=/usr/bin:/bin") {
		t.Errorf("scrubEnv dropped PATH")
	}
}

// TestAudit2FileListCapsRead: a directory with more than maxListRead
// entries must be reported as "more than N" without reading them
// all into memory, and only the first maxListEntries shown.
func TestAudit2FileListCapsRead(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxListRead+1; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d", i)), []byte("x"), 0o600); err != nil {
			t.Fatalf("create file %d: %v", i, err)
		}
	}
	tool := &fileListTool{roots: []string{dir}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": dir}))
	if err != nil {
		t.Fatalf("file_list: %v", err)
	}
	if !strings.Contains(out, fmt.Sprintf("more than %d entries, showing first %d", maxListRead, maxListEntries)) {
		t.Errorf("file_list output missing the 'more than %d entries' marker:\n%s", maxListRead, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// 1 header line + maxListEntries entries + 1 marker line.
	if len(lines) != 1+maxListEntries+1 {
		t.Errorf("file_list returned %d lines, want %d", len(lines), 1+maxListEntries+1)
	}
}

// TestAudit2IsPublicIPRanges: the ranges net.IP's IsPrivate /
// IsLoopback / IsLinkLocal* / IsMulticast / IsUnspecified miss are
// refused; a genuinely public address passes.
func TestAudit2IsPublicIPRanges(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"0.1.2.3", false},          // 0/8 "this network"
		{"100.64.1.2", false},       // CGNAT
		{"192.0.0.1", false},        // IETF protocol assignments
		{"240.1.2.3", false},        // reserved
		{"255.255.255.255", false},  // broadcast (in 240/4)
		{"2001:db8::1", false},      // IPv6 documentation
		{"64:ff9b::8.8.8.8", false}, // NAT64 well-known prefix
		{"8.8.8.8", true},
		{"93.184.216.34", true}, // example.com's public address
		{"2001:4860:4860::8888", true},
		// 198.18/15 is non-public by default (strict): the old
		// global exception for this deployment's egress DNS is
		// gone, replaced by config trusted_egress_cidrs (see the
		// trusted-range check below).
		{"198.18.245.209", false},
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("ParseIP(%q) failed", tc.ip)
		}
		if got := isPublicIP(ip, nil); got != tc.want {
			t.Errorf("isPublicIP(%s, nil) = %v, want %v", tc.ip, got, tc.want)
		}
	}

	// Trusted egress range: 198.18/15 counts as public only when
	// listed in trusted_egress_cidrs — and only that range, not
	// other non-public addresses.
	trusted, err := parseTrustedEgressCIDRs([]string{"198.18.0.0/15"})
	if err != nil {
		t.Fatalf("parseTrustedEgressCIDRs: %v", err)
	}
	if !isPublicIP(net.ParseIP("198.18.245.209"), trusted) {
		t.Errorf("isPublicIP(198.18.245.209, trusted 198.18/15) = false, want true")
	}
	if !isPublicIP(net.ParseIP("198.19.0.1"), trusted) {
		t.Errorf("isPublicIP(198.19.0.1, trusted 198.18/15) = false, want true")
	}
	if isPublicIP(net.ParseIP("10.0.0.1"), trusted) {
		t.Errorf("isPublicIP(10.0.0.1, trusted 198.18/15) = true, want false (unrelated private range stays refused)")
	}
}

// TestAudit2ListProcsViaPSHasNoArgs: the ps fallback must return
// pid/command/state only; a command name is a single word, so a
// space in it would mean full command lines (and secrets in them)
// leaked through.
func TestAudit2ListProcsViaPSHasNoArgs(t *testing.T) {
	procs, err := listProcsViaPS(context.Background())
	if err != nil {
		t.Fatalf("listProcsViaPS: %v", err)
	}
	if len(procs) == 0 {
		t.Fatalf("listProcsViaPS returned no processes")
	}
	for _, p := range procs {
		if strings.Contains(p.comm, " ") {
			t.Errorf("process %d comm %q contains a space: full command line leaked", p.pid, p.comm)
		}
	}
}
