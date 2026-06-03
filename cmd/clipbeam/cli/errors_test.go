package cli

import (
	"errors"
	"testing"

	"github.com/vybzai/clipbeam-cli/internal/sshx"
)

// TestHTTPStatusToExit verifies the client-derived HTTP→exit mapping (PLAN §8.3): the
// Go CLI reads the raw peer status (the Swift Sender folds these into a generic .io
// string, so the CLI owns the mapping).
func TestHTTPStatusToExit(t *testing.T) {
	cases := []struct {
		status int
		want   int
	}{
		{200, ExitOK},
		{204, ExitOK},
		{299, ExitOK},
		{401, ExitUnauthorized}, // 4
		{403, ExitForbidden},    // 5
		{413, ExitTooLarge},     // 7
		{500, ExitRemoteIO},     // 9 (real errno relayed)
		{502, ExitUnreachable},  // 6 (upstream proxy)
		{503, ExitRemoteIO},     // 9 (other 5xx)
		{400, ExitGeneric},      // 1 (unmapped 4xx — client/protocol)
		{405, ExitGeneric},
		{411, ExitGeneric},
		{431, ExitGeneric},
	}
	for _, c := range cases {
		if got := httpStatusToExit(c.status); got != c.want {
			t.Errorf("httpStatusToExit(%d) = %d, want %d", c.status, got, c.want)
		}
	}
}

// TestSSHErrToExit verifies the SSH-derived mapping (PLAN §8.3): host-key/dial →
// unreachable (6); CA/ProxyJump → config (8); nil → 0; unknown → generic (1).
func TestSSHErrToExit(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, ExitOK},
		{"host-key-unknown", sshx.ErrHostKeyUnknown, ExitUnreachable},
		{"host-key-mismatch", sshx.ErrHostKeyMismatch, ExitUnreachable},
		{"dial-failed", sshx.ErrDialFailed, ExitUnreachable},
		{"ca-unsupported", sshx.ErrHostKeyCAUnsupported, ExitConfig},
		{"proxyjump-unsupported", sshx.ErrProxyJumpUnsupported, ExitConfig},
		{"unknown", errors.New("some other error"), ExitGeneric},
	}
	for _, c := range cases {
		if got := sshErrToExit(c.err); got != c.want {
			t.Errorf("sshErrToExit(%s) = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestSSHErrToExit_Wrapped verifies the mapping follows wrapped errors (errors.Is).
func TestSSHErrToExit_Wrapped(t *testing.T) {
	wrapped := errors.New("dial 1.2.3.4: " + sshx.ErrDialFailed.Error())
	// A string-concatenated error does NOT wrap; build a real wrap to test errors.Is.
	realWrap := wrapErr(sshx.ErrHostKeyMismatch, "connecting to box")
	if got := sshErrToExit(realWrap); got != ExitUnreachable {
		t.Errorf("sshErrToExit(wrapped mismatch) = %d, want %d", got, ExitUnreachable)
	}
	_ = wrapped
}

// wrapErr builds a wrapping error for the errors.Is test.
func wrapErr(err error, msg string) error {
	return &wrapped{msg: msg, err: err}
}

type wrapped struct {
	msg string
	err error
}

func (w *wrapped) Error() string { return w.msg + ": " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }

// TestCodedError verifies the codedError carries its exit code through errors.As and
// that exitCodeFor reads it.
func TestCodedError(t *testing.T) {
	err := coded(ExitTooLarge, errors.New("payload too large"))
	var ce *codedError
	if !errors.As(err, &ce) {
		t.Fatal("coded() result is not a *codedError")
	}
	if ce.code != ExitTooLarge {
		t.Errorf("code = %d, want %d", ce.code, ExitTooLarge)
	}

	// A nil error wrapped is nil.
	if coded(ExitGeneric, nil) != nil {
		t.Error("coded(_, nil) should be nil")
	}
}

// TestStatusErr verifies a non-2xx status produces a codedError with the mapped code
// and surfaces the peer's error reason from the frozen JSON body.
func TestStatusErr(t *testing.T) {
	err := statusErr(500, []byte(`{"ok":false,"error":"write failed: no space left on device"}`))
	var ce *codedError
	if !errors.As(err, &ce) {
		t.Fatal("statusErr did not return a *codedError")
	}
	if ce.code != ExitRemoteIO {
		t.Errorf("code = %d, want %d (500 → remote I/O)", ce.code, ExitRemoteIO)
	}
	if got := err.Error(); got == "" || !contains(got, "no space left on device") {
		t.Errorf("error = %q, want it to surface the relayed errno", got)
	}

	// A 413 with no JSON body still maps to ExitTooLarge.
	err = statusErr(413, []byte("Payload Too Large"))
	errors.As(err, &ce)
	if ce.code != ExitTooLarge {
		t.Errorf("413 code = %d, want %d", ce.code, ExitTooLarge)
	}
}

// contains is a tiny substring check (avoids importing strings just for the test).
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestPeerErrorReason verifies extraction of the frozen error body shape.
func TestPeerErrorReason(t *testing.T) {
	if got := peerErrorReason([]byte(`{"ok":false,"error":"unauthorized"}`)); got != "unauthorized" {
		t.Errorf("reason = %q, want unauthorized", got)
	}
	if got := peerErrorReason([]byte("plain text")); got != "" {
		t.Errorf("reason = %q, want empty for non-JSON", got)
	}
	if got := peerErrorReason(nil); got != "" {
		t.Errorf("reason = %q, want empty for nil body", got)
	}
}
