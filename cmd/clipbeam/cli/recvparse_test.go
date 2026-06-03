package cli

import "testing"

// TestParseRecvBody_Image checks an image item: type/sender/path present, no text.
func TestParseRecvBody_Image(t *testing.T) {
	body := "type: image\nsender: box-01\npath: /home/me/.local/state/clipbeam/agent-inbox/shot.png"
	it := parseRecvBody(body)
	if it.Type != "image" {
		t.Errorf("type = %q, want image", it.Type)
	}
	if it.Sender != "box-01" {
		t.Errorf("sender = %q, want box-01", it.Sender)
	}
	if it.Path != "/home/me/.local/state/clipbeam/agent-inbox/shot.png" {
		t.Errorf("path = %q (a leading space must be stripped)", it.Path)
	}
	if it.HasText {
		t.Errorf("HasText = true, want false for an image item")
	}
}

// TestParseRecvBody_TextSingleLine checks a single-line text item.
func TestParseRecvBody_TextSingleLine(t *testing.T) {
	it := parseRecvBody("type: text\nsender: laptop\ntext: build is green")
	if it.Type != "text" || it.Sender != "laptop" {
		t.Fatalf("header parse wrong: %+v", it)
	}
	if !it.HasText {
		t.Fatal("HasText = false, want true")
	}
	if it.Text != "build is green" {
		t.Errorf("text = %q, want %q", it.Text, "build is green")
	}
}

// TestParseRecvBody_TextMultiLine checks that text consumes the rest VERBATIM,
// embedded newlines preserved (PLAN §8.2: text: is last and consumes everything).
func TestParseRecvBody_TextMultiLine(t *testing.T) {
	it := parseRecvBody("type: text\nsender: laptop\ntext: line one\nline two\nline three")
	if it.Text != "line one\nline two\nline three" {
		t.Errorf("text = %q, want the multi-line body verbatim", it.Text)
	}
}

// TestParseRecvBody_TextWithColon checks that a value containing colons is preserved
// (split on the FIRST colon only — PLAN §8.2).
func TestParseRecvBody_TextWithColon(t *testing.T) {
	it := parseRecvBody("type: text\nsender: laptop\ntext: see https://example.com:8080/x")
	if it.Text != "see https://example.com:8080/x" {
		t.Errorf("text = %q, want the URL with its colons intact", it.Text)
	}
}

// TestParseRecvBody_EmptyText checks the text:"" item (present-but-empty, distinct
// from an absent text line — PLAN §8.2/§8.6).
func TestParseRecvBody_EmptyText(t *testing.T) {
	it := parseRecvBody("type: text\nsender: laptop\ntext: ")
	if !it.HasText {
		t.Fatal("HasText = false, want true for a present-but-empty text line")
	}
	if it.Text != "" {
		t.Errorf("text = %q, want empty string", it.Text)
	}
}

// TestParseRecvBody_PathWithSpacesAndColons checks a path value with spaces + colons
// is preserved after dropping exactly one leading space.
func TestParseRecvBody_PathWithSpacesAndColons(t *testing.T) {
	it := parseRecvBody("type: file\nsender: box\npath: /tmp/My File: v2.txt")
	if it.Path != "/tmp/My File: v2.txt" {
		t.Errorf("path = %q, want the spaced path with its colon intact", it.Path)
	}
}

// TestSplitFirstColon checks the first-colon split + single-leading-space drop.
func TestSplitFirstColon(t *testing.T) {
	cases := []struct {
		line       string
		wantLabel  string
		wantValue  string
	}{
		{"type: image", "type", "image"},
		{"text: a:b:c", "text", "a:b:c"},
		{"path:  two-spaces", "path", " two-spaces"}, // only ONE leading space dropped
		{"noColonHere", "", "noColonHere"},
		{"empty:", "empty", ""},
	}
	for _, c := range cases {
		gotL, gotV := splitFirstColon(c.line)
		if gotL != c.wantLabel || gotV != c.wantValue {
			t.Errorf("splitFirstColon(%q) = (%q,%q), want (%q,%q)", c.line, gotL, gotV, c.wantLabel, c.wantValue)
		}
	}
}

// TestExtractCid checks the opt-in [clipbeam:cid=<uuid>] convention (PLAN §8.6):
// extracted when present, untouched otherwise, one separating space consumed.
func TestExtractCid(t *testing.T) {
	clean, cid := extractCid("[clipbeam:cid=abc-123] hello there")
	if cid != "abc-123" {
		t.Errorf("cid = %q, want abc-123", cid)
	}
	if clean != "hello there" {
		t.Errorf("clean = %q, want %q", clean, "hello there")
	}

	// No token: text unchanged, cid empty (a Swift peer round-trips as cid:null).
	clean, cid = extractCid("plain message")
	if cid != "" || clean != "plain message" {
		t.Errorf("no-token case = (%q,%q), want (plain message, '')", clean, cid)
	}

	// Malformed (no closing bracket): left untouched.
	clean, cid = extractCid("[clipbeam:cid=oops no bracket")
	if cid != "" || clean != "[clipbeam:cid=oops no bracket" {
		t.Errorf("malformed-token case = (%q,%q), want untouched", clean, cid)
	}
}

// TestPrependCidRoundTrip checks prependCid → extractCid is a clean round-trip.
func TestPrependCidRoundTrip(t *testing.T) {
	body := prependCid("uuid-9", "reply body")
	clean, cid := extractCid(body)
	if cid != "uuid-9" || clean != "reply body" {
		t.Errorf("round-trip = (%q,%q), want (reply body, uuid-9)", clean, cid)
	}
}

// TestRecvCidIntegration checks the cid is extracted out of a full labeled body's text.
func TestRecvCidIntegration(t *testing.T) {
	it := parseRecvBody("type: text\nsender: laptop\ntext: [clipbeam:cid=r1] ack")
	if it.Cid != "r1" {
		t.Errorf("cid = %q, want r1", it.Cid)
	}
	if it.Text != "ack" {
		t.Errorf("text = %q, want %q (cid token stripped)", it.Text, "ack")
	}
}
