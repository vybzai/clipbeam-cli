package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/vybzai/clipbeam-cli/internal/auth"
	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/sshx"
)

// runSetupBootstrap performs the §9.5 keystone bootstrap over an in-process SSH
// session: dial (verifying the host key per §5.4), detect remote OS/arch, decide
// idempotently whether to (re)stream the binary, stream a locally-built cross-arch
// static binary over the SAME SSH connection into an absolute path, pair a token, and
// record the alias. Every step's diagnostics go to stderr; the final deliverable
// (the recorded absolute remote path) goes to stdout.
func runSetupBootstrap(o out, spec string, t sshx.Target, opts setupOptions) error {
	if opts.fromRelease {
		// The remote-side curl|sh fetch (--from-release) is a distribution-phase path
		// (PLAN §9.5 source iii). The default in-process stream (source i) below is the
		// dev + pre-release path that needs no published Release.
		return configError("setup: --from-release (remote-side fetch) is not wired in this build; omit it to stream the locally-built binary over SSH")
	}

	client, err := sshx.NewClient()
	if err != nil {
		return sshNotReady(err)
	}
	defer func() { _ = client.Close() }()

	// Step 1+2: dial (host-key verified) and detect remote OS/arch via uname.
	o.diag("setup: dialing %s ...", spec)
	goos, goarch, remoteHome, err := detectRemotePlatform(o, client, t)
	if err != nil {
		return err
	}
	o.diag("setup: remote platform %s/%s (home %s)", goos, goarch, remoteHome)

	// Step 6 (idempotency, checked early): compare the remote `clipbeam version --json`
	// to the local version; skip the upload when identical unless --force (PLAN §9.5).
	// The recorded path is the ABSOLUTE $HOME/.local/bin/clipbeam (a bare name fails
	// under a minimal non-login SSH-exec PATH, PLAN §5.1).
	remoteBin := remoteHome + "/.local/bin/clipbeam"
	upload := true
	if !opts.force {
		if rv, ok := remoteVersion(o, client, t, remoteBin); ok && rv == cmdVersion {
			o.diag("setup: remote clipbeam %s already matches local; skipping upload (use --force to re-stream)", rv)
			upload = false
		}
	}

	// Step 3+4: build the matching static binary locally, stream it over SSH, place it
	// at the absolute path, chmod 755.
	if upload {
		bin, berr := buildCrossBinary(o, goos, goarch)
		if berr != nil {
			return berr
		}
		defer func() { _ = os.Remove(bin) }()
		if err := streamBinary(o, client, t, bin, remoteBin); err != nil {
			return err
		}
		o.diag("setup: placed remote binary at %s", remoteBin)
	}

	// Step 5: pair the token — generate a 120-bit secret, store it locally, and write
	// it to the remote 0600 token file (never in argv; fed on the remote stdin, §6.4).
	token, terr := ensureLocalToken(o)
	if terr != nil {
		return terr
	}
	if err := writeRemoteToken(o, client, t, remoteBin, token); err != nil {
		return err
	}
	o.diag("setup: paired token (local store + remote 0600 file)")

	// Record the alias (the resulting absolute path, transport, serve mode) so later
	// data verbs resolve this target with no re-discovery (PLAN §5.6).
	if err := recordAlias(o, spec, t, remoteBin, defaultServe(opts.serve)); err != nil {
		return err
	}

	// Deliverable: the recorded absolute remote binary path (symmetry with the path-on-
	// stdout contract for the data verbs).
	o.diag("setup: done")
	o.data(remoteBin)
	return nil
}

