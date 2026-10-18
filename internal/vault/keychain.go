//go:build darwin

// Package vault stores credentials in the macOS Keychain via the
// /usr/bin/security CLI (keeps the binary cgo-free).
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
	"time"
)

const (
	keychainService = "accountant"
	// chunkService is deliberately disjoint from keychainService: chunk
	// item names must never collide with real credential refs (a ref
	// like "foo.c1" is perfectly legal for an unrelated account).
	chunkService = "accountant-chunk"
)

// security -i truncates input lines at ~4 KB, silently turning the
// remainder into garbage commands. Hex payloads longer than maxInlineHex
// are therefore chunked.
//
// Chunked layout (v3): the head item in keychainService holds
// "acct1:chunks:<gen>:<count>"; the payload lives in chunkService items
// named "<ref>.g<gen>.c<i>". Every Put writes a fresh generation of
// chunks and swaps the head (a single-item atomic replace) last, so a
// concurrent reader following the old head always sees its own intact,
// never-rewritten generation — no hybrid values. Superseded generations
// are deleted after the swap. A "<ref>.pending" marker in chunkService
// (written before the chunks) names the in-flight generation so crashes
// and deletes can still find and remove every chunk.
//
// Legacy layouts kept readable: v2 heads "acct1:chunks:<count>" point at
// fixed-name chunks ref.cN in keychainService (never written anymore);
// v1 values predate hex encoding entirely.
const (
	maxInlineHex = 3800
	chunksMagic  = "acct1:chunks:"
)

// chunkHead describes where a chunked item's payload lives.
type chunkHead struct {
	gen   int64 // 0 => legacy v2 fixed-name chunks in keychainService
	count int
}

// chunkRef names legacy v2 chunk i (1-based) of ref, in keychainService.
func chunkRef(ref string, i int) string {
	return fmt.Sprintf("%s.c%d", ref, i)
}

// chunkItemRef names chunk i (1-based) of generation gen of ref, in
// chunkService.
func chunkItemRef(ref string, gen int64, i int) string {
	return fmt.Sprintf("%s.g%d.c%d", ref, gen, i)
}

// pendingRef names the in-flight-generation marker for ref, in
// chunkService.
func pendingRef(ref string) string {
	return ref + ".pending"
}

// parseChunkHead parses a head (or pending) value. count is 1..1000; gen
// is positive for v3.
func parseChunkHead(v string) (chunkHead, bool) {
	rest, ok := strings.CutPrefix(v, chunksMagic)
	if ok {
		if genStr, cntStr, isV3 := strings.Cut(rest, ":"); isV3 {
			gen, errGen := strconv.ParseInt(genStr, 10, 64)
			cnt, errCnt := strconv.Atoi(cntStr)
			if errGen != nil || errCnt != nil || gen <= 0 || cnt < 1 || cnt > 1000 {
				return chunkHead{}, false
			}
			return chunkHead{gen: gen, count: cnt}, true
		}
		cnt, err := strconv.Atoi(rest)
		if err != nil || cnt < 1 || cnt > 1000 {
			return chunkHead{}, false
		}
		return chunkHead{gen: 0, count: cnt}, true // legacy v2
	}
	// Pending marker format: "<gen>:<count>".
	genStr, cntStr, isPending := strings.Cut(v, ":")
	if !isPending {
		return chunkHead{}, false
	}
	gen, errGen := strconv.ParseInt(genStr, 10, 64)
	cnt, errCnt := strconv.Atoi(cntStr)
	if errGen != nil || errCnt != nil || gen <= 0 || cnt < 1 || cnt > 1000 {
		return chunkHead{}, false
	}
	return chunkHead{gen: gen, count: cnt}, true
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
	for attempt := 0; ; attempt++ {
		raw, err := k.getRaw(keychainService, ref)
		if err != nil {
			return nil, err
		}
		head, chunked := parseChunkHead(string(raw))
		if !chunked {
			if decoded, ok := decodeStored(raw); ok {
				return decoded, nil
			}
			return raw, nil
		}
		payload, err := k.readChunks(ref, head)
		if err == nil {
			if decoded, ok := decodeStored(payload); ok {
				return decoded, nil
			}
			return nil, fmt.Errorf("vault: corrupt chunked payload for %s", ref)
		}
		// The generation named by the head may have been garbage-collected
		// between reading the head and its chunks (a concurrent rotation
		// committed and swept). Re-read the head and follow whatever it
		// names now, bounded.
		if attempt >= 2 {
			return nil, err
		}
	}
}

// readChunks assembles the hex payload named by head.
func (k *Keychain) readChunks(ref string, head chunkHead) ([]byte, error) {
	var buf bytes.Buffer
	for i := 1; i <= head.count; i++ {
		service, name := keychainService, chunkRef(ref, i)
		if head.gen != 0 {
			service, name = chunkService, chunkItemRef(ref, head.gen, i)
		}
		chunk, err := k.getRaw(service, name)
		if err != nil {
			return nil, fmt.Errorf("vault: reading chunk %d of %s: %w", i, ref, err)
		}
		buf.Write(chunk)
	}
	return buf.Bytes(), nil
}

