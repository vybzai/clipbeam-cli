package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// testServer is an in-process x/crypto/ssh server (ssh.NewServerConn) that exercises
// the exec (0a) and forward (0b) channel types end-to-end WITHOUT docker (the required
// M2 gate, PLAN §12). It accepts any public key, runs an exec handler over session
// channels, and proxies direct-tcpip / direct-streamlocal forward channels to a
// caller-supplied backend dialer.
type testServer struct {
	ln       net.Listener
	signer   ssh.Signer
	execFn   func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int
	dialFn   func(network, addr string) (net.Conn, error)
	wg       sync.WaitGroup
	closeOne sync.Once
}

// newTestServer starts a server on 127.0.0.1:0 and returns it plus its host public key.
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testServer{ln: ln, signer: signer}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

// addr returns the host:port the server listens on.
func (s *testServer) addr() (string, int) {
	a := s.ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

// hostKey returns the server's host public key (for seeding known_hosts).
func (s *testServer) hostKey() ssh.PublicKey { return s.signer.PublicKey() }

// Close stops the listener.
func (s *testServer) Close() {
	s.closeOne.Do(func() { _ = s.ln.Close() })
}

func (s *testServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *testServer) handleConn(nc net.Conn) {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(s.signer)
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		_ = nc.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		go s.handleChannel(nc)
	}
	_ = sc.Close()
}

func (s *testServer) handleChannel(nc ssh.NewChannel) {
	switch nc.ChannelType() {
	case "session":
		s.handleSession(nc)
	case "direct-tcpip", "direct-streamlocal@openssh.com":
		s.handleForward(nc)
	default:
		_ = nc.Reject(ssh.UnknownChannelType, "unsupported")
	}
}

// handleSession runs the exec handler: it waits for an "exec" request, then pipes the
// channel's stdin into execFn and execFn's stdout/stderr back, sending the exit status.
func (s *testServer) handleSession(nc ssh.NewChannel) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	for req := range reqs {
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		// exec payload: a 4-byte length-prefixed command string.
		cmd := string(req.Payload[4:])
		if req.WantReply {
			_ = req.Reply(true, nil)
		}
		status := 0
		if s.execFn != nil {
			status = s.execFn(cmd, ch, ch, ch.Stderr())
		}
		// Send exit-status then close.
		_, _ = ch.SendRequest("exit-status", false, exitStatusPayload(status))
		_ = ch.CloseWrite()
		_ = ch.Close()
		return
	}
}

// handleForward proxies a forward channel to the backend dialer (direct-tcpip /
// direct-streamlocal). The open payload carries the target; the test backend ignores
// it and uses dialFn directly.
func (s *testServer) handleForward(nc ssh.NewChannel) {
	if s.dialFn == nil {
		_ = nc.Reject(ssh.ConnectionFailed, "no backend")
		return
	}
	backend, err := s.dialFn("tcp", "backend")
	if err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		_ = backend.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	// Bidirectional copy between the SSH channel and the backend conn.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(backend, ch); _ = closeWriteOrConn(backend) }()
	go func() { defer wg.Done(); _, _ = io.Copy(ch, backend); _ = ch.CloseWrite() }()
	wg.Wait()
	_ = ch.Close()
	_ = backend.Close()
}

// closeWriteOrConn half-closes the write side of the backend conn so the backend's
// read side sees EOF (TCP and unix both support CloseWrite via this interface).
func closeWriteOrConn(c net.Conn) error {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}

// exitStatusPayload encodes a uint32 exit status as an SSH exit-status request payload.
func exitStatusPayload(status int) []byte {
	return []byte{0, 0, 0, byte(status)}
}

// seedKnownHosts writes a known_hosts file trusting the test server's host key at its
// host:port and returns a *client configured to use it (with a no-op agent so only the
// public-key file path is exercised).
func newClientForServer(t *testing.T, s *testServer) *client {
	t.Helper()
	host, port := s.addr()
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	line := knownhosts.Line([]string{knownhosts.Normalize(addr)}, s.hostKey())
	if err := os.WriteFile(kh, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy, err := newHostKeyPolicy([]string{kh})
	if err != nil {
		t.Fatal(err)
	}
	return &client{auth: &fixedKeyAuth{t: t}, hostKeys: policy}
}

// fixedKeyAuth is an authProvider that offers a single fresh in-memory key (the test
// server accepts any key, so the key material is irrelevant — this exercises the
// PublicKeys auth path without touching ~/.ssh).
type fixedKeyAuth struct{ t *testing.T }

func (a *fixedKeyAuth) methods(Target) ([]ssh.AuthMethod, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
}

// TestExecEndToEnd is the required in-process gate (PLAN §12): a real ssh server
// receives an exec, reads the CB01 frame off stdin, and the client relays the handler's
// stdout — exactly the daemonless-exec push contract (0a, §5.1) WITHOUT docker.
func TestExecEndToEnd(t *testing.T) {
	s := newTestServer(t)
	// The exec handler echoes the remote "saved path" computed from the stdin frame: it
	// decodes the CB01 frame and prints one line per item name (mimicking `clipbeam
	// ingest` printing saved paths), no trailing newline on the last.
	s.execFn = func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int {
		if cmd == "" {
			fmt.Fprint(stderr, "empty command")
			return 2
		}
		ch, items, err := DecodeCB01(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "decode: %v", err)
			return 9
		}
		for i, it := range items {
			payload, _ := io.ReadAll(it.Payload)
			if i > 0 {
				fmt.Fprint(stdout, "\n")
			}
			fmt.Fprintf(stdout, "/saved/%s:%d:%dbytes", it.Name, ch, len(payload))
		}
		return 0
	}

	c := newClientForServer(t, s)
	defer func() { _ = c.Close() }()

	host, port := s.addr()
	target := Target{User: "tester", Host: host, Port: port}

	sess, err := c.Dial(target)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = sess.Close() }()

	var frame []byte
	{
		var buf byteSink
		items := []CB01Item{
			{Kind: cb01KindImage, Name: "shot.png", Payload: []byte("PNGDATA")},
		}
		if err := EncodeCB01(&buf, cb01ChannelClipboard, items); err != nil {
			t.Fatal(err)
		}
		frame = buf.b
	}

	stdout, stderr, err := sess.Run("/abs/clipbeam ingest", frame)
	if err != nil {
		t.Fatalf("run: %v (stderr=%s)", err, stderr)
	}
	want := "/saved/shot.png:0:7bytes"
	if string(stdout) != want {
		t.Errorf("relayed stdout = %q, want %q", stdout, want)
	}
}

