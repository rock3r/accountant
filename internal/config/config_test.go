package config

import (
	"os"
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	c, err := Load(t.TempDir() + "/missing.toml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != DefaultListen || c.DefaultProvider != DefaultProvider ||
		c.RequestBodyLimitMiB != DefaultBodyLimitMiB ||
		c.ResponseHeaderTimeoutSeconds != DefaultRespHeaderTimeout ||
		c.UtilizationScale != DefaultUtilizationScale {
		t.Fatalf("bad defaults: %+v", c)
	}
}

func TestParseExample(t *testing.T) {
	toml := `
listen = "127.0.0.1:8787"
default_provider = "anthropic"
request_body_limit_mib = 32
response_header_timeout_seconds = 120
utilization_scale = "fraction"

[[accounts]]
id = "claude-work"
provider = "anthropic"
label = "Work Max"
priority = 10
minted_at = "2026-10-17"

[[clients]]
id = "macbook"
key_sha256 = "deadbeef"
`
	path := t.TempDir() + "/config.toml"
	if err := os.WriteFile(path, []byte(toml), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a := c.FindAccount("claude-work")
	if a == nil || a.Priority != 10 || a.MintedAt != "2026-10-17" || !a.IsEnabled() {
		t.Fatalf("account: %+v", a)
	}
	if a.CredentialRef != "claude-work" {
		t.Fatalf("credential ref default: %q", a.CredentialRef)
	}
	if c.FindClient("macbook") == nil {
		t.Fatal("client missing")
	}
}

func TestLockedUpdate(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.toml"
	lockPath := dir + "/state.lock"
	err := LockedUpdate(cfgPath, lockPath, func(c *Config) error {
		c.Accounts = append(c.Accounts, Account{ID: "a1", Provider: "anthropic", Priority: 10})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.FindAccount("a1") == nil {
		t.Fatal("update not persisted")
	}
	fi, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("config perms %04o, want 0600", fi.Mode().Perm())
	}
}

func TestRoundTrip(t *testing.T) {
	path := t.TempDir() + "/config.toml"
	in := &Config{
		Accounts: []Account{{ID: "a1", Provider: "anthropic", Label: "One", Priority: 10, MintedAt: "2026-10-17"}},
		Clients:  []Client{{ID: "c1", KeySHA256: "abc"}},
	}
	if err := in.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.Accounts[0].Label != "One" || out.Clients[0].KeySHA256 != "abc" {
		t.Fatalf("round trip: %+v", out)
	}
	// The guidance header survives saves.
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# accountant configuration") {
		t.Fatal("guidance header lost on save")
	}
}
