package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"accountant/internal/config"
	"accountant/internal/provider/codex"
	"accountant/internal/state"
	"accountant/internal/vault"
)

func cmdAccount(args []string, paths config.Paths, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: accountant account add|list|remove|test ...")
		return 2
	}
	switch args[0] {
	case "add":
		return accountAdd(args[1:], paths, stdout, stderr)
	case "list":
		return accountList(paths, stdout, stderr)
	case "remove":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: accountant account remove ID")
			return 2
		}
		return accountRemove(args[1], paths, stdout, stderr)
	case "test":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: accountant account test ID")
			return 2
		}
		return accountTest(args[1], paths, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown account command %q\n", args[0])
		return 2
	}
}

var idSafe = regexp.MustCompile(`[^a-z0-9-]+`)

func accountAdd(args []string, paths config.Paths, stdout, stderr io.Writer) int {
	provider := ""
	label := ""
	from := ""
	priority := 10
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--label":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "--label needs a value")
				return 2
			}
			i++
			label = args[i]
		case "--from":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "--from needs a value")
				return 2
			}
			i++
			from = args[i]
		case "--priority":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "--priority needs a value")
				return 2
			}
			i++
			if _, err := fmt.Sscanf(args[i], "%d", &priority); err != nil {
				fmt.Fprintln(stderr, "--priority needs a number")
				return 2
			}
		default:
			if provider != "" {
				fmt.Fprintf(stderr, "unexpected argument %q\n", args[i])
				return 2
			}
			provider = args[i]
		}
	}
	// The design command form is `account add claude`; map to the provider ID.
	if provider == "claude" {
		provider = "anthropic"
	}
	if provider != "anthropic" && provider != "codex" {
		fmt.Fprintf(stderr, "unknown provider %q (supported: claude, codex)\n", provider)
		return 2
	}
	if label == "" {
		fmt.Fprintln(stderr, "--label is required")
		return 2
	}

	prov := providerFor(provider)

	// The account ID is generated before any credential work: for providers
	// with rotating credentials (codex) the vault entry is created before
	// validation, so a rotated refresh token is never lost when a later
	// step fails.
	id := generateAccountID(paths, label, stderr)
	if id == "" {
		return 1
	}

	var cred []byte
	switch provider {
	case "anthropic":
		var ok bool
		cred, ok = accountAddAnthropic(label, stdout, stderr)
		if !ok {
			return 1
		}
	case "codex":
		var ok bool
		cred, ok = accountAddCodex(from, id, stdout, stderr)
		if !ok {
			return 1
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if provider == "codex" {
		// Refresh an expired/near-expiry credential before validating, so a
		// long-idle imported login validates against a fresh access token.
		// The rotation is persisted under the shared refresh lock at once.
		fresh, didRefresh, err := ensureFreshCredential(ctx, prov, paths, id, cred)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "warning: credential refresh failed: %v (validating as-is)\n", err)
		default:
			cred = fresh
			if didRefresh {
				fmt.Fprintln(stdout, "Refreshed the OAuth tokens.")
			}
		}
	}
	if err := prov.Validate(ctx, cred); err != nil {
		cleanupFailedAdd(id, provider, stderr)
		fmt.Fprintf(stderr, "error: token validation failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Token validated.")

	if provider == "anthropic" {
		// Nothing has rotated; store only after validation passes.
		if err := vault.NewKeychain().Put(id, cred); err != nil {
			fmt.Fprintf(stderr, "error: storing credential in Keychain: %v\n", err)
			return 1
		}
	}
	minted := time.Now().Format("2006-01-02")
	err := config.LockedUpdate(paths.ConfigFile, paths.LockFile, func(c *config.Config) error {
		c.Accounts = append(c.Accounts, config.Account{
			ID:            id,
			Provider:      provider,
			Label:         label,
			Priority:      priority,
			CredentialRef: id,
			MintedAt:      minted,
		})
		return nil
	})
	if err != nil {
		cleanupFailedAdd(id, provider, stderr)
		fmt.Fprintf(stderr, "error: updating config: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Added account %q (provider %s, priority %d).\n", id, provider, priority)
	return 0
}

// generateAccountID derives a unique account ID from the label. Returns ""
// after printing an error.
func generateAccountID(paths config.Paths, label string, stderr io.Writer) string {
	id := idSafe.ReplaceAllString(strings.ToLower(label), "-")
	id = strings.Trim(id, "-")
	if id == "" {
		id = "account"
	}
	base := id
	for n := 2; ; n++ {
		cfg, err := config.Load(paths.ConfigFile)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return ""
		}
		if cfg.FindAccount(id) == nil {
			return id
		}
		id = fmt.Sprintf("%s-%d", base, n)
	}
}

