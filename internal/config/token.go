package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// TokenStore is the cross-platform shared-secret storage boundary (PLAN §6.4).
// Implementations are auto-selected by --token-store keychain|secret-service|file:
//
//	macOS          → Keychain (service com.sani.clipbeam, account shared-token,
//	                 FROZEN + shared with ClipBeam.app)
//	Linux desktop  → Secret Service / libsecret (only when $DBUS_SESSION_BUS_ADDRESS
//	                 is set and the keyring is unlocked; never block on a locked one)
//	headless Linux → 0600 token file at $XDG_CONFIG_HOME/clipbeam/token (re-chmod'd
//	                 0600 on every load, PLAN §4.2)
//
// The token is never in argv; CLIPBEAM_TOKEN is honored for CI only.
type TokenStore interface {
	// Load returns the stored token, or ("", false, nil) if none is stored. A
	// non-nil error indicates a backend failure (not a missing token).
	Load() (token string, ok bool, err error)
	// Save persists the token, replacing any existing value.
	Save(token string) error
	// Kind reports the concrete backend ("keychain" | "secret-service" | "file")
	// for doctor diagnostics.
	Kind() string
}

// TokenStoreKind selects a concrete TokenStore backend.
type TokenStoreKind string

const (
	// TokenStoreAuto auto-selects per-OS (PLAN §6.4).
	TokenStoreAuto TokenStoreKind = "auto"
	// TokenStoreKeychain forces the macOS login Keychain.
	TokenStoreKeychain TokenStoreKind = "keychain"
	// TokenStoreSecretService forces Linux Secret Service / libsecret.
	TokenStoreSecretService TokenStoreKind = "secret-service"
	// TokenStoreFile forces the 0600 token file.
	TokenStoreFile TokenStoreKind = "file"
)

// KeychainService is the FROZEN macOS Keychain service name, shared with
// ClipBeam.app so a Mac running both shares one token (PLAN §6.4, Keychain.swift).
const KeychainService = "com.sani.clipbeam"

// KeychainAccount is the FROZEN Keychain account for the shared token.
const KeychainAccount = "shared-token"

// errTokenStoreUnavailable reports that a forced backend is not usable on this host
// (e.g. --token-store secret-service with no D-Bus session, or keychain off macOS).
var errTokenStoreUnavailable = errors.New("clipbeam: requested token store backend is unavailable on this host")

// OpenTokenStore selects and returns a TokenStore for the given kind, using the
// per-OS Paths for the file backend (PLAN §6.4):
//
//	TokenStoreAuto          → macOS Keychain; else Linux Secret Service if a D-Bus
//	                          session is present and secret-tool exists; else 0600 file.
//	TokenStoreKeychain      → macOS Keychain (error off macOS).
//	TokenStoreSecretService → Linux Secret Service via secret-tool (error if absent).
//	TokenStoreFile          → the 0600 token file (always available).
//
// The auto path NEVER blocks on a locked keyring — Secret Service is only auto-chosen
// when $DBUS_SESSION_BUS_ADDRESS is set and the secret-tool binary exists; any backend
// failure at use time still falls through to the caller as an error, never a hang.
func OpenTokenStore(kind TokenStoreKind, p Paths) (TokenStore, error) {
	switch kind {
	case TokenStoreKeychain:
		if runtime.GOOS != "darwin" {
			return nil, fmt.Errorf("%w: keychain requires macOS", errTokenStoreUnavailable)
		}
		return keychainStore{}, nil
	case TokenStoreSecretService:
		ss, ok := newSecretServiceStore()
		if !ok {
			return nil, fmt.Errorf("%w: secret-service requires a D-Bus session and the secret-tool binary", errTokenStoreUnavailable)
		}
		return ss, nil
	case TokenStoreFile:
		return newFileTokenStore(p), nil
	case TokenStoreAuto, "":
		if runtime.GOOS == "darwin" {
			return keychainStore{}, nil
		}
		if ss, ok := newSecretServiceStore(); ok {
			return ss, nil
		}
		return newFileTokenStore(p), nil
	default:
		return nil, fmt.Errorf("clipbeam: unknown token store kind %q", kind)
	}
}

// fileTokenStore is the headless-Linux 0600 token file backend (the common VPS case,
// PLAN §6.4.3/§4.2). The file is re-chmod'd 0600 on every load — a genuinely weaker
// guarantee than the macOS Keychain, documented plainly: on a VPS the box's disk
// security IS the boundary.
type fileTokenStore struct {
	path string
}

func newFileTokenStore(p Paths) fileTokenStore {
	path := p.Token
	if path == "" {
		// macOS Paths leaves Token empty (Keychain holds it); a forced file backend
		// there falls back to ~/.clipbeam/token so the backend is always usable.
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			path = home + "/.clipbeam/token"
		}
	}
	return fileTokenStore{path: path}
}

