package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// makeTarGz builds an in-memory .tar.gz containing the given (name -> content) entries as
// regular files. Used to synthesize release archives for the [M1] tarball-installer tests.
func makeTarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %q: %v", name, err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("tar write %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// hexSum is the lowercase hex SHA-256 of b (mirrors what checksums.txt records).
func hexSum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fakeHTTP returns an httpGet seam serving the given url->body map (200) and a 404 for any
// other URL. It restores the original httpGet via t.Cleanup.
func fakeHTTP(t *testing.T, bodies map[string][]byte) {
	t.Helper()
	prev := httpGet
	httpGet = func(url string) (*http.Response, error) {
		if body, ok := bodies[url]; ok {
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		}
		return &http.Response{
			StatusCode: 404,
			Body:       io.NopCloser(strings.NewReader("not found")),
		}, nil
	}
	t.Cleanup(func() { httpGet = prev })
}

// TestVerifyChecksum tables the match / mismatch / missing-entry branches (fix [M1]).
func TestVerifyChecksum(t *testing.T) {
	archive := []byte("the release archive bytes")
	good := hexSum(archive)

	cases := []struct {
		name    string
		asset   string
		checks  string
		wantErr string // "" => no error
	}{
		{
			name:   "match",
			asset:  "clipbeam_1.2.3_linux_amd64.tar.gz",
			checks: good + "  clipbeam_1.2.3_linux_amd64.tar.gz\n",
		},
		{
			name:    "mismatch",
			asset:   "clipbeam_1.2.3_linux_amd64.tar.gz",
			checks:  strings.Repeat("0", 64) + "  clipbeam_1.2.3_linux_amd64.tar.gz\n",
			wantErr: "checksum mismatch",
		},
		{
			name:    "missing-entry",
			asset:   "clipbeam_1.2.3_linux_amd64.tar.gz",
			checks:  good + "  some_other_asset.tar.gz\n",
			wantErr: "no entry for",
		},
		{
			name:  "match-among-many-lines",
			asset: "clipbeam_1.2.3_darwin_universal.tar.gz",
			checks: strings.Repeat("a", 64) + "  clipbeam_1.2.3_linux_amd64.tar.gz\n" +
				good + "  clipbeam_1.2.3_darwin_universal.tar.gz\n" +
				strings.Repeat("b", 64) + "  clipbeam_1.2.3_linux_arm64.tar.gz\n",
		},
		{
			name:   "match-uppercase-hex",
			asset:  "clipbeam_1.2.3_linux_amd64.tar.gz",
			checks: strings.ToUpper(good) + "  clipbeam_1.2.3_linux_amd64.tar.gz\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyChecksum(tc.asset, archive, []byte(tc.checks))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyChecksum = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("verifyChecksum = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestExtractClipbeamBinary tables: binary at top level, nested in a subdir, and a
// not-found error (fix [M1]).
func TestExtractClipbeamBinary(t *testing.T) {
	wantBin := []byte("\x7fELF...the clipbeam binary...")

	t.Run("top-level", func(t *testing.T) {
		gz := makeTarGz(t, map[string][]byte{
			"clipbeam":  wantBin,
			"LICENSE":   []byte("license text"),
			"README.md": []byte("readme"),
		})
		got, err := extractClipbeamBinary(gz)
		if err != nil {
			t.Fatalf("extract: %v", err)
		}
		if !bytes.Equal(got, wantBin) {
			t.Fatalf("extracted %q, want the clipbeam binary bytes", got)
		}
	})

	t.Run("nested-in-subdir", func(t *testing.T) {
		gz := makeTarGz(t, map[string][]byte{
			"clipbeam_1.2.3_linux_amd64/clipbeam": wantBin,
			"clipbeam_1.2.3_linux_amd64/LICENSE":  []byte("license"),
		})
		got, err := extractClipbeamBinary(gz)
		if err != nil {
			t.Fatalf("extract nested: %v", err)
		}
		if !bytes.Equal(got, wantBin) {
			t.Fatalf("nested extracted %q, want the clipbeam binary bytes", got)
		}
	})

	t.Run("not-found", func(t *testing.T) {
		gz := makeTarGz(t, map[string][]byte{
			"NOTclipbeam": wantBin,
			"LICENSE":     []byte("license"),
		})
		_, err := extractClipbeamBinary(gz)
		if err == nil || !strings.Contains(err.Error(), "no clipbeam binary") {
			t.Fatalf("extract not-found = %v, want a 'no clipbeam binary' error", err)
		}
	})

	t.Run("not-a-gzip", func(t *testing.T) {
		_, err := extractClipbeamBinary([]byte("plain bytes, not gzip"))
		if err == nil {
			t.Fatal("extract on non-gzip bytes must error")
		}
	})

	t.Run("over-cap-rejected", func(t *testing.T) {
		prevCap := maxReleaseDownloadBytes
		maxReleaseDownloadBytes = 4
		defer func() { maxReleaseDownloadBytes = prevCap }()
		gz := makeTarGz(t, map[string][]byte{"clipbeam": []byte("0123456789")})
		_, err := extractClipbeamBinary(gz)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("extract over-cap = %v, want an 'exceeds ... (capped)' error", err)
		}
	})
}

// TestReleaseTarballInstallerInstall drives releaseTarballInstaller.install end-to-end via
// a fake httpGet serving a synthetic .tar.gz + checksums.txt and the fakeClient/fakeSession
// SSH harness, covering the happy-path stream, the checksum-mismatch error branch, and the
// no-entry error branch (fix [M1]).
func TestReleaseTarballInstallerInstall(t *testing.T) {
	const version = "v1.2.3"
	const goos, goarch = "linux", "amd64"
	asset := releaseAssetName(goos, goarch, version) // clipbeam_1.2.3_linux_amd64.tar.gz
	base := releaseBaseURL + "/" + version + "/"
	binBytes := []byte("\x7fELFclipbeam-binary-bytes")
	archive := makeTarGz(t, map[string][]byte{"clipbeam": binBytes})

	t.Run("happy-path-streams-binary-over-ssh", func(t *testing.T) {
		fakeHTTP(t, map[string][]byte{
			base + asset:           archive,
			base + "checksums.txt": []byte(hexSum(archive) + "  " + asset + "\n"),
		})
		sess := &fakeSession{stdout: []byte("ok")}
		fc := &fakeClient{sess: sess}
		o := out{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}

		err := releaseTarballInstaller{version: version}.install(o, fc, dummyTarget(), goos, goarch, "/home/u/.local/bin/clipbeam")
		if err != nil {
			t.Fatalf("happy-path install = %v, want nil", err)
		}
		// The extracted binary bytes were streamed over SSH as the remote stdin.
		if !bytes.Equal(sess.gotStdin, binBytes) {
			t.Fatalf("streamed stdin = %q, want the extracted binary bytes", sess.gotStdin)
		}
		// The remote place command targets the recorded remoteBin.
		if !strings.Contains(sess.gotCommand, "/home/u/.local/bin/clipbeam") {
			t.Errorf("remote command %q does not target the remoteBin path", sess.gotCommand)
		}
	})

	t.Run("checksum-mismatch-errors-before-stream", func(t *testing.T) {
		fakeHTTP(t, map[string][]byte{
			base + asset:           archive,
			base + "checksums.txt": []byte(strings.Repeat("0", 64) + "  " + asset + "\n"),
		})
		sess := &fakeSession{}
		fc := &fakeClient{sess: sess}
		o := out{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}

		err := releaseTarballInstaller{version: version}.install(o, fc, dummyTarget(), goos, goarch, "/home/u/.local/bin/clipbeam")
		if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
			t.Fatalf("install = %v, want a checksum-mismatch error", err)
		}
		var ce *codedError
		if !errors.As(err, &ce) || ce.code != ExitGeneric {
			t.Fatalf("checksum-mismatch error code = %v, want ExitGeneric", err)
		}
		// It must NOT have dialed / streamed anything after a failed verify.
		if fc.dialCount != 0 {
			t.Errorf("dialed %d times on a checksum failure, want 0 (no stream after mismatch)", fc.dialCount)
		}
	})

	t.Run("no-checksum-entry-errors", func(t *testing.T) {
		fakeHTTP(t, map[string][]byte{
			base + asset:           archive,
			base + "checksums.txt": []byte(hexSum(archive) + "  a_different_asset.tar.gz\n"),
		})
		sess := &fakeSession{}
		fc := &fakeClient{sess: sess}
		o := out{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}

		err := releaseTarballInstaller{version: version}.install(o, fc, dummyTarget(), goos, goarch, "/home/u/.local/bin/clipbeam")
		if err == nil || !strings.Contains(err.Error(), "no entry for") {
			t.Fatalf("install = %v, want a no-entry error", err)
		}
		var ce *codedError
		if !errors.As(err, &ce) || ce.code != ExitGeneric {
			t.Fatalf("no-entry error code = %v, want ExitGeneric", err)
		}
		if fc.dialCount != 0 {
			t.Errorf("dialed %d times on a missing checksum entry, want 0", fc.dialCount)
		}
	})

	t.Run("asset-download-404-maps-unreachable", func(t *testing.T) {
		// Serve only checksums.txt; the asset URL 404s -> fetchBytes errors.
		fakeHTTP(t, map[string][]byte{
			base + "checksums.txt": []byte(hexSum(archive) + "  " + asset + "\n"),
		})
		fc := &fakeClient{sess: &fakeSession{}}
		o := out{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}

		err := releaseTarballInstaller{version: version}.install(o, fc, dummyTarget(), goos, goarch, "/home/u/.local/bin/clipbeam")
		if err == nil {
			t.Fatal("install with a 404 asset must error")
		}
		var ce *codedError
		if !errors.As(err, &ce) || ce.code != ExitUnreachable {
			t.Fatalf("404 asset error code = %v, want ExitUnreachable", err)
		}
	})
}

// TestFetchBytesStatusAndBody covers fetchBytes: a happy small 200 body, a non-2xx error,
// a propagated transport error, and the [L2] over-cap rejection (the cap is lowered for the
// test so a tiny body trips it, exercising the io.LimitReader+1 boundary check).
func TestFetchBytesStatusAndBody(t *testing.T) {
	// Happy: a small 200 body round-trips.
	prev := httpGet
	defer func() { httpGet = prev }()
	httpGet = func(string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("hello"))}, nil
	}
	got, err := fetchBytes("https://example/x")
	if err != nil || string(got) != "hello" {
		t.Fatalf("fetchBytes happy = (%q,%v), want (hello,nil)", got, err)
	}

	// Over-cap: lower the defensive ceiling so a small body trips the LimitReader+1 check.
	prevCap := maxReleaseDownloadBytes
	maxReleaseDownloadBytes = 4
	httpGet = func(string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("0123456789"))}, nil
	}
	_, err = fetchBytes("https://example/big")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("fetchBytes over-cap = %v, want an 'exceeds ... (capped)' error", err)
	}
	// A body exactly at the cap is accepted (boundary).
	httpGet = func(string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("0123"))}, nil
	}
	got, err = fetchBytes("https://example/exact")
	if err != nil || string(got) != "0123" {
		t.Fatalf("fetchBytes at-cap = (%q,%v), want (0123,nil)", got, err)
	}
	maxReleaseDownloadBytes = prevCap

	// Non-2xx: an error citing the status.
	httpGet = func(string) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("down"))}, nil
	}
	_, err = fetchBytes("https://example/y")
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("fetchBytes non-2xx = %v, want a 503 error", err)
	}

	// Transport error propagates.
	httpGet = func(string) (*http.Response, error) { return nil, fmt.Errorf("dial boom") }
	_, err = fetchBytes("https://example/z")
	if err == nil || !strings.Contains(err.Error(), "dial boom") {
		t.Fatalf("fetchBytes transport err = %v, want it propagated", err)
	}
}
