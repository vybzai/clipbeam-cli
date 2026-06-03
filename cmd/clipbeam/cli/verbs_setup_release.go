package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/vybzai/clipbeam-cli/internal/sshx"
)

// rawInstallURL is the canonical install.sh served at the repo apex (install.sh:7/65).
// --from-release has the REMOTE box fetch this and pipe it to sh, pinning the version so
// it installs the SAME release as the local CLI (install.sh:174 honors CLIPBEAM_VERSION).
const rawInstallURL = "https://raw.githubusercontent.com/vybzai/clipbeam-cli/main/install.sh"

// remoteInstaller is the swappable INSTALL step of the setup spine (fix [C]): the dial →
// detect → idempotency → install → token → recordAlias spine is shared; only install()
// differs. install() must place the matching binary at remoteBin (chmod 755) so the
// recorded absolute path is invocable under a minimal non-login SSH-exec PATH (PLAN §5.1).
type remoteInstaller interface {
	install(o out, client sshx.Client, t sshx.Target, goos, goarch, remoteBin string) error
}

// streamInstaller is the DEFAULT no-remote-egress route (PLAN §9.5 source i): it builds
// the matching static binary locally (cross-compile) and streams those bytes over the
// SAME SSH connection. The remote box needs no internet, no curl, no Go toolchain.
type streamInstaller struct{}

func (streamInstaller) install(o out, client sshx.Client, t sshx.Target, goos, goarch, remoteBin string) error {
	bin, err := buildCrossBinary(o, goos, goarch)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(bin) }()
	return streamBinary(o, client, t, bin, remoteBin)
}

// releaseInstaller is the OPT-IN remote-egress route (--from-release, PLAN §9.5 source
// iii): the REMOTE box self-installs by fetching the published install.sh over its OWN
// internet and piping it to sh, with the version pinned to the local CLI so the install
// matches (and the idempotency check stays consistent). This INVERTS the default
// no-remote-egress promise and is therefore behind the flag. install.sh installs to
// ~/.local/bin, so the recorded remoteBin is unchanged.
type releaseInstaller struct {
	// version is the release tag to pin (CLIPBEAM_VERSION). It mirrors cmdVersion at
	// selection time; a dev build never reaches here (selectInstaller guards it).
	version string
}

func (ri releaseInstaller) install(o out, client sshx.Client, t sshx.Target, _, _, _ string) error {
	sess, derr := client.Dial(t)
	if derr != nil {
		return codedSSH(derr)
	}
	defer func() { _ = sess.Close() }()

	cmd := releaseInstallCommand(ri.version)
	o.diag("setup: --from-release: remote box fetching %s (CLIPBEAM_VERSION=%s)", rawInstallURL, ri.version)
	o.trace("setup: remote install command: %s", cmd)
	_, stderr, rerr := sess.Run(cmd, nil)
	if rerr != nil {
		// A missing curl/wget (the installer's own exit 127) or any non-zero installer
		// exit maps to ExitRemoteIO, surfacing the captured remote stderr (mirrors
		// streamBinary's error path).
		return coded(ExitRemoteIO, fmt.Errorf("setup: --from-release remote install failed: %v: %s", rerr, strings.TrimSpace(string(stderr))))
	}
	return nil
}

// releaseInstallCommand builds the single remote-exec command that fetches the published
// install.sh and pipes it to sh, with CLIPBEAM_VERSION pinned. It prefers curl, falls
// back to wget, and exits 127 with a clear message when neither is present (mapped to
// ExitRemoteIO by the caller). The installer reads from the network, NOT from our stdin.
func releaseInstallCommand(version string) string {
	return "sh -c 'set -e; " +
		"export CLIPBEAM_VERSION=" + shellQuotePosix(version) + "; " +
		"if command -v curl >/dev/null 2>&1; then curl -fsSL " + rawInstallURL + " | sh; " +
		"elif command -v wget >/dev/null 2>&1; then wget -qO- " + rawInstallURL + " | sh; " +
		"else echo \"clipbeam: need curl or wget on the remote\" >&2; exit 127; fi'"
}

// selectInstaller picks the install route from the flags + the build version (fix [C] +
// the [?] released-binary no-Go route):
//
//	--from-release        → releaseInstaller (remote box fetches the published installer;
//	                        a dev build has no matching release tag, so it is a config
//	                        error and never dials).
//	released (semver)     → releaseTarballInstaller: download the matching-arch release
//	                        tarball LOCALLY and stream its bytes over SSH — the no-Go,
//	                        no-remote-egress default for a real released binary ([?]).
//	dev build             → streamInstaller: the existing cross-build + stream (the
//	                        dev/source path; a dev build has the Go module to build from).
func selectInstaller(opts setupOptions, version string) (remoteInstaller, error) {
	if opts.fromRelease {
		if version == "dev" {
			return nil, configError("setup: --from-release needs a released version; this is a dev build (no matching release tag)")
		}
		return releaseInstaller{version: version}, nil
	}
	if version == "dev" {
		return streamInstaller{}, nil
	}
	// A released (non-dev) binary has no Go toolchain / source module on the user's box,
	// so the default cross-build would fail. Download the matching release tarball
	// locally and stream its bytes over SSH (preserving the no-remote-egress promise).
	return releaseTarballInstaller{version: version}, nil
}
