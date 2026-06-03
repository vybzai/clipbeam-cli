package cli

import (
	"bytes"
	"testing"

	"github.com/vybzai/clipbeam-cli/internal/sshx"
)

// --- shared fake SSH Client/Session harness (used by [D] send tests and [C] release
// installer tests) -----------------------------------------------------------------

// fakeSession is a fake sshx.Session capturing the remote command + stdin and returning
// a canned stdout/stderr/exit.
type fakeSession struct {
	gotCommand string
	gotStdin   []byte
	stdout     []byte
	stderr     []byte
	runErr     error
}

func (f *fakeSession) Run(remoteCommand string, stdin []byte) (stdout []byte, stderr []byte, err error) {
	f.gotCommand = remoteCommand
	f.gotStdin = append([]byte(nil), stdin...)
	return f.stdout, f.stderr, f.runErr
}

func (f *fakeSession) Close() error { return nil }

// fakeClient is a fake sshx.Client that returns a pre-built fakeSession on Dial and
// records the dialed target.
type fakeClient struct {
	sess      *fakeSession
	dialErr   error
	dialedTgt sshx.Target
	dialCount int
}

func (c *fakeClient) Dial(t sshx.Target) (sshx.Session, error) {
	c.dialCount++
	c.dialedTgt = t
	if c.dialErr != nil {
		return nil, c.dialErr
	}
	return c.sess, nil
}

func (c *fakeClient) Close() error { return nil }

// withFakeSSHClient swaps newSSHClient to return fc for the duration of the test and
// restores it via t.Cleanup.
func withFakeSSHClient(t *testing.T, fc *fakeClient) {
	t.Helper()
	prev := newSSHClient
	newSSHClient = func() (sshx.Client, error) { return fc, nil }
	t.Cleanup(func() { newSSHClient = prev })
}

// dummyTarget is a throwaway sshx.Target for installer tests that only need a Dial.
func dummyTarget() sshx.Target { return sshx.Target{User: "u", Host: "h", Port: 22} }

// --- [D] tests ---------------------------------------------------------------------

// TestRemoteIngestCommandAbsolute locks the absolute-path branch of remoteIngestCommand
// (verbs_send.go): the recorded ABSOLUTE remoteBinPath is shell-quoted + " ingest", a
// PATH-independent invocation. Path-with-space and path-with-single-quote cases lock
// shellQuote.
func TestRemoteIngestCommandAbsolute(t *testing.T) {
	if got := remoteIngestCommand("/home/u/.local/bin/clipbeam"); got != "'/home/u/.local/bin/clipbeam' ingest" {
		t.Errorf("absolute: got %q", got)
	}
	if got := remoteIngestCommand("/home/My User/bin/clipbeam"); got != "'/home/My User/bin/clipbeam' ingest" {
		t.Errorf("path-with-space: got %q", got)
	}
	// A single quote in the path is escaped via the '\'' idiom.
	if got := remoteIngestCommand("/home/o'brien/clipbeam"); got != `'/home/o'\''brien/clipbeam' ingest` {
		t.Errorf("path-with-single-quote: got %q", got)
	}
}

// TestRemoteIngestCommandBareFallback locks the documented bare fallback for a literal
// target with no recorded path (verbs_send.go:205). The product default keeps the bare
// `clipbeam ingest` form (see verbs_send.go:103-105).
func TestRemoteIngestCommandBareFallback(t *testing.T) {
	if got := remoteIngestCommand(""); got != "clipbeam ingest" {
		t.Errorf("bare fallback: got %q, want %q", got, "clipbeam ingest")
	}
}

// TestPushCB01ThreadsRemoteBinPath is the [D] guarantee: the alias's recorded
// RemoteBinPath reaches the SSH-exec remote command string (an ABSOLUTE invocation, not
// a bare `clipbeam`), and the relayed remote stdout becomes sendOutcome.remotePath.
func TestPushCB01ThreadsRemoteBinPath(t *testing.T) {
	sess := &fakeSession{stdout: []byte("/remote/saved/note.txt")}
	fc := &fakeClient{sess: sess}
	withFakeSSHClient(t, fc)

	o := out{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	items := []sshx.CB01Item{{Kind: sshx.KindByte("text"), Payload: []byte("hi")}}
	rt := resolvedTarget{remoteBinPath: "/abs/clipbeam", transport: "ssh"}

	outcome, err := pushCB01(o, rt, "clipboard", items)
	if err != nil {
		t.Fatalf("pushCB01: %v", err)
	}
	want := shellQuote("/abs/clipbeam") + " ingest"
	if sess.gotCommand != want {
		t.Errorf("remote command = %q, want %q", sess.gotCommand, want)
	}
	if outcome.remotePath != "/remote/saved/note.txt" {
		t.Errorf("remotePath = %q, want the relayed remote stdout", outcome.remotePath)
	}
	if outcome.sentItems != 1 {
		t.Errorf("sentItems = %d, want 1", outcome.sentItems)
	}
}
