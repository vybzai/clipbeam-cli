package httpd

import (
	"bufio"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/ingest"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// clipDecodeBuf is the streaming copy window for the base64-in-JSON decode (PLAN
// §3.6/§3.7): the body is never read whole into RAM.
const clipDecodeBuf = 32 * 1024

// errIncompleteBody is the 400 for a body that ends before the declared/expected
// envelope is complete (Swift readBody "incomplete body", Server.swift:652).
var errIncompleteBody = errors.New("clipbeam: incomplete body")

// errUnsupportedEnvelope is the 400 for version!=1 or zero items (Swift dispatchClip
// "unsupported envelope", Server.swift:696).
var errUnsupportedEnvelope = errors.New("clipbeam: unsupported envelope")

// handleClip serves POST /clip in the MANDATED gate ORDER (PLAN §3.3, §3.11):
// class (Tier-1 exact-peer) → constant-time token → Content-Length present (411) →
// declared ≤ HardCeiling (413, before reading) → single-flight (503, taken LAST so
// early returns need no release). It then streams the base64-in-JSON body straight to
// per-item temp files via base64.NewDecoder (never read-all, PLAN §3.6) and hands them
// to ingest.Ingest. It NEVER returns 502 on this surface (PLAN §3.11).
func (s *Server) handleClip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// 1) class → 2) token (constant-time).
	if status, reason := s.clipGate(r); status != 0 {
		s.writeError(w, status, reason)
		return
	}

	// 3) Content-Length MUST be present (PLAN §3.3) → else 411.
	if r.ContentLength < 0 {
		s.writeError(w, http.StatusLengthRequired, "length required")
		return
	}

	// 4) Declared length over the raw-wire ceiling → 413 BEFORE reading the body
	// (PLAN §3.8). The ceiling is maxBytes*4/3 + 64KB (base64-JSON wire, integer
	// multiply-first).
	ceiling := wire.HardCeiling(s.maxBytes())
	if r.ContentLength > int64(ceiling) {
		s.write413(w, r)
		return
	}

	// 5) Single-flight gate, taken LAST so the early returns above need no release
	// (PLAN §3.11). Released on EVERY teardown path via defer.
	if !s.single.tryAcquire() {
		s.writeError(w, http.StatusServiceUnavailable, "busy")
		return
	}
	defer s.single.release()

	// Running raw-byte enforcement against the ceiling regardless of Content-Length:
	// MaxBytesReader trips at ceiling+1 and we map its error to our EXPLICIT 413, not
	// MaxBytesReader's default 413 text (PLAN §3.8c, §3.11).
	body := http.MaxBytesReader(w, r.Body, int64(ceiling))

	resp, err := s.decodeAndIngest(body)
	if err != nil {
		s.writeClipError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// decodeAndIngest token-walks the Envelope JSON, streaming each item's bytesB64 value
// through base64.NewDecoder into a per-item temp file (bounded memory, PLAN §3.6),
// collecting the small metadata fields, then hands the items to ingest.Ingest. The
// temp files are removed after Ingest copies them into their final destinations. It
// returns ErrTooLarge/ErrMalformed/ErrPathEscape sentinels (mapped to status by
// writeClipError) or a store/errno error surfaced as a 500 diagnostic.
func (s *Server) decodeAndIngest(body io.Reader) (wire.ClipResponse, error) {
	br := bufio.NewReaderSize(body, clipDecodeBuf)
	env, tmpFiles, err := s.parseEnvelopeStreaming(br)
	// Always clean up the per-item temp payload files, success or failure.
	defer func() {
		for _, f := range tmpFiles {
			_ = os.Remove(f)
		}
	}()
	if err != nil {
		return wire.ClipResponse{}, err
	}

	if env.version != 1 || len(env.items) == 0 {
		return wire.ClipResponse{}, errUnsupportedEnvelope
	}

	channel := env.channel // "" | "clipboard" | "agent"
	items := make([]ingest.IngestItem, 0, len(env.items))
	openable := make([]*os.File, 0, len(env.items))
	defer func() {
		for _, f := range openable {
			_ = f.Close()
		}
	}()
	for _, pi := range env.items {
		it := ingest.IngestItem{
			Kind:    pi.kind,
			Name:    pi.name,
			Uti:     pi.uti,
			Mime:    pi.mime,
			Channel: channel,
		}
		switch pi.kind {
		case ingest.KindText:
			it.Reader = stringReader(pi.text)
		default:
			// image/file: stream the decoded temp file into Ingest.
			f, oerr := os.Open(pi.tmpPath)
			if oerr != nil {
				return wire.ClipResponse{}, ingest.ErrMalformed
			}
			openable = append(openable, f)
			it.Reader = f
		}
		items = append(items, it)
	}

	return ingest.Ingest(items, s.opts.classOf(), s.opts.Store, s.maxBytes())
}

// parsedItem is the small per-item metadata gathered during the streaming parse, plus
// the temp file path holding the decoded image/file payload (empty for text).
type parsedItem struct {
	kind    string
	name    string
	uti     string
	mime    string
	text    string
	tmpPath string
}

// parsedEnvelope is the structural result of the streaming parse: the small scalar
// fields plus the per-item metadata (payloads live in temp files referenced by
// parsedItem.tmpPath, never in RAM).
type parsedEnvelope struct {
	version int
	channel string
	items   []parsedItem
}

// parseEnvelopeStreaming walks the top-level Envelope object with a forward,
// bounded-memory parser. Scalars (version, sender, channel) and per-item metadata are
// read into memory (all tiny); each item's bytesB64 value is streamed through
// base64.NewDecoder into a 0600 temp file under the save dir so it is never buffered
// whole (PLAN §3.6). It returns the parsed structure and the list of temp files to
// clean up (populated even on a mid-parse error so the caller's defer removes them).
func (s *Server) parseEnvelopeStreaming(br *bufio.Reader) (parsedEnvelope, []string, error) {
	var env parsedEnvelope
	var tmps []string

	if err := expectByte(br, '{'); err != nil {
		return env, tmps, err
	}
	first := true
	for {
		if err := skipWS(br); err != nil {
			return env, tmps, err
		}
		b, err := br.ReadByte()
		if err != nil {
			return env, tmps, errIncompleteBody
		}
		if b == '}' {
			break
		}
		if !first {
			if b != ',' {
				return env, tmps, ingest.ErrMalformed
			}
			if err := skipWS(br); err != nil {
				return env, tmps, err
			}
			b, err = br.ReadByte()
			if err != nil {
				return env, tmps, errIncompleteBody
			}
		}
		first = false
		if b != '"' {
			return env, tmps, ingest.ErrMalformed
		}
		key, err := readShortJSONString(br)
		if err != nil {
			return env, tmps, err
		}
		if err := skipWSThen(br, ':'); err != nil {
			return env, tmps, err
		}
		switch key {
		case "version":
			n, err := readJSONNumber(br)
			if err != nil {
				return env, tmps, err
			}
			env.version = n
		case "sender":
			if _, err := readJSONStringValue(br); err != nil {
				return env, tmps, err
			}
		case "channel":
			v, isNull, err := readJSONStringOrNull(br)
			if err != nil {
				return env, tmps, err
			}
			if !isNull {
				env.channel = v
			}
		case "items":
			items, itemTmps, err := s.parseItemsStreaming(br)
			tmps = append(tmps, itemTmps...)
			if err != nil {
				return env, tmps, err
			}
			env.items = items
		default:
			// Tolerate unknown top-level keys: skip the value structurally.
			if err := skipJSONValue(br); err != nil {
				return env, tmps, err
			}
		}
	}
	return env, tmps, nil
}

// parseItemsStreaming walks the items array, streaming each item's bytesB64 into a
// temp file. It returns the per-item metadata and the temp-file paths.
func (s *Server) parseItemsStreaming(br *bufio.Reader) ([]parsedItem, []string, error) {
	var items []parsedItem
	var tmps []string

	if err := skipWSThen(br, '['); err != nil {
		return items, tmps, err
	}
	if err := skipWS(br); err != nil {
		return items, tmps, err
	}
	// Empty array?
	if b, err := br.ReadByte(); err != nil {
		return items, tmps, errIncompleteBody
	} else if b == ']' {
		return items, tmps, nil
	} else if err := br.UnreadByte(); err != nil {
		return items, tmps, err
	}

	for {
		pi, tmp, err := s.parseOneItem(br)
		if tmp != "" {
			tmps = append(tmps, tmp)
		}
		if err != nil {
			return items, tmps, err
		}
		items = append(items, pi)

		if err := skipWS(br); err != nil {
			return items, tmps, err
		}
		b, err := br.ReadByte()
		if err != nil {
			return items, tmps, errIncompleteBody
		}
		if b == ']' {
			break
		}
		if b != ',' {
			return items, tmps, ingest.ErrMalformed
		}
	}
	return items, tmps, nil
}

// parseOneItem parses one item object, streaming its bytesB64 (if present) into a
// 0600 temp file under the save dir. It returns the metadata, the temp path (empty if
// no bytesB64), and any error.
func (s *Server) parseOneItem(br *bufio.Reader) (parsedItem, string, error) {
	var pi parsedItem
	if err := skipWSThen(br, '{'); err != nil {
		return pi, "", err
	}
	first := true
	for {
		if err := skipWS(br); err != nil {
			return pi, pi.tmpPath, err
		}
		b, err := br.ReadByte()
		if err != nil {
			return pi, pi.tmpPath, errIncompleteBody
		}
		if b == '}' {
			break
		}
		if !first {
			if b != ',' {
				return pi, pi.tmpPath, ingest.ErrMalformed
			}
			if err := skipWS(br); err != nil {
				return pi, pi.tmpPath, err
			}
			b, err = br.ReadByte()
			if err != nil {
				return pi, pi.tmpPath, errIncompleteBody
			}
		}
		first = false
		if b != '"' {
			return pi, pi.tmpPath, ingest.ErrMalformed
		}
		key, err := readShortJSONString(br)
		if err != nil {
			return pi, pi.tmpPath, err
		}
		if err := skipWSThen(br, ':'); err != nil {
			return pi, pi.tmpPath, err
		}
		switch key {
		case "kind":
			v, err := readJSONStringValue(br)
			if err != nil {
				return pi, pi.tmpPath, err
			}
			pi.kind = v
		case "name":
			v, isNull, err := readJSONStringOrNull(br)
			if err != nil {
				return pi, pi.tmpPath, err
			}
			if !isNull {
				pi.name = v
			}
		case "uti":
			v, isNull, err := readJSONStringOrNull(br)
			if err != nil {
				return pi, pi.tmpPath, err
			}
			if !isNull {
				pi.uti = v
			}
		case "mime":
			v, isNull, err := readJSONStringOrNull(br)
			if err != nil {
				return pi, pi.tmpPath, err
			}
			if !isNull {
				pi.mime = v
			}
		case "text":
			v, isNull, err := readJSONStringOrNull(br)
			if err != nil {
				return pi, pi.tmpPath, err
			}
			if !isNull {
				pi.text = v
			}
		case "bytesB64":
			// Null bytesB64 → treat as absent. Otherwise stream-decode to a temp file.
			if null, err := tryConsumeNull(br); err != nil {
				return pi, pi.tmpPath, err
			} else if null {
				continue
			}
			if err := skipWSThen(br, '"'); err != nil {
				return pi, pi.tmpPath, err
			}
			tmp, err := s.streamBase64ToTemp(br)
			pi.tmpPath = tmp
			if err != nil {
				return pi, pi.tmpPath, err
			}
		default:
			if err := skipJSONValue(br); err != nil {
				return pi, pi.tmpPath, err
			}
		}
	}
	return pi, pi.tmpPath, nil
}

// streamBase64ToTemp streams the JSON string value (opening quote already consumed)
// through base64.NewDecoder into a 0600 temp file under the save dir via a 32 KB copy
// window (PLAN §3.6/§3.7). It returns the temp file path. The cap is NOT re-checked
// here — MaxBytesReader already bounds the raw wire and Ingest re-checks the decoded
// sum per item (PLAN §3.8c/§3.8d).
func (s *Server) streamBase64ToTemp(br *bufio.Reader) (string, error) {
	dir, err := s.tempDir()
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".clipbeam-clip-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	_ = f.Chmod(0o600)

	jsr := newJSONStringReader(br)
	dec := base64.NewDecoder(base64.StdEncoding, jsr)
	buf := make([]byte, clipDecodeBuf)
	if _, err := io.CopyBuffer(f, dec, buf); err != nil {
		_ = f.Close()
		// A base64 error or a malformed JSON string is a 400; a write error
		// (e.g. ENOSPC) surfaces as a 500 diagnostic via Ingest's path. Map decode
		// errors to ErrMalformed.
		if errors.Is(err, errBadJSONString) {
			return path, ingest.ErrMalformed
		}
		if _, ok := err.(base64.CorruptInputError); ok {
			return path, ingest.ErrMalformed
		}
		return path, err
	}
	if err := f.Close(); err != nil {
		return path, err
	}
	return path, nil
}

// write413 writes the explicit 413 status+body and then reads-and-discards a bounded
// remainder of the body so a mid-upload client reads the 413 instead of an RST — the
// DEFAULT mechanism, not a contingency (PLAN §3.11). The drain is bounded by BOTH a
// byte budget (≈64 KB) AND a short read deadline so a client that stops sending (but
// keeps the connection open) cannot pin the goroutine: the deadline fires, the drain
// returns, the handler returns, and net/http flushes the buffered 413.
func (s *Server) write413(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
	drainBounded(w, r)
}

// drainBounded reads and discards up to ≈64 KB of r.Body under a short read deadline
// so the client can read our buffered status before the connection closes (PLAN
// §3.11). The deadline is set via http.ResponseController so a stalled client trips it
// rather than blocking the flush; any deadline/error simply ends the drain.
func drainBounded(w http.ResponseWriter, r *http.Request) {
	if r.Body == nil {
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, _ = io.CopyN(io.Discard, r.Body, 64*1024)
	// Clear the deadline best-effort (the connection is closing anyway).
	_ = rc.SetReadDeadline(time.Time{})
}

// stringReader returns an io.Reader over s (text item payload). A separate helper so
// the import set stays minimal.
func stringReader(s string) io.Reader {
	return &byteReader{b: []byte(s)}
}

// byteReader is a tiny io.Reader over a byte slice (text payloads are small).
type byteReader struct {
	b []byte
	i int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

// --- tiny forward JSON helpers (bounded-memory; only bytesB64 is streamed) ---

// expectByte reads the next non-whitespace byte and asserts it equals want.
func expectByte(br *bufio.Reader, want byte) error {
	if err := skipWS(br); err != nil {
		return err
	}
	b, err := br.ReadByte()
	if err != nil {
		return errIncompleteBody
	}
	if b != want {
		return ingest.ErrMalformed
	}
	return nil
}

// skipWS consumes leading JSON whitespace, leaving the next significant byte unread.
func skipWS(br *bufio.Reader) error {
	for {
		b, err := br.ReadByte()
		if err != nil {
			if err == io.EOF {
				return errIncompleteBody
			}
			return err
		}
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return br.UnreadByte()
		}
	}
}

// skipWSThen skips whitespace then asserts the next byte equals want.
func skipWSThen(br *bufio.Reader, want byte) error {
	if err := skipWS(br); err != nil {
		return err
	}
	b, err := br.ReadByte()
	if err != nil {
		return errIncompleteBody
	}
	if b != want {
		return ingest.ErrMalformed
	}
	return nil
}

// readShortJSONString reads a JSON string value fully into memory (opening quote
// already consumed). Used ONLY for keys and small scalar values — never bytesB64.
func readShortJSONString(br *bufio.Reader) (string, error) {
	jsr := newJSONStringReader(br)
	b, err := io.ReadAll(jsr)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// readJSONStringValue skips whitespace, asserts an opening quote, and reads a short
// string value fully.
func readJSONStringValue(br *bufio.Reader) (string, error) {
	if err := skipWSThen(br, '"'); err != nil {
		return "", err
	}
	return readShortJSONString(br)
}

// readJSONStringOrNull reads a string value OR a JSON null (pointer-optional decode,
// PLAN §3.5). Returns (value,false,nil) for a string or ("",true,nil) for null.
func readJSONStringOrNull(br *bufio.Reader) (string, bool, error) {
	if null, err := tryConsumeNull(br); err != nil {
		return "", false, err
	} else if null {
		return "", true, nil
	}
	v, err := readJSONStringValue(br)
	return v, false, err
}

// tryConsumeNull skips whitespace and, if the next token is the literal null,
// consumes it and returns true; otherwise it leaves the reader positioned at the next
// significant byte and returns false.
func tryConsumeNull(br *bufio.Reader) (bool, error) {
	if err := skipWS(br); err != nil {
		return false, err
	}
	b, err := br.ReadByte()
	if err != nil {
		return false, errIncompleteBody
	}
	if b != 'n' {
		return false, br.UnreadByte()
	}
	for _, want := range []byte{'u', 'l', 'l'} {
		c, err := br.ReadByte()
		if err != nil || c != want {
			return false, ingest.ErrMalformed
		}
	}
	return true, nil
}

// readJSONNumber reads a JSON integer (the envelope version is a bare integer).
func readJSONNumber(br *bufio.Reader) (int, error) {
	if err := skipWS(br); err != nil {
		return 0, err
	}
	var digits []byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			if err == io.EOF {
				break
			}
			return 0, err
		}
		if (b >= '0' && b <= '9') || b == '-' || b == '+' {
			digits = append(digits, b)
			continue
		}
		// Stop at the first non-numeric byte; tolerate fractional/exponent forms by
		// reading them but parsing only the integer part defensively.
		if b == '.' || b == 'e' || b == 'E' {
			// Consume the rest of the numeric token without keeping it.
			for {
				nb, nerr := br.ReadByte()
				if nerr != nil {
					break
				}
				if (nb >= '0' && nb <= '9') || nb == '+' || nb == '-' {
					continue
				}
				_ = br.UnreadByte()
				break
			}
			break
		}
		_ = br.UnreadByte()
		break
	}
	if len(digits) == 0 {
		return 0, ingest.ErrMalformed
	}
	n, err := strconv.Atoi(string(digits))
	if err != nil {
		return 0, ingest.ErrMalformed
	}
	return n, nil
}

// skipJSONValue consumes one complete JSON value (object/array/string/number/literal)
// so unknown keys are tolerated without buffering large payloads in fields we do not
// stream. It recurses through nested structures.
func skipJSONValue(br *bufio.Reader) error {
	if err := skipWS(br); err != nil {
		return err
	}
	b, err := br.ReadByte()
	if err != nil {
		return errIncompleteBody
	}
	switch b {
	case '"':
		// Drain a string value (discard the bytes; bounded by the wire ceiling).
		jsr := newJSONStringReader(br)
		_, derr := io.Copy(io.Discard, jsr)
		return derr
	case '{':
		return skipContainer(br, '}')
	case '[':
		return skipContainer(br, ']')
	case 't', 'f', 'n':
		// true / false / null — consume the rest of the literal letters.
		for {
			c, err := br.ReadByte()
			if err != nil {
				return nil
			}
			if c < 'a' || c > 'z' {
				return br.UnreadByte()
			}
		}
	default:
		// number — consume numeric bytes.
		for {
			c, err := br.ReadByte()
			if err != nil {
				return nil
			}
			if (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
				continue
			}
			return br.UnreadByte()
		}
	}
}

// skipContainer consumes the remainder of an object/array (the opening brace/bracket
// already consumed) up to and including the matching close, honoring nested strings
// and containers.
func skipContainer(br *bufio.Reader, close byte) error {
	for {
		if err := skipWS(br); err != nil {
			return err
		}
		b, err := br.ReadByte()
		if err != nil {
			return errIncompleteBody
		}
		if b == close {
			return nil
		}
		if b == ',' || b == ':' {
			continue
		}
		if err := br.UnreadByte(); err != nil {
			return err
		}
		if err := skipJSONValue(br); err != nil {
			return err
		}
	}
}
