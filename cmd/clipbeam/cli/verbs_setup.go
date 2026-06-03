package cli

import (
	"fmt"
	"runtime"

	"github.com/vybzai/clipbeam-cli/internal/sshx"
)

// setupOptions bundles the `setup` flags (PLAN §9.5).
type setupOptions struct {
	serve       string
	acceptNew   bool
	force       bool
	fromRelease bool
}

// runSetup implements `clipbeam setup <user@host>` (PLAN §9.5): the keystone remote
// bootstrap — dial + verify host key (non-interactive accept-new TOFU only under this
// verb, §5.4), detect remote OS/arch, stream the matching locally-built static binary
// over the SAME SSH connection, place it + record the absolute path into the alias
// store, generate + pair a 120-bit token (local store + remote 0600 file). Idempotent
// via the remote `clipbeam version --json` (§9.5).
func runSetup(o out, spec string, opts setupOptions) error {
	switch opts.serve {
	case "exec", "socket", "tcp", "tailscale", "":
	default:
		return usageError("setup: --serve must be exec | socket | tcp | tailscale (got %q)", opts.serve)
	}

	// Resolve the target up front so a bad spec / unsupported ProxyJump fails with the
	// right exit code before any transport work (PLAN §5.4/§8.3).
	t, err := sshx.ResolveTarget(spec)
	if err != nil {
		return resolveErr(err)
	}
	// setup is the ONE verb allowed to record an unknown host non-interactively (TOFU),
	// gated by the resolved target plus the explicit --accept-new-hostkey flag (§5.4).
	// Without the flag, an unknown host still fails closed with the run-setup hint.
	t.AcceptNewHostKey = opts.acceptNew

	o.trace("setup: target %q, serve mode %q, accept-new=%v, force=%v, from-release=%v",
		spec, defaultServe(opts.serve), opts.acceptNew, opts.fromRelease, opts.force)

	return runSetupBootstrap(o, spec, t, opts)
}

// defaultServe returns the serve mode, defaulting an empty flag to the daemonless-exec
// mode (PLAN §5.7: setup defaults to (0a) daemonless-exec).
func defaultServe(serve string) string {
	if serve == "" {
		return "exec"
	}
	return serve
}

// runService implements `clipbeam service <install|uninstall>` (PLAN §9.7): manage the
// optional, OFF-by-default service unit (systemd --user on Linux, launchd on macOS).
// The unit writer/remover lands in the install/distribution phase; this validates the
// subcommand surface and surfaces the not-ready condition.
func runService(o out, action string) error {
	switch action {
	case "install", "uninstall":
	default:
		return usageError("service: expected install | uninstall (got %q)", action)
	}
	unit := "systemd --user (com.sani.clipbeam)"
	if runtime.GOOS == "darwin" {
		unit = "launchd (com.sani.clipbeam)"
	}
	o.trace("service: %s %s", action, unit)
	return coded(ExitConfig, fmt.Errorf("service %s: optional %s unit management not yet wired (off by default)", action, unit))
}
