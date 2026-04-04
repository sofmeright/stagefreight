package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/artifact"
	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/credentials"
	"github.com/PrPlanIT/StageFreight/src/diag"
)

// ParseBuildxPublished extracts structured publish records from a buildx metadata file.
// This is the authoritative source: buildx writes the actual pushed refs + digest
// to the metadata file after a successful push. Works for both docker-container
// and remote (buildkitd) drivers.
//
// Metadata JSON format:
//
//	{"containerimage.digest": "sha256:...", "image.name": "host/path:tag,host2/path:tag,..."}
func ParseBuildxPublished(metadataFile string, registries []build.RegistryTarget) ([]artifact.PublishedImage, error) {
	data, err := os.ReadFile(metadataFile)
	if err != nil {
		return nil, err
	}

	var meta struct {
		Digest    string `json:"containerimage.digest"`
		ImageName string `json:"image.name"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}

	if meta.ImageName == "" {
		return nil, fmt.Errorf("no image.name in metadata file")
	}

	// Build provider lookup from registry configs.
	providerByHost := make(map[string]string)
	for _, reg := range registries {
		providerByHost[strings.ToLower(reg.URL)] = reg.Provider
	}

	var images []artifact.PublishedImage
	for _, ref := range strings.Split(meta.ImageName, ",") {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}

		img := artifact.PublishedImage{
			Ref:    ref,
			Digest: meta.Digest,
		}

		// Parse host/path:tag from the ref.
		r := ref
		if idx := strings.LastIndex(r, ":"); idx > 0 && !strings.Contains(r[idx:], "/") {
			img.Tag = r[idx+1:]
			r = r[:idx]
		}
		parts := strings.SplitN(r, "/", 2)
		if len(parts) == 2 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":")) {
			img.Host = strings.ToLower(parts[0])
			img.Path = parts[1]
		} else {
			img.Host = "docker.io"
			img.Path = r
		}

		img.Provider = providerByHost[img.Host]
		images = append(images, img)
	}
	return images, nil
}

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
func (bx *Buildx) Build(ctx context.Context, step build.BuildStep) (*build.StepResult, error) {
	start := time.Now()
	result := &build.StepResult{
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

	// Construct verified publish records: identity from registries, truth from metadata.
	if step.Push && len(step.Registries) > 0 {
		imgs, err := resolvePublished(step)
		if err != nil {
			result.Status = "failed"
			result.Error = fmt.Errorf("publish verification: %w", err)
			return result, result.Error
		}
		result.PublishedImages = imgs
	}

	return result, nil
}

// BuildWithLayers executes a build step and parses the output for layer events.
// Uses --progress=plain to get parseable output. The original Stdout/Stderr
// writers receive the raw output; layer events are parsed from the stderr copy.
func (bx *Buildx) BuildWithLayers(ctx context.Context, step build.BuildStep) (*build.StepResult, []build.LayerEvent, error) {
	start := time.Now()
	result := &build.StepResult{
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

	// Construct verified publish records: identity from registries, truth from metadata.
	if step.Push && len(step.Registries) > 0 {
		imgs, err := resolvePublished(step)
		if err != nil {
			result.Status = "failed"
			result.Error = fmt.Errorf("publish verification: %w", err)
			return result, nil, result.Error
		}
		result.PublishedImages = imgs
	}

	// Parse layer events from captured stderr.
	layers := ParseBuildxOutput(stderrBuf.String())

	return result, layers, nil
}

// resolvePublished combines registry identity with buildx metadata observation.
//
// Identity (from config): host, path, bare tags, provider — clean decomposed data.
// Truth (from buildx): digest, actual pushed ref count — post-push observation.
//
// Contract:
//   - Metadata file MUST exist and be parseable for push builds (hard fail otherwise)
//   - Digest MUST be present (buildx always writes it on successful push)
//   - Actual pushed ref count MUST match expected (no silent partial success)
func resolvePublished(step build.BuildStep) ([]artifact.PublishedImage, error) {
	// Read metadata — the only observation we have of what buildx actually did.
	if step.MetadataFile == "" {
		return nil, fmt.Errorf("no metadata file for push build — cannot verify publish result")
	}

	metaData, err := os.ReadFile(step.MetadataFile)
	if err != nil {
		return nil, fmt.Errorf("reading buildx metadata: %w", err)
	}

	var meta struct {
		Digest    string `json:"containerimage.digest"`
		ImageName string `json:"image.name"`
	}
	if err := json.Unmarshal(metaData, &meta); err != nil {
		return nil, fmt.Errorf("parsing buildx metadata: %w", err)
	}

	// Digest is the immutable truth. If buildx didn't write one, the push didn't happen.
	if meta.Digest == "" {
		return nil, fmt.Errorf("buildx metadata has no digest — push may not have completed")
	}

	// Count actual pushed refs from image.name (comma-separated).
	// This is buildx's record of what it actually pushed, not what we asked for.
	var actualCount int
	if meta.ImageName != "" {
		for _, ref := range strings.Split(meta.ImageName, ",") {
			if strings.TrimSpace(ref) != "" {
				actualCount++
			}
		}
	}

	// Build expected entries from decomposed registry identity.
	var expected []artifact.PublishedImage
	for _, reg := range step.Registries {
		if reg.Provider == "local" {
			continue
		}
		host := normalizeRegistryHost(reg.URL)
		for _, tag := range reg.Tags {
			expected = append(expected, artifact.PublishedImage{
				Host:     host,
				Path:     reg.Path,
				Tag:      tag,
				Provider: reg.Provider,
				Ref:      host + "/" + reg.Path + ":" + tag,
				Digest:   meta.Digest,
			})
		}
	}

	if len(expected) == 0 {
		return nil, fmt.Errorf("no remote registries in step — nothing to publish")
	}

	// Verify: actual pushed count must match expected.
	// image.name is required — buildx 0.8+ always writes it on successful push.
	if actualCount == 0 {
		return nil, fmt.Errorf("buildx metadata has digest but no image.name — cannot verify what was actually pushed")
	}
	if actualCount != len(expected) {
		return nil, fmt.Errorf("publish count mismatch: expected %d refs, buildx reported %d — partial push or config drift",
			len(expected), actualCount)
	}

	return expected, nil
}

// normalizeRegistryHost strips scheme prefixes and trailing slashes, lowercases.
func normalizeRegistryHost(url string) string {
	h := strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	return strings.ToLower(strings.TrimSuffix(h, "/"))
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
func (bx *Buildx) buildArgs(step build.BuildStep) []string {
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
	case step.Output == build.OutputLocal:
		// Extract to local filesystem — handled separately
		args = append(args, "--output", "type=local,dest=.")
	}

	// Cache flags
	for _, cf := range step.CacheFrom {
		args = append(args, "--cache-from", cf.Flag())
	}
	for _, ct := range step.CacheTo {
		args = append(args, "--cache-to", ct.Flag())
	}

	// Build context
	context := step.Context
	if context == "" {
		context = "."
	}
	args = append(args, context)

	return args
}

// PushError is the structured error from a failed docker push.
// Implements error — PushTags return type stays (int, error).
type PushError struct {
	Tag      string // fully qualified ref that failed
	ExitCode int    // process exit code (1 if not determinable)
	Stderr   string // stderr from the failed push only
	Cause    error  // underlying exec error
}

func (e *PushError) Error() string {
	return fmt.Sprintf("docker push %s: %v", e.Tag, e.Cause)
}

func (e *PushError) Unwrap() error { return e.Cause }

// PushTags pushes already-loaded local images to their remote registries.
// Used in single-platform load-then-push strategy where buildx builds with
// --load first, then we push each remote tag explicitly.
//
// Returns the count of successfully pushed tags and the first error encountered.
// On full success: (len(tags), nil). On failure: (N, *PushError) where tags[:N]
// succeeded and tags[N] failed. Callers can retry with tags[pushed:].
func (bx *Buildx) PushTags(ctx context.Context, tags []string) (int, error) {
	for i, tag := range tags {
		if bx.Verbose {
			fmt.Fprintf(bx.Stderr, "exec: docker push %s\n", tag)
		}

		cmd := exec.CommandContext(ctx, "docker", "push", tag)
		cmd.Stdout = bx.Stdout

		// Capture per-push stderr while still forwarding to bx.Stderr
		var perPushBuf bytes.Buffer
		if bx.Stderr != nil {
			cmd.Stderr = io.MultiWriter(bx.Stderr, &perPushBuf)
		} else {
			cmd.Stderr = &perPushBuf
		}

		if err := cmd.Run(); err != nil {
			exitCode := 1
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exitCode = exitErr.ExitCode()
			}
			return i, &PushError{
				Tag:      tag,
				ExitCode: exitCode,
				Stderr:   perPushBuf.String(),
				Cause:    err,
			}
		}
	}
	return len(tags), nil
}

// IsMultiPlatform returns true if the step targets more than one platform.
// Multi-platform builds cannot use --load (buildx limitation).
func IsMultiPlatform(step build.BuildStep) bool {
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
func (bx *Buildx) Login(ctx context.Context, registries []build.RegistryTarget) error {
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
