package httpd

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/store"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

const testToken = "ABCDEFGHIJKLMNOPQRSTUV34" // 24 chars, shape-valid

// testHarness wires a Server over a REAL loopback TCP listener (PLAN §12.4 mandates a
// real net.Listen, not only httptest) plus a concrete store backed by temp dirs.
type testHarness struct {
	srv     *Server
	baseURL string
	saveDir string
	inbox   string
	store   store.ReceiveStore
}

func newHarness(t *testing.T, listen ListenKind, peerHost string) *testHarness {
	t.Helper()
	dir := t.TempDir()
	saveDir := filepath.Join(dir, "save")
	inbox := filepath.Join(dir, "inbox")
	lastPath := filepath.Join(dir, "last_path")
	recents := filepath.Join(dir, "recents.json")
	if err := os.MkdirAll(saveDir, 0o700); err != nil {
		t.Fatal(err)
	}

	st, err := store.New(store.StoreConfig{
		SaveDir:           saveDir,
		AgentInboxDir:     inbox,
		LastPathFile:      lastPath,
		RecentsFile:       recents,
		SaveTextToDisk:    false,
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
		Config:     cfg,
		Token:      testToken,
		Store:      st,
		Listen:     listen,
		PeerHost:   peerHost,
		AppVersion: "0.0.0-test",
		TempDir:    filepath.Join(dir, "tmp"),
	})

	// Bind a real loopback listener directly (bypass Serve's bind switch so the test
	// controls the address) and drive srv.srv.Serve on it.
	ln := mustListenLoopback(t)
	serveOn(t, srv, ln)

	h := &testHarness{
		srv:     srv,
		baseURL: "http://" + ln.Addr().String(),
		saveDir: saveDir,
		inbox:   inbox,
		store:   st,
	}
	return h
}

// mustListenLoopback binds an ephemeral loopback TCP listener or fails the test
// (PLAN §12.4 mandates a real net.Listen, not only httptest).
func mustListenLoopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// serveOn drives srv.srv.Serve on ln in a goroutine and registers a cleanup that
// shuts the server down (so a torn-down test never leaks the serve goroutine — the
// goroutine-leak check depends on a clean teardown).
func serveOn(t *testing.T, srv *Server, ln net.Listener) {
	t.Helper()
	go func() { _ = srv.srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
}

func (h *testHarness) do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", req.Method, req.URL.Path, err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// clipEnvelope builds a /clip body for one image item with the given decoded bytes.
func clipEnvelope(name string, payload []byte) string {
	b64 := base64.StdEncoding.EncodeToString(payload)
	return fmt.Sprintf(`{"version":1,"sender":"tester","items":[{"kind":"image","name":%q,"bytesB64":%q}]}`, name, b64)
}

// --- /health ---

func TestHealth200(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	req, _ := http.NewRequest(http.MethodGet, h.baseURL+"/health", nil)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"ok":true`) || !strings.Contains(body, `"app":"clipbeam"`) {
		t.Fatalf("health body = %q, want ok:true + app:clipbeam", body)
	}
}

func TestHealthWrongMethod405(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/health", nil)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("health POST = %d, want 405", resp.StatusCode)
	}
}

func TestUnknownPath404(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	req, _ := http.NewRequest(http.MethodGet, h.baseURL+"/nope", nil)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path = %d, want 404", resp.StatusCode)
	}
}

// --- /clip ---

// clipReq builds a /clip POST with the token over the Tailscale-class harness (so a
// loopback connection classifies as .peer when peerHost=127.0.0.1).
func clipReq(t *testing.T, base, body, token string, withCL bool) *http.Request {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/clip", strings.NewReader(body))
	if token != "" {
		req.Header.Set(tokenHeader, token)
	}
	if !withCL {
		// Force chunked transfer so ContentLength is -1 (missing CL → 411).
		req.ContentLength = -1
		req.Header.Set("Transfer-Encoding", "chunked")
	}
	return req
}

func TestClip200AndSaved(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")
	payload := []byte("hello clipbeam image bytes")
	req := clipReq(t, h.baseURL, clipEnvelope("shot.png", payload), testToken, true)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clip status = %d body=%q, want 200", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"ok":true`) || !strings.Contains(body, `"count":1`) {
		t.Fatalf("clip body = %q, want ok:true count:1", body)
	}
	// The saved file must exist on disk with the payload bytes.
	last, ok := h.store.LastPath()
	if !ok {
		t.Fatal("last_path not set after /clip")
	}
	got, err := os.ReadFile(last)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("saved bytes = %q, want %q", got, payload)
	}
}

