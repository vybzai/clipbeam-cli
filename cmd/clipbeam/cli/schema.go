package cli

// schemaDoc is the runtime self-description (PLAN §8.5): one JSON document describing
// the entire surface so an agent can introspect at runtime. `clipbeam schema` and
// `clipbeam help --json` both emit it.
type schemaDoc struct {
	Schema       string          `json:"schema"`
	OK           bool            `json:"ok"`
	Binary       string          `json:"binary"`
	Version      string          `json:"version"`
	WireProtocol string          `json:"wireProtocol"`
	ExitCodes    []exitCodeDoc   `json:"exitCodes"`
	Env          []envVarDoc     `json:"env"`
	Commands     []commandDoc    `json:"commands"`
	Channels     channelsDoc     `json:"channels"`
}

// exitCodeDoc is one row of the unified exit-code table (PLAN §8.3).
type exitCodeDoc struct {
	Code       int    `json:"code"`
	Meaning    string `json:"meaning"`
	Provenance string `json:"provenance"`
}

// envVarDoc documents one environment variable the CLI honors (PLAN §8.1).
type envVarDoc struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
}

// commandDoc describes one verb (PLAN §8.5).
type commandDoc struct {
	Name       string   `json:"name"`
	Summary    string   `json:"summary"`
	Args       string   `json:"args"`
	Flags      []string `json:"flags"`
	Stdout     string   `json:"stdout"`
	ExitCodes  []int    `json:"exitCodes"`
	JSONSchema string   `json:"jsonSchema"`
}

// channelsDoc documents the two-channel model + the cid convention (PLAN §8.6/§8.7).
type channelsDoc struct {
	Note string `json:"note"`
	Cid  string `json:"cid"`
}

// buildSchema assembles the self-description document for the given version. It is the
// single source of truth for `schema`, `help --json`, AND the build-time-generated
// embedded SKILL.md (PLAN §8.8 — a CI drift check fails the build if the committed
// skill diverges from this output).
func buildSchema(version string) schemaDoc {
	return schemaDoc{
		Schema:       schemaVersion,
		OK:           true,
		Binary:       "clipbeam",
		Version:      version,
		WireProtocol: wireProtocol,
		ExitCodes:    exitCodeTable(),
		Env: []envVarDoc{
			{Name: "CLIPBEAM_JSON", Desc: "when set truthy, every command emits --json (watch emits NDJSON)"},
			{Name: "NO_COLOR", Desc: "any non-empty value disables ANSI color"},
			{Name: "CLIPBEAM_TOKEN", Desc: "shared token override for CI (never put a real token in argv on a shared box)"},
			{Name: "XDG_RUNTIME_DIR", Desc: "if absolute, the daemon socket is $XDG_RUNTIME_DIR/clipbeam/clipbeam.sock"},
		},
		Commands: commandTable(),
		Channels: channelsDoc{
			Note: "Two channels: 'agent' (in-memory FIFO inbox + agent-inbox files, drained by recv; never touches the human clipboard) and 'clipboard' (saved to disk + last_path, latest-wins). msg ALWAYS uses agent; send/shot default to clipboard and --agent flips; push uses clipboard. Branch on the PAYLOAD (item:null / path:null), not the exit code: a long-poll timeout is exit 0.",
			Cid:  "Opt-in, agent-channel-only correlation: msg --reply-to <uuid> prepends a leading [clipbeam:cid=<uuid>] token, parsed back into the 'cid' field by recv --json / watch --json. OFF by default; a Swift peer that never emits it round-trips as cid:null.",
		},
	}
}

// exitCodeTable returns the ONE deterministic exit-code table (PLAN §8.3), emitted in
// `schema` and surfaced in `--help`.
func exitCodeTable() []exitCodeDoc {
	return []exitCodeDoc{
		{Code: ExitOK, Meaning: "success (incl. a normal long-poll timeout on wait/recv/watch)", Provenance: "-"},
		{Code: ExitGeneric, Meaning: "generic / unexpected failure", Provenance: "local"},
		{Code: ExitUsage, Meaning: "usage / bad arguments", Provenance: "local"},
		{Code: ExitNothing, Meaning: "nothing available (reserved for a future --no-wait read)", Provenance: "local"},
		{Code: ExitUnauthorized, Meaning: "unauthorized", Provenance: "HTTP 401"},
		{Code: ExitForbidden, Meaning: "forbidden (source-class / peer)", Provenance: "HTTP 403"},
		{Code: ExitUnreachable, Meaning: "peer offline / transport unreachable / host-key unknown or changed", Provenance: "HTTP 502 (client-derived) OR SSH-dial fail OR host-key unknown/mismatch"},
		{Code: ExitTooLarge, Meaning: "payload too large", Provenance: "HTTP 413 OR local pre-flight"},
		{Code: ExitConfig, Meaning: "config / identity error (no peer, no token, bad config, CA-unsupported)", Provenance: "local"},
		{Code: ExitRemoteIO, Meaning: "remote/peer I/O error (e.g. disk full)", Provenance: "HTTP 500 (real errno relayed; client-derived)"},
	}
}

