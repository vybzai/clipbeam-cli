// Command clipbeam is the CLI-first, cross-platform beam tool: it beams files,
// screenshots, clipboard content, and agent messages from a laptop into a headless
// Linux box you are already SSH'd into, landing each payload on the box's disk and
// surfacing its absolute path so a remote coding agent can Read it. It speaks
// ClipBeam's frozen Envelope v1 protocol byte-for-behavior (PLAN §1, §3).
package main

import (
	"os"

	"github.com/vybzai/clipbeam-cli/cmd/clipbeam/cli"
)

// version is injected at build time via -ldflags -X main.version (PLAN §10.8); it
// falls back to the VCS build-info stamp / "dev" for `go install` and source builds.
var version = "dev"

func main() {
	os.Exit(cli.Execute(version))
}