// TestExecNonZeroExit asserts a non-zero remote exit is surfaced as an error carrying
// the captured stderr (PLAN §5.1).
func TestExecNonZeroExit(t *testing.T) {
	s := newTestServer(t)
	s.execFn = func(string, io.Reader, io.Writer, io.Writer) int { return 0 }
	// Override to fail: read stdin fully then exit non-zero with a stderr message.
	s.execFn = func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int {
		_, _ = io.Copy(io.Discard, stdin)
		fmt.Fprint(stderr, "remote boom")
		return 9
	}
	c := newClientForServer(t, s)
	defer func() { _ = c.Close() }()
	host, port := s.addr()
	sess, err := c.Dial(Target{User: "x", Host: host, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	_, stderr, err := sess.Run("cmd", []byte("data"))
	if err == nil {
		t.Fatalf("non-zero remote exit must error")
	}
	if string(stderr) != "remote boom" {
		t.Errorf("stderr = %q, want 'remote boom'", stderr)
	}
}

// TestForwardEndToEnd is the required in-process gate for (0b): the client opens a
// direct-tcpip forward over the SSH connection to a backend the server proxies to, and
// a round-trip request/response flows — exactly the tunneled-daemon forward (§5.1 0b)
// WITHOUT docker.
func TestForwardEndToEnd(t *testing.T) {
	// Backend: a tiny TCP echo-with-prefix server the SSH server proxies forwards to.
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backendLn.Close() }()
	go func() {
		for {
			conn, aerr := backendLn.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				b, _ := io.ReadAll(c)
				_, _ = c.Write(append([]byte("pong:"), b...))
			}(conn)
		}
	}()

	s := newTestServer(t)
	s.dialFn = func(network, addr string) (net.Conn, error) {
		return net.Dial("tcp", backendLn.Addr().String())
	}

	c := newClientForServer(t, s)
	defer func() { _ = c.Close() }()
	host, port := s.addr()
	target := Target{User: "x", Host: host, Port: port}

	conn, err := c.Forward(target, "tcp", "127.0.0.1:9999")
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Send a request; the backend echoes it with a prefix.
	if _, werr := conn.Write([]byte("ping")); werr != nil {
		t.Fatal(werr)
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	resp, rerr := io.ReadAll(conn)
	if rerr != nil {
		t.Fatalf("read forward: %v", rerr)
	}
	if string(resp) != "pong:ping" {
		t.Errorf("forward round-trip = %q, want 'pong:ping'", resp)
	}
}

// TestForwardStreamLocalEndToEnd exercises the direct-streamlocal (unix socket) forward
// path (§5.1 0b unix socket) over the in-process server.
func TestForwardStreamLocalEndToEnd(t *testing.T) {
	sock := shortSock(t)
	backendLn, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backendLn.Close() }()
	go func() {
		for {
			conn, aerr := backendLn.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				b, _ := io.ReadAll(c)
				_, _ = c.Write(append([]byte("unix:"), b...))
			}(conn)
		}
	}()

	s := newTestServer(t)
	s.dialFn = func(network, addr string) (net.Conn, error) {
		return net.Dial("unix", sock)
	}
	c := newClientForServer(t, s)
	defer func() { _ = c.Close() }()
	host, port := s.addr()

	conn, err := c.Forward(Target{User: "x", Host: host, Port: port}, "unix", sock)
	if err != nil {
		t.Fatalf("streamlocal forward: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, werr := conn.Write([]byte("hi")); werr != nil {
		t.Fatal(werr)
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	resp, _ := io.ReadAll(conn)
	if string(resp) != "unix:hi" {
		t.Errorf("streamlocal round-trip = %q, want 'unix:hi'", resp)
	}
}

// byteSink is a tiny io.Writer accumulator for building a CB01 frame in tests.
type byteSink struct{ b []byte }

func (w *byteSink) Write(p []byte) (int, error) { w.b = append(w.b, p...); return len(p), nil }
