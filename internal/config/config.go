// Package config loads Orion configuration from JSON, with environment
// overrides. Provider selection lives here and nowhere else.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/irfan/orion/internal/sandbox"
)

// Caps defaults: the size limits on tool results and conversation
// history. These are the policy values; the enforcement lives in
// internal/tools (Registry.Execute) and internal/core (trimHistory).
//
// DefaultMaxToolResultBytes mirrors tools.DefaultMaxToolResultBytes:
// keep the two in sync — the config value is what NewAgent pushes
// into the registry, the tools value is what a registry enforces
// when no agent ever configured it (direct registry use, tests).
// 256 KiB: the same default web_fetch already used, so one tool
// result costs at most ~64K estimated tokens and can never by
// itself exhaust a provider context window.
//
// DefaultMaxHistoryTokens is the token budget for the history fed to
// the provider on each step. 32K: DeepSeek (the default provider)
// offers a 64K window, so 32K of history leaves headroom for the
// tool definitions, the completion, and the ~4 chars/token estimate
// being rough (CountTokens under-counts dense text). Local Ollama
// models with smaller windows should set this lower in config.
const (
	DefaultMaxToolResultBytes = 256 * 1024
	DefaultMaxHistoryTokens   = 32768
)

const (
	minMaxToolResultBytes = 1024
	maxMaxToolResultBytes = 10 * 1024 * 1024 // matches web_fetch's max_bytes ceiling
	minMaxHistoryTokens   = 1024             // system prompt alone is ~600 tokens
	maxMaxHistoryTokens   = 1000000
)

// knownProviders is the set of provider names the model layer
// supports. Config and model must agree here; a name outside this set
// can never produce a working provider. It is unexported (and only
// ever read through KnownProvider/KnownProviderNames) so no other
// package can mutate the provider set.
//
// KnownProvider reports whether name (case-insensitive) is a
// supported provider.
func KnownProvider(name string) bool {
	return knownProviders[strings.ToLower(strings.TrimSpace(name))]
}

// KnownProviderNames returns the supported provider names, sorted, as
// a fresh slice: mutating the result cannot affect the provider set.
func KnownProviderNames() []string {
	names := make([]string, 0, len(knownProviders))
	for name := range knownProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

var knownProviders = map[string]bool{
	"openai":    true,
	"deepseek":  true,
	"anthropic": true,
	"ollama":    true,
}

// providerNeedsAPIKey reports whether the named provider refuses to
// run without an API key. Ollama is local and needs none.
func providerNeedsAPIKey(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "openai", "deepseek", "anthropic":
		return true
	default:
		return false
	}
}

