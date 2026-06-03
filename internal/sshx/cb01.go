package sshx

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"os"
)

// CB01 is the internal raw-bytes SSH framing (PLAN §5.1, §3.6). It is
// clipbeam↔clipbeam-only, NEVER on /clip, and is decoded into the SAME Ingest
// per-item io.Reader before any save. It is documented as internal-only and
// explicitly NOT Envelope v1 so no contributor mistakes it for the frozen wire
// (PLAN §10.4). Wire layout:
//
//	"CB01"   4-byte magic
//	channel  1 byte   (0=clipboard, 1=agent — the wire's nil/"clipboard" collapses
//	                  to 0; Swift treats nil==clipboard, Models.swift:25-30)
//	count    uvarint  (>=1)
//	repeat count×:
//	  kind   1 byte   (0=image, 1=file, 2=text)
//	  name   uvarint-len + UTF-8 bytes (empty allowed)
//	  uti    uvarint-len + UTF-8 bytes (empty allowed)
//	  mime   uvarint-len + UTF-8 bytes (empty allowed)
//	  payload uvarint-len + RAW bytes (file/image bytes, or UTF-8 text)
//
// Cap semantics (PLAN §5.1): for a raw CB01 frame raw==decoded, so the cap is
// decoded-sum ≤ maxBytes DIRECTLY — do NOT reuse the base64-inflated /clip ceiling.
const cb01Magic = "CB01"

// CB01 channel bytes.
const (
	cb01ChannelClipboard byte = 0
	cb01ChannelAgent     byte = 1
)

// CB01 kind bytes.
const (
	cb01KindImage byte = 0
	cb01KindFile  byte = 1
	cb01KindText  byte = 2
)

// cb01MaxStringLen bounds the metadata-string length prefixes (name/uti/mime) so a
// hostile or corrupt frame cannot ask the decoder to allocate an unbounded buffer
// before any payload streaming begins. 64 KiB is far above any real filename/UTI/MIME
// and matches the frozen 64 KiB header ceiling spirit (PLAN §3.3).
const cb01MaxStringLen = 64 * 1024

// CB01Item is one item in a CB01 frame, carrying full metadata so agent-channel +
// filename fidelity survive the daemonless path (PLAN §5.1). The buffered Payload form
// is used by the sender's metadata-light text/agent frames; large file/image items use
// EncodeCB01Streaming so a 50 MB item is never held whole in RAM (PLAN §3.7).
type CB01Item struct {
	Kind    byte
	Name    string
	Uti     string
	Mime    string
	Payload []byte
}

// ErrBadFrame is returned for a malformed CB01 frame (bad magic, truncated, count<1,
// an over-long string prefix, or a short payload read).
var ErrBadFrame = errors.New("clipbeam: bad CB01 frame")

// EncodeCB01 writes a CB01 frame to w. channel is the collapsed channel byte; items
// carry their metadata + raw (buffered) payload. The sender builds this and streams it
// to the remote `clipbeam ingest` stdin (PLAN §5.1).
func EncodeCB01(w io.Writer, channel byte, items []CB01Item) error {
	streamItems := make([]CB01ItemReader, len(items))
	for i, it := range items {
		streamItems[i] = CB01ItemReader{
			Kind:        it.Kind,
			Name:        it.Name,
			Uti:         it.Uti,
			Mime:        it.Mime,
			Payload:     bytesReader(it.Payload),
			PayloadSize: int64(len(it.Payload)),
		}
	}
	return EncodeCB01Streaming(w, channel, streamItems)
}

// EncodeCB01Streaming writes a CB01 frame to w using per-item streamed payloads. Each
// item's payload is copied via io.CopyN with its declared PayloadSize as the
// length-prefix, so a large file/image item is never held whole in RAM on the send
// side (PLAN §3.7). The caller must set PayloadSize to the exact byte count the Payload
// reader will yield.
func EncodeCB01Streaming(w io.Writer, channel byte, items []CB01ItemReader) error {
	if len(items) == 0 {
		return ErrBadFrame
	}
	bw := bufio.NewWriter(w)
	if _, err := bw.WriteString(cb01Magic); err != nil {
		return err
	}
	if err := bw.WriteByte(channel); err != nil {
		return err
	}
	if err := writeUvarint(bw, uint64(len(items))); err != nil {
		return err
	}
	for _, it := range items {
		if err := bw.WriteByte(it.Kind); err != nil {
			return err
		}
		if err := writeString(bw, it.Name); err != nil {
			return err
		}
		if err := writeString(bw, it.Uti); err != nil {
			return err
		}
		if err := writeString(bw, it.Mime); err != nil {
			return err
		}
		if err := writeUvarint(bw, uint64(it.PayloadSize)); err != nil {
			return err
		}
		if it.PayloadSize > 0 {
			if it.Payload == nil {
				return ErrBadFrame
			}
			n, err := io.CopyN(bw, it.Payload, it.PayloadSize)
			if err != nil {
				return err
			}
			if n != it.PayloadSize {
				return ErrBadFrame
			}
		}
	}
	return bw.Flush()
}

