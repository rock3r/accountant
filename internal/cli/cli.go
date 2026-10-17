// Package cli implements the accountant command set: serve, account
// add|list|remove|test, client add|list|remove, env, status, doctor, and
// service install|uninstall.
package cli

import (
	"fmt"
	"io"

	"accountant/internal/config"
	"accountant/internal/state"
)

// Version is stamped at release time via -ldflags "-X accountant/internal/cli.Version=...".
var Version = "dev"

// Run dispatches argv (without the program name) and returns an exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	paths, err := config.ResolvePaths()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	switch args[0] {
	case "serve":
		return cmdServe(args[1:], paths, stderr)
	case "account":
		return cmdAccount(args[1:], paths, stdout, stderr)
	case "client":
		return cmdClient(args[1:], paths, stdout, stderr)
	case "env":
		return cmdEnv(paths, stdout)
	case "status":
		return cmdStatus(paths, stdout, stderr)
	case "doctor":
		return cmdDoctor(paths, stdout, stderr)
	case "service":
		return cmdService(args[1:], paths, stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "accountant %s\n", Version)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `accountant — multi-subscription rollover for Claude

Usage:
  accountant serve                              run the proxy daemon
  accountant account add claude --label NAME [--priority N]
  accountant account list
  accountant account remove ID
  accountant account test ID
  accountant client add ID
  accountant client list
  accountant client remove ID
  accountant env                                print Claude Code env exports
  accountant status                             quota and pin overview
  accountant doctor                             run health checks
  accountant service install|uninstall          manage the launchd user service
`)
}

func storeFor(paths config.Paths) *state.Store {
	return state.NewStore(paths.StateFile)
}
