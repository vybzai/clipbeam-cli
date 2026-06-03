package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/ingest"
	"github.com/vybzai/clipbeam-cli/internal/sshx"
	"github.com/vybzai/clipbeam-cli/internal/tailscale"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// tailscaleSendTimeout bounds a Tier-1 /clip POST (PLAN §5.2: 30 s timeout, fresh
// connection per send).
const tailscaleSendTimeout = 30 * time.Second

// tailscaleTargetIP reports whether spec is a Tier-1 Tailscale target and, if so, its
// 100.x address (PLAN §5.5 step 5). A spec carrying a user@ or an explicit :port is an
// SSH form and is NOT treated as Tailscale. A bare literal 100.x is accepted directly;
// any other bare token is probed against `tailscale status` (an empty/absent tailnet
// yields false, so the caller falls back to SSH).
func tailscaleTargetIP(spec string) (string, bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", false
	}
	// An explicit SSH form (user@… or host:port) is never a Tailscale target here.
	if strings.Contains(spec, "@") {
		return "", false
	}
	if strings.Count(spec, ":") == 1 {
		// host:port — an SSH form (a literal IPv6 has >1 colon and is handled below).
		return "", false
	}
	// A literal tailnet IPv4 resolves to itself.
	if ip, ok := tailscale.Resolve(spec); ok {
		return ip, true
	}
	return "", false
}

// tailscaleResolvedTarget builds a resolvedTarget for a Tier-1 Tailscale peer at peerIP,
// using config.Port (default 8787) as the /clip port (PLAN §5.2). An empty peerIP is a
// config error (the alias has no recorded 100.x).
func tailscaleResolvedTarget(peerIP string, cfg config.Config) (resolvedTarget, error) {
	if strings.TrimSpace(peerIP) == "" {
		return resolvedTarget{}, configError("tailscale target has no peer IP; re-run `clipbeam setup` or pass a literal 100.x address")
	}
	port := cfg.Port
	if port == 0 {
		port = 8787
	}
	return resolvedTarget{transport: "tailscale", peerIP: peerIP, peerPort: port}, nil
}

