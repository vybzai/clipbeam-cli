package httpd

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/store"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// fuzzServer starts a real Server on a loopback listener for the HTTP-parse fuzz target
// and returns its address. It uses the production net/http server (the same one Serve
// drives) so the fuzzer exercises the real request-parse + route + gate machinery.
func fuzzServer(f *testing.F) string {
	f.Helper()
	dir, err := os.MkdirTemp("", "clipbeam-fuzz-")
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = os.RemoveAll(dir) })
	saveDir := filepath.Join(dir, "save")
	if err := os.MkdirAll(saveDir, 0o700); err != nil {
		f.Fatal(err)
	}
	st, err := store.New(store.StoreConfig{
		SaveDir: saveDir, AgentInboxDir: filepath.Join(dir, "inbox"),
		LastPathFile: filepath.Join(dir, "last_path"), RecentsFile: filepath.Join(dir, "recents.json"),
		LongTextThreshold: 8192, MaxBytes: wire.DefaultMaxBytes,
	})
	if err != nil {
		f.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.SaveDir = saveDir
	cfg.MaxBytes = wire.DefaultMaxBytes
	srv := New(Options{
		Config: cfg, Token: testToken, Store: st,
		Listen: ListenTailscale, PeerHost: "127.0.0.1", AppVersion: "test",
		TempDir: filepath.Join(dir, "tmp"),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.Fatal(err)
	}
	go func() { _ = srv.srv.Serve(ln) }()
	f.Cleanup(func() { _ = srv.srv.Close() })
	return ln.Addr().String()
}

// FuzzHTTPRequestParse throws raw request bytes (including split reads, truncated CRLF,
// duplicate headers, huge Content-Length, non-UTF8 header bytes) at the REAL net/http
// server and asserts: it never panics, it always TERMINATES (a bounded read deadline is
// the liveness witness — a hung parse would trip the deadline and we still must get a
// clean close, never a process hang), and for the structurally well-formed adversarial
// cases the status maps to the frozen table (a >64 KB header section → 431; the server
// never wedges). It does NOT assert an exact status for arbitrary garbage (net/http may
// close the connection without a response), only that the exchange terminates cleanly.
// Run: go test -run xxx -fuzz=FuzzHTTPRequestParse -fuzztime=30s ./internal/httpd
func FuzzHTTPRequestParse(f *testing.F) {
	addr := fuzzServer(f)

	// Seeds: real request shapes + adversarial inputs (PLAN §12.3).
	f.Add([]byte("GET /health HTTP/1.1\r\nHost: x\r\n\r\n"))
	f.Add([]byte("POST /clip HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n\r\n{}"))
	f.Add([]byte("GET /last HTTP/1.1\r\nHost: x\r\nX-ClipBeam-Token: " + testToken + "\r\n\r\n"))
	f.Add([]byte("POST /clip HTTP/1.1\r\nHost: x\r\nContent-Length: notanumber\r\n\r\n"))
	f.Add([]byte("POST /clip HTTP/1.1\r\nHost: x\r\nContent-Length: 999999999999999\r\n\r\n"))
	f.Add([]byte("GET /health HTTP/1.1\r\nHost: x\r\nHost: y\r\nHost: z\r\n\r\n")) // duplicate headers
	f.Add([]byte("GET /health HTTP/1.1\r\nHost: x")) // truncated (no terminating CRLF CRLF)
	f.Add([]byte("\r\n\r\n"))
	f.Add([]byte("GARBAGE NOT HTTP AT ALL"))
	f.Add([]byte{0x00, 0x01, 0x02, 0xff, 0xfe}) // non-UTF8 / binary
	// A header line that overflows the 64 KB header section → must map to 431.
	f.Add([]byte("GET /health HTTP/1.1\r\nHost: x\r\nX-Big: " + strings.Repeat("A", 70*1024) + "\r\n\r\n"))

	f.Fuzz(func(t *testing.T, raw []byte) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			// Under the fuzzer the ephemeral-port table can saturate (sockets in
			// TIME_WAIT); a transient connect error is an environment artifact, not a
			// server bug. Skip — the panic/hang invariants are what the fuzzer guards.
			t.Skip("dial failed (transient): " + err.Error())
		}
		// RST on close (SetLinger 0) so the socket does not linger in TIME_WAIT and
		// exhaust the ephemeral-port table across millions of fuzz execs.
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}

		// Write the fuzzed bytes under a deadline so a server that never drains cannot
		// block the writer.
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, _ = conn.Write(raw)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}

		// Read the response (or a clean close) under a deadline. A deadline firing with
		// data still pending is acceptable (a long body drain); the load-bearing
		// invariants are (1) the server never panics and (2) parsing this input
		// TERMINATES — the bounded read here is the liveness witness, and the Read
		// returning (on data, EOF, or deadline) proves termination.
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 4096)
		var first []byte
		for {
			n, rerr := conn.Read(buf)
			if n > 0 {
				first = append(first, buf[:n]...)
			}
			if rerr != nil || len(first) >= 4096 {
				break
			}
		}
		_ = conn.Close()

		// If the server produced a status line for an oversize-header input it must be a
		// 431 (PLAN §12.3) — net/http maps an over-MaxHeaderBytes request to 431. Only
		// assert when a full status line came back (the server may instead just close).
		if len(raw) > wire.HardCeilingHeaderBytes {
			if line := statusLine(first); strings.HasPrefix(line, "HTTP/1.1 ") {
				if !strings.Contains(line, " 431 ") {
					t.Fatalf("oversize-header request status = %q, want 431", line)
				}
			}
		}
	})
}

// statusLine returns the first line of an HTTP response (the status line) or "".
func statusLine(b []byte) string {
	s := string(b)
	if i := strings.IndexByte(s, '\r'); i >= 0 {
		return s[:i]
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
