---
name: clipbeam
description: Beam a file, screenshot, or text message from your laptop into a headless Linux box you are SSH'd into (landing it on the box's disk and printing its absolute path), and receive items beamed to you. Use when an agent needs to hand a file/screenshot/message to the agent on a remote box, put something on the box's clipboard, or pick up what was sent. CLI: clipbeam send | shot | msg | push | recv | last | wait | watch | health.
---

# clipbeam — beam files, screenshots, and messages into a remote box

`clipbeam` beams files, screenshots, clipboard content, and agent messages over **SSH** (default) or **Tailscale** into a headless box you are already SSH'd into. Each payload lands on the box's disk and the CLI prints its **absolute path** so a remote coding agent can Read it. No cloud, no account; the wire is ClipBeam's frozen Envelope v1.

Run the SEND verbs on your laptop (they drive the remote box over SSH). Run the RECEIVE verbs on the box itself (they read what was beamed to it via the local `clipbeam serve` daemon).

## Two channels (important)
Two channels: 'agent' (in-memory FIFO inbox + agent-inbox files, drained by recv; never touches the human clipboard) and 'clipboard' (saved to disk + last_path, latest-wins). msg ALWAYS uses agent; send/shot default to clipboard and --agent flips; push uses clipboard. Branch on the PAYLOAD (item:null / path:null), not the exit code: a long-poll timeout is exit 0.

- **agent channel** — lands ONLY in the box's agent-inbox (files, mode 600) or an in-memory FIFO (text). It does NOT touch the human's clipboard. Drained with `clipbeam recv`. This is the channel for agent↔agent comms.
- **clipboard channel** (default for `send`/`shot`/`push`) — saved to the box's save dir and recorded as `last_path`; `clipbeam last`/`wait` surface it.

## Commands

| Command | Args | Effect |
|---------|------|--------|
| `send` | <file> [target] | Beam a file to the box; prints the remote absolute path on stdout |
| `shot` | [target] | Capture a screenshot, beam it to the box, print the remote path |
| `msg` | <text> [target] | Beam a text message on the agent channel (ALWAYS agent) |
| `push` | [target] | Beam the local clipboard contents to the box (clipboard channel) |
| `last` | - | Print the most-recently-received absolute path (no trailing newline) |
| `wait` | - | Block up to 120s for the next received path; print it (no trailing newline) |
| `recv` | - | Dequeue one agent-channel item (default 120s long-poll) |
| `watch` | - | Stream received items as NDJSON until SIGINT (re-arms recv + wait) |
| `health` | [target] | Check a peer's /health (liveness via ok==true; values are node-specific) |
| `version` | - | Print the clipbeam version (use --json for the structured schema) |
| `schema` | - | Print the machine-readable self-description (verbs, flags, exit codes) |
| `install-skill` | - | Install the clipbeam agent skill (SKILL.md/AGENTS.md) for Claude/Codex |
| `serve` | - | Run the receiver daemon (unix socket by default; --listen tcp\|tailscale) |
| `ingest` | - | Read a CB01 frame from stdin, save it, print the saved abs path(s) |
| `setup` | <user@host> | Bootstrap the box over SSH: stream the binary, pair, record the abs path |
| `doctor` | - | Diagnose config, token storage, permissions, and transport readiness |
| `service` | <install|uninstall> | Manage the optional systemd --user / launchd service unit (off by default) |

## `clipbeam recv` output format (default mode)
```
type: image|file|text
sender: <hostname>
path: <absolute path>     # present for image/file — READ THE FILE AT THIS PATH
text: <the message>       # present for text — LAST; may span multiple lines
```
Labels use a literal colon-SPACE separator; split on the FIRST colon and drop the single leading space. `text:` is always last and consumes the rest verbatim (embedded newlines preserved). For a file/image item, read the file at `path:`; for text, use the `text:` value. There is NO trailing newline.

## Agent-first guarantees
- Every command takes `--json` for a stable, versioned object (`"schema":"clipbeam.v1"`). `CLIPBEAM_JSON=1` flips all commands to JSON (and `watch` to NDJSON).
- **stdout = data, stderr = diagnostics.** `last`/`wait` print the bare absolute path with NO trailing newline; `recv` prints the labeled block. A coding agent reads a bare absolute path in the prompt (NOT `@path`).
- Branch on the PAYLOAD, not the exit code: a long-poll timeout on `wait`/`recv`/`watch` is exit 0 with `path:null` / `item:null`.
- `clipbeam schema` (or `help --json`) prints this entire surface as one machine-readable document at runtime.

### cid convention
Opt-in, agent-channel-only correlation: msg --reply-to <uuid> prepends a leading [clipbeam:cid=<uuid>] token, parsed back into the 'cid' field by recv --json / watch --json. OFF by default; a Swift peer that never emits it round-trips as cid:null.

## Exit codes

| Code | Meaning | Provenance |
|------|---------|------------|
| 0 | success (incl. a normal long-poll timeout on wait/recv/watch) | - |
| 1 | generic / unexpected failure | local |
| 2 | usage / bad arguments | local |
| 3 | nothing available (reserved for a future --no-wait read) | local |
| 4 | unauthorized | HTTP 401 |
| 5 | forbidden (source-class / peer) | HTTP 403 |
| 6 | peer offline / transport unreachable / host-key unknown or changed | HTTP 502 (client-derived) OR SSH-dial fail OR host-key unknown/mismatch |
| 7 | payload too large | HTTP 413 OR local pre-flight |
| 8 | config / identity error (no peer, no token, bad config, CA-unsupported) | local |
| 9 | remote/peer I/O error (e.g. disk full) | HTTP 500 (real errno relayed; client-derived) |

## Recipe
```sh
# On your laptop — beam to the box (clipboard channel by default):
clipbeam send /tmp/build.log user@box      # → prints the remote abs path on the box
clipbeam msg "build is green" user@box      # → agent channel (always)
clipbeam send diff.patch --agent user@box   # → agent inbox on the box

# On the box (a remote agent) — pick up what was sent:
clipbeam recv          # → labeled block; dequeues ONE agent item (FIFO), call again to drain
clipbeam last          # → the bare absolute path of the last clipboard-channel file
clipbeam wait          # → block up to 120s for the next clipboard path
clipbeam watch --json  # → NDJSON stream of every item until Ctrl-C
```

## Setup & transport
- `clipbeam setup user@host` bootstraps the box over SSH: streams the matching static binary, records its absolute path, generates + pairs a 120-bit token (token required even over SSH). The default remote serve mode is daemonless-exec (no running process, no open port). `--serve socket` runs a 0600 unix-socket daemon for `recv`/`wait`/`watch`.
- Transport reuses your SSH auth (ssh-agent + `~/.ssh/config`); host keys use strict knownhosts (TOFU only under `setup`/`--accept-new-hostkey`, never on a data verb). Tailscale is the always-on alternative.
- A failed send surfaces the real reason (e.g. `peer HTTP 500: … No space left on device`), not an opaque code.

## Environment variables
- `CLIPBEAM_JSON` — when set truthy, every command emits --json (watch emits NDJSON)
- `NO_COLOR` — any non-empty value disables ANSI color
- `CLIPBEAM_TOKEN` — shared token override for CI (never put a real token in argv on a shared box)
- `XDG_RUNTIME_DIR` — if absolute, the daemon socket is $XDG_RUNTIME_DIR/clipbeam/clipbeam.sock

