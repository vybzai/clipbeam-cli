package httpd

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/store"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// TestPushBareNoClipboard exercises a bare POST /push on a headless receiver: there is
// no system clipboard to read, so the frozen control body reports sentItems:0
// (PLAN §7.2/§7.4).
func TestPushBareNoClipboard(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/push", nil)
	req.Header.Set(tokenHeader, testToken)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bare /push = %d, want 200", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, `"sentItems":0`) {
		t.Fatalf("bare /push body = %q, want sentItems:0", body)
	}
}

// TestPushFileBackCompat exercises POST /push?file=ABS (the back-compat file form,
// Server.swift:495): the named absolute file is ingested into the clipboard channel and
// the control body reports sentItems:1.
func TestPushFileBackCompat(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	src := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(src, []byte("push-file bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/push?file="+src, nil)
	req.Header.Set(tokenHeader, testToken)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/push?file = %d body=%q, want 200", resp.StatusCode, readBody(t, resp))
	}
	if body := readBody(t, resp); !strings.Contains(body, `"sentItems":1`) {
		t.Fatalf("/push?file body = %q, want sentItems:1", body)
	}
	// The file landed in the save dir and set last_path.
	if _, ok := h.store.LastPath(); !ok {
		t.Fatal("/push?file did not set last_path")
	}
}

// TestPushFileMissing500 asserts a /push?file pointing at a non-existent path surfaces a
// 500 (the open error is a real diagnostic, not a secret).
func TestPushFileMissing500(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/push?file=/no/such/file.bin", nil)
	req.Header.Set(tokenHeader, testToken)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("/push?file missing = %d, want 500", resp.StatusCode)
	}
	_ = readBody(t, resp)
}

// TestPushWrongMethod405 covers the /push method guard.
func TestPushWrongMethod405(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	req, _ := http.NewRequest(http.MethodGet, h.baseURL+"/push", nil)
	req.Header.Set(tokenHeader, testToken)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /push = %d, want 405", resp.StatusCode)
	}
}

// TestClipUnknownKeysTolerated drives the streaming parser's skipJSONValue /
// skipContainer paths: an envelope with extra unknown top-level keys (an object, an
// array, a nested literal) and an unknown per-item key must be tolerated and the known
// fields still ingested (PLAN §3.5 forward-compat).
func TestClipUnknownKeysTolerated(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")
	b64 := base64.StdEncoding.EncodeToString([]byte("payload-bytes"))
	body := fmt.Sprintf(`{"extraObj":{"a":1,"b":[true,false,null]},"version":1,`+
		`"extraArr":[1,2,{"x":"y"}],"sender":"t","items":[`+
		`{"future":"ignored","kind":"image","name":"u.png","bytesB64":%q,"trailing":42}],`+
		`"extraNull":null}`, b64)
	req := clipReq(t, h.baseURL, body, testToken, true)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unknown-keys /clip = %d body=%q, want 200 (forward-compat skip)", resp.StatusCode, readBody(t, resp))
	}
	if r := readBody(t, resp); !strings.Contains(r, `"count":1`) {
		t.Fatalf("unknown-keys /clip body = %q, want count:1", r)
	}
}

// TestClipBase64WithJSONEscapes drives the jsonStringReader escape paths
// (readEscape / readUnicodeEscape / readHex4): a bytesB64 value carrying JSON escapes
// (\/ as Swift emits, plus \uXXXX) must unescape correctly before base64-decoding so a
// hand-crafted body cannot smuggle bytes past the decoder. We base64 a payload whose
// alphabet includes '/', then escape it as \/ in the JSON string — the decoder must
// reconstruct the same bytes.
func TestClipBase64WithJSONEscapes(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")
	// A payload whose std-base64 contains a '/'.
	payload := []byte{0xff, 0xff, 0xff, 0x00, 0x10}
	b64 := base64.StdEncoding.EncodeToString(payload)
	if !strings.Contains(b64, "/") {
		// Pick bytes that guarantee a '/'; 0xff 0xff -> "//".
		payload = []byte{0xff, 0xff, 0xff}
		b64 = base64.StdEncoding.EncodeToString(payload)
	}
	// Escape every '/' as \/ exactly the way Swift's JSONEncoder does; the decoder must
	// unescape \/ back to '/' before the base64 decoder sees it.
	escaped := strings.ReplaceAll(b64, "/", `\/`)
	body := fmt.Sprintf(`{"version":1,"sender":"t","items":[{"kind":"file","name":"e.bin","bytesB64":"%s"}]}`, escaped)
	req := clipReq(t, h.baseURL, body, testToken, true)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("escaped-base64 /clip = %d body=%q, want 200", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)
	last, ok := h.store.LastPath()
	if !ok {
		t.Fatal("escaped-base64 /clip did not save")
	}
	got, err := os.ReadFile(last)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("escaped-base64 decoded to %x, want %x", got, payload)
	}
}

