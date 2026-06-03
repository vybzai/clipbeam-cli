# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The wire protocol version (`Envelope.version`) is the integer `1` and is
decoupled from the CLI's SemVer — it is not bumped to match a release.

## [Unreleased]

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

[Unreleased]: https://github.com/vybzai/clipbeam-cli/commits/main
