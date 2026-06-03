<!-- clipbeam pull request (PLAN §10.6: the PR template references the interop-test checkbox). -->

## What & why

<!-- One or two sentences. Link any issue with `Fixes #N`. -->

## Checklist

- [ ] `go build ./...` and `go vet ./...` pass
- [ ] `go test -race ./...` passes locally
- [ ] `golangci-lint run` is clean
- [ ] **Interop tests pass** — I did NOT drift the wire (`go test ./internal/wire/... ./internal/httpd/...` golden fixtures still green). The Envelope v1 wire, HTTP header casings (`X-ClipBeam-*`), and `/health` key names are FROZEN (PROTOCOL.md, PLAN §10.2).
- [ ] If I touched the CLI surface: the install-skill drift check passes (`go test ./cmd/clipbeam/cli -run TestSkillDoesNotDrift`).
- [ ] No banned symbols introduced (`ssh.InsecureIgnoreHostKey`, a non-knownhosts `HostKeyCallback`, an `--insecure` flag, `math/rand` for tokens).
- [ ] stdout = data, stderr = diagnostics; `last`/`wait` bare path has NO trailing newline.
- [ ] Commits carry a DCO `Signed-off-by` line (`git commit -s`) — see CONTRIBUTING.md.

## Notes for reviewers

<!-- Anything risky, anything you want a second pass on. -->
