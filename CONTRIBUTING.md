# Contributing to clipbeam

Thanks for your interest in `clipbeam`. This is a small, focused tool with a
**frozen wire protocol**, so the contribution bar is mostly about *not breaking
interop* and keeping the security properties intact.

## Quick start

```sh
git clone https://github.com/vybzai/clipbeam-cli.git
cd clipbeam
go build ./...
go test ./...
```

Before opening a pull request, run the full local gate:

```sh
go build ./...
go vet ./...
go test -race ./...
golangci-lint run        # if installed
```

CI additionally runs `staticcheck`, a fuzz smoke pass, a cross-compile build matrix
(`linux/{amd64,arm64}`, `darwin/{amd64,arm64}`, `windows/amd64` build-only), the SSH
integration tests, the interop golden-fixture tests, and the banned-symbol grep gate.

## Never drift the wire

> **The Swift source and the golden interop fixtures are authoritative** (see
> `PROTOCOL.md`). When in doubt, the fixtures win.

- The Envelope v1 wire format, the HTTP header casings, and the `/health` key names
  are **frozen**. A change there is almost certainly a bug.
- **Run the interop tests** for any change that touches `internal/wire`,
  `internal/ingest`, `internal/httpd`, or the sender/receiver paths:

  ```sh
  go test ./internal/wire/... ./internal/httpd/... ./internal/ingest/...
  ```

- Interop assertions are **semantic (decode-then-compare)**, never raw-byte
  comparisons (Swift's encoder does not sort keys). Do not add a raw JSON
  string-compare.
- The `CB01` SSH frame is internal and is **not** Envelope v1 — keep it out of the
  interop fixtures.

## Banned symbols (CI grep gate)

The build fails if any of these appear:

- `ssh.InsecureIgnoreHostKey`
- any `HostKeyCallback` not backed by `knownhosts`
- any `--insecure` flag
- `math/rand` for token or secret generation (use `crypto/rand`)

## Code discipline

- **stdout = data, stderr = diagnostics.** The bare `last` / `wait` path has **no
  trailing newline.**
- Idiomatic Go; exported identifiers carry godoc comments.
- Prefer the standard library; do not add a third-party dependency without strong
  cause and discussion.
- No hardcoded secrets or IPs — read configuration.

## Developer Certificate of Origin (DCO)

There is **no CLA**. Instead, every commit must be **signed off** under the
[Developer Certificate of Origin](https://developercertificate.org/). Add a
`Signed-off-by` trailer to each commit:

```sh
git commit -s -m "your message"
```

This produces:

```
Signed-off-by: Your Name <your.email@example.com>
```

By signing off you certify that you wrote the patch (or otherwise have the right to
submit it under the project's MIT license).

## Pull requests

- Keep PRs focused and include tests for behavior changes.
- The PR template includes an **interop-test checkbox** — confirm you ran it for any
  wire-adjacent change.
- Be kind. See `CODE_OF_CONDUCT.md`.
