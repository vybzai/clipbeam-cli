package sshx

import (
	"errors"
	"testing"
)

// fakeResolver is a configResolver backed by in-memory maps so ResolveTarget logic is
// unit-tested without a real ~/.ssh/config on disk.
type fakeResolver struct {
	single map[string]map[string]string   // alias -> key -> value
	multi  map[string]map[string][]string // alias -> key -> values
}

func (f fakeResolver) Get(alias, key string) string {
	if m, ok := f.single[alias]; ok {
		return m[key]
	}
	return ""
}

func (f fakeResolver) GetAll(alias, key string) []string {
	if m, ok := f.multi[alias]; ok {
		return m[key]
	}
	return nil
}

// TestResolveUserAtHostPort asserts a literal user@host:port spec parses without any
// ssh_config rewrite (PLAN §5.5 step 3).
func TestResolveUserAtHostPort(t *testing.T) {
	tgt, err := resolveTargetWith(fakeResolver{}, "root@1.2.3.4:2222")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.User != "root" || tgt.Host != "1.2.3.4" || tgt.Port != 2222 {
		t.Errorf("got %+v, want root@1.2.3.4:2222", tgt)
	}
}

// TestResolveBareHostDefaultsPort22 asserts a bare host with no ssh_config Port
// defaults to 22 (PLAN §5.4).
func TestResolveBareHostDefaultsPort22(t *testing.T) {
	tgt, err := resolveTargetWith(fakeResolver{}, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Host != "example.com" || tgt.Port != 22 || tgt.User != "" {
		t.Errorf("got %+v, want example.com:22 with empty user", tgt)
	}
}

// TestResolveSSHConfigAlias asserts a ~/.ssh/config Host alias rewrites HostName/User/
// Port and records the ConfigAlias (PLAN §5.4).
func TestResolveSSHConfigAlias(t *testing.T) {
	r := fakeResolver{single: map[string]map[string]string{
		"my-box": {"HostName": "10.0.0.5", "User": "deploy", "Port": "2200"},
	}}
	tgt, err := resolveTargetWith(r, "my-box")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Host != "10.0.0.5" || tgt.User != "deploy" || tgt.Port != 2200 {
		t.Errorf("alias rewrite got %+v", tgt)
	}
	if tgt.ConfigAlias != "my-box" {
		t.Errorf("ConfigAlias = %q, want my-box", tgt.ConfigAlias)
	}
}

// TestResolveExplicitUserOverridesConfig asserts an explicit user@ wins over the
// ssh_config User (PLAN §5.4 auth precedence: explicit spec is authoritative).
func TestResolveExplicitUserOverridesConfig(t *testing.T) {
	r := fakeResolver{single: map[string]map[string]string{
		"my-box": {"HostName": "10.0.0.5", "User": "deploy"},
	}}
	tgt, err := resolveTargetWith(r, "admin@my-box")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.User != "admin" {
		t.Errorf("explicit user not honored: got %q", tgt.User)
	}
}

// TestResolveProxyJumpUnsupported asserts a configured ProxyJump fails with the
// SPECIFIC ErrProxyJumpUnsupported sentinel, never an opaque error (PLAN §5.4 non-goal).
func TestResolveProxyJumpUnsupported(t *testing.T) {
	r := fakeResolver{single: map[string]map[string]string{
		"jump-box": {"HostName": "10.0.0.9", "ProxyJump": "bastion"},
	}}
	_, err := resolveTargetWith(r, "jump-box")
	if !errors.Is(err, ErrProxyJumpUnsupported) {
		t.Errorf("ProxyJump err = %v, want ErrProxyJumpUnsupported", err)
	}
}

// TestResolveProxyCommandUnsupported asserts a configured ProxyCommand also fails with
// ErrProxyJumpUnsupported (clipbeam dials in-process, never shells a proxy, §5.4).
func TestResolveProxyCommandUnsupported(t *testing.T) {
	r := fakeResolver{single: map[string]map[string]string{
		"pc-box": {"HostName": "10.0.0.9", "ProxyCommand": "nc %h %p"},
	}}
	_, err := resolveTargetWith(r, "pc-box")
	if !errors.Is(err, ErrProxyJumpUnsupported) {
		t.Errorf("ProxyCommand err = %v, want ErrProxyJumpUnsupported", err)
	}
}

// TestResolveProxyJumpNoneIsAllowed asserts an explicit `ProxyJump none` is NOT treated
// as an unsupported jump (ssh's documented way to clear an inherited ProxyJump).
func TestResolveProxyJumpNoneIsAllowed(t *testing.T) {
	r := fakeResolver{single: map[string]map[string]string{
		"plain": {"HostName": "10.0.0.1", "ProxyJump": "none"},
	}}
	if _, err := resolveTargetWith(r, "plain"); err != nil {
		t.Errorf("ProxyJump none must be allowed, got %v", err)
	}
}

// TestResolveEmptySpec asserts an empty spec is an error (a verb with no target uses
// the alias store, not ResolveTarget — §5.5).
func TestResolveEmptySpec(t *testing.T) {
	if _, err := resolveTargetWith(fakeResolver{}, "   "); err == nil {
		t.Errorf("empty spec must error")
	}
}

// TestParseSpecIPv6 asserts a bracketed IPv6 literal with a port parses, and a bare
// IPv6 (multiple colons, no brackets) is a host with no port.
func TestParseSpecIPv6(t *testing.T) {
	u, h, p, err := parseSpec("user@[fd7a:115c:a1e0::1]:2022")
	if err != nil || u != "user" || h != "fd7a:115c:a1e0::1" || p != 2022 {
		t.Errorf("bracketed IPv6: u=%q h=%q p=%d err=%v", u, h, p, err)
	}
	_, h2, p2, err2 := parseSpec("fd7a:115c:a1e0::1")
	if err2 != nil || h2 != "fd7a:115c:a1e0::1" || p2 != 0 {
		t.Errorf("bare IPv6: h=%q p=%d err=%v", h2, p2, err2)
	}
}

// TestParseSpecBadPort asserts a non-numeric port is rejected.
func TestParseSpecBadPort(t *testing.T) {
	if _, _, _, err := parseSpec("host:notaport"); err == nil {
		t.Errorf("non-numeric port must error")
	}
}
