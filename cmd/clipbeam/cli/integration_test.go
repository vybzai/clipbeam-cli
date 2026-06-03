package cli

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/httpd"
	"github.com/vybzai/clipbeam-cli/internal/store"
)

// newTestServer starts a real httpd.Server over a unix socket in a temp dir, returns a
// controlClient pointed at it, and the live store so the test can populate items.
func newTestServer(t *testing.T) (*controlClient, store.ReceiveStore) {
	t.Helper()
	dir := t.TempDir()
	sock := shortSock(t)

	st, err := store.New(store.StoreConfig{
		SaveDir:           filepath.Join(dir, "save"),
		AgentInboxDir:     filepath.Join(dir, "inbox"),
		LastPathFile:      filepath.Join(dir, "last_path"),
		RecentsFile:       filepath.Join(dir, "recents.json"),
		LongTextThreshold: 8192,
		MaxBytes:          defaultMaxBytes,
	})
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	srv := httpd.New(httpd.Options{
		Config:     config.DefaultConfig(),
		Store:      st,
		Listen:     httpd.ListenUnixSocket,
		SocketPath: sock,
		AppVersion: "test-1.2.3",
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()

	// Wait for the socket to come up.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", sock); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	c := &controlClient{
		hc: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}},
		scheme: "http",
		host:   "clipbeam.local",
	}
	return c, st
}

// TestLastBarePathNoTrailingNewline verifies GET /last returns the bare path with NO
// trailing newline through the control client (PLAN §8.1, the FROZEN contract).
func TestLastBarePathNoTrailingNewline(t *testing.T) {
	c, st := newTestServer(t)

	// 204 when nothing saved.
	ctx := context.Background()
	status, body, err := c.get(ctx, "/last")
	if err != nil {
		t.Fatalf("get /last: %v", err)
	}
	if status != 204 || len(body) != 0 {
		t.Fatalf("empty /last = (%d, %q), want (204, empty)", status, body)
	}

	// Save a clipboard item, then /last must return its bare path, no trailing newline.
	path, _, err := st.SaveClipboardItem("note.txt", "file", "public.plain-text", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("SaveClipboardItem: %v", err)
	}
	if err := st.FinishClipboard("box", []string{path}); err != nil {
		t.Fatalf("FinishClipboard: %v", err)
	}

	status, body, err = c.get(ctx, "/last")
	if err != nil {
		t.Fatalf("get /last: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if string(body) != path {
		t.Errorf("body = %q, want %q", body, path)
	}
	if strings.HasSuffix(string(body), "\n") {
		t.Error("bare path has a trailing newline (banned regression, PLAN §8.1)")
	}
}

// TestRecvLabeledBodyEndToEnd verifies the recv labeled body round-trips through the
// live server + the parser: an agent text item is dequeued and parsed correctly.
func TestRecvLabeledBodyEndToEnd(t *testing.T) {
	c, st := newTestServer(t)
	if err := st.EnqueueAgentText("laptop", "deploy done\nlog attached"); err != nil {
		t.Fatalf("EnqueueAgentText: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status, body, err := c.get(ctx, "/recv?timeout=2")
	if err != nil {
		t.Fatalf("get /recv: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if strings.HasSuffix(string(body), "\n") {
		t.Error("recv labeled body has a trailing newline (PLAN §8.9: byte-for-byte the Swift output)")
	}
	it := parseRecvBody(string(body))
	if it.Type != "text" || it.Sender != "laptop" {
		t.Errorf("parsed header wrong: %+v", it)
	}
	if it.Text != "deploy done\nlog attached" {
		t.Errorf("text = %q, want the multi-line body verbatim", it.Text)
	}
}

// TestRecvTimeout204 verifies that an empty agent FIFO times out as 204 (the normal
// long-poll timeout → exit 0 path, PLAN §8.3).
func TestRecvTimeout204(t *testing.T) {
	c, _ := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status, body, err := c.get(ctx, "/recv?timeout=0")
	if err != nil {
		t.Fatalf("get /recv: %v", err)
	}
	if status != 204 || len(body) != 0 {
		t.Errorf("empty /recv?timeout=0 = (%d, %q), want (204, empty)", status, body)
	}
}

// TestHealthEndToEnd verifies /health returns the frozen key shape with the CLI's own
// values; a liveness check tests ok==true (PLAN §8.9).
func TestHealthEndToEnd(t *testing.T) {
	c, _ := newTestServer(t)
	ctx := context.Background()
	status, body, err := c.get(ctx, "/health")
	if err != nil {
		t.Fatalf("get /health: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, key := range []string{`"ok"`, `"app"`, `"version"`, `"host"`, `"platform"`} {
		if !bytes.Contains(body, []byte(key)) {
			t.Errorf("/health body %q missing frozen key %s", body, key)
		}
	}
	if !bytes.Contains(body, []byte(`"version":"test-1.2.3"`)) {
		t.Errorf("/health body %q missing the injected version value", body)
	}
}