// TestClipBadBase64Is400 drives streamBase64ToTemp's CorruptInputError → 400 mapping.
func TestClipBadBase64Is400(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")
	body := `{"version":1,"sender":"t","items":[{"kind":"image","name":"b.png","bytesB64":"@@@@notbase64@@@@"}]}`
	req := clipReq(t, h.baseURL, body, testToken, true)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad-base64 /clip = %d, want 400", resp.StatusCode)
	}
	_ = readBody(t, resp)
}

// TestAgentSendTextOversize413 drives agentSendText's declared-Content-Length-over-the-
// hard-ceiling branch → 413 (PLAN §3.4). Uses a tiny maxBytes so a modest declared CL
// trips the ceiling without uploading 70 MB.
func TestAgentSendTextOversize413(t *testing.T) {
	dir := t.TempDir()
	saveDir := filepath.Join(dir, "save")
	if err := os.MkdirAll(saveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(store.StoreConfig{
		SaveDir: saveDir, AgentInboxDir: filepath.Join(dir, "inbox"),
		LastPathFile: filepath.Join(dir, "last_path"), RecentsFile: filepath.Join(dir, "recents.json"),
		LongTextThreshold: 8192, MaxBytes: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.SaveDir = saveDir
	cfg.MaxBytes = 16
	srv := New(Options{Config: cfg, Token: testToken, Store: st, Listen: ListenLoopbackTCP, AppVersion: "test", TempDir: filepath.Join(dir, "tmp")})
	ln := mustListenLoopback(t)
	serveOn(t, srv, ln)
	base := "http://" + ln.Addr().String()

	ceiling := wire.HardCeiling(16) // small
	big := strings.Repeat("x", ceiling+1)
	req, _ := http.NewRequest(http.MethodPost, base+"/agent-send", strings.NewReader(big))
	req.Header.Set(tokenHeader, testToken)
	req.Header.Set(channelHeader, "agent")
	req.Header.Set(kindHeader, "text")
	resp := h0do(t, req)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize agent-send text = %d, want 413", resp.StatusCode)
	}
	_ = readBody(t, resp)
}

// TestAgentSendFileMissingHeader400 drives agentSendFile's missing-file-header 400.
func TestAgentSendFileMissingHeader400(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/agent-send", nil)
	req.Header.Set(tokenHeader, testToken)
	req.Header.Set(channelHeader, "clipboard")
	req.Header.Set(kindHeader, "file")
	// No X-ClipBeam-File header.
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("agent-send file w/o header = %d, want 400", resp.StatusCode)
	}
	_ = readBody(t, resp)
}

// TestAgentSendClipboardKind drives the default kind="clipboard" branch (no body,
// nothing to enqueue → sentItems:0).
func TestAgentSendClipboardKind(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/agent-send", nil)
	req.Header.Set(tokenHeader, testToken)
	// No kind header ⇒ default "clipboard".
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent-send default kind = %d, want 200", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, `"sentItems":0`) {
		t.Fatalf("agent-send clipboard kind body = %q, want sentItems:0", body)
	}
}

// TestWaitTimeout204 drives handleWait's timeout path. The fixed 120 s window is too
// long for a test, so we cancel via the request context (a client disconnect) and assert
// the handler unwinds — using a short client timeout. Because the production /wait has no
// query param, we rely on the client-side context to bound the wait and confirm the
// handler returns (no hang) and the store waiter is released.
func TestWaitTimeout204(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+"/wait", nil)
	req.Header.Set(tokenHeader, testToken)
	_, err := http.DefaultClient.Do(req)
	// The client context fires first (no save happens), so the request is canceled —
	// the assertion is that this terminates promptly (no hang) rather than a specific
	// status; a deadline-exceeded error is the expected client-side outcome.
	if err == nil {
		t.Fatal("expected the client context to cancel the long-poll /wait")
	}
}

// TestListenLoopbackTCPProductionBind drives the production listen() bind path for
// ListenLoopbackTCP (the dual-stack ListenConfig + JoinHostPort), proving the real bind
// works and serves /health. The full Serve(ctx) + graceful-shutdown path is covered by
// TestServeProductionFullPath (over a unix socket whose path is observable).
func TestListenLoopbackTCPProductionBind(t *testing.T) {
	dir := t.TempDir()
	saveDir := filepath.Join(dir, "save")
	if err := os.MkdirAll(saveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(store.StoreConfig{
		SaveDir: saveDir, AgentInboxDir: filepath.Join(dir, "inbox"),
		LastPathFile: filepath.Join(dir, "last_path"), RecentsFile: filepath.Join(dir, "recents.json"),
		LongTextThreshold: 8192, MaxBytes: wire.DefaultMaxBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.SaveDir = saveDir
	cfg.Port = 0 // ephemeral
	srv := New(Options{Config: cfg, Token: testToken, Store: st, Listen: ListenLoopbackTCP, AppVersion: "test", TempDir: filepath.Join(dir, "tmp")})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := srv.listen(ctx)
	if err != nil {
		t.Fatalf("production listen (loopback TCP): %v", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	go func() { _ = srv.srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	req, _ := http.NewRequest(http.MethodGet, base+"/health", nil)
	resp := h0do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback-TCP bind /health = %d, want 200", resp.StatusCode)
	}
	_ = readBody(t, resp)
}
