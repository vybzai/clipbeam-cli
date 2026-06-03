package cli

import (
	"encoding/base64"
	"testing"

	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/ingest"
	"github.com/vybzai/clipbeam-cli/internal/sshx"
)

// TestUnameToGOOS asserts the remote OS detection maps the v1 targets and rejects
// others (PLAN §9.5 step 2; Windows is out of v1 scope).
func TestUnameToGOOS(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"Linux", "linux", true},
		{"Darwin", "darwin", true},
		{"linux", "linux", true},
		{"MINGW64_NT", "", false},
		{"", "", false},
	} {
		got, ok := unameToGOOS(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("unameToGOOS(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestUnameToGOARCH asserts the arch detection follows the install.sh rules (PLAN §9.1).
func TestUnameToGOARCH(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"x86_64", "amd64", true},
		{"amd64", "amd64", true},
		{"arm64", "arm64", true},
		{"aarch64", "arm64", true},
		{"i386", "", false},
	} {
		got, ok := unameToGOARCH(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("unameToGOARCH(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestReplaceAliasDefaultClears asserts recording an alias clears the Default flag on
// every other entry so exactly one alias is the default (PLAN §5.6).
func TestReplaceAliasDefaultClears(t *testing.T) {
	existing := []config.Alias{
		{Name: "old", Default: true},
		{Name: "other"},
	}
	fresh := config.Alias{Name: "new", Default: true}
	got := replaceAlias(existing, fresh)
	if len(got) != 3 {
		t.Fatalf("got %d aliases, want 3", len(got))
	}
	defaults := 0
	for _, a := range got {
		if a.Default {
			defaults++
			if a.Name != "new" {
				t.Errorf("default is %q, want new", a.Name)
			}
		}
	}
	if defaults != 1 {
		t.Errorf("got %d defaults, want exactly 1", defaults)
	}
}

// TestReplaceAliasReplacesSameName asserts a same-named alias is replaced in place, not
// duplicated (idempotent re-setup, PLAN §9.5).
func TestReplaceAliasReplacesSameName(t *testing.T) {
	existing := []config.Alias{{Name: "box", RemoteBinPath: "/old/path"}}
	got := replaceAlias(existing, config.Alias{Name: "box", RemoteBinPath: "/new/path", Default: true})
	if len(got) != 1 {
		t.Fatalf("got %d aliases, want 1 (replaced)", len(got))
	}
	if got[0].RemoteBinPath != "/new/path" {
		t.Errorf("RemoteBinPath = %q, want /new/path", got[0].RemoteBinPath)
	}
}

// TestAliasName derives the alias name from the spec / config alias.
func TestAliasName(t *testing.T) {
	for _, tc := range []struct {
		t    sshx.Target
		spec string
		want string
	}{
		{sshx.Target{ConfigAlias: "my-box", Host: "10.0.0.1"}, "my-box", "my-box"},
		{sshx.Target{Host: "1.2.3.4"}, "root@1.2.3.4:22", "1.2.3.4"},
		{sshx.Target{Host: "h"}, "root@h", "h"},
		{sshx.Target{Host: "host"}, "host", "host"},
	} {
		if got := aliasName(tc.t, tc.spec); got != tc.want {
			t.Errorf("aliasName(%+v, %q) = %q, want %q", tc.t, tc.spec, got, tc.want)
		}
	}
}

// TestRemoteHomeFor asserts the conventional remote home fallback (PLAN §5.1 absolute
// path requirement).
func TestRemoteHomeFor(t *testing.T) {
	if remoteHomeFor("root") != "/root" || remoteHomeFor("") != "/root" {
		t.Errorf("root/empty home wrong")
	}
	if remoteHomeFor("deploy") != "/home/deploy" {
		t.Errorf("named-user home wrong")
	}
}

// TestPosixDir asserts the POSIX dirname used for remote paths (never the local
// separator).
func TestPosixDir(t *testing.T) {
	if got := posixDir("/root/.local/bin/clipbeam"); got != "/root/.local/bin" {
		t.Errorf("posixDir = %q", got)
	}
	if got := posixDir("/clipbeam"); got != "/" {
		t.Errorf("posixDir root = %q", got)
	}
}

// TestBuildEnvelopeImageBase64 asserts an image item is encoded with UNWRAPPED standard
// base64 in bytesB64, the name preserved, and no whitespace (the Mac receiver rejects
// any whitespace, PLAN §3.6).
func TestBuildEnvelopeImageBase64(t *testing.T) {
	items := []sshx.CB01Item{{Kind: sshx.KindByte(ingest.KindImage), Name: "shot.png", Payload: []byte("\x89PNG\x00\x01rawbytes")}}
	env, err := buildEnvelope(ingest.ChannelClipboard, items)
	if err != nil {
		t.Fatal(err)
	}
	if env.Version != 1 {
		t.Errorf("version = %d, want 1", env.Version)
	}
	if env.Channel != nil {
		t.Errorf("clipboard channel must be nil (the wire collapse), got %v", *env.Channel)
	}
	if len(env.Items) != 1 || env.Items[0].BytesB64 == nil {
		t.Fatalf("envelope items wrong: %+v", env.Items)
	}
	b64 := *env.Items[0].BytesB64
	decoded, derr := base64.StdEncoding.DecodeString(b64)
	if derr != nil {
		t.Fatalf("bytesB64 not std-base64: %v", derr)
	}
	if string(decoded) != "\x89PNG\x00\x01rawbytes" {
		t.Errorf("decoded payload = %q", decoded)
	}
	if env.Items[0].Name == nil || *env.Items[0].Name != "shot.png" {
		t.Errorf("name not preserved")
	}
}

// TestBuildEnvelopeAgentText asserts an agent text item sets the agent channel and the
// text field (not bytesB64).
func TestBuildEnvelopeAgentText(t *testing.T) {
	items := []sshx.CB01Item{{Kind: sshx.KindByte(ingest.KindText), Payload: []byte("hello agent")}}
	env, err := buildEnvelope(ingest.ChannelAgent, items)
	if err != nil {
		t.Fatal(err)
	}
	if env.Channel == nil || *env.Channel != ingest.ChannelAgent {
		t.Errorf("agent channel not set: %v", env.Channel)
	}
	if env.Items[0].Text == nil || *env.Items[0].Text != "hello agent" {
		t.Errorf("text field wrong: %+v", env.Items[0])
	}
	if env.Items[0].BytesB64 != nil {
		t.Errorf("text item must not carry bytesB64")
	}
}

// TestDecodedSumExceeds asserts the sender pre-flight cap math (raw==decoded for CB01,
// PLAN §3.8).
func TestDecodedSumExceeds(t *testing.T) {
	items := []sshx.CB01Item{
		{Payload: make([]byte, 10)},
		{Payload: make([]byte, 7)},
	}
	if over, total := decodedSumExceeds(items, 20); over || total != 17 {
		t.Errorf("under cap: over=%v total=%d", over, total)
	}
	if over, total := decodedSumExceeds(items, 16); !over || total != 17 {
		t.Errorf("over cap: over=%v total=%d", over, total)
	}
}

// TestTailscaleTargetIPLiteral asserts a literal 100.x is a Tailscale target, while an
// SSH form (user@ or host:port) is not (PLAN §5.5 precedence).
func TestTailscaleTargetIPLiteral(t *testing.T) {
	if ip, ok := tailscaleTargetIP("100.64.0.9"); !ok || ip != "100.64.0.9" {
		t.Errorf("literal 100.x: got %q ok=%v", ip, ok)
	}
	if _, ok := tailscaleTargetIP("root@host"); ok {
		t.Errorf("user@host must not be a tailscale target")
	}
	if _, ok := tailscaleTargetIP("host:22"); ok {
		t.Errorf("host:port must not be a tailscale target")
	}
	if _, ok := tailscaleTargetIP(""); ok {
		t.Errorf("empty must not be a tailscale target")
	}
}

// TestTailscaleResolvedTargetEmptyIP asserts an empty peer IP is a config error.
func TestTailscaleResolvedTargetEmptyIP(t *testing.T) {
	if _, err := tailscaleResolvedTarget("", config.DefaultConfig()); err == nil {
		t.Errorf("empty peer IP must error")
	}
}

// TestTailscaleResolvedTargetPort asserts the default /clip port is config.Port (8787).
func TestTailscaleResolvedTargetPort(t *testing.T) {
	rt, err := tailscaleResolvedTarget("100.64.0.9", config.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if rt.transport != "tailscale" || rt.peerIP != "100.64.0.9" || rt.peerPort != 8787 {
		t.Errorf("resolved tailscale target wrong: %+v", rt)
	}
}
