package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/config"
)

// GenerateOptions controls manifest generation behavior.
type GenerateOptions struct {
	RootDir   string
	BuildID   string // filter to a specific build ID
	Platform  string // filter to a specific platform (os/arch)
	Mode      string // ephemeral, workspace, commit, publish
	OutputDir string // output directory for manifest files
	DryRun    bool
	Version   string // app version for generator field
}

// Generate creates manifests for all matching builds.
func Generate(cfg *config.Config, opts GenerateOptions) ([]*Manifest, error) {
	mode := opts.Mode
	if mode == "" {
		mode = cfg.Manifest.Mode
	}
	if mode == "" {
		mode = "ephemeral"
	}

	outputDir := opts.OutputDir
	if outputDir == "" {
		outputDir = cfg.Manifest.OutputDir
	}
	if outputDir == "" {
		outputDir = ".stagefreight/manifests"
	}

	if !filepath.IsAbs(outputDir) {
		outputDir = filepath.Join(opts.RootDir, outputDir)
	}

	var manifests []*Manifest

	for _, bc := range cfg.Builds {
		if opts.BuildID != "" && bc.ID != opts.BuildID {
			continue
		}

		m, err := generateForBuild(cfg, bc, opts, mode)
		if err != nil {
			return nil, fmt.Errorf("manifest for build %q: %w", bc.ID, err)
		}

		manifests = append(manifests, m)

		if !opts.DryRun {
			slug := build.SlugifyBuildID(bc.ID)
			filename := slug + ".json"

			outPath := filepath.Join(outputDir, filename)
			data, merr := MarshalDeterministic(m)
			if merr != nil {
				return nil, fmt.Errorf("marshal manifest %q: %w", bc.ID, merr)
			}

			if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
				return nil, fmt.Errorf("creating manifest dir: %w", err)
			}
			if err := os.WriteFile(outPath, data, 0o644); err != nil {
				return nil, fmt.Errorf("writing manifest %q: %w", outPath, err)
			}
		}
	}

	if len(manifests) == 0 && opts.BuildID != "" {
		return nil, fmt.Errorf("no build found with id %q", opts.BuildID)
	}

	return manifests, nil
}

func generateForBuild(cfg *config.Config, bc config.BuildConfig, opts GenerateOptions, mode string) (*Manifest, error) {
	slug := build.SlugifyBuildID(bc.ID)

	m := &Manifest{
		SchemaVersion: 1,
		Kind:          "stagefreight.manifest",
		Metadata:      buildMetadata(mode, opts.Version),
		Repo:          buildRepo(opts.RootDir),
		Scope: Scope{
			Name:    slug,
			BuildID: bc.ID,
		},
		Build: buildBuild(cfg, bc),
		Image: Image{
			Refs: []string{},
		},
		Inventories: Invs{
			Versions: []InvItem{},
			Apk:      []InvItem{},
			Apt:      []InvItem{},
			Pip:      []InvItem{},
			Galaxy:   []InvItem{},
			Npm:      []InvItem{},
			Go:       []InvItem{},
			Binaries: []InvItem{},
		},
		Security: Security{
			Signatures: []SigInfo{},
			Scans:      []ScanInfo{},
		},
		Targets: []Target{},
	}

	// Collect targets for this build
	for _, tc := range cfg.Targets {
		if tc.Build == bc.ID {
			m.Targets = append(m.Targets, Target{
				ID:            tc.ID,
				Kind:          tc.Kind,
				Provider:      tc.Provider,
				URL:           tc.URL,
				Path:          tc.Path,
				Tags:          tc.Tags,
				CredentialRef: tc.Credentials,
			})
		}
	}

	// Extract inventory from Dockerfile
	dockerfilePath := bc.Dockerfile
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}
	if !filepath.IsAbs(dockerfilePath) {
		dockerfilePath = filepath.Join(opts.RootDir, dockerfilePath)
	}

	inv, err := build.ExtractInventory(dockerfilePath)
	if err != nil {
		// Non-fatal: inventory extraction is best-effort
		fmt.Fprintf(os.Stderr, "  warning: inventory extraction failed for %s: %v\n", bc.ID, err)
	} else {
		m.Inventories = convertInventory(inv)
	}

	// Populate base image from Dockerfile stages
	PopulateBaseImage(m, opts.RootDir)

	return m, nil
}

func buildMetadata(mode, version string) Metadata {
	md := Metadata{
		Generator: "stagefreight",
		State:     "prebuild",
		Mode:      mode,
	}

	if version != "" {
		md.Generator = "stagefreight " + version
	}

	// Include timestamp only in ephemeral and publish modes
	if mode == "ephemeral" || mode == "publish" {
		md.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	}

	return md
}

