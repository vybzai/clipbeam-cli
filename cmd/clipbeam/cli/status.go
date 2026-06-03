package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
)

// transportErr renders a connection-level failure (no HTTP status reached) as a
// human message. A dial failure to the local daemon usually means it is not running;
// the message hints at that without guessing the cause.
func transportErr(err error) error {
	var ne net.Error
	if errors.As(err, &ne) {
		return fmt.Errorf("cannot reach the clipbeam daemon (%v) — is `clipbeam serve` running?", err)
	}
	return fmt.Errorf("cannot reach the clipbeam daemon: %w (is `clipbeam serve` running?)", err)
}

// statusErr maps a non-2xx control/peer HTTP status to a codedError using the unified
// table (PLAN §8.3). It extracts the peer's error reason from the frozen
// {"ok":false,"error":"…"} body when present so the surfaced message carries the real
// cause (e.g. the relayed ENOSPC errno on a 500).
func statusErr(status int, body []byte) error {
	reason := peerErrorReason(body)
	code := httpStatusToExit(status)
	if reason == "" {
		return codedf(code, "peer HTTP %d", status)
	}
	return codedf(code, "peer HTTP %d: %s", status, reason)
}

// peerErrorReason extracts the "error" field from the frozen JSON error body, or ""
// when the body is not that shape (e.g. a plain-text body from an upstream proxy).
func peerErrorReason(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return ""
	}
	return e.Error
}