// getRaw reads the stored value for ref without any decoding.
func (k *Keychain) getRaw(service, ref string) ([]byte, error) {
	if !safeToken.MatchString(ref) {
		return nil, fmt.Errorf("vault: unsafe credential ref")
	}
	cmd := exec.Command("security", "find-generic-password", "-s", service, "-a", ref, "-w")
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
// line are chunked (see the layout comment above).
func (k *Keychain) Put(ref string, secret []byte) error {
	if !safeToken.MatchString(ref) {
		return fmt.Errorf("vault: unsafe credential ref")
	}
	if len(secret) == 0 {
		return fmt.Errorf("vault: empty credential")
	}
	hexed := hex.EncodeToString(secret)
	old, _ := k.getRaw(keychainService, ref)
	oldHead, hadChunks := parseChunkHead(string(old))
	// The generation that will be live after this Put (0 = inline).
	live := chunkHead{}
	if len(hexed) <= maxInlineHex {
		if err := k.putInline(keychainService, ref, hexed); err != nil {
			return err
		}
	} else {
		live.gen = time.Now().UnixNano()
		live.count = (len(hexed) + maxInlineHex - 1) / maxInlineHex
		// Pending marker first: whatever crashes below, the in-flight
		// generation stays findable for later cleanup.
		if err := k.putInline(chunkService, pendingRef(ref),
			fmt.Sprintf("%d:%d", live.gen, live.count)); err != nil {
			return err
		}
		for i := 0; i < live.count; i++ {
			hi := (i + 1) * maxInlineHex
			if hi > len(hexed) {
				hi = len(hexed)
			}
			if err := k.putInline(chunkService, chunkItemRef(ref, live.gen, i+1),
				hexed[i*maxInlineHex:hi]); err != nil {
				return err
			}
		}
		// Commit: the head swap is the single atomic step that makes the
		// new value visible. Chunks of the old generation are never
		// rewritten, so readers on the old head stay consistent.
		if err := k.putInline(keychainService, ref,
			fmt.Sprintf("%s%d:%d", chunksMagic, live.gen, live.count)); err != nil {
			return err
		}
	}
	// Best-effort garbage collection of superseded chunk material.
	if hadChunks {
		k.deleteGeneration(ref, oldHead)
	}
	// A pending marker naming the live generation (cleanup of a previous
	// Put was interrupted) keeps its chunks; only the marker goes away.
	k.deletePending(ref, live)
	return nil
}

// putInline writes one keychain item via `security -i`. The value must fit
// in a single input line and contain only safeToken characters.
func (k *Keychain) putInline(service, ref, value string) error {
	// `security -i` reads commands from stdin; the secret travels through
	// the pipe and never appears in the process argument list.
	script := fmt.Sprintf("add-generic-password -s %s -a \"%s\" -w \"%s\" -U\n",
		service, ref, value)
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

// deleteItem removes one item, mapping a missing item to ErrNotFound.
func (k *Keychain) deleteItem(service, ref string) error {
	if !safeToken.MatchString(ref) {
		return fmt.Errorf("vault: unsafe credential ref")
	}
	cmd := exec.Command("security", "delete-generic-password", "-s", service, "-a", ref)
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

// deleteGeneration removes every chunk of one generation (or of the legacy
// v2 fixed-name layout). Best effort.
func (k *Keychain) deleteGeneration(ref string, head chunkHead) {
	for i := 1; i <= head.count; i++ {
		service, name := keychainService, chunkRef(ref, i)
		if head.gen != 0 {
			service, name = chunkService, chunkItemRef(ref, head.gen, i)
		}
		_ = k.deleteItem(service, name)
	}
}

// deletePending removes the pending marker and, unless keep says it names
// the live generation, the chunks it points at. Best effort.
func (k *Keychain) deletePending(ref string, keep chunkHead) {
	raw, err := k.getRaw(chunkService, pendingRef(ref))
	if err != nil {
		return
	}
	pend, ok := parseChunkHead(string(raw))
	if ok && pend.gen != keep.gen {
		k.deleteGeneration(ref, pend)
	}
	_ = k.deleteItem(chunkService, pendingRef(ref))
}

// Delete removes the credential for ref together with all chunk material.
// Chunk sweeping also runs when the head is already gone, so interrupted
// Puts and partially failed deletes never leave unreachable secrets.
func (k *Keychain) Delete(ref string) error {
	if !safeToken.MatchString(ref) {
		return fmt.Errorf("vault: unsafe credential ref")
	}
	raw, _ := k.getRaw(keychainService, ref)
	head, hadChunks := parseChunkHead(string(raw))
	err := k.deleteItem(keychainService, ref)
	if hadChunks {
		k.deleteGeneration(ref, head)
	}
	// Any pending generation is garbage now; remove it and its chunks.
	k.deletePending(ref, chunkHead{})
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	return err
}

// sanitizeSecurityOutput reduces a `security -i` failure line to its
// status-code prefix so it cannot echo any secret material.
func sanitizeSecurityOutput(out []byte) string {
	line, _, _ := bytes.Cut(out, []byte("\n"))
	s := string(line)
	if i := strings.IndexByte(s, '"'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
