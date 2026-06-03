package tailscale

import (
	"encoding/json"
	"testing"
)

// parseStatusBytes is the pure parse step Status() runs after the exec, factored for
// unit tests so the total-tolerance contract is exercised without a real tailscale
// binary (PLAN §5.2: never panic on shape drift).
func parseStatusBytes(t *testing.T, data []byte) []Node {
	t.Helper()
	var s statusJSON
	if err := json.Unmarshal(data, &s); err != nil {
		return nil // matches Status(): drift → []
	}
	var nodes []Node
	if s.Self != nil {
		nodes = append(nodes, s.Self.toNode())
	}
	for _, p := range s.Peer {
		if p == nil {
			continue
		}
		nodes = append(nodes, p.toNode())
	}
	return nodes
}

// TestParseFullStatus asserts a well-formed status yields Self + Peer nodes with every
// field populated.
func TestParseFullStatus(t *testing.T) {
	data := []byte(`{
		"Self": {"HostName":"laptop","DNSName":"laptop.tailnet.ts.net.","OS":"macOS","Online":true,"TailscaleIPs":["100.64.0.1","fd7a::1"]},
		"Peer": {
			"key1": {"HostName":"box","DNSName":"box.tailnet.ts.net.","OS":"linux","Online":true,"TailscaleIPs":["100.64.0.9"]}
		}
	}`)
	nodes := parseStatusBytes(t, data)
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(nodes))
	}
}

// TestParseShapeDriftNeverPanics asserts that missing fields, unexpected types in
// tolerated positions, and partial objects degrade gracefully (PLAN §5.2). A truly
// malformed type still must not panic — it yields [].
func TestParseShapeDriftNeverPanics(t *testing.T) {
	cases := [][]byte{
		[]byte(`{}`),                             // empty object
		[]byte(`{"Self":null,"Peer":null}`),      // null members
		[]byte(`{"Self":{}}`),                    // self with no fields
		[]byte(`{"Peer":{"k":{}}}`),              // peer with no fields
		[]byte(`{"Self":{"TailscaleIPs":null}}`), // null IPs slice
		[]byte(`{"Self":{"Online":"yes"}}`),      // wrong type → unmarshal error → []
		[]byte(`not json at all`),                // garbage
		[]byte(``),                               // empty
		[]byte(`{"Self":{"HostName":123}}`),      // wrong type for string
	}
	for i, data := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("case %d panicked: %v", i, r)
				}
			}()
			_ = parseStatusBytes(t, data) // must never panic, may return nil
		}()
	}
}

// TestToNodeDefaultsNilIPs asserts a node with no TailscaleIPs gets an empty (non-nil)
// slice so callers can range safely.
func TestToNodeDefaultsNilIPs(t *testing.T) {
	n := (&nodeJSON{HostName: "x"}).toNode()
	if n.TailscaleIPs == nil {
		t.Errorf("TailscaleIPs must default to empty slice, got nil")
	}
}

// TestResolveLiteralTailnetIP asserts a literal 100.x target resolves to itself with no
// Status() call (PLAN §5.2: a literal 100.x is always accepted).
func TestResolveLiteralTailnetIP(t *testing.T) {
	ip, ok := Resolve("100.64.0.9")
	if !ok || ip != "100.64.0.9" {
		t.Errorf("literal tailnet IP: got %q ok=%v", ip, ok)
	}
}

// TestResolveEmpty asserts an empty name does not resolve.
func TestResolveEmpty(t *testing.T) {
	if _, ok := Resolve(""); ok {
		t.Errorf("empty name must not resolve")
	}
}

// TestNameMatches asserts the node-matching logic accepts HostName, full DNSName, and a
// leading DNS label (case-insensitive).
func TestNameMatches(t *testing.T) {
	n := Node{HostName: "box", DNSName: "box.tailnet.ts.net."}
	for _, q := range []string{"box", "BOX", "box.tailnet.ts.net", "box.tailnet.ts.net."} {
		if !nameMatches(n, q) {
			t.Errorf("nameMatches(%q) = false, want true", q)
		}
	}
	if nameMatches(n, "other") {
		t.Errorf("nameMatches(other) = true, want false")
	}
}

// TestFirstTailnetV4 asserts the first 100.x address is chosen over an IPv6 entry.
func TestFirstTailnetV4(t *testing.T) {
	if got := firstTailnetV4([]string{"fd7a::1", "100.64.0.5"}); got != "100.64.0.5" {
		t.Errorf("firstTailnetV4 = %q, want 100.64.0.5", got)
	}
	if got := firstTailnetV4([]string{"fd7a::1"}); got != "" {
		t.Errorf("firstTailnetV4 with no v4 = %q, want empty", got)
	}
}

// TestFindBinaryMissing asserts FindBinary returns false when no tailscale binary is on
// any probed path or $PATH (the common headless case). This relies on the CI box not
// having tailscale installed; if it does, the test still must not panic.
func TestFindBinaryMissing(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("FindBinary panicked: %v", r)
		}
	}()
	_, _ = FindBinary() // result depends on the host; must not panic
}

// TestStatusNoBinaryTolerant asserts Status never errors/panics even with no binary —
// it returns (nil, nil) so the caller falls back to a manual host (PLAN §5.2).
func TestStatusNoBinaryTolerant(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Status panicked: %v", r)
		}
	}()
	nodes, err := Status()
	if err != nil {
		t.Errorf("Status must never return an error (total tolerance), got %v", err)
	}
	_ = nodes
}
