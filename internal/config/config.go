// Package config loads and saves accountant's TOML configuration file.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Account is one registered subscription. It holds metadata only; the
// credential itself lives in the vault under CredentialRef.
type Account struct {
	ID            string `toml:"id"`
	Provider      string `toml:"provider"`
	Label         string `toml:"label"`
	Priority      int    `toml:"priority"`
	Enabled       *bool  `toml:"enabled"` // nil means enabled
	CredentialRef string `toml:"credential_ref"`
	MintedAt      string `toml:"minted_at"` // YYYY-MM-DD
}

// IsEnabled reports whether the account is enabled (default: yes).
func (a Account) IsEnabled() bool { return a.Enabled == nil || *a.Enabled }

// Client is an API consumer key record. Only the SHA-256 hash of the key is
// stored; the key itself is shown once at creation.
type Client struct {
	ID        string `toml:"id"`
	KeySHA256 string `toml:"key_sha256"`
	// ClaudeCodeCompat marks a client that is not Claude Code: its
	// requests are reshaped so upstream accepts subscription
	// credentials (see anthropic.ShapeClaudeCodeCompat).
	ClaudeCodeCompat bool `toml:"claude_code_compat,omitempty"`
}

// Config is the root of config.toml.
type Config struct {
	Listen                       string    `toml:"listen"`
	DefaultProvider              string    `toml:"default_provider"`
	RequestBodyLimitMiB          int64     `toml:"request_body_limit_mib"`
	ResponseHeaderTimeoutSeconds int       `toml:"response_header_timeout_seconds"`
	UtilizationScale             string    `toml:"utilization_scale"`
	Accounts                     []Account `toml:"accounts"`
	Clients                      []Client  `toml:"clients"`
}

// Defaults applied when fields are absent.
const (
	DefaultListen            = "127.0.0.1:8787"
	DefaultProvider          = "anthropic"
	DefaultBodyLimitMiB      = 32
	DefaultRespHeaderTimeout = 120
	DefaultUtilizationScale  = "fraction"
)

// Normalize fills defaults and validates the config.
func (c *Config) Normalize() error {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.DefaultProvider == "" {
		c.DefaultProvider = DefaultProvider
	}
	if c.RequestBodyLimitMiB <= 0 {
		c.RequestBodyLimitMiB = DefaultBodyLimitMiB
	}
	if c.ResponseHeaderTimeoutSeconds <= 0 {
		c.ResponseHeaderTimeoutSeconds = DefaultRespHeaderTimeout
	}
	if c.UtilizationScale == "" {
		c.UtilizationScale = DefaultUtilizationScale
	}
	switch c.UtilizationScale {
	case "fraction", "percent":
	default:
		return fmt.Errorf("utilization_scale must be %q or %q, got %q", "fraction", "percent", c.UtilizationScale)
	}
	seen := map[string]bool{}
	for i := range c.Accounts {
		a := &c.Accounts[i]
		if a.ID == "" {
			return fmt.Errorf("accounts[%d]: id is required", i)
		}
		if seen[a.ID] {
			return fmt.Errorf("duplicate account id %q", a.ID)
		}
		seen[a.ID] = true
		if a.Provider == "" {
			a.Provider = c.DefaultProvider
		}
		if a.CredentialRef == "" {
			a.CredentialRef = a.ID
		}
	}
	cseen := map[string]bool{}
	for i := range c.Clients {
		cl := &c.Clients[i]
		if cl.ID == "" {
			return fmt.Errorf("clients[%d]: id is required", i)
		}
		if cseen[cl.ID] {
			return fmt.Errorf("duplicate client id %q", cl.ID)
		}
		cseen[cl.ID] = true
	}
	return nil
}

// FindAccount returns the account with the given ID, or nil.
func (c *Config) FindAccount(id string) *Account {
	for i := range c.Accounts {
		if c.Accounts[i].ID == id {
			return &c.Accounts[i]
		}
	}
	return nil
}

// FindClient returns the client with the given ID, or nil.
func (c *Config) FindClient(id string) *Client {
	for i := range c.Clients {
		if c.Clients[i].ID == id {
			return &c.Clients[i]
		}
	}
	return nil
}

// MintedTime parses MintedAt (YYYY-MM-DD). ok is false when unset or invalid.
func (a Account) MintedTime() (time.Time, bool) {
	if a.MintedAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", a.MintedAt)
	return t, err == nil
}

// Paths resolves the config and state file locations, XDG-style on every OS.
type Paths struct {
	ConfigFile string
	StateDir   string
	StateFile  string
	LockFile   string
}

// ResolvePaths computes the standard paths:
//
//	$XDG_CONFIG_HOME/accountant/config.toml  (or ~/.config/accountant/config.toml)
//	$XDG_STATE_HOME/accountant/state.json    (or ~/.local/state/accountant/state.json)
//
// ACCOUNTANT_CONFIG and ACCOUNTANT_STATE override the full file paths
// (used by tests and spikes).
func ResolvePaths() (Paths, error) {
	var p Paths
	if f := os.Getenv("ACCOUNTANT_CONFIG"); f != "" {
		p.ConfigFile = f
	} else {
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return p, err
			}
			base = filepath.Join(home, ".config")
		}
		p.ConfigFile = filepath.Join(base, "accountant", "config.toml")
	}
	if f := os.Getenv("ACCOUNTANT_STATE"); f != "" {
		p.StateFile = f
	} else {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return p, err
			}
			base = filepath.Join(home, ".local", "state")
		}
		p.StateFile = filepath.Join(base, "accountant", "state.json")
	}
	p.StateDir = filepath.Dir(p.StateFile)
	p.LockFile = filepath.Join(p.StateDir, "state.lock")
	return p, nil
}

// Load reads and parses the config file. A missing file yields an empty
// config with defaults.
func Load(path string) (*Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := c.Normalize(); err != nil {
				return nil, err
			}
			return &c, nil
		}
		return nil, err
	}
	if err := toml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := c.Normalize(); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", path, err)
	}
	return &c, nil
}

// fileHeader is prepended on every save so the shipped guidance survives
// CLI edits (the TOML encoder does not preserve comments).
const fileHeader = `# accountant configuration
#
# listen: loopback bind for the proxy (LAN phase changes this; nothing else hardcodes it)
# utilization_scale: "fraction" (default; from spike S1) or "percent"
# [[accounts]]: metadata only — credentials live in the vault, never here
# [[clients]]: key_sha256 only — the key itself is shown once at creation

`

// Marshal serializes the config as TOML.
func Marshal(c *Config) ([]byte, error) {
	var b strings.Builder
	b.WriteString(fileHeader)
	enc := toml.NewEncoder(&b)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}
