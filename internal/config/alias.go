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

// LookupSpec resolves a data-verb target spec to a saved alias (fix [D] completeness).
// It first tries the exact alias NAME (the contract Lookup guarantees: empty ⇒ default,
// else the named alias). When that misses, it falls back to matching the spec parsed as a
// literal user@host[:port] / bare host against a saved SSH alias's recorded host (and
// user/port when the spec carries them) — so a user who ran `clipbeam setup root@box` and
// then `clipbeam send file root@box` (the SAME spec) resolves to that alias and uses its
// recorded absolute remoteBinPath, instead of falling through to a bare `clipbeam ingest`
// that fails 127 under a minimal non-login SSH-exec PATH (PLAN §5.1).
//
// Only SSH aliases participate in the host fallback (a tailscale alias has no SSHHost), so
// a literal tailnet 100.x target is unaffected and still routes via the tailscale probe.
func (s AliasStore) LookupSpec(spec string) (Alias, bool) {
	if a, ok := s.Lookup(spec); ok {
		return a, true
	}
	if spec == "" {
		return Alias{}, false
	}
	user, host, port := parseSSHSpec(spec)
	if host == "" {
		return Alias{}, false
	}
	for _, a := range s.Aliases {
		if a.Transport != "ssh" || a.SSHHost == "" {
			continue
		}
		if a.SSHHost != host {
			continue
		}
		// A user/port in the spec must match the alias when present; an omitted user/port in
		// the spec matches any (the host alone is the stable key after setup).
		if user != "" && a.SSHUser != "" && user != a.SSHUser {
			continue
		}
		if port != 0 && a.SSHPort != 0 && port != a.SSHPort {
			continue
		}
		return a, true
	}
	return Alias{}, false
}

// parseSSHSpec splits a literal user@host[:port] (or bare host) into its parts. A missing
// user is "" and a missing/invalid port is 0. An IPv6 colon run (more than one ':') is left
// in host untouched so only a single trailing :port is stripped (mirrors aliasName).
func parseSSHSpec(spec string) (user, host string, port int) {
	s := spec
	if at := lastIndexByte(s, '@'); at >= 0 {
		user = s[:at]
		s = s[at+1:]
	}
	if i := lastIndexByte(s, ':'); i >= 0 && countByte(s, ':') == 1 {
		if p := atoiPort(s[i+1:]); p > 0 {
			port = p
			s = s[:i]
		}
	}
	host = s
	return user, host, port
}

// lastIndexByte returns the last index of b in s, or -1.
func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// countByte counts occurrences of b in s.
func countByte(s string, b byte) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			n++
		}
	}
	return n
}

// atoiPort parses a decimal port (1..65535), returning 0 for anything invalid.
func atoiPort(s string) int {
	if s == "" || len(s) > 5 {
		return 0
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0
		}
		n = n*10 + int(s[i]-'0')
	}
	if n < 1 || n > 65535 {
		return 0
	}
	return n
}
