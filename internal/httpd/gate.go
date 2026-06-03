package httpd

import (
	"net/http"

	"github.com/vybzai/clipbeam-cli/internal/auth"
	"github.com/vybzai/clipbeam-cli/internal/classify"
)

// tokenHeader is the frozen request header carrying the shared token (PLAN §3.3,
// §6.4). It is never in argv; it travels only on the wire.
const tokenHeader = "X-ClipBeam-Token"

// clipGate authorizes POST /clip in the MANDATED order (PLAN §3.3): source class
// (Tier-1 exact-peer) THEN constant-time token. It returns the HTTP status to emit
// and a short reason on rejection, or status 0 when the request is authorized.
//
// On the unix-socket and loopback-TCP modes the source IP is NOT node-authenticated
// (PLAN §6.2): cls==.peer is meaningful ONLY on the Tailscale tier, so on those modes
// the token is the primary credential and the class gate is satisfied by loopback.
// On ListenTailscale the exact-peer-IP class AND the token are both required, byte
// identical to the shipped ClipBeam (PLAN §5.2).
func (s *Server) clipGate(r *http.Request) (status int, reason string) {
	// 1) Source-class gate first (cheaper than the token; 403 before reading a body).
	switch s.opts.Listen {
	case ListenTailscale:
		// Tier-1: require an exact-peer-IP match. Range membership is logged-only and
		// never authorizes (PLAN §5.2/§6.2).
		cls := classify.Classify(remoteHost(r), s.opts.PeerHost)
		if cls != classify.Peer {
			return http.StatusForbidden, "forbidden"
		}
	case ListenLoopbackTCP:
		// Tier-0 loopback TCP: the token is the primary credential, but a non-loopback
		// peer must never reach /clip on this bind (defense-in-depth on a shared box).
		cls := classify.Classify(remoteHost(r), s.opts.PeerHost)
		if cls != classify.Loopback && cls != classify.Peer {
			return http.StatusForbidden, "forbidden"
		}
	case ListenUnixSocket:
		// The 0600 socket mode is the gate; the token below is still required as
		// defense-in-depth over the tunnel (PLAN §5.1: token required on /clip even
		// over the tunnel).
	}

	// 2) Constant-time token (a nil/missing header still burns the compare loop,
	// PLAN §3.12).
	if !auth.ConstantTimeEquals(headerPtr(r, tokenHeader), s.opts.Token) {
		return http.StatusUnauthorized, "unauthorized"
	}
	return 0, ""
}

// controlAuthorized authorizes the loopback control endpoints (/push, /agent-send,
// /recv, /last, /wait) by the ONE rule of PLAN §6.2:
//
//	(unix-socket peer is the 0600 socket's owner) OR
//	(request class is loopback AND a valid constant-time token over --listen tcp).
//
// On the 0600 unix socket the filesystem permission IS the gate — token-free. Over
// --listen tcp (opt-in, shared box) the token is REQUIRED on every control endpoint,
// exactly as /clip is gated, because 127.0.0.1 is reachable by every local uid.
// ListenTailscale never serves control endpoints to an off-host peer: a control
// request there must originate from loopback AND carry the token.
func (s *Server) controlAuthorized(r *http.Request) (status int, reason string) {
	switch s.opts.Listen {
	case ListenUnixSocket:
		// The 0600 socket owner is the only one who can connect; the kernel enforces
		// it. Token-free (PLAN §6.2). The bind-time uid-ownership refusal (socket_*.go)
		// guarantees the socket is not another uid's.
		return 0, ""
	default:
		// TCP binds (loopback or tailscale): require loopback class AND the token.
		cls := classify.Classify(remoteHost(r), s.opts.PeerHost)
		if cls != classify.Loopback {
			return http.StatusForbidden, "forbidden"
		}
		if !auth.ConstantTimeEquals(headerPtr(r, tokenHeader), s.opts.Token) {
			return http.StatusUnauthorized, "unauthorized"
		}
		return 0, ""
	}
}

// remoteHost returns the bare source host for classification (PLAN §3.3): the
// *http.Request RemoteAddr stripped of its :port and any brackets/zone via
// classify.RemoteEndpoint. For a unix-socket connection RemoteAddr is empty/"@", which
// classifies as Foreign — harmless because the unix gate does not consult the class.
func remoteHost(r *http.Request) string {
	return classify.RemoteEndpoint(r.RemoteAddr)
}

// headerPtr returns a pointer to the first value of header key, or nil if the header
// is absent. The nil/"" distinction is load-bearing for ConstantTimeEquals (a missing
// header is nil, an empty header is &"", PLAN §3.12).
func headerPtr(r *http.Request, key string) *string {
	vals := r.Header.Values(key)
	if len(vals) == 0 {
		return nil
	}
	v := vals[0]
	return &v
}
