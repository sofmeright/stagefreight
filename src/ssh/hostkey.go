package ssh

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/skeema/knownhosts"
	gossh "golang.org/x/crypto/ssh"
)

// ResolveHostKeyCallback builds a gossh.HostKeyCallback for SSH host key verification.
//
// Resolution order:
//  1. SSH_KNOWN_HOSTS_CONTENT env var (raw known_hosts lines — for containers/CI)
//  2. SSH_KNOWN_HOSTS env var (path to file — set by GitLab CI runner)
//  3. ~/.ssh/known_hosts
//  4. SSH_INSECURE_SKIP_HOST_KEY_CHECK=true (last resort — emits warning, never silent)
//
// InsecureIgnoreHostKey is never used implicitly. If no known_hosts source is found
// and SSH_INSECURE_SKIP_HOST_KEY_CHECK is not set, an actionable error is returned.
//
// This is the single source of truth for host key verification across all SSH
// transports in StageFreight (git, docker, and any future transports).
func ResolveHostKeyCallback() (gossh.HostKeyCallback, error) {
	// 1. Inline content via env var — preferred for containers (no mount required).
	if content := os.Getenv("SSH_KNOWN_HOSTS_CONTENT"); content != "" {
		return hostKeyCallbackFromContent(content)
	}

	// 2. File path via env var.
	path := os.Getenv("SSH_KNOWN_HOSTS")
	if path == "" {
		// 3. Default ~/.ssh/known_hosts.
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, ".ssh", "known_hosts")
		}
	}

	if path != "" {
		if _, err := os.Stat(path); err == nil {
			return hostKeyCallbackFromFile(path)
		}
	}

	// 4. Insecure fallback — only when explicitly opted in.
	if os.Getenv("SSH_INSECURE_SKIP_HOST_KEY_CHECK") == "true" {
		fmt.Fprintln(os.Stderr, "WARNING: SSH host key verification disabled — "+
			"set SSH_KNOWN_HOSTS_CONTENT or SSH_KNOWN_HOSTS for production use")
		return gossh.InsecureIgnoreHostKey(), nil //nolint:gosec — opt-in dev fallback
	}

	return nil, fmt.Errorf(
		"SSH known_hosts not found — set SSH_KNOWN_HOSTS_CONTENT (inline) or SSH_KNOWN_HOSTS (path); "+
			"for local dev: add hosts with ssh-keyscan <host> >> ~/.ssh/known_hosts; "+
			"for containers: -e SSH_KNOWN_HOSTS_CONTENT=\"$(ssh-keyscan <host>)\"",
	)
}

// hostKeyCallbackFromContent parses raw known_hosts lines from env/string content.
// Writes to a temp file, parses synchronously, then removes the file immediately —
// knownhosts.New reads all entries into memory before returning, so the callback
// holds no file reference after this function returns.
func hostKeyCallbackFromContent(content string) (gossh.HostKeyCallback, error) {
	tmp, err := os.CreateTemp("", "sf-known_hosts-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp known_hosts: %w", err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		os.Remove(tmp.Name())
		return nil, fmt.Errorf("writing temp known_hosts: %w", err)
	}
	tmp.Close()
	cb, err := knownhosts.New(tmp.Name())
	os.Remove(tmp.Name()) // synchronous — knownhosts.New has fully parsed into memory
	if err != nil {
		return nil, fmt.Errorf("parsing SSH_KNOWN_HOSTS_CONTENT: %w", err)
	}
	return wrapCallback(cb), nil
}

// hostKeyCallbackFromFile loads known_hosts from a file path.
func hostKeyCallbackFromFile(path string) (gossh.HostKeyCallback, error) {
	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("loading known_hosts from %s: %w", path, err)
	}
	return wrapCallback(cb), nil
}

// wrapCallback wraps knownhosts.HostKeyCallback (a distinct named type) into
// gossh.HostKeyCallback — done once here so every caller gets the canonical type.
func wrapCallback(cb knownhosts.HostKeyCallback) gossh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		return cb(hostname, remote, key)
	}
}
