package ssh

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/skeema/knownhosts"
	gossh "golang.org/x/crypto/ssh"
)

// ResolveHostKeyCallback builds a gossh.HostKeyCallback from the known_hosts file.
// Checks $SSH_KNOWN_HOSTS first (set by GitLab CI runner), then ~/.ssh/known_hosts.
// Returns an error if no known_hosts file is found — InsecureIgnoreHostKey is never used.
//
// This is the single source of truth for host key verification across all SSH
// transports in StageFreight (git, docker, and any future transports).
func ResolveHostKeyCallback() (gossh.HostKeyCallback, error) {
	path := os.Getenv("SSH_KNOWN_HOSTS")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf(
				"SSH_KNOWN_HOSTS is not set and home dir is unresolvable: %w — "+
					"set SSH_KNOWN_HOSTS to a valid known_hosts file",
				err,
			)
		}
		path = filepath.Join(home, ".ssh", "known_hosts")
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf(
			"SSH known_hosts not found at %s — set SSH_KNOWN_HOSTS env var or create ~/.ssh/known_hosts; "+
				"add hosts with: ssh-keyscan <host> >> ~/.ssh/known_hosts",
			path,
		)
	}

	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("loading known_hosts from %s: %w", path, err)
	}
	// knownhosts.HostKeyCallback is a distinct named type, not a type alias for
	// gossh.HostKeyCallback. Wrap it here — once — so every caller receives the
	// canonical crypto/ssh type without a conversion at each call site.
	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		return cb(hostname, remote, key)
	}, nil
}