// cleanupFailedAdd removes the vault entry for an account that never made
// it into the config. Codex entries are always preserved instead: the vault
// holds the freshest rotated credential by construction (stored before
// validation, rotated under the refresh lock), while the source auth.json
// may already be stale. A re-run of the same command resumes from it.
func cleanupFailedAdd(id, provider string, stderr io.Writer) {
	if provider == "codex" {
		fmt.Fprintf(stderr, "note: the OAuth tokens are preserved in the Keychain under ref %q; re-running the same command resumes from them.\n", id)
		return
	}
	_ = vault.NewKeychain().Delete(id)
}

// accountAddAnthropic runs the interactive setup-token flow and returns
// the pasted token. ok is false on any input error.
func accountAddAnthropic(label string, stdout, stderr io.Writer) (cred []byte, ok bool) {
	fmt.Fprintf(stdout, `To register this subscription:

  1. In a terminal, run Claude Code against a scratch config dir so your
     normal login is untouched:

       export CLAUDE_CONFIG_DIR="$(mktemp -d)/claude-%s"
       claude        # complete /login with the %q subscription
       claude setup-token

  2. Paste the token below (input is hidden). It is validated, stored in
     the macOS Keychain, and never written to any plain-text file.

Token: `, label, label)
	token, err := readSecret()
	fmt.Fprintln(stdout)
	if err != nil {
		fmt.Fprintf(stderr, "error: reading token: %v\n", err)
		return nil, false
	}
	if token == "" {
		fmt.Fprintln(stderr, "error: empty token")
		return nil, false
	}
	return []byte(token), true
}

// accountAddCodex imports the OAuth token triple from a Codex CLI login
// (auth.json). ok is false on any read/parse error. A parseable credential
// already in the vault under id comes from a previous attempt that rotated
// the tokens and then failed; it takes precedence over the (now stale)
// source file.
func accountAddCodex(from, id string, stdout, stderr io.Writer) (cred []byte, ok bool) {
	if existing, err := vault.NewKeychain().Get(id); err == nil {
		if _, err := codex.ParseCredential(existing); err == nil {
			fmt.Fprintf(stdout, "Reusing the rotated OAuth tokens stored under ref %q from the previous attempt.\n", id)
			return existing, true
		}
	}
	if from == "" {
		from = defaultCodexAuthPath()
	}
	cred, err := codex.ImportAuthFile(from)
	if err != nil {
		fmt.Fprintf(stderr, "error: importing Codex login: %v\n", err)
		fmt.Fprintln(stderr, "hint: log in with a dedicated Codex CLI profile first:")
		fmt.Fprintln(stderr, "  CODEX_HOME=\"$(mktemp -d)/codex-profile\" codex login")
		return nil, false
	}
	// Store immediately: once accountant refreshes these tokens the source
	// auth.json is stale, so the vault must hold them before any fallible
	// step that follows.
	if err := vault.NewKeychain().Put(id, cred); err != nil {
		fmt.Fprintf(stderr, "error: storing credential in Keychain: %v\n", err)
		return nil, false
	}
	fmt.Fprintf(stdout, `Imported Codex OAuth tokens from %s.

NOTE: accountant refreshes these tokens itself from now on, and Codex
refresh tokens rotate on every refresh. After accountant's first refresh,
the login in %s can no longer refresh on its own. Point Codex CLI at the
proxy (see `+"`accountant env`"+`); if you also want a direct, non-proxied
Codex login, keep it in a separate CODEX_HOME with its own auth.json.
`, from, from)
	return cred, true
}

