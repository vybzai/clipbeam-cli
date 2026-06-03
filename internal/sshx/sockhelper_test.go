package sshx

import (
	"os"
	"path/filepath"
	"testing"
)

// shortSock returns a unix-socket path under /tmp, short enough for macOS's
// 104-byte sun_path limit (t.TempDir() under a long $TMPDIR overflows it and
// bind() fails with EINVAL).
func shortSock(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "cbsock")
	if err != nil {
		t.Fatalf("short socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return filepath.Join(d, "s.sock")
}
