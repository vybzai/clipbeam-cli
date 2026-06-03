package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Alias is one saved transport target written by `clipbeam setup` (PLAN §5.6). It
// lives in the CLI-owned alias store, NEVER in the app's config.json (PLAN §4.3).
// The token is never stored here (Keychain on macOS, the 0600 file otherwise).
type Alias struct {
	Name           string `json:"name"`
	Transport      string `json:"transport"` // "ssh" | "tailscale"
	SSHUser        string `json:"sshUser,omitempty"`
	SSHHost        string `json:"sshHost,omitempty"`
	SSHPort        int    `json:"sshPort,omitempty"`
	SSHConfigAlias string `json:"sshConfigAlias,omitempty"` // optional ~/.ssh/config Host name
	RemoteBinPath  string `json:"remoteBinPath,omitempty"`  // ABSOLUTE path setup recorded (§5.1)
	Serve          string `json:"serve,omitempty"`          // "exec" | "socket" | "tcp" | "tailscale"
	PeerIP         string `json:"peerIP,omitempty"`         // tailscale; empty otherwise
	Default        bool   `json:"default,omitempty"`
}

// AliasStore is the on-disk CLI-owned alias store (PLAN §4.3/§5.6). Its JSON shape:
//
//	{ "aliases": [ {Alias…} ], "defaultAlias": "<name>" }
type AliasStore struct {
	Aliases      []Alias `json:"aliases"`
	DefaultAlias string  `json:"defaultAlias"`
}

// LoadAliases reads the per-OS alias store (PLAN §4.3/§5.6), returning an empty store
// if the file is absent. A corrupt file is a hard error (a setup verb can choose to
// overwrite, but a data verb must not silently lose a saved default). The store lives
// in the CLI-owned location (~/.clipbeam/cli-aliases.json on macOS,
// $XDG_CONFIG_HOME/clipbeam/aliases.json on Linux) — never in the app's config.json.
func LoadAliases(p Paths) (AliasStore, error) {
	data, err := os.ReadFile(p.Aliases)
	if err != nil {
		if os.IsNotExist(err) {
			return AliasStore{}, nil
		}
		return AliasStore{}, fmt.Errorf("clipbeam: read alias store: %w", err)
	}
	var s AliasStore
	if err := json.Unmarshal(data, &s); err != nil {
		return AliasStore{}, fmt.Errorf("clipbeam: decode alias store: %w", err)
	}
	return s, nil
}

// SaveAliases persists the alias store at 0600 atomically (parent dir 0700). It writes
// the dedicated CLI-owned file on every OS, so it never touches the app's config.json
// even on macOS (PLAN §4.3).
func SaveAliases(p Paths, s AliasStore) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("clipbeam: encode alias store: %w", err)
	}
	return writeFileAtomic0600(p.Aliases, data)
}

// Lookup returns the alias with the given name, or (Alias{}, false). An empty name
// returns the default alias (PLAN §5.5: a verb with no target uses defaultAlias).
func (s AliasStore) Lookup(name string) (Alias, bool) {
	target := name
	if target == "" {
		target = s.DefaultAlias
	}
	for _, a := range s.Aliases {
		if a.Name == target {
			return a, true
		}
	}
	return Alias{}, false
}
