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
	"testing"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/store"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// smallCeilingServer builds a server with a tiny maxBytes so the RAW-WIRE hard ceiling
// is small enough to trip MaxBytesReader with a modest body. It returns the base URL.
func smallCeilingServer(t *testing.T, maxBytes int) (string, *testHarness) {
	t.Helper()
	dir := t.TempDir()
	saveDir := filepath.Join(dir, "save")
	if err := os.MkdirAll(saveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(store.StoreConfig{
		SaveDir: saveDir, AgentInboxDir: filepath.Join(dir, "inbox"),
		LastPathFile: filepath.Join(dir, "last_path"), RecentsFile: filepath.Join(dir, "recents.json"),
		LongTextThreshold: 8192, MaxBytes: maxBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.SaveDir = saveDir
	cfg.MaxBytes = maxBytes
	srv := New(Options{
		Config: cfg, Token: testToken, Store: st,
		Listen: ListenLoopbackTCP, PeerHost: "", AppVersion: "test",
		TempDir: filepath.Join(dir, "tmp"),
	})
	ln := mustListenLoopback(t)
	serveOn(t, srv, ln)
	return "http://" + ln.Addr().String(), &testHarness{srv: srv, saveDir: saveDir, store: st}
}

// TestAgentSendTextRunningCeiling413 drives the MaxBytesReader running-byte trip →
// asMaxBytes → explicit 413 on the /agent-send text path (PLAN §3.8c/§3.11): a body
// whose declared Content-Length is UNKNOWN (chunked) but whose actual bytes overshoot
// the hard ceiling trips http.MaxBytesReader mid-read, and agentSendText maps the
// *http.MaxBytesError via asMaxBytes to the explicit 413. A tiny maxBytes keeps the
// ceiling small so the overshoot is cheap.
func TestAgentSendTextRunningCeiling413(t *testing.T) {
	base, _ := smallCeilingServer(t, 8) // ceiling = 8*4/3 + 65536 = 65546
	addr := strings.TrimPrefix(base, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	ceiling := wire.HardCeiling(8)
	// Chunked transfer (no Content-Length) so the pre-read declared-CL 413 gate is
	// bypassed and the running MaxBytesReader(ceiling) is the one that trips.
	head := fmt.Sprintf(
		"POST /agent-send HTTP/1.1\r\nHost: %s\r\n%s: %s\r\n%s: agent\r\n%s: text\r\nTransfer-Encoding: chunked\r\n\r\n",
		addr, tokenHeader, testToken, channelHeader, kindHeader,
	)
	if _, err := conn.Write([]byte(head)); err != nil {
		t.Fatal(err)
	}
	// Stream chunks past the ceiling.
	chunk := strings.Repeat("Z", 8192)
	chunkFrame := fmt.Sprintf("%x\r\n%s\r\n", len(chunk), chunk)
	sent := 0
	for sent <= ceiling+8192 {
		if _, err := conn.Write([]byte(chunkFrame)); err != nil {
			break // server closed after tripping the ceiling — acceptable
		}
		sent += len(chunk)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	raw, _ := io.ReadAll(conn)
	if !strings.Contains(string(raw), "413") {
		t.Fatalf("agent-send overshoot response = %q, want a 413", firstLine(string(raw)))
	}
}

// TestClipNonASCIIName drives a non-ASCII (multibyte UTF-8) filename through the full
// /clip stack: the streaming parser carries the raw UTF-8 name bytes, the sanitizer maps
// each non-allowlisted byte to '_', and the save succeeds → 200. (The \uXXXX escape
// MACHINERY is unit-tested directly in parse_test.go's TestJSONStringReaderEscapes; this
// asserts the end-to-end stack tolerates a multibyte name.)
func TestClipNonASCIIName(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")
	b64 := base64.StdEncoding.EncodeToString([]byte("u"))
	body := fmt.Sprintf(`{"version":1,"sender":"t","items":[{"kind":"image","name":"aéb😀.png","bytesB64":%q}]}`, b64)
	req := clipReq(t, h.baseURL, body, testToken, true)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("non-ASCII name /clip = %d body=%q, want 200", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)
}

// TestClipExplicitNullOptionals drives readJSONStringOrNull / tryConsumeNull: an item
// with explicit JSON nulls for name/uti/mime/text alongside a bytesB64 must decode (nulls
// tolerated as absent) and save → 200 (PLAN §3.5).
func TestClipExplicitNullOptionals(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")
	b64 := base64.StdEncoding.EncodeToString([]byte("payload"))
	body := fmt.Sprintf(`{"version":1,"channel":null,"sender":"t","items":[`+
		`{"kind":"image","name":null,"uti":null,"mime":null,"text":null,"bytesB64":%q}]}`, b64)
	req := clipReq(t, h.baseURL, body, testToken, true)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("explicit-null /clip = %d body=%q, want 200", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)
}

// TestClipNullBytesB64Is400 drives parseOneItem's null-bytesB64 branch: a null bytesB64
// is treated as absent (no temp payload file), so an image item arrives at
// decodeAndIngest with an empty tmpPath; os.Open("") fails → ingest.ErrMalformed → 400.
// An image/file item with no payload is malformed (PLAN §3.11).
func TestClipNullBytesB64Is400(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")
	body := `{"version":1,"sender":"t","items":[{"kind":"image","name":"z.png","bytesB64":null}]}`
	req := clipReq(t, h.baseURL, body, testToken, true)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("null-bytesB64 image /clip = %d body=%q, want 400 (no payload is malformed)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)
}

// TestClipMalformedShapes drives the streaming parser's error branches (PLAN §3.11): a
// non-object root, a bad top-level key separator, a truncated items array, a bad item
// separator, a non-object item, and a malformed nested unknown-key value. Each must map
// to a 400, never panic.
func TestClipMalformedShapes(t *testing.T) {
	bodies := map[string]string{
		"non-object root":     `[1,2,3]`,
		"bad top separator":   `{"version":1 "sender":"t"}`,
		"truncated items":     `{"version":1,"sender":"t","items":[{"kind":"text"`,
		"bad item separator":  `{"version":1,"sender":"t","items":[{"kind":"text","text":"a"} {"kind":"text"}]}`,
		"non-object item":     `{"version":1,"sender":"t","items":["notanobject"]}`,
		"non-string key":      `{1:2}`,
		"missing items":       `{"version":1,"sender":"t"}`,
		"unterminated string": `{"version":1,"sender":"t","items":[{"kind":"text","text":"unterminated`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, ListenTailscale, "127.0.0.1")
			req := clipReq(t, h.baseURL, body, testToken, true)
			resp := h.do(t, req)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("malformed %q = %d, want 400", name, resp.StatusCode)
			}
			_ = readBody(t, resp)
		})
	}
}

// TestClipDeepNestedUnknownKey drives skipContainer's nested-container recursion: a
// deeply nested unknown top-level key must be skipped structurally and the known fields
// still ingested → 200.
func TestClipDeepNestedUnknownKey(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")
	b64 := base64.StdEncoding.EncodeToString([]byte("p"))
	body := fmt.Sprintf(`{"meta":{"a":{"b":{"c":[1,[2,[3,{"d":"e"}]]]}}},"version":1,"sender":"t",`+
		`"items":[{"kind":"image","name":"n.png","bytesB64":%q}]}`, b64)
	req := clipReq(t, h.baseURL, body, testToken, true)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deep-nested unknown key /clip = %d body=%q, want 200", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)
}

// TestWaitResumes200 drives handleWait's 200 path (a concurrent clipboard save resumes
// the parked /wait with the bare path, no trailing newline). This covers the success
// branch of handleWait + writeBarePath.
func TestWaitResumes200(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	want := filepath.Join(h.saveDir, "resumed.png")
	done := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, h.baseURL+"/wait", nil)
		req.Header.Set(tokenHeader, testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- "ERR"
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		done <- string(b)
	}()
	time.Sleep(150 * time.Millisecond)
	if err := h.store.FinishClipboard("t", []string{want}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got != want {
			t.Fatalf("/wait body = %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("/wait did not resume")
	}
}

// TestWaitWrongMethod405 + TestRecvWrongMethod405 cover the method guards on /wait,/recv.
func TestWaitWrongMethod405(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/wait", nil)
	req.Header.Set(tokenHeader, testToken)
	if resp := h.do(t, req); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /wait = %d, want 405", resp.StatusCode)
	}
}

func TestRecvWrongMethod405(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/recv", nil)
	req.Header.Set(tokenHeader, testToken)
	if resp := h.do(t, req); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /recv = %d, want 405", resp.StatusCode)
	}
}

func TestLastWrongMethod405(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/last", nil)
	req.Header.Set(tokenHeader, testToken)
	if resp := h.do(t, req); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /last = %d, want 405", resp.StatusCode)
	}
}

