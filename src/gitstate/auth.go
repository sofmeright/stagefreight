package gitstate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport"

	sfxssh "github.com/PrPlanIT/StageFreight/src/ssh"
)

// isSSHURL returns true when the remote URL uses an SSH transport.
// Explicit match only — correctness over coverage.
func isSSHURL(url string) bool {
	return strings.HasPrefix(url, "ssh://") ||
		strings.HasPrefix(url, "git@")
}

// ResolveAuth resolves the go-git SSH transport auth method for a remote URL.
//
// Resolution order (exclusive — first match wins):
//  1. SSH_PRIVATE_KEY env var (in-memory, no filesystem dependency)
//  2. SSH agent (SSH_AUTH_SOCK)
//  3. Standard key files: id_ed25519, id_ecdsa, id_rsa
//
// Host key verification is resolved via sfxssh.ResolveHostKeyCallback (same priority
// as raw SSH transport — SSH_KNOWN_HOSTS_CONTENT, SSH_KNOWN_HOSTS, ~/.ssh/known_hosts,
// SSH_INSECURE_SKIP_HOST_KEY_CHECK).
//
// Returns an error when no auth is available — SSH auth failure is always fatal.
func ResolveAuth(remoteURL string) (transport.AuthMethod, error) {
	user := sshUser(remoteURL)

	cb, err := sfxssh.ResolveHostKeyCallback()
	if err != nil {
		return nil, fmt.Errorf("resolving SSH host key callback: %w", err)
	}

	// Priority 1: SSH_PRIVATE_KEY env var — authoritative, skips agent and filesystem.
	if keyContent := os.Getenv("SSH_PRIVATE_KEY"); keyContent != "" {
		signer, err := sfxssh.SignerFromDataEnv([]byte(keyContent))
		if err != nil {
			return nil, fmt.Errorf("invalid SSH_PRIVATE_KEY: %w", err)
		}
		pkAuth := &gitssh.PublicKeys{User: user, Signer: signer}
		pkAuth.HostKeyCallback = cb
		return pkAuth, nil
	}

	// Priority 2: SSH agent.
	if os.Getenv("SSH_AUTH_SOCK") != "" {
		agentAuth, err := gitssh.NewSSHAgentAuth(user)
		if err == nil {
			agentAuth.HostKeyCallback = cb
			return agentAuth, nil
		}
		// Agent socket present but auth failed — continue to key files rather
		// than failing, but don't hide the reason. TODO: route through diag.Debug.
	}

	// Priority 3: standard key files — try each, track last parse error.
	home, _ := os.UserHomeDir()
	var lastKeyErr error
	for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		keyPath := filepath.Join(home, ".ssh", name)
		if _, err := os.Stat(keyPath); err != nil {
			continue // file absent — not an error
		}
		signer, err := sfxssh.SignerFromFile(keyPath)
		if err != nil {
			lastKeyErr = fmt.Errorf("%s: %w", name, err)
			continue // file present but unparseable — record and try next
		}
		pkAuth := &gitssh.PublicKeys{User: user, Signer: signer}
		pkAuth.HostKeyCallback = cb
		return pkAuth, nil
	}

	if lastKeyErr != nil {
		return nil, fmt.Errorf("SSH key found but could not be loaded: %w", lastKeyErr)
	}
	return nil, fmt.Errorf(
		"no SSH auth available for %s — set SSH_PRIVATE_KEY, SSH_AUTH_SOCK, "+
			"or place a key at ~/.ssh/{id_ed25519,id_ecdsa,id_rsa}",
		remoteURL,
	)
}

// ResolveHTTPAuth returns HTTP basic auth for a remote URL.
// Currently returns nil (no auth) for public HTTPS repositories.
//
// TODO: implement credential resolution for private HTTPS remotes.
// Private GitLab/GitHub/Gitea repos using HTTPS will fail silently
// without credentials here. Planned: STAGEFREIGHT_GIT_USERNAME +
// STAGEFREIGHT_GIT_PASSWORD env vars, mirroring the GIT_ASKPASS pattern
// used in the git_mirror sync path.
func ResolveHTTPAuth(_ string) (*githttp.BasicAuth, error) {
	return nil, nil
}

// sshUser extracts the SSH username from a remote URL.
// git@host:path → "git", ssh://user@host:port/path → "user"
func sshUser(remoteURL string) string {
	if strings.HasPrefix(remoteURL, "ssh://") {
		rest := strings.TrimPrefix(remoteURL, "ssh://")
		if idx := strings.IndexByte(rest, '@'); idx > 0 {
			return rest[:idx]
		}
		return "git"
	}
	if idx := strings.IndexByte(remoteURL, '@'); idx > 0 {
		return remoteURL[:idx]
	}
	return "git"
}
