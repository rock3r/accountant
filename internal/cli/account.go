package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"accountant/internal/config"
	"accountant/internal/provider/anthropic"
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
	if provider != "anthropic" {
		fmt.Fprintf(stderr, "unknown provider %q (v1 supports: claude)\n", provider)
		return 2
	}
	if label == "" {
		fmt.Fprintln(stderr, "--label is required")
		return 2
	}

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
		return 1
	}
	if token == "" {
		fmt.Fprintln(stderr, "error: empty token")
		return 1
	}

	prov := anthropic.New("")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := prov.Validate(ctx, []byte(token)); err != nil {
		fmt.Fprintf(stderr, "error: token validation failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Token validated.")

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
			return 1
		}
		if cfg.FindAccount(id) == nil {
			break
		}
		id = fmt.Sprintf("%s-%d", base, n)
	}

	if err := vault.NewKeychain().Put(id, []byte(token)); err != nil {
		fmt.Fprintf(stderr, "error: storing credential in Keychain: %v\n", err)
		return 1
	}
	minted := time.Now().Format("2006-01-02")
	err = config.LockedUpdate(paths.ConfigFile, paths.LockFile, func(c *config.Config) error {
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
		_ = vault.NewKeychain().Delete(id)
		fmt.Fprintf(stderr, "error: updating config: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Added account %q (provider %s, priority %d).\n", id, provider, priority)
	return 0
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
	prov := anthropic.New("")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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
