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
