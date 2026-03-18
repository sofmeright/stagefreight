package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/credentials"
	"github.com/PrPlanIT/StageFreight/src/diag"
	"github.com/PrPlanIT/StageFreight/src/registry"
)

// Buildx wraps docker buildx commands.
type Buildx struct {
	Verbose bool
	Stdout  io.Writer
	Stderr  io.Writer
}

// NewBuildx creates a Buildx runner with default output writers.
func NewBuildx(verbose bool) *Buildx {
	return &Buildx{
		Verbose: verbose,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	}
}

// Build executes a single build step via docker buildx.
// When ParseLayers is true, buildx runs with --progress=plain and the output
// is parsed into layer events for structured display.
func (bx *Buildx) Build(ctx context.Context, step BuildStep) (*StepResult, error) {
	start := time.Now()
	result := &StepResult{
		Name: step.Name,
	}

	args := bx.buildArgs(step)

	if bx.Verbose {
		fmt.Fprintf(bx.Stderr, "exec: docker %s\n", strings.Join(args, " "))
	}

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = bx.Stdout
	cmd.Stderr = bx.Stderr

	if err := cmd.Run(); err != nil {
		result.Status = "failed"
		result.Duration = time.Since(start)
		result.Error = fmt.Errorf("docker buildx build failed: %w", err)
		return result, result.Error
	}

	result.Status = "success"
	result.Duration = time.Since(start)
	result.Images = step.Tags

	return result, nil
}

// BuildWithLayers executes a build step and parses the output for layer events.
// Uses --progress=plain to get parseable output. The original Stdout/Stderr
// writers receive the raw output; layer events are parsed from the stderr copy.
func (bx *Buildx) BuildWithLayers(ctx context.Context, step BuildStep) (*StepResult, []LayerEvent, error) {
	start := time.Now()
	result := &StepResult{
		Name: step.Name,
	}

	args := bx.buildArgs(step)
	// Inject --progress=plain for parseable output
	args = injectProgressPlain(args)

	if bx.Verbose {
		fmt.Fprintf(bx.Stderr, "exec: docker %s\n", strings.Join(args, " "))
	}

	// Capture stderr for parsing while still forwarding to original writer.
	var stderrBuf strings.Builder
	var stderrWriter io.Writer
	if bx.Stderr != nil {
		stderrWriter = io.MultiWriter(bx.Stderr, &stderrBuf)
	} else {
		stderrWriter = &stderrBuf
	}

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = bx.Stdout
	cmd.Stderr = stderrWriter

	if err := cmd.Run(); err != nil {
		result.Status = "failed"
		result.Duration = time.Since(start)
		result.Error = fmt.Errorf("docker buildx build failed: %w", err)
		return result, nil, result.Error
	}

	result.Status = "success"
	result.Duration = time.Since(start)
	result.Images = step.Tags

	// Parse layer events from captured stderr.
	layers := ParseBuildxOutput(stderrBuf.String())

	return result, layers, nil
}

// injectProgressPlain adds --progress=plain to buildx args if not already present.
func injectProgressPlain(args []string) []string {
	for _, a := range args {
		if strings.HasPrefix(a, "--progress") {
			return args
		}
	}
	// Insert after "buildx build"
	for i, a := range args {
		if a == "build" && i > 0 && args[i-1] == "buildx" {
			result := make([]string, 0, len(args)+1)
			result = append(result, args[:i+1]...)
			result = append(result, "--progress=plain")
			result = append(result, args[i+1:]...)
			return result
		}
	}
	return args
}

