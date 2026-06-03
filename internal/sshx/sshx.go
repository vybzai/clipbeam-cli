// Package sshx is the Tier-0 in-process SSH transport (PLAN §5.1, §5.4): a
// golang.org/x/crypto/ssh client with hand-rolled ~/.ssh/config resolution,
// knownhosts host-key policy (accept-new TOFU only under setup; never under data
// verbs), and ssh-agent discovery. The "reuse your SSH auth" promise is NOT free —
// these are protocol libraries; we own the config resolution, host-key policy, and
// agent discovery. BANNED: ssh.InsecureIgnoreHostKey, any non-knownhosts
// HostKeyCallback, any --insecure flag (CI grep-gated).
package sshx

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
)

// Sentinel transport errors mapped into the unified exit-code table (PLAN §8.3).
var (
	// ErrHostKeyUnknown is an unknown host under a data verb (never prompt; fail
	// closed with exit 6 and a hint to run `clipbeam setup user@host`, PLAN §5.4).
	ErrHostKeyUnknown = errors.New("clipbeam: host key unknown (run clipbeam setup)")
	// ErrHostKeyMismatch is a changed host key — ALWAYS hard-fail, exit 6 (§5.4).
	ErrHostKeyMismatch = errors.New("clipbeam: host key mismatch")
	// ErrHostKeyCAUnsupported is a CA-signed host key (@cert-authority) — a
	// documented v1 NON-GOAL with a specific error (PLAN §5.4 R14).
	ErrHostKeyCAUnsupported = errors.New("clipbeam: host certificate (cert-authority) unsupported")
	// ErrProxyJumpUnsupported is a resolved ProxyJump/Include — a documented v1
	// NON-GOAL with a specific message, never an opaque handshake failure (§5.4).
	ErrProxyJumpUnsupported = errors.New("clipbeam: ProxyJump/Include unsupported in clipbeam v1")
	// ErrDialFailed is any SSH-dial failure (maps to exit 6, PLAN §8.3).
	ErrDialFailed = errors.New("clipbeam: ssh dial failed")
)

// dialTimeout bounds the TCP+handshake phase so an unreachable host fails fast as
// ErrDialFailed (exit 6) rather than hanging the data verb.
const dialTimeout = 20 * time.Second

// Target is a resolved SSH destination (after ~/.ssh/config alias expansion).
type Target struct {
	User          string
	Host          string
	Port          int
	IdentityFiles []string // from ssh_config IdentityFile(s), else default keys
	ConfigAlias   string   // the ~/.ssh/config Host name, if any
	// AcceptNewHostKey records an unknown host non-interactively (setup /
	// --accept-new-hostkey / StrictHostKeyChecking=accept-new only, PLAN §5.4). Data
	// verbs leave this false and fail closed on an unknown host.
	AcceptNewHostKey bool
}

// addr returns the "host:port" dial target.
func (t Target) addr() string {
	port := t.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(t.Host, strconv.Itoa(port))
}

// Client is the in-process SSH client boundary consumed by send/setup verbs.
// Implementations dial with a knownhosts-backed HostKeyCallback (never nil, never
// InsecureIgnoreHostKey, PLAN §5.4) and authenticate via ssh-agent then IdentityFiles.
type Client interface {
	// Dial opens an SSH connection to the resolved target. It returns
	// ErrHostKeyUnknown/ErrHostKeyMismatch/ErrHostKeyCAUnsupported/
	// ErrProxyJumpUnsupported/ErrDialFailed as appropriate (PLAN §5.4/§8.3).
	Dial(t Target) (Session, error)
	// Close tears down the underlying connection.
	Close() error
}

// Session is one exec channel on a dialed connection. The daemonless-exec push runs
// the ABSOLUTE remote `clipbeam ingest` (a bare name frequently fails — minimal
// non-login PATH, PLAN §5.1) and streams a CB01 frame to its stdin, relaying the
// remote stdout (the saved abs path(s)).
type Session interface {
	// Run execs remoteCommand, writes stdin fully, and returns the captured stdout
	// (the saved path(s), one per line) + stderr. A non-zero remote exit is an error.
	Run(remoteCommand string, stdin []byte) (stdout []byte, stderr []byte, err error)
	// Close ends the session.
	Close() error
}

// client is the default in-process Client. It is constructed once (NewClient) and
// lazily opens an ssh.Client on the first Dial, keeping the connection for reuse +
// the in-process forward (0b).
type client struct {
	auth      authProvider
	hostKeys  hostKeyPolicy
	sshClient *ssh.Client
}

// NewClient constructs the in-process SSH client. It resolves auth methods in order
// (ssh-agent via $SSH_AUTH_SOCK, then IdentityFiles) and installs the
// knownhosts-backed HostKeyCallback (PLAN §5.4). It returns an error only if the
// host-key database files cannot be opened (an empty/missing known_hosts is fine —
// every host is then unknown, failing closed under data verbs).
func NewClient() (Client, error) {
	hk, err := newHostKeyPolicy(defaultKnownHostsFiles())
	if err != nil {
		return nil, err
	}
	return &client{auth: defaultAuthProvider(), hostKeys: hk}, nil
}

