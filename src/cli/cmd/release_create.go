package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/artifact"
	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/build/pipeline"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/credentials"
	"github.com/PrPlanIT/StageFreight/src/diag"
	"github.com/PrPlanIT/StageFreight/src/forge"
	"github.com/PrPlanIT/StageFreight/src/gitver"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/registry"
	"github.com/PrPlanIT/StageFreight/src/release"
	"github.com/PrPlanIT/StageFreight/src/retention"
)

// ReleaseCreateRequest is the explicit input contract for RunReleaseCreate.
// Cobra command fills this from flags; CI runner fills it from config/ciCtx.
// Ctx is inside the request (matches docker.Request pattern).
type ReleaseCreateRequest struct {
	Ctx             context.Context
	RootDir         string
	Config          *config.Config
	Tag             string
	Name            string
	NotesFile       string
	SecuritySummary string
	Draft           bool
	Prerelease      bool
	Assets          []string
	RegistryLinks   bool
	CatalogLinks    bool
	SkipSync        bool
	ReadOnly        bool // run_from: read-only mode — evaluate + narrate but do not mutate
	Verbose         bool
	Writer          io.Writer
}

var (
	rcTag             string
	rcName            string
	rcNotesFile       string
	rcSecuritySummary string
	rcDraft           bool
	rcPrerelease      bool
	rcAssets          []string
	rcRegistryLinks   bool
	rcCatalogLinks    bool
	rcSkipSync        bool
)

var releaseCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a release on the forge and sync to targets",
	Long: `Create a release on the detected forge (GitLab, GitHub, Gitea)
with generated or provided release notes.

Optionally uploads assets (scan artifacts, SBOMs) and adds
registry image links. Syncs to configured remote release targets
unless --skip-sync is set.`,
	RunE: runReleaseCreate,
}

func init() {
	releaseCreateCmd.Flags().StringVar(&rcTag, "tag", "", "release tag (default: detected from git)")
	releaseCreateCmd.Flags().StringVar(&rcName, "name", "", "release name (default: tag)")
	releaseCreateCmd.Flags().StringVar(&rcNotesFile, "notes", "", "path to release notes markdown file")
	releaseCreateCmd.Flags().StringVar(&rcSecuritySummary, "security-summary", "", "path to security output directory (reads summary.md)")
	releaseCreateCmd.Flags().BoolVar(&rcDraft, "draft", false, "create as draft release")
	releaseCreateCmd.Flags().BoolVar(&rcPrerelease, "prerelease", false, "mark as prerelease")
	releaseCreateCmd.Flags().StringSliceVar(&rcAssets, "asset", nil, "files to attach to release (repeatable)")
	releaseCreateCmd.Flags().BoolVar(&rcRegistryLinks, "registry-links", true, "add registry image links to release")
	releaseCreateCmd.Flags().BoolVar(&rcCatalogLinks, "catalog-links", true, "add GitLab Catalog link to release")
	releaseCreateCmd.Flags().BoolVar(&rcSkipSync, "skip-sync", false, "skip syncing to other forges")

	releaseCmd.AddCommand(releaseCreateCmd)
}

// actionResult tracks the outcome of a single release action.
type actionResult struct {
	Name string
	OK   bool
	Err  error
}

// releaseReport collects all release action outcomes for rendering.
type releaseReport struct {
	Tag, Forge, URL string
	Assets          []actionResult
	Links           []actionResult
	Tags            []actionResult
}

func runReleaseCreate(cmd *cobra.Command, args []string) error {
	rootDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	// Apply config defaults when CLI flags are not explicitly set, then merge into request.
	secSummary := rcSecuritySummary
	if !cmd.Flags().Changed("security-summary") && cfg.Release.SecuritySummary != "" {
		secSummary = cfg.Release.SecuritySummary
	}
	regLinks := rcRegistryLinks
	if !cmd.Flags().Changed("registry-links") {
		regLinks = cfg.Release.RegistryLinks
	}
	catLinks := rcCatalogLinks
	if !cmd.Flags().Changed("catalog-links") {
		catLinks = cfg.Release.CatalogLinks
	}

	return RunReleaseCreate(ReleaseCreateRequest{
		Ctx:             cmd.Context(),
		RootDir:         rootDir,
		Config:          cfg,
		Tag:             rcTag,
		Name:            rcName,
		NotesFile:       rcNotesFile,
		SecuritySummary: secSummary,
		Draft:           rcDraft,
		Prerelease:      rcPrerelease,
		Assets:          rcAssets,
		RegistryLinks:   regLinks,
		CatalogLinks:    catLinks,
		SkipSync:        rcSkipSync,
		Verbose:         verbose,
		Writer:          os.Stdout,
	})
}

