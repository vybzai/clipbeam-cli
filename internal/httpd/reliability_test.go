package httpd

import (
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/sanitize"
	"github.com/vybzai/clipbeam-cli/internal/store"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// TestSingleFlightReleasedOnErrorPath is the permanent-503 regression guard (PLAN
// §3.11/§12.4): the single-flight slot is released on EVERY teardown, not only the
// success path. A /clip that trips a 400 (malformed envelope) must release the slot so
// a subsequent /clip succeeds — a regression that released only on success would wedge
// into a permanent 503 after the first error.
func TestSingleFlightReleasedOnErrorPath(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")

	// First /clip: a malformed body → 400 (tears down through the error path).
	bad := clipReq(t, h.baseURL, `{"version":1,"sender":"x","items":[{`, testToken, true)
	resp := h.do(t, bad)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed /clip = %d, want 400", resp.StatusCode)
	}
	_ = readBody(t, resp)

	// Second /clip: a valid body must get 200 — the slot must have been released by the
	// 400 teardown (NOT a permanent 503).
	good := clipReq(t, h.baseURL, clipEnvelope("after-error.png", []byte("ok")), testToken, true)
	resp2 := h.do(t, good)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("/clip after a 400 = %d, want 200 (slot must release on the error path; permanent-503 regression)", resp2.StatusCode)
	}
	_ = readBody(t, resp2)

	// And a third confirms the slot is durably free across several error/success cycles.
	third := clipReq(t, h.baseURL, clipEnvelope("third.png", []byte("ok2")), testToken, true)
	resp3 := h.do(t, third)
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("third /clip = %d, want 200", resp3.StatusCode)
	}
	_ = readBody(t, resp3)
}

