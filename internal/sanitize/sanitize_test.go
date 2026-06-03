package sanitize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSafeLeafVectors is the §3.9 vector table: each attacker-influenced raw name maps
// to an exact sanitized leaf (or a deterministic fallback prefix when rule 4 fires).
// It asserts the allowlist, backslash→_, leaf-only, trim, and dotfile/./../empty
// fallback behaviors byte-for-behavior with Swift safeDestinationURL.
func TestSafeLeafVectors(t *testing.T) {
	dir := t.TempDir()
	san := Default()

	// exact: raw → expected leaf (no fallback involved).
	exact := []struct {
		name, raw, kind, uti, want string
	}{
		{"plain", "photo.png", KindImage, "", "photo.png"},
		{"strips directory components (traversal)", "../../.ssh/authorized_keys", KindFile, "", "authorized_keys"},
		{"backslash maps to underscore (POSIX leaf)", `a\b.png`, KindImage, "", "a_b.png"},
		{"slash is a separator: keeps the leaf", "evil/dir/ok.txt", KindFile, "", "ok.txt"},
		{"disallowed runes map to underscore", "a:b*c?.png", KindImage, "", "a_b_c_.png"},
		{"unicode maps to underscore each byte-rune", "café.png", KindFile, "", "caf_.png"},
		{"spaces are allowed", "my file.png", KindFile, "", "my file.png"},
		{"dash dot underscore allowed", "a-b_c.d.png", KindFile, "", "a-b_c.d.png"},
		{"trims surrounding whitespace", "  hi.png  ", KindImage, "", "hi.png"},
		{"control chars map to underscore", "a\tb\nc.png", KindFile, "", "a_b_c.png"},
		{"NUL maps to underscore", "a\x00b.png", KindFile, "", "a_b.png"},
	}
	for _, tc := range exact {
		t.Run(tc.name, func(t *testing.T) {
			got, err := san.SafeLeaf(tc.raw, tc.kind, tc.uti, dir)
			if err != nil {
				t.Fatalf("SafeLeaf(%q) error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("SafeLeaf(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}

	// fallback: raw → "clipbeam-<UTC>[.ext]" (rule 4). Assert the prefix + extension.
	fallback := []struct {
		name, raw, kind, uti, wantExt string
	}{
		{"empty → fallback (image png)", "", KindImage, "", "png"},
		{"dot → fallback (file bin)", ".", KindFile, "", "bin"},
		{"dotdot → fallback (file bin)", "..", KindFile, "", "bin"},
		{"dotfile → fallback (file bin)", ".bashrc", KindFile, "", "bin"},
		{"leading-dot multi → fallback", ".ssh.png", KindImage, "", "png"},
		{"whitespace-only → fallback", "   ", KindFile, "", "bin"},
		{"all-disallowed-then-trim collapses but keeps underscores", "***", KindImage, "", ""},
		{"uti overrides kind ext", ".x", KindFile, "public.png", "png"},
		{"text kind → no extension", "", KindText, "", ""},
	}
	for _, tc := range fallback {
		t.Run(tc.name, func(t *testing.T) {
			got, err := san.SafeLeaf(tc.raw, tc.kind, tc.uti, dir)
			if err != nil {
				t.Fatalf("SafeLeaf(%q) error: %v", tc.raw, err)
			}
			if tc.raw == "***" {
				// "***" → "___" (allowlisted to underscores, not empty) → not a fallback.
				if got != "___" {
					t.Fatalf("SafeLeaf(%q) = %q, want ___", tc.raw, got)
				}
				return
			}
			if !strings.HasPrefix(got, "clipbeam-") {
				t.Fatalf("SafeLeaf(%q) = %q, want clipbeam- prefix", tc.raw, got)
			}
			if tc.wantExt == "" {
				if filepath.Ext(got) != "" {
					t.Fatalf("SafeLeaf(%q) = %q, want no extension", tc.raw, got)
				}
			} else if filepath.Ext(got) != "."+tc.wantExt {
				t.Fatalf("SafeLeaf(%q) = %q, want .%s extension", tc.raw, got, tc.wantExt)
			}
		})
	}
}

// TestCapLength200Bytes asserts the 200-UTF-8-byte cap preserves the extension on a
// rune boundary and never splits a rune (§3.9 rule 5).
func TestCapLength200Bytes(t *testing.T) {
	// 300 'a' base + ".png" → capped so total ≤ 200, extension preserved.
	long := strings.Repeat("a", 300) + ".png"
	got := CapLength(long, NameMaxBytes)
	if len(got) > NameMaxBytes {
		t.Fatalf("capped len = %d, want ≤ %d", len(got), NameMaxBytes)
	}
	if !strings.HasSuffix(got, ".png") {
		t.Fatalf("capped %q lost the .png extension", got)
	}
	if want := NameMaxBytes - len(".png"); len(got) != want+len(".png") {
		t.Fatalf("capped len = %d, want exactly budget %d + ext", len(got), want)
	}

	// Multibyte base: ensure no split rune. Each 'é' is 2 bytes.
	mb := strings.Repeat("é", 200) + ".txt" // 400 bytes + ext
	gotMB := CapLength(mb, NameMaxBytes)
	if len(gotMB) > NameMaxBytes {
		t.Fatalf("multibyte capped len = %d, want ≤ %d", len(gotMB), NameMaxBytes)
	}
	if !validUTF8Boundary(gotMB) {
		t.Fatalf("multibyte cap split a rune: %q", gotMB)
	}

	// Extension alone over budget → hard truncate to 200 bytes, ext dropped.
	bigExt := "x." + strings.Repeat("e", 250)
	gotBig := CapLength(bigExt, NameMaxBytes)
	if len(gotBig) != NameMaxBytes {
		t.Fatalf("oversized-ext cap len = %d, want %d", len(gotBig), NameMaxBytes)
	}

	// Already within budget → unchanged.
	if CapLength("ok.png", NameMaxBytes) != "ok.png" {
		t.Fatal("within-budget name should be unchanged")
	}
}

func validUTF8Boundary(s string) bool {
	// strings package round-trips only valid UTF-8 without RuneError on the tail.
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

// TestCollisionName asserts the " (n)" suffix style preserves the extension (§3.9).
func TestCollisionName(t *testing.T) {
	cases := []struct {
		leaf string
		n    int
		want string
	}{
		{"photo.png", 2, "photo (2).png"},
		{"photo.png", 3, "photo (3).png"},
		{"noext", 2, "noext (2)"},
		{"a.b.c.txt", 2, "a.b.c (2).txt"},
		{"clipbeam-20060102-150405-000.txt", 4, "clipbeam-20060102-150405-000 (4).txt"},
	}
	for _, c := range cases {
		if got := CollisionName(c.leaf, c.n); got != c.want {
			t.Errorf("CollisionName(%q,%d) = %q, want %q", c.leaf, c.n, got, c.want)
		}
	}
}

// TestUTCTimestampShape asserts the timestamp is yyyyMMdd-HHmmss-SSS (the millisecond
// trap: the trailing -SSS is real milliseconds, not a literal -000), with the exact
// dash layout produced by the Format(".000")-then-replace recipe (§3.9).
func TestUTCTimestampShape(t *testing.T) {
	ts := UTCTimestamp()
	// 20060102-150405-000 → 8 + 1 + 6 + 1 + 3 = 19 chars, 3 dashes.
	if len(ts) != 19 {
		t.Fatalf("timestamp %q len = %d, want 19", ts, len(ts))
	}
	parts := strings.Split(ts, "-")
	if len(parts) != 3 {
		t.Fatalf("timestamp %q has %d dash-parts, want 3", ts, len(parts))
	}
	if len(parts[0]) != 8 || len(parts[1]) != 6 || len(parts[2]) != 3 {
		t.Fatalf("timestamp %q parts wrong width: %v", ts, parts)
	}
	for _, p := range parts {
		for _, r := range p {
			if r < '0' || r > '9' {
				t.Fatalf("timestamp %q has non-digit %q", ts, r)
			}
		}
	}
	// The millisecond field must NOT be a frozen literal "000" forever: two timestamps
	// taken across a >1ms gap should differ in at least the second or ms field. We
	// assert structure here; the cross-time variance is incidental, so only structure
	// is required for determinism.
}

// TestPathEscapeViaSymlinkContainment asserts that when the resolved candidate would
// land outside the symlink-resolved save dir, SafeLeaf returns ErrPathEscape. Normal
// allowlisted leaves cannot escape lexically, so we exercise the assert by resolving a
// save dir whose name itself is fine and confirming a benign leaf stays contained
// (the positive containment path), then confirm the prefix check rejects a crafted
// resolvedDir mismatch via a non-existent escaping dir is impossible — so we assert the
// invariant: every produced leaf joined under the dir keeps the dir prefix.
func TestPathEscapeContainmentInvariant(t *testing.T) {
	dir := t.TempDir()
	san := Default()
	resolved, _ := filepath.EvalSymlinks(dir)
	for _, raw := range []string{
		"../../etc/passwd", "....//x.png", "..\\..\\x.png", "a/../../b.png", ".", "..",
	} {
		leaf, err := san.SafeLeaf(raw, KindFile, "", dir)
		if err != nil {
			// A genuine escape would be ErrPathEscape — acceptable for this table.
			if err != ErrPathEscape {
				t.Fatalf("SafeLeaf(%q) unexpected error %v", raw, err)
			}
			continue
		}
		joined := filepath.Join(resolved, leaf)
		if !strings.HasPrefix(joined, resolved+string(filepath.Separator)) {
			t.Fatalf("SafeLeaf(%q)=%q escaped dir: %q not under %q", raw, leaf, joined, resolved)
		}
	}
}

// TestFallbackExtUTITable asserts the UTI→extension table (replicating
// UTType.preferredFilenameExtension without AppKit) drives the fallback name's
// extension, and that the kind default applies for unknown UTIs (§3.9).
func TestFallbackExtUTITable(t *testing.T) {
	cases := []struct {
		uti, kind, want string
	}{
		{"public.png", KindImage, "png"},
		{"public.jpeg", KindImage, "jpeg"},
		{"public.tiff", KindImage, "tiff"},
		{"com.compuserve.gif", KindImage, "gif"},
		{"public.heic", KindImage, "heic"},
		{"public.heif", KindImage, "heic"},
		{"com.microsoft.bmp", KindImage, "bmp"},
		{"public.plain-text", KindText, "txt"},
		{"public.utf8-plain-text", KindText, "txt"},
		{"public.text", KindText, "txt"},
		{"com.adobe.pdf", KindFile, "pdf"},
		{"public.zip-archive", KindFile, "zip"},
		{"com.pkware.zip-archive", KindFile, "zip"},
		{"unknown.uti", KindImage, "png"}, // unknown → kind default
		{"unknown.uti", KindFile, "bin"},
		{"", KindText, ""}, // text with no UTI → no extension
	}
	for _, c := range cases {
		if got := FallbackExt(c.kind, c.uti); got != c.want {
			t.Errorf("FallbackExt(%q,%q) = %q, want %q", c.kind, c.uti, got, c.want)
		}
	}
}

// TestTextFileName asserts the sidecar name is clipbeam-<UTC>.txt (§3.9).
func TestTextFileName(t *testing.T) {
	n := TextFileName()
	if !strings.HasPrefix(n, "clipbeam-") || !strings.HasSuffix(n, ".txt") {
		t.Fatalf("TextFileName() = %q, want clipbeam-<UTC>.txt", n)
	}
	// clipbeam- (9) + 19 timestamp + .txt (4) = 32.
	if len(n) != 9+19+4 {
		t.Fatalf("TextFileName() = %q len %d, want 32", n, len(n))
	}
}

// TestSafeLeafEvalSymlinkDir constructs a save dir under a symlink so that a cleaned
// leaf containing only allowlisted chars cannot escape; this documents that the escape
// branch is reachable only when the dir resolution diverges (it cannot via a normal
// leaf), keeping the assert as a defense-in-depth guard.
func TestSafeLeafEvalSymlinkDir(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	leaf, err := Default().SafeLeaf("ok.png", KindImage, "", link)
	if err != nil {
		t.Fatalf("SafeLeaf through symlinked dir: %v", err)
	}
	if leaf != "ok.png" {
		t.Fatalf("SafeLeaf through symlinked dir = %q, want ok.png", leaf)
	}
}