// detectRemotePlatform runs `uname -s; uname -m; printf %s "$HOME"` over SSH in one
// session and maps the output to Go GOOS/GOARCH tokens + the remote home dir (PLAN §9.5
// step 2/4). Resolving $HOME on the remote keeps the recorded binary path absolute and
// correct for any user, not a local guess. An unmappable platform is a config error.
func detectRemotePlatform(o out, client sshx.Client, t sshx.Target) (goos, goarch, home string, err error) {
	sess, derr := client.Dial(t)
	if derr != nil {
		return "", "", "", codedSSH(derr)
	}
	defer func() { _ = sess.Close() }()

	// Three lines: uname -s, uname -m, then $HOME (printed last on its own line).
	stdout, stderr, rerr := sess.Run(`uname -s; uname -m; printf '%s\n' "$HOME"`, nil)
	if rerr != nil {
		return "", "", "", coded(ExitUnreachable, fmt.Errorf("setup: detect remote platform: %v: %s", rerr, strings.TrimSpace(string(stderr))))
	}
	lines := strings.Split(strings.TrimRight(string(stdout), "\n"), "\n")
	if len(lines) < 3 {
		return "", "", "", configError("setup: unexpected platform-detect output %q", strings.TrimSpace(string(stdout)))
	}
	goos, ok := unameToGOOS(lines[0])
	if !ok {
		return "", "", "", configError("setup: unsupported remote OS %q (clipbeam v1 targets Linux and macOS)", lines[0])
	}
	goarch, ok = unameToGOARCH(lines[1])
	if !ok {
		return "", "", "", configError("setup: unsupported remote arch %q", lines[1])
	}
	home = strings.TrimSpace(lines[2])
	if home == "" || !strings.HasPrefix(home, "/") {
		// A non-absolute $HOME is a misconfigured remote; fall back to the conventional
		// home for the user so the recorded path stays absolute (PLAN §5.1).
		home = remoteHomeFor(t.User)
	}
	o.trace("setup: uname -s=%q -m=%q HOME=%q -> %s/%s", lines[0], lines[1], home, goos, goarch)
	return goos, goarch, home, nil
}

// unameToGOOS maps `uname -s` to a GOOS token (PLAN §9.5; Windows is out of v1 scope).
func unameToGOOS(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "linux":
		return "linux", true
	case "darwin":
		return "darwin", true
	default:
		return "", false
	}
}

// unameToGOARCH maps `uname -m` to a GOARCH token (PLAN §9.1 arch detect rules).
func unameToGOARCH(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "x86_64", "amd64":
		return "amd64", true
	case "arm64", "aarch64":
		return "arm64", true
	default:
		return "", false
	}
}

// remoteVersion reads the remote `clipbeam version --json` (PLAN §9.5 step 6) and
// returns its semver "version" field. A missing binary / parse failure yields
// ("", false) so the caller treats it as "not installed" and uploads.
func remoteVersion(o out, client sshx.Client, t sshx.Target, remoteBin string) (string, bool) {
	sess, err := client.Dial(t)
	if err != nil {
		return "", false
	}
	defer func() { _ = sess.Close() }()

	stdout, _, rerr := sess.Run(shellQuote(remoteBin)+" version --json", nil)
	if rerr != nil {
		return "", false
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(stdout))), &v); err != nil {
		return "", false
	}
	o.trace("setup: remote version --json reports %q", v.Version)
	if v.Version == "" {
		return "", false
	}
	return v.Version, true
}

// buildCrossBinary builds the matching static clipbeam for goos/goarch via
// `GOOS=<os> GOARCH=<arch> CGO_ENABLED=0 go build` into a temp file (PLAN §9.5 source
// i — the dev + pre-release path that needs NO published Release). The temp file path
// is returned; the caller removes it.
func buildCrossBinary(o out, goos, goarch string) (string, error) {
	pkg, perr := mainPackagePath()
	if perr != nil {
		return "", configError("setup: locate clipbeam source to build the remote binary: %v", perr)
	}
	tmp, err := os.CreateTemp("", "clipbeam-setup-*")
	if err != nil {
		return "", coded(ExitGeneric, fmt.Errorf("setup: temp file: %w", err))
	}
	out := tmp.Name()
	_ = tmp.Close()

	o.diag("setup: building static %s/%s binary (CGO_ENABLED=0) ...", goos, goarch)
	cmd := exec.Command("go", "build",
		"-trimpath",
		"-ldflags", "-s -w -X main.version="+cmdVersion,
		"-o", out, pkg)
	cmd.Env = append(os.Environ(),
		"GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(out)
		return "", coded(ExitGeneric, fmt.Errorf("setup: cross-build failed: %v: %s", err, strings.TrimSpace(stderr.String())))
	}
	return out, nil
}

