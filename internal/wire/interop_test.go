package wire

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// interopDir is the repo-root golden-fixture directory (real bytes captured from the
// shipped Swift app's frozen Codable types, PLAN §12.5). From internal/wire it is two
// levels up.
const interopDir = "../../testdata/interop"

// readFixture reads a golden fixture by name or fails the test.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(interopDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// semanticEqualEnvelope compares two Envelopes by DECODED VALUE, never by raw JSON
// bytes (PLAN §3.5/§12.2 — raw byte-compare is BANNED because Swift's encoder escapes
// `/` as `\/` and does not sort keys, so the wire bytes differ while the decoded
// values are identical). It compares version, sender, the channel pointer (nil vs the
// pointed-to string), and every item field pointer-aware (nil distinguished from a
// present empty string — the load-bearing &"" rule). It returns a human-readable
// reason on mismatch.
func semanticEqualEnvelope(a, b Envelope) (bool, string) {
	if a.Version != b.Version {
		return false, "version differs"
	}
	if a.Sender != b.Sender {
		return false, "sender differs"
	}
	if !ptrEqual(a.Channel, b.Channel) {
		return false, "channel differs (nil vs present is significant)"
	}
	if len(a.Items) != len(b.Items) {
		return false, "item count differs"
	}
	for i := range a.Items {
		if ok, why := semanticEqualItem(a.Items[i], b.Items[i]); !ok {
			return false, "item " + itoaSmall(i) + ": " + why
		}
	}
	return true, ""
}

// semanticEqualItem compares two Items pointer-aware: a nil optional is DISTINCT from a
// present empty string (&"" emitted vs omitted, PLAN §3.5).
func semanticEqualItem(a, b Item) (bool, string) {
	if a.Kind != b.Kind {
		return false, "kind differs"
	}
	if !ptrEqual(a.Name, b.Name) {
		return false, "name differs"
	}
	if !ptrEqual(a.Uti, b.Uti) {
		return false, "uti differs"
	}
	if !ptrEqual(a.Mime, b.Mime) {
		return false, "mime differs"
	}
	if !ptrEqual(a.BytesB64, b.BytesB64) {
		return false, "bytesB64 differs"
	}
	if !ptrEqual(a.Text, b.Text) {
		return false, "text differs (nil vs present-empty is significant)"
	}
	return true, ""
}

