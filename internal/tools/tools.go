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
// config leaves sandboxing off. web_fetch uses the strict default:
// no trusted egress CIDRs.
func DefaultRegistryWithSandbox(sandboxCfg sandbox.Config) (*Registry, error) {
	return DefaultRegistryWithSandboxAndTrustedEgress(sandboxCfg, nil)
}

// DefaultRegistryWithSandboxAndTrustedEgress is
// DefaultRegistryWithSandbox with the deployment's trusted egress
// CIDRs plumbed into web_fetch (config trusted_egress_cidrs).
// A malformed CIDR fails registry construction loudly.
func DefaultRegistryWithSandboxAndTrustedEgress(sandboxCfg sandbox.Config, trustedEgressCIDRs []string) (*Registry, error) {
	fetchTool, err := newWebFetchTool(trustedEgressCIDRs)
	if err != nil {
		return nil, err
	}
	searchTool, err := newWebSearchTool(trustedEgressCIDRs)
	if err != nil {
		return nil, err
	}
	r := NewRegistry()
	roots := defaultFileRoots()
	tools := []Tool{
		&shellExecTool{sandboxCfg: sandboxCfg},
		&fileReadTool{roots: roots},
		&fileWriteTool{roots: roots},
		&fileEditTool{roots: roots},
		&fileListTool{roots: roots},
		fetchTool,
		searchTool,
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
