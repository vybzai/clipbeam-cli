# clipbeam (agent quickstart)

`clipbeam` beams files/screenshots/messages into a remote box over SSH/Tailscale and prints the saved absolute path. Wire: envelope-v1. CLI JSON envelope: clipbeam.v1.

Canonical, always-current surface: run `clipbeam schema` (or `clipbeam help --json`) — it emits every verb, flag, stdout shape, and exit code as one JSON document.

Key rules:
- stdout=data, stderr=diagnostics; `last`/`wait` emit a bare absolute path with NO trailing newline.
- Branch on the payload (`path:null`/`item:null`), not the exit code; a long-poll timeout is exit 0.
- Two channels: agent (FIFO, drained by `recv`) and clipboard (disk + `last_path`). `msg` is always agent; `send`/`shot` default to clipboard, `--agent` flips.
