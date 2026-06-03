package config

import (
	"os"
	"runtime"
	"testing"
)

// TestFileTokenStoreRoundTrip asserts the 0600 file backend round-trips a token,
// lands 0600, re-chmods a loosened file on load, and trims a trailing newline a human
// may have added (PLAN §4.2/§6.4.3).
func TestFileTokenStoreRoundTrip(t *testing.T) {
	withHome(t)
	p, _ := Resolve()
	if p.Token == "" {
		// macOS Paths leaves Token empty; force a usable file path under ~/.clipbeam.
		p.Token = withHomeTokenPath(t)
	}
	f := newFileTokenStore(p)

	// Missing → no token, no error.
	if tok, ok, err := f.Load(); err != nil || ok || tok != "" {
		t.Fatalf("missing token: tok=%q ok=%v err=%v", tok, ok, err)
	}

	const want = "ABCDEFGHIJKLMNOPQRSTUVWX"
	if err := f.Save(want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file perm = %o, want 600", perm)
	}
	tok, ok, err := f.Load()
	if err != nil || !ok || tok != want {
		t.Fatalf("round-trip: tok=%q ok=%v err=%v", tok, ok, err)
	}

	// Loosen the perms + add a trailing newline; Load must re-chmod 0600 and trim.
	if err := os.WriteFile(f.path, []byte(want+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tok, ok, err = f.Load()
	if err != nil || !ok || tok != want {
		t.Fatalf("loosened/newline load: tok=%q ok=%v err=%v", tok, ok, err)
	}
	info, _ = os.Stat(f.path)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file must be re-chmod'd 0600 on load, got %o", perm)
	}
}

// withHomeTokenPath returns ~/.clipbeam/token under the test HOME and ensures parent.
func withHomeTokenPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return home + "/.clipbeam/token"
}

// TestOpenTokenStoreFile asserts the file backend is always selectable and reports
// kind "file".
func TestOpenTokenStoreFile(t *testing.T) {
	withHome(t)
	p, _ := Resolve()
	ts, err := OpenTokenStore(TokenStoreFile, p)
	if err != nil {
		t.Fatal(err)
	}
	if ts.Kind() != "file" {
		t.Errorf("kind = %q, want file", ts.Kind())
	}
}

// TestOpenTokenStoreAuto asserts auto-selection: keychain on macOS, else file when no
// D-Bus session is present (the secret-service branch fast-falls through, PLAN §6.4).
func TestOpenTokenStoreAuto(t *testing.T) {
	withHome(t)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "") // no desktop secret service
	p, _ := Resolve()
	ts, err := OpenTokenStore(TokenStoreAuto, p)
	if err != nil {
		t.Fatal(err)
	}
	wantKind := "file"
	if runtime.GOOS == "darwin" {
		wantKind = "keychain"
	}
	if ts.Kind() != wantKind {
		t.Errorf("auto kind = %q, want %q", ts.Kind(), wantKind)
	}
}

// TestOpenTokenStoreKeychainOffMacErrors asserts forcing keychain off macOS errors.
func TestOpenTokenStoreKeychainOffMacErrors(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("keychain is valid on macOS")
	}
	withHome(t)
	p, _ := Resolve()
	if _, err := OpenTokenStore(TokenStoreKeychain, p); err == nil {
		t.Errorf("forcing keychain off macOS must error")
	}
}

// TestOpenTokenStoreSecretServiceNoBusErrors asserts forcing secret-service with no
// D-Bus session errors (never blocks on a locked keyring, PLAN §6.4.2).
func TestOpenTokenStoreSecretServiceNoBusErrors(t *testing.T) {
	withHome(t)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	p, _ := Resolve()
	if _, err := OpenTokenStore(TokenStoreSecretService, p); err == nil {
		t.Errorf("forcing secret-service with no D-Bus session must error")
	}
}

// TestOpenTokenStoreUnknownErrors asserts an unknown kind errors.
func TestOpenTokenStoreUnknownErrors(t *testing.T) {
	withHome(t)
	p, _ := Resolve()
	if _, err := OpenTokenStore(TokenStoreKind("nope"), p); err == nil {
		t.Errorf("unknown token store kind must error")
	}
}