// RunReleaseCreate executes the full release creation pipeline from an explicit request.
// All inputs are taken from req — no package-level vars are referenced.
func RunReleaseCreate(req ReleaseCreateRequest) error {
	rootDir := req.RootDir
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	w := req.Writer
	if w == nil {
		w = os.Stdout
	}
	color := output.UseColor()

	// Detect version for tag
	versionInfo, err := build.DetectVersion(rootDir, req.Config)
	if err != nil {
		return fmt.Errorf("detecting version: %w", err)
	}

	tag := req.Tag
	if tag == "" {
		tag = "v" + versionInfo.Version
	}
	name := req.Name
	if name == "" {
		name = tag
	}

	// Load security summary if provided
	var secTile, secBody string
	if req.SecuritySummary != "" {
		summaryPath := req.SecuritySummary + "/summary.md"
		data, err := os.ReadFile(summaryPath)
		if err != nil {
			// Not fatal — security scan may have been skipped
			if req.Verbose {
				fmt.Fprintf(os.Stderr, "note: no security summary at %s: %v\n", summaryPath, err)
			}
		} else {
			content := strings.TrimSpace(string(data))
			if content != "" {
				parts := strings.SplitN(content, "\n", 2)
				secTile = strings.TrimSpace(parts[0])
				secBody = content
			}
		}
	}

	// Build image availability rows.
	// Truth mode: read from publish manifest (build stage output).
	// Fallback mode: build from config targets (local dev, manual release).
	currentTag := os.Getenv("CI_COMMIT_TAG")

	manifest, manifestErr := artifact.ReadPublishManifest(rootDir)
	var imageRows []release.ImageRow

	switch {
	case manifestErr == nil:
		// Truth mode: build rows from verified publish manifest
		if len(manifest.Published) > 0 {
			// Credential resolver for verification
			credResolver := func(prefix string) (string, string) {
				cred := credentials.ResolvePrefix(prefix)
				if cred.Kind == credentials.SecretPassword {
					diag.Warn("credentials %s: authenticating with %s — consider using %s_TOKEN instead (scoped, revocable)",
						prefix, cred.SecretEnv, strings.ToUpper(prefix))
				}
				return cred.User, cred.Secret
			}

			// Remote verification
			results, verifyErr := registry.VerifyImages(ctx, manifest.Published, credResolver)
			if verifyErr != nil {
				return fmt.Errorf("verifying published images: %w", verifyErr)
			}
			for _, r := range results {
				if !r.Verified {
					return fmt.Errorf("published image %s failed remote verification: %v", r.Image.Ref, r.Err)
				}
			}

			// Artifact discovery (best-effort)
			artifactMap := registry.DiscoverAllArtifacts(ctx, manifest.Published, credResolver)

			// Group by host+path, dedup tags
			type imageKey struct{ host, path string }
			type pendingTarget struct {
				resolved  registry.ResolvedRegistryTarget
				seen      map[string]bool
				digestRef string
				sbom      string
				prov      string
				sig       string
			}
			targetIndex := make(map[imageKey]*pendingTarget)
			var targetOrder []imageKey

			for _, img := range manifest.Published {
				k := imageKey{host: img.Host, path: img.Path}
				pt, exists := targetIndex[k]
				if !exists {
					pt = &pendingTarget{
						resolved: registry.ResolvedRegistryTarget{
							Provider: img.Provider,
							Host:     img.Host,
							Path:     img.Path,
						},
						seen: make(map[string]bool),
					}
					targetIndex[k] = pt
					targetOrder = append(targetOrder, k)
				}
				if !pt.seen[img.Tag] {
					pt.seen[img.Tag] = true
					pt.resolved.Tags = append(pt.resolved.Tags, img.Tag)
				}
				if img.Digest != "" && pt.digestRef == "" {
					pt.digestRef = img.Host + "/" + img.Path + "@" + img.Digest
					// Look up artifact links
					aKey := img.Host + "/" + img.Path + "@" + img.Digest
					if links, ok := artifactMap[aKey]; ok {
						pt.sbom = links.SBOM
						pt.prov = links.Provenance
						pt.sig = links.Signature
					}
				}
			}

			// Sort by provider, then domain
			sort.SliceStable(targetOrder, func(i, j int) bool {
				ri, rj := targetIndex[targetOrder[i]], targetIndex[targetOrder[j]]
				if ri.resolved.Provider != rj.resolved.Provider {
					return ri.resolved.Provider < rj.resolved.Provider
				}
				return ri.resolved.Host < rj.resolved.Host
			})

			imageRows = make([]release.ImageRow, 0, len(targetOrder))
			for _, k := range targetOrder {
				pt := targetIndex[k]
				rt := pt.resolved
				tags := make([]release.ResolvedTag, 0, len(rt.Tags))
				for _, t := range rt.Tags {
					tags = append(tags, release.ResolvedTag{
						Name: t,
						URL:  rt.TagURL(t),
					})
				}
				imageRows = append(imageRows, release.ImageRow{
					RegistryLabel: rt.DisplayName(),
					RegistryURL:   rt.RepoURL(),
					ImageRef:      rt.ImageRef(),
					Tags:          tags,
					DigestRef:     pt.digestRef,
					SBOM:          pt.sbom,
					Provenance:    pt.prov,
					Signature:     pt.sig,
				})
			}
		}
		// Empty manifest = no images, no fallback (intentional)

	case errors.Is(manifestErr, artifact.ErrPublishManifestNotFound):
		// No truth artifact — fallback to config targets (local dev, manual release)
		imageRows = buildImageRowsFromConfig(req.Config, currentTag, versionInfo)

	default:
		// Manifest exists but invalid (checksum mismatch, parse error)
		return fmt.Errorf("publish manifest: %w", manifestErr)
	}

	// Build download rows and collect manifest assets for upload.
	// Archives are the primary distributable; raw binaries are uploaded
	// only when no archive covers that binary.
	var downloadRows []release.BinaryRow
	var manifestAssets []string // local paths to auto-upload

	if manifest != nil {
		// Track which binaries have archives
		archivedBinaries := make(map[string]bool)
		for _, a := range manifest.Archives {
			archivedBinaries[a.BuildID+"/"+a.Binary.OS+"/"+a.Binary.Arch] = true
		}

		// Archives first — these are the user-facing downloads
		for _, a := range manifest.Archives {
			downloadRows = append(downloadRows, release.BinaryRow{
				Name:     a.Name,
				Platform: a.Binary.OS + "/" + a.Binary.Arch,
				Size:     a.Size,
				SHA256:   a.SHA256,
			})
			manifestAssets = append(manifestAssets, a.Path)
		}

		// Raw binaries only if no archive covers them
		for _, bin := range manifest.Binaries {
			key := bin.BuildID + "/" + bin.OS + "/" + bin.Arch
			if archivedBinaries[key] {
				continue
			}
			downloadRows = append(downloadRows, release.BinaryRow{
				Name:     bin.Name,
				Platform: bin.OS + "/" + bin.Arch,
				Size:     bin.Size,
				SHA256:   bin.SHA256,
			})
			manifestAssets = append(manifestAssets, bin.Path)
		}
	}

	// Generate or load release notes
	var notes string
	if req.NotesFile != "" {
		data, err := os.ReadFile(req.NotesFile)
		if err != nil {
			return fmt.Errorf("reading notes file: %w", err)
		}
		notes = string(data)
	} else {
		sha := versionInfo.SHA
		if len(sha) > 8 {
			sha = sha[:8]
		}
		// Collect release tag patterns from versioning tag sources
		var tagPatterns []string
		for _, ts := range req.Config.Versioning.TagSources {
			tagPatterns = append(tagPatterns, ts.Pattern)
		}

		input := release.NotesInput{
			RepoDir:      rootDir,
			ToRef:        tag,
			TagPatterns:  tagPatterns,
			SecurityTile: secTile,
			SecurityBody: secBody,
			Version:      versionInfo.Version,
			SHA:          sha,
			IsPrerelease: versionInfo.IsPrerelease,
			Images:       imageRows,
			Downloads:    downloadRows,
		}
		notes, err = release.GenerateNotes(input)
		if err != nil {
			return fmt.Errorf("generating notes: %w", err)
		}
	}

	// Detect forge from git remote
	remoteURL, err := detectRemoteURL(rootDir)
	if err != nil {
		return fmt.Errorf("detecting remote: %w", err)
	}

	provider := forge.DetectProvider(remoteURL)
	if provider == forge.Unknown {
		return fmt.Errorf("could not detect forge from remote URL: %s", remoteURL)
	}

	// Create forge client
	forgeClient, err := newForgeClient(provider, remoteURL)
	if err != nil {
		return err
	}

	// Collect release targets from config
	primaryRelease := findPrimaryReleaseTarget(req.Config)
	remoteReleases := findRemoteReleaseTargets(req.Config)

	// ── Collect all results ──
	start := time.Now()
	report := releaseReport{
		Tag:   tag,
		Forge: string(provider),
	}

	// Create release on primary forge.
	// In read-only mode: narrate but do not mutate.
	if req.ReadOnly {
		fmt.Fprintf(w, "\n    [read-only] would create release %s on %s\n", tag, string(provider))
		fmt.Fprintf(w, "    [read-only] notes: %d bytes, %d assets\n\n", len(notes), len(req.Assets))
		return nil
	}

	rel, createErr := forgeClient.CreateRelease(ctx, forge.ReleaseOptions{
		TagName:     tag,
		Name:        name,
		Description: notes,
		Draft:       req.Draft,
		Prerelease:  req.Prerelease,
	})
	if createErr != nil {
		return fmt.Errorf("creating release: %w", createErr)
	}
	report.URL = rel.URL

	// Upload assets: manifest artifacts (binaries/archives) + explicit --asset flags.
	allAssets := append(manifestAssets, req.Assets...)
	for _, assetPath := range allAssets {
		assetName := filepath.Base(assetPath)

		if err := forgeClient.UploadAsset(ctx, rel.ID, forge.Asset{
			Name:     assetName,
			FilePath: assetPath,
		}); err != nil {
			report.Assets = append(report.Assets, actionResult{Name: assetName, Err: err})
			fmt.Fprintf(os.Stderr, "warning: failed to upload %s: %v\n", assetPath, err)
		} else {
			report.Assets = append(report.Assets, actionResult{Name: assetName, OK: true})
		}
	}

	// Add registry image links (from kind: registry targets, deduplicate by URL)
	registryTargets := pipeline.CollectTargetsByKind(req.Config, "registry")
	if req.RegistryLinks && len(registryTargets) > 0 {
		linkedURLs := make(map[string]bool)
		for _, t := range registryTargets {
			resolved, resolveErr := config.ResolveRegistryForTarget(t, req.Config.Registries, req.Config.Vars)
			if resolveErr != nil {
				report.Links = append(report.Links, actionResult{Name: t.ID, Err: resolveErr})
				continue
			}
			regProvider := resolved.Provider
			if regProvider == "" {
				regProvider = build.DetectProvider(resolved.URL)
			}
			if p, err := registry.CanonicalProvider(regProvider); err == nil {
				regProvider = p
			} else {
				regProvider = "generic"
			}

			link := buildRegistryLinkFromTarget(resolved.URL, resolved.Path, tag, regProvider)
			if linkedURLs[link.URL] {
				continue
			}
			linkedURLs[link.URL] = true

			if err := forgeClient.AddReleaseLink(ctx, rel.ID, link); err != nil {
				report.Links = append(report.Links, actionResult{Name: link.Name, Err: err})
				fmt.Fprintf(os.Stderr, "warning: failed to add registry link for %s: %v\n", resolved.URL, err)
			} else {
				report.Links = append(report.Links, actionResult{Name: link.Name, OK: true})
			}
		}
	}

	// Add GitLab Catalog link (from kind: gitlab-component targets)
	if req.CatalogLinks && provider == forge.GitLab {
		for _, t := range req.Config.Targets {
			if t.Kind == "gitlab-component" && t.Catalog {
				catalogLink := buildCatalogLink(remoteURL, tag)
				if catalogLink.URL != "" {
					if err := forgeClient.AddReleaseLink(ctx, rel.ID, catalogLink); err != nil {
						report.Links = append(report.Links, actionResult{Name: catalogLink.Name, Err: err})
						fmt.Fprintf(os.Stderr, "warning: failed to add catalog link: %v\n", err)
					} else {
						report.Links = append(report.Links, actionResult{Name: catalogLink.Name, OK: true})
					}
				}
				break // only one catalog link
			}
		}
	}

	// Auto-tagging: create rolling git tags for configured aliases on primary release target.
	// These are lightweight git tags (not releases) — they point at the release tag.
	if primaryRelease != nil && len(primaryRelease.Aliases) > 0 {
		currentTag := os.Getenv("CI_COMMIT_TAG")
		// CRITICAL:
		// tag_sources as map is ONLY for when.git_tags lookup on target conditions.
		// DO NOT reuse this for version selection — that would reintroduce
		// global filtering and break the search-path invariant.
		tagPatternMap := tagSourceMap(req.Config.Versioning.TagSources)
		// Check when conditions on the primary release target
		if targetWhenMatches(*primaryRelease, currentTag, tagPatternMap, req.Config.Policies.Branches) {
			rollingTags := gitver.ResolveTags(primaryRelease.Aliases, versionInfo)
			for _, rt := range rollingTags {
				if rt == tag || rt == "" {
					continue
				}
				// Try create, fallback to delete+recreate on conflict
				err := forgeClient.CreateTag(ctx, rt, tag)
				if err != nil {
					// Rolling tag may already exist — delete then recreate
					_ = forgeClient.DeleteTag(ctx, rt)
					err = forgeClient.CreateTag(ctx, rt, tag)
					if err != nil {
						report.Tags = append(report.Tags, actionResult{Name: rt, Err: err})
						fmt.Fprintf(os.Stderr, "warning: rolling tag %s: %v\n", rt, err)
						continue
					}
				}
				report.Tags = append(report.Tags, actionResult{Name: rt, OK: true})
			}
		}
	}

	elapsed := time.Since(start)

	// ── Release section ──
	overallStatus := "created"
	overallIcon := "success"
	if hasActionFailures(report.Assets) || hasActionFailures(report.Links) || hasActionFailures(report.Tags) {
		overallStatus = "partial"
		overallIcon = "skipped" // yellow icon
	}

	output.SectionStart(w, "sf_release", "Release")
	sec := output.NewSection(w, "Release", elapsed, color)
	sec.Row("%s  →  %s   %s  %s", tag, provider, output.StatusIcon(overallIcon, color), overallStatus)
	sec.Row("%s", report.URL)

	if len(report.Assets) > 0 || len(report.Links) > 0 || len(report.Tags) > 0 {
		sec.Row("")
		if len(report.Assets) > 0 {
			renderCheckpoint(sec, color, "assets", report.Assets)
		}
		if len(report.Links) > 0 {
			renderCheckpoint(sec, color, "links", report.Links)
		}
		if len(report.Tags) > 0 {
			renderCheckpoint(sec, color, "tags", report.Tags)
		}
	}

	sec.Close()
	output.SectionEnd(w, "sf_release")

	// ── Release projection ──
	// Sources declare destinations. Targets declare production + optional overrides.
	// Precedence per mirror:
	//   1. Explicit release target with mirror: <id> → use target behavior
	//   2. Mirror with sync.releases: true → default projection from canonical release
	//   3. Neither → skip
	if !req.SkipSync {
		currentTag := os.Getenv("CI_COMMIT_TAG")
		var syncResults []actionResult
		syncStart := time.Now()

		// Collect mirrors that have explicit target overrides.
		overriddenMirrors := make(map[string]bool)
		for _, t := range remoteReleases {
			if t.Mirror != "" {
				overriddenMirrors[t.Mirror] = true
			}
		}

		// CRITICAL:
		// tag_sources as map is ONLY for when.git_tags lookup on target conditions.
		// DO NOT reuse this for version selection — that would reintroduce
		// global filtering and break the search-path invariant.
		remoteTagPatternMap := tagSourceMap(req.Config.Versioning.TagSources)

		// Path 1: Explicit target overrides.
		for _, t := range remoteReleases {
			if !targetWhenMatches(t, currentTag, remoteTagPatternMap, req.Config.Policies.Branches) {
				if req.Verbose {
					fmt.Fprintf(os.Stderr, "skip sync: %s (when conditions not met)\n", t.ID)
				}
				continue
			}
			if req.ReadOnly {
				syncResults = append(syncResults, actionResult{Name: fmt.Sprintf("[read-only] %s: would project release %s", t.ID, tag), OK: true})
			} else {
				syncResults = append(syncResults, projectRelease(ctx, t, req, tag, name, notes, allAssets)...)
			}
		}

		// Path 2: Mirror-driven default projection.
		// Mirrors with sync.releases that don't have an explicit override.
		resolvedMirrors, _ := config.ResolveAllMirrors(req.Config.Repos, req.Config.Forges, req.Config.Vars)
		for _, m := range resolvedMirrors {
			if !m.Sync.Releases || overriddenMirrors[m.ID] {
				continue
			}
			if req.ReadOnly {
				syncResults = append(syncResults, actionResult{Name: fmt.Sprintf("[read-only] mirror:%s: would project canonical release %s", m.ID, tag), OK: true})
			} else {
				syncResults = append(syncResults, projectToMirror(ctx, *m, tag, name, notes, req.Draft, req.Prerelease)...)
			}
		}

		if len(syncResults) > 0 {
			syncElapsed := time.Since(syncStart)
			output.SectionStart(w, "sf_sync", "Release Projection")
			syncSec := output.NewSection(w, "Release Projection", syncElapsed, color)
			for _, r := range syncResults {
				if r.OK {
					syncSec.Row("%s %s", output.StatusIcon("success", color), r.Name)
				} else {
					msg := "unknown error"
					if r.Err != nil {
						msg = r.Err.Error()
					}
					syncSec.Row("%s %s: %s", output.StatusIcon("failed", color), r.Name, msg)
				}
			}
			syncSec.Close()
			output.SectionEnd(w, "sf_sync")
		}
	}

	// ── Retention section (from primary release target) ──
	if primaryRelease != nil && primaryRelease.Retention != nil && primaryRelease.Retention.Active() {
		retStart := time.Now()
		var patterns []string
		if len(primaryRelease.Aliases) > 0 {
			patterns = retention.TemplatesToPatterns(primaryRelease.Aliases)
		}
		store := &forgeStore{forge: forgeClient}
		result, retErr := retention.Apply(ctx, store, patterns, *primaryRelease.Retention)

		retElapsed := time.Since(retStart)

		output.SectionStart(w, "sf_retention", "Retention")
		retSec := output.NewSection(w, "Retention", retElapsed, color)

		if retErr != nil {
			retSec.Row("error: %v", retErr)
			fmt.Fprintf(os.Stderr, "warning: release retention: %v\n", retErr)
		} else {
			retSec.Row("%-16s%d", "matched", result.Matched)
			retSec.Row("%-16s%d", "kept", result.Kept)
			retSec.Row("%-16s%d", "pruned", len(result.Deleted))
			for _, d := range result.Deleted {
				retSec.Row("  - %s", d)
			}
		}

		retSec.Close()
		output.SectionEnd(w, "sf_retention")
	}

	return nil
}

