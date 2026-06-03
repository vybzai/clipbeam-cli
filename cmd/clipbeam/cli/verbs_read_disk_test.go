package cli

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/httpd"
	"github.com/vybzai/clipbeam-cli/internal/store"
)

// withDaemonlessHome points config + the control client at a fresh, short-pathed temp
// HOME with NO daemon socket, so loadControl falls back to a refused loopback-TCP dial
// (the daemonless default). It returns the resolved Paths. Uses /tmp to keep the macOS
// unix-socket path under the 104-byte limit and an unusual loopback port to guarantee no
// stray daemon answers.
func withDaemonlessHome(t *testing.T) config.Paths {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "cbhome")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	// Linux XDG layout points at the same temp home; macOS uses ~/.clipbeam off HOME.
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	// No XDG_RUNTIME_DIR -> controlSocketPath resolves to ~/.clipbeam/clipbeam.sock,
	// which does not exist -> the control client falls back to loopback TCP. Pin
	// config.Port to a guaranteed-free port so that fallback dial is REFUSED rather than
	// connecting to a stray clipbeam daemon already running on the default 8787 on this
	// host (which would otherwise return an HTTP status and skip the disk fallback).
	t.Setenv("XDG_RUNTIME_DIR", "")
	p, err := config.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	writeConfigPort(t, p, freePort(t))
	return p
}

// freePort returns a currently-free loopback TCP port (bind :0, read the port, close).
func freePort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return uint16(port)
}

