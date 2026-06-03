// Package classify ports the source-IP classifier verbatim from Swift Server.swift
// (PLAN §3.3, §6.2). It is the load-bearing gate for the Tier-1 Tailscale /clip
// surface (exact-peer → loopback → tailnet-range-logged-only → foreign) using
// numeric net/netip comparison, v4-mapped-v6 unwrap, and zone-id strip. It is NOT
// the auth control for Tier-0 SSH (a unix-socket peer is gated by the 0600 mode; a
// loopback-TCP /clip peer by the token).
package classify

import (
	"net/netip"
	"strings"
)

// SourceClass is how a connection's validated socket source address is classified.
type SourceClass int

const (
	// Foreign is anything not matching peer/loopback/tailnet (the default).
	Foreign SourceClass = iota
	// Loopback is 127.0.0.0/8, ::1, or a v4-mapped 127/8.
	Loopback
	// Peer is an exact numeric match against the configured peer host.
	Peer
	// TailnetOther is the Tailscale range (100.64.0.0/10 or fd7a:115c:a1e0::/48) —
	// a LOGGED-ONLY belt that never authorizes on its own.
	TailnetOther
)

// String renders the class for logs/tests.
func (c SourceClass) String() string {
	switch c {
	case Loopback:
		return "loopback"
	case Peer:
		return "peer"
	case TailnetOther:
		return "tailnetOther"
	default:
		return "foreign"
	}
}

// Classify classifies a remote host string relative to the configured peer host,
// with the MANDATED ordering: exact-peer → loopback → tailnet-range → foreign
// (Swift classify(), PLAN §3.3). Numeric comparison only — never string-prefix.
// Both remote and peer are unwrapped from their v4-mapped-v6 form and zone-stripped
// before comparison. An unparseable remote → Foreign. An empty/unparseable peer
// simply never matches Peer (the never-configured case).
func Classify(remote string, peer string) SourceClass {
	ip, ok := parseIP(remote)
	if !ok {
		return Foreign
	}
	// 1) Exact peer match (numeric, both sides v4-mapped-unwrapped).
	if peerIP, ok := parseIP(peer); ok && ip == peerIP {
		return Peer
	}
	// 2) Loopback.
	if ip.IsLoopback() {
		return Loopback
	}
	// 3) Tailnet range (logged-only belt — never sufficient on its own for /clip).
	if isTailnet(ip) {
		return TailnetOther
	}
	// 4) Everything else.
	return Foreign
}

// parseIP parses a host string into a normalized netip.Addr: strips surrounding
// brackets and a %zone suffix, and unwraps ::ffff:a.b.c.d to the embedded IPv4 so
// equality/classification ignore the v4-mapped form and the interface scope
// (Swift parseIP, PLAN §3.3). Returns ok=false for hostnames / unparseable input.
func parseIP(raw string) (netip.Addr, bool) {
	s := strings.TrimSpace(raw)
	if len(s) >= 2 && strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	addr = addr.WithZone("") // strip zone id (e.g. fe80::1%eth0)
	if addr.Is4In6() {
		addr = addr.Unmap() // ::ffff:a.b.c.d → a.b.c.d
	}
	return addr, true
}

// isTailnet reports whether ip is in 100.64.0.0/10 (CGNAT) or fd7a:115c:a1e0::/48
// (Tailscale ULA). Computed on the raw bytes to match Swift's bit-test exactly
// (PLAN §3.3, §12.2: 100.64..100.127 via b[1]&0xC0==0x40).
func isTailnet(ip netip.Addr) bool {
	if ip.Is4() {
		b := ip.As4()
		// 100.64.0.0/10 → first octet 100, second octet top 6 bits in [64..127].
		return b[0] == 100 && (b[1]&0xC0) == 0x40
	}
	if ip.Is6() {
		b := ip.As16()
		// fd7a:115c:a1e0::/48 → first 6 bytes fd 7a 11 5c a1 e0.
		return b[0] == 0xfd && b[1] == 0x7a && b[2] == 0x11 &&
			b[3] == 0x5c && b[4] == 0xa1 && b[5] == 0xe0
	}
	return false
}

// RemoteEndpoint extracts the bare host from a net.Addr-style "host:port" or a raw
// host string, stripping brackets and a trailing :port where unambiguous. It is the
// Go analogue of Swift remoteEndpoint(of:)+hostString(from:) (PLAN §6.2): callers
// pass an *http.Request RemoteAddr or a forwarded socket address. Zone ids are kept
// here and stripped by parseIP.
func RemoteEndpoint(remoteAddr string) string {
	s := strings.TrimSpace(remoteAddr)
	if s == "" {
		return ""
	}
	// Bracketed IPv6 with optional port: [::1]:54321 or [::1].
	if strings.HasPrefix(s, "[") {
		if end := strings.LastIndex(s, "]"); end >= 0 {
			return s[1:end]
		}
		return s
	}
	// IPv4 (or hostname) with a single trailing :port → strip it. A bare IPv6
	// literal contains multiple colons and no brackets — leave it untouched.
	if strings.Count(s, ":") == 1 {
		if i := strings.LastIndex(s, ":"); i >= 0 {
			return s[:i]
		}
	}
	return s
}