// pushTailscale beams items to a Tier-1 peer via plain HTTP POST /clip to the peer's
// 100.x (PLAN §5.2). It builds the FROZEN Envelope v1 (base64 std-encoding, UNWRAPPED —
// the Mac receiver rejects any whitespace, PLAN §3.6), runs the decoded-cap pre-flight
// (§3.8), attaches X-ClipBeam-Token + X-ClipBeam-Sender, and maps the response status to
// the unified exit table (§8.3). WireGuard provides confidentiality/integrity, so there
// is no TLS (§5.2). The remote saved path is NOT relayed on this path (the Mac/CLI peer
// chooses its own save dir and returns it in ClipResponse, but the Tier-1 send surfaces
// only the control response; the daemonless-exec SSH path is the one that relays a path).
func pushTailscale(o out, rt resolvedTarget, channel string, items []sshx.CB01Item) (sendOutcome, error) {
	token, err := loadSendToken()
	if err != nil {
		return sendOutcome{}, err
	}

	env, err := buildEnvelope(channel, items)
	if err != nil {
		return sendOutcome{}, err
	}

	// Decoded-cap pre-flight (PLAN §3.8): sum the decoded payload bytes and fail locally
	// before any network I/O if it exceeds the cap.
	cfg, _ := config.Load()
	if over, total := decodedSumExceeds(items, maxBytesOf(cfg)); over {
		return sendOutcome{}, coded(ExitTooLarge, fmt.Errorf("payload %d bytes exceeds the %d-byte cap", total, maxBytesOf(cfg)))
	}

	body, merr := wire.MarshalEnvelope(env)
	if merr != nil {
		return sendOutcome{}, coded(ExitGeneric, merr)
	}

	url := "http://" + net.JoinHostPort(rt.peerIP, portString(rt.peerPort)) + "/clip"
	o.trace("send: POST %s (%d items, %d body bytes)", url, len(items), len(body))

	ctx, cancel := context.WithTimeout(context.Background(), tailscaleSendTimeout)
	defer cancel()
	req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if rerr != nil {
		return sendOutcome{}, coded(ExitGeneric, rerr)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ClipBeam-Token", token)
	req.Header.Set("X-ClipBeam-Sender", senderHostname())

	// A fresh connection per send (PLAN §5.2): a non-shared transport with keep-alives
	// disabled so each send is independent.
	hc := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, derr := hc.Do(req)
	if derr != nil {
		return sendOutcome{}, coded(ExitUnreachable, fmt.Errorf("tailscale send to %s: %w", rt.peerIP, derr))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return sendOutcome{}, coded(httpStatusToExit(resp.StatusCode), fmt.Errorf("peer /clip returned HTTP %d", resp.StatusCode))
	}
	return sendOutcome{sentItems: len(items)}, nil
}

// buildEnvelope assembles a FROZEN Envelope v1 from the CB01 items + channel (PLAN
// §3.5). image/file items carry UNWRAPPED standard base64 in bytesB64; text items carry
// the raw UTF-8 in text. The channel is nil for the clipboard channel (the wire's
// nil==clipboard collapse) and &"agent" for the agent channel.
func buildEnvelope(channel string, items []sshx.CB01Item) (wire.Envelope, error) {
	env := wire.Envelope{Version: 1, Sender: senderHostname()}
	if channel == ingest.ChannelAgent {
		env.Channel = wire.Ptr(ingest.ChannelAgent)
	}
	for _, it := range items {
		kind := sshx.KindString(it.Kind)
		wi := wire.Item{Kind: kind}
		switch kind {
		case ingest.KindText:
			wi.Text = wire.Ptr(string(it.Payload))
		default:
			// image/file: UNWRAPPED standard base64 (no MIME line wrapping, PLAN §3.6).
			wi.BytesB64 = wire.Ptr(base64.StdEncoding.EncodeToString(it.Payload))
			if it.Name != "" {
				wi.Name = wire.Ptr(it.Name)
			}
			if it.Uti != "" {
				wi.Uti = wire.Ptr(it.Uti)
			}
			if it.Mime != "" {
				wi.Mime = wire.Ptr(it.Mime)
			}
		}
		env.Items = append(env.Items, wi)
	}
	if len(env.Items) == 0 {
		return wire.Envelope{}, usageError("send: nothing to send (empty items)")
	}
	return env, nil
}

// decodedSumExceeds reports whether the decoded payload sum exceeds maxBytes, returning
// the running total (PLAN §3.8 sender pre-flight). For CB01 items the payload is already
// raw bytes (decoded == len), so the sum is the direct byte count.
func decodedSumExceeds(items []sshx.CB01Item, maxBytes int) (bool, int) {
	total := 0
	for _, it := range items {
		total += len(it.Payload)
	}
	return total > maxBytes, total
}

// loadSendToken loads the shared token for a peer-facing send (PLAN §5.2: the token is
// REQUIRED on /clip on every tier). A missing token is a config error directing the
// user to run setup.
func loadSendToken() (string, error) {
	p, err := config.Resolve()
	if err != nil {
		return "", configError("%v", err)
	}
	ts, err := config.OpenTokenStore(config.TokenStoreAuto, p)
	if err != nil {
		return "", configError("open token store: %v", err)
	}
	tok, ok, lerr := ts.Load()
	if lerr != nil {
		return "", configError("load token: %v", lerr)
	}
	if !ok || tok == "" {
		return "", configError("no shared token; run `clipbeam setup` to pair before sending")
	}
	return tok, nil
}

// senderHostname returns this box's hostname for X-ClipBeam-Sender (informational only —
// it MUST NEVER gate anything on the receiver, PLAN §3.3). An unknown hostname falls
// back to "clipbeam".
func senderHostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "clipbeam"
}
