package auth

import "testing"

// TestConstantTimeEqualsVectors covers the §3.12 vectors: equal→true; off-by-one→
// false; empty/empty→true; nil-vs-real→false; matching-prefix/wrong-suffix→false.
func TestConstantTimeEqualsVectors(t *testing.T) {
	s := func(v string) *string { return &v }
	cases := []struct {
		name      string
		presented *string
		expected  string
		want      bool
	}{
		{"equal", s("ABCDEF"), "ABCDEF", true},
		{"off-by-one-longer", s("ABCDEFG"), "ABCDEF", false},
		{"off-by-one-shorter", s("ABCDE"), "ABCDEF", false},
		{"empty-empty", s(""), "", true},
		{"nil-vs-real", nil, "ABCDEF", false},
		{"nil-vs-empty", nil, "", false},
		{"matching-prefix-wrong-suffix", s("ABCDEX"), "ABCDEF", false},
		{"present-empty-vs-real", s(""), "ABCDEF", false},
	}
	for _, c := range cases {
		if got := ConstantTimeEquals(c.presented, c.expected); got != c.want {
			t.Errorf("%s: ConstantTimeEquals = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestMakeTokenInvariants asserts a generated token is exactly 24 Base32 chars from
// the no-pad alphabet (120 bits), and two tokens differ (PLAN §3.12). It does not
// assert a fixed value (CSPRNG).
func TestMakeTokenInvariants(t *testing.T) {
	tok, err := MakeToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != TokenChars {
		t.Fatalf("token len = %d, want %d", len(tok), TokenChars)
	}
	for _, r := range tok {
		if !containsRune(base32Alphabet, r) {
			t.Fatalf("token char %q not in Base32 no-pad alphabet", r)
		}
	}
	tok2, _ := MakeToken()
	if tok == tok2 {
		t.Fatalf("two CSPRNG tokens collided: %q", tok)
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}
