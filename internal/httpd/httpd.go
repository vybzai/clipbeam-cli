// Package httpd is the net/http receiver: a single dual-stack http.Server with the
// full frozen route table (PLAN §3.3), per-route source-class gating, streaming
// base64-JSON decode straight to disk on /clip, single-flight, the long-poll
// endpoints, and the byte-identical response writer. It deletes the hand-rolled
// HTTP/1.1 parser the Swift app carried (PLAN §2.1) by using net/http.
package httpd

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"

	"github.com/vybzai/clipbeam-cli/internal/classify"
	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/store"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// ListenKind selects how Serve binds (PLAN §5.1, §5.7).
type ListenKind int

const (
	// ListenUnixSocket is the Linux default: a 0600 unix-domain socket (PLAN §5.1).
	ListenUnixSocket ListenKind = iota
	// ListenLoopbackTCP binds 127.0.0.1:port with the token required on control
	// endpoints (opt-in shared-box mode, PLAN §6.2/§6.3).
	ListenLoopbackTCP
	// ListenTailscale binds the dual-stack all-interfaces TCP listener on
	// config.port so the box can RECEIVE Tier-1 /clip from a tailnet peer
	// (PLAN §5.7); /clip gated by exact-peer-IP AND token.
	ListenTailscale
)

// Options parameterizes the Server.
type Options struct {
	Config config.Config
	Token  string
	Store  store.ReceiveStore
	Listen ListenKind
	// SocketPath is the unix-socket path for ListenUnixSocket (PLAN §5.1).
	SocketPath string
	// PeerHost is the configured Tailscale peer host for the exact-peer /clip gate
	// on ListenTailscale (PLAN §5.2). Empty for non-tailscale modes.
	PeerHost string
	// AppVersion is the CLI semver surfaced in /health (PLAN §3.3); values are the
	// CLI's own, only the key names are frozen.
	AppVersion string
	// TempDir overrides the scratch directory for the streaming /clip base64 decode
	// (PLAN §3.6). Empty ⇒ derive from Config.SaveDir / ResolvedSaveDir.
	TempDir string
}

// classOf returns the SourceClass to pass through Ingest. It is informational on the
// receive path (Tier-1 auth happens in clipGate before Ingest, PLAN §6.2): the value
// reflects the bind mode so logging/diagnostics carry the validated transport class.
func (o Options) classOf() classify.SourceClass {
	switch o.Listen {
	case ListenTailscale:
		return classify.Peer
	default:
		return classify.Loopback
	}
}

// Server is the receiver. It owns the http.Server, the single-flight gate, and the
// route mux. All routes are registered in routes() (PLAN §3.3).
type Server struct {
	opts   Options
	srv    *http.Server
	single *singleFlight
}

// New builds a Server with the route table wired. It does not bind a socket; call
// Serve to listen.
func New(opts Options) *Server {
	s := &Server{
		opts:   opts,
		single: newSingleFlight(),
	}
	mux := http.NewServeMux()
	s.routes(mux)
	s.srv = &http.Server{
		Handler:        mux,
		MaxHeaderBytes: wire.HardCeilingHeaderBytes, // → 431, mirrors Swift's 64 KB cap
	}
	// Mirror Swift's always-close semantics (PLAN §3.3): keep-alives disabled.
	s.srv.SetKeepAlivesEnabled(false)
	return s
}

// Serve binds per the configured ListenKind and serves until ctx is canceled.
// For ListenTailscale/ListenLoopbackTCP it uses dualStackListenConfig so a single
// [::]:port bind serves IPv4+IPv6 (IPV6_V6ONLY cleared, PLAN §3.3). For
// ListenUnixSocket it binds the 0600 socket with stale-socket + uid-ownership
// handling (PLAN §5.1; the socket hardening is completed in M2b).
//
// The TCP bind paths are wired here; the unix-socket hardening (stale-socket unlink,
// uid-ownership refusal, umask) is completed in M2b.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := s.listen(ctx)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = s.Shutdown(context.Background())
	}()
	if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// listen opens the configured listener. TCP modes use dualStackListenConfig (the
// IPV6_V6ONLY clear, PLAN §3.3); the unix-socket mode is hardened in M2b.
func (s *Server) listen(ctx context.Context) (net.Listener, error) {
	switch s.opts.Listen {
	case ListenUnixSocket:
		// umask(0177) before bind, chmod 0600 after, stale-socket unlink, and REFUSE
		// to bind a socket path owned by another uid (PLAN §5.1). Per-OS in
		// socket_unix.go / socket_other.go.
		return listenUnixSocket(s.opts.SocketPath)
	case ListenLoopbackTCP:
		lc := dualStackListenConfig()
		return lc.Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", itoa(s.opts.Config.Port)))
	case ListenTailscale:
		lc := dualStackListenConfig()
		return lc.Listen(ctx, "tcp", net.JoinHostPort("::", itoa(s.opts.Config.Port)))
	default:
		return nil, errors.New("clipbeam: unknown listen kind")
	}
}

// maxBytes is the DECODED-payload cap (config.maxBytes), defaulting to
// wire.DefaultMaxBytes when the config carries a non-positive value (PLAN §3.8).
func (s *Server) maxBytes() int {
	if s.opts.Config.MaxBytes > 0 {
		return s.opts.Config.MaxBytes
	}
	return wire.DefaultMaxBytes
}

// tempDir returns the scratch directory for the streaming /clip base64 decode (PLAN
// §3.6): the resolved save dir, so the decoded scratch file sits on the destination
// filesystem (the store's atomic rename of its own temp stays same-device). An
// explicit Options.TempDir overrides it (tests point this at t.TempDir()). The
// directory is created 0700; a creation failure is surfaced, not papered over.
func (s *Server) tempDir() (string, error) {
	if s.opts.TempDir != "" {
		if err := os.MkdirAll(s.opts.TempDir, 0o700); err != nil {
			return "", err
		}
		return s.opts.TempDir, nil
	}
	dir, err := config.ResolvedSaveDir(s.opts.Config)
	if err != nil {
		return "", err
	}
	return dir, nil
}

// itoa renders a uint16 port without importing strconv at every call site.
func itoa(p uint16) string {
	if p == 0 {
		return "0"
	}
	var buf [5]byte
	i := len(buf)
	for p > 0 {
		i--
		buf[i] = byte('0' + p%10)
		p /= 10
	}
	return string(buf[i:])
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// dualStackListenConfig returns a net.ListenConfig whose Control hook clears
// IPV6_V6ONLY via x/sys/unix so one [::]:PORT bind serves IPv4+IPv6 — REQUIRED on
// darwin (IPV6_V6ONLY defaults to 1 on macOS/BSD; without the clear the shim's
// ::ffff:127.0.0.1 loopback is rejected, PLAN §3.3). Defined per-OS in
// listen_unix.go / listen_other.go.
func dualStackListenConfig() net.ListenConfig {
	return net.ListenConfig{Control: controlClearV6Only}
}
