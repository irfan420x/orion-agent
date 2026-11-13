package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the parsed <DataDir>/mcp.json. The only search path is
// the user data dir (see the package doc); there is no cwd, repo,
// or environment fallback.
type Config struct {
	// MCPServers maps server name to its launch config.
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// ServerConfig is one MCP server's launch config.
type ServerConfig struct {
	// Command is the server executable: an absolute path, or a
	// bare name resolved through PATH. A relative path with a
	// separator is refused (see resolveServerCommand).
	Command string `json:"command"`
	// Args are the command-line arguments, verbatim.
	Args []string `json:"args"`
	// Env holds extra environment variables for the server child.
	// They are the operator's explicit choice (see the package
	// doc) and overlay the scrubbed process environment.
	Env map[string]string `json:"env"`
}

// maxConfigBytes caps mcp.json: a config file is small, and an
// unbounded file is either a mistake or an attempt to smuggle data
// past review.
const maxConfigBytes = 1024 * 1024 // 1 MiB

// LoadConfig reads <dataDir>/mcp.json. A missing file means "no MCP
// servers configured" (an empty Config, nil error) — MCP is
// optional, and its absence must never fail startup. An empty
// dataDir is the same. A present but malformed file is an error the
// caller logs; no servers start from a file that cannot be parsed.
func LoadConfig(dataDir string) (*Config, error) {
	cfg := &Config{MCPServers: map[string]ServerConfig{}}
	if strings.TrimSpace(dataDir) == "" {
		return cfg, nil
	}
	path := filepath.Join(dataDir, "mcp.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("mcp: read config %q: %w", path, err)
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("mcp: config %q is %d bytes (max %d)", path, len(data), maxConfigBytes)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("mcp: parse config %q: %w", path, err)
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = map[string]ServerConfig{}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks every configured server: the name must match
// namePattern, the command must be non-empty (and, when it carries
// a path separator, absolute), and env keys must be non-empty with
// no '=' or NUL (exec would reject them later with a confusing
// error; fail here with the server named). PATH resolution of bare
// names happens at Start, not here, so a server installed after
// config load still starts.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("mcp: config is nil")
	}
	for name, srv := range c.MCPServers {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("mcp: invalid server name %q (want letters, digits, _ or -)", name)
		}
		if err := srv.Validate(); err != nil {
			return fmt.Errorf("mcp: server %q: %w", name, err)
		}
	}
	return nil
}

// Validate checks one server's launch config.
func (s ServerConfig) Validate() error {
	if strings.TrimSpace(s.Command) == "" {
		return errors.New("command must not be empty")
	}
	if strings.ContainsRune(s.Command, os.PathSeparator) && !filepath.IsAbs(s.Command) {
		return fmt.Errorf("command %q is a relative path; use an absolute path or a bare command name", s.Command)
	}
	for k := range s.Env {
		if strings.TrimSpace(k) == "" {
			return errors.New("env key must not be empty")
		}
		if strings.ContainsAny(k, "=\x00") {
			return fmt.Errorf("env key %q contains '=' or NUL", k)
		}
	}
	return nil
}
