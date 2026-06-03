//go:build !unix

package httpd

import "syscall"

// controlClearV6Only is a no-op on non-unix platforms (the build-only Windows
// matrix, PLAN §7.8). The dual-stack V6ONLY clear is a unix concern.
func controlClearV6Only(network, address string, c syscall.RawConn) error {
	_ = network
	_ = address
	_ = c
	return nil
}
