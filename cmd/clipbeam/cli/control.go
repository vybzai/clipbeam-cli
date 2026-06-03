package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/config"
)

// controlClient talks to the LOCAL clipbeam daemon's loopback control endpoints
// (/last, /wait, /recv) over the same transport `clipbeam serve` binds: a unix-domain
// socket on Linux (the default), or loopback TCP. The CLI is a CLIENT of its own
// daemon here — these verbs read what a peer beamed to THIS box (PLAN §5.1, §8.4).
//
// It is NOT used for the peer-facing send verbs (those drive SSH/Tailscale to a
// remote box, PLAN §5.1 daemonless-exec).
type controlClient struct {
	hc      *http.Client
	scheme  string // "http" for both unix and tcp (the host is virtual for unix)
	host    string // the Host header / URL authority
	token   string // sent as X-ClipBeam-Token on TCP binds (token-free over the 0600 socket)
	overTCP bool
}

// controlSocketPath resolves the daemon's unix-socket path (PLAN §5.1): under
// $XDG_RUNTIME_DIR/clipbeam/clipbeam.sock, falling back to ~/.clipbeam/clipbeam.sock
// when $XDG_RUNTIME_DIR is unset (bare SSH-exec / cron — the documented stale-socket
// implication). The parent dir is created 0700 by the serve path, not here.
func controlSocketPath() (string, error) {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" && filepath.IsAbs(rt) {
		return filepath.Join(rt, "clipbeam", "clipbeam.sock"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("clipbeam: cannot resolve home directory for socket path")
	}
	return filepath.Join(home, ".clipbeam", "clipbeam.sock"), nil
}

// newControlClient builds a client for the local daemon. It prefers the unix socket
// (token-free, the 0600 file is the gate); if no socket exists it falls back to the
// loopback TCP bind on config.Port, sending the resolved token. A long httpTimeout is
// used because /wait and /recv are long-poll endpoints (up to 120 s plus slack); the
// caller's context still bounds the actual wait.
func newControlClient(cfg config.Config, token string) (*controlClient, error) {
	sock, err := controlSocketPath()
	if err == nil {
		if _, statErr := os.Stat(sock); statErr == nil {
			return &controlClient{
				hc: &http.Client{
					Transport: &http.Transport{
						DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
							var d net.Dialer
							return d.DialContext(ctx, "unix", sock)
						},
					},
				},
				scheme: "http",
				host:   "clipbeam.local", // virtual authority for the unix transport
				token:  "",               // token-free over the 0600 socket (PLAN §6.2)
			}, nil
		}
	}
	// No socket — fall back to the loopback-TCP daemon (opt-in --listen tcp). The token
	// is REQUIRED on every control endpoint over TCP (PLAN §6.2). On Linux the unix
	// socket is the default, so its absence usually means no daemon is running; the TCP
	// attempt still lets a --listen tcp daemon be reached and yields a clear connection
	// error otherwise.
	host := net.JoinHostPort("127.0.0.1", portString(cfg.Port))
	return &controlClient{
		hc:      &http.Client{Transport: &http.Transport{}},
		scheme:  "http",
		host:    host,
		token:   token,
		overTCP: true,
	}, nil
}

// get issues a GET to a control path with the given context, returning the status,
// body bytes, and any transport error. The token is attached on TCP binds. The
// long-poll endpoints rely on ctx for their deadline; a context cancel/timeout returns
// a context error the caller treats as a normal timeout (exit 0, PLAN §8.3).
func (c *controlClient) get(ctx context.Context, path string) (status int, body []byte, err error) {
	url := c.scheme + "://" + c.host + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	if c.overTCP && c.token != "" {
		req.Header.Set("X-ClipBeam-Token", c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return resp.StatusCode, nil, readErr
	}
	return resp.StatusCode, b, nil
}

// portString renders a uint16 port; a zero port defaults to the frozen 8787.
func portString(p uint16) string {
	if p == 0 {
		p = 8787
	}
	return fmt.Sprintf("%d", p)
}

// controlGetTimeout is a generous client-side cap for a single long-poll GET (the
// server's /recv grace is poll+15, /wait is fixed 120 s); the per-call context still
// bounds the actual wait window (PLAN §3.10/§8.4).
const controlGetTimeout = 150 * time.Second
