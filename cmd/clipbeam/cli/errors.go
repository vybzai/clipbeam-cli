package cli

import (
	"errors"
	"fmt"

	"github.com/vybzai/clipbeam-cli/internal/sshx"
)

// codedError carries an explicit exit code from the unified table (PLAN §8.3) up to
// Execute, which maps it to the process exit status and (under --json) the structured
// error envelope. Verbs return a codedError so the exit code is decided at the failure
// site (where the provenance — HTTP status, SSH-dial, local pre-flight — is known),
// not re-derived from an opaque string.
type codedError struct {
	code int
	err  error
}

// Error renders the wrapped error message.
func (e *codedError) Error() string { return e.err.Error() }

// Unwrap exposes the wrapped error for errors.Is/As.
func (e *codedError) Unwrap() error { return e.err }

// coded wraps err with an explicit exit code (PLAN §8.3).
func coded(code int, err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: code, err: err}
}

// codedf wraps a formatted message with an explicit exit code.
func codedf(code int, format string, args ...any) error {
	return &codedError{code: code, err: fmt.Errorf(format, args...)}
}

// usageError flags a bad-arguments failure (exit 2, matching the shim's `exit 2`,
// PLAN §8.3). cobra's own arg/flag validation errors are mapped to ExitUsage in
// exitCodeFor as well.
func usageError(format string, args ...any) error {
	return codedf(ExitUsage, format, args...)
}

// configError flags a config / identity failure (exit 8): no peer, no token, bad
// config, CA-unsupported (PLAN §8.3).
func configError(format string, args ...any) error {
	return codedf(ExitConfig, format, args...)
}

// httpStatusToExit maps a raw peer HTTP status to the unified exit table (PLAN §8.3).
// Exit codes 4-9 with a peer source are CLIENT-derived: the Swift Sender folds
// 400/405/411/431/500/502/503 into a generic .io string with no typed wire field, so
// the Go CLI (which owns the client) reads the raw status itself. A 2xx is success
// (code 0). Anything not explicitly mapped is a generic remote I/O error (9) when it
// is a 5xx, else a generic failure (1).
func httpStatusToExit(status int) int {
	switch status {
	case 401:
		return ExitUnauthorized // 4
	case 403:
		return ExitForbidden // 5
	case 413:
		return ExitTooLarge // 7
	case 500:
		return ExitRemoteIO // 9 (real errno relayed by the peer)
	case 502:
		return ExitUnreachable // 6 (client observing an upstream proxy's 502)
	}
	switch {
	case status >= 200 && status < 300:
		return ExitOK
	case status >= 500:
		return ExitRemoteIO // 9 — other 5xx (503, etc.) are remote-side failures
	default:
		return ExitGeneric // 1 — unmapped 4xx (400/405/411/431) is a client/protocol bug
	}
}

// sshErrToExit maps an sshx transport sentinel to the unified table (PLAN §8.3):
// host-key unknown/mismatch and dial failures are "unreachable" (6); a CA-signed key
// or a resolved ProxyJump is a config/identity error (8 — a documented v1 non-goal).
// A nil error is success (0); an unrecognized error is generic (1).
func sshErrToExit(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, sshx.ErrHostKeyUnknown),
		errors.Is(err, sshx.ErrHostKeyMismatch),
		errors.Is(err, sshx.ErrDialFailed):
		return ExitUnreachable // 6
	case errors.Is(err, sshx.ErrHostKeyCAUnsupported),
		errors.Is(err, sshx.ErrProxyJumpUnsupported):
		return ExitConfig // 8 — documented v1 non-goal, an identity/config limitation
	default:
		return ExitGeneric // 1
	}
}

// codedSSH wraps an sshx transport error with its mapped exit code (PLAN §8.3).
func codedSSH(err error) error {
	if err == nil {
		return nil
	}
	return coded(sshErrToExit(err), err)
}
