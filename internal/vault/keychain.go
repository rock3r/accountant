// Package vault — macOS Keychain backend. One generic-password item per
// account, service "accountant". Implemented via the /usr/bin/security CLI
// so the binary stays cgo-free. Secrets are piped to `security -i` on
// stdin, never passed as process arguments, and never included in error
// text.
package vault

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

const keychainService = "accountant"

// safeToken restricts secrets and refs to characters that are safe to embed
// in a double-quoted `security -i` command line. OAuth tokens and refs are
// base64url-ish; anything outside this set is rejected rather than escaped.
var safeToken = regexp.MustCompile(`^[A-Za-z0-9._~+/=@:-]+$`)

// Keychain is the macOS Keychain vault backend.
type Keychain struct{}

// NewKeychain returns the Keychain backend.
func NewKeychain() *Keychain { return &Keychain{} }

// Get reads the secret for ref. The secret appears on this process's stdout
// pipe only.
func (k *Keychain) Get(ref string) ([]byte, error) {
	if !safeToken.MatchString(ref) {
		return nil, fmt.Errorf("vault: unsafe credential ref")
	}
	cmd := exec.Command("security", "find-generic-password", "-s", keychainService, "-a", ref, "-w")
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		if strings.Contains(errOut.String(), "could not be found") {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
		}
		return nil, fmt.Errorf("keychain read failed for %s: %s", ref, strings.TrimSpace(errOut.String()))
	}
	return bytes.TrimRight(out.Bytes(), "\r\n"), nil
}

// Put stores the secret for ref, updating an existing item.
func (k *Keychain) Put(ref string, secret []byte) error {
	if !safeToken.MatchString(ref) {
		return fmt.Errorf("vault: unsafe credential ref")
	}
	if !safeToken.Match(secret) {
		return fmt.Errorf("vault: credential contains characters outside the safe set")
	}
	// `security -i` reads commands from stdin; the secret travels through
	// the pipe and never appears in the process argument list.
	script := fmt.Sprintf("add-generic-password -s %s -a \"%s\" -w \"%s\" -U\n",
		keychainService, ref, secret)
	cmd := exec.Command("security", "-i")
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		// Never include the script (it contains the secret) in the error.
		return errors.New("keychain write failed")
	}
	if bytes.Contains(out.Bytes(), []byte("returned -")) {
		return fmt.Errorf("keychain write failed: %s", sanitizeSecurityOutput(out.Bytes()))
	}
	return nil
}

// Delete removes the item for ref.
func (k *Keychain) Delete(ref string) error {
	if !safeToken.MatchString(ref) {
		return fmt.Errorf("vault: unsafe credential ref")
	}
	cmd := exec.Command("security", "delete-generic-password", "-s", keychainService, "-a", ref)
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		if strings.Contains(errOut.String(), "could not be found") {
			return fmt.Errorf("%w: %s", ErrNotFound, ref)
		}
		return fmt.Errorf("keychain delete failed for %s: %s", ref, strings.TrimSpace(errOut.String()))
	}
	return nil
}

// sanitizeSecurityOutput strips anything after the first newline and any
// quoted content, so an echoed command cannot leak into an error message.
func sanitizeSecurityOutput(out []byte) string {
	line, _, _ := bytes.Cut(out, []byte("\n"))
	s := string(line)
	if i := strings.IndexByte(s, '"'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
