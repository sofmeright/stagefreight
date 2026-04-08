// Package ssh provides SSH authentication and host key resolution for all
// SSH transports in StageFreight. It is the single authority for:
//
//   - SSH agent discovery (SSH_AUTH_SOCK)
//   - Private key file and in-memory resolution and parsing
//   - known_hosts host key verification
//
// Both git-over-SSH (gitstate) and raw SSH execution (docker transport) depend
// on this package. No other package resolves SSH credentials or host keys.
package ssh

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	gossh "golang.org/x/crypto/ssh"
	gosshagent "golang.org/x/crypto/ssh/agent"
)

// ResolveAuthMethods returns golang.org/x/crypto/ssh auth methods for raw SSH
// connections (remote command execution, file transfer, tunneling).
//
// Resolution is exclusive — the first matching source wins entirely:
//  1. SSH_PRIVATE_KEY env var (PEM content; fails hard if set but invalid)
//  2. SSH_AUTH_SOCK agent
//  3. keyPath argument if non-empty
//  4. Standard key files: id_ed25519, id_ecdsa, id_rsa (first match wins)
//
// SSH_PRIVATE_KEY is authoritative — when set, agent and filesystem are skipped,
// ensuring identical behavior across laptop, container, and CI.
//
// Returns an error only when no method could be resolved at all.
// For git-over-SSH, callers should use gitstate.ResolveAuth instead.
func ResolveAuthMethods(keyPath string) ([]gossh.AuthMethod, error) {
	// SSH_PRIVATE_KEY is authoritative — skip all other sources when set.
	if keyContent := os.Getenv("SSH_PRIVATE_KEY"); keyContent != "" {
		signer, err := SignerFromDataEnv([]byte(keyContent))
		if err != nil {
			return nil, fmt.Errorf("invalid SSH_PRIVATE_KEY: %w", err)
		}
		return []gossh.AuthMethod{gossh.PublicKeys(signer)}, nil
	}

	var methods []gossh.AuthMethod

	// SSH agent — covers GitLab CI and local dev with forwarded agent.
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			methods = append(methods, gossh.PublicKeysCallback(gosshagent.NewClient(conn).Signers))
		}
	}

	if len(methods) > 0 {
		return methods, nil
	}

	// Explicit key path overrides the standard search.
	if keyPath != "" {
		if signer, err := SignerFromFile(keyPath); err == nil {
			return []gossh.AuthMethod{gossh.PublicKeys(signer)}, nil
		}
	} else {
		home, _ := os.UserHomeDir()
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			if signer, err := SignerFromFile(filepath.Join(home, ".ssh", name)); err == nil {
				return []gossh.AuthMethod{gossh.PublicKeys(signer)}, nil
			}
		}
	}

	return nil, fmt.Errorf(
		"no SSH auth available: SSH_PRIVATE_KEY not set, SSH_AUTH_SOCK not set or socket unavailable, "+
			"and no usable key at %q or ~/.ssh/{id_ed25519,id_ecdsa,id_rsa}",
		keyPath,
	)
}

// SignerFromDataEnv parses a PEM private key, using SSH_PRIVATE_KEY_PASSPHRASE
// if set. Single source of truth for env-driven key parsing — used by both
// ResolveAuthMethods (raw SSH) and gitstate.ResolveAuth (go-git SSH transport).
func SignerFromDataEnv(data []byte) (gossh.Signer, error) {
	if passphrase := os.Getenv("SSH_PRIVATE_KEY_PASSPHRASE"); passphrase != "" {
		return SignerFromDataWithPassphrase(data, []byte(passphrase))
	}
	return SignerFromData(data)
}

// SignerFromFile parses an SSH private key file and returns a gossh.Signer.
// Returns os.ErrNotExist if the file is absent; other errors indicate parse failures.
func SignerFromFile(keyPath string) (gossh.Signer, error) {
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	return gossh.ParsePrivateKey(data)
}

// SignerFromData parses an in-memory PEM private key and returns a gossh.Signer.
// Use SignerFromDataWithPassphrase for encrypted keys.
func SignerFromData(data []byte) (gossh.Signer, error) {
	return gossh.ParsePrivateKey(data)
}

// SignerFromDataWithPassphrase parses an encrypted in-memory PEM private key
// using the supplied passphrase.
func SignerFromDataWithPassphrase(data, passphrase []byte) (gossh.Signer, error) {
	return gossh.ParsePrivateKeyWithPassphrase(data, passphrase)
}
