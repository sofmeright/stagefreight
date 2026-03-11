# StageFreight — Rehearsals

Rehearsals provide a first-class verification primitive inside StageFreight.
Instead of scattering test commands across CI configs, rehearsals let
StageFreight orchestrate named verification steps through pluggable backends —
language-agnostic, composable, and rendered through the same section system as
every other subsystem.

> **All configuration examples below are illustrative draft syntax.** They do
> not imply parser support and will change as implementation proceeds.

---

## Core Model

| Concept | Description |
|---------|-------------|
| **rehearsal** | A named verification step with a backend, subject binding, and optional stage |
| **backend** | Internal execution strategy (shell, container, HTTP probe) |
| **uses** | Subject binding — references a StageFreight pipeline object |
| **stage** | Optional phase hint (pre-build, post-build, pre-release, post-release) |
| **result** | Status, duration, captured logs, optional output artifacts |

A **subject** is any named StageFreight pipeline object: `source`,
`build.<id>`, or `artifact.<id>`. Subjects are not artifact types — they are
references into the pipeline graph.

---

## Configuration Shape

```yaml
# draft configuration shape — illustrative only
# NOT currently supported by the StageFreight parser
rehearsals:
  - name: unit
    backend: shell
    uses: [source]
    stage: pre-build
    run: go test ./...

  - name: smoke
    backend: container
    uses: [build.api]
    run: /app/healthcheck.sh

  - name: health
    backend: http
    uses: [build.api]
    url: http://localhost:8080/health
    expect: 200
```

The `rehearsals:` key is an optional top-level config field. Adding it requires
no schema version bump — pipelines without rehearsals behave identically.

---

## Subject Binding

`uses:` references **StageFreight subjects**, not artifact types. This is
critical for three reasons:

1. **Multi-image monorepos** — a repo may produce `build.api` and
   `build.worker`; rehearsals bind to the specific build, not "the image."
2. **Non-Docker futures** — subjects can represent binaries, archives, or
   any future artifact kind without changing the rehearsal model.
3. **Multi-platform** — a single `build.<id>` may produce multiple platform
   images; the rehearsal targets the logical build, not a platform variant.

Subject references are validated at config load time. An unknown subject is a
hard error, not a silent skip.

---

## Backends

StageFreight executes rehearsals internally within the running StageFreight
process. Backends represent internal execution strategies, not external CI
runner abstractions.

| Backend | Description | Status |
|---------|-------------|--------|
| `shell` | Runs a command via the host shell | First implementation |
| `container` | Runs a command inside a container image | When forced by a real need |
| `http` | Probes a URL and asserts on status code | When forced by a real need |

Backends share a common interface: accept a rehearsal definition, execute it,
return a result. Additional backends must be driven by real project needs
rather than speculative expansion.

---

## Stages

Optional phase hints that control when a rehearsal runs relative to the
pipeline:

| Stage | When |
|-------|------|
| `pre-build` | Before any build step (source-level checks) |
| `post-build` | After builds complete (smoke tests, integration) |
| `pre-release` | Before release creation (final gates) |
| `post-release` | After release (deployment verification) |

Omitting `stage:` makes the rehearsal unordered — it runs when explicitly
invoked or when the pipeline scheduler places it.

---

## Result Model

Each rehearsal produces a result:

| Field | Type | Description |
|-------|------|-------------|
| `status` | `pass` · `fail` · `skip` · `error` | Outcome |
| `duration` | duration | Wall-clock execution time |
| `logs` | string | Captured stdout/stderr |
| `artifacts` | list | Optional output files (coverage reports, etc.) |

Rehearsal results render through the existing StageFreight section system
(`output.SectionStartCollapsed`, `sec.Row`, etc.), ensuring consistent CLI
and CI output formatting. A failed rehearsal sets the pipeline exit code
non-zero.

---

## Relationship with Existing Subsystems

- **Lint** — Static verification. Rehearsals are dynamic verification. They
  complement each other; lint does not become a rehearsal backend.
- **Crucible** — Inspiration for the rehearsal model. Stays separate for now;
  if convergence makes sense later, rehearsals absorb crucible rather than
  the reverse.
- **Security** — Scanning (Trivy, Grype, SBOM). Rehearsals do not replace
  security scans. A rehearsal could gate on scan results, but scanning itself
  remains its own subsystem.

---

## Implementation Order

1. **Shell backend** — minimal viable rehearsal: name, uses, run, result.
2. **First real adopter** — StageFreight's own pipeline uses rehearsals to
   run `go test` and smoke checks, proving the model.
3. **Container backend** — added when a real project needs to run verification
   inside a built image.
4. **Report normalization** — structured output (JUnit XML, TAP) parsed into
   the result model. Added when CI integration demands it.

---

## Migration

Existing CI test steps and project test scripts are expected to migrate
gradually into rehearsals as the system matures. Rehearsals orchestrate
verification by wrapping existing commands (`go test`, smoke scripts,
integration checks) rather than replacing language-native testing frameworks.

Mental model: `language test framework → existing scripts → StageFreight
rehearsal → StageFreight pipeline`.

This prevents dual permanent systems while keeping scope small.

---

## Non-Goals

- **Not a CI runner system** — rehearsals execute inside StageFreight, not as
  dispatched CI jobs.
- **Not language-specific** — no Go test parser, no pytest integration. Backends
  run commands; interpreting output is the report normalization phase (later).
- **Not a Go test wrapper** — `go test ./...` is just a shell command to a
  rehearsal. StageFreight has no opinion on test frameworks.
- **No ephemeral service graphs** — docker-compose-style service dependencies
  are out of scope. A rehearsal runs one command against one subject.
- **No matrix** — no fan-out across OS/version/platform combinations yet.
