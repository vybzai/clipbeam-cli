package httpd

import (
	"os"
	"path/filepath"
	"testing"
)

// shortSock returns a unix-socket path under /tmp, short enough for macOS's
// 104-byte sun_path limit. t.TempDir() under a long $TMPDIR (e.g. the
// /var/folders/... path on macOS CI runners) overflows it and bind() fails
// with EINVAL ("invalid argument"). Production socket paths are short
// ($XDG_RUNTIME_DIR/clipbeam/… or ~/.clipbeam/…); only long-TMPDIR tests need this.
func shortSock(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "cbsock")
	if err != nil {
		t.Fatalf("short socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return filepath.Join(d, "s.sock")
}