// buildArgs constructs the docker buildx build argument list.
func (bx *Buildx) buildArgs(step BuildStep) []string {
	args := []string{"buildx", "build"}

	// Dockerfile
	if step.Dockerfile != "" {
		args = append(args, "--file", step.Dockerfile)
	}

	// Target stage
	if step.Target != "" {
		args = append(args, "--target", step.Target)
	}

	// Platforms
	if len(step.Platforms) > 0 {
		args = append(args, "--platform", strings.Join(step.Platforms, ","))
	}

	// Build args
	for k, v := range step.BuildArgs {
		args = append(args, "--build-arg", fmt.Sprintf("%s=%s", k, v))
	}

	// Labels
	for k, v := range step.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", k, v))
	}

	// Tags
	for _, tag := range step.Tags {
		args = append(args, "--tag", tag)
	}

	// Metadata file for digest capture
	if step.MetadataFile != "" && step.Push {
		args = append(args, "--metadata-file", step.MetadataFile)
	}

	// Output mode
	switch {
	case step.Push:
		args = append(args, "--push")
	case step.Load:
		args = append(args, "--load")
	case step.Output == OutputLocal:
		// Extract to local filesystem — handled separately
		args = append(args, "--output", "type=local,dest=.")
	}

	// Build context
	context := step.Context
	if context == "" {
		context = "."
	}
	args = append(args, context)

	return args
}

// PushTags pushes already-loaded local images to their remote registries.
// Used in single-platform load-then-push strategy where buildx builds with
// --load first, then we push each remote tag explicitly.
func (bx *Buildx) PushTags(ctx context.Context, tags []string) error {
	for _, tag := range tags {
		if bx.Verbose {
			fmt.Fprintf(bx.Stderr, "exec: docker push %s\n", tag)
		}

		cmd := exec.CommandContext(ctx, "docker", "push", tag)
		cmd.Stdout = bx.Stdout
		cmd.Stderr = bx.Stderr

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("docker push %s: %w", tag, err)
		}
	}
	return nil
}

// IsMultiPlatform returns true if the step targets more than one platform.
// Multi-platform builds cannot use --load (buildx limitation).
func IsMultiPlatform(step BuildStep) bool {
	return len(step.Platforms) > 1
}

// Login authenticates to registries that have a credentials label configured.
// The Credentials field on each RegistryTarget is a user-chosen env var prefix:
//
//	credentials: DOCKERHUB_PRPLANIT  →  DOCKERHUB_PRPLANIT_USER / DOCKERHUB_PRPLANIT_PASS
//	credentials: GHCR_ORG            →  GHCR_ORG_USER / GHCR_ORG_PASS
//
// No credentials field → no login attempted (public or pre-authenticated).
// If credentials are configured but the env vars are missing, Login returns an error.
func (bx *Buildx) Login(ctx context.Context, registries []RegistryTarget) error {
	for _, reg := range registries {
		if reg.Provider == "local" {
			continue
		}
		if reg.Credentials == "" {
			if bx.Verbose {
				fmt.Fprintf(bx.Stderr, "skip login: no credentials configured for %s\n", reg.URL)
			}
			continue
		}

		cred := credentials.ResolvePrefix(reg.Credentials)
		if !cred.IsSet() {
			return fmt.Errorf("registry %s: credentials %q configured but %s_USER and/or %s_TOKEN env vars not set",
				reg.URL, reg.Credentials, strings.ToUpper(reg.Credentials), strings.ToUpper(reg.Credentials))
		}
		if cred.Kind == credentials.SecretPassword {
			diag.Warn("registry %s: authenticating with %s — consider using %s_TOKEN instead (scoped, revocable)",
				reg.URL, cred.SecretEnv, strings.ToUpper(reg.Credentials))
		}
		user, pass := cred.User, cred.Secret

		if bx.Verbose {
			fmt.Fprintf(bx.Stderr, "exec: docker login -u %s --password-stdin %s\n", user, reg.URL)
		}

		cmd := exec.CommandContext(ctx, "docker", "login", "-u", user, "--password-stdin", reg.URL)
		cmd.Stdin = strings.NewReader(pass)
		cmd.Stdout = bx.Stderr
		cmd.Stderr = bx.Stderr

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("docker login to %s: %w", reg.URL, err)
		}
	}
	return nil
}

