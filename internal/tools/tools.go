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
	// Planning tools (todo_write + Task*) share one store. The
	// store starts in-memory; the daemon points it at the data
	// dir via Registry.PlanningStore().SetDataDir, after which
	// state persists per chat under <DataDir>/chats/<chatID>/.
	// See planning.go for the per-chat resolution.
	planningTools := NewPlanningTools(NewPlanningStore(""))
	tools := []Tool{
		&shellExecTool{sandboxCfg: sandboxCfg},
		&fileReadTool{roots: roots},
		&fileWriteTool{roots: roots},
		&fileEditTool{roots: roots},
		&fileListTool{roots: roots},
		&grepTool{roots: roots},
		&globTool{roots: roots},
		fetchTool,
		searchTool,
		&calculatorTool{},
		&datetimeTool{},
		&processListTool{},
		// ask_user_question starts with no transport: the daemon
		// installs the Telegram asker via Registry.SetQuestionAsker
		// after it builds the gateway. Local runs never install
		// one, so the tool fails cleanly there instead of hanging.
		&askUserQuestionTool{},
	}
	tools = append(tools, planningTools...)
	for _, t := range tools {
		if err := r.Register(t); err != nil {
			return nil, fmt.Errorf("tools: failed to register %q: %w", t.Name(), err)
		}
	}
	return r, nil
}
