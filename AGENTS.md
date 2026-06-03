# AGENTS.md — machine-readable guide for coding agents

This file orients an AI agent working in this repository. Humans: see
[README.md](README.md).

## What this is

`clipbeam` is a single static Go CLI that beams files, screenshots, clipboard
content, and agent messages from a laptop into a headless box you are SSH'd into,
landing each payload on the box's disk and printing its absolute path. It speaks
the frozen **Envelope v1** wire protocol byte-for-behavior (see `PROTOCOL.md`).

## Build / test / lint

```sh
CGO_ENABLED=0 go build ./...      # build (static; no cgo)
go vet ./...                      # vet
staticcheck ./...                 # lint (honnef.co/go/tools)
go test ./...                     # tests
go test -race ./...               # race detector (required before commit)
go test -run Interop ./...        # the wire interop gate (golden fixtures)
```

Requires Go 1.26+. The interop fixtures in `testdata/interop/` were captured from
the real macOS `ClipBeam.app` and are authoritative for the wire alongside the
Swift source — never hand-edit them to make a test pass; fix the code.

## Layout

```
cmd/clipbeam/         CLI entry + cobra command tree (cli/)
internal/wire/        Envelope, Item, ClipResponse; ceiling math + decoded length
internal/classify/    source-IP classification (exact-peer / loopback / tailnet)
internal/auth/        constant-time token compare + CSPRNG token
internal/config/      per-OS paths, config, token storage, alias store
internal/ingest/      the single Ingest() save/dispatch path
internal/sanitize/    traversal-proof filename sanitizer
internal/store/       ReceiveStore: clipboard disk/last_path/recents + agent FIFO
internal/httpd/       net/http server, routes, gating, streaming decode
internal/sshx/        x/crypto/ssh client, ssh_config, knownhosts, CB01 framing
internal/tailscale/   `tailscale status --json` discovery
testdata/interop/     golden Envelope-v1 fixtures from ClipBeam.app
```

## Self-description (the entry point for agents)

Run **`clipbeam schema`** for the complete machine-readable surface: every
command, its flags, its `--json` stdout shape, and the exit-code table. Every
command also accepts `--json` for a stable, versioned (`clipbeam.v1`) object on
stdout; `CLIPBEAM_JSON=1` flips all commands to JSON. Data goes to stdout,
diagnostics to stderr; `last`/`wait` print a bare path with no trailing newline.

## Invariants (do not break)

- The wire `Envelope.version` is the integer `1`, forever, decoupled from CLI
  SemVer. Interop tests must stay green in both directions.
- Host-key verification is strict: `ssh.InsecureIgnoreHostKey` is banned, there is
  no `--insecure` flag, and tokens use `crypto/rand` (never `math/rand`). CI greps
  for these.
- Filenames are sanitized and asserted to stay inside the save dir.