// commandTable describes every verb (PLAN §8.5). The jsonSchema strings are the §8.2
// per-command --json shapes, given as inline schema fragments an agent can pattern on.
func commandTable() []commandDoc {
	return []commandDoc{
		{
			Name: "send", Summary: "Beam a file to the box; prints the remote absolute path on stdout",
			Args: "<file> [target]", Flags: []string{"--agent"},
			Stdout:     "default: {\"ok\":true,\"sentItems\":N} then the remote abs path (no trailing newline); --json: sendResult",
			ExitCodes:  []int{0, 1, 2, 5, 6, 7, 8, 9},
			JSONSchema: `{"schema":"clipbeam.v1","ok":true,"sentItems":N,"remotePath":"<abs>|null"}`,
		},
		{
			Name: "shot", Summary: "Capture a screenshot, beam it to the box, print the remote path",
			Args: "[target]", Flags: []string{"--agent"},
			Stdout:     "same as send",
			ExitCodes:  []int{0, 1, 2, 5, 6, 7, 8, 9},
			JSONSchema: `{"schema":"clipbeam.v1","ok":true,"sentItems":N,"remotePath":"<abs>|null"}`,
		},
		{
			Name: "msg", Summary: "Beam a text message on the agent channel (ALWAYS agent)",
			Args: "<text> [target]", Flags: []string{"--reply-to <uuid>"},
			Stdout:     "same as send",
			ExitCodes:  []int{0, 1, 2, 5, 6, 8, 9},
			JSONSchema: `{"schema":"clipbeam.v1","ok":true,"sentItems":N,"remotePath":"<abs>|null"}`,
		},
		{
			Name: "push", Summary: "Beam the local clipboard contents to the box (clipboard channel)",
			Args: "[target]", Flags: nil,
			Stdout:     "same as send",
			ExitCodes:  []int{0, 1, 2, 5, 6, 7, 8, 9},
			JSONSchema: `{"schema":"clipbeam.v1","ok":true,"sentItems":N,"remotePath":"<abs>|null"}`,
		},
		{
			Name: "last", Summary: "Print the most-recently-received absolute path (no trailing newline)",
			Args: "", Flags: nil,
			Stdout:     "the bare absolute path with NO trailing newline; nothing-available prints nothing (exit 0)",
			ExitCodes:  []int{0, 1, 6},
			JSONSchema: `{"schema":"clipbeam.v1","ok":true,"path":"<abs>|null"}`,
		},
		{
			Name: "wait", Summary: "Block up to 120s for the next received path; print it (no trailing newline)",
			Args: "", Flags: nil,
			Stdout:     "the bare absolute path with NO trailing newline; timeout prints nothing (exit 0)",
			ExitCodes:  []int{0, 1, 6},
			JSONSchema: `{"schema":"clipbeam.v1","ok":true,"path":"<abs>|null"}`,
		},
		{
			Name: "recv", Summary: "Dequeue one agent-channel item (default 120s long-poll)",
			Args: "", Flags: []string{"--timeout N"},
			Stdout:     "default: labeled block (type:/sender:/path:/text: last, colon-SPACE, no trailing newline); timeout prints nothing (exit 0)",
			ExitCodes:  []int{0, 1, 6},
			JSONSchema: `{"schema":"clipbeam.v1","ok":true,"type":"image|file|text","sender":"<host>","path":<abs|null>,"text":<utf8|null>,"channel":"agent","cid":<uuid|null>}; on timeout {"schema":"clipbeam.v1","ok":true,"item":null}`,
		},
		{
			Name: "watch", Summary: "Stream received items as NDJSON until SIGINT (re-arms recv + wait)",
			Args: "", Flags: []string{"--channel agent|clipboard|both"},
			Stdout:     "NDJSON, one object per line, flushed per item; clipboard channel is latest-wins (coalescing); SIGINT exits 0",
			ExitCodes:  []int{0, 1, 2, 6},
			JSONSchema: `agent: {"schema":"clipbeam.v1","event":"item","channel":"agent","type":"…","sender":"…","path":<abs|null>,"text":<utf8|null>,"cid":<uuid|null>,"ts":"<RFC3339 UTC>"}; clipboard: {"schema":"clipbeam.v1","event":"item","channel":"clipboard","path":"<abs>","ts":"<RFC3339 UTC>"}`,
		},
		{
			Name: "health", Summary: "Check a peer's /health (liveness via ok==true; values are node-specific)",
			Args: "[target]", Flags: nil,
			Stdout:     "the /health JSON {ok,app,version,host,platform}; test ok==true, never the app/platform strings",
			ExitCodes:  []int{0, 1, 6, 8},
			JSONSchema: `{"ok":true,"app":"clipbeam","version":"<semver>","host":"<hostname>","platform":"<goos> <kernel>"}`,
		},
		{
			Name: "version", Summary: "Print the clipbeam version (use --json for the structured schema)",
			Args: "", Flags: nil,
			Stdout:     "default: the bare semver (with newline); --json: the structured version object",
			ExitCodes:  []int{0},
			JSONSchema: `{"schema":"clipbeam.v1","ok":true,"version":"<semver>","commit":"<vcs-or-dev>","wireProtocol":"envelope-v1"}`,
		},
		{
			Name: "schema", Summary: "Print the machine-readable self-description (verbs, flags, exit codes)",
			Args: "", Flags: nil,
			Stdout:     "this document (also via `help --json`)",
			ExitCodes:  []int{0},
			JSONSchema: `the full self-description document`,
		},
		{
			Name: "install-skill", Summary: "Install the clipbeam agent skill (SKILL.md/AGENTS.md) for Claude/Codex",
			Args: "", Flags: []string{"--target claude|codex|all", "--dir <path>", "--force", "--list-targets"},
			Stdout:     "each written path, one per line",
			ExitCodes:  []int{0, 1, 2, 8},
			JSONSchema: `{"schema":"clipbeam.v1","ok":true,"written":["<path>"]}`,
		},
		{
			Name: "serve", Summary: "Run the receiver daemon (unix socket by default; --listen tcp|tailscale)",
			Args: "", Flags: []string{"--listen socket|tcp|tailscale", "--set-clipboard"},
			Stdout:     "diagnostics to stderr; serves until SIGINT/SIGTERM",
			ExitCodes:  []int{0, 1, 8},
			JSONSchema: `n/a (long-running)`,
		},
		{
			Name: "ingest", Summary: "Read a CB01 frame from stdin, save it, print the saved abs path(s)",
			Args: "", Flags: nil,
			Stdout:     "the saved absolute path(s), one per line, no trailing newline on the last",
			ExitCodes:  []int{0, 1, 2, 7, 9},
			JSONSchema: `n/a (bare path lines)`,
		},
		{
			Name: "setup", Summary: "Bootstrap the box over SSH: stream the binary, pair, record the abs path",
			Args: "<user@host>", Flags: []string{"--serve exec|socket|tcp|tailscale", "--accept-new-hostkey", "--force", "--from-release"},
			Stdout:     "the recorded remote binary path on success",
			ExitCodes:  []int{0, 1, 2, 6, 8},
			JSONSchema: `{"schema":"clipbeam.v1","ok":true,"remoteBinPath":"<abs>","alias":"<name>"}`,
		},
		{
			Name: "doctor", Summary: "Diagnose config, token storage, permissions, and transport readiness",
			Args: "", Flags: nil,
			Stdout:     "diagnostics to stderr; --json: the structured findings (warns on a group/other-readable token file)",
			ExitCodes:  []int{0, 8},
			JSONSchema: `{"schema":"clipbeam.v1","ok":true,"config":"…","tokenStore":"…","tokenPresent":true,"platform":"…"}`,
		},
		{
			Name: "service", Summary: "Manage the optional systemd --user / launchd service unit (off by default)",
			Args: "<install|uninstall>", Flags: nil,
			Stdout:     "the written/removed unit path",
			ExitCodes:  []int{0, 1, 2, 8},
			JSONSchema: `{"schema":"clipbeam.v1","ok":true,"unit":"<path>"}`,
		},
	}
}

// runSchema implements `clipbeam schema` and `help --json` (PLAN §8.5): the full
// self-description to stdout. It is always JSON (the document IS the deliverable),
// regardless of the global --json flag.
func runSchema(o out) error {
	return o.emitJSON(buildSchema(cmdVersion))
}