// buildImageRowsFromConfig builds image rows from config targets (fallback when no publish manifest).
func buildImageRowsFromConfig(cfg *config.Config, currentTag string, versionInfo *gitver.VersionInfo) []release.ImageRow {
	type imageKey struct{ host, path string }
	type pendingTarget struct {
		resolved registry.ResolvedRegistryTarget
		seen     map[string]bool
	}
	targetIndex := make(map[imageKey]*pendingTarget)
	var targetOrder []imageKey

	// CRITICAL:
	// tag_sources as map is ONLY for when.git_tags lookup on target conditions.
	// DO NOT reuse this for version selection — that would reintroduce
	// global filtering and break the search-path invariant.
	registryTagPatternMap := tagSourceMap(cfg.Versioning.TagSources)

	for _, t := range pipeline.CollectTargetsByKind(cfg, "registry") {
		if !targetWhenMatches(t, currentTag, registryTagPatternMap, cfg.Policies.Branches) {
			continue
		}
		resolved, resolveErr := config.ResolveRegistryForTarget(t, cfg.Registries, cfg.Vars)
		if resolveErr != nil {
			continue
		}
		regProvider := resolved.Provider
		if regProvider == "" {
			regProvider = build.DetectProvider(resolved.URL)
		}
		if p, err := registry.CanonicalProvider(regProvider); err == nil {
			regProvider = p
		} else {
			regProvider = "generic"
		}

		resolvedTags := gitver.ResolveTags(t.Tags, versionInfo)

		host := registry.NormalizeHost(resolved.URL)
		k := imageKey{host: host, path: resolved.Path}
		pt, exists := targetIndex[k]
		if !exists {
			pt = &pendingTarget{
				resolved: registry.ResolvedRegistryTarget{
					Provider: regProvider,
					Host:     host,
					Path:     resolved.Path,
				},
				seen: make(map[string]bool),
			}
			targetIndex[k] = pt
			targetOrder = append(targetOrder, k)
		}
		for _, rt := range resolvedTags {
			if !pt.seen[rt] {
				pt.seen[rt] = true
				pt.resolved.Tags = append(pt.resolved.Tags, rt)
			}
		}
	}

	sort.SliceStable(targetOrder, func(i, j int) bool {
		ri, rj := targetIndex[targetOrder[i]], targetIndex[targetOrder[j]]
		if ri.resolved.Provider != rj.resolved.Provider {
			return ri.resolved.Provider < rj.resolved.Provider
		}
		return ri.resolved.Host < rj.resolved.Host
	})

	imageRows := make([]release.ImageRow, 0, len(targetOrder))
	for _, k := range targetOrder {
		rt := targetIndex[k].resolved
		tags := make([]release.ResolvedTag, 0, len(rt.Tags))
		for _, t := range rt.Tags {
			tags = append(tags, release.ResolvedTag{
				Name: t,
				URL:  rt.TagURL(t),
			})
		}
		imageRows = append(imageRows, release.ImageRow{
			RegistryLabel: rt.DisplayName(),
			RegistryURL:   rt.RepoURL(),
			ImageRef:      rt.ImageRef(),
			Tags:          tags,
		})
	}

	return imageRows
}

