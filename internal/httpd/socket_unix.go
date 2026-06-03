//go:build unix

package httpd

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// listenUnixSocket binds the 0600 unix-domain socket for the Linux default serve mode
// (PLAN §5.1). It (1) ensures the parent dir exists 0700; (2) REFUSES to bind if an
// existing socket path is owned by another uid (hard requirement — a hostile co-tenant
// must not pre-create the path); (3) unlinks a stale socket it owns; (4) sets
// umask(0177) around the bind so the socket inode is created 0600 even on platforms
// that ignore a post-bind chmod; (5) chmods 0600 after bind as a belt. The socket
// owner uid is then the only principal that can connect — the control-surface gate
// (PLAN §6.2) treats that as authorization.
func listenUnixSocket(path string) (net.Listener, error) {
	if path == "" {
		return nil, fmt.Errorf("clipbeam: empty unix socket path")
	}

	// Parent dir 0700 (it holds the owner-only socket).
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("clipbeam: create socket dir: %w", err)
	}

	// Refuse-or-unlink an existing path. A path owned by another uid is a hard refusal
	// (a co-tenant must not hand us a socket they control); a stale path we own is
	// unlinked so the bind can recreate it.
	if fi, err := os.Lstat(path); err == nil {
		st, ok := fi.Sys().(*unix.Stat_t)
		if !ok {
			return nil, fmt.Errorf("clipbeam: cannot stat existing socket %q", path)
		}
		if uint32(st.Uid) != uint32(os.Getuid()) {
			return nil, fmt.Errorf("clipbeam: refusing to bind socket %q owned by uid %d (not %d)", path, st.Uid, os.Getuid())
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("clipbeam: unlink stale socket %q: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("clipbeam: stat socket %q: %w", path, err)
	}

	// umask(0177) so the socket inode is created with mode 0600 atomically; restore
	// the previous umask immediately after the bind.
	old := unix.Umask(0o177)
	ln, err := net.Listen("unix", path)
	unix.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("clipbeam: bind unix socket %q: %w", path, err)
	}

	// Belt-and-suspenders chmod 0600 (umask already constrained creation).
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("clipbeam: chmod socket %q: %w", path, err)
	}

	// Remove the socket file on close so a later bind does not see it as stale-owned.
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(true)
	}
	return ln, nil
}
