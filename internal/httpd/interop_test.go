package httpd

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/store"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// interopDir is the repo-root golden-fixture directory (real bytes captured from the
// shipped Swift app, PLAN §12.5). From internal/httpd it is two levels up.
const interopDir = "../../testdata/interop"

func readInterop(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(interopDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// interopHarness wires a Server over a real loopback listener in ListenLoopbackTCP
// mode (peerHost=""), so a loopback connection classifies as .loopback — which BOTH the
// /clip gate (Loopback OR Peer) and the control-endpoint gate (/recv, /last require
// Loopback) accept with the token. SaveTextToDisk is configurable so the multi-fixture's
// text sidecar is produced. It returns the harness plus the saveDir / inbox so the test
// can assert on-disk leaf names.
func newInteropHarness(t *testing.T, saveTextToDisk bool) *testHarness {
	t.Helper()
	dir := t.TempDir()
	saveDir := filepath.Join(dir, "save")
	inbox := filepath.Join(dir, "inbox")
	if err := os.MkdirAll(saveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(store.StoreConfig{
		SaveDir:           saveDir,
		AgentInboxDir:     inbox,
		LastPathFile:      filepath.Join(dir, "last_path"),
		RecentsFile:       filepath.Join(dir, "recents.json"),
		SaveTextToDisk:    saveTextToDisk,
		LongTextThreshold: 8192,
		MaxBytes:          wire.DefaultMaxBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.SaveDir = saveDir
	cfg.MaxBytes = wire.DefaultMaxBytes
	srv := New(Options{
		Config: cfg, Token: testToken, Store: st,
		Listen: ListenLoopbackTCP, PeerHost: "", AppVersion: "test",
		TempDir: filepath.Join(dir, "tmp"),
	})
	ln := mustListenLoopback(t)
	serveOn(t, srv, ln)
	return &testHarness{
		srv: srv, baseURL: "http://" + ln.Addr().String(),
		saveDir: saveDir, inbox: inbox, store: st,
	}
}

// postClipFixture POSTs a golden envelope fixture verbatim to /clip with the token and
// returns the response.
func (h *testHarness) postClipFixture(t *testing.T, fixture string) *http.Response {
	t.Helper()
	body := readInterop(t, fixture)
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/clip", strings.NewReader(string(body)))
	req.Header.Set(tokenHeader, testToken)
	req.ContentLength = int64(len(body))
	return h.do(t, req)
}

// leavesIn returns the non-temp file leaf names in dir (sorted-insensitive, just the
// set we assert membership against).
func leavesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".clipbeam-") {
			continue // streaming-decode scratch / reservation temp
		}
		if e.IsDir() && n == "journal" {
			continue // the disk-backed agent journal (metadata, not a payload leaf)
		}
		out = append(out, n)
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestInteropClipImageSavesScreenshot decodes the real Swift PNG envelope through the
// production /clip streaming decode + Ingest and asserts the ClipResponse and the saved
// leaf match response_clip_image.json's basename (PLAN §12.5 direction (a)).
func TestInteropClipImageSavesScreenshot(t *testing.T) {
	h := newInteropHarness(t, false)
	resp := h.postClipFixture(t, "envelope_image_png.json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clip image fixture = %d body=%q, want 200", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"ok":true`) || !strings.Contains(body, `"count":1`) {
		t.Fatalf("clip image response = %q, want ok:true count:1", body)
	}
	// The fixture's response basename is screenshot.png (the sanitizer keeps it verbatim).
	if leaves := leavesIn(t, h.saveDir); !contains(leaves, "screenshot.png") {
		t.Fatalf("save dir leaves = %v, want screenshot.png", leaves)
	}
	last, ok := h.store.LastPath()
	if !ok || filepath.Base(last) != "screenshot.png" {
		t.Fatalf("last_path = %q, want .../screenshot.png", last)
	}
}

// TestInteropClipFileSaves decodes the real Swift file envelope and asserts the saved
// leaf is notes.bin and the bytes decode to the captured 32-byte payload.
func TestInteropClipFileSaves(t *testing.T) {
	h := newInteropHarness(t, false)
	resp := h.postClipFixture(t, "envelope_file.json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clip file fixture = %d, want 200", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, `"count":1`) {
		t.Fatalf("clip file response = %q, want count:1", body)
	}
	if leaves := leavesIn(t, h.saveDir); !contains(leaves, "notes.bin") {
		t.Fatalf("save dir leaves = %v, want notes.bin", leaves)
	}
	// The payload "clipbeam interop file payload v1\n" must be on disk verbatim.
	got, err := os.ReadFile(filepath.Join(h.saveDir, "notes.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "clipbeam interop file payload v1\n" {
		t.Fatalf("saved file bytes = %q", got)
	}
}

// TestInteropClipTextNoSidecar decodes the real text envelope; on the clipboard channel
// with saveTextToDisk=false short text is NOT written to disk (the carve-out, §3.8) yet
// the response still reports count:1 (item count).
func TestInteropClipTextNoSidecar(t *testing.T) {
	h := newInteropHarness(t, false)
	resp := h.postClipFixture(t, "envelope_text.json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clip text fixture = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"count":1`) || !strings.Contains(body, `"saved":[]`) {
		t.Fatalf("clip text response = %q, want count:1 saved:[] (short text uncounted/unsaved)", body)
	}
	if leaves := leavesIn(t, h.saveDir); len(leaves) != 0 {
		t.Fatalf("short clipboard text wrote files %v, want none", leaves)
	}
}

// TestInteropClipEmptyText is the &"" gate on the receive side: the empty-text fixture
// ({"text":"","kind":"text"}) must decode to a PRESENT-EMPTY text, dispatch as one
// clipboard text item (count:1), and write no sidecar (short). A decoder that dropped
// the empty text would yield a zero-item envelope → 400, so 200/count:1 proves the
// present-empty was carried (PLAN §12.5).
func TestInteropClipEmptyText(t *testing.T) {
	h := newInteropHarness(t, false)
	resp := h.postClipFixture(t, "envelope_text_empty.json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty-text fixture = %d body=%q, want 200 (present-empty must be carried, not dropped)",
			resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"count":1`) {
		t.Fatalf("empty-text response = %q, want count:1", body)
	}
}

// TestInteropClipMulti decodes the multi-item fixture (image+file+text, order
// preserved). With saveTextToDisk=true the text becomes a sidecar, so the response has
// count:3 and three saved paths whose first two basenames are shot.png and doc.bin and
// whose third matches the clipbeam-<UTC>.txt sidecar pattern
// (response_clip_multi.json).
func TestInteropClipMulti(t *testing.T) {
	h := newInteropHarness(t, true) // force the text sidecar so count:3 matches the fixture
	resp := h.postClipFixture(t, "envelope_multi.json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("multi fixture = %d body=%q, want 200", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"count":3`) {
		t.Fatalf("multi response = %q, want count:3", body)
	}
	leaves := leavesIn(t, h.saveDir)
	if !contains(leaves, "shot.png") || !contains(leaves, "doc.bin") {
		t.Fatalf("multi save dir = %v, want shot.png + doc.bin", leaves)
	}
	var sawSidecar bool
	for _, l := range leaves {
		if strings.HasPrefix(l, "clipbeam-") && strings.HasSuffix(l, ".txt") {
			sawSidecar = true
		}
	}
	if !sawSidecar {
		t.Fatalf("multi save dir = %v, want a clipbeam-<UTC>.txt sidecar", leaves)
	}
}

// TestInteropClipChannelOmittedIsClipboard decodes the channel-omitted fixture; nil
// channel → clipboard behavior, so the item lands in the save dir and sets last_path
// (PLAN §3.4). With saveTextToDisk=true the text writes a sidecar so it is observable.
func TestInteropClipChannelOmittedIsClipboard(t *testing.T) {
	h := newInteropHarness(t, true)
	resp := h.postClipFixture(t, "envelope_channel_omitted.json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("channel-omitted fixture = %d, want 200", resp.StatusCode)
	}
	_ = readBody(t, resp)
	if _, ok := h.store.LastPath(); !ok {
		t.Fatal("channel-omitted (nil) must route to clipboard and set last_path")
	}
}

// TestInteropClipChannelClipboardExplicit decodes the explicit "clipboard" channel
// fixture; it must behave identically to the omitted form (both route to clipboard).
func TestInteropClipChannelClipboardExplicit(t *testing.T) {
	h := newInteropHarness(t, true)
	resp := h.postClipFixture(t, "envelope_channel_clipboard.json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("explicit-clipboard fixture = %d, want 200", resp.StatusCode)
	}
	_ = readBody(t, resp)
	if _, ok := h.store.LastPath(); !ok {
		t.Fatal("explicit clipboard channel must set last_path")
	}
}

// TestInteropClipAgentText decodes the agent-channel text fixture; agent text is
// enqueued in memory only (saved:[]) but counted (count:1) — matching
// response_clip_agent_text.json — and then drains via /recv with the labeled body
// (text last, colon-space).
func TestInteropClipAgentText(t *testing.T) {
	h := newInteropHarness(t, false)
	resp := h.postClipFixture(t, "envelope_agent_text.json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent-text fixture = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"saved":[]`) || !strings.Contains(body, `"count":1`) {
		t.Fatalf("agent-text response = %q, want saved:[] count:1", body)
	}
	if leaves := leavesIn(t, h.inbox); len(leaves) != 0 {
		t.Fatalf("agent text wrote inbox files %v, want none (text never on disk)", leaves)
	}
	// /recv yields the labeled body with text last and the captured text.
	rreq, _ := http.NewRequest(http.MethodGet, h.baseURL+"/recv?timeout=5", nil)
	rreq.Header.Set(tokenHeader, testToken)
	got := readBody(t, h.do(t, rreq))
	if !strings.HasSuffix(got, "text: agent says hi") {
		t.Fatalf("/recv body = %q, want it to END with 'text: agent says hi'", got)
	}
}

// TestInteropClipAgentImage decodes the agent-channel image fixture; it saves to the
// agent inbox under the verbatim leaf agentshot.png (matching
// response_clip_agent_image.json's basename) and /recv yields a type:image body with a
// path: pointing into the inbox.
func TestInteropClipAgentImage(t *testing.T) {
	h := newInteropHarness(t, false)
	resp := h.postClipFixture(t, "envelope_agent_image.json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent-image fixture = %d body=%q, want 200", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"count":1`) {
		t.Fatalf("agent-image response = %q, want count:1", body)
	}
	if leaves := leavesIn(t, h.inbox); !contains(leaves, "agentshot.png") {
		t.Fatalf("agent inbox leaves = %v, want agentshot.png", leaves)
	}
	rreq, _ := http.NewRequest(http.MethodGet, h.baseURL+"/recv?timeout=5", nil)
	rreq.Header.Set(tokenHeader, testToken)
	got := readBody(t, h.do(t, rreq))
	if !strings.HasPrefix(got, "type: image\n") || !strings.Contains(got, "path: ") {
		t.Fatalf("/recv agent image body = %q, want type:image + path:", got)
	}
	if strings.Contains(got, "text: ") {
		t.Fatalf("/recv agent image body = %q, must NOT carry a text: label", got)
	}
}

// TestInteropRecvLabeledBodyMatchesFixture asserts the /recv labeled body shape matches
// the captured recv_text.txt: type/sender/text in order, colon-SPACE separators, text
// LAST, and NO trailing newline (PLAN §12.5). It drives the body through writeRecvBody
// with the same field values the fixture carries.
func TestInteropRecvLabeledBodyMatchesFixture(t *testing.T) {
	want := string(readInterop(t, "recv_text.txt"))
	// recv_text.txt: type: text\nsender: macbook.example\ntext: agent says hi (no NL).
	got := recvBodyString(&wire.AgentItem{
		Type:   "text",
		Sender: "macbook.example",
		Text:   wire.Ptr("agent says hi"),
	})
	if got != want {
		t.Fatalf("/recv text body = %q, want fixture %q", got, want)
	}
	if strings.HasSuffix(got, "\n") {
		t.Fatalf("/recv body has a trailing newline (banned): %q", got)
	}
	// Colon-SPACE separator on every label.
	for _, label := range []string{"type: ", "sender: ", "text: "} {
		if !strings.Contains(got, label) {
			t.Fatalf("/recv body missing colon-space label %q: %q", label, got)
		}
	}
	// text MUST be last: nothing after the text value.
	ti := strings.LastIndex(got, "text: ")
	if ti < 0 {
		t.Fatalf("no text: label in %q", got)
	}
}

// TestInteropRecvImageBodyMatchesFixture asserts the /recv image body matches
// recv_image.txt: type/sender/path, a path: label (image), and NO text: label.
func TestInteropRecvImageBodyMatchesFixture(t *testing.T) {
	want := string(readInterop(t, "recv_image.txt"))
	got := recvBodyString(&wire.AgentItem{
		Type:   "image",
		Sender: "macbook.example",
		Path:   wire.Ptr("/home/agent/.local/state/clipbeam/agent-inbox/agentshot.png"),
	})
	if got != want {
		t.Fatalf("/recv image body = %q, want fixture %q", got, want)
	}
	if strings.Contains(got, "text: ") {
		t.Fatalf("image /recv body must not carry text:: %q", got)
	}
}

// TestInteropRecvMultilineTextLast asserts the recv_text_multiline.txt invariant: a
// text value carrying embedded newlines and a colon stays unambiguous because text is
// LAST (everything after the `text: ` label is the text, PLAN §12.5).
func TestInteropRecvMultilineTextLast(t *testing.T) {
	want := string(readInterop(t, "recv_text_multiline.txt"))
	// The multiline text is everything after "text: " in the fixture.
	idx := strings.Index(want, "text: ")
	if idx < 0 {
		t.Fatal("fixture has no text: label")
	}
	multiline := want[idx+len("text: "):]
	got := recvBodyString(&wire.AgentItem{
		Type:   "text",
		Sender: "macbook.example",
		Text:   wire.Ptr(multiline),
	})
	if got != want {
		t.Fatalf("/recv multiline body = %q, want fixture %q", got, want)
	}
}

// TestInteropLastNoTrailingNewline asserts last_path.txt is a bare absolute path with NO
// trailing newline — the contract $(clipbeam last) depends on (PLAN §12.5). The fixture
// is the captured /last body; the Go /last writer must produce the identical bare form.
func TestInteropLastNoTrailingNewline(t *testing.T) {
	fixture := string(readInterop(t, "last_path.txt"))
	if strings.HasSuffix(fixture, "\n") {
		t.Fatalf("last_path.txt fixture itself has a trailing newline: %q", fixture)
	}
	// Drive the store + /last and confirm the Go writer emits a bare no-NL path.
	h := newInteropHarness(t, false)
	want := filepath.Join(h.saveDir, "screenshot.png")
	if err := h.store.FinishClipboard("macbook.example", []string{want}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, h.baseURL+"/last", nil)
	req.Header.Set(tokenHeader, testToken)
	body := readBody(t, h.do(t, req))
	if strings.HasSuffix(body, "\n") {
		t.Fatalf("/last body %q has a trailing newline (banned)", body)
	}
	if body != want {
		t.Fatalf("/last body = %q, want %q", body, want)
	}
}

// TestInteropHealthKeyNames asserts the /health body carries the FROZEN key names
// (ok,app,version,host,platform) — the only frozen part; the values are the CLI's own
// (response_health.json keeps the Swift VALUES; the Go CLI substitutes its own). The
// fixture proves the key SET; this test proves the Go server emits the same key set
// with ok==true (PLAN §12.5).
func TestInteropHealthKeyNames(t *testing.T) {
	// The captured fixture must contain exactly the frozen keys.
	fixture := string(readInterop(t, "response_health.json"))
	for _, key := range []string{`"ok"`, `"app"`, `"version"`, `"host"`, `"platform"`} {
		if !strings.Contains(fixture, key) {
			t.Fatalf("response_health.json missing frozen key %s", key)
		}
	}
	// The Go server's /health must carry the same key names + ok:true (values are its own).
	h := newInteropHarness(t, false)
	req, _ := http.NewRequest(http.MethodGet, h.baseURL+"/health", nil)
	body := readBody(t, h.do(t, req))
	for _, key := range []string{`"ok":`, `"app":`, `"version":`, `"host":`, `"platform":`} {
		if !strings.Contains(body, key) {
			t.Fatalf("Go /health body %q missing frozen key %s", body, key)
		}
	}
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("Go /health body %q must report ok:true", body)
	}
}
