package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withHome points $HOME (and clears XDG_* unless set) at a temp dir so Resolve() and
// the stores operate in isolation. It returns the temp home.
func withHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Clear XDG so the ~/.local|.config defaults apply unless a test sets them.
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	return home
}

// TestXDGValidation asserts $XDG_*_HOME is honored only when absolute; a relative or
// empty value falls back to the ~/.local|.config default (PLAN §4 XDG validation),
// so state dirs are never created in cwd.
func TestXDGValidation(t *testing.T) {
	if got := xdgDir("CLIPBEAM_TEST_XDG", "/fallback"); got != "/fallback" {
		t.Errorf("unset env: got %q, want /fallback", got)
	}
	t.Setenv("CLIPBEAM_TEST_XDG", "relative/path")
	if got := xdgDir("CLIPBEAM_TEST_XDG", "/fallback"); got != "/fallback" {
		t.Errorf("relative env must be rejected: got %q, want /fallback", got)
	}
	t.Setenv("CLIPBEAM_TEST_XDG", "/abs/xdg")
	if got := xdgDir("CLIPBEAM_TEST_XDG", "/fallback"); got != "/abs/xdg" {
		t.Errorf("absolute env must be honored: got %q, want /abs/xdg", got)
	}
}

// TestLinuxPathsLayout asserts the XDG-idiomatic Linux layout (PLAN §4), independent
// of the host GOOS (it exercises linuxPaths directly).
func TestLinuxPathsLayout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	p := linuxPaths(home)
	want := map[string]string{
		"SaveDir":    filepath.Join(home, ".local", "share", "clipbeam"),
		"AgentInbox": filepath.Join(home, ".local", "state", "clipbeam", "agent-inbox"),
		"LastPath":   filepath.Join(home, ".local", "state", "clipbeam", "last_path"),
		"Recents":    filepath.Join(home, ".local", "state", "clipbeam", "recents.json"),
		"Config":     filepath.Join(home, ".config", "clipbeam", "config.json"),
		"Aliases":    filepath.Join(home, ".config", "clipbeam", "aliases.json"),
		"Token":      filepath.Join(home, ".config", "clipbeam", "token"),
		"Log":        filepath.Join(home, ".local", "state", "clipbeam", "clipbeam.log"),
	}
	got := map[string]string{
		"SaveDir": p.SaveDir, "AgentInbox": p.AgentInbox, "LastPath": p.LastPath,
		"Recents": p.Recents, "Config": p.Config, "Aliases": p.Aliases,
		"Token": p.Token, "Log": p.Log,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("linuxPaths %s = %q, want %q", k, got[k], w)
		}
	}
}

// TestDarwinPathsLayout asserts the macOS ~/.clipbeam parity layout and the empty
// Token (Keychain holds it) (PLAN §4).
func TestDarwinPathsLayout(t *testing.T) {
	home := t.TempDir()
	p := darwinPaths(home)
	if p.SaveDir != filepath.Join(home, "Downloads", "ClipBeam") {
		t.Errorf("darwin SaveDir = %q", p.SaveDir)
	}
	if p.Config != filepath.Join(home, ".clipbeam", "config.json") {
		t.Errorf("darwin Config = %q", p.Config)
	}
	if p.Aliases != filepath.Join(home, ".clipbeam", "cli-aliases.json") {
		t.Errorf("darwin Aliases = %q", p.Aliases)
	}
	if p.Token != "" {
		t.Errorf("darwin Token must be empty (Keychain holds it), got %q", p.Token)
	}
}