// DecodeCB01 reads a CB01 frame from r and returns the channel byte plus a slice of
// per-item readers feeding Ingest directly (PLAN §5.1, §3.7). Each item's metadata
// (kind/name/uti/mime) is populated up front; its payload is exposed as a bounded
// io.Reader. The returned readers MUST be consumed strictly in order.
//
// The wire interleaves metadata and payload per item (metadata of item N+1 follows
// payload N), so to populate every item's metadata before the caller reads any payload
// the decoder reads each item in order: the LAST item's payload is a direct bounded
// reader over r (zero copy — the overwhelmingly common count==1 path streams the whole
// payload straight from r to disk), while each EARLIER item's payload is spilled to a
// bounded temp file via io.CopyN (32 KiB window) so its bytes survive past the point
// where item N+1's metadata is read. A payload is therefore NEVER read whole into a
// single in-memory []byte; bounded memory holds (PLAN §3.7). Temp spills auto-remove
// on close/full-read.
func DecodeCB01(r io.Reader) (channel byte, items []CB01ItemReader, err error) {
	br := asByteReader(r)

	magic := make([]byte, len(cb01Magic))
	if _, err := io.ReadFull(br, magic); err != nil {
		return 0, nil, ErrBadFrame
	}
	if string(magic) != cb01Magic {
		return 0, nil, ErrBadFrame
	}

	ch, err := br.ReadByte()
	if err != nil {
		return 0, nil, ErrBadFrame
	}

	count, err := binary.ReadUvarint(br)
	if err != nil || count < 1 {
		return 0, nil, ErrBadFrame
	}

	items = make([]CB01ItemReader, 0, count)
	for i := 0; i < int(count); i++ {
		last := i == int(count)-1
		it, perr := decodeItem(br, last)
		if perr != nil {
			// Clean up any temp spills already created for earlier items.
			for _, prev := range items {
				if sp, ok := prev.Payload.(*spillReader); ok {
					_ = sp.Close()
				}
			}
			return 0, nil, perr
		}
		items = append(items, it)
	}
	return ch, items, nil
}

// decodeItem reads one item's metadata and its payload reader. For the last item the
// payload is a direct bounded reader over br (zero copy); for an earlier item it is
// spilled to a bounded temp file so the bytes survive while later items' metadata is
// read off the same forward-only stream.
func decodeItem(br byteReader, last bool) (CB01ItemReader, error) {
	kind, err := br.ReadByte()
	if err != nil {
		return CB01ItemReader{}, ErrBadFrame
	}
	name, err := readString(br)
	if err != nil {
		return CB01ItemReader{}, err
	}
	uti, err := readString(br)
	if err != nil {
		return CB01ItemReader{}, err
	}
	mime, err := readString(br)
	if err != nil {
		return CB01ItemReader{}, err
	}
	plen, err := binary.ReadUvarint(br)
	if err != nil {
		return CB01ItemReader{}, ErrBadFrame
	}

	it := CB01ItemReader{Kind: kind, Name: name, Uti: uti, Mime: mime, PayloadSize: int64(plen)}
	if last {
		// The last item streams its payload directly from the wire (no spill).
		it.Payload = &guardedReader{r: io.LimitReader(br, int64(plen)), remaining: int64(plen)}
		return it, nil
	}
	// An earlier item is spilled to a bounded temp file via io.CopyN so its payload
	// survives reading item N+1's metadata; never held whole in memory (PLAN §3.7).
	sp, serr := spillPayload(br, int64(plen))
	if serr != nil {
		return CB01ItemReader{}, serr
	}
	it.Payload = sp
	return it, nil
}

