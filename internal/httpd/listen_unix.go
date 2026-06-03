//go:build unix

package httpd

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// controlClearV6Only is the net.ListenConfig.Control hook that clears IPV6_V6ONLY so
// a single [::]:PORT bind serves IPv4+IPv6 (PLAN §3.3). It is REQUIRED on darwin
// (IPV6_V6ONLY defaults to 1 on macOS/BSD); the ::ffff:a.b.c.d shim is unwrapped in
// classify. Best-effort: a setsockopt failure on a v4-only socket is non-fatal, so
// the hook records but does not surface the error (the bind still proceeds).
func controlClearV6Only(network, address string, c syscall.RawConn) error {
	var setErr error
	err := c.Control(func(fd uintptr) {
		// 0 = allow IPv4-mapped IPv6 (clear V6ONLY). Ignored cleanly for AF_INET.
		setErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0)
	})
	if err != nil {
		return err
	}
	_ = setErr // non-fatal on v4-only sockets
	return nil
}
