package cli

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"time"

	"accountant/internal/config"
	"accountant/internal/provider"
	"accountant/internal/provider/anthropic"
	"accountant/internal/vault"
)

// cmdDoctor runs health checks: file permissions, token test per account,
// upstream reachability. It re-runs S1's calls on demand.
func cmdDoctor(paths config.Paths, stdout, stderr io.Writer) int {
	failed := false
	report := func(ok bool, warn bool, format string, args ...any) {
		mark := "PASS"
		if warn {
			mark = "WARN"
		}
		if !ok {
			mark = "FAIL"
			failed = true
		}
		fmt.Fprintf(stdout, "%s %s\n", mark, fmt.Sprintf(format, args...))
	}

	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		report(false, false, "config: %v", err)
		return 1
	}
	report(true, false, "config: %s parses", paths.ConfigFile)
	checkPerms(paths.ConfigFile, 0600, report)
	checkPerms(paths.StateDir, 0700, report)
	if _, err := os.Stat(paths.StateFile); err == nil {
		checkPerms(paths.StateFile, 0600, report)
	}

	enabled := 0
	for _, a := range cfg.Accounts {
		if a.IsEnabled() {
			enabled++
		}
	}
	if enabled == 0 {
		report(false, false, "no enabled accounts (add one: accountant account add claude|codex --label NAME)")
	} else {
		report(true, false, "%d enabled account(s)", enabled)
	}
	if len(cfg.Clients) == 0 {
		report(true, true, "no client keys (create one: accountant client add ID)")
	} else {
		report(true, false, "%d client key(s)", len(cfg.Clients))
	}

	// Upstream reachability per configured provider (no auth: any HTTP
	// answer proves reachability).
	provs := map[string]provider.Provider{}
	for _, a := range cfg.Accounts {
		if p := providerFor(a.Provider); p != nil {
			provs[a.Provider] = p
		}
	}
	if len(provs) == 0 {
		provs["anthropic"] = anthropic.New("")
	}
	for id, prov := range provs {
		reachable := false
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, prov.UpstreamBaseURL()+"/healthz", nil)
			if err != nil {
				return
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			_ = resp.Body.Close()
			reachable = true
		}()
		report(reachable, false, "upstream %s (%s) reachable", prov.UpstreamBaseURL(), id)
	}

	// Per-account credential check, provider-aware; expiring credentials
	// (Codex OAuth) are refreshed first.
	kc := vault.NewKeychain()
	for _, a := range cfg.Accounts {
		if !a.IsEnabled() {
			continue
		}
		prov := providerFor(a.Provider)
		if prov == nil {
			report(false, false, "account %s: unknown provider %q", a.ID, a.Provider)
			continue
		}
		cred, err := kc.Get(a.CredentialRef)
		if err != nil {
			report(false, false, "account %s: %v", a.ID, err)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cred, _, err = ensureFreshCredential(ctx, prov, paths, a.CredentialRef, cred)
		if err == nil {
			err = prov.Validate(ctx, cred)
		}
		cancel()
		if err != nil {
			report(false, false, "account %s: token test failed: %v", a.ID, err)
			continue
		}
		report(true, false, "account %s: token valid", a.ID)
	}

	if failed {
		fmt.Fprintln(stderr, "doctor: failures found")
		return 1
	}
	return 0
}

func checkPerms(path string, want fs.FileMode, report func(bool, bool, string, ...any)) {
	fi, err := os.Stat(path)
	if err != nil {
		report(false, false, "%s: %v", path, err)
		return
	}
	got := fi.Mode().Perm()
	if got != want {
		report(true, true, "%s: permissions %04o, want %04o (fix: chmod %o %s)", path, got, want, want, path)
		return
	}
	report(true, false, "%s: permissions %04o", path, got)
}