// findPrimaryReleaseTarget returns the first release target with no remote forge fields (primary mode).
func findPrimaryReleaseTarget(cfg *config.Config) *config.TargetConfig {
	for _, t := range cfg.Targets {
		if t.Kind == "release" && !t.IsRemoteRelease() {
			return &t
		}
	}
	return nil
}

// findRemoteReleaseTargets returns all release targets with remote forge fields set.
func findRemoteReleaseTargets(cfg *config.Config) []config.TargetConfig {
	var targets []config.TargetConfig
	for _, t := range cfg.Targets {
		if t.Kind == "release" && t.IsRemoteRelease() {
			targets = append(targets, t)
		}
	}
	return targets
}

// targetWhenMatches checks if a target's when conditions match the current CI environment.
// Resolves policy names from the provided policies config.
func targetWhenMatches(t config.TargetConfig, currentTag string, tagPatterns map[string]string, branchPatterns map[string]string) bool {
	if len(t.When.GitTags) > 0 && currentTag != "" {
		resolved := resolveWhenPatternsFromCfg(t.When.GitTags, tagPatterns)
		if !config.MatchPatterns(resolved, currentTag) {
			return false
		}
	}
	if len(t.When.Branches) > 0 {
		branch := resolveBranchFromEnv()
		resolved := resolveWhenPatternsFromCfg(t.When.Branches, branchPatterns)
		if !config.MatchPatterns(resolved, branch) {
			return false
		}
	}
	return true
}