// writeConfigPort writes a minimal config.json at p.Config pinning the loopback control
// port so the daemonless TCP fallback is deterministically refused.
func writeConfigPort(t *testing.T, p config.Paths, port uint16) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p.Config), 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"port":` + itoa(int(port)) + `}`)
	if err := os.WriteFile(p.Config, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeLastPathFile writes a bare no-trailing-newline path to p.LastPath (mirroring what
// FinishClipboard persists).
func writeLastPathFile(t *testing.T, p config.Paths, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p.LastPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.LastPath, []byte(path), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReadLastPathRoundTrip locks store.ReadLastPath: a bare no-NL path round-trips, an
// empty/missing file returns ("", false).
func TestReadLastPathRoundTrip(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "last_path")
	if err := os.WriteFile(file, []byte("/abs/saved/file.png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.ReadLastPath(file); !ok || got != "/abs/saved/file.png" {
		t.Errorf("ReadLastPath = (%q,%v), want (/abs/saved/file.png,true)", got, ok)
	}
	// Empty file.
	if err := os.WriteFile(file, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.ReadLastPath(file); ok || got != "" {
		t.Errorf("empty file ReadLastPath = (%q,%v), want (\"\",false)", got, ok)
	}
	// Missing file.
	if got, ok := store.ReadLastPath(filepath.Join(dir, "nope")); ok || got != "" {
		t.Errorf("missing file ReadLastPath = (%q,%v), want (\"\",false)", got, ok)
	}
}

// TestRunLastDiskFallbackWhenNoDaemon is the [E] core: no daemon, a last_path file
// present -> runLast prints the bare path with NO trailing newline and exits nil (not
// ExitUnreachable).
func TestRunLastDiskFallbackWhenNoDaemon(t *testing.T) {
	p := withDaemonlessHome(t)
	writeLastPathFile(t, p, "/abs/saved/note.txt")

	var buf bytes.Buffer
	o := out{stdout: &buf, stderr: &bytes.Buffer{}}
	if err := runLast(o); err != nil {
		t.Fatalf("runLast disk fallback must succeed, got %v", err)
	}
	if buf.String() != "/abs/saved/note.txt" {
		t.Errorf("stdout = %q, want bare path no trailing newline", buf.String())
	}
	if strings.HasSuffix(buf.String(), "\n") {
		t.Error("disk-fallback last has a trailing newline (PLAN §8.1 regression)")
	}
}

// TestRunLastDiskFallbackNothingAvailable: no daemon AND no last_path -> default mode
// prints nothing, --json prints {...,"path":null}, exit 0.
func TestRunLastDiskFallbackNothingAvailable(t *testing.T) {
	withDaemonlessHome(t)

	var buf bytes.Buffer
	o := out{stdout: &buf, stderr: &bytes.Buffer{}}
	if err := runLast(o); err != nil {
		t.Fatalf("runLast (nothing) must exit 0, got %v", err)
	}
	if buf.String() != "" {
		t.Errorf("default-mode nothing = %q, want empty", buf.String())
	}

	var jbuf bytes.Buffer
	oj := out{stdout: &jbuf, stderr: &bytes.Buffer{}, json: true}
	if err := runLast(oj); err != nil {
		t.Fatalf("runLast --json (nothing) must exit 0, got %v", err)
	}
	if !strings.Contains(jbuf.String(), `"path":null`) {
		t.Errorf("--json nothing = %q, want a null path", jbuf.String())
	}
}

// TestRunLastPrefersDaemonOverDisk: with a LIVE daemon returning a different path than
// disk, the DAEMON value wins (no fallback).
func TestRunLastPrefersDaemonOverDisk(t *testing.T) {
	p := withDaemonlessHome(t)
	// Disk has one value...
	writeLastPathFile(t, p, "/disk/value")

	// ...but a live daemon at the resolved socket reports another. Bind the httpd server
	// at controlSocketPath() so loadControl picks the socket transport.
	sockPath, err := controlSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(store.StoreConfig{
		SaveDir:           filepath.Join(t.TempDir(), "save"),
		AgentInboxDir:     filepath.Join(t.TempDir(), "inbox"),
		LastPathFile:      filepath.Join(t.TempDir(), "daemon_last"),
		RecentsFile:       filepath.Join(t.TempDir(), "recents.json"),
		LongTextThreshold: 8192,
		MaxBytes:          defaultMaxBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	dpath, _, err := st.SaveClipboardItem("d.txt", "file", "public.plain-text", strings.NewReader("daemon"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishClipboard("box", []string{dpath}); err != nil {
		t.Fatal(err)
	}

	srv := httpd.New(httpd.Options{
		Config:     config.DefaultConfig(),
		Store:      st,
		Listen:     httpd.ListenUnixSocket,
		SocketPath: sockPath,
		AppVersion: "test",
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	waitForSocket(t, sockPath)

	var buf bytes.Buffer
	o := out{stdout: &buf, stderr: &bytes.Buffer{}}
	if err := runLast(o); err != nil {
		t.Fatalf("runLast over live daemon: %v", err)
	}
	if buf.String() != dpath {
		t.Errorf("stdout = %q, want the DAEMON path %q (disk must NOT shadow a reachable daemon)", buf.String(), dpath)
	}
	_ = p
}

// TestIsDaemonUnreachable tables the connect-failure classifier.
func TestIsDaemonUnreachable(t *testing.T) {
	connRefused := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"net.OpError", connRefused, true},
		{"fs.ErrNotExist", fs.ErrNotExist, true},
		{"generic", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isDaemonUnreachable(tc.err); got != tc.want {
			t.Errorf("%s: isDaemonUnreachable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRunWaitDiskFallback: no daemon, last_path present -> one-shot returns it; absent ->
// emitNothingPath.
func TestRunWaitDiskFallback(t *testing.T) {
	p := withDaemonlessHome(t)
	writeLastPathFile(t, p, "/abs/waited.txt")

	var buf bytes.Buffer
	o := out{stdout: &buf, stderr: &bytes.Buffer{}}
	if err := runWait(o); err != nil {
		t.Fatalf("runWait disk fallback must succeed, got %v", err)
	}
	if buf.String() != "/abs/waited.txt" {
		t.Errorf("stdout = %q, want bare path", buf.String())
	}

	// Remove the file -> nothing available, exit 0.
	if err := os.Remove(p.LastPath); err != nil {
		t.Fatal(err)
	}
	var buf2 bytes.Buffer
	o2 := out{stdout: &buf2, stderr: &bytes.Buffer{}}
	if err := runWait(o2); err != nil {
		t.Fatalf("runWait (nothing) must exit 0, got %v", err)
	}
	if buf2.String() != "" {
		t.Errorf("nothing = %q, want empty", buf2.String())
	}
}

// seedAgentJournal enqueues an agent item into the on-disk journal under the resolved
// AgentInbox (the daemonless write path the SSH-exec ingest takes). It opens a fresh
// store over the same config paths runRecv's fallback will use.
func seedAgentText(t *testing.T, text string) {
	t.Helper()
	st := freshStore(t)
	if err := st.EnqueueAgentText("", text); err != nil {
		t.Fatal(err)
	}
}

func freshStore(t *testing.T) store.ReceiveStore {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := config.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	saveDir, err := config.ResolvedSaveDir(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(store.StoreConfig{
		SaveDir:           saveDir,
		AgentInboxDir:     p.AgentInbox,
		LastPathFile:      p.LastPath,
		RecentsFile:       p.Recents,
		SaveTextToDisk:    cfg.SaveTextToDisk,
		LongTextThreshold: cfg.LongTextThreshold,
		MaxBytes:          defaultMaxBytes,
		// This test helper plays the role of the daemonless `clipbeam ingest` writer when it
		// SEEDS the journal, so it must journal (fix [F]). The production drain path
		// (drainAgentDiskAndEmit) builds its store WITHOUT this flag — it only reads/claims.
		JournalAgentItems: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestRunRecvDiskDrainWhenNoDaemon: no daemon, a journal text record present -> runRecv
// prints the labeled block byte-identical to recvBodyString(item) and exits nil; --json
// matches the daemon recvJSON shape (fix [F] T5).
func TestRunRecvDiskDrainWhenNoDaemon(t *testing.T) {
	withDaemonlessHome(t)
	seedAgentText(t, "deploy done\nlog attached")

	var buf bytes.Buffer
	o := out{stdout: &buf, stderr: &bytes.Buffer{}}
	if err := runRecv(o, 0); err != nil {
		t.Fatalf("runRecv disk drain must succeed, got %v", err)
	}
	want := "type: text\nsender: \ntext: deploy done\nlog attached"
	if buf.String() != want {
		t.Errorf("default body = %q, want %q (byte-identical to recvBodyString)", buf.String(), want)
	}
	if strings.HasSuffix(buf.String(), "\n") {
		t.Error("disk-drain recv body has a trailing newline (PLAN §8.9 regression)")
	}

	// --json: re-seed (the first drain consumed it) and assert the recvJSON shape.
	seedAgentText(t, "ack")
	var jbuf bytes.Buffer
	oj := out{stdout: &jbuf, stderr: &bytes.Buffer{}, json: true}
	if err := runRecv(oj, 0); err != nil {
		t.Fatalf("runRecv --json disk drain: %v", err)
	}
	js := jbuf.String()
	for _, want := range []string{`"type":"text"`, `"sender":""`, `"text":"ack"`, `"channel":"agent"`} {
		if !strings.Contains(js, want) {
			t.Errorf("--json recv = %q, missing %q", js, want)
		}
	}
}

// TestRunRecvDiskEmptyJournalTimeout: no daemon, empty journal -> default prints nothing,
// --json prints {...,"item":null}, exit 0 (fix [F] T6).
func TestRunRecvDiskEmptyJournalTimeout(t *testing.T) {
	withDaemonlessHome(t)

	var buf bytes.Buffer
	o := out{stdout: &buf, stderr: &bytes.Buffer{}}
	if err := runRecv(o, 0); err != nil {
		t.Fatalf("runRecv empty journal must exit 0, got %v", err)
	}
	if buf.String() != "" {
		t.Errorf("default empty = %q, want nothing", buf.String())
	}

	var jbuf bytes.Buffer
	oj := out{stdout: &jbuf, stderr: &bytes.Buffer{}, json: true}
	if err := runRecv(oj, 0); err != nil {
		t.Fatalf("runRecv --json empty journal: %v", err)
	}
	if !strings.Contains(jbuf.String(), `"item":null`) {
		t.Errorf("--json empty = %q, want item:null", jbuf.String())
	}
}

// TestRunRecvDiskImageBodyHasPathNoText: a drained image item body starts `type: image\n`
// and contains `path: ` and NO `text: ` (mirrors TestInteropRecvImageBodyMatchesFixture,
// fix [F] T7).
func TestRunRecvDiskImageBodyHasPathNoText(t *testing.T) {
	withDaemonlessHome(t)
	st := freshStore(t)
	if _, _, err := st.SaveAgentItem("", "shot.png", "image", "public.png", strings.NewReader("PNGDATA")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	o := out{stdout: &buf, stderr: &bytes.Buffer{}}
	if err := runRecv(o, 0); err != nil {
		t.Fatalf("runRecv image drain: %v", err)
	}
	body := buf.String()
	if !strings.HasPrefix(body, "type: image\n") {
		t.Errorf("image body = %q, want it to start `type: image\\n`", body)
	}
	if !strings.Contains(body, "path: ") {
		t.Errorf("image body = %q, want a `path: ` line", body)
	}
	if strings.Contains(body, "text: ") {
		t.Errorf("image body = %q, must NOT carry a `text: ` line", body)
	}
}

// waitForSocket blocks until the unix socket at path accepts a connection (or fails).
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", path); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("socket %s never came up", path)
}
