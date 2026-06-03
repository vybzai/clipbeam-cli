package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Paths resolves the per-OS file layout (PLAN §4). It is the single place that
// branches on GOOS: Linux uses XDG ($XDG_*_HOME honored only if absolute, else
// ~/.local|.config defaults); macOS uses ~/.clipbeam (and ~/Downloads/ClipBeam) for
// parity with ClipBeam.app and its shim. Every other module gets its paths from here.
type Paths struct {
	// SaveDir is the clipboard-channel save dir (dir 0700).
	SaveDir string
	// AgentInbox is the agent-channel inbox dir (dir 0700, files 0600).
	AgentInbox string
	// LastPath is the last_path file.
	LastPath string
	// Recents is recents.json (0600).
	Recents string
	// Config is config.json (0600).
	Config string
	// Aliases is the CLI-owned alias store (0600), separate from the app config.
	Aliases string
	// Token is the 0600 Linux token file (empty on macOS, where the Keychain holds it).
	Token string
	// Log is the redacted, size-rotated log (0600).
	Log string
}

// errNoHome is returned when the user home directory cannot be resolved.
var errNoHome = errors.New("clipbeam: cannot resolve home directory")

// Resolve computes the per-OS Paths. It does not create any directories — callers
// create dirs 0700 and files 0600 on demand (PLAN §4: self-heal + re-chmod on load).
func Resolve() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return Paths{}, errNoHome
	}
	if runtime.GOOS == "darwin" {
		return darwinPaths(home), nil
	}
	return linuxPaths(home), nil
}

// darwinPaths returns the macOS layout: ~/.clipbeam for state/config, and
// ~/Downloads/ClipBeam for clipboard saves (PLAN §4). The token lives in the
// Keychain on macOS, so Token is empty.
func darwinPaths(home string) Paths {
	dot := filepath.Join(home, ".clipbeam")
	return Paths{
		SaveDir:    filepath.Join(home, "Downloads", "ClipBeam"),
		AgentInbox: filepath.Join(dot, "agent-inbox"),
		LastPath:   filepath.Join(dot, "last_path"),
		Recents:    filepath.Join(dot, "recents.json"),
		Config:     filepath.Join(dot, "config.json"),
		Aliases:    filepath.Join(dot, "cli-aliases.json"),
		Token:      "",
		Log:        filepath.Join(dot, "clipbeam.log"),
	}
}

// linuxPaths returns the XDG-idiomatic layout (PLAN §4). $XDG_*_HOME is honored only
// when absolute; otherwise the ~/.local|.config defaults apply.
func linuxPaths(home string) Paths {
	dataHome := xdgDir("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	stateHome := xdgDir("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	configHome := xdgDir("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	stateDir := filepath.Join(stateHome, "clipbeam")
	configDir := filepath.Join(configHome, "clipbeam")
	return Paths{
		SaveDir:    filepath.Join(dataHome, "clipbeam"),
		AgentInbox: filepath.Join(stateDir, "agent-inbox"),
		LastPath:   filepath.Join(stateDir, "last_path"),
		Recents:    filepath.Join(stateDir, "recents.json"),
		Config:     filepath.Join(configDir, "config.json"),
		Aliases:    filepath.Join(configDir, "aliases.json"),
		Token:      filepath.Join(configDir, "token"),
		Log:        filepath.Join(stateDir, "clipbeam.log"),
	}
}

// xdgDir returns $env if it is set AND absolute (begins with "/"), else fallback.
// Never returns a relative path so state dirs are never created in cwd (PLAN §4
// XDG validation rule).
func xdgDir(env, fallback string) string {
	v := os.Getenv(env)
	if v != "" && strings.HasPrefix(v, "/") {
		return v
	}
	return fallback
}
