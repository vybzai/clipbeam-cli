package cli

import (
	"fmt"

	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/ingest"
	"github.com/vybzai/clipbeam-cli/internal/sshx"
)

// sendResultJSON is the FROZEN --json shape for send/push/shot/msg (PLAN §8.2/§8.7):
// {"schema","ok":true,"sentItems":N,"remotePath":"<abs>|null"}. remotePath is the
// remote saved path the daemonless-exec push relayed (the symmetry-with-`last`
// guarantee, §8.7); it is null for a bare /push that saves nothing on the receiver.
type sendResultJSON struct {
	Schema     string  `json:"schema"`
	OK         bool    `json:"ok"`
	SentItems  int     `json:"sentItems"`
	RemotePath *string `json:"remotePath"`
}

// sendOutcome carries a completed send's results so the success printer is shared by
// every send verb.
type sendOutcome struct {
	sentItems  int
	remotePath string // "" ⇒ null in --json; the bare path on stdout in default mode
}

// emitSendOutcome prints a successful send per §8.7: default mode prints the control
// response {"ok":true,"sentItems":N} VERBATIM (parity with the shim's show_request),
// and — when the daemonless-exec push relayed a remote path — ends with that path on
// stdout (symmetry with `last`). --json emits the normalized sendResultJSON one-liner.
func emitSendOutcome(o out, r sendOutcome) error {
	if o.json {
		return o.emitJSON(sendResultJSON{
			Schema:     schemaVersion,
			OK:         true,
			SentItems:  r.sentItems,
			RemotePath: nilIfEmpty(r.remotePath),
		})
	}
	// Default mode: the control response verbatim, then the remote path (if any) on its
	// own line as the deliverable.
	o.dataln(fmt.Sprintf(`{"ok":true,"sentItems":%d}`, r.sentItems))
	if r.remotePath != "" {
		o.data(r.remotePath) // bare path, no trailing newline (symmetry with `last`)
	}
	return nil
}

// resolvedTarget bundles the SSH target + the recorded remote binary path needed for
// the daemonless-exec push (PLAN §5.1), or — for a Tier-1 Tailscale target — the peer's
// 100.x IP + port (PLAN §5.2). It is resolved from the alias store (a saved `setup`
// target), a literal user@host spec, or a literal/resolved tailnet address.
type resolvedTarget struct {
	target        sshx.Target
	remoteBinPath string
	transport     string // "ssh" | "tailscale"
	peerIP        string // tailscale only: the peer's 100.x address
	peerPort      uint16 // tailscale only: the peer's /clip port (config.Port, default 8787)
}

// resolveTarget resolves a target spec (or the default alias when empty) into a
// transport-tagged target (PLAN §5.1/§5.2/§5.5). The precedence: a saved alias name →
// its stored transport; a literal tailnet 100.x (or a name resolving via tailscale
// status) → Tier-1 Tailscale; otherwise → Tier-0 SSH (user@host[:port]). A missing
// default alias with no explicit spec is a config error (no peer paired).
func resolveTarget(spec string) (resolvedTarget, error) {
	p, err := config.Resolve()
	if err != nil {
		return resolvedTarget{}, configError("%v", err)
	}
	cfg, _ := config.Load()
	aliases, err := config.LoadAliases(p)
	if err != nil {
		return resolvedTarget{}, configError("%v", err)
	}

	// An empty spec uses the default alias; a non-empty spec may name a saved alias OR
	// be a literal user@host[:port] / tailnet address.
	if alias, ok := aliases.Lookup(spec); ok {
		if alias.Transport == "tailscale" {
			return tailscaleResolvedTarget(alias.PeerIP, cfg)
		}
		t, terr := sshx.ResolveTarget(aliasSpec(alias))
		if terr != nil {
			return resolvedTarget{}, resolveErr(terr)
		}
		return resolvedTarget{target: t, remoteBinPath: alias.RemoteBinPath, transport: "ssh"}, nil
	}
	if spec == "" {
		return resolvedTarget{}, configError("no target given and no default alias; run `clipbeam setup user@host` first")
	}

	// A literal tailnet target (100.x) or a name resolving via `tailscale status` is a
	// Tier-1 Tailscale target (PLAN §5.5 step 5), UNLESS the spec carries a user@ or a
	// :port (an explicit SSH form). A bare token is probed against tailscale.
	if ip, ok := tailscaleTargetIP(spec); ok {
		return tailscaleResolvedTarget(ip, cfg)
	}

	// Literal spec: resolve via ssh_config. The remote bin path is unknown for an
	// unpaired literal target, so the daemonless push uses a bare `clipbeam ingest`
	// (documented to frequently fail under a minimal non-login PATH); callers should
	// prefer a saved alias.
	t, terr := sshx.ResolveTarget(spec)
	if terr != nil {
		return resolvedTarget{}, resolveErr(terr)
	}
	return resolvedTarget{target: t, remoteBinPath: "", transport: "ssh"}, nil
}

// resolveErr maps a sshx.ResolveTarget error to the unified table (PLAN §8.3). A typed
// transport sentinel (CA-unsupported / ProxyJump / host-key) maps via codedSSH; any
// other resolve failure is a config/identity problem (ExitConfig) — a bad spec or an
// unresolvable host is an identity issue, not a peer/network failure.
func resolveErr(err error) error {
	if isTypedSSHErr(err) {
		return codedSSH(err)
	}
	return coded(ExitConfig, fmt.Errorf("cannot resolve target: %w", err))
}