// TestLoadMissingReturnsDefaults asserts a missing config.json yields DefaultConfig
// with no implicit write (the file must not exist afterward).
func TestLoadMissingReturnsDefaults(t *testing.T) {
	withHome(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 8787 || c.MaxBytes != DefaultConfig().MaxBytes || c.LongTextThreshold != 8192 {
		t.Errorf("defaults not applied: %+v", c)
	}
	p, _ := Resolve()
	if _, err := os.Stat(p.Config); !os.IsNotExist(err) {
		t.Errorf("Load must NOT create config.json on a missing read (stat err=%v)", err)
	}
}

// TestLoadPartialKeepsDefaults asserts a partial config.json keeps defaults for any
// absent key (a partial file must not zero unspecified fields).
func TestLoadPartialKeepsDefaults(t *testing.T) {
	withHome(t)
	p, _ := Resolve()
	if err := os.MkdirAll(filepath.Dir(p.Config), 0o700); err != nil {
		t.Fatal(err)
	}
	// Only port is set; everything else must fall back to defaults.
	if err := os.WriteFile(p.Config, []byte(`{"port":9999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 9999 {
		t.Errorf("port = %d, want 9999", c.Port)
	}
	if c.MaxBytes != DefaultConfig().MaxBytes || c.LongTextThreshold != 8192 {
		t.Errorf("absent keys must keep defaults: %+v", c)
	}
}

// TestLoadMacOSToleratesSpaceColon asserts the Go reader tolerates the app's
// space-before-colon / escaped-slash byte-shape (PLAN §4.1c). encoding/json normalizes
// both, so this holds on every OS.
func TestLoadMacOSToleratesSpaceColon(t *testing.T) {
	withHome(t)
	p, _ := Resolve()
	if err := os.MkdirAll(filepath.Dir(p.Config), 0o700); err != nil {
		t.Fatal(err)
	}
	appShape := "{\n  \"port\" : 8787,\n  \"saveDir\" : \"~\\/Downloads\\/ClipBeam\"\n}\n"
	if err := os.WriteFile(p.Config, []byte(appShape), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("must tolerate app byte-shape: %v", err)
	}
	if c.Port != 8787 || c.SaveDir != "~/Downloads/ClipBeam" {
		t.Errorf("app-shape decode = %+v", c)
	}
}

// TestSaveMacOSRefuses asserts Save refuses on macOS (the app owns config.json,
// PLAN §4.1a), and on Linux writes a 0600 round-trippable file.
func TestSaveMacOSRefuses(t *testing.T) {
	withHome(t)
	c := DefaultConfig()
	c.Port = 12345
	err := Save(c)
	if runtime.GOOS == "darwin" {
		if err != ErrMacOSConfigReadOnly {
			t.Fatalf("macOS Save must return ErrMacOSConfigReadOnly, got %v", err)
		}
		return
	}
	// Linux: write succeeds, round-trips, and lands 0600.
	if err != nil {
		t.Fatalf("linux Save failed: %v", err)
	}
	p, _ := Resolve()
	info, err := os.Stat(p.Config)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.json perm = %o, want 600", perm)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != 12345 {
		t.Errorf("round-trip port = %d, want 12345", got.Port)
	}
	// Linux writer uses space-less colons (plain encoding/json).
	data, _ := os.ReadFile(p.Config)
	if strings.Contains(string(data), `"port" :`) {
		t.Errorf("Linux writer must use space-less colons, got: %s", data)
	}
}

// TestResolvedSaveDirTilde asserts tilde expansion + dir self-heal at 0700 (PLAN §4).
func TestResolvedSaveDirTilde(t *testing.T) {
	home := withHome(t)
	c := DefaultConfig()
	c.SaveDir = "~/beam-saves"
	got, err := ResolvedSaveDir(c)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "beam-saves")
	if got != want {
		t.Errorf("ResolvedSaveDir = %q, want %q", got, want)
	}
	info, err := os.Stat(got)
	if err != nil || !info.IsDir() {
		t.Fatalf("save dir not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("save dir perm = %o, want 700", perm)
	}
}

// TestResolvedSaveDirDefault asserts an empty saveDir falls back to the per-OS Paths
// default and creates it.
func TestResolvedSaveDirDefault(t *testing.T) {
	withHome(t)
	c := DefaultConfig() // SaveDir == ""
	got, err := ResolvedSaveDir(c)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := Resolve()
	if got != p.SaveDir {
		t.Errorf("empty saveDir must use Paths.SaveDir: got %q, want %q", got, p.SaveDir)
	}
	if info, err := os.Stat(got); err != nil || !info.IsDir() {
		t.Fatalf("default save dir not created: %v", err)
	}
}

// TestExpandTilde covers the tilde-expansion edge cases.
func TestExpandTilde(t *testing.T) {
	home := withHome(t)
	cases := map[string]string{
		"~":           home,
		"~/x":         filepath.Join(home, "x"),
		"/abs/path":   "/abs/path",
		"relative/x":  "relative/x", // not expanded; only leading ~ is
		"~user/notme": "~user/notme",
	}
	for in, want := range cases {
		got, err := expandTilde(in)
		if err != nil {
			t.Fatalf("expandTilde(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("expandTilde(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDefaultConfigByteShape guards the default JSON shape so a Linux write does not
// drift the keys the protocol/shim read.
func TestDefaultConfigByteShape(t *testing.T) {
	b, err := json.Marshal(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"port", "saveDir", "maxBytes", "saveTextToDisk", "longTextThreshold", "notify"} {
		if _, ok := m[k]; !ok {
			t.Errorf("default config missing key %q", k)
		}
	}
	// linuxClipboard is omitempty and false by default → must be absent (keeps the
	// macOS byte-shape unchanged, PLAN §7.3).
	if _, ok := m["linuxClipboard"]; ok {
		t.Errorf("linuxClipboard must be omitted when false")
	}
}
