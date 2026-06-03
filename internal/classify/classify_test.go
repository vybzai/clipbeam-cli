package classify

import "testing"

// TestClassifyVectors covers the §3.3/§12.2 source-classification vectors using the
// numeric net/netip path: v4-mapped loopback/peer, the 100.64/10 tailnet boundary
// via b[1]&0xC0==0x40, off-range foreign, zone strip, and unparseable→foreign.
func TestClassifyVectors(t *testing.T) {
	const peer = "100.73.230.15"
	cases := []struct {
		name   string
		remote string
		want   SourceClass
	}{
		{"v4-mapped-loopback", "::ffff:127.0.0.1", Loopback},
		{"loopback-v4", "127.0.0.1", Loopback},
		{"loopback-v6", "::1", Loopback},
		{"exact-peer", "100.73.230.15", Peer},
		{"v4-mapped-peer", "::ffff:100.73.230.15", Peer},
		{"tailnet-low", "100.64.0.1", TailnetOther},
		{"tailnet-high", "100.127.255.255", TailnetOther},
		{"foreign-just-below", "100.63.255.255", Foreign},
		{"foreign-just-above", "100.128.0.0", Foreign},
		{"tailnet-v6", "fd7a:115c:a1e0::1", TailnetOther},
		{"zone-stripped-linklocal", "fe80::1%eth0", Foreign},
		{"unparseable", "not-an-ip", Foreign},
		{"empty", "", Foreign},
	}
	for _, c := range cases {
		if got := Classify(c.remote, peer); got != c.want {
			t.Errorf("%s: Classify(%q) = %v, want %v", c.name, c.remote, got, c.want)
		}
	}
}

// TestClassifyEmptyPeerNeverMatchesPeer asserts the never-configured peer case: an
// empty peer host never yields Peer but still classifies loopback/tailnet correctly.
func TestClassifyEmptyPeerNeverMatchesPeer(t *testing.T) {
	if got := Classify("127.0.0.1", ""); got != Loopback {
		t.Errorf("empty peer, loopback remote = %v, want Loopback", got)
	}
	if got := Classify("100.64.0.1", ""); got != TailnetOther {
		t.Errorf("empty peer, tailnet remote = %v, want TailnetOther", got)
	}
}

// TestRemoteEndpoint covers host extraction from RemoteAddr-style strings.
func TestRemoteEndpoint(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:54321":  "127.0.0.1",
		"[::1]:54321":      "::1",
		"[fe80::1%eth0]":   "fe80::1%eth0",
		"100.73.230.15":    "100.73.230.15",
		"::1":              "::1", // bare IPv6, multiple colons, untouched
	}
	for in, want := range cases {
		if got := RemoteEndpoint(in); got != want {
			t.Errorf("RemoteEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}
