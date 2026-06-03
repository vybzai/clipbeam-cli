//go:build !unix

package httpd

import (
	"errors"
	"net"
)

// listenUnixSocket is unsupported on the build-only Windows matrix (PLAN §7.8): the
// unix-domain-socket 0600 owner gate is a unix concern. The Windows receiver is
// deferred, so this fails closed rather than binding an unprotected listener.
func listenUnixSocket(path string) (net.Listener, error) {
	_ = path
	return nil, errors.New("clipbeam: unix-socket serve mode is not supported on this platform")
}
