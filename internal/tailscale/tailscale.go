// Package tailscale is the Tier-1 discovery layer (PLAN §5.2): it shells
// `tailscale status --json` via PATH-based binary discovery (NOT the macOS-only
// Homebrew hardcode the Swift app used) and parses Self/Peer with TOTAL TOLERANCE —
// returning [] on missing-binary / non-zero exit / empty data / JSON shape drift,
// every field optional-defaulted, NEVER panicking on drift across Tailscale versions.
package tailscale

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"
)

// probeOrder is the PATH-based binary discovery order (PLAN §5.2). The first
// existing/executable path wins; $PATH is consulted last.
var probeOrder = []string{
	"/usr/bin/tailscale",
	"/usr/local/bin/tailscale",
	"/opt/homebrew/bin/tailscale",
	"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
}

// statusTimeout bounds the `tailscale status --json` exec so a wedged daemon socket
// degrades to "no nodes" rather than hanging a send (PLAN §5.2 total tolerance).
const statusTimeout = 5 * time.Second

// tailnetPrefixV4 is the CGNAT range tailnet IPv4 addresses live in (100.64.0.0/10);
// a literal target in this range is a Tailscale target (PLAN §5.5 step 5).
const tailnetPrefixV4 = "100."

// Node is a discovered tailnet node, every field optional-defaulted so JSON shape
// drift degrades gracefully (PLAN §5.2).
type Node struct {
	HostName     string
	DNSName      string
	OS           string
	Online       bool
	TailscaleIPs []string
}

// FindBinary returns the first tailscale binary from probeOrder, falling back to a
// $PATH lookup. It returns ("", false) when no binary is found (the caller then falls
// back to a manual host — PLAN §5.2).
func FindBinary() (string, bool) {
	for _, p := range probeOrder {
		if isExecutable(p) {
			return p, true
		}
	}
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p, true
	}
	return "", false
}

// isExecutable reports whether path exists and is an executable regular file.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	mode := info.Mode()
	if !mode.IsRegular() {
		return false
	}
	return mode.Perm()&0o111 != 0
}

// statusJSON is the subset of `tailscale status --json` the discovery needs. Every
// field is optional and the decoder tolerates shape drift (extra keys ignored, missing
// keys zero-valued) so a future Tailscale version never breaks the parse (PLAN §5.2).
type statusJSON struct {
	Self *nodeJSON            `json:"Self"`
	Peer map[string]*nodeJSON `json:"Peer"`
}

// nodeJSON mirrors a single Self/Peer entry with every field optional-defaulted.
type nodeJSON struct {
	HostName     string   `json:"HostName"`
	DNSName      string   `json:"DNSName"`
	OS           string   `json:"OS"`
	Online       bool     `json:"Online"`
	TailscaleIPs []string `json:"TailscaleIPs"`
}

// Status runs `tailscale status --json` and parses Self + Peers with total tolerance.
// It returns ([], nil) on ANY failure (missing binary, non-zero exit, empty data, JSON
// drift) — never an error the caller must special-case, never a panic (PLAN §5.2). The
// nil error keeps the caller's path simple: an empty slice means "no tailnet visible",
// and the caller falls back to a manual host / literal 100.x.
func Status() ([]Node, error) {
	bin, ok := FindBinary()
	if !ok {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, bin, "status", "--json").Output()
	if err != nil || len(out) == 0 {
		return nil, nil
	}

	var s statusJSON
	if err := json.Unmarshal(out, &s); err != nil {
		return nil, nil
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
	return nodes, nil
}

// toNode converts a decoded nodeJSON to a Node, defaulting nil slices to empty.
func (n *nodeJSON) toNode() Node {
	ips := n.TailscaleIPs
	if ips == nil {
		ips = []string{}
	}
	return Node{
		HostName:     n.HostName,
		DNSName:      n.DNSName,
		OS:           n.OS,
		Online:       n.Online,
		TailscaleIPs: ips,
	}
}

// Resolve maps a tailnet DNS-name or HostName to a 100.x address using Status(). A
// literal 100.x target resolves to itself. On Linux a DNS-name target requires the
// laptop to also run tailscale status; else only a literal 100.x is accepted (PLAN
// §5.2 R20). Returns ("", false) when unresolved.
func Resolve(name string) (ip string, ok bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	// A literal tailnet IPv4 (100.x) is its own resolution — no Status() call needed.
	if strings.HasPrefix(name, tailnetPrefixV4) {
		return name, true
	}

	nodes, _ := Status()
	for _, n := range nodes {
		if nameMatches(n, name) {
			if v4 := firstTailnetV4(n.TailscaleIPs); v4 != "" {
				return v4, true
			}
		}
	}
	return "", false
}

// nameMatches reports whether the node matches the queried name by HostName, full
// DNSName, or the leading DNS label (so "my-box" matches "my-box.tailnet.ts.net.").
// Both the node DNSName and the query have any trailing FQDN dot stripped first.
func nameMatches(n Node, name string) bool {
	name = strings.TrimSuffix(name, ".")
	if strings.EqualFold(n.HostName, name) {
		return true
	}
	dns := strings.TrimSuffix(n.DNSName, ".")
	if strings.EqualFold(dns, name) {
		return true
	}
	if label, _, found := strings.Cut(dns, "."); found && strings.EqualFold(label, name) {
		return true
	}
	return false
}

// firstTailnetV4 returns the first 100.x IPv4 address from ips, or "" if none.
func firstTailnetV4(ips []string) string {
	for _, ip := range ips {
		if strings.HasPrefix(ip, tailnetPrefixV4) {
			return ip
		}
	}
	return ""
}