// resolveWhenPatternsFromCfg resolves when condition entries to regex patterns.
// "re:" prefixed entries are inline regex, others are policy name lookups.
func resolveWhenPatternsFromCfg(entries []string, policyMap map[string]string) []string {
	resolved := make([]string, 0, len(entries))
	for _, entry := range entries {
		if len(entry) > 3 && entry[:3] == "re:" {
			resolved = append(resolved, entry[3:])
		} else if regex, ok := policyMap[entry]; ok {
			resolved = append(resolved, regex)
		} else {
			resolved = append(resolved, entry)
		}
	}
	return resolved
}

// renderCheckpoint renders a checkpoint line with pass/fail count, expanding failures.
func renderCheckpoint(sec *output.Section, color bool, label string, results []actionResult) {
	total := len(results)
	ok := 0
	var failed []actionResult
	for _, r := range results {
		if r.OK {
			ok++
		} else {
			failed = append(failed, r)
		}
	}

	status := "success"
	if ok != total {
		status = "failed"
	}
	icon := output.StatusIcon(status, color)

	sec.Row("%s %-7s %d/%d", icon, label+":", ok, total)

	for _, r := range failed {
		msg := "unknown error"
		if r.Err != nil {
			msg = r.Err.Error()
		}
		sec.Row("  - %s: %s", r.Name, msg)
	}
}

