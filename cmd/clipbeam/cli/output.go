package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// schemaVersion is the CLI JSON envelope version (PLAN §8.2): every --json payload
// begins with "schema":"clipbeam.v1". It is DISTINCT from the wire Envelope.version:1
// integer (the wire stays v1 forever; this CLI schema bumps only on a breaking JSON
// change).
const schemaVersion = "clipbeam.v1"

// wireProtocol is the frozen wire-protocol identifier surfaced in version/schema
// (PLAN §8.2/§8.5). The on-the-wire Envelope.version integer is 1 forever.
const wireProtocol = "envelope-v1"

// out bundles the output sinks + the resolved output mode for one command run, so
// every verb obeys the §8.1 discipline (stdout=data, stderr=diagnostics) without
// re-reading the global flags. It is built by newOut at the top of each RunE.
type out struct {
	stdout  io.Writer
	stderr  io.Writer
	json    bool
	quiet   bool
	verbose bool
	// color is the resolved ANSI-color decision for this run (PLAN §8.1): false under
	// --json / --no-color / NO_COLOR / off-TTY. clipbeam emits no decorative color
	// today, so a colorized diagnostic added later consults this single resolved value
	// rather than re-deriving the policy at each call site.
	color bool
}

// newOut resolves the effective output mode for a command from the global flags and
// the environment (PLAN §8.1): --json (or CLIPBEAM_JSON) flips JSON mode; --quiet
// silences stderr; --verbose adds a stderr trace; color is resolved once via
// colorEnabled (--json implies --no-color).
func newOut(cmd *cobra.Command) out {
	return out{
		stdout:  cmd.OutOrStdout(),
		stderr:  cmd.ErrOrStderr(),
		json:    gflags.json,
		quiet:   gflags.quiet,
		verbose: gflags.verbose,
		color:   colorEnabled(),
	}
}

// useColor reports the resolved ANSI-color decision for this run (PLAN §8.1). It is the
// single authority any colorized diagnostic consults; clipbeam emits no decorative
// color in v1, so today it gates nothing visible but keeps the policy in one place.
func (o out) useColor() bool { return o.color }

// diag writes a human diagnostic line to stderr unless --quiet (PLAN §8.1). It never
// touches stdout. A trailing newline is added.
func (o out) diag(format string, args ...any) {
	if o.quiet {
		return
	}
	_, _ = fmt.Fprintf(o.stderr, format+"\n", args...)
}

// trace writes a verbose-only diagnostic line to stderr (PLAN §8.1: --verbose adds a
// stderr trace). Suppressed unless --verbose, and always suppressed under --quiet.
func (o out) trace(format string, args ...any) {
	if o.verbose && !o.quiet {
		_, _ = fmt.Fprintf(o.stderr, format+"\n", args...)
	}
}

// data writes a deliverable to stdout VERBATIM (no added newline). The bare-path
// contract (PLAN §8.1) depends on this never appending '\n'.
func (o out) data(s string) {
	_, _ = io.WriteString(o.stdout, s)
}

// dataln writes a deliverable to stdout WITH a trailing newline (used for the --json
// one-line objects and NDJSON, which carry a newline, PLAN §8.1/§8.4).
func (o out) dataln(s string) {
	_, _ = io.WriteString(o.stdout, s+"\n")
}

// emitJSON marshals v to a single compact line on stdout WITH a trailing newline
// (PLAN §8.2: the --json one-liner carries a newline, unlike the bare path).
func (o out) emitJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	o.dataln(string(b))
	return nil
}

// jsonError is the FROZEN error envelope every command emits to stdout under --json on
// failure (PLAN §8.2): {"schema":"clipbeam.v1","ok":false,"error":"<reason>","code":N}.
type jsonError struct {
	Schema string `json:"schema"`
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
	Code   int    `json:"code"`
}

// emitJSONError writes the §8.2 error envelope to stdout (data sink) and returns. The
// human message still goes to stderr in non-json mode via Execute's error handler;
// under --json the structured object is the deliverable.
func (o out) emitJSONError(reason string, code int) {
	_ = o.emitJSON(jsonError{Schema: schemaVersion, OK: false, Error: reason, Code: code})
}

// colorEnabled reports whether ANSI color is permitted for this run (PLAN §8.1):
// disabled under --json, --no-color, NO_COLOR (any non-empty value), or off-TTY.
// clipbeam emits no decorative color today, so this is the single authority a future
// colorized diagnostic would consult; it is exercised by the schema/help text gate.
func colorEnabled() bool {
	if gflags.json || gflags.noColor {
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return isTTY(os.Stderr)
}

// isTTY reports whether f is a character device (a terminal). It uses os.Stat's mode
// bits so it stays CGO-free and portable.
func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
