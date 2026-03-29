package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/runtime"
)

func init() {
	runtime.Register("docker", "compose", func() runtime.LifecycleBackend {
		return &ComposeBackend{}
	})
}

// ComposeBackend implements runtime.LifecycleBackend for Docker lifecycle
// orchestration using docker compose as the execution engine.
type ComposeBackend struct {
	targets  []HostTarget
	stacks   []StackInfo
	drifted  []DriftResult
	stamps   *HashStamps
	secrets  SecretsProvider
}

func (c *ComposeBackend) Name() string { return "compose" }

func (c *ComposeBackend) Capabilities() []runtime.Capability {
	return []runtime.Capability{
		runtime.CapReconcile,
		runtime.CapDryRun,
		runtime.CapPlanExecute,
	}
}

// Validate checks prerequisites: inventory parseable, secrets provider available,
// at least one target resolvable.
func (c *ComposeBackend) Validate(ctx context.Context, cfg *config.Config, rctx *runtime.RuntimeContext) error {
	dcfg := cfg.Docker

	// Validate inventory source
	if dcfg.Targets.Source != "ansible" && dcfg.Targets.Source != "" {
		return fmt.Errorf("unknown target source: %q (supported: ansible)", dcfg.Targets.Source)
	}
	if dcfg.Targets.Inventory == "" {
		return fmt.Errorf("docker.targets.inventory is required")
	}
	invPath := filepath.Join(rctx.RepoRoot, dcfg.Targets.Inventory)
	if _, err := os.Stat(invPath); err != nil {
		return fmt.Errorf("inventory file %s: %w", invPath, err)
	}

	// Validate selector has groups
	if len(dcfg.Targets.Selector.Groups) == 0 {
		return fmt.Errorf("docker.targets.selector.groups is required — targets must be declared")
	}

	// Validate secrets provider
	sp, err := ResolveSecretsProvider(dcfg.Secrets.Provider)
	if err != nil {
		return err
	}
	c.secrets = sp

	// Validate docker compose available
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker CLI not found in PATH")
	}

	return nil
}

// Prepare resolves targets from inventory and establishes transports.
func (c *ComposeBackend) Prepare(ctx context.Context, cfg *config.Config, rctx *runtime.RuntimeContext) error {
	dcfg := cfg.Docker
	invPath := filepath.Join(rctx.RepoRoot, dcfg.Targets.Inventory)

	inv := &AnsibleInventory{Path: invPath}
	targets, err := inv.Resolve(ctx, dcfg.Targets.Selector)
	if err != nil {
		return fmt.Errorf("resolving targets: %w", err)
	}
	if len(targets) == 0 {
		return fmt.Errorf("no targets resolved from selector groups %v", dcfg.Targets.Selector.Groups)
	}

	// Resolve transports for each target.
	for i := range targets {
		targets[i].Transport = ResolveTransport(targets[i])
	}
	c.targets = targets

	// Register transport cleanup.
	rctx.Resolved.AddCleanup(func() {
		for _, t := range c.targets {
			if t.Transport != nil {
				t.Transport.Close()
			}
		}
	})

	return nil
}

