# Architecture Boundaries

This document defines the package boundary rules for StageFreight.
These are enforced by convention; violations should be caught in code review.

## Package Roles

| Package | Role | May import |
|---------|------|------------|
| `src/artifact` | Published artifact identity and manifest (cross-cutting, no execution logic) | — (stdlib only) |
| `src/cli/cmd` | CLI adapters only — flag parsing, Cobra wiring, CI runner orchestration | everything |
| `src/build/docker` | Docker/buildx adapter: CLI execution, cosign signing, image inspection, buildx parsing, crucible engine, Dockerfile inventory; plus build orchestration (detect → plan → execute → publish) | build (parent), artifact, config, credentials, diag, output, pipeline, postbuild, registry |
| `src/build/pipeline` | Generic in-process phase runner and PipelineContext | artifact, build, config |
| `src/cistate` | Cross-job file-based state ledger (pipeline.json) | ci |
| `src/postbuild` | Post-build integration glue (badges, retention, harbor, readme) | artifact, build, pipeline, registry |
| `src/registry` | Registry client + operational integration (verification, retention, referrers) | artifact, config, credentials, diag, retention |
| `src/build` | Shared build foundation: engine interfaces, plan/step/result, provenance model, OCI labeling, detection, tags, version | config, gitver; credentials/diag in execution paths |

## Scratch Rules

- `Scratch` is for **intra-package ephemeral state** only — keys written and read within the same package.
- Cross-package pipeline state uses **typed fields** on `PipelineContext` (`Manifest`, `BuildPlan`).
- Keys are namespaced by package prefix: `docker.*`, `binary.*`.
- Never add a Scratch key readable from a different package. Promote it to a typed field instead.

## Service Function Rule

Commands invocable from CI runners must expose an explicit request-based function.
`Ctx` lives inside the request — matches the `docker.Request` pattern already established:

```go
type XxxRequest struct {
    Ctx context.Context
    // ... all other inputs explicit, no package vars
}
func RunXxx(req XxxRequest) error { ... }
```

The Cobra `RunE` wrapper builds the request from flag vars and calls `RunXxx`.
CI runners call `RunXxx` directly — no flag var mutation.

**Any exported or testable non-Cobra function in `src/cli/cmd` must not read `cfg.*`.**
Config must arrive by parameter. The Cobra wrapper is the only global-aware edge.

## Import Audit Notes

- `src/config`: pure data model, zero internal imports — safe to import anywhere.
- `src/props`: no internal imports — stable leaf node.
- `narrator` → `props`: one-way, intentional — healthy.
- `postbuild`: high fan-in by design — acceptable as long as it stays post-build-only.
- `src/output` → `lint`: only upward coupling in output; acceptable.

## Rot Risk Watchlist

These are the areas most likely to accumulate coupling if not actively maintained.

| Area | Risk | Signal to watch for |
|------|------|---------------------|
| `src/artifact` | Foundation-package drift | Any internal imports or non-manifest execution/provider logic appearing |
| `src/postbuild` | Temporal junk drawer | New files that aren't badge/readme/retention/harbor |
| `src/build` | Domain sprawl | Files importing `forge` or `release` (strong smell) |
| `src/cli/cmd` | cfg-as-global creep | Any non-Cobra function reading `cfg.*` without a param |
| `src/registry` | Dependency regression | Any import of `src/build` (hard boundary after artifact extraction) |
| `narrator_run.go` helpers | cfg swap recurrence | Any save/restore pattern on a package global |
| `output` → `lint` | Upward coupling | output importing any domain package other than lint |

## Import Contracts

Tight boundary rules for the hottest packages. Violations should be caught in code review.
These rules are intended for future automated checking.

### `src/artifact`
- **Identity:** cross-cutting published artifact identity and manifest — not execution logic
- **Permitted:** stdlib only (zero internal imports)
- **Forbidden:** everything internal — this is a foundational leaf package
- **Rules:**
  - Owns published image/binary/archive metadata; build and registry both import it
  - Contains only data structures plus serialization/persistence directly related to
    artifact identity and manifest handling. No execution logic, orchestration,
    network I/O, or provider integration.

### `src/cli/cmd`
- **Permitted:** any service/domain package (this is the dependency graph leaf)
- **Forbidden:** must not be imported by any package
- **Rules:**
  - Non-Cobra helpers must not read package globals (cfg.*, flag vars, mutable package state)
  - Command wrappers may translate flags/config into request structs, but business logic
    should not accumulate here
  - Command wrappers must not pass partially-initialized config or mutate shared state
    before calling service functions. All inputs must be explicit.
  - Non-wrapper helpers should prefer narrow params or explicit request structs over
    wrapper-owned state fanout

### `src/build` (root package files only)
- **Identity:** shared build foundation — NOT a Docker implementation home
- **Permitted:** `config`, `gitver`, `artifact`
- **Permitted in execution/proving paths only:** `credentials`, `diag`
- **Forbidden:** `forge`, `release`, `cli`, `postbuild`
- **Forbidden in root package:** `registry`
- **Rules:**
  - Root must NOT contain CLI adapter code (docker/buildx/cosign/podman command invocation)
  - Shared build semantics (provenance model, OCI labels, plan fingerprinting) belong here
  - Docker adapter code belongs in `build/docker/`
  - Binary-specific code (gobuild.go, archive.go) stays until volume justifies `build/binary/`
- **Subpackage note:** `build/docker` and `build/pipeline` are higher-level coordinators
  and may have wider imports consistent with their orchestration role

### `src/registry`
- **Identity:** operational registry integration layer, not a pure registry client
- **Permitted:** `artifact`, `config`, `credentials`, `diag`, `retention`
- **Forbidden:** `build` (hard boundary — do not reintroduce), `forge`, `release`, `cli`, `postbuild`, `narrator`
- **Rules:**
  - Provider logic and registry-domain operations belong here
  - Registry operates on artifact types but does not define or mutate artifact identity semantics
  - Retention application is allowed here only as long as registry remains the execution
    home for registry retention operations

### `src/config`
- **Zero internal imports.** Must stay a pure data model.

### `src/props`
- **Zero internal imports.** Stable leaf node.

### `src/postbuild`
- **Identity:** post-build integration layer for completed artifacts only
- **Permitted:** `artifact`, `build`, `registry`, `config`, `output`
- **Forbidden:** `forge`, `release`, `cli`, `narrator`
- **Rule:** only work that is strictly downstream of completed build results belongs here
- **Smells (should trigger review pushback):** release authoring, pipeline triggering,
  configuration mutation, provenance generation, signing, notification fanout

## Enforcement (planned)

Static checks should fail CI if:
- `cfg.` appears outside Cobra wrappers in `src/cli/cmd`
- `src/registry` imports `src/build`
- `src/artifact` imports any internal package