func buildRepo(rootDir string) Repo {
	r := Repo{}

	det, err := build.DetectRepo(rootDir)
	if err != nil {
		return r
	}

	if det.GitInfo != nil {
		r.URL = det.GitInfo.Remote
		r.DefaultBranch = det.GitInfo.Branch
		r.Commit = det.GitInfo.SHA
	}

	return r
}

func buildBuild(cfg *config.Config, bc config.BuildConfig) Build {
	b := Build{
		ConfigPath: ".stagefreight.yml",
		BuildID:    bc.ID,
		Dockerfile: bc.Dockerfile,
		Context:    bc.Context,
		Args:       map[string]string{},
	}

	if b.Dockerfile == "" {
		b.Dockerfile = "Dockerfile"
	}
	if b.Context == "" {
		b.Context = "."
	}
	if bc.Target != "" {
		t := bc.Target
		b.Target = &t
	}

	// Copy build args
	for k, v := range bc.BuildArgs {
		b.Args[k] = v
	}

	return b
}

func convertInventory(inv *build.InventoryResult) Invs {
	result := Invs{
		Versions: []InvItem{},
		Apk:      []InvItem{},
		Apt:      []InvItem{},
		Pip:      []InvItem{},
		Galaxy:   []InvItem{},
		Npm:      []InvItem{},
		Go:       []InvItem{},
		Binaries: []InvItem{},
	}

	// Convert base image versions
	for _, p := range inv.Versions {
		result.Versions = append(result.Versions, convertPackage(p))
	}

	// Route packages to the correct group
	for _, p := range inv.Packages {
		item := convertPackage(p)
		switch p.Manager {
		case "apk":
			result.Apk = append(result.Apk, item)
		case "apt":
			result.Apt = append(result.Apt, item)
		case "pip":
			result.Pip = append(result.Pip, item)
		case "galaxy":
			result.Galaxy = append(result.Galaxy, item)
		case "npm":
			result.Npm = append(result.Npm, item)
		case "go":
			result.Go = append(result.Go, item)
		case "binary":
			result.Binaries = append(result.Binaries, item)
		}
	}

	return result
}

func convertPackage(p build.PackageInfo) InvItem {
	item := InvItem{
		Name:      p.Name,
		Version:   p.Version,
		Pinned:    p.Pinned,
		Source:    p.Source,
		SourceRef: p.SourceRef,
		Manager:   p.Manager,
	}

	if p.Confidence != "" {
		item.Confidence = p.Confidence
	}
	if p.URL != "" {
		item.URL = p.URL
	}

	return item
}

// ResolveManifestPath returns the path to a manifest JSON for a given build ID and mode.
func ResolveManifestPath(rootDir string, cfg *config.Config, buildID string) string {
	outputDir := cfg.Manifest.OutputDir
	if outputDir == "" {
		outputDir = ".stagefreight/manifests"
	}

	slug := build.SlugifyBuildID(buildID)

	if !filepath.IsAbs(outputDir) {
		return filepath.Join(rootDir, outputDir, slug+".json")
	}
	return filepath.Join(outputDir, slug+".json")
}

// FindDefaultBuildID returns the first build ID from config, or empty string.
func FindDefaultBuildID(cfg *config.Config) string {
	if len(cfg.Builds) > 0 {
		return cfg.Builds[0].ID
	}
	return ""
}

// LoadManifest reads and parses a manifest JSON file.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var m Manifest
	if err := jsonUnmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing manifest %s: %w", path, err)
	}

	return &m, nil
}

// jsonUnmarshal is a thin wrapper for testability.
var jsonUnmarshal = func(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

// baseImageFromStages returns the base image from the last FROM stage.
func baseImageFromStages(dockerfilePath string) string {
	info, err := build.ParseDockerfile(dockerfilePath)
	if err != nil || len(info.Stages) == 0 {
		return ""
	}

	// Use the last stage's base image (final build stage)
	last := info.Stages[len(info.Stages)-1]
	return last.BaseImage
}

// PopulateBaseImage fills in the base_image field from Dockerfile parsing.
func PopulateBaseImage(m *Manifest, rootDir string) {
	dockerfilePath := m.Build.Dockerfile
	if !filepath.IsAbs(dockerfilePath) {
		dockerfilePath = filepath.Join(rootDir, dockerfilePath)
	}

	base := baseImageFromStages(dockerfilePath)
	if base != "" {
		// Resolve ARG references in base image
		for argName, argVal := range m.Build.Args {
			base = strings.ReplaceAll(base, "${"+argName+"}", argVal)
			base = strings.ReplaceAll(base, "$"+argName, argVal)
		}
		m.Build.BaseImage = base
	}
}
