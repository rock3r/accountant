// Package cli — launchd user-service adapter. This is one of the two
// places macOS-specific code lives (the other is the Keychain vault).
package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"accountant/internal/config"
)

const launchdLabel = "com.accountant.agent"

func cmdService(args []string, paths config.Paths, stdout, stderr io.Writer) int {
	if len(args) != 1 || (args[0] != "install" && args[0] != "uninstall") {
		fmt.Fprintln(stderr, "usage: accountant service install|uninstall")
		return 2
	}
	if args[0] == "install" {
		return serviceInstall(paths, stdout, stderr)
	}
	return serviceUninstall(paths, stdout, stderr)
}

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

func serviceInstall(paths config.Paths, stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	exe, _ = filepath.EvalSymlinks(exe)
	pp, err := plistPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	envEntries := ""
	for _, k := range []string{"ACCOUNTANT_CONFIG", "ACCOUNTANT_STATE"} {
		if v := os.Getenv(k); v != "" {
			envEntries += fmt.Sprintf("\t\t<key>%s</key>\n\t\t<string>%s</string>\n", k, v)
		}
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>serve</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>%s
</dict>
</plist>
`, launchdLabel, exe, filepath.Join(paths.StateDir, "daemon.log"),
		filepath.Join(paths.StateDir, "daemon.log"), envTag(envEntries))

	if err := os.MkdirAll(filepath.Dir(pp), 0755); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if err := os.WriteFile(pp, []byte(plist), 0600); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	target := fmt.Sprintf("gui/%d", os.Getuid())
	cmd := exec.Command("launchctl", "bootstrap", target, pp)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Already bootstrapped: reload it.
		_ = exec.Command("launchctl", "bootout", target+"/"+launchdLabel).Run()
		if out2, err2 := exec.Command("launchctl", "bootstrap", target, pp).CombinedOutput(); err2 != nil {
			fmt.Fprintf(stderr, "error: launchctl bootstrap: %s\n", strings.TrimSpace(string(out2)))
			return 1
		}
		_ = out
	}
	fmt.Fprintf(stdout, "Installed and started %s\n  plist: %s\n  logs:  %s\n", launchdLabel, pp, filepath.Join(paths.StateDir, "daemon.log"))
	return 0
}

func envTag(entries string) string {
	if entries == "" {
		return ""
	}
	return "\n\t<key>EnvironmentVariables</key>\n\t<dict>\n" + entries + "\t</dict>"
}

func serviceUninstall(paths config.Paths, stdout, stderr io.Writer) int {
	pp, err := plistPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel)
	cmd := exec.Command("launchctl", "bootout", target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(stderr, "warning: launchctl bootout: %s\n", strings.TrimSpace(string(out)))
	}
	if err := os.Remove(pp); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Uninstalled %s\n", launchdLabel)
	return 0
}
