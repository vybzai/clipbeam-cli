package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/httpd"
	"github.com/vybzai/clipbeam-cli/internal/store"
)

// runServe implements `clipbeam serve` (PLAN §5.7). The serve MODES:
//   - socket   (default): a 0600 unix-domain socket for recv/wait/watch + reverse comms
//     (the daemonless-exec push needs NO daemon, so the recommended remote serve mode is
//     `exec`, which runs no `clipbeam serve` at all — PLAN §5.7).
//   - tcp:       127.0.0.1:port, token REQUIRED on control endpoints (shared box, §6.2).
//   - tailscale: dual-stack all-interfaces TCP on config.port so the box can RECEIVE
//     Tier-1 /clip from a tailnet peer, gated by exact-peer-IP AND token (§5.2/§6.2).
//
// It binds, serves until SIGINT/SIGTERM, then shuts down gracefully.
func runServe(o out, listen string, setClipboard bool) error {
	cfg, err := config.Load()
	if err != nil {
		return configError("%v", err)
	}
	if setClipboard {
		// Persisted opt-in lives in config; the runtime flag forces it on for this run.
		cfg.LinuxClipboard = true
		o.trace("serve: best-effort X/Wayland clipboard set enabled for this run")
	}

	p, err := config.Resolve()
	if err != nil {
		return configError("%v", err)
	}

	kind, socketPath, peerHost, err := resolveListen(listen, cfg)
	if err != nil {
		return err
	}

	// The token is required on the TCP binds; the unix socket is token-free (the 0600
	// file is the gate). A missing token on a TCP bind is a config error.
	token := ""
	if ts, err := config.OpenTokenStore(config.TokenStoreAuto, p); err == nil {
		if tok, ok, err := ts.Load(); err == nil && ok {
			token = tok
		}
	}
	if kind != httpd.ListenUnixSocket && token == "" {
		return configError("serve --listen %s requires a shared token; run `clipbeam setup` first", listen)
	}

	saveDir, err := config.ResolvedSaveDir(cfg)
	if err != nil {
		return configError("%v", err)
	}
	st, err := store.New(store.StoreConfig{
		SaveDir:           saveDir,
		AgentInboxDir:     p.AgentInbox,
		LastPathFile:      p.LastPath,
		RecentsFile:       p.Recents,
		SaveTextToDisk:    cfg.SaveTextToDisk,
		LongTextThreshold: cfg.LongTextThreshold,
		MaxBytes:          cfg.MaxBytes,
	})
	if err != nil {
		return coded(ExitGeneric, err)
	}

	srv := httpd.New(httpd.Options{
		Config:     cfg,
		Token:      token,
		Store:      st,
		Listen:     kind,
		SocketPath: socketPath,
		PeerHost:   peerHost,
		AppVersion: cmdVersion,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch kind {
	case httpd.ListenUnixSocket:
		o.diag("serve: listening on unix socket %s (0600; token-free)", socketPath)
	case httpd.ListenLoopbackTCP:
		o.diag("serve: listening on 127.0.0.1:%d (token required on control endpoints)", cfg.Port)
	case httpd.ListenTailscale:
		o.diag("serve: listening on [::]:%d for Tier-1 /clip (exact-peer-IP + token)", cfg.Port)
	}

	if err := srv.Serve(ctx); err != nil {
		return coded(ExitGeneric, err)
	}
	o.diag("serve: shut down cleanly")
	return nil
}

// resolveListen maps the --listen string to a ListenKind + socket/peer parameters
// (PLAN §5.7). An unknown value is a usage error.
func resolveListen(listen string, cfg config.Config) (httpd.ListenKind, string, string, error) {
	switch listen {
	case "socket", "":
		sock, err := controlSocketPath()
		if err != nil {
			return 0, "", "", configError("%v", err)
		}
		return httpd.ListenUnixSocket, sock, "", nil
	case "tcp":
		return httpd.ListenLoopbackTCP, "", "", nil
	case "tailscale":
		// The exact-peer host is the configured default peer's host (§5.2). Empty when
		// no peer is paired — serve still binds, but /clip will 403 every source.
		peerHost := ""
		for _, peer := range cfg.Peers {
			if peer.ID == cfg.DefaultPeerID || cfg.DefaultPeerID == "" {
				peerHost = peer.Host
				break
			}
		}
		return httpd.ListenTailscale, "", peerHost, nil
	default:
		return 0, "", "", usageError("serve: --listen must be socket | tcp | tailscale (got %q)", listen)
	}
}