// ptrEqual reports whether two *string are equal: both nil, or both non-nil with equal
// values. A nil pointer (omitted) is NEVER equal to a &"" (present-empty).
func ptrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestInteropDecodeAllEnvelopes decodes EVERY Swift-emitted envelope fixture into a
// wire.Envelope and asserts the load-bearing semantics: version bare 1, nil optionals
// distinguished from present-empty, channel nil vs set, the PNG/file base64 decoding
// to its real bytes (PLAN §12.5). The fixtures are real captures, so this is the
// non-circular interop gate.
func TestInteropDecodeAllEnvelopes(t *testing.T) {
	pngSig := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

	cases := []struct {
		file        string
		wantVersion int
		wantSender  string
		wantItems   int
		wantChannel *string // nil ⇒ key must be absent
		check       func(t *testing.T, e Envelope)
	}{
		{
			file: "envelope_image_png.json", wantVersion: 1, wantSender: "macbook.example",
			wantItems: 1, wantChannel: nil,
			check: func(t *testing.T, e Envelope) {
				it := e.Items[0]
				if it.Kind != "image" {
					t.Fatalf("kind = %q, want image", it.Kind)
				}
				if it.Name == nil || *it.Name != "screenshot.png" {
					t.Fatalf("name = %v, want screenshot.png", it.Name)
				}
				if it.Uti == nil || *it.Uti != "public.png" {
					t.Fatalf("uti = %v, want public.png", it.Uti)
				}
				if it.Mime == nil || *it.Mime != "image/png" {
					t.Fatalf("mime = %v, want image/png (the \\/ escape must decode)", it.Mime)
				}
				if it.BytesB64 == nil {
					t.Fatal("bytesB64 missing")
				}
				if it.Text != nil {
					t.Fatalf("image item must have nil text, got %v", it.Text)
				}
				raw, err := base64.StdEncoding.DecodeString(*it.BytesB64)
				if err != nil {
					t.Fatalf("bytesB64 not valid std base64: %v", err)
				}
				if len(raw) < len(pngSig) || string(raw[:len(pngSig)]) != string(pngSig) {
					t.Fatalf("decoded image is not a PNG (signature mismatch)")
				}
			},
		},
		{
			file: "envelope_file.json", wantVersion: 1, wantSender: "macbook.example",
			wantItems: 1, wantChannel: nil,
			check: func(t *testing.T, e Envelope) {
				it := e.Items[0]
				if it.Kind != "file" {
					t.Fatalf("kind = %q, want file", it.Kind)
				}
				if it.Uti == nil || *it.Uti != "public.data" {
					t.Fatalf("uti = %v, want public.data", it.Uti)
				}
				if it.Mime == nil || *it.Mime != "application/octet-stream" {
					t.Fatalf("mime = %v, want application/octet-stream", it.Mime)
				}
				if it.BytesB64 == nil {
					t.Fatal("file bytesB64 missing")
				}
				if _, err := base64.StdEncoding.DecodeString(*it.BytesB64); err != nil {
					t.Fatalf("file bytesB64 not valid std base64: %v", err)
				}
			},
		},
		{
			file: "envelope_text.json", wantVersion: 1, wantSender: "macbook.example",
			wantItems: 1, wantChannel: nil,
			check: func(t *testing.T, e Envelope) {
				it := e.Items[0]
				if it.Kind != "text" {
					t.Fatalf("kind = %q, want text", it.Kind)
				}
				if it.Text == nil || *it.Text != "hello from clipbeam interop" {
					t.Fatalf("text = %v, want the interop string", it.Text)
				}
				// A text item must carry NO payload metadata (PLAN §3.5).
				if it.BytesB64 != nil || it.Name != nil || it.Uti != nil || it.Mime != nil {
					t.Fatalf("text item carries unexpected metadata: %+v", it)
				}
			},
		},
		{
			file: "envelope_text_empty.json", wantVersion: 1, wantSender: "macbook.example",
			wantItems: 1, wantChannel: nil,
			check: func(t *testing.T, e Envelope) {
				it := e.Items[0]
				// THE &"" RULE: text is PRESENT and EMPTY, not nil (PLAN §3.5/§12.5).
				if it.Text == nil {
					t.Fatal("empty-text fixture: text must be present (&\"\"), not nil/omitted")
				}
				if *it.Text != "" {
					t.Fatalf("empty-text fixture: text = %q, want \"\"", *it.Text)
				}
			},
		},
		{
			file: "envelope_multi.json", wantVersion: 1, wantSender: "macbook.example",
			wantItems: 3, wantChannel: nil,
			check: func(t *testing.T, e Envelope) {
				// Item ORDER preserved: image, file, text (PLAN §12.5).
				if e.Items[0].Kind != "image" || e.Items[1].Kind != "file" || e.Items[2].Kind != "text" {
					t.Fatalf("multi item order = [%s %s %s], want [image file text]",
						e.Items[0].Kind, e.Items[1].Kind, e.Items[2].Kind)
				}
				if e.Items[2].Text == nil || *e.Items[2].Text != "caption text" {
					t.Fatalf("multi text item = %v, want 'caption text'", e.Items[2].Text)
				}
			},
		},
		{
			file: "envelope_agent_text.json", wantVersion: 1, wantSender: "macbook.example",
			wantItems: 1, wantChannel: Ptr("agent"),
			check: func(t *testing.T, e Envelope) {
				if e.Items[0].Kind != "text" || e.Items[0].Text == nil || *e.Items[0].Text != "agent says hi" {
					t.Fatalf("agent text item = %+v", e.Items[0])
				}
			},
		},
		{
			file: "envelope_agent_image.json", wantVersion: 1, wantSender: "macbook.example",
			wantItems: 1, wantChannel: Ptr("agent"),
			check: func(t *testing.T, e Envelope) {
				if e.Items[0].Kind != "image" || e.Items[0].Name == nil || *e.Items[0].Name != "agentshot.png" {
					t.Fatalf("agent image item = %+v, want name agentshot.png", e.Items[0])
				}
			},
		},
		{
			file: "envelope_channel_omitted.json", wantVersion: 1, wantSender: "macbook.example",
			wantItems: 1, wantChannel: nil, // channel KEY ABSENT ⇒ nil ⇒ clipboard
			check: func(t *testing.T, e Envelope) {
				if e.Channel != nil {
					t.Fatalf("channel-omitted fixture: channel must decode nil, got %q", *e.Channel)
				}
			},
		},
		{
			file: "envelope_channel_clipboard.json", wantVersion: 1, wantSender: "macbook.example",
			wantItems: 1, wantChannel: Ptr("clipboard"),
			check: func(t *testing.T, e Envelope) {
				if e.Channel == nil || *e.Channel != "clipboard" {
					t.Fatalf("explicit clipboard channel = %v, want 'clipboard'", e.Channel)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			env, err := UnmarshalEnvelope(readFixture(t, tc.file))
			if err != nil {
				t.Fatalf("decode %s: %v", tc.file, err)
			}
			if env.Version != tc.wantVersion {
				t.Fatalf("version = %d, want %d", env.Version, tc.wantVersion)
			}
			if env.Sender != tc.wantSender {
				t.Fatalf("sender = %q, want %q", env.Sender, tc.wantSender)
			}
			if len(env.Items) != tc.wantItems {
				t.Fatalf("items = %d, want %d", len(env.Items), tc.wantItems)
			}
			if !ptrEqual(env.Channel, tc.wantChannel) {
				t.Fatalf("channel = %v, want %v", env.Channel, tc.wantChannel)
			}
			if tc.check != nil {
				tc.check(t, env)
			}
		})
	}
}

// TestInteropChannelOmittedEqualsExplicitClipboard proves the two clipboard forms
// (channel key absent vs "channel":"clipboard") are BEHAVIORALLY equivalent: nil
// collapses to clipboard (PLAN §3.4, Models.swift:25-30). Decoded they differ only in
// the channel pointer; both route to the clipboard channel.
func TestInteropChannelOmittedEqualsExplicitClipboard(t *testing.T) {
	omitted, err := UnmarshalEnvelope(readFixture(t, "envelope_channel_omitted.json"))
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := UnmarshalEnvelope(readFixture(t, "envelope_channel_clipboard.json"))
	if err != nil {
		t.Fatal(err)
	}
	if omitted.Channel != nil {
		t.Fatalf("omitted channel must be nil, got %q", *omitted.Channel)
	}
	if explicit.Channel == nil || *explicit.Channel != "clipboard" {
		t.Fatalf("explicit channel must be 'clipboard', got %v", explicit.Channel)
	}
	// Same items (both carry the same text), so the only difference is the channel form.
	if ok, why := semanticEqualItem(omitted.Items[0], explicit.Items[0]); !ok {
		t.Fatalf("the two clipboard forms must carry the same item: %s", why)
	}
}

// TestSenderRoundTripMatchesFixtures asserts direction (b) of the M1 gate (PLAN §12.5):
// the Go sender's own encoded envelope, decoded back, matches the Swift fixture
// SEMANTICALLY (decode-then-compare via semanticEqualEnvelope — never raw bytes). It
// rebuilds each envelope from the decoded fixture, re-marshals it with the Go encoder,
// decodes the Go bytes, and asserts semantic equality with the original.
func TestSenderRoundTripMatchesFixtures(t *testing.T) {
	for _, file := range []string{
		"envelope_image_png.json",
		"envelope_file.json",
		"envelope_text.json",
		"envelope_text_empty.json",
		"envelope_multi.json",
		"envelope_agent_text.json",
		"envelope_agent_image.json",
		"envelope_channel_omitted.json",
		"envelope_channel_clipboard.json",
	} {
		t.Run(file, func(t *testing.T) {
			golden, err := UnmarshalEnvelope(readFixture(t, file))
			if err != nil {
				t.Fatalf("decode golden %s: %v", file, err)
			}
			// Re-encode with the Go (sender) encoder.
			goBytes, err := MarshalEnvelope(golden)
			if err != nil {
				t.Fatalf("Go marshal: %v", err)
			}
			// Decode the Go bytes back and compare SEMANTICALLY with the golden.
			roundTripped, err := UnmarshalEnvelope(goBytes)
			if err != nil {
				t.Fatalf("decode Go bytes: %v", err)
			}
			if ok, why := semanticEqualEnvelope(golden, roundTripped); !ok {
				t.Fatalf("Go sender round-trip diverged from %s: %s\n golden=%s\n     go=%s",
					file, why, readFixture(t, file), goBytes)
			}
		})
	}
}

// TestSenderEmptyTextRoundTrip is the &"" round-trip guard: a Go-built envelope with a
// present-empty text must re-decode to a present-empty (not nil) text. A naive
// `string,omitempty` would drop it and fail this (PLAN §3.5/§12.5).
func TestSenderEmptyTextRoundTrip(t *testing.T) {
	src := Envelope{Version: 1, Sender: "macbook.example", Items: []Item{{Kind: "text", Text: EmptyString()}}}
	b, err := MarshalEnvelope(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalEnvelope(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Items[0].Text == nil {
		t.Fatal("present-empty text was dropped on round-trip (the &\"\" rule is broken)")
	}
	if *got.Items[0].Text != "" {
		t.Fatalf("round-tripped text = %q, want \"\"", *got.Items[0].Text)
	}
	// And it must match the captured empty-text fixture semantically.
	golden, err := UnmarshalEnvelope(readFixture(t, "envelope_text_empty.json"))
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := semanticEqualEnvelope(golden, got); !ok {
		t.Fatalf("Go empty-text envelope diverged from the fixture: %s", why)
	}
}

// TestSemanticEqualBansNilVsEmpty is a guard ON THE HELPER itself: it must treat a nil
// (omitted) optional as DISTINCT from a present-empty &"" — otherwise the &"" gate
// would silently pass a regression (PLAN §12.2).
func TestSemanticEqualBansNilVsEmpty(t *testing.T) {
	withEmpty := Envelope{Version: 1, Sender: "h", Items: []Item{{Kind: "text", Text: EmptyString()}}}
	withNil := Envelope{Version: 1, Sender: "h", Items: []Item{{Kind: "text", Text: nil}}}
	if ok, _ := semanticEqualEnvelope(withEmpty, withNil); ok {
		t.Fatal("semanticEqualEnvelope wrongly treats &\"\" == nil; the helper cannot guard the rule")
	}
	if ok, _ := semanticEqualEnvelope(withEmpty, withEmpty); !ok {
		t.Fatal("semanticEqualEnvelope must treat a value equal to itself")
	}
}
