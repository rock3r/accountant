// Command accountant is the multi-subscription rollover proxy for Claude.
package main

import (
	"os"

	"accountant/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
