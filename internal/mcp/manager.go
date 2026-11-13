package mcp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"

	"github.com/irfan/orion/internal/tools"
)

// Manager owns the running MCP servers for one Orion app: it loads
// the config, starts every configured server, lists their tools,
// and hands the resulting tools.Tool values to the caller for
// registration. A server that fails to start or list never fails
// the manager (or the daemon): its error is recorded in StartErrors
// for the caller to log, and the remaining servers still start.
// Create it with StartManager; Close it at shutdown to kill the
// server processes.
type Manager struct {
	mu        sync.RWMutex
	dataDir   string
	clients   map[string]*Client
	toolsList []tools.Tool
	startErrs map[string]error
	logger    *log.Logger
}

// StartManager loads <dataDir>/mcp.json and starts every configured
// server. A missing config file means "no MCP configured": it
// returns (nil, nil), and the caller wires nothing. A malformed
// config file is an error (nil manager): no server starts from a
// file that cannot be parsed. Individual server failures are NOT
// returned as an error — they land in StartErrors, and the manager
// still holds the servers that did start (possibly none, in which
// case Tools is empty and the manager is still usable and closable).
func StartManager(dataDir string, logger *log.Logger) (*Manager, error) {
	cfg, err := LoadConfig(dataDir)
	if err != nil {
		return nil, err
	}
	if len(cfg.MCPServers) == 0 {
		return nil, nil
	}
	if logger == nil {
		logger = log.New(os.Stderr, "orion/mcp: ", log.LstdFlags)
	}
	m := &Manager{
		dataDir:   dataDir,
		clients:   map[string]*Client{},
		startErrs: map[string]error{},
		logger:    logger,
	}
	names := make([]string, 0, len(cfg.MCPServers))
	for name := range cfg.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		m.startOne(name, cfg.MCPServers[name])
	}
	return m, nil
}

// startOne starts one server, lists its tools, and registers the
// resulting tools with the manager. Any failure is recorded in
// startErrs (and logged) and the server is left out — it never
// aborts the other servers.
func (m *Manager) startOne(name string, cfg ServerConfig) {
	ctx := context.Background()
	client := NewClient(name, cfg, m.dataDir)
	client.SetLogger(m.logger)
	if err := client.Start(ctx); err != nil {
		m.recordError(name, err)
		return
	}
	infos, err := client.ListTools(ctx)
	if err != nil {
		m.recordError(name, err)
		_ = client.Close()
		return
	}
	m.mu.Lock()
	m.clients[name] = client
	for _, info := range infos {
		tool := newTool(client, info)
		dup := false
		for _, existing := range m.toolsList {
			if existing.Name() == tool.Name() {
				dup = true
				break
			}
		}
		if dup {
			m.recordErrorLocked(name, fmt.Errorf("tool %q already registered; skipping", tool.Name()))
			continue
		}
		m.toolsList = append(m.toolsList, tool)
	}
	m.mu.Unlock()
	m.logger.Printf("mcp: server %q started with %d tool(s)", name, len(infos))
}

// recordError stores a per-server start error (and logs it).
func (m *Manager) recordError(name string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordErrorLocked(name, err)
}

// recordErrorLocked is recordError with m.mu already held.
func (m *Manager) recordErrorLocked(name string, err error) {
	if m.startErrs == nil {
		m.startErrs = map[string]error{}
	}
	m.startErrs[name] = err
	m.logger.Printf("mcp: server %q failed: %v", name, err)
}

// Tools returns the discovered MCP tools (mcp__<server>__<tool>),
// in server-then-tool order. The returned slice is a copy.
func (m *Manager) Tools() []tools.Tool {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]tools.Tool(nil), m.toolsList...)
}

// StartErrors returns the per-server errors recorded while
// starting (server name -> error), as a copy. Empty when every
// configured server started.
func (m *Manager) StartErrors() map[string]error {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]error, len(m.startErrs))
	for k, v := range m.startErrs {
		out[k] = v
	}
	return out
}

// Client returns the running client for a server, or nil when the
// server is not running (never configured, failed to start, or
// closed).
func (m *Manager) Client(name string) *Client {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clients[name]
}

// Close terminates every running server. It is safe to call more
// than once, and on a nil manager (a no-op). Errors from individual
// servers are joined and returned; a failing server never stops the
// rest from being closed.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.clients = map[string]*Client{}
	m.mu.Unlock()
	var errs []error
	for _, c := range clients {
		if err := c.Close(); err != nil {
			errs = append(errs, fmt.Errorf("mcp: close server %q: %w", c.Name(), err))
		}
	}
	return errors.Join(errs...)
}
