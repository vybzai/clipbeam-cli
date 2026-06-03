package httpd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// TestItoaPort drives the uint16 port renderer across 0, a single digit, and a
// multi-digit port (the loop). It is the helper that builds JoinHostPort's port string.
func TestItoaPort(t *testing.T) {
	cases := map[uint16]string{0: "0", 7: "7", 80: "80", 8787: "8787", 65535: "65535"}
	for in, want := range cases {
		if got := itoa(in); got != want {
			t.Errorf("itoa(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestItoa64 drives the int64 renderer used for the {sentItems:N} body.
func TestItoa64(t *testing.T) {
	cases := map[int64]string{0: "0", 9: "9", 123: "123", 52428800: "52428800"}
	for in, want := range cases {
		if got := itoa64(in); got != want {
			t.Errorf("itoa64(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestMaxBytesFallback drives Server.maxBytes: a positive Config.MaxBytes is used
// verbatim; a non-positive value falls back to wire.DefaultMaxBytes (PLAN §3.8).
func TestMaxBytesFallback(t *testing.T) {
	s := &Server{opts: Options{Config: config.Config{MaxBytes: 1234}}}
	if got := s.maxBytes(); got != 1234 {
		t.Fatalf("maxBytes positive = %d, want 1234", got)
	}
	s0 := &Server{opts: Options{Config: config.Config{MaxBytes: 0}}}
	if got := s0.maxBytes(); got != wire.DefaultMaxBytes {
		t.Fatalf("maxBytes zero = %d, want default %d", got, wire.DefaultMaxBytes)
	}
	sneg := &Server{opts: Options{Config: config.Config{MaxBytes: -5}}}
	if got := sneg.maxBytes(); got != wire.DefaultMaxBytes {
		t.Fatalf("maxBytes negative = %d, want default", got)
	}
}

// TestTempDirExplicitAndDerived drives Server.tempDir: an explicit Options.TempDir is
// created and returned; an empty TempDir derives the dir from config.ResolvedSaveDir.
func TestTempDirExplicitAndDerived(t *testing.T) {
	// Explicit TempDir branch.
	explicit := filepath.Join(t.TempDir(), "scratch")
	s := &Server{opts: Options{TempDir: explicit}}
	got, err := s.tempDir()
	if err != nil {
		t.Fatalf("tempDir explicit: %v", err)
	}
	if got != explicit {
		t.Fatalf("tempDir = %q, want %q", got, explicit)
	}
	if fi, statErr := os.Stat(explicit); statErr != nil || !fi.IsDir() {
		t.Fatalf("explicit TempDir not created: %v", statErr)
	}

	// Derived branch: empty TempDir → ResolvedSaveDir(Config).
	saveDir := filepath.Join(t.TempDir(), "save")
	if err := os.MkdirAll(saveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.SaveDir = saveDir
	sd := &Server{opts: Options{Config: cfg}}
	derived, err := sd.tempDir()
	if err != nil {
		t.Fatalf("tempDir derived: %v", err)
	}
	if derived == "" {
		t.Fatal("derived tempDir is empty")
	}
}

// TestShutdownNilServer drives Server.Shutdown's nil-srv guard (a New'd server always
// has srv set, but the guard must be safe).
func TestShutdownNilServer(t *testing.T) {
	s := &Server{}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown of a nil-srv Server = %v, want nil", err)
	}
}

// TestServeUnknownListenKind drives listen()'s default (unknown kind) error branch.
func TestServeUnknownListenKind(t *testing.T) {
	s := New(Options{Listen: ListenKind(99)})
	_, err := s.listen(context.Background())
	if err == nil {
		t.Fatal("listen with an unknown ListenKind must error")
	}
}
