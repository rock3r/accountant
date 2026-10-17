package cli

import (
	"bytes"
	"strings"
	"testing"

	"accountant/internal/config"
	"accountant/internal/proxy"
)

func testPaths(t *testing.T) config.Paths {
	t.Helper()
	dir := t.TempDir()
	return config.Paths{
		ConfigFile: dir + "/config.toml",
		StateDir:   dir,
		StateFile:  dir + "/state.json",
		LockFile:   dir + "/state.lock",
	}
}

func TestClientLifecycle(t *testing.T) {
	paths := testPaths(t)
	var out, errOut bytes.Buffer

	if rc := clientAdd("macbook", paths, &out, &errOut); rc != 0 {
		t.Fatalf("clientAdd rc=%d: %s", rc, errOut.String())
	}
	// The key is shown once and only the hash is stored.
	if !strings.Contains(out.String(), "ak_") {
		t.Fatalf("key not shown: %q", out.String())
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	cl := cfg.FindClient("macbook")
	if cl == nil {
		t.Fatal("client not stored")
	}
	if strings.Contains(cl.KeySHA256, "ak_") {
		t.Fatal("plaintext key stored")
	}
	// Duplicate add fails.
	if rc := clientAdd("macbook", paths, &out, &errOut); rc == 0 {
		t.Fatal("duplicate add must fail")
	}
	out.Reset()
	if rc := clientList(paths, &out, &errOut); rc != 0 || !strings.Contains(out.String(), "macbook") {
		t.Fatalf("list rc=%d out=%q", rc, out.String())
	}
	if rc := clientRemove("macbook", paths, &out, &errOut); rc != 0 {
		t.Fatalf("remove rc=%d", rc)
	}
	cfg, _ = config.Load(paths.ConfigFile)
	if cfg.FindClient("macbook") != nil {
		t.Fatal("client not removed")
	}
}

func TestClientKeyVerifies(t *testing.T) {
	// The hash stored by client add must validate the presented key.
	paths := testPaths(t)
	var out, errOut bytes.Buffer
	if rc := clientAdd("c1", paths, &out, &errOut); rc != 0 {
		t.Fatal(errOut.String())
	}
	var key string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ak_") {
			key = strings.TrimSpace(line)
		}
	}
	if key == "" {
		t.Fatalf("no key in output: %q", out.String())
	}
	cfg, _ := config.Load(paths.ConfigFile)
	if cfg.Clients[0].KeySHA256 != proxy.HashKey(key) {
		t.Fatal("stored hash does not match shown key")
	}
}

func TestEnvPrintsExports(t *testing.T) {
	paths := testPaths(t)
	var out bytes.Buffer
	if rc := cmdEnv(paths, &out); rc != 0 {
		t.Fatal("env rc")
	}
	s := out.String()
	for _, want := range []string{"ANTHROPIC_BASE_URL=http://127.0.0.1:8787", "ANTHROPIC_AUTH_TOKEN", "ENABLE_TOOL_SEARCH=true"} {
		if !strings.Contains(s, want) {
			t.Fatalf("env output missing %q:\n%s", want, s)
		}
	}
}

func TestStatusEmptyAndWithAccount(t *testing.T) {
	paths := testPaths(t)
	var out, errOut bytes.Buffer
	if rc := cmdStatus(paths, &out, &errOut); rc != 0 {
		t.Fatal(errOut.String())
	}
	if !strings.Contains(out.String(), "No accounts") {
		t.Fatalf("status: %q", out.String())
	}
}
