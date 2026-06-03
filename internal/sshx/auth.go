package sshx

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// authProvider builds the SSH auth methods for a target in the locked order (PLAN
// §5.4): (1) ssh-agent via $SSH_AUTH_SOCK; (2) the target's IdentityFiles, else the
// default keys (id_ed25519, id_rsa, id_ecdsa). A passphrase prompt is only ever done
// under `clipbeam setup` (the CLI passes the prompt in via the setup verb; the data
// verbs never prompt — an encrypted key with no agent simply yields no usable method).
type authProvider interface {
	// methods returns the merged publickey auth method plus a cleanup func that the caller
	// MUST call after the dial completes (success or failure). cleanup closes any
	// lazily-opened ssh-agent socket connection so it is not leaked for the lifetime of the
	// process (the agent signers may be used through the whole handshake, so the conn cannot
	// be closed before the dial returns — cleanup is deferred to after it). cleanup is
	// never nil and is safe to call exactly once.
	methods(t Target) (methods []ssh.AuthMethod, cleanup func(), err error)
}

// defaultKeyNames are the conventional default private keys probed when ssh_config
// names no IdentityFile (PLAN §5.4), in preference order.
var defaultKeyNames = []string{"id_ed25519", "id_ecdsa", "id_rsa"}

// fileAuthProvider is the production authProvider: ssh-agent first, then key files.
type fileAuthProvider struct{}

// defaultAuthProvider returns the production authProvider.
func defaultAuthProvider() authProvider { return fileAuthProvider{} }

// methods assembles a SINGLE publickey auth method that merges the ssh-agent's signers
// (if $SSH_AUTH_SOCK is reachable) with every loadable unencrypted key from the target's
// IdentityFiles or the default key set, in that order. Agent signers come FIRST so a
// loaded agent identity is still preferred; the on-disk file signers follow.
//
// Why one merged method and not two: x/crypto's client_auth dedupes by RFC-4252 method
// NAME ("publickey") — both an agent PublicKeysCallback and a file PublicKeys report the
// same name, so once an EMPTY agent's publickey attempt returns authFailure the client
// marks "publickey" as tried and never offers the on-disk method (the observed
// `attempted methods [none publickey]` failure when SSH_AUTH_SOCK is set but the agent
// holds 0 identities). Merging both candidate sets under one lazy callback makes the
// client walk every candidate under one attempt, so an empty/broken agent never poisons
// the file keys (PLAN §5.4). Encrypted keys are skipped here (no prompt under a data
// verb).
func (fileAuthProvider) methods(t Target) ([]ssh.AuthMethod, func(), error) {
	noopCleanup := func() {}
	agentFn, agentOK := agentSignersFunc()
	fileSigners := loadKeySigners(keyCandidates(t))

	// No agent advertised AND no usable file key → nothing to offer (the existing
	// no-method error). An advertised-but-empty agent still counts as "available" here,
	// but the merged callback yields just the file signers, so this guard only trips
	// when there is genuinely no public-key material at all.
	if !agentOK && len(fileSigners) == 0 {
		return nil, noopCleanup, fmt.Errorf("%w: no ssh-agent and no usable private key (set $SSH_AUTH_SOCK or run `clipbeam setup`)", ErrDialFailed)
	}

	// agentConn holds the ssh-agent socket connection once the lazy callback opens it, so
	// the deferred cleanup can Close it after the dial. The agent signers reference this
	// conn for every Sign during the handshake, so it MUST stay open until the dial
	// completes — cleanup is the caller's post-dial defer, not an in-callback close.
	var agentConn net.Conn
	merged := ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
		var out []ssh.Signer
		if agentOK {
			// Swallow a broken/empty agent's error so it never poisons the file keys: an
			// empty agent yields nil signers, a dead socket yields an error we ignore.
			if conn, ags, err := agentFn(); err == nil {
				agentConn = conn
				out = append(out, ags...)
			} else if conn != nil {
				// Dial succeeded but Signers() failed — still close the conn (no leak).
				_ = conn.Close()
			}
		}
		out = append(out, fileSigners...)
		// Never return an error: an empty agent + no file keys is impossible here (guarded
		// above), and a present-but-empty result is a legitimate "no candidates" answer.
		return out, nil
	})
	cleanup := func() {
		if agentConn != nil {
			_ = agentConn.Close()
			agentConn = nil
		}
	}
	return []ssh.AuthMethod{merged}, cleanup, nil
}

// agentSignersFunc returns a lazy accessor for the ssh-agent's signers at
// $SSH_AUTH_SOCK (PLAN §5.4). The socket is contacted ONLY when the returned func is
// called (at auth time), never at assembly time. It returns (nil, false) when no agent
// is advertised; an unreachable socket still returns (fn, true) — the dial error is
// surfaced lazily from fn and swallowed by the merged callback, so a broken agent never
// preempts the on-disk key path.
//
// The lazy func also returns the opened net.Conn so the caller can Close it after the
// dial completes (the returned signers reference this conn for every Sign during the
// handshake, so it cannot be closed before the dial returns — closing it here would break
// signing). On a dial error the conn is nil; on a Signers() error the conn is returned
// non-nil so the caller can close it without leaking.
func agentSignersFunc() (func() (net.Conn, []ssh.Signer, error), bool) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, false
	}
	return func() (net.Conn, []ssh.Signer, error) {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			return nil, nil, err
		}
		signers, serr := agent.NewClient(conn).Signers()
		if serr != nil {
			return conn, nil, serr
		}
		return conn, signers, nil
	}, true
}

// keyCandidates returns the private-key file paths to try, in order: the target's
// resolved IdentityFiles first, else the default keys under ~/.ssh (PLAN §5.4).
func keyCandidates(t Target) []string {
	if len(t.IdentityFiles) > 0 {
		return t.IdentityFiles
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	var out []string
	for _, name := range defaultKeyNames {
		out = append(out, filepath.Join(home, ".ssh", name))
	}
	return out
}

// loadKeySigners parses each candidate key file into a Signer, skipping missing files
// and encrypted keys (which need a passphrase — not prompted under data verbs). Only
// keys that load cleanly become signers.
func loadKeySigners(paths []string) []ssh.Signer {
	var signers []ssh.Signer
	for _, p := range paths {
		pem, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			// An encrypted key returns *ssh.PassphraseMissingError; skip it here (the
			// agent is the path for encrypted keys; setup handles the passphrase prompt).
			continue
		}
		signers = append(signers, signer)
	}
	return signers
}
