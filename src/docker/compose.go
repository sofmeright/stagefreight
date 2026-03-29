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

	c.targets = targets

	// TODO: establish SSH transports for remote hosts
	// For now, transport is deferred until Execute needs it

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

		dr := DetectDrift(stack, rctx.RepoRoot, stamps, c.secrets)
		order++

		action := "noop"
		if dr.Drifted {
			action = "up"
			drifted = append(drifted, dr)
		}

		actions = append(actions, runtime.PlannedAction{
			Name:        dr.Stack,
			Description: dr.Reason,
			Order:       order,
			Action:      action,
			Metadata: map[string]string{
				"scope":       stack.Scope,
				"scope_kind":  stack.ScopeKind,
				"stack":       stack.Name,
				"path":        stack.Path,
				"bundle_hash": dr.BundleHash,
				"stored_hash": dr.StoredHash,
				"drift_tier":  fmt.Sprintf("%d", dr.Tier),
				"deploy_kind": stack.DeployKind,
			},
		})
	}
	c.drifted = drifted

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

	// Execute only plan actions with Action == "up"
	for _, pa := range plan.Actions {
		if pa.Action != "up" {
			continue
		}

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
		err := deployStack(ctx, *stack, rctx.RepoRoot, c.secrets)
		ar := runtime.ActionResult{
			Name:     pa.Name,
			Duration: time.Since(start),
		}

		if err != nil {
			ar.Success = false
			ar.Message = err.Error()
		} else {
			ar.Success = true
			ar.Message = "deployed"
			// Update hash stamps from plan metadata (not rediscovered)
			bundleHash := pa.Metadata["bundle_hash"]
			c.stamps.Stacks[pa.Name] = StackStamp{
				BundleHash: bundleHash,
				DeployedAt: time.Now(),
			}
		}

		results = append(results, ar)
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

// deployStack handles secret decryption, pre/post scripts, and docker compose up.
func deployStack(ctx context.Context, stack StackInfo, rootDir string, secrets SecretsProvider) error {
	stackDir := filepath.Join(rootDir, stack.Path)

	// Create tmpfs staging for decrypted secrets
	tmpDir, err := os.MkdirTemp("", "sf-docker-*")
	if err != nil {
		return fmt.Errorf("creating staging dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Decrypt encrypted env files to staging
	var envArgs []string
	for _, ef := range stack.EnvFiles {
		if ef.Encrypted && secrets != nil {
			plaintext, err := secrets.Decrypt(ctx, ef.FullPath)
			if err != nil {
				return fmt.Errorf("decrypting %s: %w", ef.Path, err)
			}
			staged := filepath.Join(tmpDir, ef.Path)
			if err := os.WriteFile(staged, plaintext, 0600); err != nil {
				return fmt.Errorf("staging %s: %w", ef.Path, err)
			}
			envArgs = append(envArgs, "--env-file", staged)
		} else {
			envArgs = append(envArgs, "--env-file", filepath.Join(stackDir, ef.Path))
		}
	}

	// Run pre.sh if present
	if containsScript(stack.Scripts, "pre.sh") {
		if err := runScript(ctx, stackDir, "pre.sh"); err != nil {
			return fmt.Errorf("pre.sh: %w", err)
		}
	}

	// docker compose up -d
	args := []string{"compose", "-f", filepath.Join(stackDir, stack.ComposeFile)}
	args = append(args, envArgs...)
	args = append(args, "-p", stack.Name, "up", "-d")

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = stackDir
	// Strict environment isolation (from DD-UI)
	cmd.Env = minimalEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose up: %s", strings.TrimSpace(string(out)))
	}

	// Run post.sh if present
	if containsScript(stack.Scripts, "post.sh") {
		if err := runScript(ctx, stackDir, "post.sh"); err != nil {
			return fmt.Errorf("post.sh: %w", err)
		}
	}

	return nil
}

// runScript executes a deploy lifecycle script.
func runScript(ctx context.Context, dir, name string) error {
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(dir, name))
	cmd.Dir = dir
	cmd.Env = minimalEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

// minimalEnv returns a restricted environment to prevent host variable leakage.
// DD-UI proven pattern.
func minimalEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
	if v := os.Getenv("DOCKER_HOST"); v != "" {
		env = append(env, "DOCKER_HOST="+v)
	}
	if v := os.Getenv("SOPS_AGE_KEY"); v != "" {
		env = append(env, "SOPS_AGE_KEY="+v)
	}
	if v := os.Getenv("SOPS_AGE_KEY_FILE"); v != "" {
		env = append(env, "SOPS_AGE_KEY_FILE="+v)
	}
	return env
}

func containsScript(scripts []string, name string) bool {
	for _, s := range scripts {
		if s == name {
			return true
		}
	}
	return false
}
