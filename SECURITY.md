# Security Policy

`clipbeam` is a network-facing tool that moves files between machines. We take its
security seriously and welcome coordinated disclosure.

## Reporting a vulnerability

Report it privately via a **GitHub Security Advisory** —
<https://github.com/vybzai/clipbeam-cli/security/advisories/new> ("Report a
vulnerability"). Please do **not** open a public issue for a security problem.

- We aim to acknowledge a report within a few business days.
- We follow a **90-day coordinated-disclosure window**: we will work with you to
  understand and fix the issue, and we ask that you keep it private until a fix ships
  or 90 days elapse, whichever comes first.
- Please include a reproduction, the affected version (`clipbeam version`), and the
  transport tier and OS involved.

## Supported versions

Security fixes target the **latest released minor version**. Older versions are
patched only at the maintainers' discretion. Always run the latest release
(re-run the installer, or `brew upgrade` once the Homebrew tap is live).

## Threat model (per transport tier)

ClipBeam's original Mac-to-Mac design leaned on **WireGuard (Tailscale)** for
confidentiality, integrity, and source-IP authentication. The CLI runs in
environments without those guarantees, so the load-bearing controls shift onto **(a)
the transport (SSH/TLS)** and **(b) a constant-time shared token**.

| Tier | Transport guarantee | Primary attacker | Mitigation |
|---|---|---|---|
| **Tier-0 SSH, daemonless exec** (default) | OpenSSH: confidentiality + integrity + client/host auth | remote network attacker; on-wire MITM | **No listening port** — nothing to attack remotely. MITM blocked by strict `known_hosts` verification. The SSH session *is* the auth. |
| **Tier-0 SSH, tunneled daemon** | SSH tunnel encryption + auth | a hostile co-tenant (same box, different uid) | Default bind is a **0600 unix-domain socket** (kernel-enforced owner-only). If loopback TCP is opted into (`--listen tcp`), the **token is REQUIRED** on every control endpoint. |
| **Tier-1 Tailscale** | WireGuard | an authorized-but-untrusted tailnet member | Exact-peer-IP class **AND** constant-time token; range membership is logged only. Byte-identical to the shipped Mac app. |
| **Tier-2 public IP** | none until TLS | the whole internet | **Deferred for v1.** If ever shipped: mandatory TLS + per-source token-bucket rate limiting + a strong token. A plain-TCP public bind is forbidden. |

Key insight: **on a multi-user VPS, `127.0.0.1:PORT` is not private.** A blind port
of a Mac's token-free loopback control surface would be a regression. We close that
by preferring the daemonless-exec path (no port at all) and, when a daemon is
needed, binding a **0600 unix socket** rather than loopback TCP. The token stays
required on any TCP bind. `--listen tcp` on a multi-user box reopens this risk and is
loudly documented at the flag.

## Host-key trust (TOFU, explicit-flag)

`clipbeam` uses in-process SSH with `golang.org/x/crypto/ssh`. Host keys are verified
against `~/.ssh/known_hosts` and `/etc/ssh/ssh_known_hosts`.

- `ssh.InsecureIgnoreHostKey` is **banned**. There is **no `--insecure` flag.** A CI
  grep gate fails the build on either symbol or on any `HostKeyCallback` not backed
  by `known_hosts`.
- A genuinely **unknown** host key is recorded only under `clipbeam setup` (or the
  one-shot `--accept-new-hostkey`). Under any data verb (`send`/`shot`/`msg`/
  `push`/`recv`) an unknown host **fails closed** (exit 6) and never prompts.
- A **changed** host key **always hard-fails** (exit 6), never auto-accepts.

## Token storage

- **macOS** → login Keychain (`com.sani.clipbeam` / `shared-token`,
  `AfterFirstUnlockThisDeviceOnly`, non-synced). This item is **shared with
  `ClipBeam.app`** by design (one token for the pair).
- **Linux desktop** → Secret Service / libsecret, only if the keyring is unlocked;
  never blocks on a locked keyring.
- **Headless Linux VPS** (the common case) → a **0600 token file** under
  `$XDG_CONFIG_HOME/clipbeam/token` (parent 0700), re-chmod'd 0600 on every load.
  This is a **genuinely weaker** guarantee than the macOS Keychain: on a VPS, the
  box's own disk security **is** the boundary. `clipbeam doctor` warns loudly if the
  file is group/other-readable.

The token is **never** passed in argv (it would leak via `ps` / `/proc`): on the
wire it is the `X-ClipBeam-Token` header; on the SSH-exec path the remote reads its
own local token. `CLIPBEAM_TOKEN` is honored for CI only, with a doctor warning that
env vars leak via `/proc/<pid>/environ`.

## Logging

The daemon writes a **redacted**, size-rotated, **0600** log. Every write funnels
through a redactor that strips any `X-ClipBeam-*` value and any `token=` / `Bearer `
run to `<redacted>`. The per-accepted-ingest audit line uses only the validated
transport identity (unix-socket owner uid / SSH client identity / validated socket
IP) — never the attacker-controllable `X-ClipBeam-Sender`.

## Non-goals (accepted residual risk for v1)

These are **deliberate** v1 limitations, documented honestly:

- **No Tier-2 (raw public-IP) bind.** Deferred entirely; requires mandatory TLS.
- **No replay nonce.** Tier-0 SSH and Tier-2 TLS block on-path replay at the
  transport layer, but a credentialed Tier-1 insider could application-replay a
  captured `/clip` body. Unmitigated and documented.
- **No per-source rate limit.** Single-flight bounds *concurrency* to 1, not the
  total request *rate*. The only shipped surfaces are an authenticated SSH session or
  a 0600 unix socket.
- **No `tailscale whois` peer binding** — exact-IP + token suffices on Tier-1.
- **No ProxyJump / Include / `@cert-authority` host-cert support.** These fail with a
  specific error, never an opaque handshake failure and never a silent system-`ssh`
  fallback (which would bypass the in-process MITM guarantee).
