package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/diag"
)

// CosignSign signs an image digest ref using cosign.
// The digestRef must be in the form repo@sha256:... — tags are never used.
func CosignSign(ctx context.Context, digestRef, keyPath string, multiArch bool) error {
	args := []string{"sign",
		"--key", keyPath,
		"--tlog-upload=false",
		"--upload=true",
	}
	if multiArch {
		args = append(args, "--recursive")
	}
	args = append(args, digestRef)

	cmd := exec.CommandContext(ctx, "cosign", args...)
	cmd.Env = append(os.Environ(), "COSIGN_YES=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		diag.Warn("cosign sign failed: %s", strings.TrimSpace(string(out)))
		return fmt.Errorf("cosign sign: %w", err)
	}
	return nil
}

// CosignAttest attests a predicate against an image digest ref using cosign.
// The digestRef must be in the form repo@sha256:... — tags are never used.
func CosignAttest(ctx context.Context, digestRef, predicatePath, keyPath string) error {
	cmd := exec.CommandContext(ctx, "cosign", "attest",
		"--key", keyPath,
		"--predicate", predicatePath,
		"--type", "slsaprovenance",
		"--tlog-upload=false",
		"--upload=true",
		digestRef)
	cmd.Env = append(os.Environ(), "COSIGN_YES=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		diag.Warn("cosign attest failed: %s", strings.TrimSpace(string(out)))
		return fmt.Errorf("cosign attest: %w", err)
	}
	return nil
}

// ResolveCosignKey finds the cosign signing key path.
// Checks COSIGN_KEY env var first, then .stagefreight/cosign.key.
func ResolveCosignKey() string {
	if key := os.Getenv("COSIGN_KEY"); key != "" {
		return key
	}
	keyPath := ".stagefreight/cosign.key"
	if _, err := os.Stat(keyPath); err == nil {
		return keyPath
	}
	return ""
}

// CosignAvailable returns true if cosign is on PATH.
func CosignAvailable() bool {
	_, err := exec.LookPath("cosign")
	return err == nil
}