// EnsureHarborProjects pre-creates Harbor projects for all Harbor registry targets
// that have credentials configured. Must be called after Login() and only when
// a real remote push will occur. Dedupes by (registryURL, project).
func (bx *Buildx) EnsureHarborProjects(ctx context.Context, registries []RegistryTarget) error {
	seen := map[string]struct{}{}
	for _, reg := range registries {
		if registry.NormalizeProvider(reg.Provider) != "harbor" || reg.Credentials == "" {
			continue
		}
		// Trim leading slash defensively — config validation should prevent it,
		// but a stray "/" would make the first segment empty and silently misbehave.
		project := strings.TrimPrefix(reg.Path, "/")
		if idx := strings.IndexByte(project, '/'); idx >= 0 {
			project = project[:idx]
		}
		if project == "" {
			return fmt.Errorf("harbor %s: registry target has empty path — check config", reg.URL)
		}
		key := reg.URL + "|" + project
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		cred := credentials.ResolvePrefix(reg.Credentials)
		if !cred.IsSet() {
			upper := strings.ToUpper(reg.Credentials)
			return fmt.Errorf("harbor %s: credentials %q configured but %s_USER and %s_TOKEN/%s_PASS/%s_PASSWORD are not set",
				reg.URL, reg.Credentials, upper, upper, upper, upper)
		}
		h := registry.NewHarbor(reg.URL, cred.User, cred.Secret)
		if err := h.EnsureProject(ctx, project); err != nil {
			upper := strings.ToUpper(reg.Credentials)
			var httpErr *registry.HTTPError
			if errors.As(err, &httpErr) {
				switch httpErr.StatusCode {
				case 401:
					return fmt.Errorf("harbor %s: authentication failed while ensuring project %q — check %s_USER and %s_TOKEN/%s_PASS/%s_PASSWORD: %w",
						reg.URL, project, upper, upper, upper, upper, err)
				case 403:
					return fmt.Errorf("harbor %s: account %q lacks 'Create Project' permission for project %q — grant it or pre-create the project: %w",
						reg.URL, cred.User, project, err)
				}
			}
			return fmt.Errorf("harbor %s: %w", reg.URL, err)
		}
	}
	return nil
}

// DetectProvider determines the registry vendor from the URL.
// Well-known domains are matched directly. For unknown domains, returns "generic"
// (future: probe the registry API to identify the vendor).
func DetectProvider(registryURL string) string {
	host := strings.ToLower(registryURL)
	// Strip scheme if present
	if idx := strings.Index(host, "://"); idx >= 0 {
		host = host[idx+3:]
	}
	// Strip path
	if idx := strings.IndexByte(host, '/'); idx >= 0 {
		host = host[:idx]
	}

	switch {
	case host == "docker.io" || host == "registry-1.docker.io" || host == "index.docker.io":
		return "dockerhub"
	case host == "ghcr.io":
		return "ghcr"
	case host == "quay.io":
		return "quay"
	case strings.Contains(host, "gitlab"):
		return "gitlab"
	case strings.Contains(host, "jfrog") || strings.Contains(host, "artifactory") || strings.Contains(host, "jcr"):
		return "jfrog"
	case strings.Contains(host, "harbor"):
		return "harbor"
	case strings.HasSuffix(host, ".amazonaws.com") && strings.Contains(host, ".dkr.ecr."):
		return "ecr"
	case strings.HasSuffix(host, ".pkg.dev"):
		return "gar"
	case strings.HasSuffix(host, ".azurecr.io"):
		return "acr"
	default:
		return "generic"
	}
}

// Save exports a loaded image as a tarball for downstream scanning and attestation.
// The image must be loaded into the daemon first (--load or docker load).
func (bx *Buildx) Save(ctx context.Context, imageRef string, outputPath string) error {
	if bx.Verbose {
		fmt.Fprintf(bx.Stderr, "exec: docker save -o %s %s\n", outputPath, imageRef)
	}

	cmd := exec.CommandContext(ctx, "docker", "save", "-o", outputPath, imageRef)
	cmd.Stdout = bx.Stderr
	cmd.Stderr = bx.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker save %s: %w", imageRef, err)
	}
	return nil
}

