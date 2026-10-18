package vault

import "regexp"

// safeToken restricts refs to characters that are safe to embed in a
// double-quoted `security -i` command line (Keychain backend). Secrets are
// hex-encoded before storage (they can be structured JSON, e.g. Codex OAuth
// credentials), so they are always inside this set by construction.
var safeToken = regexp.MustCompile(`^[A-Za-z0-9._~+/=@:-]+$`)

// safeFileRef is the stricter set accepted by the file backend: '/'
// (present in safeToken for CLI quoting) would allow path traversal into
// subdirectories, so it is rejected here.
var safeFileRef = regexp.MustCompile(`^[A-Za-z0-9._~+=@:-]+$`)

// maxFileRefLen bounds a ref to a sane filename component length.
const maxFileRefLen = 128
