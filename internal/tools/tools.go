// Package tools — DefaultRegistry wires every built-in tool into a registry.
package tools

import (
	"time"

	"github.com/irfan/orion/internal/sandbox"
)

// timeoutSecondsToDuration converts a shell timeout in seconds to a duration.
func timeoutSecondsToDuration(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}

// DefaultRegistry constructs a registry with all built-in tools registered.
// It returns an error if any registration fails.
//
// The sandbox is OFF in the registry this returns: it is a
// test/unconfigured constructor only. Production must use
// DefaultRegistryWithSandbox(cfg.Sandbox) with the config-loaded
// sandbox (default ON since rc8, see internal/config) — calling
// this from production code would silently run shell_exec
// unsandboxed.
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
// The tools come from the factories (see framework.go): there is
// no literal tool list here to edit or forget.
func DefaultRegistryWithSandboxAndTrustedEgress(sandboxCfg sandbox.Config, trustedEgressCIDRs []string) (*Registry, error) {
	return DefaultRegistryWithDeps(ToolDeps{
		SandboxCfg:         sandboxCfg,
		TrustedEgressCIDRs: trustedEgressCIDRs,
	})
}
