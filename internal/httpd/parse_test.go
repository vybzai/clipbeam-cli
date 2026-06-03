package httpd

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

// readJSONString is a tiny helper: it consumes the opening quote then drains the
// jsonStringReader fully, returning the unescaped bytes (mirrors how the streaming
// decoder reads a string value).
func readJSONString(t *testing.T, s string) (string, error) {
	t.Helper()
	br := bufio.NewReader(strings.NewReader(s))
	q, err := br.ReadByte()
	if err != nil || q != '"' {
		t.Fatalf("test input must start with a quote, got %q", s)
	}
	jsr := newJSONStringReader(br)
	out, err := io.ReadAll(jsr)
	return string(out), err
}

// TestJSONStringReaderEscapes drives readEscape / readUnicodeEscape / readHex4 /
// encodeRune across every escape the defensive decoder must handle (PLAN §3.6): the
// simple two-char escapes, a \uXXXX BMP rune, a surrogate PAIR (astral plane), and the
// Swift \/ form. These exercise the escape machinery directly (a unit test of the
// hand-rolled streamer is more reliable than smuggling escapes through the full stack).
func TestJSONStringReaderEscapes(t *testing.T) {
	cases := []struct {
		name string
		in   string // includes the opening quote; the reader stops at the closing quote
		want string
	}{
		{"plain", `"hello"`, "hello"},
		{"escaped-quote", `"a\"b"`, `a"b`},
		{"escaped-backslash", `"a\\b"`, `a\b`},
		{"escaped-slash (Swift form)", `"image\/png"`, "image/png"},
		{"control escapes", `"\b\f\n\r\t"`, "\b\f\n\r\t"},
		// \uXXXX escapes: the input carries LITERAL backslash-u sequences (built with a
		// raw backtick string), driving readUnicodeEscape / readHex4 / encodeRune.
		{`u-escape ASCII`, "\"\\u0041\\u0042\"", "AB"},             // A,B
		{`u-escape 2-byte`, "\"a\\u00e9b\"", "aéb"},                // é (C3 A9)
		{`u-escape 3-byte`, "\"\\u20ac\"", "€"},                    // € (E2 82 AC)
		{`u-escape boundary`, "\"\\u00ff\"", "ÿ"},                  // ÿ (C3 BF)
		{`u-escape surrogate pair`, "\"x\\ud83d\\ude00y\"", "x😀y"}, // 😀 (astral, F0 9F 98 80)
		{`u-escape uppercase hex`, "\"\\u00E9\"", "é"},             // hex A-F branch
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := readJSONString(t, c.in)
			if err != nil {
				t.Fatalf("decode %q: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("decoded %q → %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestJSONStringReaderErrors drives the error branches of the streamer: an unterminated
// string, a bad escape letter, a short \u (EOF mid-hex), a non-hex digit, a lone low
// surrogate, and a high surrogate not followed by a low surrogate. Each must return
// errBadJSONString, never panic (PLAN §3.6/§12.3).
func TestJSONStringReaderErrors(t *testing.T) {
	for _, in := range []string{
		`"unterminated`,  // no closing quote
		`"bad\xescape"`,  // \x is not a valid escape
		`"\u12"`,         // short \u (hits the closing quote mid-hex)
		`"\uZZZZ"`,       // non-hex digits
		`"\uDC00"`,       // lone low surrogate
		`"\uD83Dnotlow"`, // high surrogate without a following \u low surrogate
		`"\uD83DA"`,      // high surrogate followed by a NON-surrogate \u
	} {
		t.Run(in, func(t *testing.T) {
			_, err := readJSONString(t, in)
			if !errors.Is(err, errBadJSONString) {
				t.Fatalf("decode %q err = %v, want errBadJSONString", in, err)
			}
		})
	}
}

// TestJSONStringReaderPendingDrain drives the multi-byte-escape pending path in Read: a
// surrogate-pair emoji (4 UTF-8 bytes) read through a 1-byte buffer must be delivered
// byte-by-byte via s.pending without loss.
func TestJSONStringReaderPendingDrain(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(`"😀"`))
	_, _ = br.ReadByte() // opening quote
	jsr := newJSONStringReader(br)
	var got []byte
	buf := make([]byte, 1) // force one byte per Read so pending must buffer
	for {
		n, err := jsr.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	if string(got) != "😀" {
		t.Fatalf("byte-wise read = %q, want the emoji", got)
	}
}

// TestReadJSONNumber drives readJSONNumber across the integer, fractional (defensive
// consume), and error (no digits) cases.
func TestReadJSONNumber(t *testing.T) {
	mk := func(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

	if n, err := readJSONNumber(mk("1}")); err != nil || n != 1 {
		t.Fatalf("readJSONNumber(\"1\") = %d,%v, want 1,nil", n, err)
	}
	if n, err := readJSONNumber(mk("42,")); err != nil || n != 42 {
		t.Fatalf("readJSONNumber(\"42\") = %d,%v, want 42,nil", n, err)
	}
	// Fractional/exponent: the integer part is parsed, the rest consumed defensively.
	if n, err := readJSONNumber(mk("1.5}")); err != nil || n != 1 {
		t.Fatalf("readJSONNumber(\"1.5\") = %d,%v, want 1,nil", n, err)
	}
	if n, err := readJSONNumber(mk("2e3}")); err != nil || n != 2 {
		t.Fatalf("readJSONNumber(\"2e3\") = %d,%v, want 2,nil", n, err)
	}
	// No digits → ErrMalformed.
	if _, err := readJSONNumber(mk("}")); err == nil {
		t.Fatal("readJSONNumber with no digits must error")
	}
}

// TestSkipJSONValue drives skipJSONValue / skipContainer over each value shape so the
// unknown-key tolerance path is fully covered: a string (with an escape), a nested
// object, a nested array, the three literals, and a number.
func TestSkipJSONValue(t *testing.T) {
	for _, v := range []string{
		`"a string with a \" escape"`,
		`{"nested":{"deep":[1,2,3]},"k":"v"}`,
		`[1,"two",{"x":[true,false,null]}]`,
		`true`,
		`false`,
		`null`,
		`-12.5e9`,
	} {
		t.Run(v, func(t *testing.T) {
			// Append a trailing sentinel so skipJSONValue must STOP at the value's end.
			br := bufio.NewReader(strings.NewReader(v + ",SENTINEL"))
			if err := skipJSONValue(br); err != nil {
				t.Fatalf("skipJSONValue(%q): %v", v, err)
			}
			// The next significant byte must be the comma (the value was fully consumed).
			if err := skipWS(br); err != nil {
				t.Fatalf("skipWS after value: %v", err)
			}
			b, _ := br.ReadByte()
			if b != ',' {
				t.Fatalf("after skipping %q, next byte = %q, want ','", v, b)
			}
		})
	}
}

// TestExpectByteAndSkipWSThen drives expectByte / skipWSThen success + mismatch + EOF.
func TestExpectByteAndSkipWSThen(t *testing.T) {
	mk := func(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

	if err := expectByte(mk("  {"), '{'); err != nil {
		t.Fatalf("expectByte WS+{ : %v", err)
	}
	if err := expectByte(mk("x"), '{'); err == nil {
		t.Fatal("expectByte mismatch must error")
	}
	if err := expectByte(mk(""), '{'); err == nil {
		t.Fatal("expectByte EOF must error")
	}
	if err := skipWSThen(mk("\t\n["), '['); err != nil {
		t.Fatalf("skipWSThen WS+[ : %v", err)
	}
	if err := skipWSThen(mk("]"), '['); err == nil {
		t.Fatal("skipWSThen mismatch must error")
	}
}

// TestTryConsumeNull drives tryConsumeNull: a real null, a non-null token (left unread),
// and a malformed n-prefix.
func TestTryConsumeNull(t *testing.T) {
	mk := func(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

	if ok, err := tryConsumeNull(mk("null")); err != nil || !ok {
		t.Fatalf("tryConsumeNull(null) = %v,%v, want true,nil", ok, err)
	}
	br := mk(`"x"`)
	if ok, err := tryConsumeNull(br); err != nil || ok {
		t.Fatalf("tryConsumeNull(\"x\") = %v,%v, want false,nil", ok, err)
	}
	// The non-null token must be left unread (the next ReadByte sees the quote).
	if b, _ := br.ReadByte(); b != '"' {
		t.Fatalf("tryConsumeNull must leave a non-null token unread, next = %q", b)
	}
	if _, err := tryConsumeNull(mk("nope")); err == nil {
		t.Fatal("tryConsumeNull with a malformed n-prefix must error")
	}
}
