// Package tools — DefaultRegistry wires every built-in tool into a registry.
package tools

import (
	"fmt"
	"time"

	"github.com/irfan/orion/internal/sandbox"
)

// timeoutSecondsToDuration converts a shell timeout in seconds to a duration.
func timeoutSecondsToDuration(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}

// DefaultRegistry constructs a registry with all built-in tools registered.
// It returns an error if any registration fails.
func DefaultRegistry() (*Registry, error) {
	return DefaultRegistryWithSandbox(sandbox.Config{})
}

// DefaultRegistryWithSandbox is DefaultRegistry with sandbox mode
// configured for shell_exec (from config: cfg.Sandbox). The zero
// config leaves sandboxing off.
func DefaultRegistryWithSandbox(sandboxCfg sandbox.Config) (*Registry, error) {
	r := NewRegistry()
	roots := defaultFileRoots()
	tools := []Tool{
		&shellExecTool{sandboxCfg: sandboxCfg},
		&fileReadTool{roots: roots},
		&fileWriteTool{roots: roots},
		&fileEditTool{roots: roots},
		&fileListTool{roots: roots},
		&webFetchTool{},
		&webSearchTool{},
		&calculatorTool{},
		&datetimeTool{},
		&processListTool{},
	}
	for _, t := range tools {
		if err := r.Register(t); err != nil {
			return nil, fmt.Errorf("tools: failed to register %q: %w", t.Name(), err)
		}
	}
	return r, nil
}
