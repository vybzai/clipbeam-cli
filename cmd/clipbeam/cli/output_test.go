package cli

import (
	"bytes"
	"os"
	"testing"
)

// TestColorEnabled verifies the §8.1 color policy: disabled under --json, --no-color,
// NO_COLOR, or off-TTY. The test buffers are not TTYs, so the baseline is already off;
// the --json / NO_COLOR gates are asserted explicitly.
func TestColorEnabled(t *testing.T) {
	// Save + restore global flags and the env.
	savedJSON, savedNoColor := gflags.json, gflags.noColor
	savedNoColorEnv, hadNoColorEnv := os.LookupEnv("NO_COLOR")
	t.Cleanup(func() {
		gflags.json, gflags.noColor = savedJSON, savedNoColor
		if hadNoColorEnv {
			_ = os.Setenv("NO_COLOR", savedNoColorEnv)
		} else {
			_ = os.Unsetenv("NO_COLOR")
		}
	})

	gflags.json = true
	gflags.noColor = false
	_ = os.Unsetenv("NO_COLOR")
	if colorEnabled() {
		t.Error("--json must imply --no-color (PLAN §8.1)")
	}

	gflags.json = false
	gflags.noColor = true
	if colorEnabled() {
		t.Error("--no-color must disable color")
	}

	gflags.json = false
	gflags.noColor = false
	_ = os.Setenv("NO_COLOR", "1")
	if colorEnabled() {
		t.Error("NO_COLOR (non-empty) must disable color")
	}
}

// TestQuietSilencesStderr verifies --quiet silences diag (PLAN §8.1) but never touches
// stdout.
func TestQuietSilencesStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := out{stdout: &stdout, stderr: &stderr, quiet: true}
	o.diag("a diagnostic")
	o.data("DATA")
	if stderr.Len() != 0 {
		t.Errorf("--quiet wrote to stderr: %q", stderr.String())
	}
	if stdout.String() != "DATA" {
		t.Errorf("stdout = %q, want DATA (data is never silenced)", stdout.String())
	}
}

// TestVerboseTrace verifies --verbose enables trace, and the default suppresses it.
func TestVerboseTrace(t *testing.T) {
	var stderr bytes.Buffer
	o := out{stderr: &stderr, verbose: false}
	o.trace("hidden")
	if stderr.Len() != 0 {
		t.Errorf("trace leaked without --verbose: %q", stderr.String())
	}
	stderr.Reset()
	o.verbose = true
	o.trace("shown")
	if stderr.String() != "shown\n" {
		t.Errorf("verbose trace = %q, want %q", stderr.String(), "shown\n")
	}
}

// TestUseColorReflectsResolved verifies useColor returns the resolved field.
func TestUseColorReflectsResolved(t *testing.T) {
	if (out{color: true}).useColor() != true {
		t.Error("useColor should reflect color=true")
	}
	if (out{color: false}).useColor() != false {
		t.Error("useColor should reflect color=false")
	}
}

// TestEnvFlag verifies the CLIPBEAM_JSON truthiness rule (PLAN §8.1).
func TestEnvFlag(t *testing.T) {
	cases := []struct {
		val  string
		set  bool
		want bool
	}{
		{"1", true, true},
		{"true", true, true},
		{"yes", true, true},
		{"0", true, false},
		{"false", true, false},
		{"", true, false},
	}
	for _, c := range cases {
		_ = os.Unsetenv("CB_TEST_FLAG")
		if c.set {
			_ = os.Setenv("CB_TEST_FLAG", c.val)
		}
		if got := envFlag("CB_TEST_FLAG"); got != c.want {
			t.Errorf("envFlag(%q set=%v) = %v, want %v", c.val, c.set, got, c.want)
		}
	}
	_ = os.Unsetenv("CB_TEST_FLAG")
}
