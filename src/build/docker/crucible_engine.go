package docker

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/diag"
)

// CrucibleOpts configures the pass-2 container invocation.
type CrucibleOpts struct {
	Image      string   // pass-1 candidate image ref
	FinalTag   string   // tag for the verification artifact
	RepoDir    string   // absolute path to repo root (mounted into container)
	ExtraFlags []string // original user flags minus --build-mode
	EnvVars    []string // credential and CI env vars to forward (KEY=VALUE)
	RunID      string   // correlates passes in logs
	Verbose    bool
}

// CrucibleResult captures the outcome of a pass-2 invocation.
type CrucibleResult struct {
	Passed        bool
	ExitCode      int
	FinalImageRef string
}

// VerificationArtifact encapsulates the extra --tag + --local added to pass 2
// for post-build verification. Centralizes the concept so it isn't ad-hoc flag
// munging scattered across call sites.
type VerificationArtifact struct {
	Tag string // e.g. "stagefreight/crucible-verify:<run-id>"
}

// AppendFlags returns the flags needed to produce the verification artifact.
func (va VerificationArtifact) AppendFlags() []string {
	return []string{"--tag", va.Tag, "--local"}
}

// CrucibleTag returns a namespaced temporary image tag for crucible.
// Uses stagefreight/crucible-* namespace to prevent accidental pushes.
func CrucibleTag(purpose, runID string) string {
	return fmt.Sprintf("stagefreight/crucible-%s:%s", purpose, runID)
}

// RunCrucible executes pass 2 inside the pass-1 candidate image.
// It streams stdout/stderr directly — pass-2 output is the canonical build log.
func RunCrucible(ctx context.Context, opts CrucibleOpts) (*CrucibleResult, error) {
	result := &CrucibleResult{FinalImageRef: opts.FinalTag}

	args := []string{"run", "--rm", "--network", "host"}

	// Docker socket: forward DOCKER_HOST + TLS vars, or mount the socket.
	// When DOCKER_HOST uses a hostname (e.g. tcp://docker:2376 in GitLab CI
	// DinD), resolve it to an IP now — the pass-2 container runs with
	// --network host and won't be on the CI runner's Docker network where
	// the hostname resolves.
	dockerHost := resolveDockerHost(os.Getenv("DOCKER_HOST"))
	if dockerHost != "" {
		args = append(args, "-e", "DOCKER_HOST="+dockerHost)
		for _, tlsVar := range []string{"DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
			if v := os.Getenv(tlsVar); v != "" {
				args = append(args, "-e", tlsVar+"="+v)
			}
		}
		if certPath := os.Getenv("DOCKER_CERT_PATH"); certPath != "" {
			args = append(args, "-v", certPath+":"+certPath+":ro")
		}
	} else {
		args = append(args, "-v", "/var/run/docker.sock:/var/run/docker.sock")
	}

	// Mount repo directory
	args = append(args, "-v", opts.RepoDir+":"+opts.RepoDir, "-w", opts.RepoDir)

	// Mount buildx state so crucible can reuse sf-builder from pass 1.
	// The runner mounts /stagefreight/buildx:/root/.docker/buildx in job containers.
	// The crucible container needs the same mapping to see existing builders.
	args = append(args, "-v", "/stagefreight/buildx:/root/.docker/buildx")

	// Recursion guard + run ID
	args = append(args, "-e", build.CrucibleEnvVar+"=1")
	args = append(args, "-e", build.CrucibleRunIDEnvVar+"="+opts.RunID)

	// Forward credential and CI env vars
	for _, ev := range opts.EnvVars {
		args = append(args, "-e", ev)
	}

	args = append(args, opts.Image)

	// Reuse sf-builder from pass 1 (created by skeleton's .dind-setup).
	// The buildx state is mounted from /stagefreight/buildx, so sf-builder
	// is visible. Validate it exists and bootstrap it — no silent fallback.
	va := VerificationArtifact{Tag: opts.FinalTag}
	innerFlags := make([]string, 0, len(opts.ExtraFlags)+len(va.AppendFlags()))
	innerFlags = append(innerFlags, opts.ExtraFlags...)
	innerFlags = append(innerFlags, va.AppendFlags()...)

	// sf-builder's endpoint is tls-context (created by skeleton in pass 1).
	// The crucible container has the certs and DOCKER_HOST but not the context.
	// Recreate it before using the builder.
	shellCmd := fmt.Sprintf(
		`docker context create tls-context --docker "host=$DOCKER_HOST,ca=$DOCKER_CERT_PATH/ca.pem,cert=$DOCKER_CERT_PATH/cert.pem,key=$DOCKER_CERT_PATH/key.pem" 2>/dev/null || true && docker buildx use sf-builder && docker buildx inspect --bootstrap sf-builder && mkdir -p .stagefreight/runtime/docker && printf '{"name":"sf-builder","action":"reused","driver":"docker-container"}\n' > .stagefreight/runtime/docker/builder.json && stagefreight docker build %s`,
		strings.Join(innerFlags, " "),
	)
	args = append(args, "sh", "-c", shellCmd)

	diag.Debug(opts.Verbose, "exec: docker %s", strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = 1
		}
		result.Passed = false
		return result, err
	}

	result.Passed = true
	result.ExitCode = 0
	return result, nil
}

