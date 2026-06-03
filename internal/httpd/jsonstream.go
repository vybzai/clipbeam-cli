package httpd

import (
	"bufio"
	"errors"
	"io"
)

// errBadJSONString is returned when a streamed JSON string value is malformed (an
// unterminated string, a bad escape, or a lone surrogate). It maps to a 400.
var errBadJSONString = errors.New("clipbeam: malformed JSON string")

// jsonStringReader streams the UNESCAPED bytes of a single JSON string value from an
// underlying buffered reader, WITHOUT first buffering the whole (possibly ~70 MB
// base64) string in memory (PLAN §3.6). The opening quote must already have been
// consumed by the caller; Read yields unescaped UTF-8/ASCII bytes until the closing
// quote, after which it returns io.EOF and leaves the reader positioned just past the
// closing quote.
//
// It handles the JSON escapes that can appear in a base64 string value: although
// standard unwrapped base64 (the only form the Mac receiver and the Go sender emit,
// PLAN §3.6) contains none of them, a defensive decoder must still unescape \", \\,
// \/, \b, \f, \n, \r, \t and \uXXXX so a hand-crafted body cannot smuggle bytes past
// the base64 decoder. Surrogate pairs are decoded to their UTF-8 form.
type jsonStringReader struct {
	br   *bufio.Reader
	done bool
	// pending holds bytes produced by a multi-byte escape (e.g. a \uXXXX rune) that
	// did not fit the caller's buffer in one Read.
	pending []byte
}

// newJSONStringReader wraps br to stream one JSON string value. The caller must have
// consumed the opening double-quote already.
func newJSONStringReader(br *bufio.Reader) *jsonStringReader {
	return &jsonStringReader{br: br}
}

// Read implements io.Reader, emitting unescaped string bytes until the closing quote.
func (s *jsonStringReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n := 0
	// Drain any bytes pending from a previous multi-byte escape first.
	for len(s.pending) > 0 && n < len(p) {
		p[n] = s.pending[0]
		s.pending = s.pending[1:]
		n++
	}
	if n == len(p) {
		return n, nil
	}
	if s.done {
		if n > 0 {
			return n, nil
		}
		return 0, io.EOF
	}

	for n < len(p) {
		b, err := s.br.ReadByte()
		if err != nil {
			if err == io.EOF {
				return n, errBadJSONString // unterminated string
			}
			return n, err
		}
		switch b {
		case '"':
			s.done = true
			if n > 0 {
				return n, nil
			}
			return 0, io.EOF
		case '\\':
			out, perr := s.readEscape()
			if perr != nil {
				return n, perr
			}
			for _, ob := range out {
				if n < len(p) {
					p[n] = ob
					n++
				} else {
					s.pending = append(s.pending, ob)
				}
			}
		default:
			p[n] = b
			n++
		}
	}
	return n, nil
}

// readEscape consumes the byte(s) following a backslash and returns the unescaped
// output bytes.
func (s *jsonStringReader) readEscape() ([]byte, error) {
	c, err := s.br.ReadByte()
	if err != nil {
		return nil, errBadJSONString
	}
	switch c {
	case '"':
		return []byte{'"'}, nil
	case '\\':
		return []byte{'\\'}, nil
	case '/':
		return []byte{'/'}, nil
	case 'b':
		return []byte{'\b'}, nil
	case 'f':
		return []byte{'\f'}, nil
	case 'n':
		return []byte{'\n'}, nil
	case 'r':
		return []byte{'\r'}, nil
	case 't':
		return []byte{'\t'}, nil
	case 'u':
		return s.readUnicodeEscape()
	default:
		return nil, errBadJSONString
	}
}

// readUnicodeEscape decodes a \uXXXX escape (and a following \uXXXX low surrogate when
// the first is a high surrogate) into UTF-8 bytes.
func (s *jsonStringReader) readUnicodeEscape() ([]byte, error) {
	r1, err := s.readHex4()
	if err != nil {
		return nil, err
	}
	if r1 >= 0xD800 && r1 <= 0xDBFF {
		// High surrogate: a low surrogate must follow as \uXXXX.
		if b, err := s.br.ReadByte(); err != nil || b != '\\' {
			return nil, errBadJSONString
		}
		if b, err := s.br.ReadByte(); err != nil || b != 'u' {
			return nil, errBadJSONString
		}
		r2, err := s.readHex4()
		if err != nil {
			return nil, err
		}
		if r2 < 0xDC00 || r2 > 0xDFFF {
			return nil, errBadJSONString
		}
		cp := 0x10000 + ((r1 - 0xD800) << 10) + (r2 - 0xDC00)
		return encodeRune(cp), nil
	}
	if r1 >= 0xDC00 && r1 <= 0xDFFF {
		return nil, errBadJSONString // lone low surrogate
	}
	return encodeRune(r1), nil
}

// readHex4 reads exactly four hex digits and returns their integer value.
func (s *jsonStringReader) readHex4() (int, error) {
	v := 0
	for i := 0; i < 4; i++ {
		b, err := s.br.ReadByte()
		if err != nil {
			return 0, errBadJSONString
		}
		var d int
		switch {
		case b >= '0' && b <= '9':
			d = int(b - '0')
		case b >= 'a' && b <= 'f':
			d = int(b-'a') + 10
		case b >= 'A' && b <= 'F':
			d = int(b-'A') + 10
		default:
			return 0, errBadJSONString
		}
		v = v<<4 | d
	}
	return v, nil
}

// encodeRune encodes a Unicode code point as UTF-8 bytes (a tiny stdlib-free path so
// the streamer has no extra import surface).
func encodeRune(cp int) []byte {
	switch {
	case cp < 0x80:
		return []byte{byte(cp)}
	case cp < 0x800:
		return []byte{byte(0xC0 | cp>>6), byte(0x80 | cp&0x3F)}
	case cp < 0x10000:
		return []byte{byte(0xE0 | cp>>12), byte(0x80 | (cp>>6)&0x3F), byte(0x80 | cp&0x3F)}
	default:
		return []byte{
			byte(0xF0 | cp>>18),
			byte(0x80 | (cp>>12)&0x3F),
			byte(0x80 | (cp>>6)&0x3F),
			byte(0x80 | cp&0x3F),
		}
	}
}