func TestAgentSendWrongMethod405(t *testing.T) {
	h := newHarness(t, ListenLoopbackTCP, "")
	req, _ := http.NewRequest(http.MethodGet, h.baseURL+"/agent-send", nil)
	req.Header.Set(tokenHeader, testToken)
	if resp := h.do(t, req); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /agent-send = %d, want 405", resp.StatusCode)
	}
}

func TestClipWrongMethod405(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")
	req, _ := http.NewRequest(http.MethodGet, h.baseURL+"/clip", nil)
	req.Header.Set(tokenHeader, testToken)
	if resp := h.do(t, req); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /clip = %d, want 405", resp.StatusCode)
	}
}

// TestServeProductionFullPath drives the production Serve(ctx) entry point end to end:
// the real bind + the ctx-cancel graceful-shutdown goroutine. It binds a unix socket
// (a known path, unlike an ephemeral TCP port Serve never reports back), makes a
// /health request over it, then cancels and asserts Serve returns cleanly.
func TestServeProductionFullPath(t *testing.T) {
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
	sock := shortSock(t)
	srv := New(Options{
		Config: cfg, Token: testToken, Store: st,
		Listen: ListenUnixSocket, SocketPath: sock, AppVersion: "test",
		TempDir: filepath.Join(dir, "tmp"),
	})

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	// Wait for the socket to appear, then make a /health request over it.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(sock); statErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", sock)
		},
	}}
	resp, err := hc.Get("http://unix/health")
	if err != nil {
		t.Fatalf("/health over unix socket via Serve: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Serve /health = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Cancel → Serve must return (graceful shutdown via the ctx.Done goroutine).
	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned error after cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}
}
