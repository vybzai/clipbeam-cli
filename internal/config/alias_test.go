package config

import (
	"os"
	"testing"
)

// TestAliasStoreRoundTrip asserts SaveAliases→LoadAliases round-trips the store and
// lands the file 0600 (PLAN §4.3/§5.6).
func TestAliasStoreRoundTrip(t *testing.T) {
	withHome(t)
	p, _ := Resolve()
	in := AliasStore{
		Aliases: []Alias{
			{Name: "my-box", Transport: "ssh", SSHUser: "root", SSHHost: "1.2.3.4", SSHPort: 22,
				RemoteBinPath: "/root/.local/bin/clipbeam", Serve: "exec", Default: true},
			{Name: "tailbox", Transport: "tailscale", PeerIP: "100.64.0.9"},
		},
		DefaultAlias: "my-box",
	}
	if err := SaveAliases(p, in); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p.Aliases)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("alias store perm = %o, want 600", perm)
	}
	got, err := LoadAliases(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultAlias != "my-box" || len(got.Aliases) != 2 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Aliases[0].RemoteBinPath != "/root/.local/bin/clipbeam" || !got.Aliases[0].Default {
		t.Errorf("alias[0] = %+v", got.Aliases[0])
	}
}

// TestLoadAliasesMissing asserts an absent file yields an empty store, not an error.
func TestLoadAliasesMissing(t *testing.T) {
	withHome(t)
	p, _ := Resolve()
	s, err := LoadAliases(p)
	if err != nil {
		t.Fatalf("missing alias store must not error: %v", err)
	}
	if len(s.Aliases) != 0 || s.DefaultAlias != "" {
		t.Errorf("missing store must be empty, got %+v", s)
	}
}

// TestAliasLookupDefault asserts an empty name resolves to the default alias (PLAN
// §5.5: a verb with no target uses defaultAlias), and an unknown name misses.
func TestAliasLookupDefault(t *testing.T) {
	s := AliasStore{
		Aliases:      []Alias{{Name: "a"}, {Name: "b"}},
		DefaultAlias: "b",
	}
	if a, ok := s.Lookup(""); !ok || a.Name != "b" {
		t.Errorf("empty name must resolve default 'b', got %+v ok=%v", a, ok)
	}
	if a, ok := s.Lookup("a"); !ok || a.Name != "a" {
		t.Errorf("explicit name 'a' must resolve, got %+v ok=%v", a, ok)
	}
	if _, ok := s.Lookup("missing"); ok {
		t.Errorf("unknown name must miss")
	}
}

// TestLookupSpecHostFallback asserts LookupSpec resolves a saved SSH alias when the data
// verb re-uses the SAME literal user@host[:port] / bare host spec that `clipbeam setup`
// was given — so the recorded absolute remoteBinPath is used instead of a bare
// `clipbeam ingest` that fails 127 under a minimal non-login SSH-exec PATH (fix [D]
// completeness). The alias is keyed by the bare host (aliasName strips user@/:port), so a
// later `send file root@host` must still find it.
func TestLookupSpecHostFallback(t *testing.T) {
	s := AliasStore{
		Aliases: []Alias{
			{Name: "box", Transport: "ssh", SSHUser: "root", SSHHost: "box", SSHPort: 22,
				RemoteBinPath: "/root/.local/bin/clipbeam", Default: true},
			{Name: "tailbox", Transport: "tailscale", PeerIP: "100.64.0.9"},
		},
		DefaultAlias: "box",
	}

	cases := []struct {
		name      string
		spec      string
		wantOK    bool
		wantAlias string
	}{
		{"exact name", "box", true, "box"},
		{"empty uses default", "", true, "box"},
		{"literal user@host (the setup spec)", "root@box", true, "box"},
		{"bare host", "box", true, "box"}, // same as the name here; explicit for intent
		{"user@host:port matching", "root@box:22", true, "box"},
		{"wrong user misses", "deploy@box", false, ""},
		{"wrong port misses", "root@box:2222", false, ""},
		{"unknown host misses", "root@other", false, ""},
		{"tailnet IP does not match an ssh alias host", "100.64.0.9", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, ok := s.LookupSpec(c.spec)
			if ok != c.wantOK {
				t.Fatalf("LookupSpec(%q) ok=%v, want %v (alias %+v)", c.spec, ok, c.wantOK, a)
			}
			if ok {
				if a.Name != c.wantAlias {
					t.Errorf("LookupSpec(%q) alias=%q, want %q", c.spec, a.Name, c.wantAlias)
				}
				if a.Transport == "ssh" && a.RemoteBinPath == "" {
					t.Errorf("LookupSpec(%q) resolved ssh alias with empty RemoteBinPath — the [D] fix needs the recorded abs path", c.spec)
				}
			}
		})
	}
}

// TestParseSSHSpec asserts the literal-spec parser splits user@host[:port] correctly and
// leaves an IPv6 colon run (more than one ':') untouched so only a single trailing :port
// is stripped.
func TestParseSSHSpec(t *testing.T) {
	cases := []struct {
		spec       string
		user, host string
		port       int
	}{
		{"host", "", "host", 0},
		{"root@host", "root", "host", 0},
		{"root@host:22", "root", "host", 22},
		{"host:2222", "", "host", 2222},
		{"root@host:0", "root", "host:0", 0},               // invalid port → not stripped
		{"root@host:notaport", "root", "host:notaport", 0}, // non-numeric → not stripped
		{"::1", "", "::1", 0},                              // IPv6 run left intact
	}
	for _, c := range cases {
		u, h, p := parseSSHSpec(c.spec)
		if u != c.user || h != c.host || p != c.port {
			t.Errorf("parseSSHSpec(%q) = (%q,%q,%d), want (%q,%q,%d)", c.spec, u, h, p, c.user, c.host, c.port)
		}
	}
}

// TestLoadAliasesCorrupt asserts a corrupt file is a hard error (a data verb must not
// silently lose a saved default).
func TestLoadAliasesCorrupt(t *testing.T) {
	withHome(t)
	p, _ := Resolve()
	if err := writeFileAtomic0600(p.Aliases, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAliases(p); err == nil {
		t.Errorf("corrupt alias store must error")
	}
}
