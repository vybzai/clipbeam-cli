package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// captureOut returns an `out` writing to a buffer in the given mode, plus the buffer.
func captureOut(jsonMode bool) (out, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	return out{stdout: &stdout, stderr: &stderr, json: jsonMode}, &stdout, &stderr
}

// TestLastJSONShape verifies the last/wait --json shape (PLAN §8.2):
// {"schema":"clipbeam.v1","ok":true,"path":<abs|null>}, one line WITH a newline.
func TestLastJSONShape(t *testing.T) {
	o, stdout, _ := captureOut(true)
	p := "/home/me/file.png"
	if err := o.emitJSON(pathJSON{Schema: schemaVersion, OK: true, Path: &p}); err != nil {
		t.Fatal(err)
	}
	got := stdout.String()
	if !strings.HasSuffix(got, "\n") {
		t.Error("--json line must carry a trailing newline (PLAN §8.2)")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(got)), &m); err != nil {
		t.Fatalf("not valid JSON: %v (%q)", err, got)
	}
	if m["schema"] != "clipbeam.v1" || m["ok"] != true || m["path"] != "/home/me/file.png" {
		t.Errorf("shape = %v, want schema/ok/path", m)
	}
}

// TestLastJSONNull verifies the nothing-available --json path:null shape.
func TestLastJSONNull(t *testing.T) {
	o, stdout, _ := captureOut(true)
	if err := emitNothingPath(o); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &m); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if v, ok := m["path"]; !ok || v != nil {
		t.Errorf("path = %v, want explicit null", v)
	}
}

// TestLastDefaultNothingPrintsNothing verifies that nothing-available in default mode
// prints NOTHING (and so exits 0) — PLAN §8.1.
func TestLastDefaultNothingPrintsNothing(t *testing.T) {
	o, stdout, _ := captureOut(false)
	if err := emitNothingPath(o); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Errorf("default nothing-available wrote %q, want empty", stdout.String())
	}
}

// TestDataNoTrailingNewline verifies out.data writes VERBATIM with no added newline
// (the bare-path contract, PLAN §8.1).
func TestDataNoTrailingNewline(t *testing.T) {
	o, stdout, _ := captureOut(false)
	o.data("/abs/path")
	if stdout.String() != "/abs/path" {
		t.Errorf("data wrote %q, want exactly /abs/path with no newline", stdout.String())
	}
}

// TestRecvJSONShape verifies the recv --json shape (PLAN §8.2) including channel:agent
// and the null-vs-value handling of path/text/cid.
func TestRecvJSONShape(t *testing.T) {
	o, stdout, _ := captureOut(true)
	p := "/inbox/a.png"
	if err := o.emitJSON(recvJSON{
		Schema: schemaVersion, OK: true, Type: "image", Sender: "box",
		Path: &p, Text: nil, Channel: "agent", Cid: nil,
	}); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &m); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if m["channel"] != "agent" {
		t.Errorf("channel = %v, want agent", m["channel"])
	}
	if m["type"] != "image" || m["sender"] != "box" || m["path"] != "/inbox/a.png" {
		t.Errorf("shape = %v", m)
	}
	if v, ok := m["text"]; !ok || v != nil {
		t.Errorf("text = %v, want explicit null for an image item", v)
	}
	if v, ok := m["cid"]; !ok || v != nil {
		t.Errorf("cid = %v, want explicit null (off by default)", v)
	}
}

// TestRecvTimeoutJSONShape verifies the recv timeout --json shape (item:null).
func TestRecvTimeoutJSONShape(t *testing.T) {
	o, stdout, _ := captureOut(true)
	if err := emitRecvTimeout(o); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &m); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if m["ok"] != true {
		t.Errorf("ok = %v, want true (a timeout is success)", m["ok"])
	}
	if v, ok := m["item"]; !ok || v != nil {
		t.Errorf("item = %v, want explicit null", v)
	}
}

// TestSendResultJSONShape verifies the send/push/shot/msg --json shape (PLAN §8.2/§8.7).
func TestSendResultJSONShape(t *testing.T) {
	o, stdout, _ := captureOut(true)
	if err := emitSendOutcome(o, sendOutcome{sentItems: 2, remotePath: "/box/save/x.png"}); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &m); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if m["sentItems"] != float64(2) || m["remotePath"] != "/box/save/x.png" {
		t.Errorf("shape = %v, want sentItems:2, remotePath set", m)
	}
}