// hasActionFailures returns true if any result has a failure.
func hasActionFailures(results []actionResult) bool {
	for _, r := range results {
		if !r.OK {
			return true
		}
	}
	return false
}

// buildRegistryLinkFromTarget creates a forge release link for a registry target.
// Uses ResolvedRegistryTarget for deterministic URL generation.
func buildRegistryLinkFromTarget(url, path, tag, provider string) forge.ReleaseLink {
	rt := registry.ResolvedRegistryTarget{
		Provider: provider,
		Host:     registry.NormalizeHost(url),
		Path:     path,
	}
	return forge.ReleaseLink{
		Name:     fmt.Sprintf("%s %s", rt.DisplayName(), tag),
		URL:      rt.TagURL(tag),
		LinkType: "image",
	}
}

// ownerFromPath extracts the owner/org from "owner/repo" or "owner/repo/sub".
func ownerFromPath(path string) string {
	if idx := strings.IndexByte(path, '/'); idx >= 0 {
		return path[:idx]
	}
	return path
}

// repoFromPath extracts the repo name from "owner/repo".
func repoFromPath(path string) string {
	if idx := strings.IndexByte(path, '/'); idx >= 0 {
		rest := path[idx+1:]
		if idx2 := strings.IndexByte(rest, '/'); idx2 >= 0 {
			return rest[:idx2]
		}
		return rest
	}
	return path
}