// Dial opens (or reuses) the SSH connection to t and returns an exec Session. Auth is
// ssh-agent first, then the target's IdentityFiles / default keys (PLAN §5.4). The
// HostKeyCallback is the knownhosts policy, never nil (a nil callback panics at dial).
func (c *client) Dial(t Target) (Session, error) {
	sc, err := c.ssh(t)
	if err != nil {
		return nil, err
	}
	sess, err := sc.NewSession()
	if err != nil {
		return nil, fmt.Errorf("%w: open session: %v", ErrDialFailed, err)
	}
	return &execSession{sess: sess}, nil
}

// ssh returns the dialed ssh.Client, opening it on first use.
func (c *client) ssh(t Target) (*ssh.Client, error) {
	if c.sshClient != nil {
		return c.sshClient, nil
	}
	user := t.User
	if user == "" {
		user = os.Getenv("USER")
	}
	authMethods, err := c.auth.methods(t)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: c.hostKeys.callback(t),
		Timeout:         dialTimeout,
	}
	conn, err := ssh.Dial("tcp", t.addr(), cfg)
	if err != nil {
		// A host-key failure surfaces from the callback wrapped in the handshake
		// error; map the typed sentinels through so the exit code is right (PLAN §8.3).
		if mapped := mapHostKeyErr(err); mapped != nil {
			return nil, mapped
		}
		return nil, fmt.Errorf("%w: %v", ErrDialFailed, err)
	}
	c.sshClient = conn
	return conn, nil
}

// Forward opens an in-process forward to the remote serve socket/port (PLAN §5.1 0b):
// a direct-streamlocal channel for a unix socket, or direct-tcpip for a loopback TCP
// port. The returned net.Conn carries the HTTP control traffic to the remote daemon
// over the existing SSH connection — no extra port, no system ssh. network is "unix"
// (addr = remote socket path) or "tcp" (addr = "host:port", typically 127.0.0.1:N).
func (c *client) Forward(t Target, network, addr string) (net.Conn, error) {
	sc, err := c.ssh(t)
	if err != nil {
		return nil, err
	}
	switch network {
	case "unix":
		conn, derr := sc.Dial("unix", addr)
		if derr != nil {
			return nil, fmt.Errorf("clipbeam: ssh direct-streamlocal to %s: %w", addr, derr)
		}
		return conn, nil
	case "tcp", "tcp4", "tcp6":
		conn, derr := sc.Dial("tcp", addr)
		if derr != nil {
			return nil, fmt.Errorf("clipbeam: ssh direct-tcpip to %s: %w", addr, derr)
		}
		return conn, nil
	default:
		return nil, fmt.Errorf("clipbeam: unsupported forward network %q", network)
	}
}

// Close tears down the underlying ssh.Client (and thus every channel/forward on it).
func (c *client) Close() error {
	if c.sshClient == nil {
		return nil
	}
	err := c.sshClient.Close()
	c.sshClient = nil
	return err
}

// mapHostKeyErr inspects a dial error for an embedded knownhosts host-key failure and
// returns the typed sentinel; nil if the error is not host-key-related (so the caller
// falls back to ErrDialFailed). The sentinels are wrapped by the host-key callback, so
// errors.Is matches across the ssh.Dial error wrapping.
func mapHostKeyErr(err error) error {
	switch {
	case errors.Is(err, ErrHostKeyMismatch):
		return ErrHostKeyMismatch
	case errors.Is(err, ErrHostKeyUnknown):
		return ErrHostKeyUnknown
	case errors.Is(err, ErrHostKeyCAUnsupported):
		return ErrHostKeyCAUnsupported
	}
	return nil
}

// defaultKnownHostsFiles returns the known_hosts files probed by the host-key policy:
// the user's ~/.ssh/known_hosts and the system /etc/ssh/ssh_known_hosts (PLAN §5.4).
// A missing file is tolerated (every host is then unknown).
func defaultKnownHostsFiles() []string {
	var files []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		files = append(files, filepath.Join(home, ".ssh", "known_hosts"))
	}
	files = append(files, "/etc/ssh/ssh_known_hosts")
	return files
}

// defaultUserKnownHosts returns the writable ~/.ssh/known_hosts path accept-new TOFU
// appends to (PLAN §5.4); never the system file (not writable by a normal user).
func defaultUserKnownHosts() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("clipbeam: cannot resolve home directory for known_hosts")
	}
	return filepath.Join(home, ".ssh", "known_hosts"), nil
}

// ResolveTarget expands a user@host[:port] spec (or a ~/.ssh/config alias) into a
// Target via kevinburke/ssh_config (HostName/User/Port/IdentityFile). It detects a
// resolved ProxyJump/Include and returns ErrProxyJumpUnsupported (PLAN §5.4).
func ResolveTarget(spec string) (Target, error) {
	return resolveTargetWith(defaultConfigResolver{}, spec)
}