// TestSendDefaultModeShape verifies the default-mode send output (PLAN §8.7): the
// control response verbatim, then the remote path with NO trailing newline.
func TestSendDefaultModeShape(t *testing.T) {
	o, stdout, _ := captureOut(false)
	if err := emitSendOutcome(o, sendOutcome{sentItems: 1, remotePath: "/box/save/x.png"}); err != nil {
		t.Fatal(err)
	}
	got := stdout.String()
	if !strings.HasPrefix(got, `{"ok":true,"sentItems":1}`+"\n") {
		t.Errorf("default send output = %q, want the control response first", got)
	}
	if !strings.HasSuffix(got, "/box/save/x.png") || strings.HasSuffix(got, "\n") {
		t.Errorf("default send output = %q, want it to END with the remote path, no trailing newline", got)
	}
}

// TestVersionJSONShape verifies the version --json shape (PLAN §8.2).
func TestVersionJSONShape(t *testing.T) {
	o, stdout, _ := captureOut(true)
	cmdVersion = "9.9.9"
	if err := runVersion(o); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &m); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if m["version"] != "9.9.9" {
		t.Errorf("version = %v, want 9.9.9", m["version"])
	}
	if m["wireProtocol"] != "envelope-v1" {
		t.Errorf("wireProtocol = %v, want envelope-v1 (distinct from the CLI schema)", m["wireProtocol"])
	}
	if m["schema"] != "clipbeam.v1" {
		t.Errorf("schema = %v, want clipbeam.v1", m["schema"])
	}
}

// TestVersionDefaultMode verifies default-mode version prints the bare semver + newline.
func TestVersionDefaultMode(t *testing.T) {
	o, stdout, _ := captureOut(false)
	cmdVersion = "1.0.0"
	if err := runVersion(o); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "1.0.0\n" {
		t.Errorf("version default = %q, want %q", stdout.String(), "1.0.0\n")
	}
}

// TestSchemaShape verifies the schema document carries the exit-code table, the env
// vars, every command, and the wire/CLI version distinction (PLAN §8.5/§8.2).
func TestSchemaShape(t *testing.T) {
	doc := buildSchema("2.0.0")
	if doc.Schema != "clipbeam.v1" {
		t.Errorf("schema = %q, want clipbeam.v1", doc.Schema)
	}
	if doc.WireProtocol != "envelope-v1" {
		t.Errorf("wireProtocol = %q, want envelope-v1", doc.WireProtocol)
	}
	if doc.Version != "2.0.0" {
		t.Errorf("version = %q, want 2.0.0", doc.Version)
	}
	if len(doc.ExitCodes) != 10 {
		t.Errorf("exit codes = %d rows, want 10 (codes 0-9)", len(doc.ExitCodes))
	}
	// Every code 0-9 present exactly once.
	seen := map[int]bool{}
	for _, e := range doc.ExitCodes {
		seen[e.Code] = true
	}
	for code := 0; code <= 9; code++ {
		if !seen[code] {
			t.Errorf("exit-code table missing code %d", code)
		}
	}
	// Spot-check a few verbs are described.
	names := map[string]bool{}
	for _, c := range doc.Commands {
		names[c.Name] = true
	}
	for _, want := range []string{"send", "msg", "recv", "last", "wait", "watch", "health", "version", "schema", "install-skill", "serve", "ingest", "setup", "doctor", "service", "push", "shot"} {
		if !names[want] {
			t.Errorf("schema commands missing %q", want)
		}
	}
}

// TestSchemaEmitIsValidJSON verifies runSchema emits valid JSON to stdout.
func TestSchemaEmitIsValidJSON(t *testing.T) {
	o, stdout, _ := captureOut(false) // schema is always JSON regardless of mode
	cmdVersion = "3.3.3"
	if err := runSchema(o); err != nil {
		t.Fatal(err)
	}
	var doc schemaDoc
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if doc.Binary != "clipbeam" {
		t.Errorf("binary = %q, want clipbeam", doc.Binary)
	}
}

// TestJSONErrorEnvelope verifies the §8.2 error envelope shape.
func TestJSONErrorEnvelope(t *testing.T) {
	o, stdout, _ := captureOut(true)
	o.emitJSONError("payload too large", ExitTooLarge)
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &m); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if m["ok"] != false || m["error"] != "payload too large" || m["code"] != float64(ExitTooLarge) {
		t.Errorf("error envelope = %v, want ok:false/error/code:%d", m, ExitTooLarge)
	}
	if m["schema"] != "clipbeam.v1" {
		t.Errorf("schema = %v, want clipbeam.v1", m["schema"])
	}
}
