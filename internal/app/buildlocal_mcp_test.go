// F-A21-9 regression test: BuildLocal must wire MCP before it loads
// skills, as Build does. Before the fix loadSkills ran first, so the
// CLI's runtime known-tool set never contained MCP names and a skill
// naming an MCP tool warned "unknown tool" — misleading, because the
// tool was registered moments later and worked. The test builds the
// real wiring end to end (fake MCP server + a skill naming its tool)
// and asserts the warning is gone and the tool is registered.
//
// The fake server is the internal/mcp test binary compiled with
// `go test -c`: the same binary the mcp package's own tests
// re-invoke as their fake server. The test lives in internal/app
// (not cmd/orion) because that is where BuildLocal lives; it shells
// out to `go` only to compile the fake server, and skips when no Go
// toolchain is on PATH.
package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestBuildLocalWiresMCPBeforeSkills(t *testing.T) {
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH: cannot compile the fake MCP server")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	skillDir := filepath.Join(dataDir, "skills")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Compile the fake MCP server (internal/mcp's own test binary,
	// which acts as a server when run with -test.run matching
	// TestFakeMCPServerProcess).
	fakeBin := filepath.Join(dir, "fake-mcp.test")
	compile := exec.Command(goPath, "test", "-c", "-o", fakeBin, "./internal/mcp")
	compile.Dir = repoRoot
	if out, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("compile fake MCP server: %v\n%s", err, out)
	}

	// One fake server, unsandboxed (the binary lives under /tmp,
	// never under /usr, so a sandboxed start would refuse it —
	// the same reason the mcp package's own tests run it
	// unsandboxed).
	mcpCfg := map[string]any{"mcpServers": map[string]any{"fake": map[string]any{
		"command": fakeBin,
		"args":    []string{"-test.run=^TestFakeMCPServerProcess$", "-test.count=1"},
		"env":     map[string]string{"ORION_TEST_FAKE_MCP_SERVER": "1"},
		"sandbox": false,
	}}}
	raw, err := json.Marshal(mcpCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "mcp.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// A skill whose ## Tools names the fake server's echo tool
	// (registered as mcp__fake__echo).
	skill := "# MCP User\n\nA skill that uses an MCP tool.\n\n## Tools\n\n- mcp__fake__echo\n"
	if err := os.WriteFile(filepath.Join(skillDir, "mcpuser.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}

	// A config that points at the scratch data dir and nothing else.
	cfgPath := filepath.Join(dir, "config.json")
	cfgRaw, err := json.Marshal(map[string]string{"data_dir": dataDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, cfgRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	// Capture everything BuildLocal (and the skills loader's std
	// log warning) writes to stderr: swap os.Stderr for
	// BuildLocal's own logger and log.SetOutput for the std log
	// package, which the skills loader writes through.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&buf, r)
	}()
	oldStderr := os.Stderr
	os.Stderr = w
	oldLogOut := log.Writer()
	log.SetOutput(w)
	defer func() {
		_ = w.Close()
		os.Stderr = oldStderr
		log.SetOutput(oldLogOut)
		wg.Wait()
	}()

	local, err := BuildLocal(cfgPath)
	if err != nil {
		t.Fatalf("BuildLocal: %v", err)
	}
	defer func() { _ = local.Close() }()

	// Closing the pipe end flushes the reader before asserting.
	_ = w.Close()
	os.Stderr = oldStderr
	log.SetOutput(oldLogOut)
	wg.Wait()
	stderr := buf.String()

	if local.MCP == nil {
		t.Error("BuildLocal did not start the configured MCP manager")
	}
	found := false
	for _, tool := range local.Tools.List() {
		if tool.Name() == "mcp__fake__echo" {
			found = true
		}
	}
	if !found {
		t.Error("mcp__fake__echo is not registered on the Local registry")
	}
	if strings.Contains(stderr, `unknown tool "mcp__fake__echo"`) {
		t.Errorf("BuildLocal warned about the MCP tool despite wiring MCP first:\n%s", stderr)
	}
}