// Plan scans IaC, computes drift for each stack on each target.
// Deterministic: identical config + inputs → identical output.
func (c *ComposeBackend) Plan(ctx context.Context, cfg *config.Config, rctx *runtime.RuntimeContext) (*runtime.LifecyclePlan, error) {
	dcfg := cfg.Docker

	// Build known hosts set for scope classification
	knownHosts := map[string]bool{}
	for _, t := range c.targets {
		knownHosts[t.Name] = true
	}

	// Scan IaC
	stacks, err := ScanIaC(rctx.RepoRoot, dcfg.IaC.Path, knownHosts)
	if err != nil {
		return nil, fmt.Errorf("scanning IaC: %w", err)
	}
	c.stacks = stacks

	// Load hash stamps
	stamps, err := LoadHashStamps(rctx.RepoRoot)
	if err != nil {
		return nil, fmt.Errorf("loading hash stamps: %w", err)
	}
	c.stamps = stamps

	// Compute drift per stack — include ALL stacks in plan (noop + drifted).
	// Plan is the complete picture; execute consumes only drifted actions.
	var actions []runtime.PlannedAction
	var drifted []DriftResult
	order := 0

	for _, stack := range stacks {
		// Only process stacks with compose files
		if stack.DeployKind != "compose" {
			continue
		}

		// Resolve transport for Tier 2 drift check (may be nil for read-only).
		transport := c.resolveTransportForStack(DockerPlanMeta{Scope: stack.Scope})
		dr := DetectDrift(ctx, stack, rctx.RepoRoot, stamps, c.secrets, transport)
		order++

		action := "noop"
		if dr.Drifted {
			action = "up"
			drifted = append(drifted, dr)
		}

		meta := DockerPlanMeta{
			Scope:      stack.Scope,
			ScopeKind:  stack.ScopeKind,
			Stack:      stack.Name,
			Path:       stack.Path,
			BundleHash: dr.BundleHash,
			StoredHash: dr.StoredHash,
			DriftTier:  dr.Tier,
			DeployKind: stack.DeployKind,
		}

		actions = append(actions, runtime.PlannedAction{
			Name:        dr.Stack,
			Description: dr.Reason,
			Order:       order,
			Action:      action,
			Metadata:    meta.ToMetadata(),
		})
	}
	c.drifted = drifted

	// Orphan detection: find compose projects running but not in IaC.
	// Gated by repository trust — never destroy from absence without proof.
	trust := EvaluateTrust(rctx.RepoRoot, dcfg.IaC.Path, cfg.Lifecycle.Mode)
	trust.MarkScanResult(err == nil, len(stacks))
	trust.MarkDeclaredTargets(len(c.targets) > 0)

	// Build known project set from IaC stacks
	knownProjects := map[string]bool{}
	for _, stack := range stacks {
		if stack.ComposeProject != "" {
			knownProjects[stack.ComposeProject] = true
		}
	}

	// Query running projects per target host
	for _, target := range c.targets {
		if target.Transport == nil {
			continue
		}
		runningProjects, listErr := target.Transport.ListProjects(ctx)
		if listErr != nil {
			// Can't list projects — skip orphan detection for this host
			continue
		}

		// Find orphans: running but not in IaC
		var orphans []string
		for _, proj := range runningProjects {
			if !knownProjects[proj] {
				orphans = append(orphans, proj)
			}
		}

		if len(orphans) == 0 {
			continue
		}

		// Apply trust gate + anomaly guards
		orphanAction := dcfg.Drift.OrphanAction
		if orphanAction == "" {
			orphanAction = "report"
		}

		allowed, reason := AllowDestructiveOrphanAction(
			trust,
			len(knownProjects),
			len(runningProjects),
			len(orphans),
			dcfg.Drift.OrphanThreshold,
		)

		if !allowed && orphanAction != "report" {
			// Degrade destructive action to report
			orphanAction = "report"
		}

		// Prune safety: require force flag
		if orphanAction == "prune" && dcfg.Drift.PruneRequiresConfirmation {
			orphanAction = "down" // degrade prune → down without force
		}

		for _, proj := range orphans {
			order++
			desc := "orphaned: running but not declared in IaC"
			if !allowed {
				desc = fmt.Sprintf("orphaned (blocked: %s)", reason)
			}

			meta := DockerPlanMeta{
				Scope:     target.Name,
				ScopeKind: "host",
				Stack:     proj,
			}

			actions = append(actions, runtime.PlannedAction{
				Name:        target.Name + "/" + proj,
				Description: desc,
				Order:       order,
				Action:      orphanAction,
				Metadata:    meta.ToMetadata(),
			})
		}
	}

	return &runtime.LifecyclePlan{
		Mode:    "docker",
		Backend: "compose",
		Actions: actions,
	}, nil
}

// Execute consumes the plan — applies only actions marked "up".
// Does NOT rediscover state. Plan decides; execute applies.
// Idempotent: no drifted actions → no mutations.
func (c *ComposeBackend) Execute(ctx context.Context, plan *runtime.LifecyclePlan, rctx *runtime.RuntimeContext) (*runtime.LifecycleResult, error) {
	var results []runtime.ActionResult

	// Build stack lookup
	stackByKey := map[string]*StackInfo{}
	for i := range c.stacks {
		key := c.stacks[i].Scope + "/" + c.stacks[i].Name
		stackByKey[key] = &c.stacks[i]
	}

	// Execute plan actions: "up" (deploy drifted), "down" (orphan removal), "prune" (orphan + volumes)
	for _, pa := range plan.Actions {
		if pa.Action == "noop" || pa.Action == "report" || pa.Action == "orphan" {
			continue
		}

		meta := ParseDockerPlanMeta(pa.Metadata)
		transport := c.resolveTransportForStack(meta)

		switch pa.Action {
		case "up":
			// Deploy drifted stack.
			stack, ok := stackByKey[pa.Name]
			if !ok {
				results = append(results, runtime.ActionResult{
					Name:    pa.Name,
					Success: false,
					Message: "stack info not found in plan",
				})
				continue
			}

			start := time.Now()
			execResult, err := deployStack(ctx, *stack, rctx.RepoRoot, c.secrets, transport)
			ar := runtime.ActionResult{
				Name:     pa.Name,
				Duration: time.Since(start),
			}

			if err != nil {
				ar.Success = false
				ar.Message = err.Error()
				ar.Stderr = execResult.Stderr
			} else {
				ar.Success = true
				ar.Message = "deployed"

				// Capture runtime config hash post-deploy for Tier 2 baseline.
				configHash := ""
				if inspection, err := transport.InspectStack(ctx, stack.ComposeProject); err == nil {
					for _, svc := range inspection.Services {
						if svc.ConfigHash != "" {
							configHash = svc.ConfigHash
							break
						}
					}
				}

				c.stamps.Stacks[pa.Name] = StackStamp{
					BundleHash: meta.BundleHash,
					ConfigHash: configHash,
					DeployedAt: time.Now(),
				}
			}
			results = append(results, ar)

		case "down", "prune":
			// Remove orphaned compose project via transport.
			start := time.Now()
			downAction := StackAction{
				Target:      meta.Scope,
				Stack:       pa.Name,
				Action:      pa.Action,
				ProjectName: meta.Stack,
			}
			execResult, err := transport.ExecuteAction(ctx, downAction)
			ar := runtime.ActionResult{
				Name:     pa.Name,
				Duration: time.Since(start),
			}
			if err != nil {
				ar.Success = false
				ar.Message = err.Error()
				ar.Stderr = execResult.Stderr
			} else {
				ar.Success = true
				ar.Message = pa.Action + "ed"
				// Remove from stamps
				delete(c.stamps.Stacks, pa.Name)
			}
			results = append(results, ar)
		}
	}

	// Save updated stamps
	if err := SaveHashStamps(rctx.RepoRoot, c.stamps); err != nil {
		return nil, fmt.Errorf("saving hash stamps: %w", err)
	}

	return &runtime.LifecycleResult{Actions: results}, nil
}

