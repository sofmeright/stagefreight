// Package credentials provides centralized registry credential resolution.
// It defines one contract, implemented once, used by all callers.
package credentials

import (
	"os"
	"strings"
)

// SecretKind classifies how a secret was issued.
// It is inferred from the env var suffix — a policy hint, not deep truth.
// The registry sees only a secret string; the distinction is naming/issuance-side.
type SecretKind string

const (
	// SecretToken indicates the secret was named with a _TOKEN suffix.
	// Tokens are typically scoped to specific projects/actions and revocable
	// without affecting the account password or other tokens.
	SecretToken SecretKind = "token"

	// SecretPassword indicates the secret was named with a _PASS or _PASSWORD suffix.
	// Passwords authenticate the account directly and are typically broader in scope.
	SecretPassword SecretKind = "password"
)

// Resolved holds the result of resolving a credential prefix.
// User and Secret may be empty if the env vars were not set.
type Resolved struct {
	User       string
	Secret     string
	Kind       SecretKind
	SecretEnv  string // e.g. "HARBOR_TOKEN" — the env var that provided the secret
}

// IsSet returns true if both User and Secret are non-empty.
func (r Resolved) IsSet() bool {
	return r.User != "" && r.Secret != ""
}

// ResolvePrefix resolves registry credentials from environment variables
// using the given prefix (e.g. "HARBOR", "DOCKER", "GHCR_ORG").
//
// Resolution order for the secret:
//
//	{PREFIX}_TOKEN    → SecretToken    (preferred: scoped, revocable)
//	{PREFIX}_PASS     → SecretPassword (accepted)
//	{PREFIX}_PASSWORD → SecretPassword (accepted)
//
// Username is always read from {PREFIX}_USER.
// Returns a zero Resolved if prefix is empty.
func ResolvePrefix(prefix string) Resolved {
	if prefix == "" {
		return Resolved{}
	}

	p := strings.ToUpper(prefix)
	user := os.Getenv(p + "_USER")

	for _, candidate := range []struct {
		suffix string
		kind   SecretKind
	}{
		{"_TOKEN", SecretToken},
		{"_PASS", SecretPassword},
		{"_PASSWORD", SecretPassword},
	} {
		if v := os.Getenv(p + candidate.suffix); v != "" {
			return Resolved{
				User:      user,
				Secret:    v,
				Kind:      candidate.kind,
				SecretEnv: p + candidate.suffix,
			}
		}
	}

	return Resolved{User: user}
}