// buildCatalogLink creates a GitLab Catalog release link for a component project.
func buildCatalogLink(remoteURL, tag string) forge.ReleaseLink {
	// Try CI env first (most reliable in GitLab CI).
	if serverURL := os.Getenv("CI_SERVER_URL"); serverURL != "" {
		if projectPath := os.Getenv("CI_PROJECT_PATH"); projectPath != "" {
			return forge.ReleaseLink{
				Name:     fmt.Sprintf("GitLab Catalog %s", tag),
				URL:      fmt.Sprintf("%s/explore/catalog/%s", serverURL, projectPath),
				LinkType: "other",
			}
		}
	}

	// Fallback: extract from remote URL.
	projectPath := projectPathFromRemote(remoteURL)
	if projectPath == "" {
		return forge.ReleaseLink{}
	}

	baseURL := forge.BaseURL(remoteURL)
	return forge.ReleaseLink{
		Name:     fmt.Sprintf("GitLab Catalog %s", tag),
		URL:      fmt.Sprintf("%s/explore/catalog/%s", baseURL, projectPath),
		LinkType: "other",
	}
}

// projectPathFromRemote extracts the "org/repo" project path from a git remote URL.
// Handles SSH (git@host:org/repo.git) and HTTPS (https://host/org/repo.git).
func projectPathFromRemote(remoteURL string) string {
	url := remoteURL

	// SSH format: git@host:org/repo.git or git@host:port:org/repo.git
	if idx := strings.Index(url, ":"); idx >= 0 && !strings.HasPrefix(url, "http") {
		path := url[idx+1:]
		// Handle SSH with port: git@host:port/org/repo.git
		if slashIdx := strings.Index(path, "/"); slashIdx >= 0 {
			// Check if part before / is a port number
			possiblePort := path[:slashIdx]
			isPort := true
			for _, c := range possiblePort {
				if c < '0' || c > '9' {
					isPort = false
					break
				}
			}
			if isPort {
				path = path[slashIdx+1:]
			}
		}
		return strings.TrimSuffix(path, ".git")
	}

	// HTTPS format: https://host/org/repo.git
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(url, prefix) {
			withoutScheme := strings.TrimPrefix(url, prefix)
			// Remove host
			if slashIdx := strings.Index(withoutScheme, "/"); slashIdx >= 0 {
				path := withoutScheme[slashIdx+1:]
				return strings.TrimSuffix(path, ".git")
			}
		}
	}

	return ""
}

