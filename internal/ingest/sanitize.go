package ingest

import "github.com/vybzai/clipbeam-cli/internal/sanitize"

// Sanitizer produces a safe destination leaf name inside a save dir for an
// attacker-influenced raw name, reimplementing Swift safeDestinationURL
// byte-for-behavior (PLAN §3.9). Filenames are part of the OBSERVABLE contract
// (echoed in ClipResponse.saved and /recv path:), so any drift breaks automation.
//
// It is an interface so callers/tests can inject a containment / write-failure seam
// (PLAN §12.4) while production uses the default implementation. The canonical body
// lives in internal/sanitize (a leaf package) so internal/store can share it without
// an import cycle; this is a type alias so the frozen ingest.Sanitizer API is exact.
type Sanitizer = sanitize.Sanitizer

// DefaultSanitizer returns the production Sanitizer (PLAN §3.9).
func DefaultSanitizer() Sanitizer { return sanitize.Default() }

// TextFileName returns the sanitized text-sidecar name clipbeam-<UTC>.txt (PLAN §3.9).
func TextFileName() string { return sanitize.TextFileName() }
