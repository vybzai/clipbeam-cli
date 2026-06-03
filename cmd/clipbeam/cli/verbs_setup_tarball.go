package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/sshx"
)

// releaseBaseURL is the GitHub release download base; the asset + checksums live under
// <releaseBaseURL>/<tag>/ (mirrors install.sh).
const releaseBaseURL = "https://github.com/vybzai/clipbeam-cli/releases/download"

// maxReleaseDownloadBytes is a defensive ceiling for both the fetched release archive and
// the extracted binary (fix [L2] size cap). The source is OUR own GitHub release (a small
// static binary, low single-digit MB), but the download is still untrusted bytes from the
// network until the checksum verifies, so cap reads with an io.LimitReader to bound memory
// even if a redirect were ever hijacked to a huge body. 512 MiB is generous headroom over
// any real clipbeam release while still preventing an unbounded read. It is a var (not a
// const) so a test can lower it to exercise the over-cap rejection without a giant body.
var maxReleaseDownloadBytes int64 = 512 << 20

// httpGet is the seam for fetching release assets, swappable in tests.
var httpGet = func(url string) (*http.Response, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	return client.Get(url)
}

// releaseTarballInstaller is the no-Go, no-remote-egress route for a RELEASED binary
// (the [?] case): the invoking Mac (which already has internet for SSH) downloads the
// matching-arch release archive, verifies it against checksums.txt, unpacks the remote
// binary, and streams THOSE bytes over the SAME SSH connection. The remote box needs no
// internet, no curl, and no Go toolchain.
type releaseTarballInstaller struct {
	version string
}

func (ri releaseTarballInstaller) install(o out, client sshx.Client, t sshx.Target, goos, goarch, remoteBin string) error {
	asset := releaseAssetName(goos, goarch, ri.version)
	base := releaseBaseURL + "/" + ri.version + "/"
	o.diag("setup: downloading release asset %s ...", asset)

	archive, err := fetchBytes(base + asset)
	if err != nil {
		return coded(ExitUnreachable, fmt.Errorf("setup: download release asset %s: %w", asset, err))
	}
	// Verify against checksums.txt (download both, match the SHA-256 line for the asset)
	// before unpacking, to avoid streaming a corrupt/wrong-arch binary.
	checks, cerr := fetchBytes(base + "checksums.txt")
	if cerr != nil {
		return coded(ExitUnreachable, fmt.Errorf("setup: download checksums.txt: %w", cerr))
	}
	if err := verifyChecksum(asset, archive, checks); err != nil {
		return coded(ExitGeneric, fmt.Errorf("setup: %w", err))
	}

	bin, err := extractClipbeamBinary(archive)
	if err != nil {
		return coded(ExitGeneric, fmt.Errorf("setup: extract clipbeam from %s: %w", asset, err))
	}

	// Write the extracted binary to a temp file and stream it over SSH (same path the
	// dev cross-build uses).
	tmp, err := os.CreateTemp("", "clipbeam-release-*")
	if err != nil {
		return coded(ExitGeneric, fmt.Errorf("setup: temp file: %w", err))
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, werr := tmp.Write(bin); werr != nil {
		_ = tmp.Close()
		return coded(ExitGeneric, fmt.Errorf("setup: write temp binary: %w", werr))
	}
	if cerr := tmp.Close(); cerr != nil {
		return coded(ExitGeneric, fmt.Errorf("setup: close temp binary: %w", cerr))
	}
	return streamBinary(o, client, t, tmpName, remoteBin)
}

// releaseAssetName centralizes the install.sh asset-naming rule (install.sh:148-152 +
// .goreleaser.yaml name_template): clipbeam_<ver-without-leading-v>_<os>_<arch>.tar.gz,
// where <arch> is amd64|arm64 for linux but the literal "universal" for darwin (the
// GoReleaser universal_binaries with replace:true removes per-arch darwin archives).
func releaseAssetName(goos, goarch, version string) string {
	ver := strings.TrimPrefix(version, "v")
	arch := goarch
	if goos == "darwin" {
		arch = "universal"
	}
	return fmt.Sprintf("clipbeam_%s_%s_%s.tar.gz", ver, goos, arch)
}

// fetchBytes GETs url and returns the body, erroring on a non-2xx status. The body read
// is bounded by maxReleaseDownloadBytes (fix [L2]): an io.LimitReader caps the bytes, and
// a body that would exceed the cap is rejected rather than silently truncated (a truncated
// archive would later fail the checksum/extract, but failing loud here is clearer).
func fetchBytes(url string) ([]byte, error) {
	resp, err := httpGet(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http %d for %s", resp.StatusCode, url)
	}
	// Read one extra byte so an at-the-limit overflow is detectable (the cap is generous
	// headroom over any real release, so hitting it means something is wrong upstream).
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReleaseDownloadBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxReleaseDownloadBytes {
		return nil, fmt.Errorf("release body for %s exceeds %d bytes (capped)", url, maxReleaseDownloadBytes)
	}
	return body, nil
}

// verifyChecksum matches the SHA-256 of archive against the line for asset in a
// checksums.txt body (each line: "<hex>  <filename>"). A missing line or a mismatch is
// an error so a corrupt/wrong-arch download never reaches the remote box.
func verifyChecksum(asset string, archive, checks []byte) error {
	sum := sha256.Sum256(archive)
	want := hex.EncodeToString(sum[:])
	for _, line := range strings.Split(string(checks), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[1] == asset {
			if !strings.EqualFold(fields[0], want) {
				return fmt.Errorf("checksum mismatch for %s (got %s, want %s)", asset, want, fields[0])
			}
			return nil
		}
	}
	return fmt.Errorf("checksums.txt has no entry for %s", asset)
}

// extractClipbeamBinary unpacks the "clipbeam" binary from a .tar.gz archive's bytes.
func extractClipbeamBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		// The archive carries the binary at the top level named "clipbeam".
		name := hdr.Name
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		if name == "clipbeam" && hdr.Typeflag == tar.TypeReg {
			// Cap the decompressed read so a malformed/oversized (or zip-bomb) entry cannot
			// inflate unbounded (fix [L2]); the real binary is a few MB, well under the cap.
			bin, err := io.ReadAll(io.LimitReader(tr, maxReleaseDownloadBytes+1))
			if err != nil {
				return nil, err
			}
			if int64(len(bin)) > maxReleaseDownloadBytes {
				return nil, fmt.Errorf("clipbeam binary in archive exceeds %d bytes (capped)", maxReleaseDownloadBytes)
			}
			return bin, nil
		}
	}
	return nil, fmt.Errorf("no clipbeam binary in archive")
}