// telegramTokenPattern matches a Telegram bot token: a numeric bot id,
// a colon, and a secret of 20-64 characters. The secret length is a
// range, not an exact count: Telegram's token shape has changed before
// and may change again, so an exact length would reject future valid
// tokens. Tokens shorter than 20 or longer than 64 characters, or with
// characters outside the token alphabet, can never authenticate.
var telegramTokenPattern = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]{20,64}$`)

// ProviderConfig mirrors model.ProviderConfig to avoid a config -> model
// import cycle in the other direction (model stays dependency-free).
// config.ProviderConfig and model.ProviderConfig have identical fields;
// convert explicitly at the boundary in core.
type ProviderConfig struct {
	Name    string `json:"name"`
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
}

// Config is the whole Orion configuration.
type Config struct {
	DefaultProvider string                    `json:"default_provider"`
	Providers       map[string]ProviderConfig `json:"providers"`
	Model           string                    `json:"model"`
	TelegramToken   string                    `json:"telegram_token"`
	// TelegramAllowedChats is the allowlist of Telegram chat IDs that
	// may drive the agent. When non-empty, the gateway drops messages
	// from any other chat before the handler runs: without it, anyone
	// who discovers the bot username could run the OS agent (including
	// shell_exec). When empty, all chats are accepted (first-run
	// convenience); the daemon logs a warning in that case.
	TelegramAllowedChats []int64 `json:"telegram_allowed_chats"`
	DataDir              string  `json:"data_dir"`
	MaxSteps             int     `json:"max_steps"`
	TimeoutSeconds       int     `json:"timeout_seconds"`
	// MaxToolResultBytes caps the bytes of any single tool result
	// that reach the model (enforced centrally in the tools
	// registry; see TruncateToolResult). Results over the cap are
	// truncated with a "[truncated ...]" note carrying the original
	// size, never dropped and never an error.
	MaxToolResultBytes int `json:"max_tool_result_bytes"`
	// MaxHistoryTokens caps the estimated tokens of the conversation
	// history fed to the provider on each step (system prompt +
	// recent history + recall injection + in-turn messages).
	// Over-budget history is trimmed oldest-first in the agent loop
	// (never the system prompt, never the latest user turn, never
	// breaking a tool-call/tool-result pair).
	MaxHistoryTokens int `json:"max_history_tokens"`
	// Temperature, when set, is passed to the provider on every call.
	// Nil means the provider default. 0 requests deterministic output
	// (used by the live-model E2E test); the valid range is 0-2.
	Temperature *float64 `json:"temperature,omitempty"`

	// TelegramFooter, when set, is the default for the per-chat
	// runtime footer on final Telegram replies (see
	// internal/messaging.FormatFooter and the /footer command). Nil
	// means ON (the footer is shown unless a chat turns it off with
	// /footer off). A per-chat override in the session store wins
	// over this default.
	TelegramFooter *bool `json:"telegram_footer,omitempty"`

	// Sandbox configures sandbox mode for shell_exec (user+mount
	// namespace chroot; see internal/sandbox). Disabled by default:
	// when off, shell_exec behaves exactly as before, protected
	// only by the IsBlockedCommand denylist. When on and the
	// mechanism is available, commands run confined to
	// Sandbox.Root/work; when on but unavailable, shell_exec falls
	// back to the denylist alone and logs a warning.
	Sandbox sandbox.Config `json:"sandbox"`

	// fileSecrets snapshots the secrets as loaded from the config
	// file, before environment overrides are applied. envSecrets
	// records the values the environment supplied. Save restores the
	// file values for any secret that still holds its env-supplied
	// value, so an env-supplied API key or Telegram token is never
	// written to disk. A secret changed after load (e.g. by
	// `orion config set-api-key`) no longer matches its env value
	// and is persisted normally.
	fileAPIKeys    map[string]string
	fileTelegram   string
	envAPIKeys     map[string]string
	envTelegram    string
	envTelegramSet bool
}

// defaultConfig builds the default configuration with the given data
// directory (which may be empty when no home directory is known; see
// Default).
func defaultConfig(dataDir string) *Config {
	cfg := &Config{
		DefaultProvider: "ollama",
		Providers: map[string]ProviderConfig{
			"openai":    {Name: "openai", Model: "gpt-4o-mini"},
			"deepseek":  {Name: "deepseek", BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat"},
			"anthropic": {Name: "anthropic", Model: "claude-sonnet-4-5"},
			"ollama":    {Name: "ollama", BaseURL: "http://localhost:11434", Model: "llama3.1"},
		},
		Model:              "",
		DataDir:            dataDir,
		MaxSteps:           20,
		TimeoutSeconds:     120,
		MaxToolResultBytes: DefaultMaxToolResultBytes,
		MaxHistoryTokens:   DefaultMaxHistoryTokens,
	}
	if dataDir != "" {
		cfg.Sandbox = sandbox.Config{Root: sandbox.DefaultRoot(dataDir)}
	}
	return cfg
}

// DefaultE returns a safe, local-first configuration. It fails with a
// clear error when the home directory cannot be determined, instead of
// silently falling back to a relative ".orion" data directory (which
// would put config and memory in whatever the working directory
// happens to be). Prefer DefaultE (and Load, which uses it) over
// Default whenever an error can be returned.
func DefaultE() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("config: cannot determine home directory: %w", err)
	}
	return defaultConfig(filepath.Join(home, ".orion")), nil
}

// Default returns a safe, local-first configuration. It cannot return
// an error, so when the home directory cannot be determined it returns
// the defaults with an empty DataDir: Validate then rejects the config
// ("data_dir must not be empty") instead of the config silently
// pointing at a relative ".orion" directory. Callers that can handle
// an error should use DefaultE or Load, which surface the home-directory
// error at the source.
func Default() *Config {
	cfg, err := DefaultE()
	if err != nil {
		return defaultConfig("")
	}
	return cfg
}

// DefaultPath returns the default config file path
// (~/.orion/config.json). It fails with a clear error when the home
// directory cannot be determined, instead of building a relative path
// that would read or write config.json in the working directory.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".orion", "config.json"), nil
}

// Validate checks the config is usable. It rejects every
// misconfiguration that would otherwise fail later, at runtime, with a
// confusing error: bad numeric bounds, a bad or missing data dir, an
// unknown provider, a provider entry that disagrees with its key, a
// bad base URL, a default provider with no model, a key-requiring
// provider with no API key, and a malformed Telegram token. Only the
// default (active) provider is checked for a model and an API key: the
// other entries are spares and may legitimately be key-less until used.
func (c *Config) Validate() error {
	if c.MaxSteps < 1 || c.MaxSteps > 200 {
		return errors.New("config: max_steps must be between 1 and 200")
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > 3600 {
		return errors.New("config: timeout_seconds must be between 1 and 3600")
	}
	// Absurd caps are rejected rather than silently clamped: a
	// 1-byte tool cap would truncate every result into uselessness,
	// and a 1 GB cap (or a 1-token history) would defeat the limits
	// entirely. The bounds are generous — 1 KiB to 10 MiB per tool
	// result, 1K to 1M history tokens — so any sane tuning passes.
	if c.MaxToolResultBytes < minMaxToolResultBytes || c.MaxToolResultBytes > maxMaxToolResultBytes {
		return fmt.Errorf("config: max_tool_result_bytes must be between %d and %d", minMaxToolResultBytes, maxMaxToolResultBytes)
	}
	if c.MaxHistoryTokens < minMaxHistoryTokens || c.MaxHistoryTokens > maxMaxHistoryTokens {
		return fmt.Errorf("config: max_history_tokens must be between %d and %d", minMaxHistoryTokens, maxMaxHistoryTokens)
	}
	if c.Temperature != nil && (*c.Temperature < 0 || *c.Temperature > 2) {
		return errors.New("config: temperature must be between 0 and 2")
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return errors.New("config: data_dir must not be empty")
	}
	if !filepath.IsAbs(c.DataDir) {
		return fmt.Errorf("config: data_dir %q must be an absolute path", c.DataDir)
	}
	if fi, err := os.Stat(c.DataDir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("config: data_dir %q exists and is not a directory", c.DataDir)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("config: data_dir %q: %w", c.DataDir, err)
	}
	if strings.TrimSpace(c.DefaultProvider) == "" {
		return errors.New("config: default_provider must not be empty")
	}
	defaultName := strings.ToLower(strings.TrimSpace(c.DefaultProvider))
	if !KnownProvider(defaultName) {
		return fmt.Errorf("config: unknown provider %q (supported providers: %s)", c.DefaultProvider, strings.Join(KnownProviderNames(), ", "))
	}
	// Every configured provider entry must name a known provider, agree
	// with its map key, and carry a usable base URL when one is set.
	for key, p := range c.Providers {
		entryName := strings.ToLower(strings.TrimSpace(key))
		if !KnownProvider(entryName) {
			return fmt.Errorf("config: unknown provider %q in providers (supported providers: %s)", key, strings.Join(KnownProviderNames(), ", "))
		}
		if strings.TrimSpace(p.Name) != "" && !strings.EqualFold(strings.TrimSpace(p.Name), key) {
			return fmt.Errorf("config: provider %q has mismatched name %q (name must match its key)", key, p.Name)
		}
		if strings.TrimSpace(p.BaseURL) != "" {
			if err := validateBaseURL(p.BaseURL); err != nil {
				return fmt.Errorf("config: provider %q: %w", key, err)
			}
		}
	}
	active, ok := c.providerEntry(c.DefaultProvider)
	if !ok {
		return fmt.Errorf("config: default_provider %q is not in providers", c.DefaultProvider)
	}
	// Effective model: the provider's own model wins, else the global
	// model. One of them must be set or the provider has nothing to run.
	model := strings.TrimSpace(active.Model)
	if model == "" {
		model = strings.TrimSpace(c.Model)
	}
	if model == "" {
		return fmt.Errorf("config: provider %q has no model configured (set providers.%s.model or the top-level model)", c.DefaultProvider, defaultName)
	}
	if providerNeedsAPIKey(defaultName) && strings.TrimSpace(active.APIKey) == "" {
		return fmt.Errorf("config: provider %q requires an API key (set providers.%s.api_key or ORION_API_KEY)", c.DefaultProvider, defaultName)
	}
	if strings.TrimSpace(c.TelegramToken) != "" && !telegramTokenPattern.MatchString(strings.TrimSpace(c.TelegramToken)) {
		return errors.New("config: telegram_token is not a valid Telegram bot token (expected <numeric bot id>:<20-64 characters>)")
	}
	// Telegram chat IDs are non-zero: positive for private chats,
	// negative for groups. Zero can never be a real chat.
	for _, id := range c.TelegramAllowedChats {
		if id == 0 {
			return errors.New("config: telegram_allowed_chats must not contain 0 (not a valid chat id)")
		}
	}
	if err := c.Sandbox.Validate(); err != nil {
		return err
	}
	return nil
}

// validateBaseURL checks a provider base URL is an http(s) URL with a host.
func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("base_url %q is not a valid URL: %v", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("base_url %q must use http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("base_url %q must include a host", raw)
	}
	return nil
}

// providerEntry finds a provider entry by name, case-insensitively, so
// "OpenAI" as default_provider still matches a configured "openai".
func (c *Config) providerEntry(name string) (ProviderConfig, bool) {
	for key, p := range c.Providers {
		if strings.EqualFold(key, name) {
			return p, true
		}
	}
	return ProviderConfig{}, false
}

// Load reads a JSON config file. A missing file returns Default().
// Environment overrides (highest priority): ORION_PROVIDER,
// ORION_API_KEY, ORION_MODEL, ORION_TELEGRAM_TOKEN, ORION_DATA_DIR,
// ORION_SANDBOX (on/off), ORION_SANDBOX_ROOT.
// Load fails with a clear error when the home directory cannot be
// determined (the defaults are home-relative), instead of silently
// using a relative data directory.
func Load(path string) (*Config, error) {
	cfg, err := DefaultE()
	if err != nil {
		return nil, err
	}
	if path == "" {
		path, err = DefaultPath()
		if err != nil {
			return nil, err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg.snapshotFileSecrets()
			cfg.applyEnv()
			return cfg, cfg.Validate()
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	// Merge file entries over the defaults field-by-field (P1-8):
	// json.Unmarshal replaces a provider entry wholesale, so a file
	// that only sets providers.openai.api_key would otherwise zero
	// that entry's default model and base URL. Empty fields are
	// filled from the corresponding default entry; entries for
	// providers missing from the file entirely are restored from the
	// defaults; an entry for a genuinely new (unknown) name is left
	// alone so Validate can reject it.
	cfg.mergeProviderDefaults()
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderConfig{}
	}
	cfg.snapshotFileSecrets()
	cfg.applyEnv()
	return cfg, cfg.Validate()
}

// mergeProviderDefaults fills empty fields of each configured provider
// entry from the matching default entry, and restores any known
// provider the file did not mention at all.
func (c *Config) mergeProviderDefaults() {
	defaults := defaultConfig("").Providers
	if c.Providers == nil {
		c.Providers = map[string]ProviderConfig{}
	}
	for key, p := range c.Providers {
		def, ok := defaults[strings.ToLower(strings.TrimSpace(key))]
		if !ok {
			continue
		}
		if strings.TrimSpace(p.Name) == "" {
			p.Name = def.Name
		}
		if p.Model == "" {
			p.Model = def.Model
		}
		if p.BaseURL == "" {
			p.BaseURL = def.BaseURL
		}
		if p.APIKey == "" {
			p.APIKey = def.APIKey
		}
		c.Providers[key] = p
	}
	for name, def := range defaults {
		if _, ok := c.canonicalProviderKey(name); !ok {
			c.Providers[name] = def
		}
	}
}

// canonicalProviderKey returns the map key for a provider name,
// matched case-insensitively, so "OpenAI" finds the "openai" entry
// instead of creating a second, half-filled entry under a new key.
func (c *Config) canonicalProviderKey(name string) (string, bool) {
	for key := range c.Providers {
		if strings.EqualFold(key, name) {
			return key, true
		}
	}
	return "", false
}

// snapshotFileSecrets records the secrets as loaded from the file, so
// Save can tell an env-supplied secret (never persisted) from a
// deliberately stored one.
func (c *Config) snapshotFileSecrets() {
	c.fileAPIKeys = map[string]string{}
	for name, p := range c.Providers {
		c.fileAPIKeys[name] = p.APIKey
	}
	c.fileTelegram = c.TelegramToken
}

func (c *Config) applyEnv() {
	if v := os.Getenv("ORION_PROVIDER"); v != "" {
		c.DefaultProvider = v
	}
	if v := os.Getenv("ORION_API_KEY"); v != "" {
		// Resolve the canonical map key case-insensitively: storing
		// under the raw DefaultProvider ("OpenAI") would create a
		// second entry alongside the default "openai" one.
		key, ok := c.canonicalProviderKey(c.DefaultProvider)
		if !ok {
			key = c.DefaultProvider
		}
		p := c.Providers[key]
		p.APIKey = v
		c.Providers[key] = p
		if c.envAPIKeys == nil {
			c.envAPIKeys = map[string]string{}
		}
		c.envAPIKeys[key] = v
	}
	if v := os.Getenv("ORION_MODEL"); v != "" {
		c.Model = v
	}
	if v := os.Getenv("ORION_TELEGRAM_TOKEN"); v != "" {
		c.TelegramToken = v
		c.envTelegram = v
		c.envTelegramSet = true
	}
	if v := os.Getenv("ORION_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("ORION_SANDBOX"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "on", "true", "1", "yes":
			c.Sandbox.Enabled = true
		case "off", "false", "0", "no":
			c.Sandbox.Enabled = false
		}
	}
	if v := os.Getenv("ORION_SANDBOX_ROOT"); v != "" {
		c.Sandbox.Root = v
	}
}

// Save writes the config as indented JSON, creating parent dirs.
// Secrets that still hold an environment-supplied value are restored
// to their file values first, so `Save` never persists an env-supplied
// API key or Telegram token to disk.
func (c *Config) Save(path string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if path == "" {
		path = filepath.Join(c.DataDir, "config.json")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config: mkdir: %w", err)
	}
	out := *c
	if len(c.envAPIKeys) > 0 {
		providers := make(map[string]ProviderConfig, len(c.Providers))
		for name, p := range c.Providers {
			if envVal, ok := c.envAPIKeys[name]; ok && p.APIKey == envVal {
				p.APIKey = c.fileAPIKeys[name]
			}
			providers[name] = p
		}
		out.Providers = providers
	}
	if c.envTelegramSet && c.TelegramToken == c.envTelegram {
		out.TelegramToken = c.fileTelegram
	}
	data, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	// Atomic write, like memory's FileStore: temp file in the same
	// directory (0600), fsync, rename over the destination, fsync
	// the directory. A plain os.WriteFile can leave a torn
	// config.json behind on a crash mid-write (Load then fails to
	// parse and Orion will not start), and it never changes the
	// mode of a pre-existing file — a config.json created with
	// 0644 would stay world-readable, leaking the API keys and
	// Telegram token it holds. The rename replaces any such file
	// with a 0600 one.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("config: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on any failure path; after a successful
	// rename the temp path no longer exists and Remove is a no-op.
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("config: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("config: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("config: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("config: rename temp file to %q: %w", path, err)
	}
	if err := syncConfigDir(dir); err != nil {
		return fmt.Errorf("config: sync data dir %q: %w", dir, err)
	}
	return nil
}

// syncConfigDir fsyncs a directory so renames into it survive a crash.
func syncConfigDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ActiveProvider returns the configured default provider entry and the
// effective model name (per-provider model wins, else Config.Model).
// It fills in an empty entry name from the default provider and
// rejects a provider with no effective model, so callers never hand
// the model layer a provider it cannot run.
func (c *Config) ActiveProvider() (ProviderConfig, error) {
	p, ok := c.providerEntry(c.DefaultProvider)
	if !ok {
		return ProviderConfig{}, fmt.Errorf("config: provider %q not configured", c.DefaultProvider)
	}
	if strings.TrimSpace(p.Name) == "" {
		p.Name = c.DefaultProvider
	}
	// Effective model: the provider's own model wins, else the global
	// model — the same rule Validate uses, so a config that validates
	// runs the model Validate checked.
	if strings.TrimSpace(p.Model) == "" {
		p.Model = c.Model
	}
	if strings.TrimSpace(p.Model) == "" {
		return ProviderConfig{}, fmt.Errorf("config: provider %q has no model configured (set providers.%s.model or the top-level model)", c.DefaultProvider, strings.ToLower(strings.TrimSpace(c.DefaultProvider)))
	}
	return p, nil
}
