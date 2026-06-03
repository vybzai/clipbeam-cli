package httpd

import (
	"errors"
	"io"
	"net/http"
)

// readAllBounded drains r fully into memory. It is used only for the /agent-send text
// body, which is bounded by http.MaxBytesReader at the call site (PLAN §3.4): a text
// message is small, so buffering it whole is correct (unlike /clip image/file
// payloads, which stream to disk).
func readAllBounded(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

// asMaxBytes reports whether err is (or wraps) an *http.MaxBytesError, binding target.
// A MaxBytesReader trip maps to the explicit 413 (PLAN §3.8c/§3.11).
func asMaxBytes(err error, target **http.MaxBytesError) bool {
	return errors.As(err, target)
}
