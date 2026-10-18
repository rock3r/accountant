package vault

import (
	"fmt"
	"regexp"
)

// safeFileRef restricts refs to characters safe as a filename component
// and as a Secret Service attribute. '/' is rejected: it would allow
// path traversal into subdirectories in the file backend.
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
