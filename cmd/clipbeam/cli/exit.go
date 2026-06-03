// Package cli is the cobra command layer (PLAN §8): the root command, the single
// deterministic exit-code table, global output flags, and a stub subcommand for
// every verb. Downstream milestones (M2/M3) implement the verb bodies against the
// internal packages; this layer defines the surface they fill in.
package cli

// Exit codes — the ONE deterministic table (PLAN §8.3), merged from HTTP-derived,
// SSH-derived, and local sources. Emitted in `clipbeam schema` and `--help`.
const (
	// ExitOK is success, including a normal long-poll timeout on wait/recv/watch.
	ExitOK = 0
	// ExitGeneric is a generic / unexpected failure (local).
	ExitGeneric = 1
	// ExitUsage is usage / bad arguments (local; matches the shim's exit 2).
	ExitUsage = 2
	// ExitNothing is nothing available (reserved for a future --no-wait read).
	ExitNothing = 3
	// ExitUnauthorized maps HTTP 401.
	ExitUnauthorized = 4
	// ExitForbidden maps HTTP 403 (source-class / peer).
	ExitForbidden = 5
	// ExitUnreachable is peer offline / transport unreachable / host-key unknown or
	// changed: HTTP 502 (client-derived) OR SSH-dial fail OR host-key unknown/mismatch.
	ExitUnreachable = 6
	// ExitTooLarge is payload too large: HTTP 413 OR local pre-flight.
	ExitTooLarge = 7
	// ExitConfig is a config / identity error (no peer, no token, bad config,
	// CA-unsupported) — local.
	ExitConfig = 8
	// ExitRemoteIO is a remote/peer I/O error (e.g. disk full): HTTP 500 with the
	// real errno relayed (client-derived).
	ExitRemoteIO = 9
)