// mainPackagePath returns the import path of the clipbeam main package to build. It
// prefers the module-relative path so `go build` resolves it from the source checkout
// the running CLI was built from.
func mainPackagePath() (string, error) {
	return "github.com/vybzai/clipbeam-cli/cmd/clipbeam", nil
}

// streamBinary uploads a local binary to remoteBin over SSH (PLAN §9.5 step 4): it
// pipes the bytes to a remote shell that creates the parent dir, writes a temp file,
// chmods 755, and atomically renames into place. The binary travels over the SAME SSH
// connection (no remote outbound internet, no curl). The whole binary is read into
// memory for the single exec; a clipbeam binary is a few MB so this is acceptable for
// the one-time bootstrap.
func streamBinary(o out, client sshx.Client, t sshx.Target, localPath, remoteBin string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return coded(ExitGeneric, fmt.Errorf("setup: read built binary: %w", err))
	}
	sess, derr := client.Dial(t)
	if derr != nil {
		return codedSSH(derr)
	}
	defer func() { _ = sess.Close() }()

	dir := shellQuotePosix(posixDir(remoteBin))
	tmp := shellQuotePosix(remoteBin + ".clipbeam-tmp")
	final := shellQuotePosix(remoteBin)
	// mkdir -p the parent, write stdin to the temp file, chmod 755, atomic mv into place.
	remoteCmd := fmt.Sprintf("mkdir -p %s && cat > %s && chmod 755 %s && mv -f %s %s", dir, tmp, tmp, tmp, final)
	o.trace("setup: streaming %d bytes -> %s", len(data), remoteBin)
	_, stderr, rerr := sess.Run(remoteCmd, data)
	if rerr != nil {
		return coded(ExitRemoteIO, fmt.Errorf("setup: place remote binary: %v: %s", rerr, strings.TrimSpace(string(stderr))))
	}
	return nil
}

// ensureLocalToken loads the existing shared token or generates+stores a fresh 120-bit
// one (PLAN §9.5 step 5 / §6.4). The same token is then written to the remote box so
// the pair shares one secret.
func ensureLocalToken(o out) (string, error) {
	p, err := config.Resolve()
	if err != nil {
		return "", configError("%v", err)
	}
	ts, err := config.OpenTokenStore(config.TokenStoreAuto, p)
	if err != nil {
		return "", configError("setup: open token store: %v", err)
	}
	if tok, ok, lerr := ts.Load(); lerr == nil && ok {
		o.trace("setup: reusing existing local token (store=%s)", ts.Kind())
		return tok, nil
	}
	tok, err := auth.MakeToken()
	if err != nil {
		return "", coded(ExitGeneric, fmt.Errorf("setup: generate token: %w", err))
	}
	if err := ts.Save(tok); err != nil {
		return "", configError("setup: save local token: %v", err)
	}
	o.trace("setup: generated a fresh 120-bit token (store=%s)", ts.Kind())
	return tok, nil
}

// writeRemoteToken writes the shared token to the remote box's 0600 token file (PLAN
// §6.4 / §4.2). The token is fed on the remote stdin, NEVER as an argv argument (which
// would surface in the box's ps/proc). The remote path is resolved on the remote side
// honoring $XDG_CONFIG_HOME, defaulting to ~/.config/clipbeam/token; the parent is
// created 0700 and the file is chmod'd 0600.
func writeRemoteToken(o out, client sshx.Client, t sshx.Target, remoteBin, token string) error {
	sess, derr := client.Dial(t)
	if derr != nil {
		return codedSSH(derr)
	}
	defer func() { _ = sess.Close() }()

	// Resolve the token path on the remote side: $XDG_CONFIG_HOME (if absolute) else
	// $HOME/.config; create the dir 0700, write stdin to the file, chmod 0600.
	remoteCmd := `dir="${XDG_CONFIG_HOME:-$HOME/.config}/clipbeam"; ` +
		`case "$dir" in /*) ;; *) dir="$HOME/.config/clipbeam";; esac; ` +
		`mkdir -p "$dir" && chmod 700 "$dir" && cat > "$dir/token" && chmod 600 "$dir/token"`
	o.trace("setup: writing remote token file (stdin, not argv)")
	_, stderr, rerr := sess.Run(remoteCmd, []byte(token))
	if rerr != nil {
		return coded(ExitRemoteIO, fmt.Errorf("setup: write remote token: %v: %s", rerr, strings.TrimSpace(string(stderr))))
	}
	return nil
}

