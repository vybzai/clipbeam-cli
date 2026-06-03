package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestReleaseInstallCommandPinsVersion asserts the remote command pins CLIPBEAM_VERSION,
// names the raw install.sh URL, and carries a curl||wget fallback (fix [C]).
func TestReleaseInstallCommandPinsVersion(t *testing.T) {
	cmd := releaseInstallCommand("v1.2.3")
	for _, want := range []string{
		"export CLIPBEAM_VERSION='v1.2.3'",
		rawInstallURL,
		"command -v curl",
		"command -v wget",
		"exit 127",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("release install command missing %q\ngot: %s", want, cmd)
		}
	}
}

// TestReleaseInstallerMissingDownloaderMapsRemoteIO drives releaseInstaller against a
// fake session whose Run returns a non-zero exit + the "need curl or wget" stderr, and
// asserts the returned error is coded ExitRemoteIO and carries the stderr (fix [C] T2).
func TestReleaseInstallerMissingDownloaderMapsRemoteIO(t *testing.T) {
	sess := &fakeSession{
		stderr: []byte("clipbeam: need curl or wget on the remote"),
		runErr: errors.New("status 127"),
	}
	fc := &fakeClient{sess: sess}
	o := out{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}

	err := releaseInstaller{version: "v1.2.3"}.install(o, fc, dummyTarget(), "linux", "amd64", "/home/u/.local/bin/clipbeam")
	if err == nil {
		t.Fatal("expected an error from a non-zero remote install")
	}
	var ce *codedError
	if !errors.As(err, &ce) || ce.code != ExitRemoteIO {
		t.Fatalf("error code = %v, want ExitRemoteIO; err=%v", err, err)
	}
	if !strings.Contains(err.Error(), "need curl or wget") {
		t.Errorf("error must carry the remote stderr, got %q", err.Error())
	}
}

// TestFromReleaseDevBuildErrors asserts selectInstaller with --from-release on a dev
// build returns a config-class error and never reaches a dial (fix [C] T3).
func TestFromReleaseDevBuildErrors(t *testing.T) {
	_, err := selectInstaller(setupOptions{fromRelease: true}, "dev")
	if err == nil {
		t.Fatal("--from-release on a dev build must error")
	}
	var ce *codedError
	if !errors.As(err, &ce) || ce.code != ExitConfig {
		t.Fatalf("error = %v, want ExitConfig", err)
	}
}

// TestReleaseInstallerCapturesStdout asserts a clean (exit 0) remote install returns nil
// (fix [C] T4).
func TestReleaseInstallerCapturesStdout(t *testing.T) {
	sess := &fakeSession{stdout: []byte("installed clipbeam v1.2.3")}
	fc := &fakeClient{sess: sess}
	o := out{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}

	if err := (releaseInstaller{version: "v1.2.3"}).install(o, fc, dummyTarget(), "linux", "arm64", "/home/u/.local/bin/clipbeam"); err != nil {
		t.Fatalf("clean install must return nil, got %v", err)
	}
	// The pinned version reached the remote command.
	if !strings.Contains(sess.gotCommand, "export CLIPBEAM_VERSION='v1.2.3'") {
		t.Errorf("remote command did not pin the version: %s", sess.gotCommand)
	}
}

// TestReleaseAssetName mirrors the install.sh naming rules (the [?] no-Go route): v-strip
// + darwin->universal.
func TestReleaseAssetName(t *testing.T) {
	for _, tc := range []struct {
		goos, goarch, version, want string
	}{
		{"linux", "amd64", "v1.2.3", "clipbeam_1.2.3_linux_amd64.tar.gz"},
		{"linux", "arm64", "v1.2.3", "clipbeam_1.2.3_linux_arm64.tar.gz"},
		{"darwin", "amd64", "v1.2.3", "clipbeam_1.2.3_darwin_universal.tar.gz"},
		{"darwin", "arm64", "v0.9.0", "clipbeam_0.9.0_darwin_universal.tar.gz"},
		{"linux", "amd64", "1.2.3", "clipbeam_1.2.3_linux_amd64.tar.gz"}, // no leading v
	} {
		if got := releaseAssetName(tc.goos, tc.goarch, tc.version); got != tc.want {
			t.Errorf("releaseAssetName(%q,%q,%q) = %q, want %q", tc.goos, tc.goarch, tc.version, got, tc.want)
		}
	}
}

// TestSelectInstallerDispatch asserts the install-route selection: dev -> streamInstaller
// (cross-build); a released semver default -> releaseTarballInstaller (no-Go tarball);
// --from-release on a release -> releaseInstaller.
func TestSelectInstallerDispatch(t *testing.T) {
	// dev, no flag -> streamInstaller
	in, err := selectInstaller(setupOptions{}, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := in.(streamInstaller); !ok {
		t.Errorf("dev default = %T, want streamInstaller", in)
	}

	// released semver, no flag -> releaseTarballInstaller
	in, err = selectInstaller(setupOptions{}, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	tb, ok := in.(releaseTarballInstaller)
	if !ok {
		t.Errorf("released default = %T, want releaseTarballInstaller", in)
	} else if tb.version != "v1.2.3" {
		t.Errorf("tarball version = %q, want v1.2.3", tb.version)
	}

	// --from-release on a release -> releaseInstaller
	in, err = selectInstaller(setupOptions{fromRelease: true}, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if ri, ok := in.(releaseInstaller); !ok {
		t.Errorf("--from-release = %T, want releaseInstaller", in)
	} else if ri.version != "v1.2.3" {
		t.Errorf("release version = %q, want v1.2.3", ri.version)
	}
}