// Cleanup closes transports and removes staged secrets.
func (c *ComposeBackend) Cleanup(rctx *runtime.RuntimeContext) {
	// Transport cleanup via rctx.Resolved.Cleanup()
}

// resolveTransportForStack finds the transport for a stack's scope host.
func (c *ComposeBackend) resolveTransportForStack(meta DockerPlanMeta) HostTransport {
	for _, t := range c.targets {
		if t.Name == meta.Scope {
			return t.Transport
		}
	}
	// Fallback: local transport (scope might be a group, not a host)
	return &LocalTransport{}
}

// deployStack builds a staged bundle and typed StackAction, then delegates to transport.
// Backend owns staging. Transport owns execution. No coupling.
func deployStack(ctx context.Context, stack StackInfo, rootDir string, secrets SecretsProvider, transport HostTransport) (ExecResult, error) {
	stackDir := filepath.Join(rootDir, stack.Path)

	// Create local bundle staging dir.
	bundleDir, err := os.MkdirTemp("", "sf-bundle-*")
	if err != nil {
		return ExecResult{}, fmt.Errorf("creating bundle dir: %w", err)
	}
	defer os.RemoveAll(bundleDir)

	// Stage compose file into bundle.
	if stack.ComposeFile != "" {
		if err := copyFile(filepath.Join(stackDir, stack.ComposeFile), filepath.Join(bundleDir, stack.ComposeFile)); err != nil {
			return ExecResult{}, fmt.Errorf("staging compose file: %w", err)
		}
	}

	// Stage env files — decrypt encrypted ones in-memory.
	var envFiles []string
	for _, ef := range stack.EnvFiles {
		var data []byte
		if ef.Encrypted && secrets != nil {
			data, err = secrets.Decrypt(ctx, ef.FullPath)
			if err != nil {
				return ExecResult{}, fmt.Errorf("decrypting %s: %w", ef.Path, err)
			}
		} else {
			data, err = os.ReadFile(ef.FullPath)
			if err != nil {
				return ExecResult{}, fmt.Errorf("reading %s: %w", ef.Path, err)
			}
		}
		if err := os.WriteFile(filepath.Join(bundleDir, ef.Path), data, 0600); err != nil {
			return ExecResult{}, fmt.Errorf("staging %s: %w", ef.Path, err)
		}
		envFiles = append(envFiles, ef.Path)
	}

	// Stage scripts into bundle + build hooks.
	var hooks []Hook
	for _, s := range stack.Scripts {
		if err := copyFile(filepath.Join(stackDir, s), filepath.Join(bundleDir, s)); err != nil {
			return ExecResult{}, fmt.Errorf("staging %s: %w", s, err)
		}
		phase := ""
		switch s {
		case "pre.sh":
			phase = "pre"
		case "post.sh":
			phase = "post"
		}
		if phase != "" {
			hooks = append(hooks, Hook{Phase: phase, Path: s})
		}
	}

	// Build typed execution intent.
	action := StackAction{
		Target:      stack.Scope,
		Stack:       stack.Scope + "/" + stack.Name,
		Action:      "up",
		ProjectName: stack.Name,
		WorkDir:     stackDir,
		BundleDir:   bundleDir,
		ComposeFile: stack.ComposeFile,
		EnvFiles:    envFiles,
		Hooks:       hooks,
	}

	// Delegate to transport.
	result, err := transport.ExecuteAction(ctx, action)
	if err != nil {
		return result, fmt.Errorf("%s", err)
	}

	return result, nil
}

// copyFile copies a single file.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0600)
}


