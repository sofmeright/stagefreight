package gitstate

import (
	"fmt"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// SyncSession is opened once per sync/push operation.
// State is resolved once at Open() and explicitly refreshed only after
// state-changing operations (Fetch, FastForward). Remote polling never
// happens opportunistically.
type SyncSession struct {
	repo      *git.Repository
	rootDir   string
	state     RepoState
	auth      transport.AuthMethod
	httpAuth  *githttp.BasicAuth
	fetched   bool // true after Fetch() invalidates ahead/behind
}

// OpenSyncSession opens a SyncSession for the repository at rootDir.
// Reads repo state and resolves auth once; both are reused throughout the session.
// Returns an error if the remote URL uses SSH and auth cannot be resolved.
func OpenSyncSession(rootDir string) (*SyncSession, error) {
	repo, err := OpenRepo(rootDir)
	if err != nil {
		return nil, fmt.Errorf("opening repo at %s: %w", rootDir, err)
	}

	state, err := ReadRepoState(repo)
	if err != nil {
		return nil, fmt.Errorf("reading repo state: %w", err)
	}

	var auth transport.AuthMethod
	var httpAuth *githttp.BasicAuth

	// Resolve auth for the effective remote. When no upstream is configured
	// (first push), RemoteName is empty — fall back to "origin" so auth is
	// always resolved before Push is called. go-git has no auth fallback of
	// its own and will error with "SSH agent not found" if auth is nil.
	effectiveRemote := state.RemoteName
	if effectiveRemote == "" {
		effectiveRemote = "origin"
	}
	remoteURL, remoteErr := RemoteURL(repo, effectiveRemote)
	if remoteErr != nil {
		return nil, fmt.Errorf("resolving remote URL for %s: %w", effectiveRemote, remoteErr)
	}
	if isSSHURL(remoteURL) {
		// SSH transport: auth failure is fatal — no silent nil
		sshAuth, authErr := ResolveAuth(remoteURL)
		if authErr != nil {
			return nil, fmt.Errorf("resolving SSH auth for %s: %w", remoteURL, authErr)
		}
		auth = sshAuth
	} else {
		// HTTPS transport: nil auth is acceptable (public repos)
		ha, authErr := ResolveHTTPAuth(remoteURL)
		if authErr != nil {
			return nil, fmt.Errorf("resolving HTTP auth for %s: %w", remoteURL, authErr)
		}
		httpAuth = ha
		if ha != nil {
			auth = ha
		}
	}

	return &SyncSession{
		repo:     repo,
		rootDir:  rootDir,
		state:    state,
		auth:     auth,
		httpAuth: httpAuth,
	}, nil
}

// State returns the current resolved repo state.
func (s *SyncSession) State() RepoState {
	return s.state
}

// Repo returns the underlying git.Repository.
func (s *SyncSession) Repo() *git.Repository {
	return s.repo
}

// Auth returns the resolved transport auth (may be nil for public HTTPS remotes).
func (s *SyncSession) Auth() transport.AuthMethod {
	return s.auth
}

// Refresh re-reads repo state after a mutation (fetch, fast-forward, reset).
func (s *SyncSession) Refresh() error {
	state, err := ReadRepoState(s.repo)
	if err != nil {
		return fmt.Errorf("refreshing repo state: %w", err)
	}
	s.state = state
	return nil
}

// Fetch fetches from the configured remote using targeted refspecs.
// Only fetches refs/heads/* to avoid fetching tags or other namespaces implicitly.
// Updates fetched flag and refreshes state.
func (s *SyncSession) Fetch(remote string) error {
	// Explicit refspec: only fetch branch heads, not tags or other namespaces
	refspec := gitconfig.RefSpec(fmt.Sprintf("+refs/heads/*:refs/remotes/%s/*", remote))

	err := s.repo.Fetch(&git.FetchOptions{
		RemoteName: remote,
		RefSpecs:   []gitconfig.RefSpec{refspec},
		Auth:       s.auth,
	})
	if err == git.NoErrAlreadyUpToDate {
		s.fetched = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetch %s: %w", remote, err)
	}
	s.fetched = true
	return s.Refresh()
}

// FastForward performs a fast-forward pull from the tracked upstream.
// Returns git.ErrNonFastForwardUpdate if the histories have diverged.
func (s *SyncSession) FastForward(remote string) error {
	wt, err := s.repo.Worktree()
	if err != nil {
		return fmt.Errorf("opening worktree: %w", err)
	}
	err = wt.Pull(&git.PullOptions{
		RemoteName: remote,
		Auth:       s.auth,
	})
	if err == git.NoErrAlreadyUpToDate {
		return s.Refresh()
	}
	if err != nil {
		return err // includes git.ErrNonFastForwardUpdate
	}
	return s.Refresh()
}

// Push pushes to remote. When setUpstream is true, also configures branch tracking.
func (s *SyncSession) Push(remote, refspec string, setUpstream bool) error {
	pushOpts := &git.PushOptions{
		RemoteName: remote,
		Auth:       s.auth,
	}
	if refspec != "" {
		pushOpts.RefSpecs = []gitconfig.RefSpec{gitconfig.RefSpec(refspec)}
	}

	err := s.repo.Push(pushOpts)
	if err == git.NoErrAlreadyUpToDate {
		return nil
	}
	if err != nil {
		return fmt.Errorf("push to %s: %w", remote, err)
	}

	// Configure upstream tracking when pushing a new branch
	if setUpstream && s.state.Branch != "" {
		_ = s.configureUpstream(remote, s.state.Branch)
	}
	return nil
}

// configureUpstream sets the upstream tracking branch in .git/config.
func (s *SyncSession) configureUpstream(remote, branch string) error {
	cfg, err := s.repo.Config()
	if err != nil {
		return err
	}
	if cfg.Branches == nil {
		cfg.Branches = make(map[string]*gitconfig.Branch)
	}
	cfg.Branches[branch] = &gitconfig.Branch{
		Name:   branch,
		Remote: remote,
		Merge:  plumbing.ReferenceName("refs/heads/" + branch),
	}
	return s.repo.SetConfig(cfg)
}

// FetchedUpstreamHash returns the upstream hash as observed after the last Fetch.
// Used by the replay race guard to detect concurrent pushes.
func (s *SyncSession) FetchedUpstreamHash() plumbing.Hash {
	return s.state.UpstreamHash
}