// CleanupCrucibleImages removes temporary crucible images. Best-effort; errors
// are returned but should never downgrade a successful crucible result.
// Does not use --force to avoid removing images that a user may have manually
// tagged from the crucible output.
func CleanupCrucibleImages(ctx context.Context, tags ...string) error {
	var errs []string
	for _, tag := range tags {
		cmd := exec.CommandContext(ctx, "docker", "rmi", tag)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", tag, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("cleanup: %s", strings.Join(errs, "; "))
	}
	return nil
}

// resolveDockerHost resolves hostnames in DOCKER_HOST to IPs.
// In GitLab CI DinD, DOCKER_HOST is typically tcp://docker:2376 where "docker"
// only resolves on the CI runner's Docker network. Since the pass-2 container
// uses --network host, it needs the resolved IP instead.
// Returns the original value unchanged if empty, already an IP, or unresolvable.
func resolveDockerHost(dockerHost string) string {
	if dockerHost == "" {
		return ""
	}
	u, err := url.Parse(dockerHost)
	if err != nil {
		return dockerHost
	}
	hostname := u.Hostname()
	if hostname == "" || net.ParseIP(hostname) != nil {
		return dockerHost // already an IP or unparseable
	}
	ips, err := net.LookupHost(hostname)
	if err != nil || len(ips) == 0 {
		return dockerHost // can't resolve, return as-is
	}
	// Prefer IPv4 — Docker daemon in DinD is typically IPv4-only.
	chosen := ips[0]
	for _, ip := range ips {
		if net.ParseIP(ip).To4() != nil {
			chosen = ip
			break
		}
	}
	u.Host = net.JoinHostPort(chosen, u.Port())
	return u.String()
}

// CrucibleCheck is a single verification data point.
type CrucibleCheck struct {
	Name   string // e.g. "binary hash", "version", "image digest"
	Status string // "match", "differs", "unavailable"
	Detail string // e.g. "sha256:abc123..."
}

// IsHardFailure returns true if this check's failure should fail the crucible.
func (c CrucibleCheck) IsHardFailure() bool {
	switch c.Name {
	case "binary hash":
		return false // soft — differs is "consistent" not "deterministic"
	case "version":
		return c.Status == "differs"
	case "build graph":
		return c.Status == "differs"
	case "image digest":
		return false // always soft
	case "env fingerprint":
		return false // always soft
	}
	return false
}

// CrucibleVerification holds the complete verification result.
type CrucibleVerification struct {
	ArtifactChecks  []CrucibleCheck
	ExecutionChecks []CrucibleCheck
	TrustLevel      string
}

// HasHardFailure returns true if any check is a hard failure.
func (cv *CrucibleVerification) HasHardFailure() bool {
	for _, c := range cv.ArtifactChecks {
		if c.IsHardFailure() {
			return true
		}
	}
	for _, c := range cv.ExecutionChecks {
		if c.IsHardFailure() {
			return true
		}
	}
	return false
}