// isTypedSSHErr reports whether err is one of the sshx transport sentinels that carry a
// specific exit code (PLAN §8.3).
func isTypedSSHErr(err error) bool {
	for _, s := range []error{
		sshx.ErrHostKeyUnknown, sshx.ErrHostKeyMismatch, sshx.ErrHostKeyCAUnsupported,
		sshx.ErrProxyJumpUnsupported, sshx.ErrDialFailed,
	} {
		if isErr(err, s) {
			return true
		}
	}
	return false
}

// aliasSpec builds the ssh_config resolution spec for a saved alias: prefer its
// ~/.ssh/config Host name, else user@host.
func aliasSpec(a config.Alias) string {
	if a.SSHConfigAlias != "" {
		return a.SSHConfigAlias
	}
	if a.SSHUser != "" {
		return a.SSHUser + "@" + a.SSHHost
	}
	return a.SSHHost
}

// pushCB01 dials the resolved target, execs the remote `clipbeam ingest` over SSH, and
// streams a CB01 frame to its stdin, relaying the remote stdout (the saved absolute
// path) as the deliverable (PLAN §5.1). It returns the number of items sent and the
// relayed remote path. A Tier-1 Tailscale target is routed to the plain-HTTP /clip path
// (PLAN §5.2) instead of the SSH CB01 frame.
func pushCB01(o out, rt resolvedTarget, channel string, items []sshx.CB01Item) (sendOutcome, error) {
	// A Tier-1 Tailscale target dials plain HTTP POST /clip to the peer's 100.x with the
	// token (PLAN §5.2) — the byte-identical-to-ClipBeam path, NOT the SSH CB01 frame.
	if rt.transport == "tailscale" {
		return pushTailscale(o, rt, channel, items)
	}

	client, err := sshx.NewClient()
	if err != nil {
		return sendOutcome{}, sshNotReady(err)
	}
	defer func() { _ = client.Close() }()

	sess, err := client.Dial(rt.target)
	if err != nil {
		return sendOutcome{}, codedSSH(err)
	}
	defer func() { _ = sess.Close() }()

	frame, err := encodeFrame(channel, items)
	if err != nil {
		return sendOutcome{}, coded(ExitGeneric, err)
	}

	remoteCmd := remoteIngestCommand(rt.remoteBinPath)
	o.trace("send: exec %q on %s", remoteCmd, rt.target.Host)
	stdout, stderr, err := sess.Run(remoteCmd, frame)
	if err != nil {
		return sendOutcome{}, coded(ExitRemoteIO, fmt.Errorf("remote ingest failed: %v: %s", err, string(stderr)))
	}
	return sendOutcome{sentItems: len(items), remotePath: string(stdout)}, nil
}

// encodeFrame encodes a CB01 frame for the given channel/items (PLAN §5.1).
func encodeFrame(channel string, items []sshx.CB01Item) ([]byte, error) {
	var buf byteBuffer
	if err := sshx.EncodeCB01(&buf, sshx.ChannelByte(channel), items); err != nil {
		return nil, err
	}
	return buf.b, nil
}

// remoteIngestCommand builds the remote command: the ABSOLUTE recorded path + " ingest"
// (a bare `clipbeam` frequently fails under a minimal non-login SSH-exec PATH, so the
// absolute path is preferred — PLAN §5.1). When no path was recorded (a literal target)
// it falls back to a bare `clipbeam ingest`.
func remoteIngestCommand(remoteBin string) string {
	if remoteBin != "" {
		return shellQuote(remoteBin) + " ingest"
	}
	return "clipbeam ingest"
}

// shellQuote single-quotes s for safe interpolation into the remote command string
// (paths may contain spaces; setup records absolute paths). Embedded single quotes are
// escaped via the '\'' idiom.
func shellQuote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\\', '\'', '\'')
			continue
		}
		out = append(out, s[i])
	}
	out = append(out, '\'')
	return string(out)
}

// byteBuffer is a tiny io.Writer accumulator so encodeFrame avoids importing bytes at
// the call site (the CB01 frame for a 50 MB item streams from disk in the SSH phase;
// for the metadata-light text/agent frames buffering is fine).
type byteBuffer struct{ b []byte }

func (w *byteBuffer) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

// sshNotReady maps a sshx.NewClient construction error to the unified table. NewClient
// only fails when the known_hosts host-key database cannot be parsed — a config/identity
// problem (ExitConfig), not a peer/network failure.
func sshNotReady(err error) error {
	return coded(ExitConfig, fmt.Errorf("ssh client unavailable (check ~/.ssh/known_hosts): %w", err))
}

// remoteNotReady is returned by verbs whose remote-target form is not yet wired through
// the SSH transport (e.g. `health <target>`). The SSH transport itself is live (sends
// work); these specific verbs surface a clear config-class not-wired message rather than
// a confusing transport error. A NewClient failure (a broken known_hosts) is surfaced
// first as the host-key config problem.
func remoteNotReady(verb, target string) error {
	if _, err := sshx.NewClient(); err != nil {
		return sshNotReady(err)
	}
	return coded(ExitConfig, fmt.Errorf("%s %s: remote transport not yet wired for this verb", verb, target))
}

// channelFor decides the routing channel from the --agent flag (PLAN §8.7): send/shot
// default to the clipboard channel; --agent flips to the agent inbox.
func channelFor(agent bool) string {
	if agent {
		return ingest.ChannelAgent
	}
	return ingest.ChannelClipboard
}
