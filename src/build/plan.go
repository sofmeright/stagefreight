package build

import "github.com/PrPlanIT/StageFreight/src/config"

// OutputMode describes what the build produces.
type OutputMode string

const (
	OutputImage OutputMode = "image" // container image (push to registry or load to daemon)
	OutputLocal OutputMode = "local" // extract files to local filesystem
	OutputTar   OutputMode = "tar"   // export as tarball
)

// BuildPlan is the resolved execution plan for a build.
type BuildPlan struct {
	Steps []BuildStep
}

// BuildStep is a single build invocation.
type BuildStep struct {
	Name       string
	Dockerfile string
	Context    string
	Target     string
	Platforms  []string
	BuildArgs  map[string]string
	Labels     map[string]string // OCI labels injected via --label
	Tags       []string
	Output     OutputMode
	Extract    []ExtractRule    // artifact mode only
	Registries []RegistryTarget // image mode only
	Load         bool             // --load into daemon
	Push         bool             // --push to registries
	SavePath     string           // save image tarball here after build (for security scanning)
	MetadataFile string           // temp file for buildx --metadata-file (digest capture)
}

// ExtractRule defines a file to extract from a build container.
type ExtractRule struct {
	From string // path inside the container
	To   string // local destination path
}

// RegistryTarget is a resolved registry push destination.
type RegistryTarget struct {
	URL         string
	Path        string
	Tags        []string
	Credentials string                 // env var prefix for auth (e.g., "DOCKERHUB" → DOCKERHUB_USER/DOCKERHUB_PASS)
	Provider    string                 // registry vendor: dockerhub, ghcr, gitlab, jfrog, harbor, quay, gitea, generic
	Retention   config.RetentionPolicy // restic-style retention policy
	TagPatterns []string               // original unresolved tag templates for pattern matching during retention
	NativeScan  bool                   // trigger registry's own built-in scan after push (Harbor: built-in Trivy)
}
