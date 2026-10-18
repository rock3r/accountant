// Package vault — macOS Keychain backend. One generic-password item per
// account, service "accountant". Implemented via the /usr/bin/security CLI
// so the binary stays cgo-free. Secrets are piped to `security -i` on
// stdin, never passed as process arguments, and never included in error
// text.
package vault

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

const keychainService = "accountant"

// security -i truncates input lines at ~4 KB, silently turning the
// remainder into garbage commands. Hex payloads longer than maxInlineHex
// are therefore split into numbered chunk items (ref.c1, ref.c2, …); the
// head item then holds a short magic pointer instead of the payload.
// Chunks are written before the head (the head is the commit point), and
// any stale higher-numbered chunks are removed afterwards.
const (
	maxInlineHex = 3800
	chunksMagic  = "acct1:chunks:"
)

// chunkRef names chunk i (1-based) of a chunked item. The suffix uses only
// safeToken characters.
func chunkRef(ref string, i int) string {
	return fmt.Sprintf("%s.c%d", ref, i)
}

// safeToken restricts refs to characters that are safe to embed in a
// double-quoted `security -i` command line. Secrets are hex-encoded before
// storage (they can be structured JSON, e.g. Codex OAuth credentials), so
// they are always inside this set by construction.
var safeToken = regexp.MustCompile(`^[A-Za-z0-9._~+/=@:-]+$`)

// Keychain is the macOS Keychain vault backend.
type Keychain struct{}

// NewKeychain returns the Keychain backend.
func NewKeychain() *Keychain { return &Keychain{} }

// Get reads the secret for ref. The secret appears on this process's stdout
// pipe only.
func (k *Keychain) Get(ref string) ([]byte, error) {
	raw, err := k.getRaw(ref)
	if err != nil {
		return nil, err
	}
	// Chunked item: the head points at numbered chunk items holding the
	// hex payload in pieces.
	if n, ok := strings.CutPrefix(string(raw), chunksMagic); ok {
		count, err := strconv.Atoi(n)
		if err != nil || count < 1 || count > 1000 {
			return nil, fmt.Errorf("vault: corrupt chunk pointer for %s", ref)
		}
		var buf bytes.Buffer
		for i := 1; i <= count; i++ {
			chunk, err := k.getRaw(chunkRef(ref, i))
			if err != nil {
				return nil, fmt.Errorf("vault: reading chunk %d of %s: %w", i, ref, err)
			}
			buf.Write(chunk)
		}
		if decoded, ok := decodeStored(buf.Bytes()); ok {
			return decoded, nil
		}
		return nil, fmt.Errorf("vault: corrupt chunked payload for %s", ref)
	}
	if decoded, ok := decodeStored(raw); ok {
		return decoded, nil
	}
	return raw, nil
}

// getRaw reads the stored value for ref without any decoding.
func (k *Keychain) getRaw(ref string) ([]byte, error) {
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

// decodeStored undoes Put's hex encoding. A value that is not canonical
// even-length lowercase hex predates the encoding (v1 stored setup-tokens
// raw) and is returned unchanged.
func decodeStored(v []byte) ([]byte, bool) {
	if len(v) == 0 || len(v)%2 != 0 {
		return nil, false
	}
	decoded, err := hex.DecodeString(string(v))
	if err != nil || hex.EncodeToString(decoded) != string(v) {
		return nil, false
	}
	return decoded, true
}

// Put stores the secret for ref, updating an existing item. The secret is
// hex-encoded first: structured credentials (Codex OAuth JSON) contain
// characters that cannot be embedded safely, and lowercase hex is inside
// the safe set by construction. Payloads too long for one `security -i`
// line are chunked (see maxInlineHex).
func (k *Keychain) Put(ref string, secret []byte) error {
	if !safeToken.MatchString(ref) {
		return fmt.Errorf("vault: unsafe credential ref")
	}
	if len(secret) == 0 {
		return fmt.Errorf("vault: empty credential")
	}
	hexed := hex.EncodeToString(secret)
	if len(hexed) <= maxInlineHex {
		if err := k.putInline(ref, hexed); err != nil {
			return err
		}
		k.deleteChunks(ref, 1)
		return nil
	}
	// Write chunks first, the head last: the head is the commit point, so
	// a reader either sees the whole old value or the whole new one (for a
	// same-chunk-count rewrite). A rotation must never strand the only
	// valid credential on a half-written item.
	n := (len(hexed) + maxInlineHex - 1) / maxInlineHex
	for i := 0; i < n; i++ {
		hi := (i + 1) * maxInlineHex
		if hi > len(hexed) {
			hi = len(hexed)
		}
		if err := k.putInline(chunkRef(ref, i+1), hexed[i*maxInlineHex:hi]); err != nil {
			return err
		}
	}
	if err := k.putInline(ref, chunksMagic+strconv.Itoa(n)); err != nil {
		return err
	}
	k.deleteChunks(ref, n+1)
	return nil
}

// putInline writes one keychain item via `security -i`. The value must fit
// in a single input line and contain only safeToken characters.
func (k *Keychain) putInline(ref, value string) error {
	// `security -i` reads commands from stdin; the secret travels through
	// the pipe and never appears in the process argument list.
	script := fmt.Sprintf("add-generic-password -s %s -a \"%s\" -w \"%s\" -U\n",
		keychainService, ref, value)
	cmd := exec.Command("security", "-i")
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		// Never include the script or output (they may contain the secret)
		// in the error.
		return fmt.Errorf("keychain write failed: %w", err)
	}
	if bytes.Contains(out.Bytes(), []byte("returned -")) {
		return fmt.Errorf("keychain write failed: %s", sanitizeSecurityOutput(out.Bytes()))
	}
	return nil
}

// deleteChunks removes chunk items ref.c<from>, ref.c<from+1>, … until the
// first gap. Best effort: leftovers are unreachable once the head no
// longer points at them.
func (k *Keychain) deleteChunks(ref string, from int) {
	for i := from; ; i++ {
		if err := k.Delete(chunkRef(ref, i)); errors.Is(err, ErrNotFound) || i > from+1000 {
			return
		}
	}
}

// Delete removes the item for ref, including any chunk items.
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
	k.deleteChunks(ref, 1)
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
