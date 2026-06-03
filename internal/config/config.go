// Package config owns the per-OS file layout (PLAN §4 — the single table every
// other module defers to), the Config struct, config read/write (with the macOS
// no-write rule §4.1a), the token-storage interface (§6.4), and the alias store
// (§4.3/§5.6). Linux uses XDG; macOS uses ~/.clipbeam parity with ClipBeam.app.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// ErrMacOSConfigReadOnly is returned by Save on macOS: the app owns config.json and
// a Go rewrite that drops the non-optional hotkey/peers[].name would make Swift's
// strict JSONDecoder throw and silently reset the peer IP (PLAN §4.1a).
var ErrMacOSConfigReadOnly = errors.New("clipbeam: config.json is owned by ClipBeam.app on macOS and must not be rewritten by the CLI (PLAN §4.1a)")

// Peer mirrors Swift Config.Peer. name is non-optional and present (load-bearing
// for the macOS strict-decode no-write rule, PLAN §4.1a).
type Peer struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	Name string `json:"name"`
}

// HotkeyConfig mirrors Swift Config.HotkeyConfig. It is present and non-optional in
// the app's config.json; the Go reader preserves it verbatim and NEVER drops it on
// macOS (PLAN §4.1a).
type HotkeyConfig struct {
	KeyCode   uint32   `json:"keyCode"`
	Modifiers []string `json:"modifiers"`
}

// Config mirrors Swift Config (Config.swift). saveDir is a tilde path expanded only
// at use; maxBytes is the DECODED payload cap. On macOS Go reads but never rewrites
// this file (the app owns it, PLAN §4.1a). On Linux Go owns it fully.
//
// Raw preserves every unknown key verbatim so a macOS rewrite (only if ever forced)
// round-trips the app's hotkey/peers[].name + any future keys (PLAN §4.1a).
type Config struct {
	Port              uint16       `json:"port"`
	Peers             []Peer       `json:"peers"`
	DefaultPeerID     string       `json:"defaultPeerId"`
	SaveDir           string       `json:"saveDir"`
	MaxBytes          int          `json:"maxBytes"`
	Hotkey            HotkeyConfig `json:"hotkey"`
	SaveTextToDisk    bool         `json:"saveTextToDisk"`
	LongTextThreshold int          `json:"longTextThreshold"`
	Notify            bool         `json:"notify"`

	// LinuxClipboard is a CLI-owned persistent opt-in for the best-effort
	// X/Wayland clipboard set on Linux (PLAN §7.3). Off by default; absent in the
	// app's config.json (omitempty keeps the macOS byte-shape unchanged).
	LinuxClipboard bool `json:"linuxClipboard,omitempty"`
}

// DefaultConfig returns the defaults used when no config.json exists. On Linux these
// back the XDG-resolved saveDir; the app's hardcoded peer IP is NOT reproduced here
// (a fresh CLI config has no peer until setup pairs one).
func DefaultConfig() Config {
	return Config{
		Port:              8787,
		Peers:             nil,
		DefaultPeerID:     "",
		SaveDir:           "", // resolved per-OS via Paths (PLAN §4); empty ⇒ default
		MaxBytes:          wire.DefaultMaxBytes,
		Hotkey:            HotkeyConfig{KeyCode: 9, Modifiers: []string{"control", "option", "command"}},
		SaveTextToDisk:    false,
		LongTextThreshold: 8192,
		Notify:            true,
	}
}

// Load reads the per-OS config.json, applying defaults for any absent file. On macOS
// it parses the app's space-before-colon / escaped-slash byte-shape tolerantly
// (Go's encoding/json normalizes both transparently) and NEVER writes back (PLAN
// §4.1a). A genuinely corrupt file is a hard error; a missing file yields
// DefaultConfig (no implicit write — unlike the Swift app, the CLI does not persist
// defaults on first read).
func Load() (Config, error) {
	p, err := Resolve()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(p.Config)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return Config{}, fmt.Errorf("clipbeam: read config.json: %w", err)
	}
	// Start from defaults so any key absent in the file keeps its default value
	// (a partial config.json must not zero unspecified fields).
	c := DefaultConfig()
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("clipbeam: decode config.json: %w", err)
	}
	return c, nil
}

// Save persists config.json. On macOS it REFUSES (ErrMacOSConfigReadOnly): the app
// owns the file and a Go rewrite that drops the non-optional hotkey/peers[].name
// makes Swift's strict JSONDecoder throw and reset the peer IP (PLAN §4.1a). On Linux
// Go owns the file fully and writes pretty-printed encoding/json (space-less colons)
// at 0600 atomically, parent dir 0700.
func Save(c Config) error {
	if runtime.GOOS == "darwin" {
		return ErrMacOSConfigReadOnly
	}
	p, err := Resolve()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("clipbeam: encode config.json: %w", err)
	}
	return writeFileAtomic0600(p.Config, data)
}

// ResolvedSaveDir expands the tilde saveDir (or the per-OS default from Paths when
// empty) to an absolute path, ensuring the directory exists 0700 and self-heals if it
// vanished (PLAN §4, mirrors Swift ConfigStore.resolvedSaveDir).
func ResolvedSaveDir(c Config) (string, error) {
	p, err := Resolve()
	if err != nil {
		return "", err
	}
	dir := c.SaveDir
	if dir == "" {
		dir = p.SaveDir
	} else {
		dir, err = expandTilde(dir)
		if err != nil {
			return "", err
		}
	}
	if err := ensureDir0700(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// expandTilde expands a leading "~" or "~/" to the user home directory. A bare
// absolute path is returned unchanged. Only a leading "~" is expanded (matching
// NSString.expandingTildeInPath for the saveDir case); "~user" is not supported.
func expandTilde(path string) (string, error) {
	if path == "~" || (len(path) >= 2 && path[0] == '~' && path[1] == '/') {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", errNoHome
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}
