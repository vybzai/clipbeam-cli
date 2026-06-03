package wire

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzEnvelopeDecode throws arbitrary bytes at UnmarshalEnvelope and asserts it NEVER
// panics — it either returns a valid Envelope or a clean error (PLAN §12.3). When it
// decodes, re-marshaling the result must also not panic, and re-decoding the marshaled
// bytes must succeed (a structural round-trip invariant: a value the decoder accepted and
// the encoder produced must be re-decodable). Seeds are the real golden fixtures plus
// adversarial inputs (truncated, deeply nested, huge numbers, lone surrogates, explicit
// nulls). Run: go test -run xxx -fuzz=FuzzEnvelopeDecode -fuzztime=30s ./internal/wire
func FuzzEnvelopeDecode(f *testing.F) {
	// Seed with every captured Swift fixture (real Envelope-v1 bytes).
	for _, name := range []string{
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
		if b, err := os.ReadFile(filepath.Join(interopDir, name)); err == nil {
			f.Add(b)
		}
	}
	// Adversarial seeds.
	for _, s := range []string{
		``,
		`{`,
		`{}`,
		`[]`,
		`null`,
		`{"version":1}`,
		`{"version":1,"sender":"h","items":[]}`,
		`{"version":1,"sender":"h","items":[{"kind":"text","text":null}]}`,
		`{"version":1,"sender":"h","items":[{"kind":"text","text":""}]}`,
		`{"version":99999999999999999999,"sender":"h","items":[]}`,
		`{"version":1,"sender":"h","items":[{"kind":"image","bytesB64":"!!!notbase64!!!"}]}`,
		`{"version":1,"items":[{"kind":"text","text":"\ud834"}]}`, // lone surrogate
		`{"version":1,"channel":null,"sender":"h","items":[{"kind":"text","text":"x"}]}`,
		`{"version":1,"channel":"agent","sender":"h","items":[{"kind":"text","text":"x"}]}`,
		"{\"version\":1,\"sender\":\"\x00\x01\x02\",\"items\":[]}", // control bytes
	} {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		env, err := UnmarshalEnvelope(data)
		if err != nil {
			return // a clean decode error is a valid outcome
		}
		// A successfully decoded envelope must re-marshal without panic and re-decode.
		out, merr := MarshalEnvelope(env)
		if merr != nil {
			t.Fatalf("re-marshal of a decoded envelope failed: %v", merr)
		}
		if _, derr := UnmarshalEnvelope(out); derr != nil {
			t.Fatalf("re-decode of Go-encoded envelope failed: %v (input=%q)", derr, data)
		}
	})
}
