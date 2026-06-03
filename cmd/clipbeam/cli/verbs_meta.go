package cli

import (
	"context"
	"errors"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// cmdVersion is the resolved CLI semver, set once by Execute from the injected build
// version. It backs `version`, `schema`, and the /health App version.
var cmdVersion = "dev"

// defaultMaxBytes mirrors wire.DefaultMaxBytes (50 MiB) as a package constant so the
// ingest/serve paths default the cap without re-importing wire at every call site.
const defaultMaxBytes = wire.DefaultMaxBytes

// isErr is errors.Is, kept short for the dense sentinel-mapping switches.
func isErr(err, target error) bool { return errors.Is(err, target) }

// versionJSON is the `version --json` shape (PLAN §8.2): a NET-NEW CLI surface (the
// shim has no version). setup-idempotency (§9.5) + self-update (§9.6) compare the
// "version" semver field specifically.
type versionJSON struct {
	Schema       string `json:"schema"`
	OK           bool   `json:"ok"`
	Version      string `json:"version"`
	Commit       string `json:"commit"`
	WireProtocol string `json:"wireProtocol"`
}

// runVersion implements `clipbeam version` (PLAN §8.2). Default mode prints the bare
// semver (with a newline — it is a human line, not the bare-path contract); --json
// emits {schema,ok,version,commit,wireProtocol}.
func runVersion(o out) error {
	v := cmdVersion
	commit := vcsCommit()
	if o.json {
		return o.emitJSON(versionJSON{
			Schema:       schemaVersion,
			OK:           true,
			Version:      v,
			Commit:       commit,
			WireProtocol: wireProtocol,
		})
	}
	o.dataln(v)
	return nil
}

// vcsCommit returns the VCS revision stamped into the build info, or "dev" for a
// source build with no VCS stamp (PLAN §8.2: commit is "<vcs-or-dev>").
func vcsCommit() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "dev"
}

// runHealth implements `clipbeam health [target]` (PLAN §8.2). With no target it
// checks the LOCAL daemon's /health; the body is the frozen {ok,app,version,host,
// platform} shape — a liveness check tests ok==true, never the app/platform strings
// (PLAN §8.9). The remote-target form drives SSH/Tailscale and is wired in the SSH
// phase; until then a target argument surfaces the sshx not-implemented error.
func runHealth(o out, target string) error {
	if target != "" {
		// A remote /health probe rides the same transport as a remote send (SSH/Tailscale);
		// the dial path is the sshx interface, stubbed until the SSH phase.
		return remoteNotReady("health", target)
	}
	c, err := loadControl()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, body, err := c.get(ctx, "/health")
	if err != nil {
		return coded(ExitUnreachable, transportErr(err))
	}
	if status != 200 {
		return statusErr(status, body)
	}
	// stdout = the verbatim /health JSON (the deliverable). It already matches the
	// frozen key names; --json mode also emits it verbatim (it is already JSON).
	o.dataln(string(body))
	return nil
}

// runDoctor implements `clipbeam doctor` (PLAN §4.2/§6.4): config + token-storage +
// permissions diagnostics. It WARNS (stderr) on a group/other-readable token file
// (§4.2). The structured findings go to stdout under --json; the human report goes to
// stderr. It returns a non-zero exit only on a hard config error, never on a warning.
func runDoctor(o out) error {
	p, err := config.Resolve()
	if err != nil {
		return configError("%v", err)
	}
	cfg, cfgErr := config.Load()

	findings := doctorReport{Schema: schemaVersion, OK: true, Platform: platformString()}

	// Config.
	if cfgErr != nil {
		findings.OK = false
		findings.Config = "error: " + cfgErr.Error()
		o.diag("doctor: config: %v", cfgErr)
	} else {
		findings.Config = "ok"
		findings.Port = int(cfg.Port)
		o.diag("doctor: config loaded (port %d, maxBytes %d)", cfg.Port, maxBytesOf(cfg))
	}

	// Token store.
	ts, tsErr := config.OpenTokenStore(config.TokenStoreAuto, p)
	if tsErr != nil {
		findings.TokenStore = "unavailable: " + tsErr.Error()
		o.diag("doctor: token store unavailable: %v", tsErr)
	} else {
		findings.TokenStore = ts.Kind()
		_, hasToken, loadErr := ts.Load()
		findings.TokenPresent = hasToken && loadErr == nil
		if loadErr != nil {
			o.diag("doctor: token load error: %v", loadErr)
		} else if hasToken {
			o.diag("doctor: token store=%s, token present", ts.Kind())
		} else {
			o.diag("doctor: token store=%s, NO token (run `clipbeam setup`)", ts.Kind())
		}
	}

	// Token-file permission warning (§4.2): warn on group/other-readable bits.
	if p.Token != "" {
		if mode, ok := fileMode(p.Token); ok {
			findings.TokenFileMode = mode.String()
			if mode.Perm()&0o077 != 0 {
				findings.TokenFileWarning = "token file is group/other-readable; expected 0600"
				o.diag("doctor: WARNING: token file %s mode %v is group/other-readable (expected 0600)", p.Token, mode.Perm())
			}
		}
	}

	if o.json {
		return o.emitJSON(findings)
	}
	o.diag("doctor: %s", okWord(findings.OK))
	return nil
}

// doctorReport is the `doctor --json` shape. It is intentionally NOT a frozen wire
// surface (it is a CLI diagnostic), so it carries the clipbeam.v1 CLI schema tag.
type doctorReport struct {
	Schema           string `json:"schema"`
	OK               bool   `json:"ok"`
	Config           string `json:"config"`
	Port             int    `json:"port,omitempty"`
	TokenStore       string `json:"tokenStore"`
	TokenPresent     bool   `json:"tokenPresent"`
	TokenFileMode    string `json:"tokenFileMode,omitempty"`
	TokenFileWarning string `json:"tokenFileWarning,omitempty"`
	Platform         string `json:"platform"`
}

// fileMode stats path and returns its os.FileMode + whether the stat succeeded.
func fileMode(path string) (os.FileMode, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return fi.Mode(), true
}

// okWord renders a bool as a short human word for the doctor summary.
func okWord(ok bool) string {
	if ok {
		return "all checks passed"
	}
	return "problems found (see above)"
}

// platformString is the GOOS used for diagnostics (the /health platform value is the
// server's own; this mirrors it for the doctor report).
func platformString() string { return runtime.GOOS }
