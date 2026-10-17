package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"

	"accountant/internal/config"
	"accountant/internal/proxy"
)

func cmdClient(args []string, paths config.Paths, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: accountant client add|list|remove ...")
		return 2
	}
	switch args[0] {
	case "add":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: accountant client add ID")
			return 2
		}
		return clientAdd(args[1], paths, stdout, stderr)
	case "list":
		return clientList(paths, stdout, stderr)
	case "remove":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: accountant client remove ID")
			return 2
		}
		return clientRemove(args[1], paths, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown client command %q\n", args[0])
		return 2
	}
}

func clientAdd(id string, paths config.Paths, stdout, stderr io.Writer) int {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	key := "ak_" + hex.EncodeToString(raw)
	err := config.LockedUpdate(paths.ConfigFile, paths.LockFile, func(c *config.Config) error {
		if c.FindClient(id) != nil {
			return fmt.Errorf("client %q already exists", id)
		}
		c.Clients = append(c.Clients, config.Client{ID: id, KeySHA256: proxy.HashKey(key)})
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, `Client %q created. Key (shown once, store it now):

  %s

Only the SHA-256 hash is stored; accountant cannot show it again.
`, id, key)
	return 0
}

func clientList(paths config.Paths, stdout, stderr io.Writer) int {
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if len(cfg.Clients) == 0 {
		fmt.Fprintln(stdout, "No clients. Create one with: accountant client add ID")
		return 0
	}
	for _, c := range cfg.Clients {
		fmt.Fprintln(stdout, c.ID)
	}
	return 0
}

func clientRemove(id string, paths config.Paths, stdout, stderr io.Writer) int {
	err := config.LockedUpdate(paths.ConfigFile, paths.LockFile, func(c *config.Config) error {
		if c.FindClient(id) == nil {
			return fmt.Errorf("no client %q", id)
		}
		kept := c.Clients[:0]
		for _, cl := range c.Clients {
			if cl.ID != id {
				kept = append(kept, cl)
			}
		}
		c.Clients = kept
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Removed client %q.\n", id)
	return 0
}

func cmdEnv(paths config.Paths, stdout io.Writer) int {
	cfg, err := config.Load(paths.ConfigFile)
	listen := config.DefaultListen
	if err == nil && cfg.Listen != "" {
		listen = cfg.Listen
	}
	fmt.Fprintf(stdout, `# Claude Code through accountant:
export ANTHROPIC_BASE_URL=http://%s
export ANTHROPIC_AUTH_TOKEN=<your client key>   # from: accountant client add ID
export ENABLE_TOOL_SEARCH=true

# Or the equivalent env block in ~/.claude/settings.json.
# Known losses behind the proxy: no Remote Control, no claude.ai connectors,
# no /login account display. Local MCP servers still work.
`, listen)
	return 0
}