// resolveBranchFromEnv resolves the current branch from CI environment variables.
func resolveBranchFromEnv() string {
	if b := os.Getenv("CI_COMMIT_BRANCH"); b != "" {
		return b
	}
	if b := os.Getenv("GITHUB_REF_NAME"); b != "" {
		return b
	}
	return ""
}

// detectRemoteURL gets the git remote origin URL.
func detectRemoteURL(rootDir string) (string, error) {
	det, err := build.DetectRepo(rootDir)
	if err != nil {
		return "", err
	}
	if det.GitInfo != nil && det.GitInfo.Remote != "" {
		return det.GitInfo.Remote, nil
	}
	return "", fmt.Errorf("no git remote URL found")
}

// newForgeClient creates a forge client from the detected provider and remote URL.
func newForgeClient(provider forge.Provider, remoteURL string) (forge.Forge, error) {
	baseURL := forge.BaseURL(remoteURL)

	switch provider {
	case forge.GitLab:
		return forge.NewGitLab(baseURL), nil
	case forge.GitHub:
		return forge.NewGitHub(baseURL), nil
	case forge.Gitea:
		return forge.NewGitea(baseURL), nil
	default:
		return nil, fmt.Errorf("unknown forge provider: %s", provider)
	}
}

// newSyncForgeClientFromTarget creates a forge client for a remote release target.
// projectToMirror projects a canonical release to a mirror destination.
// Mirrors are first-class sources, not synthetic targets. Forge identity
// comes directly from the mirror config.
func projectToMirror(ctx context.Context, m config.ResolvedRepo, tag, name, notes string, draft, prerelease bool) []actionResult {
	var results []actionResult
	label := "mirror:" + m.ID

	client, err := forge.NewFromAccessory(m.Provider, m.BaseURL, m.Project, m.Credentials)
	if err != nil {
		results = append(results, actionResult{Name: label, Err: err})
		fmt.Fprintf(os.Stderr, "warning: mirror projection to %s: %v\n", m.ID, err)
		return results
	}

	rel, err := client.CreateRelease(ctx, forge.ReleaseOptions{
		TagName:     tag,
		Name:        name,
		Description: notes,
		Draft:       draft,
		Prerelease:  prerelease,
	})
	if err != nil {
		results = append(results, actionResult{Name: label, Err: err})
		fmt.Fprintf(os.Stderr, "warning: release projection to %s: %v\n", m.ID, err)
		return results
	}
	results = append(results, actionResult{Name: fmt.Sprintf("%s: %s", label, rel.URL), OK: true})
	return results
}

// projectRelease projects a canonical release to a single destination via target config.
// Used by explicit target overrides only.
func projectRelease(ctx context.Context, t config.TargetConfig, req ReleaseCreateRequest, tag, name, notes string, allAssets []string) []actionResult {
	var results []actionResult

	syncClient, err := newSyncForgeClientFromTarget(t, req.Config)
	if err != nil {
		results = append(results, actionResult{Name: t.ID, Err: err})
		fmt.Fprintf(os.Stderr, "warning: projection to %s: %v\n", t.ID, err)
		return results
	}

	if t.SyncRelease {
		syncRel, err := syncClient.CreateRelease(ctx, forge.ReleaseOptions{
			TagName:     tag,
			Name:        name,
			Description: notes,
			Draft:       req.Draft,
			Prerelease:  req.Prerelease,
		})
		if err != nil {
			results = append(results, actionResult{Name: t.ID, Err: err})
			fmt.Fprintf(os.Stderr, "warning: release projection to %s: %v\n", t.ID, err)
			return results
		}
		results = append(results, actionResult{Name: fmt.Sprintf("%s: %s", t.ID, syncRel.URL), OK: true})

		if t.SyncAssets {
			for _, assetPath := range allAssets {
				assetName := filepath.Base(assetPath)
				if err := syncClient.UploadAsset(ctx, syncRel.ID, forge.Asset{
					Name:     assetName,
					FilePath: assetPath,
				}); err != nil {
					fmt.Fprintf(os.Stderr, "warning: asset projection %s to %s: %v\n", assetName, t.ID, err)
				}
			}
		}
	}

	return results
}

func newSyncForgeClientFromTarget(t config.TargetConfig, cfg *config.Config) (forge.Forge, error) {
	// Resolve mirror reference — forge identity comes from the repo graph.
	if t.Mirror != "" {
		repo := config.FindRepoByID(cfg.Repos, t.Mirror)
		if repo == nil {
			return nil, fmt.Errorf("release target %s: mirror %q not found in repos", t.ID, t.Mirror)
		}
		resolved, err := config.ResolveRepo(*repo, cfg.Forges, cfg.Vars)
		if err != nil {
			return nil, fmt.Errorf("release target %s: resolving mirror %q: %w", t.ID, t.Mirror, err)
		}
		return forge.NewFromAccessory(resolved.Provider, resolved.BaseURL, resolved.Project, resolved.Credentials)
	}

	return nil, fmt.Errorf("release target %s: mirror: is required for remote release targets", t.ID)
}