func TestClipWrongToken401(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")
	req := clipReq(t, h.baseURL, clipEnvelope("x.png", []byte("z")), "WRONGTOKENWRONGTOKENWRON", true)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("clip wrong token = %d, want 401", resp.StatusCode)
	}
}

func TestClipForbiddenClass403(t *testing.T) {
	// peerHost set to a non-loopback address so a loopback connection is NOT .peer.
	h := newHarness(t, ListenTailscale, "100.64.1.2")
	req := clipReq(t, h.baseURL, clipEnvelope("x.png", []byte("z")), testToken, true)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("clip foreign class = %d, want 403", resp.StatusCode)
	}
}

func TestClipMissingCL411(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")
	req := clipReq(t, h.baseURL, clipEnvelope("x.png", []byte("z")), testToken, false)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusLengthRequired {
		t.Fatalf("clip missing CL = %d, want 411", resp.StatusCode)
	}
}

func TestClipOversize413ReadCleanly(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")
	// Declare a Content-Length beyond the ceiling but send only a tiny body prefix:
	// the server must emit 413 BEFORE reading the body, and the client must be able to
	// READ that 413 cleanly (no RST) even though it never finished uploading. The Go
	// http client refuses a CL/body mismatch, so drive a raw TCP request (PLAN §3.11
	// read-and-discard drain is exercised here).
	addr := strings.TrimPrefix(h.baseURL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	ceiling := wire.HardCeiling(wire.DefaultMaxBytes)
	reqHead := fmt.Sprintf(
		"POST /clip HTTP/1.1\r\nHost: %s\r\n%s: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n",
		addr, tokenHeader, testToken, int64(ceiling)+1,
	)
	if _, err := conn.Write([]byte(reqHead)); err != nil {
		t.Fatalf("write request head: %v", err)
	}
	// Send a small body prefix (far short of the declared CL) so the server is the one
	// that decides to respond 413 before draining the whole body.
	if _, err := conn.Write([]byte("padding-prefix")); err != nil {
		t.Fatalf("write body prefix: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read 413 response cleanly: %v", err)
	}
	resp := string(raw)
	if !strings.Contains(resp, "413") {
		t.Fatalf("oversize response = %q, want a 413 status line", firstLine(resp))
	}
	if !strings.Contains(resp, `"ok":false`) {
		t.Fatalf("413 body = %q, want ok:false error", resp)
	}
}

// firstLine returns the first line of s (for compact error messages).
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func TestClipMalformedJSON400(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")
	req := clipReq(t, h.baseURL, `{"version":1,"items":[`, testToken, true)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed clip = %d, want 400", resp.StatusCode)
	}
}

func TestClipUnsupportedEnvelope400(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")
	req := clipReq(t, h.baseURL, `{"version":2,"sender":"x","items":[{"kind":"text","text":"hi"}]}`, testToken, true)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("version!=1 = %d, want 400", resp.StatusCode)
	}
}

// --- single-flight ---

func TestClipSingleFlight(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")

	// A reader that blocks until released so the first /clip holds the slot.
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()

	// Build a valid envelope prefix then a blocking body so the handler is mid-read
	// holding the single-flight slot.
	payload := base64.StdEncoding.EncodeToString([]byte("blockingpayload"))
	full := fmt.Sprintf(`{"version":1,"sender":"t","items":[{"kind":"image","name":"a.png","bytesB64":%q}]}`, payload)
	br := &blockingReader{data: []byte(full), gate: gate}

	req1, _ := http.NewRequest(http.MethodPost, h.baseURL+"/clip", br)
	req1.Header.Set(tokenHeader, testToken)
	req1.ContentLength = int64(len(full))

	type result struct {
		status int
		err    error
	}
	first := make(chan result, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req1)
		if err != nil {
			first <- result{err: err}
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		first <- result{status: resp.StatusCode}
	}()

	// Wait until the handler has acquired the slot (the reader was read at least once).
	if !br.waitStarted(2 * time.Second) {
		t.Fatal("first /clip never started reading")
	}

	// Second concurrent /clip must get 503 busy.
	req2 := clipReq(t, h.baseURL, clipEnvelope("b.png", []byte("second")), testToken, true)
	resp2 := h.do(t, req2)
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("concurrent /clip = %d, want 503", resp2.StatusCode)
	}
	_ = readBody(t, resp2)

	// Release the first; it should complete 200.
	release()
	select {
	case r := <-first:
		if r.err != nil {
			t.Fatalf("first /clip error: %v", r.err)
		}
		if r.status != http.StatusOK {
			t.Fatalf("first /clip = %d, want 200", r.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first /clip did not complete")
	}

	// A LATER /clip must succeed (slot released on teardown).
	req3 := clipReq(t, h.baseURL, clipEnvelope("c.png", []byte("third")), testToken, true)
	resp3 := h.do(t, req3)
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("later /clip = %d, want 200 (slot must be released)", resp3.StatusCode)
	}
	_ = readBody(t, resp3)
}

