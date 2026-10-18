package vault

import (
	"fmt"
	"regexp"
)

// safeToken restricts refs to characters that are safe to embed in a
// double-quoted `security -i` command line (Keychain backend). Secrets are
// hex-encoded before storage (they can be structured JSON, e.g. Codex OAuth
// credentials), so they are always inside this set by construction.
var safeToken = regexp.MustCompile(`^[A-Za-z0-9._~+/=@:-]+$`)

// safeFileRef is the stricter set accepted by the file and Secret Service
// backends: '/' (present in safeToken for CLI quoting) would allow path
// traversal into subdirectories, so it is rejected here.
var safeFileRef = regexp.MustCompile(`^[A-Za-z0-9._~+=@:-]+$`)

// maxRefLen bounds a ref to a sane length (also a filename component for
// the file backend).
const maxRefLen = 128

// validateRef enforces the non-Keychain ref rules shared by the file and
// Secret Service backends.
func validateRef(ref string) error {
	if len(ref) > maxRefLen {
		return fmt.Errorf("vault: credential ref too long")
	}
	if !safeFileRef.MatchString(ref) {
		return fmt.Errorf("vault: unsafe credential ref")
	}
	return nil
}
