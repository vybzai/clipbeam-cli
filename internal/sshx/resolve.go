package sshx

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kevinburke/ssh_config"
)

// configResolver abstracts ssh_config lookups so ResolveTarget is unit-testable
// without a real ~/.ssh/config on disk. The default implementation reads the user's
// config via kevinburke/ssh_config.
type configResolver interface {
	// Get returns the first value of key for alias ("" if unset).
	Get(alias, key string) string
	// GetAll returns every value of key for alias (for IdentityFile, which may repeat).
	GetAll(alias, key string) []string
}

// defaultConfigResolver reads ~/.ssh/config (and any system config) through the
// kevinburke/ssh_config package's process-global UserSettings.
type defaultConfigResolver struct{}

func (defaultConfigResolver) Get(alias, key string) string {
	return ssh_config.Get(alias, key)
}

func (defaultConfigResolver) GetAll(alias, key string) []string {
	return ssh_config.GetAll(alias, key)
}

// resolveTargetWith resolves spec against a configResolver (PLAN §5.4/§5.5). The spec
// is either user@host[:port] or a bare host / ~/.ssh/config Host alias. It applies
// ssh_config HostName/User/Port/IdentityFile, defaults the port to 22, and detects an
// unsupported ProxyJump/Include (ErrProxyJumpUnsupported) — kevinburke/ssh_config does
// not expand either, so a configured ProxyJump is a documented v1 non-goal that must
// fail with a SPECIFIC error, never an opaque handshake failure (PLAN §5.4).
func resolveTargetWith(r configResolver, spec string) (Target, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Target{}, fmt.Errorf("clipbeam: empty SSH target")
	}

	userPart, hostPart, port, err := parseSpec(spec)
	if err != nil {
		return Target{}, err
	}

	// The alias used for ssh_config lookups is the literal host token the user typed
	// (a ~/.ssh/config Host alias matches on this token, before HostName rewrites it).
	alias := hostPart

	// ProxyJump / Include are documented non-goals — kevinburke/ssh_config does not
	// expand them, so honoring a config that sets ProxyJump would silently dial the
	// WRONG host. Fail with the specific sentinel instead (PLAN §5.4).
	if pj := r.Get(alias, "ProxyJump"); pj != "" && !strings.EqualFold(pj, "none") {
		return Target{}, fmt.Errorf("%w (host %q sets ProxyJump %q): use a plain reachable host or the Mac GUI's system-ssh path",
			ErrProxyJumpUnsupported, alias, pj)
	}
	if pc := r.Get(alias, "ProxyCommand"); pc != "" && !strings.EqualFold(pc, "none") {
		return Target{}, fmt.Errorf("%w (host %q sets ProxyCommand): clipbeam v1 dials in-process and does not shell a proxy",
			ErrProxyJumpUnsupported, alias)
	}

	// HostName rewrite (an alias may map to a different real host).
	host := hostPart
	if hn := r.Get(alias, "HostName"); hn != "" {
		host = hn
	}

	// User: explicit user@ wins; else ssh_config User; else leave empty (the dial
	// defaults it to $USER).
	user := userPart
	if user == "" {
		user = r.Get(alias, "User")
	}

	// Port: explicit :port wins; else ssh_config Port; else 22.
	if port == 0 {
		if ps := r.Get(alias, "Port"); ps != "" {
			if pn, perr := strconv.Atoi(ps); perr == nil && pn > 0 {
				port = pn
			}
		}
	}
	if port == 0 {
		port = 22
	}

	// IdentityFile(s): expand ~ and resolve relative to $HOME; skip ssh_config's
	// built-in defaults that do not exist on disk (it returns ~/.ssh/id_rsa etc.).
	identities := expandIdentityFiles(r.GetAll(alias, "IdentityFile"))

	configAlias := ""
	if alias != host {
		configAlias = alias
	}

	return Target{
		User:          user,
		Host:          host,
		Port:          port,
		IdentityFiles: identities,
		ConfigAlias:   configAlias,
	}, nil
}

// parseSpec splits user@host[:port] (or bare host) into its parts. An IPv6 literal
// host must be bracketed ([::1]:22); a bare IPv6 without brackets is treated as a
// hostname with no port. Returns (user, host, port, err) where port==0 means unset.
func parseSpec(spec string) (user, host string, port int, err error) {
	rest := spec
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		user = rest[:at]
		rest = rest[at+1:]
	}
	if rest == "" {
		return "", "", 0, fmt.Errorf("clipbeam: SSH target has no host: %q", spec)
	}

	// Bracketed IPv6: [host]:port or [host].
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			return "", "", 0, fmt.Errorf("clipbeam: malformed IPv6 SSH target: %q", spec)
		}
		host = rest[1:end]
		tail := rest[end+1:]
		if strings.HasPrefix(tail, ":") {
			p, perr := strconv.Atoi(tail[1:])
			if perr != nil || p <= 0 {
				return "", "", 0, fmt.Errorf("clipbeam: invalid SSH port in %q", spec)
			}
			port = p
		}
		return user, host, port, nil
	}

	// Unbracketed: a single colon means host:port; more than one colon is a bare IPv6
	// literal (no port) per the SSH convention.
	if strings.Count(rest, ":") == 1 {
		i := strings.Index(rest, ":")
		host = rest[:i]
		p, perr := strconv.Atoi(rest[i+1:])
		if perr != nil || p <= 0 {
			return "", "", 0, fmt.Errorf("clipbeam: invalid SSH port in %q", spec)
		}
		port = p
		return user, host, port, nil
	}
	host = rest
	return user, host, port, nil
}

// expandIdentityFiles expands ~ to $HOME in each IdentityFile and keeps only files
// that actually exist on disk — ssh_config.GetAll returns the built-in default list
// (~/.ssh/id_rsa, id_ecdsa, …) even when the user set none, so filtering to existing
// files avoids feeding non-existent paths to the auth chain. An empty result lets the
// auth provider fall back to its own default-key probe (PLAN §5.4).
func expandIdentityFiles(raw []string) []string {
	var out []string
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	for _, f := range raw {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if strings.HasPrefix(f, "~/") && home != "" {
			f = filepath.Join(home, f[2:])
		}
		if seen[f] {
			continue
		}
		if _, err := os.Stat(f); err != nil {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}