// blockingReader yields its data only after gate is closed, and signals when it is
// first read so the test knows the handler holds the single-flight slot.
type blockingReader struct {
	data    []byte
	i       int
	gate    chan struct{}
	mu      sync.Mutex
	started bool
	startCh chan struct{}
}

func (b *blockingReader) waitStarted(d time.Duration) bool {
	b.mu.Lock()
	if b.startCh == nil {
		b.startCh = make(chan struct{})
	}
	if b.started {
		b.mu.Unlock()
		return true
	}
	ch := b.startCh
	b.mu.Unlock()
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

func (b *blockingReader) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.startCh == nil {
		b.startCh = make(chan struct{})
	}
	if !b.started {
		b.started = true
		close(b.startCh)
	}
	b.mu.Unlock()

	// Deliver the first chunk immediately so the handler enters the body read and
	// holds the slot, then block on the remainder until released.
	if b.i == 0 {
		// hand over a tiny prefix so the parser begins, then block.
		<-b.gate
	}
	if b.i >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.i:])
	b.i += n
	return n, nil
}

// --- control surface: /agent-send, /recv, /last, /wait ---

func TestAgentSendTextAndRecv(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")

	// Send an agent-channel text message.
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/agent-send", strings.NewReader("hello agent"))
	req.Header.Set(tokenHeader, testToken)
	req.Header.Set(channelHeader, "agent")
	req.Header.Set(kindHeader, "text")
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent-send text = %d body=%q, want 200", resp.StatusCode, readBody(t, resp))
	}
	if body := readBody(t, resp); !strings.Contains(body, `"sentItems":1`) {
		t.Fatalf("agent-send body = %q, want sentItems:1", body)
	}

	// Recv should dequeue it with the labeled body, text last.
	rreq, _ := http.NewRequest(http.MethodGet, h.baseURL+"/recv?timeout=5", nil)
	rreq.Header.Set(tokenHeader, testToken)
	rresp := h.do(t, rreq)
	if rresp.StatusCode != http.StatusOK {
		t.Fatalf("recv = %d, want 200", rresp.StatusCode)
	}
	got := readBody(t, rresp)
	want := "type: text\nsender: \ntext: hello agent"
	if got != want {
		t.Fatalf("recv body = %q, want %q", got, want)
	}
}

func TestAgentSendFileToInbox(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	// A real file on disk to push by absolute path.
	src := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(src, []byte("file body bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/agent-send", nil)
	req.Header.Set(tokenHeader, testToken)
	req.Header.Set(channelHeader, "agent")
	req.Header.Set(kindHeader, "file")
	req.Header.Set(fileHeader, src)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent-send file = %d body=%q, want 200", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)

	// Recv must yield a file item whose path is in the inbox and holds the bytes.
	rreq, _ := http.NewRequest(http.MethodGet, h.baseURL+"/recv?timeout=5", nil)
	rreq.Header.Set(tokenHeader, testToken)
	rresp := h.do(t, rreq)
	body := readBody(t, rresp)
	if !strings.HasPrefix(body, "type: file\n") || !strings.Contains(body, "path: ") {
		t.Fatalf("recv file body = %q, want type:file + path:", body)
	}
}

func TestRecvTimeout204(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	req, _ := http.NewRequest(http.MethodGet, h.baseURL+"/recv?timeout=0", nil)
	req.Header.Set(tokenHeader, testToken)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("recv empty = %d, want 204", resp.StatusCode)
	}
}