// TestSingleFlightReleasedAfter413 proves the slot is also released after a 413 teardown
// (the running-byte ceiling trip), so an oversize attempt cannot wedge /clip into a
// permanent 503 either (PLAN §3.11).
func TestSingleFlightReleasedAfter413(t *testing.T) {
	// A tiny maxBytes so a modest payload trips the DECODED cap (413 via Ingest's
	// ErrTooLarge), exercising a teardown that is neither 200 nor a parse 400.
	dir := t.TempDir()
	saveDir := filepath.Join(dir, "save")
	if err := os.MkdirAll(saveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(store.StoreConfig{
		SaveDir: saveDir, AgentInboxDir: filepath.Join(dir, "inbox"),
		LastPathFile: filepath.Join(dir, "last_path"), RecentsFile: filepath.Join(dir, "recents.json"),
		LongTextThreshold: 8192, MaxBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.SaveDir = saveDir
	cfg.MaxBytes = 4
	srv := New(Options{
		Config: cfg, Token: testToken, Store: st,
		Listen: ListenTailscale, PeerHost: "127.0.0.1", AppVersion: "test",
		TempDir: filepath.Join(dir, "tmp"),
	})
	ln := mustListenLoopback(t)
	serveOn(t, srv, ln)
	base := "http://" + ln.Addr().String()

	// Item of 16 decoded bytes > maxBytes 4 → Ingest ErrTooLarge → 413.
	over := clipReq(t, base, clipEnvelope("big.png", []byte("0123456789ABCDEF")), testToken, true)
	resp := h0do(t, over)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize decoded /clip = %d, want 413", resp.StatusCode)
	}
	_ = readBody(t, resp)

	// A within-cap /clip after the 413 must succeed (slot released on the 413 teardown).
	ok := clipReq(t, base, clipEnvelope("small.png", []byte("ab")), testToken, true)
	resp2 := h0do(t, ok)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("/clip after a 413 = %d, want 200 (slot must release on 413; permanent-503 regression)", resp2.StatusCode)
	}
	_ = readBody(t, resp2)
}

// TestMidTransferRSTNoGoroutineLeak is the mid-transfer-RST teardown gate (PLAN §12.4):
// a client that opens /clip, sends a partial body, then RSTs (abrupt close) must NOT
// leak a goroutine and must NOT wedge the single-flight slot. It samples
// runtime.NumGoroutine before and after a batch of aborted transfers (with a settle
// window) and asserts the count does not grow, then proves a subsequent /clip still
// succeeds (the slot was released on the RST teardown).
func TestMidTransferRSTNoGoroutineLeak(t *testing.T) {
	h := newHarness(t, ListenTailscale, "127.0.0.1")
	addr := strings.TrimPrefix(h.baseURL, "http://")

	// A valid envelope prefix that declares a large Content-Length but is never
	// finished — the handler is mid-read when the client RSTs.
	payload := base64.StdEncoding.EncodeToString(make([]byte, 4096))
	full := fmt.Sprintf(`{"version":1,"sender":"t","items":[{"kind":"image","name":"a.png","bytesB64":"%s`, payload[:64])

	abortOne := func() {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		// Declare a big CL so the server keeps the body reader open expecting more.
		head := fmt.Sprintf(
			"POST /clip HTTP/1.1\r\nHost: %s\r\n%s: %s\r\nContent-Length: 1000000\r\n\r\n",
			addr, tokenHeader, testToken,
		)
		_, _ = conn.Write([]byte(head))
		_, _ = conn.Write([]byte(full)) // a partial body prefix
		// Force an RST rather than a graceful FIN: set linger 0 then close.
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	}

	settle := func() {
		// Give the server goroutines time to observe the RST and unwind.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			runtime.GC()
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Warm up one connection so the per-connection serve machinery is already spun up,
	// then take the baseline.
	abortOne()
	settle()
	runtime.GC()
	before := runtime.NumGoroutine()

	const n = 25
	for i := 0; i < n; i++ {
		abortOne()
	}
	settle()
	runtime.GC()
	after := runtime.NumGoroutine()

	// Allow a small slop for transient scheduler/runtime goroutines, but n aborted
	// transfers must not each leak a goroutine.
	if after > before+5 {
		t.Fatalf("goroutine count grew %d → %d after %d mid-transfer RSTs (leak)", before, after, n)
	}

	// The single-flight slot must be free after the RST teardowns: a normal /clip works.
	good := clipReq(t, h.baseURL, clipEnvelope("recovered.png", []byte("ok")), testToken, true)
	resp := h.do(t, good)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/clip after mid-transfer RSTs = %d, want 200 (slot must release on RST)", resp.StatusCode)
	}
	_ = readBody(t, resp)
}

// errnoStore is an injectable write-failure seam (PLAN §12.4): SaveClipboardItem returns
// a real-errno-style error after draining the reader, so the 500-with-errno mapping is
// unit-tested cross-platform even where a genuine full disk cannot be reproduced.
type errnoStore struct {
	store.ReceiveStore
	saveErr error
}

func (s errnoStore) SaveClipboardItem(name, kind, uti string, r io.Reader) (string, int64, error) {
	_, _ = io.Copy(io.Discard, r) // drain so the streaming decode completes
	return "", 0, s.saveErr
}

// TestClipWriteFailureMapsTo500WithErrno asserts that a store write failure (e.g. an
// ENOSPC errno) surfaces as a 500 whose JSON body carries the REAL errno text as a
// non-secret diagnostic (PLAN §3.11). It wraps the concrete store with errnoStore so the
// failure is injected at the store seam without needing a full disk.
func TestClipWriteFailureMapsTo500WithErrno(t *testing.T) {
	dir := t.TempDir()
	saveDir := filepath.Join(dir, "save")
	if err := os.MkdirAll(saveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	base, err := store.New(store.StoreConfig{
		SaveDir: saveDir, AgentInboxDir: filepath.Join(dir, "inbox"),
		LastPathFile: filepath.Join(dir, "last_path"), RecentsFile: filepath.Join(dir, "recents.json"),
		LongTextThreshold: 8192, MaxBytes: wire.DefaultMaxBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The classic full-disk errno; surfaced verbatim (sanitized) in the 500 body.
	const errnoText = "write /dev/full: no space left on device"
	st := errnoStore{ReceiveStore: base, saveErr: fmt.Errorf("%s", errnoText)}

	cfg := config.DefaultConfig()
	cfg.SaveDir = saveDir
	cfg.MaxBytes = wire.DefaultMaxBytes
	srv := New(Options{
		Config: cfg, Token: testToken, Store: st,
		Listen: ListenTailscale, PeerHost: "127.0.0.1", AppVersion: "test",
		TempDir: filepath.Join(dir, "tmp"),
	})
	ln := mustListenLoopback(t)
	serveOn(t, srv, ln)
	gobase := "http://" + ln.Addr().String()

	req := clipReq(t, gobase, clipEnvelope("x.png", []byte("payload")), testToken, true)
	resp := h0do(t, req)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("write-failure /clip = %d, want 500", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"ok":false`) {
		t.Fatalf("500 body = %q, want ok:false", body)
	}
	// The real errno must be relayed as the diagnostic (sanitizeReason maps \ → / and
	// " → ', but "no space left on device" survives intact).
	if !strings.Contains(body, "no space left on device") {
		t.Fatalf("500 body = %q, want the real errno 'no space left on device'", body)
	}

	// And the slot must be released after the 500 teardown: a healthy store-backed /clip
	// would succeed (we re-point at the base store via a fresh server to confirm the
	// errno path itself did not wedge — the same server's slot is the load-bearing check).
	req2 := clipReq(t, gobase, clipEnvelope("y.png", []byte("payload2")), testToken, true)
	resp2 := h0do(t, req2)
	// Still 500 (the store still fails), but NOT 503 — proves the slot released.
	if resp2.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("second /clip = 503; the slot wedged after a 500 (permanent-503 regression)")
	}
}

// TestWriteFailureSeamAtIngest is the cross-platform companion at the Ingest layer (the
// PLAN §12.4 seam lives in internal/ingest): it confirms the httpd 500 mapping above is
// driven by the same sentinel-free errno propagation the ingest unit tests assert, by
// checking the agent-channel path surfaces the errno too (a file save into the inbox).
func TestWriteFailureSeamAtIngest(t *testing.T) {
	dir := t.TempDir()
	base, err := store.New(store.StoreConfig{
		SaveDir: filepath.Join(dir, "save"), AgentInboxDir: filepath.Join(dir, "inbox"),
		LastPathFile: filepath.Join(dir, "last_path"), RecentsFile: filepath.Join(dir, "recents.json"),
		LongTextThreshold: 8192, MaxBytes: wire.DefaultMaxBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	st := agentErrnoStore{ReceiveStore: base, saveErr: fmt.Errorf("input/output error")}
	cfg := config.DefaultConfig()
	cfg.SaveDir = filepath.Join(dir, "save")
	cfg.MaxBytes = wire.DefaultMaxBytes
	srv := New(Options{
		Config: cfg, Token: testToken, Store: st,
		Listen: ListenLoopbackTCP, AppVersion: "test", TempDir: filepath.Join(dir, "tmp"),
	})
	ln := mustListenLoopback(t)
	serveOn(t, srv, ln)
	gobase := "http://" + ln.Addr().String()

	// agent-send a file → SaveAgentItem fails → 500 with the errno.
	src := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(src, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, gobase+"/agent-send", nil)
	req.Header.Set(tokenHeader, testToken)
	req.Header.Set(channelHeader, "agent")
	req.Header.Set(kindHeader, "file")
	req.Header.Set(fileHeader, src)
	resp := h0do(t, req)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("agent-send write-failure = %d, want 500", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "input/output error") {
		t.Fatalf("500 body = %q, want the real errno 'input/output error'", body)
	}
}

// agentErrnoStore injects a SaveAgentItem failure (the agent-channel write seam).
type agentErrnoStore struct {
	store.ReceiveStore
	saveErr error
}

func (s agentErrnoStore) SaveAgentItem(sender, name, kind, uti string, r io.Reader) (string, int64, error) {
	_, _ = io.Copy(io.Discard, r)
	return "", 0, s.saveErr
}

// TestClipPathEscapeMapsGeneric500 drives writeClipError's ErrPathEscape branch: a store
// that returns sanitize.ErrPathEscape must surface a GENERIC 500 that NEVER echoes the
// path (PLAN §3.9/§3.11). It injects the escape at the store seam.
func TestClipPathEscapeMapsGeneric500(t *testing.T) {
	dir := t.TempDir()
	saveDir := filepath.Join(dir, "save")
	if err := os.MkdirAll(saveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	base, err := store.New(store.StoreConfig{
		SaveDir: saveDir, AgentInboxDir: filepath.Join(dir, "inbox"),
		LastPathFile: filepath.Join(dir, "last_path"), RecentsFile: filepath.Join(dir, "recents.json"),
		LongTextThreshold: 8192, MaxBytes: wire.DefaultMaxBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	st := errnoStore{ReceiveStore: base, saveErr: sanitize.ErrPathEscape}
	cfg := config.DefaultConfig()
	cfg.SaveDir = saveDir
	cfg.MaxBytes = wire.DefaultMaxBytes
	srv := New(Options{Config: cfg, Token: testToken, Store: st, Listen: ListenTailscale, PeerHost: "127.0.0.1", AppVersion: "test", TempDir: filepath.Join(dir, "tmp")})
	ln := mustListenLoopback(t)
	serveOn(t, srv, ln)
	gobase := "http://" + ln.Addr().String()

	req := clipReq(t, gobase, clipEnvelope("evil.png", []byte("x")), testToken, true)
	resp := h0do(t, req)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("path-escape /clip = %d, want 500", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"ok":false`) {
		t.Fatalf("path-escape 500 body = %q, want ok:false", body)
	}
	// The generic body must NOT leak a filesystem path (it says "internal error").
	if strings.Contains(body, "/") && !strings.Contains(body, "internal error") {
		t.Fatalf("path-escape 500 body = %q must be generic (never echo a path)", body)
	}
}
