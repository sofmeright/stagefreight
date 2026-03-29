package docker

import "context"

// InventorySource resolves candidate hosts from an external source.
// Adapter boundary: no source-specific concepts (Ansible, etc.) leak past this.
type InventorySource interface {
	Name() string
	Resolve(ctx context.Context, selector TargetSelector) ([]HostTarget, error)
}

// SecretsProvider handles encryption/decryption for stack secret files.
// SOPS today, Vault/Infisical later.
type SecretsProvider interface {
	Name() string
	Decrypt(ctx context.Context, path string) ([]byte, error)
	Encrypt(ctx context.Context, path string, data []byte) error
	IsEncrypted(path string) bool
}

// HostTransport executes typed stack actions on a target host.
// Transport compiles the intent to whatever execution form it needs.
// It does NOT know compose lifecycle semantics — it executes steps.
type HostTransport interface {
	ExecuteAction(ctx context.Context, action StackAction) (ExecResult, error)
	// InspectStack queries runtime state for a compose project.
	// Read-only. Returns structured runtime facts, not CLI output.
	// Selects containers by Compose project label, not name.
	InspectStack(ctx context.Context, project string) (StackInspection, error)
	// ListProjects returns all compose project names running on this host.
	// Uses -a to include stopped containers (stopped = still logically exists).
	ListProjects(ctx context.Context) ([]string, error)
	Close() error
}
