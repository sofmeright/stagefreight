package docker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os/exec"
	"strings"
)

// ImageBinaryHash extracts the sha256 hash of /usr/local/bin/stagefreight
// from a local docker image.
func ImageBinaryHash(ctx context.Context, image string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--entrypoint", "sha256sum",
		image, "/usr/local/bin/stagefreight").Output()
	if err != nil {
		return "", err
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) == 0 {
		return "", fmt.Errorf("empty sha256sum output")
	}
	return parts[0], nil
}

// ImageVersion extracts the stagefreight version string from a local docker image.
func ImageVersion(ctx context.Context, image string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", image,
		"stagefreight", "version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ImageDigest returns the local image ID (config digest) via docker inspect.
func ImageDigest(ctx context.Context, image string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.Id}}", image).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ImageLabel reads a specific OCI label from a local image via docker inspect.
func ImageLabel(ctx context.Context, image, label string) (string, error) {
	tmpl := fmt.Sprintf("{{index .Config.Labels %q}}", label)
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", tmpl, image).Output()
	if err != nil {
		return "", err
	}
	val := strings.TrimSpace(string(out))
	if val == "<no value>" {
		return "", nil
	}
	return val, nil
}

// ImageEnvFingerprint returns an informational hash of the execution
// environment inside a docker image. Non-authoritative.
func ImageEnvFingerprint(ctx context.Context, image string) (string, error) {
	sfVer, err := exec.CommandContext(ctx, "docker", "run", "--rm", image,
		"stagefreight", "version").Output()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(sfVer)
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
