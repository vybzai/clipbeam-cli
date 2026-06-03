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
	methods(t Target) ([]ssh.AuthMethod, error)
}

// defaultKeyNames are the conventional default private keys probed when ssh_config
// names no IdentityFile (PLAN §5.4), in preference order.
var defaultKeyNames = []string{"id_ed25519", "id_ecdsa", "id_rsa"}

// fileAuthProvider is the production authProvider: ssh-agent first, then key files.
type fileAuthProvider struct{}

// defaultAuthProvider returns the production authProvider.
func defaultAuthProvider() authProvider { return fileAuthProvider{} }

// methods assembles the auth method list: the agent callback (if $SSH_AUTH_SOCK is
// reachable) followed by a PublicKeys method over every loadable unencrypted key from
// the target's IdentityFiles or the default key set. Encrypted keys are skipped here
// (no prompt under a data verb); the agent is the path for those (PLAN §5.4).
func (fileAuthProvider) methods(t Target) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if am, ok := agentAuthMethod(); ok {
		methods = append(methods, am)
	}

	signers := loadKeySigners(keyCandidates(t))
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("%w: no ssh-agent and no usable private key (set $SSH_AUTH_SOCK or run `clipbeam setup`)", ErrDialFailed)
	}
	return methods, nil
}

// agentAuthMethod connects to the ssh-agent at $SSH_AUTH_SOCK and returns a
// PublicKeysCallback method backed by it (PLAN §5.4). It returns (nil, false) when no
// agent is advertised or the socket is unreachable — the key-file path then carries
// the auth.
func agentAuthMethod() (ssh.AuthMethod, bool) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, false
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, false
	}
	ag := agent.NewClient(conn)
	return ssh.PublicKeysCallback(ag.Signers), true
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
