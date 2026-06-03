package wire

import "testing"

// TestHardCeilingLiteral asserts the EXACT raw-wire ceiling for the default
// maxBytes: (52428800*4)/3 + 65536 = 69905066 + 65536 = 69970602 (PLAN §3.8). A
// drift here accepts/rejects a payload the Mac app would reject/accept.
func TestHardCeilingLiteral(t *testing.T) {
	const want = 69970602
	if got := HardCeiling(DefaultMaxBytes); got != want {
		t.Fatalf("HardCeiling(%d) = %d, want %d", DefaultMaxBytes, got, want)
	}
}

// TestHardCeilingMultiplyFirst guards the multiply-first integer truncation: writing
// the formula as maxBytes/3*4 would truncate differently (PLAN §3.8 trap).
func TestHardCeilingMultiplyFirst(t *testing.T) {
	const maxBytes = 52_428_800
	multiplyFirst := maxBytes*4/3 + 64*1024
	divideFirst := maxBytes/3*4 + 64*1024
	if multiplyFirst == divideFirst {
		t.Skip("no truncation divergence for this maxBytes; formula order still asserted by HardCeiling literal test")
	}
	if HardCeiling(maxBytes) != multiplyFirst {
		t.Fatalf("HardCeiling must use multiply-first; got %d want %d", HardCeiling(maxBytes), multiplyFirst)
	}
}

// TestDecodedLength replicates the Swift decodedLength(ofBase64:) recipe: chars =
// non-pad non-whitespace; pad = '='; groups=(chars+pad)/4; decoded=groups*3-pad, ≥0.
func TestDecodedLength(t *testing.T) {
	cases := []struct {
		b64  string
		want int
	}{
		{"", 0},
		{"QQ==", 1},      // "A"
		{"QUI=", 2},      // "AB"
		{"QUJD", 3},      // "ABC"
		{"QUJDRA==", 4},  // "ABCD"
		{"QU\nJD", 3},    // whitespace ignored
		{"=", 0},         // clamp ≥ 0
	}
	for _, c := range cases {
		if got := DecodedLength(c.b64); got != c.want {
			t.Errorf("DecodedLength(%q) = %d, want %d", c.b64, got, c.want)
		}
	}
}