// defaultCodexAuthPath resolves the Codex CLI's auth.json: $CODEX_HOME
// first, else ~/.codex.
func defaultCodexAuthPath() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "auth.json"
	}
	return filepath.Join(home, ".codex", "auth.json")
}

// readSecret reads one line with echo off when stdin is a terminal, else one
// line from the pipe.
func readSecret() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		return strings.TrimSpace(string(b)), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func accountList(paths config.Paths, stdout, stderr io.Writer) int {
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if len(cfg.Accounts) == 0 {
		fmt.Fprintln(stdout, "No accounts. Add one with: accountant account add claude --label NAME")
		return 0
	}
	sorted := append([]config.Account(nil), cfg.Accounts...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority < sorted[j].Priority })
	fmt.Fprintf(stdout, "%-18s %-10s %-8s %-8s %-12s %s\n", "ID", "PROVIDER", "PRIORITY", "ENABLED", "MINTED", "LABEL")
	for _, a := range sorted {
		fmt.Fprintf(stdout, "%-18s %-10s %-8d %-8t %-12s %s\n", a.ID, a.Provider, a.Priority, a.IsEnabled(), a.MintedAt, a.Label)
	}
	return 0
}

func accountRemove(id string, paths config.Paths, stdout, stderr io.Writer) int {
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	acct := cfg.FindAccount(id)
	if acct == nil {
		fmt.Fprintf(stderr, "error: no account %q\n", id)
		return 1
	}
	credRef := acct.CredentialRef
	err = config.LockedUpdate(paths.ConfigFile, paths.LockFile, func(c *config.Config) error {
		if c.FindAccount(id) == nil {
			return fmt.Errorf("no account %q", id)
		}
		kept := c.Accounts[:0]
		for _, a := range c.Accounts {
			if a.ID != id {
				kept = append(kept, a)
			}
		}
		c.Accounts = kept
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if err := vault.NewKeychain().Delete(credRef); err != nil {
		fmt.Fprintf(stderr, "warning: removing credential from Keychain: %v\n", err)
	}
	st := storeFor(paths)
	if err := st.Locked(func(s *state.State) (bool, error) {
		delete(s.Accounts, id)
		s.DropPinsFor(id)
		return true, nil
	}); err != nil {
		fmt.Fprintf(stderr, "warning: cleaning state: %v\n", err)
	}
	fmt.Fprintf(stdout, "Removed account %q.\n", id)
	return 0
}

func accountTest(id string, paths config.Paths, stdout, stderr io.Writer) int {
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	acct := cfg.FindAccount(id)
	if acct == nil {
		fmt.Fprintf(stderr, "error: no account %q\n", id)
		return 1
	}
	cred, err := vault.NewKeychain().Get(acct.CredentialRef)
	if err != nil {
		fmt.Fprintf(stderr, "error: reading credential: %v\n", err)
		return 1
	}
	prov := providerFor(acct.Provider)
	if prov == nil {
		fmt.Fprintf(stderr, "error: unknown provider %q\n", acct.Provider)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// ensureFreshCredential returns the current vault value, which may have
	// been refreshed by the daemon while this command waited on the shared
	// lock — always validate that value, not the pre-lock read.
	if fresh, refreshed, err := ensureFreshCredential(ctx, prov, paths, acct.CredentialRef, cred); err != nil {
		fmt.Fprintf(stderr, "FAIL %s: %v\n", id, err)
		return 1
	} else {
		cred = fresh
		if refreshed {
			fmt.Fprintln(stdout, "Refreshed the OAuth tokens.")
		}
	}
	if err := prov.Validate(ctx, cred); err != nil {
		fmt.Fprintf(stderr, "FAIL %s: %v\n", id, err)
		return 1
	}
	// A passing test clears auth-broken; traffic never does.
	st := storeFor(paths)
	if err := st.Locked(func(s *state.State) (bool, error) {
		as := s.Account(id)
		if !as.AuthBroken {
			return false, nil
		}
		as.AuthBroken = false
		return true, nil
	}); err != nil {
		fmt.Fprintf(stderr, "warning: clearing auth-broken state: %v\n", err)
	}
	fmt.Fprintf(stdout, "OK %s: credential valid; auth-broken cleared.\n", id)
	return 0
}