// ResolveDigest queries the registry for the manifest digest of a pushed image.
func ResolveDigest(ctx context.Context, ref string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "buildx", "imagetools",
		"inspect", ref, "--format", "{{.Manifest.Digest}}").Output()
	if err != nil {
		return "", fmt.Errorf("resolving digest for %s: %w", ref, err)
	}
	digest := strings.TrimSpace(string(out))
	if digest == "" || digest == "null" {
		return "", fmt.Errorf("no digest returned for %s (schema-1 registry?)", ref)
	}
	if strings.HasPrefix(digest, "sha256:") {
		return digest, nil
	}
	// Fallback: buildx may return multi-line inspect output instead of
	// honoring --format. Scan for a "Digest:" field.
	for _, line := range strings.Split(digest, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Digest:") {
			d := strings.TrimSpace(strings.TrimPrefix(line, "Digest:"))
			if strings.HasPrefix(d, "sha256:") {
				return d, nil
			}
		}
	}
	return "", fmt.Errorf("unexpected digest format for %s: %s", ref, digest)
}

// ResolveLocalDigest extracts the pushed digest from a locally loaded image
// via docker inspect RepoDigests. This is a fallback for when buildx imagetools
// inspect can't reach the registry. Only returns a digest that matches the
// requested ref's registry/path to prevent cross-ref confusion.
func ResolveLocalDigest(ctx context.Context, ref string) (string, error) {
	// Parse the ref to extract registry/path prefix for matching.
	// ref format: "host/path:tag" or "host/path@sha256:..."
	refPrefix := ref
	if idx := strings.LastIndex(refPrefix, ":"); idx > 0 {
		// Check it's a tag separator, not part of a port
		slash := strings.LastIndex(refPrefix, "/")
		if idx > slash {
			refPrefix = refPrefix[:idx]
		}
	}
	if idx := strings.Index(refPrefix, "@"); idx > 0 {
		refPrefix = refPrefix[:idx]
	}

	out, err := exec.CommandContext(ctx, "docker", "inspect", ref,
		"--format", "{{json .RepoDigests}}").Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect %s: %w", ref, err)
	}

	var repoDigests []string
	if err := json.Unmarshal(out, &repoDigests); err != nil {
		return "", fmt.Errorf("parsing RepoDigests for %s: %w", ref, err)
	}

	// Normalize Docker Hub aliases for matching
	normalizeDockerHub := func(s string) string {
		for _, alias := range []string{"index.docker.io/", "registry-1.docker.io/"} {
			if strings.HasPrefix(s, alias) {
				return "docker.io/" + strings.TrimPrefix(s, alias)
			}
		}
		return s
	}

	normalizedPrefix := normalizeDockerHub(refPrefix)

	for _, rd := range repoDigests {
		normalizedRD := normalizeDockerHub(rd)
		// RepoDigest format: "registry/path@sha256:..."
		if atIdx := strings.Index(normalizedRD, "@"); atIdx > 0 {
			rdPrefix := normalizedRD[:atIdx]
			digest := normalizedRD[atIdx+1:]
			if rdPrefix == normalizedPrefix && strings.HasPrefix(digest, "sha256:") {
				return digest, nil
			}
		}
	}

	return "", fmt.Errorf("no matching RepoDigest for %s in %v", ref, repoDigests)
}

// ParseMetadataDigest parses the digest from a buildx --metadata-file JSON output.
func ParseMetadataDigest(metadataFile string) (string, error) {
	data, err := os.ReadFile(metadataFile)
	if err != nil {
		return "", err
	}
	var meta struct {
		Digest string `json:"containerimage.digest"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", err
	}
	if meta.Digest == "" {
		return "", fmt.Errorf("no digest in metadata file")
	}
	return meta.Digest, nil
}

// EnsureBuilder checks that a buildx builder is available and creates one if needed.
func (bx *Buildx) EnsureBuilder(ctx context.Context) error {
	// Check if default builder exists
	cmd := exec.CommandContext(ctx, "docker", "buildx", "inspect")
	if err := cmd.Run(); err != nil {
		// Create a builder
		create := exec.CommandContext(ctx, "docker", "buildx", "create", "--use", "--name", "stagefreight")
		create.Stdout = bx.Stderr
		create.Stderr = bx.Stderr
		if createErr := create.Run(); createErr != nil {
			return fmt.Errorf("creating buildx builder: %w", createErr)
		}
	}
	return nil
}
