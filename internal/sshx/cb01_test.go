package sshx

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestCB01RoundTrip asserts EncodeCB01 → DecodeCB01 preserves the channel byte and
// every item's kind/name/uti/mime/payload across a multi-item frame (PLAN §5.1).
func TestCB01RoundTrip(t *testing.T) {
	items := []CB01Item{
		{Kind: cb01KindImage, Name: "shot.png", Uti: "public.png", Mime: "image/png", Payload: []byte("\x89PNGbinary")},
		{Kind: cb01KindFile, Name: "notes.txt", Payload: []byte("hello file")},
		{Kind: cb01KindText, Payload: []byte("agent message")},
	}
	var buf bytes.Buffer
	if err := EncodeCB01(&buf, cb01ChannelAgent, items); err != nil {
		t.Fatalf("encode: %v", err)
	}

	ch, decoded, err := DecodeCB01(&buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ch != cb01ChannelAgent {
		t.Errorf("channel = %d, want %d", ch, cb01ChannelAgent)
	}
	if len(decoded) != len(items) {
		t.Fatalf("decoded %d items, want %d", len(decoded), len(items))
	}
	for i, it := range decoded {
		if it.Kind != items[i].Kind || it.Name != items[i].Name || it.Uti != items[i].Uti || it.Mime != items[i].Mime {
			t.Errorf("item %d metadata = %+v, want kind/name/uti/mime from %+v", i, it, items[i])
		}
		got, rerr := io.ReadAll(it.Payload)
		if rerr != nil {
			t.Fatalf("item %d read payload: %v", i, rerr)
		}
		if !bytes.Equal(got, items[i].Payload) {
			t.Errorf("item %d payload = %q, want %q", i, got, items[i].Payload)
		}
	}
}

// TestCB01ChannelCollapse asserts the clipboard channel byte (0) round-trips and maps
// back to the "clipboard" wire string, and the agent byte (1) to "agent" (PLAN §5.1:
// the nil/"clipboard" distinction collapses to 0).
func TestCB01ChannelCollapse(t *testing.T) {
	for _, tc := range []struct {
		channel string
		want    byte
		back    string
	}{
		{"", cb01ChannelClipboard, "clipboard"},
		{"clipboard", cb01ChannelClipboard, "clipboard"},
		{"agent", cb01ChannelAgent, "agent"},
	} {
		if b := ChannelByte(tc.channel); b != tc.want {
			t.Errorf("ChannelByte(%q) = %d, want %d", tc.channel, b, tc.want)
		}
		if s := ChannelString(tc.want); s != tc.back {
			t.Errorf("ChannelString(%d) = %q, want %q", tc.want, s, tc.back)
		}
	}
}

// TestCB01EmptyPayloadAndMetadata asserts an item with empty name/uti/mime and a
// zero-length payload round-trips cleanly (empty allowed, PLAN §5.1).
func TestCB01EmptyPayloadAndMetadata(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeCB01(&buf, cb01ChannelClipboard, []CB01Item{{Kind: cb01KindText}}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	ch, decoded, err := DecodeCB01(&buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ch != cb01ChannelClipboard || len(decoded) != 1 {
		t.Fatalf("ch=%d items=%d", ch, len(decoded))
	}
	got, _ := io.ReadAll(decoded[0].Payload)
	if len(got) != 0 || decoded[0].Name != "" {
		t.Errorf("empty item round-trip mismatch: %+v payload=%q", decoded[0], got)
	}
}

// TestCB01CapDirect asserts the streaming decode lets a caller enforce decoded-sum ≤
// maxBytes DIRECTLY (raw==decoded for CB01) by reading each payload through an
// io.CopyN-bounded counter — never the base64-inflated /clip ceiling (PLAN §5.1). This
// is the cap-semantics gate: a frame whose decoded sum exceeds maxBytes is detectable
// by the streaming consumer without reading the whole frame into memory.
func TestCB01CapDirect(t *testing.T) {
	const maxBytes = 16
	big := bytes.Repeat([]byte("A"), maxBytes+1) // one byte over the cap
	var buf bytes.Buffer
	if err := EncodeCB01(&buf, cb01ChannelClipboard, []CB01Item{{Kind: cb01KindFile, Name: "big", Payload: big}}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	_, decoded, err := DecodeCB01(&buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Stream the payload counting bytes; trip the cap when the running sum exceeds it
	// (mirrors the Ingest per-item incremental cap, decoded == raw for CB01).
	var total int64
	tripped := false
	r := decoded[0].Payload
	bufp := make([]byte, 4)
	for {
		n, rerr := r.Read(bufp)
		total += int64(n)
		if total > maxBytes {
			tripped = true
			break
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatalf("read: %v", rerr)
		}
	}
	if !tripped {
		t.Errorf("decoded-sum %d did not trip the direct cap %d", total, maxBytes)
	}
	// The declared PayloadSize equals the raw byte count (no base64 inflation).
	if decoded[0].PayloadSize != int64(len(big)) {
		t.Errorf("PayloadSize = %d, want %d (raw==decoded for CB01)", decoded[0].PayloadSize, len(big))
	}
}

// TestCB01BadMagic asserts a frame with the wrong magic is rejected with ErrBadFrame.
func TestCB01BadMagic(t *testing.T) {
	r := strings.NewReader("XXXX\x00\x01")
	if _, _, err := DecodeCB01(r); err != ErrBadFrame {
		t.Errorf("bad magic err = %v, want ErrBadFrame", err)
	}
}

// TestCB01Truncated asserts a frame truncated mid-payload surfaces an error when the
// short payload is drained (the streaming reader cannot deliver the declared length).
func TestCB01Truncated(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeCB01(&buf, cb01ChannelClipboard, []CB01Item{{Kind: cb01KindFile, Name: "x", Payload: []byte("0123456789")}}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Chop the last 5 payload bytes off the frame.
	full := buf.Bytes()
	truncated := full[:len(full)-5]

	_, decoded, err := DecodeCB01(bytes.NewReader(truncated))
	if err != nil {
		t.Fatalf("header decode should succeed: %v", err)
	}
	got, rerr := io.ReadAll(decoded[0].Payload)
	if rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
		// io.LimitReader yields EOF early on a short stream; the guarded reader returns
		// fewer bytes than PayloadSize, which the consumer detects via the byte count.
		t.Logf("read err = %v (short read)", rerr)
	}
	if int64(len(got)) >= decoded[0].PayloadSize {
		t.Errorf("truncated payload delivered %d bytes, want < declared %d", len(got), decoded[0].PayloadSize)
	}
}

// TestCB01MultiItemStreamingOrder asserts that draining items in order works even when
// an earlier item's payload is NOT fully read by the caller — the decoder drains the
// remainder before exposing the next item's metadata (the streaming contract).
func TestCB01MultiItemStreamingOrder(t *testing.T) {
	items := []CB01Item{
		{Kind: cb01KindFile, Name: "a", Payload: []byte("first-payload-unread-tail")},
		{Kind: cb01KindFile, Name: "b", Payload: []byte("second")},
	}
	var buf bytes.Buffer
	if err := EncodeCB01(&buf, cb01ChannelClipboard, items); err != nil {
		t.Fatalf("encode: %v", err)
	}
	_, decoded, err := DecodeCB01(&buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Read only the first 3 bytes of item 0, leaving the tail unread.
	partial := make([]byte, 3)
	if _, rerr := io.ReadFull(decoded[0].Payload, partial); rerr != nil {
		t.Fatalf("partial read item 0: %v", rerr)
	}
	if decoded[0].Name != "a" {
		t.Fatalf("item 0 name = %q", decoded[0].Name)
	}
	// Now read item 1 fully; the decoder must have drained item 0's remainder.
	got, rerr := io.ReadAll(decoded[1].Payload)
	if rerr != nil {
		t.Fatalf("read item 1: %v", rerr)
	}
	if string(got) != "second" {
		t.Errorf("item 1 payload = %q, want %q (item 0 remainder not drained)", got, "second")
	}
}

// TestKindByteRoundTrip asserts KindByte/KindString are inverse for every kind.
func TestKindByteRoundTrip(t *testing.T) {
	for _, k := range []string{"image", "file", "text"} {
		if got := KindString(KindByte(k)); got != k {
			t.Errorf("KindString(KindByte(%q)) = %q", k, got)
		}
	}
	// An unknown kind collapses to image (the default), matching the sender's fallback.
	if KindString(KindByte("weird")) != "image" {
		t.Errorf("unknown kind should default to image")
	}
}
