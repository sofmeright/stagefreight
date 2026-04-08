// Package ssh provides SSH authentication and host key resolution for all
// SSH transports in StageFreight. It is the single authority for:
//
//   - SSH agent discovery (SSH_AUTH_SOCK)
//   - Private key file resolution and parsing
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
// Resolution order:
//  1. SSH agent via $SSH_AUTH_SOCK (GitLab CI, local dev with forwarded agent)
//  2. keyPath if non-empty
//  3. Standard key files: id_ed25519, id_ecdsa, id_rsa (first match wins)
//
// Returns an error only when no method could be resolved at all.
// For git-over-SSH, callers should use gitstate.ResolveAuth instead.
func ResolveAuthMethods(keyPath string) ([]gossh.AuthMethod, error) {
	var methods []gossh.AuthMethod

	// SSH agent first — covers GitLab CI and local dev with forwarded agent.
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			methods = append(methods, gossh.PublicKeysCallback(gosshagent.NewClient(conn).Signers))
		}
	}

	// Explicit key path overrides the standard search.
	if keyPath != "" {
		if signer, err := SignerFromFile(keyPath); err == nil {
			methods = append(methods, gossh.PublicKeys(signer))
		}
	} else {
		home, _ := os.UserHomeDir()
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			if signer, err := SignerFromFile(filepath.Join(home, ".ssh", name)); err == nil {
				methods = append(methods, gossh.PublicKeys(signer))
				break
			}
		}
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf(
			"no SSH auth available: SSH_AUTH_SOCK not set or socket unavailable, "+
				"and no usable key at %q or ~/.ssh/{id_ed25519,id_ecdsa,id_rsa}",
			keyPath,
		)
	}
	return methods, nil
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
