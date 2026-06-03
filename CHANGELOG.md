# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The wire protocol version (`Envelope.version`) is the integer `1` and is
decoupled from the CLI's SemVer — it is not bumped to match a release.

## [0.1.1] - 2026-06-03

### Fixed
- **Installer**: the one-line `curl | sh` now resolves real release assets. Two
  bugs are fixed — the repo slug (was a leftover `clipbeam/clipbeam` placeholder
  → now `vybzai/clipbeam-cli`) and the archive name (the release TAG carries a
  leading `v`, but GoReleaser strips it from `{{.Version}}`, so the asset is
  `clipbeam_0.1.0_<os>_<arch>.tar.gz`). Verified end-to-end on a real Linux box.
- **SSH auth**: an empty-but-present `ssh-agent` (`$SSH_AUTH_SOCK` set, 0
  identities) no longer shadows the on-disk key — agent and file signers are
  merged into a single publickey method, matching OpenSSH's fallback.
- **`send`/`msg` over SSH**: the SSH-exec path now invokes the alias's recorded
  absolute `remoteBinPath` (and resolves an alias by host as well as name),
  fixing `clipbeam: command not found` immediately after `setup`.
- **`last`/`wait`**: fall back to the on-disk `last_path` when no daemon is
  running, so `$(clipbeam last)` works after a daemonless `setup` (a reachable
  daemon still takes precedence).
- **Agent channel over SSH**: `recv` now drains SSH-delivered text and file items
  exactly once from a disk-backed FIFO journal. The journal is written only by
  the one-shot `ingest` path; the `serve` daemon stays in-memory-only (no
  unbounded growth, no plaintext retention, no double delivery).

### Added
- `clipbeam setup --from-release`: bootstrap a box by having it run the published
  installer over SSH (curl/wget), plus a tarball download-and-stream install path
  so a released binary with no Go toolchain can set up a remote box.

### Docs
- README leads with the agent-first value; removed dead links; dropped the
  phantom `clipbeam self-update` reference.

## [0.1.0] - 2026-06-03

### Added
- Initial implementation of the `clipbeam` CLI.
- Frozen **Envelope v1** wire protocol, re-implemented in Go and validated
  against golden fixtures captured from the macOS `ClipBeam.app`
  (`testdata/interop/`), asserted in both directions.
- Receive/control surface over `net/http`: `POST /clip`, `GET /health`, and the
  loopback control endpoints `POST /push`, `POST /agent-send`, `GET /recv`,
  `GET /last`, `GET /wait` — with constant-time token auth, source-class gating,
  the `maxBytes*4/3 + 64KB` raw ceiling, single-flight `/clip`, and graceful 413
  delivery.
- Two-channel model: a human **clipboard channel** (disk + `last_path` + recents)
  and a private **agent channel** (FIFO inbox, drained by `recv`).
- Transports: **Tier-0 SSH** (daemonless exec + in-process tunnel via
  `golang.org/x/crypto/ssh`, `ssh_config` resolution, strict `known_hosts` with
  non-interactive accept-new TOFU under `setup`) and **Tier-1 Tailscale**
  (exact-peer-IP + token). Tier-2 public+TLS is designed but deferred.
- Agent-first CLI: `--json` on every command, a deterministic exit-code table,
  `clipbeam watch` (NDJSON), `clipbeam schema` (runtime self-description), and
  `clipbeam install-skill`.
- Cross-platform receiver: Linux (XDG paths, headless disk+path), macOS
  (`~/.clipbeam` parity, Keychain token).
- Distribution: `install.sh`, GoReleaser config (multi-arch archives, checksums,
  SBOM, Homebrew cask, cosign keyless signing), and CI.

[0.1.1]: https://github.com/vybzai/clipbeam-cli/releases/tag/v0.1.1
[0.1.0]: https://github.com/vybzai/clipbeam-cli/releases/tag/v0.1.0