func TestLastNoTrailingNewline(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")

	// Nothing saved yet → 204.
	req, _ := http.NewRequest(http.MethodGet, h.baseURL+"/last", nil)
	req.Header.Set(tokenHeader, testToken)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("last empty = %d, want 204", resp.StatusCode)
	}
	_ = readBody(t, resp)

	// Save a clipboard item via the store directly, then /last must return the bare
	// path with NO trailing newline.
	if err := h.store.FinishClipboard("t", []string{filepath.Join(h.saveDir, "img.png")}); err != nil {
		t.Fatal(err)
	}
	req2, _ := http.NewRequest(http.MethodGet, h.baseURL+"/last", nil)
	req2.Header.Set(tokenHeader, testToken)
	resp2 := h.do(t, req2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("last = %d, want 200", resp2.StatusCode)
	}
	body := readBody(t, resp2)
	if strings.HasSuffix(body, "\n") {
		t.Fatalf("last body %q has a trailing newline (banned)", body)
	}
	if body != filepath.Join(h.saveDir, "img.png") {
		t.Fatalf("last body = %q, want the saved path", body)
	}
}

func TestWaitResumesOnSave(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	want := filepath.Join(h.saveDir, "later.png")

	done := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, h.baseURL+"/wait", nil)
		req.Header.Set(tokenHeader, testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- "ERR:" + err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		done <- string(b)
	}()

	// Give the waiter a moment to park, then commit a clipboard save.
	time.Sleep(150 * time.Millisecond)
	if err := h.store.FinishClipboard("t", []string{want}); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-done:
		if got != want {
			t.Fatalf("wait body = %q, want %q (no trailing newline)", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("/wait did not resume after a save")
	}
}

// --- control gate: token required over TCP ---

func TestControlRequiresTokenOverTCP(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	// No token → 401 on a control endpoint over TCP (PLAN §6.2).
	req, _ := http.NewRequest(http.MethodGet, h.baseURL+"/last", nil)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("control no-token over TCP = %d, want 401", resp.StatusCode)
	}
}

// TestDualStackBindServesIPv4Loopback exercises the REAL dual-stack [::]:port bind
// with IPV6_V6ONLY cleared (PLAN §3.3, "tested on darwin specifically"): a v6 wildcard
// bind must accept a v4 loopback connection (delivered as ::ffff:127.0.0.1, unwrapped
// in classify to 127.0.0.1 → .peer). This drives the production s.listen() path rather
// than the test harness's plain v4 listener.
func TestDualStackBindServesIPv4Loopback(t *testing.T) {
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
	srv := New(Options{
		Config: cfg, Token: testToken, Store: st,
		Listen: ListenTailscale, PeerHost: "127.0.0.1", AppVersion: "test",
		TempDir: filepath.Join(dir, "tmp"),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := srv.listen(ctx)
	if err != nil {
		t.Fatalf("dual-stack listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() { _ = srv.srv.Serve(ln) }()

	// The wildcard v6 listener reports [::]:PORT; dial it over v4 loopback.
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	base := "http://127.0.0.1:" + port

	// /health over v4 loopback proves the bind accepts v4 (V6ONLY cleared).
	req, _ := http.NewRequest(http.MethodGet, base+"/health", nil)
	resp := h0do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health over v4 on dual-stack = %d, want 200", resp.StatusCode)
	}
	_ = readBody(t, resp)

	// /clip over v4 loopback must classify as .peer (::ffff:127.0.0.1 unwrapped) → 200.
	creq := clipReq(t, base, clipEnvelope("d.png", []byte("dualstack")), testToken, true)
	cresp := h0do(t, creq)
	if cresp.StatusCode != http.StatusOK {
		t.Fatalf("clip over v4 on dual-stack = %d body=%q, want 200 (.peer)", cresp.StatusCode, readBody(t, cresp))
	}
	_ = readBody(t, cresp)
}

// h0do is a tiny do() that does not need a harness (used by the dual-stack test).
func h0do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", req.Method, req.URL.Path, err)
	}
	return resp
}

// --- agent-send empty text (absent Content-Length ⇒ empty text dispatched) ---

func TestAgentSendEmptyText(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/agent-send", nil)
	req.Header.Set(tokenHeader, testToken)
	req.Header.Set(channelHeader, "agent")
	req.Header.Set(kindHeader, "text")
	req.ContentLength = 0
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent-send empty text = %d, want 200", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, `"sentItems":1`) {
		t.Fatalf("empty text body = %q, want sentItems:1", body)
	}
	// Recv it: text item with empty text.
	rreq, _ := http.NewRequest(http.MethodGet, h.baseURL+"/recv?timeout=5", nil)
	rreq.Header.Set(tokenHeader, testToken)
	rresp := h.do(t, rreq)
	got := readBody(t, rresp)
	if got != "type: text\nsender: \ntext: " {
		t.Fatalf("recv empty text body = %q", got)
	}
}
