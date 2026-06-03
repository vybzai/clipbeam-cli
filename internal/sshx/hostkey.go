package sshx

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// hostKeyPolicy is the knownhosts-backed HostKeyCallback factory (PLAN §5.4). It
// verifies against known_hosts and, ONLY when the target opts into accept-new TOFU
// (setup / --accept-new-hostkey / StrictHostKeyChecking=accept-new), records a
// genuinely unknown host's verified line. A changed key ALWAYS hard-fails. The
// callback is NEVER nil and NEVER ssh.InsecureIgnoreHostKey (both CI grep-banned).
type hostKeyPolicy struct {
	// verify is the knownhosts.New callback over the probed known_hosts files. It
	// returns a *knownhosts.KeyError (empty Want = unknown host, non-empty = mismatch)
	// or a *knownhosts.RevokedError; nil means the key is trusted.
	verify ssh.HostKeyCallback
	// userKnownHosts is the writable file accept-new TOFU appends a verified line to.
	userKnownHosts string
}

// newHostKeyPolicy builds the policy over files (a missing file is fine — every host
// is then unknown, failing closed under data verbs). An error is returned only if a
// present file cannot be parsed.
func newHostKeyPolicy(files []string) (hostKeyPolicy, error) {
	existing := make([]string, 0, len(files))
	for _, f := range files {
		if _, err := os.Stat(f); err == nil {
			existing = append(existing, f)
		}
	}
	var cb ssh.HostKeyCallback
	if len(existing) > 0 {
		var err error
		cb, err = knownhosts.New(existing...)
		if err != nil {
			return hostKeyPolicy{}, fmt.Errorf("clipbeam: parse known_hosts: %w", err)
		}
	}
	userKH, _ := defaultUserKnownHosts()
	return hostKeyPolicy{verify: cb, userKnownHosts: userKH}, nil
}

// callback returns the ssh.HostKeyCallback for one dial. It closes over the target's
// AcceptNewHostKey flag so a data verb (flag false) fails closed on an unknown host
// while setup (flag true) records it (PLAN §5.4). The returned callback is never nil.
func (p hostKeyPolicy) callback(t Target) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		// A CA-signed host key (@cert-authority) is a documented v1 non-goal: knownhosts
		// does not handle host certs (Go #33366). Reject with the specific sentinel so
		// the user gets a clear error, never an opaque handshake failure (PLAN §5.4).
		if _, isCert := key.(*ssh.Certificate); isCert {
			return ErrHostKeyCAUnsupported
		}

		// No known_hosts files at all → every host is unknown.
		if p.verify == nil {
			return p.handleUnknown(t, hostname, remote, key)
		}

		err := p.verify(hostname, remote, key)
		if err == nil {
			return nil // trusted: the key matches a known_hosts line.
		}

		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) {
			if len(keyErr.Want) == 0 {
				// Unknown host: not in known_hosts at all.
				return p.handleUnknown(t, hostname, remote, key)
			}
			// Non-empty Want = the host IS known but the presented key differs — a
			// changed key. ALWAYS hard-fail, never auto-accept (PLAN §5.4).
			return ErrHostKeyMismatch
		}

		var revoked *knownhosts.RevokedError
		if errors.As(err, &revoked) {
			return ErrHostKeyMismatch // a revoked key is a mismatch-class hard failure.
		}

		// Any other verification error is treated as a mismatch-class failure (fail
		// closed — never trust on an ambiguous verification error).
		return ErrHostKeyMismatch
	}
}

// handleUnknown decides the unknown-host outcome: record the verified line under
// accept-new TOFU (setup), else fail closed with ErrHostKeyUnknown so a data verb
// never prompts and exits 6 with a hint to run `clipbeam setup` (PLAN §5.4).
func (p hostKeyPolicy) handleUnknown(t Target, hostname string, remote net.Addr, key ssh.PublicKey) error {
	if !t.AcceptNewHostKey {
		return ErrHostKeyUnknown
	}
	if p.userKnownHosts == "" {
		return fmt.Errorf("%w: cannot resolve a writable known_hosts to record the new key", ErrHostKeyUnknown)
	}
	if err := appendKnownHost(p.userKnownHosts, hostname, remote, key); err != nil {
		return fmt.Errorf("%w: record new host key: %v", ErrHostKeyUnknown, err)
	}
	return nil // accepted: the verified line is now persisted.
}

// appendKnownHost writes a single verified known_hosts line for the accepted host
// (PLAN §5.4: hand-rolled accept-new TOFU via knownhosts.Normalize/Line). It records
// both the dialed hostname address and the remote IP address so a later lookup by
// either matches. The file/parent are created 0700/0644 (known_hosts is conventionally
// world-readable; it holds only public keys).
func appendKnownHost(path, hostname string, remote net.Addr, key ssh.PublicKey) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	addresses := knownHostAddresses(hostname, remote)
	line := knownhosts.Line(addresses, key)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line + "\n"); err != nil {
		return err
	}
	return f.Sync()
}

// knownHostAddresses returns the normalized address forms to record for an accepted
// host: the dialed hostname:port and the remote IP:port (so a lookup by either the
// name the user typed or the resolved IP matches the recorded line).
func knownHostAddresses(hostname string, remote net.Addr) []string {
	seen := map[string]bool{}
	var out []string
	add := func(a string) {
		if a == "" {
			return
		}
		n := knownhosts.Normalize(a)
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, n)
	}
	add(hostname)
	if remote != nil {
		add(remote.String())
	}
	return out
}