// recordAlias persists the bootstrap result to the CLI-owned alias store (PLAN §5.6),
// setting it as the default and recording the ABSOLUTE remote binary path the data
// verbs exec. An existing alias of the same name is replaced.
func recordAlias(o out, spec string, t sshx.Target, remoteBin, serve string) error {
	p, err := config.Resolve()
	if err != nil {
		return configError("%v", err)
	}
	store, err := config.LoadAliases(p)
	if err != nil {
		return configError("%v", err)
	}
	name := aliasName(t, spec)
	alias := config.Alias{
		Name:           name,
		Transport:      "ssh",
		SSHUser:        t.User,
		SSHHost:        t.Host,
		SSHPort:        t.Port,
		SSHConfigAlias: t.ConfigAlias,
		RemoteBinPath:  remoteBin,
		Serve:          serve,
		Default:        true,
	}
	store.Aliases = replaceAlias(store.Aliases, alias)
	store.DefaultAlias = name
	if err := config.SaveAliases(p, store); err != nil {
		return configError("setup: save alias store: %v", err)
	}
	o.diag("setup: recorded alias %q (default)", name)
	return nil
}

// replaceAlias returns aliases with any same-named entry replaced by a, else a
// appended. Other entries' Default flag is cleared so exactly one alias is the default.
func replaceAlias(aliases []config.Alias, a config.Alias) []config.Alias {
	out := make([]config.Alias, 0, len(aliases)+1)
	replaced := false
	for _, existing := range aliases {
		if existing.Name == a.Name {
			out = append(out, a)
			replaced = true
			continue
		}
		existing.Default = false
		out = append(out, existing)
	}
	if !replaced {
		out = append(out, a)
	}
	return out
}

// aliasName derives the saved alias name: the ~/.ssh/config Host alias if the spec
// used one, else the bare host. It keeps the name stable across re-runs (idempotent).
func aliasName(t sshx.Target, spec string) string {
	if t.ConfigAlias != "" {
		return t.ConfigAlias
	}
	// Strip any user@ and :port from the literal spec so the name is the host.
	s := spec
	if at := strings.LastIndex(s, "@"); at >= 0 {
		s = s[at+1:]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s[i+1:], "]") {
		// Only strip a trailing :port (a single numeric tail), not an IPv6 colon run.
		if strings.Count(s, ":") == 1 {
			s = s[:i]
		}
	}
	if s == "" {
		return t.Host
	}
	return s
}

// remoteHomeFor returns the conventional remote home directory for a user so the
// recorded path is absolute (PLAN §5.1 requires an ABSOLUTE remote path; a non-login
// SSH-exec PATH is minimal). root → /root; any other user → /home/<user>; empty → a
// best-effort /root (the common VPS default user).
func remoteHomeFor(user string) string {
	switch user {
	case "", "root":
		return "/root"
	default:
		return "/home/" + user
	}
}

// posixDir returns the POSIX directory portion of an absolute remote path (the remote
// is Linux/macOS, so '/' is the separator regardless of the local OS — never use
// filepath here, which would use the local separator).
func posixDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

// shellQuotePosix single-quotes s for a POSIX remote shell (the remote is Linux/macOS).
// It mirrors shellQuote but is named to make the remote-side intent explicit. Embedded
// single quotes use the '\” idiom.
func shellQuotePosix(s string) string {
	return shellQuote(s)
}
