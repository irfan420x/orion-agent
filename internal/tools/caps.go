// Package tools — central tool-result size cap.
//
// Every tool result that reaches the model passes through
// Registry.Execute, which is the single choke point for output size:
// per-tool caps (shell 64 KiB, file_read 1 MiB, web_fetch 256 KiB)
// bound memory while a tool runs, but only the registry cap bounds
// what the model actually sees, uniformly, for every tool —
// including tools added later. A result over the cap is truncated,
// never dropped and never an error, and the truncation is always
// marked with the original size so the model knows data was cut.
package tools

import (
	"fmt"
	"unicode/utf8"
)

// DefaultMaxToolResultBytes is the default per-result cap: 256 KiB.
// It mirrors config.DefaultMaxToolResultBytes (keep the two in sync):
// the config value is what NewAgent pushes into the registry, this
// value is what a registry enforces when no agent ever configured
// it. 256 KiB is the default web_fetch already used, and it keeps one
// result at ~64K estimated tokens — large enough for real file and
// command output, small enough that a single result cannot exhaust
// a provider context window on its own.
const DefaultMaxToolResultBytes = 256 * 1024

// TruncateToolResult cuts s to at most limit bytes, appending a
// "[truncated ...]" note that states how many bytes were shown and
// how many the result had in total, so the model knows data was cut
// instead of silently seeing a partial file or partial command
// output. A result at or under the limit is returned unchanged (no
// note). The cut backs off to a UTF-8 rune boundary so the result
// stays valid UTF-8. A non-positive limit means the default cap.
//
// The note itself is not counted in the limit: a truncated result is
// limit bytes of content plus one short marker line.
func TruncateToolResult(s string, limit int) string {
	if limit <= 0 {
		limit = DefaultMaxToolResultBytes
	}
	if len(s) <= limit {
		return s
	}
	cut := []byte(s[:limit])
	for len(cut) > 0 && !utf8.Valid(cut) {
		cut = cut[:len(cut)-1]
	}
	return string(cut) + fmt.Sprintf("\n[truncated: showing first %d bytes of %d total bytes]", len(cut), len(s))
}
