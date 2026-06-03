package cli

import (
	"fmt"
	"strings"
)

// generateSkillMarkdown renders the maintained SKILL.md from the schema document
// (PLAN §8.8): the embedded skill is GENERATED at build time from `clipbeam schema`,
// and a CI drift check fails the build if the committed file diverges from this
// output — so the dropped skill can never lie. It is a Linux/SSH-aware rewrite of the
// original macOS/Tailscale SKILL.md: it drops the two-Mac/Tailscale specifics but keeps
// the two-channel model, the recv labeled-block format, and the "read the bare absolute
// path" recipe.
//
// The output is DETERMINISTIC for a fixed schema (it must be, for the drift check). To
// keep it version-independent, the version field is NOT embedded in the body (a version
// bump must not force a skill rewrite); the live `clipbeam schema` carries the version.
func generateSkillMarkdown(doc schemaDoc) string {
	var b strings.Builder

	// Front matter (the skill description an agent's loader reads).
	b.WriteString("---\n")
	b.WriteString("name: clipbeam\n")
	b.WriteString("description: Beam a file, screenshot, or text message from your laptop into a headless Linux box you are SSH'd into (landing it on the box's disk and printing its absolute path), and receive items beamed to you. Use when an agent needs to hand a file/screenshot/message to the agent on a remote box, put something on the box's clipboard, or pick up what was sent. CLI: clipbeam send | shot | msg | push | recv | last | wait | watch | health.\n")
	b.WriteString("---\n\n")

	b.WriteString("# clipbeam — beam files, screenshots, and messages into a remote box\n\n")
	b.WriteString("`clipbeam` beams files, screenshots, clipboard content, and agent messages over **SSH** (default) or **Tailscale** into a headless box you are already SSH'd into. Each payload lands on the box's disk and the CLI prints its **absolute path** so a remote coding agent can Read it. No cloud, no account; the wire is ClipBeam's frozen Envelope v1.\n\n")
	b.WriteString("Run the SEND verbs on your laptop (they drive the remote box over SSH). Run the RECEIVE verbs on the box itself (they read what was beamed to it via the local `clipbeam serve` daemon).\n\n")

	// Two channels.
	b.WriteString("## Two channels (important)\n")
	b.WriteString(doc.Channels.Note + "\n\n")
	b.WriteString("- **agent channel** — lands ONLY in the box's agent-inbox (files, mode 600) or an in-memory FIFO (text). It does NOT touch the human's clipboard. Drained with `clipbeam recv`. This is the channel for agent↔agent comms.\n")
	b.WriteString("- **clipboard channel** (default for `send`/`shot`/`push`) — saved to the box's save dir and recorded as `last_path`; `clipbeam last`/`wait` surface it.\n\n")

	// Commands table.
	b.WriteString("## Commands\n\n")
	b.WriteString("| Command | Args | Effect |\n")
	b.WriteString("|---------|------|--------|\n")
	for _, c := range doc.Commands {
		args := c.Args
		if args == "" {
			args = "-"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", c.Name, args, escapePipes(c.Summary))
	}
	b.WriteString("\n")

	// recv format.
	b.WriteString("## `clipbeam recv` output format (default mode)\n")
	b.WriteString("```\n")
	b.WriteString("type: image|file|text\n")
	b.WriteString("sender: <hostname>\n")
	b.WriteString("path: <absolute path>     # present for image/file — READ THE FILE AT THIS PATH\n")
	b.WriteString("text: <the message>       # present for text — LAST; may span multiple lines\n")
	b.WriteString("```\n")
	b.WriteString("Labels use a literal colon-SPACE separator; split on the FIRST colon and drop the single leading space. `text:` is always last and consumes the rest verbatim (embedded newlines preserved). For a file/image item, read the file at `path:`; for text, use the `text:` value. There is NO trailing newline.\n\n")

	// Agent-first guarantees.
	b.WriteString("## Agent-first guarantees\n")
	b.WriteString("- Every command takes `--json` for a stable, versioned object (`\"schema\":\"" + doc.Schema + "\"`). `CLIPBEAM_JSON=1` flips all commands to JSON (and `watch` to NDJSON).\n")
	b.WriteString("- **stdout = data, stderr = diagnostics.** `last`/`wait` print the bare absolute path with NO trailing newline; `recv` prints the labeled block. A coding agent reads a bare absolute path in the prompt (NOT `@path`).\n")
	b.WriteString("- Branch on the PAYLOAD, not the exit code: a long-poll timeout on `wait`/`recv`/`watch` is exit 0 with `path:null` / `item:null`.\n")
	b.WriteString("- `clipbeam schema` (or `help --json`) prints this entire surface as one machine-readable document at runtime.\n\n")
	b.WriteString("### cid convention\n")
	b.WriteString(doc.Channels.Cid + "\n\n")

	// Exit codes.
	b.WriteString("## Exit codes\n\n")
	b.WriteString("| Code | Meaning | Provenance |\n")
	b.WriteString("|------|---------|------------|\n")
	for _, e := range doc.ExitCodes {
		fmt.Fprintf(&b, "| %d | %s | %s |\n", e.Code, escapePipes(e.Meaning), escapePipes(e.Provenance))
	}
	b.WriteString("\n")

	// Recipe.
	b.WriteString("## Recipe\n")
	b.WriteString("```sh\n")
	b.WriteString("# On your laptop — beam to the box (clipboard channel by default):\n")
	b.WriteString("clipbeam send /tmp/build.log user@box      # → prints the remote abs path on the box\n")
	b.WriteString("clipbeam msg \"build is green\" user@box      # → agent channel (always)\n")
	b.WriteString("clipbeam send diff.patch --agent user@box   # → agent inbox on the box\n\n")
	b.WriteString("# On the box (a remote agent) — pick up what was sent:\n")
	b.WriteString("clipbeam recv          # → labeled block; dequeues ONE agent item (FIFO), call again to drain\n")
	b.WriteString("clipbeam last          # → the bare absolute path of the last clipboard-channel file\n")
	b.WriteString("clipbeam wait          # → block up to 120s for the next clipboard path\n")
	b.WriteString("clipbeam watch --json  # → NDJSON stream of every item until Ctrl-C\n")
	b.WriteString("```\n\n")

	// Setup + transport.
	b.WriteString("## Setup & transport\n")
	b.WriteString("- `clipbeam setup user@host` bootstraps the box over SSH: streams the matching static binary, records its absolute path, generates + pairs a 120-bit token (token required even over SSH). The default remote serve mode is daemonless-exec (no running process, no open port). `--serve socket` runs a 0600 unix-socket daemon for `recv`/`wait`/`watch`.\n")
	b.WriteString("- Transport reuses your SSH auth (ssh-agent + `~/.ssh/config`); host keys use strict knownhosts (TOFU only under `setup`/`--accept-new-hostkey`, never on a data verb). Tailscale is the always-on alternative.\n")
	b.WriteString("- A failed send surfaces the real reason (e.g. `peer HTTP 500: … No space left on device`), not an opaque code.\n\n")

	b.WriteString("## Environment variables\n")
	for _, e := range doc.Env {
		fmt.Fprintf(&b, "- `%s` — %s\n", e.Name, e.Desc)
	}
	b.WriteString("\n")

	return b.String()
}

// generateAgentsMarkdown renders the companion AGENTS.md (PLAN §8.8): a terse,
// machine-readable pointer to `clipbeam schema` as the canonical runtime surface.
func generateAgentsMarkdown(doc schemaDoc) string {
	var b strings.Builder
	b.WriteString("# clipbeam (agent quickstart)\n\n")
	b.WriteString("`clipbeam` beams files/screenshots/messages into a remote box over SSH/Tailscale and prints the saved absolute path. Wire: " + doc.WireProtocol + ". CLI JSON envelope: " + doc.Schema + ".\n\n")
	b.WriteString("Canonical, always-current surface: run `clipbeam schema` (or `clipbeam help --json`) — it emits every verb, flag, stdout shape, and exit code as one JSON document.\n\n")
	b.WriteString("Key rules:\n")
	b.WriteString("- stdout=data, stderr=diagnostics; `last`/`wait` emit a bare absolute path with NO trailing newline.\n")
	b.WriteString("- Branch on the payload (`path:null`/`item:null`), not the exit code; a long-poll timeout is exit 0.\n")
	b.WriteString("- Two channels: agent (FIFO, drained by `recv`) and clipboard (disk + `last_path`). `msg` is always agent; `send`/`shot` default to clipboard, `--agent` flips.\n")
	return b.String()
}

// escapePipes escapes a literal '|' so a value embedded in a Markdown table cell does
// not break the column (the deterministic-output guarantee for the drift check).
func escapePipes(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}
