package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// newHostKey returns a fresh ed25519 ssh.PublicKey for host-key policy tests.
func newHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

// fakeAddr is a net.Addr for the remote in callback tests.
type fakeAddr struct{ s string }

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return a.s }

// TestAcceptNewWritesLine asserts accept-new TOFU records a verified known_hosts line
// for a genuinely unknown host (PLAN §5.4), and that a later verification of the SAME
// key against the now-populated file succeeds.
func TestAcceptNewWritesLine(t *testing.T) {
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")

	policy, err := newHostKeyPolicy([]string{kh}) // file does not exist yet → all unknown
	if err != nil {
		t.Fatal(err)
	}
	policy.userKnownHosts = kh // record accepted lines here

	key := newHostKey(t)
	target := Target{Host: "box.example", Port: 22, AcceptNewHostKey: true}
	cb := policy.callback(target)

	remote := fakeAddr{s: "203.0.113.7:22"}
	if err := cb("box.example:22", remote, key); err != nil {
		t.Fatalf("accept-new should record the unknown host, got %v", err)
	}

	data, rerr := os.ReadFile(kh)
	if rerr != nil {
		t.Fatalf("known_hosts not written: %v", rerr)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		t.Fatalf("known_hosts is empty after accept-new")
	}

	// Re-verify the SAME key against the now-populated file: it must be trusted.
	verify, verr := knownhosts.New(kh)
	if verr != nil {
		t.Fatal(verr)
	}
	if err := verify("box.example:22", remote, key); err != nil {
		t.Errorf("recorded key should verify, got %v", err)
	}
}

// TestDataVerbUnknownFailsClosed asserts a data verb (AcceptNewHostKey=false) NEVER
// records an unknown host — it fails closed with ErrHostKeyUnknown (exit 6, §5.4).
func TestDataVerbUnknownFailsClosed(t *testing.T) {
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")
	policy, err := newHostKeyPolicy([]string{kh})
	if err != nil {
		t.Fatal(err)
	}
	policy.userKnownHosts = kh

	cb := policy.callback(Target{Host: "box.example", Port: 22, AcceptNewHostKey: false})
	err = cb("box.example:22", fakeAddr{s: "203.0.113.7:22"}, newHostKey(t))
	if !errors.Is(err, ErrHostKeyUnknown) {
		t.Errorf("data-verb unknown host err = %v, want ErrHostKeyUnknown", err)
	}
	// Nothing must have been written.
	if _, serr := os.Stat(kh); serr == nil {
		t.Errorf("data verb must NOT create known_hosts")
	}
}

// TestChangedKeyHardFails asserts a host whose recorded key differs from the presented
// key ALWAYS hard-fails with ErrHostKeyMismatch — even under accept-new — never an
// auto-accept (PLAN §5.4).
func TestChangedKeyHardFails(t *testing.T) {
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")

	// Pre-populate known_hosts with the OLD key for box.example.
	oldKey := newHostKey(t)
	line := knownhosts.Line([]string{knownhosts.Normalize("box.example:22")}, oldKey)
	if werr := os.WriteFile(kh, []byte(line+"\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}

	policy, err := newHostKeyPolicy([]string{kh})
	if err != nil {
		t.Fatal(err)
	}
	policy.userKnownHosts = kh

	// Present a DIFFERENT key — even with accept-new on, this is a changed key.
	newKey := newHostKey(t)
	cb := policy.callback(Target{Host: "box.example", Port: 22, AcceptNewHostKey: true})
	err = cb("box.example:22", fakeAddr{s: "203.0.113.7:22"}, newKey)
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Errorf("changed key err = %v, want ErrHostKeyMismatch", err)
	}
}

// TestKnownKeyTrusted asserts a presented key that matches the recorded line verifies
// cleanly (nil error) — the happy path that does not record or fail.
func TestKnownKeyTrusted(t *testing.T) {
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")

	key := newHostKey(t)
	line := knownhosts.Line([]string{knownhosts.Normalize("box.example:22")}, key)
	if werr := os.WriteFile(kh, []byte(line+"\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}

	policy, err := newHostKeyPolicy([]string{kh})
	if err != nil {
		t.Fatal(err)
	}
	cb := policy.callback(Target{Host: "box.example", Port: 22, AcceptNewHostKey: false})
	if err := cb("box.example:22", fakeAddr{s: "203.0.113.7:22"}, key); err != nil {
		t.Errorf("known matching key must verify, got %v", err)
	}
}

// TestCertAuthorityUnsupported asserts a CA-signed host certificate is rejected with
// the specific ErrHostKeyCAUnsupported sentinel (PLAN §5.4 documented non-goal).
func TestCertAuthorityUnsupported(t *testing.T) {
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")
	policy, err := newHostKeyPolicy([]string{kh})
	if err != nil {
		t.Fatal(err)
	}
	// Build a host certificate over a fresh key.
	cert := &ssh.Certificate{
		Key:         newHostKey(t),
		CertType:    ssh.HostCert,
		ValidBefore: ssh.CertTimeInfinity,
	}
	cb := policy.callback(Target{Host: "box.example", Port: 22, AcceptNewHostKey: true})
	var key ssh.PublicKey = cert
	if err := cb("box.example:22", fakeAddr{s: "203.0.113.7:22"}, key); !errors.Is(err, ErrHostKeyCAUnsupported) {
		t.Errorf("cert host key err = %v, want ErrHostKeyCAUnsupported", err)
	}
}

// _ keeps net imported for the fakeAddr type assertion to net.Addr.
var _ net.Addr = fakeAddr{}