// Load reads the token file, re-chmod'ing it 0600 first (PLAN §4.2). A missing file
// is ("", false, nil); any other read error is surfaced. Surrounding whitespace
// (a trailing newline a human may have added) is trimmed.
func (f fileTokenStore) Load() (string, bool, error) {
	if f.path == "" {
		return "", false, errNoHome
	}
	if _, err := os.Stat(f.path); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("clipbeam: stat token file: %w", err)
	}
	// Re-tighten perms on every load before reading (defense for a file that may have
	// been created or copied with looser bits).
	_ = os.Chmod(f.path, 0o600)
	data, err := os.ReadFile(f.path)
	if err != nil {
		return "", false, fmt.Errorf("clipbeam: read token file: %w", err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", false, nil
	}
	return tok, true, nil
}

// Save writes the token to the 0600 file atomically (parent dir 0700). No trailing
// newline is added, so a round-trip is byte-stable.
func (f fileTokenStore) Save(token string) error {
	if f.path == "" {
		return errNoHome
	}
	return writeFileAtomic0600(f.path, []byte(token))
}

// Kind reports the backend name for doctor diagnostics.
func (f fileTokenStore) Kind() string { return "file" }

// keychainStore is the macOS login-Keychain backend. It shells /usr/bin/security so
// the binary stays CGO_ENABLED=0 / libSystem-only (PLAN §2.3, §6.4): no Security.framework
// cgo. The service/account are the FROZEN com.sani.clipbeam/shared-token item shared
// with ClipBeam.app so a Mac running both shares one token.
type keychainStore struct{}

// Load reads the generic-password item, or ("", false, nil) if absent. -w prints only
// the password to stdout. A non-found exit is treated as "no token", not an error.
func (keychainStore) Load() (string, bool, error) {
	cmd := exec.Command("/usr/bin/security", "find-generic-password",
		"-s", KeychainService, "-a", KeychainAccount, "-w")
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		// errSecItemNotFound surfaces as exit code 44 / a "could not be found" message.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("clipbeam: keychain read failed: %w", err)
	}
	tok := strings.TrimRight(out.String(), "\n")
	if tok == "" {
		return "", false, nil
	}
	return tok, true, nil
}

// Save deletes any prior item then adds a fresh one with
// AfterFirstUnlockThisDeviceOnly accessibility (-T "" allows no app to read it without
// prompting; -U updates in place). The password is passed via -w as a positional
// value — acceptable here because the token is the CLI's own freshly-generated secret
// written under `clipbeam setup`, not an attacker-controlled value, and `security`
// runs as the same uid (PLAN §6.4 forbids putting the token in the wire/SSH argv, not
// in a local keychain-write subprocess).
func (keychainStore) Save(token string) error {
	// Best-effort delete of any prior entry so add-generic-password does not collide.
	_ = exec.Command("/usr/bin/security", "delete-generic-password",
		"-s", KeychainService, "-a", KeychainAccount).Run()
	cmd := exec.Command("/usr/bin/security", "add-generic-password",
		"-s", KeychainService, "-a", KeychainAccount,
		"-w", token, "-U", "-A",
		"-T", "")
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clipbeam: keychain save failed: %w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	return nil
}

// Kind reports the backend name for doctor diagnostics.
func (keychainStore) Kind() string { return "keychain" }

// secretServiceStore is the Linux desktop Secret Service / libsecret backend. It
// shells secret-tool so the binary stays cgo-free, and is only selected when a D-Bus
// session is present (PLAN §6.4.2). The keyring is addressed by two attributes
// matching the macOS service/account so the lookup is unambiguous.
type secretServiceStore struct {
	bin string
}

// newSecretServiceStore reports whether the Secret Service backend is usable: a D-Bus
// session must be advertised AND the secret-tool binary must exist. It NEVER blocks on
// a locked keyring (the locked-keyring stall happens at Load/Save time, surfaced as an
// error to the caller — the auto path having already preferred this backend, callers
// fall back to the file store on the returned error).
func newSecretServiceStore() (secretServiceStore, bool) {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return secretServiceStore{}, false
	}
	bin, err := exec.LookPath("secret-tool")
	if err != nil {
		return secretServiceStore{}, false
	}
	return secretServiceStore{bin: bin}, true
}

// Load looks up the stored secret by attribute, or ("", false, nil) if absent.
func (s secretServiceStore) Load() (string, bool, error) {
	cmd := exec.Command(s.bin, "lookup",
		"service", KeychainService, "account", KeychainAccount)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// secret-tool lookup exits non-zero when the item is not found.
			return "", false, nil
		}
		return "", false, fmt.Errorf("clipbeam: secret-service read failed: %w", err)
	}
	tok := strings.TrimRight(out.String(), "\n")
	if tok == "" {
		return "", false, nil
	}
	return tok, true, nil
}

// Save stores the secret keyed by the service/account attributes. The token is fed on
// stdin (secret-tool store reads the secret from stdin), never argv, so it does not
// surface in the box's ps/proc.
func (s secretServiceStore) Save(token string) error {
	cmd := exec.Command(s.bin, "store", "--label", "clipbeam shared token",
		"service", KeychainService, "account", KeychainAccount)
	cmd.Stdin = strings.NewReader(token)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clipbeam: secret-service save failed: %w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	return nil
}

// Kind reports the backend name for doctor diagnostics.
func (s secretServiceStore) Kind() string { return "secret-service" }