// CB01ItemReader exposes one decoded CB01 item's metadata plus a bounded Reader over
// its payload (streamed straight into Ingest, PLAN §5.1). On encode the caller sets
// PayloadSize + Payload; on decode the decoder fills them and Payload is bounded.
type CB01ItemReader struct {
	Kind byte
	Name string
	Uti  string
	Mime string
	// Payload is a bounded reader over exactly the item's payload length; consume it
	// fully (or Close it) before relying on the next item. For a non-last item it is a
	// temp-file spill that auto-removes on full read or Close.
	Payload io.Reader
	// PayloadSize is the exact payload byte count (the wire length-prefix).
	PayloadSize int64
}

// guardedReader is a bounded payload reader over exactly remaining bytes of the shared
// wire stream (used for the last item's direct, zero-spill payload).
type guardedReader struct {
	r         io.Reader
	remaining int64
}

func (g *guardedReader) Read(p []byte) (int, error) {
	if g.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > g.remaining {
		p = p[:g.remaining]
	}
	n, err := g.r.Read(p)
	g.remaining -= int64(n)
	return n, err
}

// spillReader is a bounded payload reader backed by a temp file (an earlier item's
// payload, copied off the wire via io.CopyN so it survives later metadata reads). It
// removes the temp file when fully read or Closed.
type spillReader struct {
	f      *os.File
	path   string
	closed bool
}

// spillPayload copies exactly n bytes from br to a temp file via io.CopyN (32 KiB
// window through the stdlib copy) and returns a spillReader positioned at the start. A
// short copy is a bad frame.
func spillPayload(br io.Reader, n int64) (*spillReader, error) {
	f, err := os.CreateTemp("", "clipbeam-cb01-*")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	if n > 0 {
		copied, cerr := io.CopyN(f, br, n)
		if cerr != nil || copied != n {
			_ = f.Close()
			_ = os.Remove(path)
			return nil, ErrBadFrame
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &spillReader{f: f, path: path}, nil
}

func (s *spillReader) Read(p []byte) (int, error) {
	n, err := s.f.Read(p)
	if err == io.EOF {
		_ = s.Close()
	}
	return n, err
}

// Close removes the temp file (idempotent).
func (s *spillReader) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	err := s.f.Close()
	_ = os.Remove(s.path)
	return err
}

// byteReader is the minimal interface DecodeCB01 needs: an io.Reader that can also read
// a single byte (for binary.ReadUvarint and the kind/channel bytes).
type byteReader interface {
	io.Reader
	io.ByteReader
}

// asByteReader returns r as a byteReader, wrapping it in a bufio.Reader only when it is
// not already one (so a caller passing a *bufio.Reader, as cmd/clipbeam does, is not
// double-buffered).
func asByteReader(r io.Reader) byteReader {
	if br, ok := r.(byteReader); ok {
		return br
	}
	return bufio.NewReader(r)
}

// writeUvarint writes x as a uvarint to bw.
func writeUvarint(bw io.ByteWriter, x uint64) error {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], x)
	for i := 0; i < n; i++ {
		if err := bw.WriteByte(buf[i]); err != nil {
			return err
		}
	}
	return nil
}

// writeString writes a uvarint length-prefixed UTF-8 string (empty allowed).
func writeString(bw *bufio.Writer, s string) error {
	if err := writeUvarint(bw, uint64(len(s))); err != nil {
		return err
	}
	if len(s) == 0 {
		return nil
	}
	_, err := bw.WriteString(s)
	return err
}

// readString reads a uvarint length-prefixed UTF-8 string, bounded by cb01MaxStringLen
// so a hostile prefix cannot force an unbounded allocation (empty allowed).
func readString(br byteReader) (string, error) {
	n, err := binary.ReadUvarint(br)
	if err != nil {
		return "", ErrBadFrame
	}
	if n == 0 {
		return "", nil
	}
	if n > cb01MaxStringLen {
		return "", ErrBadFrame
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(br, buf); err != nil {
		return "", ErrBadFrame
	}
	return string(buf), nil
}

// bytesReader returns an io.Reader over b (nil b yields an empty reader so a zero-len
// payload encodes cleanly).
func bytesReader(b []byte) io.Reader {
	return &sliceReader{b: b}
}

// sliceReader is a tiny io.Reader over a byte slice (avoids importing bytes just for
// the buffered-encode path).
type sliceReader struct {
	b   []byte
	off int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}
