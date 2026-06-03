// Package sanitize is the byte-for-behavior re-implementation of Swift
// safeDestinationURL (PLAN §3.9): the traversal-proof filename sanitizer, the
// 200-UTF-8-byte ext-preserving cap, the collision-suffix scheme, and the
// millisecond UTC timestamp. It is a LEAF package (stdlib only) so both
// internal/ingest (which re-exports it under the frozen ingest.* names) and
// internal/store (which calls it directly to produce final leaf names) can depend on
// it without an import cycle.
//
// Filenames are part of the OBSERVABLE contract (echoed in ClipResponse.saved and
// /recv path:), so any drift from Swift breaks cross-app automation.
package sanitize

import (
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Item kinds (Swift ItemKind raw values) — duplicated here so the leaf package owns
// no upward dependency. ingest re-exports its own copies.
const (
	KindImage = "image"
	KindFile  = "file"
	KindText  = "text"
)

// ErrPathEscape is returned when a sanitized candidate would escape the save dir
// (PLAN §3.9 rule 6). ingest re-exports it as ingest.ErrPathEscape.
var ErrPathEscape = errors.New("clipbeam: path escape")

// NameMaxBytes caps a sanitized filename to 200 UTF-8 bytes (PLAN §3.9 rule 5).
const NameMaxBytes = 200

// Sanitizer produces a safe destination leaf name inside a save dir for an
// attacker-influenced raw name. It is an interface so the store can inject a
// containment / write-failure seam for tests (PLAN §12.4) while production uses the
// default implementation.
type Sanitizer interface {
	// SafeLeaf returns the sanitized, collision-FREE-pending leaf for raw inside
	// saveDir, or ErrPathEscape if the resolved candidate escapes saveDir.
	SafeLeaf(raw, kind, uti string, saveDir string) (string, error)
}

// allowed is the filename allowlist: [A-Za-z0-9 ._-] (space allowed). Every rune
// outside it maps to '_' (PLAN §3.9 rule 2, Swift Clipboard.swift:494).
func allowed(r rune) bool {
	switch {
	case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return true
	case r == ' ' || r == '.' || r == '_' || r == '-':
		return true
	}
	return false
}

// defaultSanitizer is the production Sanitizer (no injected failure seam).
type defaultSanitizer struct{}

// Default returns the production Sanitizer.
func Default() Sanitizer { return defaultSanitizer{} }

// SafeLeaf implements the §3.9 rules byte-for-behavior against Swift
// safeDestinationURL (Clipboard.swift:488-516):
//
//  1. take only the leaf (filepath.Base); backslash is NOT a path separator on
//     POSIX, so the allowlist below (not Base) demotes it to '_';
//  2. map every rune ∉ [A-Za-z0-9 ._-] → '_';
//  3. trim surrounding ASCII whitespace;
//  4. empty / "." / ".." / leading-dot → fallback clipbeam-<UTC>[.ext];
//  5. cap to 200 UTF-8 BYTES preserving the extension;
//  6. EvalSymlinks(saveDir) then assert the candidate has prefix resolvedDir+"/"
//     (else ErrPathEscape → a generic 500 that never echoes the path).
func (defaultSanitizer) SafeLeaf(raw, kind, uti string, saveDir string) (string, error) {
	cleaned := CleanLeaf(raw, kind, uti)

	// Rule 6: resolve the save dir's symlinks and assert containment. We resolve the
	// DIR (which exists) — mirroring Swift's resolvingSymlinksInPath on saveDir then a
	// prefix check on the candidate path — rather than the not-yet-existent candidate.
	resolvedDir := evalDir(saveDir)
	candidate := filepath.Join(resolvedDir, cleaned)
	dirPrefix := resolvedDir
	if !strings.HasSuffix(dirPrefix, string(filepath.Separator)) {
		dirPrefix += string(filepath.Separator)
	}
	if !strings.HasPrefix(candidate, dirPrefix) {
		return "", ErrPathEscape
	}
	return cleaned, nil
}

// CleanLeaf runs rules 1-5 (everything except the containment assert) and returns the
// sanitized leaf. Exposed so the store can reuse the deterministic core without a
// saveDir, and so the §3.9 vector table can assert the leaf directly.
func CleanLeaf(raw, kind, uti string) string {
	// Rule 1: leaf only. filepath.Base handles '/'; '\' is handled by the allowlist.
	leaf := filepath.Base(raw)

	// Rule 2: map every rune outside the allowlist → '_'.
	var b strings.Builder
	for _, r := range leaf {
		if allowed(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	cleaned := b.String()

	// Rule 3: trim surrounding ASCII whitespace. The allowlist already collapsed
	// everything except ASCII space, so trimming ' ' and '\t' is the byte residue of
	// Swift's .whitespaces trim.
	cleaned = strings.Trim(cleaned, " \t")

	// Rule 4: empty / "." / ".." / leading-dot → deterministic fallback. The fallback
	// starts with "clipbeam-", so it never re-trips the leading-dot guard.
	if cleaned == "" || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".") {
		cleaned = FallbackName(kind, uti)
	}

	// Rule 5: cap to 200 UTF-8 bytes preserving the extension.
	return CapLength(cleaned, NameMaxBytes)
}

// evalDir resolves symlinks in dir the way Swift's resolvingSymlinksInPath does on a
// directory: if the dir does not exist yet (it self-heals later), fall back to a
// lexical Clean so containment can still be asserted lexically. The save itself fails
// later with a real errno if the dir is genuinely unusable.
func evalDir(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return filepath.Clean(dir)
}

// FallbackName returns the deterministic safe fallback "clipbeam-<UTC>[.ext]" used by
// rule 4 (Swift fallbackName + the in-line fallback). The extension is the UTI/MIME-
// preferred one else png(image)/bin(file); text/unknown gets no ext.
func FallbackName(kind, uti string) string {
	base := "clipbeam-" + UTCTimestamp()
	if ext := FallbackExt(kind, uti); ext != "" {
		return base + "." + ext
	}
	return base
}

// FallbackExt returns the fallback extension: UTI-preferred (a small built-in table,
// no cgo / UTType) else png for image, bin for file, "" for text/unknown (PLAN §3.9,
// Swift fallbackName Clipboard.swift:687-695).
func FallbackExt(kind, uti string) string {
	if ext, ok := utiExt(uti); ok {
		return ext
	}
	switch kind {
	case KindImage:
		return "png"
	case KindFile:
		return "bin"
	default:
		return ""
	}
}

// utiExt maps the common Uniform Type Identifiers ClipBeam emits to a preferred
// filename extension, replicating UTType.preferredFilenameExtension without AppKit.
func utiExt(uti string) (string, bool) {
	switch uti {
	case "public.png":
		return "png", true
	case "public.jpeg":
		return "jpeg", true
	case "public.tiff":
		return "tiff", true
	case "com.compuserve.gif":
		return "gif", true
	case "public.heic", "public.heif":
		return "heic", true
	case "com.microsoft.bmp":
		return "bmp", true
	case "public.plain-text", "public.utf8-plain-text", "public.text":
		return "txt", true
	case "com.adobe.pdf":
		return "pdf", true
	case "public.zip-archive", "com.pkware.zip-archive":
		return "zip", true
	}
	return "", false
}

// CapLength caps name to maxBytes UTF-8 bytes, preserving the extension where the
// budget allows, byte-for-behavior with Swift capLength (Clipboard.swift:519-537).
func CapLength(name string, maxBytes int) string {
	if len(name) <= maxBytes {
		return name
	}
	ext := PathExtension(name)
	base := DeletingPathExtension(name)
	dotExt := ""
	if ext != "" {
		dotExt = "." + ext
	}
	budget := maxBytes - len(dotExt)
	if budget <= 0 {
		// Extension alone blows the budget — drop it and hard-truncate to maxBytes.
		return truncateBytes(name, maxBytes)
	}
	truncated := truncateBytes(base, budget)
	result := truncated + dotExt
	if result == "" {
		return "clipbeam-" + UTCTimestamp()
	}
	return result
}

// truncateBytes returns the longest prefix of s that is ≤ maxBytes bytes and does not
// split a UTF-8 rune (Swift's removeLast() loop stops on a scalar boundary).
func truncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	b := []byte(s)[:maxBytes]
	for len(b) > 0 {
		if r, size := utf8.DecodeLastRune(b); !(r == utf8.RuneError && size <= 1) {
			break
		}
		b = b[:len(b)-1]
	}
	return string(b)
}

// PathExtension returns the extension WITHOUT the dot, matching NSString.pathExtension:
// the chars after the LAST dot of the leaf, but only when that dot is not the leading
// char and there is at least one char after it. A leading-dot name (".bashrc") has no
// extension. Mirrors Cocoa so CapLength preserves exactly what Swift preserves.
func PathExtension(name string) string {
	leaf := filepath.Base(name)
	dot := strings.LastIndex(leaf, ".")
	if dot <= 0 || dot == len(leaf)-1 {
		return ""
	}
	return leaf[dot+1:]
}

// DeletingPathExtension returns name with its PathExtension (and the separating dot)
// removed, matching NSString.deletingPathExtension on the already-leaf input.
func DeletingPathExtension(name string) string {
	ext := PathExtension(name)
	if ext == "" {
		return name
	}
	return name[:len(name)-len(ext)-1]
}

// CollisionName returns the n-th collision candidate leaf, preserving the extension:
// n==2 → "base (2).ext", n==3 → "base (3).ext" (leading space + parens), byte-for-
// behavior with Swift uniqueURL (Clipboard.swift:466-478). leaf must be a sanitized
// leaf (no directory components).
func CollisionName(leaf string, n int) string {
	ext := PathExtension(leaf)
	base := DeletingPathExtension(leaf)
	if ext == "" {
		return base + " (" + itoa(n) + ")"
	}
	return base + " (" + itoa(n) + ")." + ext
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TextFileName returns the sanitized text-sidecar name clipbeam-<UTC>.txt (PLAN §3.9,
// Swift textFileName Clipboard.swift:697).
func TextFileName() string { return "clipbeam-" + UTCTimestamp() + ".txt" }

// UTCTimestamp returns yyyyMMdd-HHmmss-SSS in UTC with millisecond precision. The
// recipe is t.UTC().Format("20060102-150405.000") THEN replace '.' with '-' — writing
// "20060102-150405-000" directly emits a literal -000, not milliseconds (PLAN §3.9
// verified trap). Swift uses DateFormatter "yyyyMMdd-HHmmss-SSS"; Go's reference
// layout for fractional seconds is ".000", so the post-format dot→dash swap is
// mandatory.
func UTCTimestamp() string {
	s := time.Now().UTC().Format("20060102-150405.000")
	return strings.Replace(s, ".", "-", 1)
}