// VerifyCrucible compares pass-1 and pass-2 images to determine trust level.
// Uses promoted identity helpers from image_inspect.go for all inspections.
func VerifyCrucible(ctx context.Context, pass1Image, pass2Image string) (*CrucibleVerification, error) {
	v := &CrucibleVerification{}

	// --- Artifact checks ---

	// Binary hash
	hash1, err1 := ImageBinaryHash(ctx, pass1Image)
	hash2, err2 := ImageBinaryHash(ctx, pass2Image)
	if err1 != nil || err2 != nil {
		detail := "extraction failed"
		if err1 != nil {
			detail = fmt.Sprintf("pass1: %v", err1)
		} else {
			detail = fmt.Sprintf("pass2: %v", err2)
		}
		v.ArtifactChecks = append(v.ArtifactChecks, CrucibleCheck{
			Name: "binary hash", Status: "unavailable", Detail: detail,
		})
	} else if hash1 == hash2 {
		v.ArtifactChecks = append(v.ArtifactChecks, CrucibleCheck{
			Name: "binary hash", Status: "match", Detail: hash1,
		})
	} else {
		v.ArtifactChecks = append(v.ArtifactChecks, CrucibleCheck{
			Name: "binary hash", Status: "differs",
			Detail: fmt.Sprintf("pass1=%s pass2=%s", build.TruncHash(hash1), build.TruncHash(hash2)),
		})
	}

	// Version
	ver1, verr1 := ImageVersion(ctx, pass1Image)
	ver2, verr2 := ImageVersion(ctx, pass2Image)
	if verr1 != nil || verr2 != nil {
		v.ArtifactChecks = append(v.ArtifactChecks, CrucibleCheck{
			Name: "version", Status: "unavailable", Detail: "extraction failed",
		})
	} else if ver1 == ver2 {
		v.ArtifactChecks = append(v.ArtifactChecks, CrucibleCheck{
			Name: "version", Status: "match", Detail: ver1,
		})
	} else {
		v.ArtifactChecks = append(v.ArtifactChecks, CrucibleCheck{
			Name: "version", Status: "differs",
			Detail: fmt.Sprintf("pass1=%q pass2=%q", ver1, ver2),
		})
	}

	// Image digest
	dig1, derr1 := ImageDigest(ctx, pass1Image)
	dig2, derr2 := ImageDigest(ctx, pass2Image)
	if derr1 != nil || derr2 != nil {
		v.ArtifactChecks = append(v.ArtifactChecks, CrucibleCheck{
			Name: "image digest", Status: "unavailable", Detail: "inspection failed",
		})
	} else if dig1 == dig2 {
		v.ArtifactChecks = append(v.ArtifactChecks, CrucibleCheck{
			Name: "image digest", Status: "match", Detail: build.TruncHash(dig1),
		})
	} else {
		v.ArtifactChecks = append(v.ArtifactChecks, CrucibleCheck{
			Name: "image digest", Status: "info", Detail: "expected (layer metadata differs between builds)",
		})
	}

	// --- Execution checks ---

	// Build graph — compare plan hashes embedded as OCI labels
	graph1, gerr1 := ImageLabel(ctx, pass1Image, build.LabelPlanHash)
	graph2, gerr2 := ImageLabel(ctx, pass2Image, build.LabelPlanHash)
	if gerr1 != nil || gerr2 != nil || graph1 == "" || graph2 == "" {
		v.ExecutionChecks = append(v.ExecutionChecks, CrucibleCheck{
			Name: "build graph", Status: "unavailable",
			Detail: fmt.Sprintf("label %s not found", build.LabelPlanHash),
		})
	} else if graph1 == graph2 {
		v.ExecutionChecks = append(v.ExecutionChecks, CrucibleCheck{
			Name: "build graph", Status: "match", Detail: "identical",
		})
	} else {
		v.ExecutionChecks = append(v.ExecutionChecks, CrucibleCheck{
			Name: "build graph", Status: "differs",
			Detail: fmt.Sprintf("pass1=%s pass2=%s", build.TruncHash(graph1), build.TruncHash(graph2)),
		})
	}

	// Env fingerprint (non-authoritative — informational only, never overshadows
	// the primary signals: pass 2 success, version match, binary match, graph match)
	env1, eerr1 := ImageEnvFingerprint(ctx, pass1Image)
	env2, eerr2 := ImageEnvFingerprint(ctx, pass2Image)
	if eerr1 != nil || eerr2 != nil {
		v.ExecutionChecks = append(v.ExecutionChecks, CrucibleCheck{
			Name: "env fingerprint", Status: "unavailable", Detail: "informational",
		})
	} else if env1 == env2 {
		v.ExecutionChecks = append(v.ExecutionChecks, CrucibleCheck{
			Name: "env fingerprint", Status: "match", Detail: "informational",
		})
	} else {
		v.ExecutionChecks = append(v.ExecutionChecks, CrucibleCheck{
			Name: "env fingerprint", Status: "differs", Detail: "informational",
		})
	}

	// --- Trust level ---
	v.TrustLevel = computeTrustLevel(v)

	return v, nil
}

// computeTrustLevel determines the highest achievable trust from check results.
func computeTrustLevel(v *CrucibleVerification) string {
	if v.HasHardFailure() {
		return build.TrustViable
	}

	for _, c := range v.ArtifactChecks {
		if c.Name == "image digest" && c.Status == "match" {
			return build.TrustReproducible
		}
	}

	for _, c := range v.ArtifactChecks {
		if c.Name == "binary hash" && c.Status == "match" {
			return build.TrustDeterministic
		}
	}

	versionOK := false
	graphOK := false
	for _, c := range v.ArtifactChecks {
		if c.Name == "version" && c.Status == "match" {
			versionOK = true
		}
	}
	for _, c := range v.ExecutionChecks {
		if c.Name == "build graph" && c.Status == "match" {
			graphOK = true
		}
	}
	if versionOK && graphOK {
		return build.TrustConsistent
	}

	return build.TrustViable
}
