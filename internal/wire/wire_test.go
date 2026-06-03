package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPointerOptionalOmitNil asserts a nil optional is OMITTED from the wire JSON
// (no "name":null), matching Swift's nil-omission (PLAN §3.5).
func TestPointerOptionalOmitNil(t *testing.T) {
	b, err := MarshalEnvelope(Envelope{Version: 1, Sender: "h", Items: []Item{{Kind: "text", Text: Ptr("hi")}}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, k := range []string{`"name"`, `"uti"`, `"mime"`, `"bytesB64"`, `"channel"`} {
		if strings.Contains(s, k) {
			t.Errorf("nil optional %s should be omitted, got: %s", k, s)
		}
	}
}

// TestEmptyTextEmitted asserts the load-bearing &"" rule: a present-but-empty text
// is emitted as "text":"" (NOT dropped), matching an empty /agent-send body
// (PLAN §3.5).
func TestEmptyTextEmitted(t *testing.T) {
	b, err := MarshalEnvelope(Envelope{Version: 1, Sender: "h", Items: []Item{{Kind: "text", Text: EmptyString()}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"text":""`) {
		t.Errorf("present-but-empty text must be emitted as \"text\":\"\", got: %s", b)
	}
}

// TestDecodeTolerance asserts pointer optionals tolerate both a missing key and an
// explicit null on decode (PLAN §3.5).
func TestDecodeTolerance(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"sender":"h","items":[{"kind":"text","text":"x"}]}`,
		`{"version":1,"sender":"h","items":[{"kind":"text","text":"x","name":null}]}`,
	} {
		e, err := UnmarshalEnvelope([]byte(raw))
		if err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
		if e.Version != 1 || len(e.Items) != 1 || e.Items[0].Name != nil {
			t.Errorf("decode %q: unexpected %+v", raw, e)
		}
	}
}

// TestVersionBareInteger asserts version encodes as a bare JSON number (PLAN §3.5).
func TestVersionBareInteger(t *testing.T) {
	b, _ := MarshalEnvelope(Envelope{Version: 1, Sender: "h", Items: []Item{{Kind: "text", Text: Ptr("x")}}})
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["version"]) != "1" {
		t.Errorf("version must be bare 1, got %s", m["version"])
	}
}
