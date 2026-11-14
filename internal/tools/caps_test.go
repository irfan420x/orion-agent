package tools

// Caps tests: the central tool-result size cap (Registry.Execute),
// the per-tool truncation markers, and multi-megabyte outputs.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
)

// --- TruncateToolResult ---

func TestTruncateToolResultUnderLimitUnchanged(t *testing.T) {
	if got := TruncateToolResult("short", 100); got != "short" {
		t.Fatalf("under-limit result changed: %q", got)
	}
	if got := TruncateToolResult(strings.Repeat("x", 100), 100); got != strings.Repeat("x", 100) {
		t.Fatalf("at-limit result changed (len %d)", len(got))
	}
}

func TestTruncateToolResultMarksOriginalSize(t *testing.T) {
	out := TruncateToolResult(strings.Repeat("y", 5000), 1024)
	if !strings.Contains(out, "[truncated: showing first 1024 bytes of 5000 total bytes]") {
		t.Fatalf("missing size marker in %q", out[len(out)-100:])
	}
	if len(out) > 1024+100 {
		t.Fatalf("truncated result too long: %d bytes", len(out))
	}
	if !strings.HasPrefix(out, strings.Repeat("y", 1024)) {
		t.Fatal("truncated result does not keep the head of the original")
	}
}

func TestTruncateToolResultDoesNotSplitUTF8(t *testing.T) {
	// Cut lands inside a multi-byte rune: the result must stay valid UTF-8.
	s := strings.Repeat("a", 1023) + "é" + strings.Repeat("b", 100)
	out := TruncateToolResult(s, 1024)
	if !utf8.ValidString(out) {
		t.Fatal("truncated result is not valid UTF-8")
	}
	if !strings.HasPrefix(out, strings.Repeat("a", 1023)) {
		t.Fatal("truncated result should keep the 1023 ASCII bytes before the rune")
	}
}

func TestTruncateToolResultNonPositiveLimitMeansDefault(t *testing.T) {
	out := TruncateToolResult(strings.Repeat("z", DefaultMaxToolResultBytes+10), 0)
	if !strings.Contains(out, fmt.Sprintf("[truncated: showing first %d bytes of %d total bytes]", DefaultMaxToolResultBytes, DefaultMaxToolResultBytes+10)) {
		t.Fatalf("zero limit did not use the default cap: %q", out[len(out)-100:])
	}
}

// --- Registry central cap ---

// hugeTool returns whatever payload the test sets, unbounded: it
// stands in for any tool (built-in or added later) that produces
// far more output than the cap.
type hugeTool struct {
	payload string
	errText string
}

func (h hugeTool) Name() string        { return "huge" }
func (h hugeTool) Description() string { return "Returns a huge payload for cap tests." }

// RiskClass declares the tool Safe (Register requires a class
// matching permissions.Classify; "huge" is not in the gate's maps).
func (h hugeTool) RiskClass() permissions.Class { return permissions.ClassSafe }
func (h hugeTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: "huge", Description: h.Description(), Parameters: map[string]any{"type": "object"}}
}
func (h hugeTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	if h.errText != "" {
		return "", fmt.Errorf("%s", h.errText)
	}
	return h.payload, nil
}

func TestRegistryCapsHugeResult(t *testing.T) {
	reg := NewRegistry()
	payload := strings.Repeat("q", 2*1024*1024) // 2 MiB
	if err := reg.Register(hugeTool{payload: payload}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	out, err := reg.Execute(context.Background(), "huge", json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(out) > DefaultMaxToolResultBytes+100 {
		t.Fatalf("result not capped: %d bytes", len(out))
	}
	if !strings.Contains(out, fmt.Sprintf("[truncated: showing first %d bytes of %d total bytes]", DefaultMaxToolResultBytes, len(payload))) {
		t.Fatalf("missing size marker: tail %q", out[len(out)-120:])
	}
}

func TestRegistryCapsHugeErrorText(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(hugeTool{errText: strings.Repeat("e", 2*1024*1024)}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err := reg.Execute(context.Background(), "huge", json.RawMessage("{}"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(err.Error()) > DefaultMaxToolResultBytes+200 {
		t.Fatalf("error text not capped: %d bytes", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "[truncated:") {
		t.Fatalf("capped error text has no marker: tail %q", err.Error()[len(err.Error())-120:])
	}
}

func TestRegistrySetMaxResultBytes(t *testing.T) {
	reg := NewRegistry()
	if got := reg.MaxResultBytes(); got != DefaultMaxToolResultBytes {
		t.Fatalf("default MaxResultBytes = %d, want %d", got, DefaultMaxToolResultBytes)
	}
	reg.SetMaxResultBytes(4096)
	if got := reg.MaxResultBytes(); got != 4096 {
		t.Fatalf("MaxResultBytes = %d, want 4096", got)
	}
	if err := reg.Register(hugeTool{payload: strings.Repeat("q", 100000)}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	out, err := reg.Execute(context.Background(), "huge", json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "[truncated: showing first 4096 bytes of 100000 total bytes]") {
		t.Fatalf("custom cap not honoured: tail %q", out[len(out)-120:])
	}
	// Non-positive restores the default.
	reg.SetMaxResultBytes(0)
	if got := reg.MaxResultBytes(); got != DefaultMaxToolResultBytes {
		t.Fatalf("SetMaxResultBytes(0) = %d, want default %d", got, DefaultMaxToolResultBytes)
	}
}

// --- Multi-megabyte tool outputs ---

func TestFileReadMultiMBTruncates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	const size = 3 * 1024 * 1024
	if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o600); err != nil {
		t.Fatalf("write big file: %v", err)
	}
	// Tool level: first 1 MiB plus a marker with the true size.
	tool := &fileReadTool{roots: []string{dir}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("file_read of a 3 MiB file should truncate, not fail: %v", err)
	}
	marker := fmt.Sprintf("\n[truncated to 1MB] (file is %d bytes)", size)
	if !strings.HasSuffix(out, marker) {
		t.Fatalf("missing size marker: tail %q", out[len(out)-100:])
	}
	if len(out) != maxFileRead+len(marker) {
		t.Fatalf("file_read output len = %d, want %d", len(out), maxFileRead+len(marker))
	}
	// Registry level (the central choke point): the same read
	// reaches the model capped at the default 256 KiB, with the
	// registry's own marker carrying the size it saw.
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	regOut, err := reg.Execute(context.Background(), "file_read", mustJSON(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("registry file_read: %v", err)
	}
	if len(regOut) > DefaultMaxToolResultBytes+100 {
		t.Fatalf("registry result not capped: %d bytes", len(regOut))
	}
	wantMarker := fmt.Sprintf("[truncated: showing first %d bytes of %d total bytes]", DefaultMaxToolResultBytes, maxFileRead+len(marker))
	if !strings.Contains(regOut, wantMarker) {
		t.Fatalf("missing registry marker: tail %q", regOut[len(regOut)-140:])
	}
}

func TestShellExecMultiMBTruncatesAndReportsTotal(t *testing.T) {
	tool := &shellExecTool{}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "yes x | head -c 3000000"}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "[truncated]") {
		t.Fatalf("expected [truncated] marker, output len %d", len(out))
	}
	// The writer counts discarded bytes, so the marker reports the
	// true output size (3,000,000), not just the cap.
	if !strings.Contains(out, "total output was 3000000 bytes") {
		t.Fatalf("marker should report the true total size: tail %q", out[len(out)-120:])
	}
	if len(out) > maxShellOutput+512 {
		t.Fatalf("output not capped: %d bytes", len(out))
	}
}
