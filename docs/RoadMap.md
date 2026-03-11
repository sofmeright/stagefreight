# StageFreight — Road Map

> **Repository Lifecycle Steward**
> A daemon that watches over your entire repo portfolio and actively maintains it.

StageFreight is evolving from a GitLab CI component with bash scripts into a platform-agnostic **Repository Lifecycle Steward**. This document captures the full vision — from the Go CLI rewrite through daemon mode — absorbing the [prplangit concept](https://gitlab.prplanit.com/precisionplanit/dungeon/-/blob/main/docs/Ideas.md) under the StageFreight brand.

---

## Tracking Index

Quick reference to all planned and completed work. Items link to detailed
sections below. Split by current priority — will migrate to issues as we
start tracking there.

### Priority

- [ ] [Fix Bitnami rNN rebuild suffix bug](#freshness-known-bugs) — `normalizeFamily` produces `debian-r` not `debian`
- [ ] [Fix isHexHash uppercase detection](#freshness-known-bugs) — only matches lowercase `[a-f0-9]`
- [ ] [Fix release depth / precision mismatch](#freshness-known-bugs) — `redis:7` incorrectly matches `7.4.2`
- [ ] [Component templates (`{component.*}`)](#phase-0--foundation-cli-rewrite) — needs GitLab API client
- [ ] [Banner with chafa rendering](#phase-0--foundation-cli-rewrite) — chafa version mismatch pending Alpine bump

### Medium Interest

- [ ] Rehearsals — composable verification ([docs/Rehearsals.md](Rehearsals.md)) — shell backend first, language-agnostic
- [ ] [Custom release pattern templates](#freshness-p3-custom-release-pattern-templates) — user escape hatch for non-standard tags
- [ ] [Auditable failure messages](#freshness-p4-auditable-failure-messages) — structured warnings for registry failures and parse errors
- [ ] [OCI manifest annotation fetching](#freshness-p5-oci-manifest-annotation-fetching) — `image.version` for ambiguous tags
- [ ] [Changelog & release notes linking](#freshness-p6-changelog--release-notes-linking) — depends on annotation fetching
- [ ] [Digest-based rebuild detection](#freshness-p8-digest-based-rebuild-detection) — detect same-tag rebuilds
- [ ] [Digest deduplication](#freshness-p9-digest-deduplication) — suppress alias-tag false positives
- [ ] [Configuration API redesign](#freshness-configuration-api-redesign) — extends / adherence / repository
- [ ] [Multi-provider support (Phase 1)](#phase-1--multi-provider)
- [ ] [Cross-provider sync & docs engine (Phase 2)](#phase-2--cross-provider-sync--documentation-engine)

### Low Interest

- [ ] [Multi-registry tag aggregation](#freshness-p7-multi-registry-tag-aggregation) — aggregate tags across mirrors
- [ ] [Preset / extends system](#freshness-p10-preset--extends-system) — org-wide policy distribution
- [ ] Color in build streaming — cached=gray, result=bold
- [ ] [Fork maintenance (Phase 3)](#phase-3--fork-maintenance)
- [ ] [Supply chain & security (Phase 4)](#phase-4--supply-chain--security)
- [ ] [Daemon mode (Phase 5)](#phase-5--daemon-mode-the-endgame)

### Completed

- [x] Props — composable presentation subsystem ([docs/Props.md](Props.md)) — 37 Tier 1 badge types, CLI commands, narrator integration
- [x] Tag classification overhaul — multi-stage pipeline, family grouping, pre-release ranking (2026-02-24)
- [x] Configuration API design — extends/adherence/repository schema (2026-02-24)
- [x] Cache TTL for external-state lint modules — CacheTTLModule interface (2026-02-25)
- [x] Lint config level respect — CLI flag > config > default (2026-02-25)
- [x] Freshness: Docker Hub pagination, v-prefix normalization, ordering fix (2026-02-25)
- [x] Linter configuration reference — [docs/Linter.md](Linter.md) (2026-02-25)
- [x] Lint cache relocation to XDG-aware paths
- [x] Section rendering, phase sections, per-module lint stats
- [x] Scoped versions, static templates, date/time formatting
- [x] Project metadata templates (`{project.*}`)
- [x] Narrator CLI — compose (ad-hoc) + run (config-driven)
- [x] Docker Hub API templates (`{docker.*}`)
- [x] Build streaming — buildx `--progress=plain` layer parsing
- [x] Badges in build pipeline

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  docker.io/prplanit/stagefreight                            │
│                                                             │
│  ┌───────────────────────────────────────────────────────┐  │
│  │  /usr/local/bin/stagefreight   (Go binary)            │  │
│  │                                                       │  │
│  │  subcommands:                                         │  │
│  │    init                                               │  │
│  │    release notes | release create                     │  │
│  │    docs inputs                                        │  │
│  │    badge update                                       │  │
│  │    docker build | build                               │  │
│  │    lint                                               │  │
│  │    test                                               │  │
│  │    deps pull | deps verify | deps cache               │  │
│  │    docker verify                                      │  │
│  │    approve                                            │  │
│  │    security scan                                      │  │
│  │    dev build | dev test | dev up                      │  │
│  │    sync readme | sync metadata                        │  │
│  │    lint links                                         │  │
│  │    flair                                              │  │
│  │    fork sync | fork status | fork audit               │  │
│  │    audit deps | audit packages | audit codebase        │  │
│  │    sbom diff                                          │  │
│  │    serve           ← daemon mode                      │  │
│  └───────────────────────────────────────────────────────┘  │
│                                                             │
│  Alpine base + docker-cli + buildx (existing toolchain)     │
└─────────────────────────────────────────────────────────────┘

Consumers:
  ┌──────────────────────┐    ┌──────────────────────────┐
  │  GitLab CI component │    │  GitHub Actions wrapper   │
  │  (thin YAML calling  │    │  (thin YAML calling       │
  │   the binary)        │    │   the binary)             │
  └──────────────────────┘    └──────────────────────────┘
  ┌──────────────────────────────────────────────────────┐
  │  stagefreight serve  (K8s pod / daemon mode)         │
  │  — org scanning, manifest discovery, scheduled jobs  │
  └──────────────────────────────────────────────────────┘
```

**Key architectural decisions:**

- **Go binary** — all logic lives in `stagefreight`, the single Go CLI. Consistency with internal tooling (hasteward), mature DevOps ecosystem (go-git, go-gitlab, go-github, go-sdk), single-binary distribution.
- **Provider abstraction** — GitLab, GitHub, Gitea/Forgejo behind a common interface. CLI subcommands work against any provider; daemon mode connects to multiple simultaneously.
- **OCI image** — same image (`docker.io/prplanit/stagefreight`), now ships the Go binary alongside the existing Alpine toolchain. CI components become thin YAML wrappers that call the binary.
- **MR-first** — every write operation produces a merge/pull request, never direct commits. Unless YOLO mode is on.

---

## Adoption — Existing Projects with Minimal Friction

StageFreight is useless if onboarding an existing project means rewriting CI pipelines, restructuring Dockerfiles, or committing a 50-line manifest before anything works. The adoption model is designed around **zero friction at entry, progressive depth as you want it.**

### Zero-Config CLI (No Manifest Required)

Every CLI subcommand works without a `.stagefreight.yml`. The binary inspects the repo and infers sensible defaults:

```
$ cd my-existing-project
$ stagefreight release notes              # scans git log, generates release notes
$ stagefreight docker build --local       # finds Dockerfile, builds for local platform
$ stagefreight security scan              # finds Dockerfile or lockfiles, runs Trivy
$ stagefreight dev test                   # no dev.test config? runs Dockerfile healthcheck if defined
```

**How inference works:**
- **Dockerfile** — found by walking the repo root, `build/`, `docker/`, `.` for `Dockerfile`, `Dockerfile.*`, `*.dockerfile`. If multiple exist, pick the root one or error with a clear message.
- **Language/toolchain** — detected from lockfiles (`go.mod`, `package.json`, `Cargo.toml`, `requirements.txt`, `*.csproj`, etc.). Informs security scanning, `.gitignore` lint, license header patterns.
- **Git provider** — detected from `git remote` origin URL. GitLab, GitHub, Gitea recognized automatically.
- **Existing CI** — StageFreight does not touch or conflict with existing `.gitlab-ci.yml` / `.github/workflows/`. It runs alongside them. You can adopt one subcommand at a time without disrupting anything.
- **Existing Dockerfiles** — used as-is. StageFreight never modifies, generates, or rewrites Dockerfiles. Your multistage builds, your base images, your build args — all untouched. StageFreight is the orchestrator, not the author.
- **Healthcheck fallback** — if no `dev.test` config exists but the Dockerfile has a `HEALTHCHECK` instruction, `stagefreight dev test` uses it. If the image exposes ports, it probes them. Something useful happens without any config.

### `stagefreight init` — Scaffold a Manifest from What Exists

For projects that want a manifest but don't want to write one from scratch:

```
$ stagefreight init
Detected:
  Language:    Go (go.mod)
  Dockerfile:  ./Dockerfile (multistage: builder, production, dev)
  Provider:    GitLab (gitlab.prplanit.com)
  CI:          .gitlab-ci.yml (existing — will not modify)
  Registry:    registry.prplanit.com/tools/my-app (from CI variables)

Generated .stagefreight.yml:

  docker:
    dockerfile: Dockerfile
    target: production
    platforms: [linux/amd64]
    registries:
      - url: registry.prplanit.com
        path: tools/my-app
        tags: ["{version}", latest]
  security:
    scan: true
  dev:
    target: dev              # detected "dev" stage in Dockerfile
    test:
      healthchecks:
        - { name: http, type: http, url: "http://localhost:8080/", expect_status: 200 }

Creating branch stagefreight/init...
Opening MR against main...
  → https://gitlab.prplanit.com/precisionplanit/my-app/-/merge_requests/42

$ stagefreight init --local   # alternative: just write the file, no git operations
Write .stagefreight.yml? [Y/n]
```

`init` reads everything that already exists — Dockerfile stages, CI config, git remotes, exposed ports, HEALTHCHECK instructions, existing registry references — and produces a manifest that describes what the project already does. No behavior change on day one, just a declarative description of the status quo that you can then evolve.

**Default behavior: creates an MR.** The manifest is committed to a `stagefreight/init` branch and an MR is opened against the default branch. This is the MR-first philosophy — the repo owner reviews what StageFreight detected, adjusts if needed, and merges when ready. The MR description explains what was inferred and why.

**Flags:**
- `stagefreight init` — creates MR with the generated manifest (default, MR-first)
- `stagefreight init --commit` — skip the MR, commit `.stagefreight.yml` directly to the current branch (for solo devs, YOLO users, or repos where you're the only maintainer)
- `stagefreight init --branch <name>` — target a specific branch. With `--commit`, commits directly to that branch. Without `--commit`, opens the MR against that branch as the base. Useful for dropping the manifest into a feature branch first to test before it hits main.
- `stagefreight init --local` — write the file to disk only, no git operations (for inspection before committing yourself)
- `stagefreight init --dry-run` — print what it would generate without writing anything
- `stagefreight init --full` — include all config sections with commented-out defaults as documentation
- `stagefreight init --interactive` — walk through each section interactively, asking what to enable

**Daemon mode:** when `stagefreight serve` discovers a repo with no `.stagefreight.yml` but org defaults are configured, it can auto-open an `init` MR to adopt the repo. The MR contains the generated manifest based on what the daemon inferred + org defaults applied. Repo owner merges to opt in, closes to opt out. This is how org-wide rollout works without touching every repo manually.

### Progressive Adoption Path

The adoption curve is intentionally shallow:

```
Level 0 — CLI only, no manifest
  └─ "I just want release notes / a local build / a security scan"
  └─ Install the binary, run subcommands ad-hoc
  └─ Works today, nothing to configure

Level 1 — Manifest for builds
  └─ "I want reproducible builds with proper tagging"
  └─ Run `stagefreight init`, tweak the docker section
  └─ Existing CI calls `stagefreight docker build` instead of raw docker commands

Level 2 — Manifest for dev experience
  └─ "I want contributors to be able to run and test locally"
  └─ Add dev.services, dev.test.healthchecks, dev.test.sanity
  └─ `stagefreight dev up` replaces the "how to run locally" README section

Level 3 — Manifest for full lifecycle
  └─ "I want security scanning, license checks, badges, the works"
  └─ Enable sections incrementally — each one is independent
  └─ CI pipeline shrinks as StageFreight absorbs more jobs

Level 4 — Daemon mode
  └─ "I want all my repos managed automatically"
  └─ Deploy `stagefreight serve`, configure org defaults
  └─ Repos with manifests get full treatment, repos without get org defaults
  └─ Repos with no manifest and no org defaults are left alone
```

### Coexistence with Existing CI

StageFreight does not replace your CI system. It's a tool your CI calls — or that runs alongside it.

**GitLab — gradual migration:**
```yaml
# Before: raw script in .gitlab-ci.yml
build:
  image: golang:1.24-bullseye
  script: |
    apt-get update && apt-get install -y ...
    go build -o my-app
    docker build -t registry.prplanit.com/tools/my-app:$CI_COMMIT_TAG .
    docker push ...

# After: one line calling stagefreight
build:
  image: docker.io/prplanit/stagefreight:latest
  script:
    - stagefreight docker build

# Or even thinner — use the StageFreight GitLab component
include:
  - component: gitlab.prplanit.com/components/stagefreight/docker-build@main
```

Each CI job can be migrated independently. Replace the `release_notes` job first. Then `docker build`. Then `security scan`. The rest of the pipeline stays exactly as it is. No big bang rewrite.

**GitHub Actions — same pattern:**
```yaml
# Before: 40 lines of shell in a workflow
- name: Build
  run: |
    docker buildx build --platform linux/amd64,linux/arm64 ...

# After: one step
- name: Build
  uses: sofmeright/stagefreight-action@v1
  # reads .stagefreight.yml, does the right thing
```

**No CI at all — also fine:**
Projects without CI pipelines benefit immediately from the CLI. `stagefreight dev build && stagefreight dev test` gives them a reproducible build and test workflow without ever setting up GitLab CI or GitHub Actions. When they're ready for CI, the manifest they already have plugs straight in.

### Existing Dockerfiles — No Changes Required

StageFreight works with whatever Dockerfile you have today:

| What you have | What StageFreight does |
|---|---|
| Single-stage `FROM alpine ... RUN ...` | Builds it. Tags it. Pushes it. |
| Multistage `FROM node AS builder` / `FROM nginx AS production` | Builds targeting the final (or configured) stage. |
| `HEALTHCHECK` instruction | Uses it for `dev test` healthchecks if no manifest test config. |
| `EXPOSE` ports | Probes them during `dev test` if no manifest healthchecks. |
| `ARG` build args | Pass them via manifest `build_args` or CLI `--build-arg`. |
| No Dockerfile (library, docs, pure config repo) | `stagefreight build` still works for artifact builds. Everything else (release notes, security scan, badges, flair) works without any Dockerfile at all. |
| Raw CI scripts with `apt-get install` and inline builds | Works as-is today. When you're ready, wrap the toolchain in a Dockerfile and let `stagefreight build` handle it. No rush. |

---

## Phased Feature Road Map

### Phase 0 — Foundation (CLI Rewrite)

Rewrite existing bash/Python logic as Go subcommands. No new features, just parity with what exists today plus proper structure.

| Subcommand | Replaces | Description |
|---|---|---|
| `stagefreight init` | Manual manifest creation | Infer project config from existing repo, scaffold `.stagefreight.yml`, open MR to adopt |
| `stagefreight release notes` | `generate-release_notes.sh` | Git log mining, commit categorization, markdown output |
| `stagefreight release create` | GitLab API bash calls | API release creation with asset linking |
| `stagefreight docs inputs` | Embedded Python + jq | Component YAML spec → markdown table generation |
| `stagefreight badge update` | Badge bash scripts | SVG templating, pipeline status query, commit back |
| `stagefreight docker build` | Docker build bash scripts | Flexible multi-registry, multi-arch, multi-OS container image build+push engine (see below) |
| `stagefreight build` | Raw CI scripts (apt-get, wget, etc.) | Artifact builds — multi-toolchain Dockerfile pipelines that output binaries, zips, packages instead of images (see below) |
| `stagefreight lint` | Pre-commit hooks, manual checks | Cache-aware, delta-only code quality gate that runs before every build (see below) |
| `stagefreight test` | Scattered CI test scripts, Makefile targets | Structured, delta-aware testing engine — scoped tests across source/binary/container contexts with declarative HTTP/gRPC/exec/schema primitives (see below) |
| `stagefreight commit` | Freeform `git commit -m` | Interactive item-by-item commit flow — module-driven conventional prefixes, templated notes, composable line-by-line messages, repo-customizable commit types and language (see below) |
| `stagefreight security scan` | Trivy bash wrapper | Trivy orchestration, SBOM generation, vendor-specific result upload |
| `stagefreight approve` | Manual CI/CD gates, Slack "approve" buttons | Human-in-the-loop approval gates — OIDC SSO approval flow for dangerous operations with audit logging (see below) |
| `stagefreight version stamp` | Manual find-and-replace across files | Unified metadata injection — version, description, license, URLs across all project files (see below) |

#### `stagefreight version stamp` — Metadata Synchronization

Every project scatters the same metadata across a dozen files: version strings in `go.mod`, `Cargo.toml`, `package.json`, Dockerfile LABELs, Helm Chart.yaml, OCI annotations, README badges, `--version` CLI output, and copyright headers. Today you update them by hand (miss one, ship stale info) or write fragile sed scripts in CI. StageFreight makes this declarative.

**The problem:** A Go CLI embeds its version via `-ldflags`, the Dockerfile has `org.opencontainers.image.version`, `Chart.yaml` has `appVersion`, the README shows a badge, and the LICENSE file has a copyright year. Change any one of them and the rest go stale. Multiply by every repo in the org.

**What `stagefreight version stamp` does:**

1. Reads version + metadata from a single source of truth:
   - **Tag-driven (default):** version is the git tag. Everything else comes from the manifest or is inferred.
   - **Manifest-driven:** explicitly declared in `.stagefreight.yml` under `metadata:`.
   - **Hybrid:** tag sets the version, manifest provides description/license/URLs/motto.
2. Stamps that metadata into every file that references it, using per-ecosystem conventions.
3. In CI: runs automatically before build. In daemon mode: runs on tag push.
4. Locally: `stagefreight version stamp --dry-run` shows what would change.

**Manifest config:**
```yaml
metadata:
  # These are the canonical values — single source of truth
  name: stagefreight                          # project name
  description: "Repository Lifecycle Steward" # one-liner
  motto: "Hello World's a Stage"              # slogan/tagline (optional)
  license: AGPL-3.0-only                      # SPDX identifier
  homepage: https://stagefreight.dev          # project URL
  repository: https://github.com/sofmeright/stagefreight
  author: "SoFMeRight <sofmeright@gmail.com>"

  # Version source — where the version number comes from
  # "tag" (default): latest git tag, stripped of 'v' prefix
  # "file:<path>": read from a specific file (e.g., VERSION)
  # "manifest": use the version field below
  version_source: tag
  # version: 1.2.3                           # only used when version_source: manifest

  # Stamp targets — where metadata gets written
  # Auto-detected by default; explicit list overrides auto-detection
  stamp:
    # Go: injects via -ldflags at build time (no file modification needed)
    - type: go
      package: github.com/sofmeright/stagefreight/src/version
      vars:
        Version: "{version}"
        Description: "{description}"
        License: "{license}"
        BuildDate: "{build_date}"
        Commit: "{commit_sha}"

    # Rust: updates version in Cargo.toml
    - type: cargo
      file: Cargo.toml
      fields: [version, description, license, homepage, repository]

    # Node: updates version in package.json
    - type: npm
      file: package.json
      fields: [version, description, license, homepage, repository, author]

    # Docker: updates LABEL instructions in Dockerfile
    - type: dockerfile
      file: Dockerfile
      labels:
        org.opencontainers.image.version: "{version}"
        org.opencontainers.image.title: "{name}"
        org.opencontainers.image.description: "{description}"
        org.opencontainers.image.licenses: "{license}"
        org.opencontainers.image.source: "{repository}"
        org.opencontainers.image.url: "{homepage}"
        org.opencontainers.image.revision: "{commit_sha}"
        org.opencontainers.image.created: "{build_date}"

    # Helm: updates Chart.yaml
    - type: helm
      file: charts/stagefreight/Chart.yaml
      fields: [version, appVersion, description, home]

    # Generic: regex-based find-and-replace for any file
    - type: generic
      file: README.md
      replacements:
        - pattern: 'Version: .*'
          replace: 'Version: {version}'
        - pattern: '!\[version\]\(.*\)'
          replace: '![version](https://img.shields.io/badge/version-{version}-blue)'

    # Version file: write a plain VERSION file (consumed by other tools)
    - type: file
      file: VERSION
      content: "{version}"
```

**Auto-detection (zero config):** Without an explicit `stamp:` list, StageFreight scans the repo and stamps everything it recognizes:

| File | What gets stamped |
|---|---|
| `go.mod` / Go source with `version` package | `-ldflags` injection at build time |
| `Cargo.toml` | `version`, `description`, `license`, `homepage`, `repository` |
| `package.json` | `version`, `description`, `license`, `homepage`, `repository` |
| `Dockerfile` | OCI labels (`org.opencontainers.image.*`) |
| `Chart.yaml` | `version`, `appVersion`, `description`, `home` |
| `setup.py` / `pyproject.toml` | `version`, `description`, `license`, `url` |
| `*.csproj` | `Version`, `Description`, `PackageLicenseExpression` |
| `LICENSE` / `LICENSE.md` | Copyright year (updates `2024` → `2024-2026`) |

**Tag-driven workflow (the golden path):**

```
$ git tag v1.3.0
$ git push --tags
  → CI pipeline starts
  → stagefreight version stamp       # injects v1.3.0 everywhere
  → stagefreight lint                 # quality gate
  → stagefreight docker build         # builds with correct version baked in
  → stagefreight release create       # creates release with notes + assets
```

The developer tags, pushes, and walks away. Every file in the repo that references the version, description, license, or URLs is updated automatically. The Go binary reports `stagefreight version 1.3.0 (commit abc1234, built 2026-02-22)`. The Docker image has correct OCI labels. The Helm chart has the right `appVersion`. No manual editing. No stale metadata.

**Dev builds:** When there's no tag (feature branch, local dev), version stamp generates a dev version: `0.0.0-dev+abc1234` (or `{last_tag}-dev+{sha}` if a previous tag exists). This ensures `--version` output is always meaningful, even in development.

**Build integration:** `stagefreight docker build` and `stagefreight build` automatically run `version stamp` before building. The version is injected into the build context so Dockerfiles and build scripts always have access to correct metadata. For Go, this means `-ldflags` are set automatically — no manual `go build -ldflags "-X main.version=..."` incantation.

#### `stagefreight release` — Platform-Agnostic Release Lifecycle

The release subsystem handles the entire release lifecycle: notes generation, release creation, asset attachment, badge updates, and cross-platform sync. Everything is platform-agnostic — the same commands work against GitLab, GitHub, and Gitea/Forgejo through the forge abstraction layer.

**Subcommands:**

| Command | Description |
|---|---|
| `stagefreight release notes` | Generate markdown release notes from conventional commits |
| `stagefreight release create` | Create a release on the forge with notes, assets, and registry links |
| `stagefreight release badge` | Generate and commit a release status badge SVG |

**Release creation flow:**

```
$ git tag v1.3.0 && git push --tags
  → CI pipeline starts
  → stagefreight docker build       # builds + pushes image
  → stagefreight security scan      # Trivy + SBOM generation
  → stagefreight release create \
      --asset .stagefreight/security/security-scan.json \
      --asset .stagefreight/security/sbom.spdx.json \
      --registry-links
  → stagefreight release badge
```

The `release create` command:
1. Detects the forge from the git remote URL (GitLab, GitHub, Gitea)
2. Generates release notes from conventional commits (or reads from `--notes` file)
3. Creates the release via the forge API
4. Uploads scan artifacts (SARIF, SBOM) as release assets
5. Adds vendor-aware registry image links (Docker Hub web URL, GHCR package URL, etc.)
6. Syncs the release to all configured sync targets

**Conditional sync — cross-platform release distribution:**

Sync targets let you mirror releases across forges with fine-grained control over when and what gets synced. Use cases:

- Mirror GitLab releases to GitHub for public visibility
- Sync security scan artifacts to a compliance-focused forge
- Push badges to a documentation repo

```yaml
release:
  badge:
    enabled: true
    path: .badges/release.svg
    branch: main

  sync:
    - name: github-mirror
      provider: github
      url: https://github.com
      credentials: GITHUB_MIRROR       # → GITHUB_MIRROR_TOKEN env var
      project_id: prplanit/stagefreight
      branches: ["^main$"]             # only sync releases from main
      tags: ["^v\\d+\\.\\d+\\.\\d+$"]  # only stable semver tags
      sync_release: true               # create release on target
      sync_assets: true                # upload scan artifacts
      sync_badge: false                # don't push badge here

    - name: internal-gitlab
      provider: gitlab
      url: https://gitlab.internal.com
      credentials: INTERNAL_GL
      project_id: "42"
      branches: ["!^develop$"]         # sync everything except develop
      tags: ["!^v.*-alpha"]            # exclude alpha tags
      sync_release: true
      sync_assets: true
      sync_badge: true
```

**Sync conditions — standard pattern syntax:**

All `branches`, `tags`, and `git_tags` fields across StageFreight use the same pattern syntax:

| Pattern | Meaning |
|---|---|
| `"^main$"` | Regex match — only `main` |
| `"^release/.*"` | Regex match — any `release/` branch |
| `"!^develop$"` | Negated regex — everything except `develop` |
| `"!^v.*-rc"` | Negated regex — exclude release candidates |
| `"main"` | Literal/regex match — matches `main` |

When multiple patterns are in a list, evaluation is:
1. **Excludes first** — any `!` pattern that matches → rejected
2. **Includes second** — if any include pattern matches → allowed
3. **Exclude-only lists** — if the list contains only `!` patterns and none matched, value is allowed (everything not excluded passes)
4. **Empty list** — no filter, always allowed

| Field | Behavior |
|---|---|
| `branches: ["^main$", "^release/.*"]` | Only sync on main or release branches |
| `branches: ["!^develop$", "!^wip/.*"]` | Sync on everything except develop and wip branches |
| `tags: ["^v\\d+\\.\\d+\\.\\d+$"]` | Only sync on stable semver tags |
| `tags: ["!^v.*-rc", "!^v.*-alpha"]` | Sync all tags except RC and alpha |
| `sync_release` | Create/update the release on the target forge (default: true) |
| `sync_assets` | Upload scan artifacts to the target release (default: true) |
| `sync_badge` | Commit badge SVG to the target repo (default: false) |

This same pattern syntax is used in:
- **Registry config** — `branches` and `git_tags` filter when to push images
- **Sync targets** — `branches` and `tags` filter when to sync releases
- **Security detail rules** — `tag` and `branch` in `Condition` control detail level
- **Future features** — notifications, approvals, test scopes will all use the same primitive

#### `stagefreight tag` — Version Lifecycle & Tag Management

Manages the version lifecycle: creating, removing, and bumping git tags through user-definable release channels with auto-incrementing semantics.

**Subcommands:**

| Command | Description |
|---|---|
| `stagefreight tag` | Show current version, latest tag, and channel state |
| `stagefreight tag bump <channel>` | Bump to the next version in a channel and create the git tag |
| `stagefreight tag add <version>` | Create a git tag (local + remote push) |
| `stagefreight tag remove <version>` | Delete a git tag (local + remote) |
| `stagefreight tag next <channel>` | Dry-run: show what the next version would be without creating it |
| `stagefreight tag list` | List all tags with channel classification |

**Release channels:**

Channels define the promotion ladder — how versions flow from development through testing to stable release. Each channel specifies its tag pattern, what it increments, and what channel it promotes from.

```yaml
release:
  # Ordered promotion ladder: dev → alpha → beta → rc → stable
  channels:
    dev:
      pattern: "dev-{n:5}"          # dev-00001, dev-00002, ... dev-99999
      ephemeral: true               # no git tag, image-only
      increment: n                  # auto-increments per push
      retention: 10                 # prune old dev tags, keep last 10
      # Dev builds push to Quay — no tag clutter on Docker Hub
      #
      # {n:WIDTH} pads the counter to WIDTH digits with leading zeros.
      # Ensures lexicographic sort matches numeric sort — tools like
      # Renovate, registry UIs, and `docker image ls` display correctly.
      #
      #   {n}       → 1, 2, ... 99999          (no padding)
      #   {n:3}     → 001, 002, ... 999
      #   {n:5}     → 00001, 00002, ... 99999
      #
      # Alternative patterns — it's all templates:
      #   "dev-{sha}"                 — hash-based, no counter needed
      #   "dev-{sha}-{n:3}"           — hash + padded counter for rebuild disambiguation
      #   "{branch}-{sha}"            — branch-scoped: feature-auth-abc1234
      #   "{branch}-{n:4}"            — branch-scoped padded counter: main-0042
      #   "dev-{env:CI_PIPELINE_IID}" — CI pipeline counter (GitLab native)

    alpha:
      pattern: "{next}-alpha.{n}"   # 0.3.0-alpha.1, 0.3.0-alpha.2
      from: stable                  # base version comes from latest stable tag
      increment: n                  # auto-increments the counter

    beta:
      pattern: "{next}-beta.{n}"    # 0.3.0-beta.1
      from: alpha                   # promotes from alpha
      increment: n

    rc:
      pattern: "{next}-rc.{n}"      # 0.3.0-rc.1
      from: beta                    # promotes from beta
      increment: n

    stable:
      pattern: "{major}.{minor}.{patch}"
      bump: [major, minor, patch]   # user chooses which segment to bump
      # stable tags trigger the full rolling tag set on Docker Hub
```

**Bump behavior:**

```bash
# Starting from latest stable tag: 0.2.0

$ stagefreight tag bump alpha
  → 0.3.0-alpha.1        # bumps minor from 0.2.0, starts alpha counter at 1

$ stagefreight tag bump alpha
  → 0.3.0-alpha.2        # increments alpha counter

$ stagefreight tag bump beta
  → 0.3.0-beta.1         # promotes from alpha, resets counter

$ stagefreight tag bump rc
  → 0.3.0-rc.1           # promotes from beta

$ stagefreight tag bump stable
  → 0.3.0                # drops prerelease suffix — this is the release

$ stagefreight tag bump patch
  → 0.3.1                # patch bump within stable

$ stagefreight tag bump minor
  → 0.4.0                # minor bump, resets patch

$ stagefreight tag bump major
  → 1.0.0                # major bump, resets minor and patch
```

**How `{next}` resolves:**

The `{next}` template in channel patterns resolves to the *next* version that the promotion ladder is targeting. This is determined by:

1. Find the latest stable tag (e.g., `0.2.0`)
2. Apply the default bump level (default: `minor`, configurable per-project)
3. Result: `0.3.0`

This means `alpha.1`, `beta.1`, and `rc.1` all target the same base version (`0.3.0`) until a stable release is cut. After `0.3.0` ships, the next `bump alpha` targets `0.4.0`.

```yaml
release:
  default_bump: minor              # what stagefreight tag bump alpha uses as base
```

Override at bump time: `stagefreight tag bump alpha --major` → `1.0.0-alpha.1`.

**Custom channels:**

Users can define their own channels for project-specific workflows:

```yaml
release:
  channels:
    nightly:
      pattern: "{base}-nightly.{env:CI_PIPELINE_IID}"
      ephemeral: true

    snapshot:
      pattern: "{base}-SNAPSHOT"
      ephemeral: true

    hotfix:
      pattern: "{major}.{minor}.{patch}"
      from: stable
      bump: [patch]                 # hotfix channel always bumps patch only

    lts:
      pattern: "{major}.{minor}.{patch}-lts"
      from: stable
      bump: [patch]                 # LTS releases track a specific minor
```

**Tag list output:**

```
$ stagefreight tag list
  CHANNEL    TAG                 DATE
  stable     0.2.0               2026-02-15
  alpha      0.3.0-alpha.2       2026-02-20
  alpha      0.3.0-alpha.1       2026-02-18
  stable     0.1.1               2026-01-10
  stable     0.1.0               2025-12-01
```

**Integration with image tagging:**

The channel system connects directly to the registry tag templates and filters. When `stagefreight tag bump alpha` creates `0.3.0-alpha.1`:

- The `git_tags: ["\\d+\\.\\d+\\.\\d+-.+"]` filter on the pre-release registry entry matches → pushes `0.3.0-alpha.1` tag only
- The `git_tags: ["^\\d+\\.\\d+\\.\\d+$"]` filter on the stable entry does NOT match → `latest` and rolling tags untouched

When `stagefreight tag bump stable` creates `0.3.0`:

- The stable filter matches → pushes `0.3.0`, `0.3`, `0`, `latest`
- The pre-release filter does NOT match → no duplicate push

This is the unified naming convention: channels control the version lifecycle, registry filters control which tags land where, and template resolution handles the image naming. One system, zero ambiguity.

**Vendor-aware registry links:**

When `--registry-links` is enabled (default), `release create` adds clickable links to the release for each configured registry. The link URL is vendor-aware:

| Provider | Link URL Pattern |
|---|---|
| Docker Hub | `https://hub.docker.com/r/{path}/tags?name={tag}` |
| GHCR | `https://github.com/{owner}/pkgs/container/{repo}` |
| Quay.io | `https://quay.io/repository/{path}?tag={tag}` |
| GitLab Registry | `{url}/{path}/container_registry` |
| JFrog Artifactory | `https://{url}/ui/repos/tree/General/{path}` |
| Generic | Raw image reference (`{url}/{path}:{tag}`) |

**Security summary in release notes — configurable detail level:**

The security scan results can be embedded in release notes at varying detail levels, controlled via `--security-detail` or the config `security.release_detail`. The level can also be conditional — different branches or release types can have different verbosity.

```
$ stagefreight release notes --security-summary .stagefreight/security/ --security-detail detailed
```

| Detail Level | What's Shown | Use Case |
|---|---|---|
| `none` | Nothing — security section omitted entirely | Public releases where you don't want to advertise vuln counts |
| `counts` | Severity count table only (Critical: 2, High: 5, etc.) | Standard releases — quick glance at the security posture |
| `detailed` | Count table + per-vulnerability CVE ID and one-line description | Internal releases, compliance-focused projects |
| `full` | Count table + CVE ID + description + affected package + fixed version | Security-focused repos, audit trail releases |

**Config-driven conditional detail:**

The `release_detail_rules` use separate `tag` and `branch` regex fields so there's no ambiguity about what's being matched. Each field is optional — if omitted, that dimension isn't tested. Both fields must match when both are present. Rules are evaluated top-down, first match wins.

```yaml
security:
  enabled: true
  sbom: true
  fail_on_critical: true

  # Default detail level when no rule matches
  release_detail: counts

  # Conditional rules — explicit tag/branch regex matching
  release_detail_rules:
    - tag: "^v\\d+\\.\\d+\\.\\d+$"     # stable semver tags (v1.2.3)
      detail: detailed                   # production releases get CVE-level detail

    - tag: "^v\\d+.*-rc"                # release candidates (v1.2.3-rc1)
      detail: full                       # RC releases get the full vulnerability table

    - branch: "^(main|master)$"         # main branch (non-tag builds)
      detail: counts                     # just the summary table

    - branch: "^(develop|feature/.*)"   # dev branches
      detail: none                       # omit security section entirely

    # Tag + branch combined: only match RC tags on release branches
    - tag: "^v\\d+.*-rc"
      branch: "^release/.*"
      detail: full
```

**Matching logic:**
- `tag` regex is tested against `CI_COMMIT_TAG` (or git tag). Only evaluated when a tag is present.
- `branch` regex is tested against `CI_COMMIT_BRANCH` (or git branch).
- If both `tag` and `branch` are specified, both must match (AND logic).
- If only `tag` is specified, the rule only applies to tag-triggered pipelines.
- If only `branch` is specified, the rule only applies to branch pipelines (no tag present).
- First matching rule wins. If no rule matches, falls back to `release_detail` default.

| Field | Matches Against | When Evaluated |
|---|---|---|
| `tag: "^v\\d+\\.\\d+\\.\\d+$"` | `CI_COMMIT_TAG` / git tag | Only when pipeline was tag-triggered |
| `branch: "^main$"` | `CI_COMMIT_BRANCH` / git branch | Always (branch is always known) |
| Both `tag` + `branch` | Tag AND branch must both match | Only when tag is present |

CLI override always wins: `--security-detail full` bypasses all rules.

**Example output at each level:**

`counts`:
```markdown
### Security Scan

| Severity | Count |
|---|---|
| **Critical** | **2** |
| High | 5 |
| Medium | 12 |

Total: 19 vulnerabilities
```

`detailed`:
```markdown
### Security Scan

| Severity | Count |
|---|---|
| **Critical** | **2** |
| High | 5 |

#### Critical Vulnerabilities
- **CVE-2026-1234** — Remote code execution in libfoo < 2.1.0
- **CVE-2026-5678** — SQL injection in database driver < 4.0.3

#### High Vulnerabilities
- **CVE-2026-2345** — Path traversal in archive extraction
- **CVE-2026-3456** — Denial of service via malformed input
- ... and 3 more (see full report in release assets)
```

`full`:
```markdown
### Security Scan

| Severity | CVE | Package | Installed | Fixed | Description |
|---|---|---|---|---|---|
| **Critical** | CVE-2026-1234 | libfoo | 1.9.2 | 2.1.0 | Remote code execution via crafted input |
| **Critical** | CVE-2026-5678 | db-driver | 3.8.1 | 4.0.3 | SQL injection in parameterized queries |
| High | CVE-2026-2345 | archive-lib | 0.4.0 | 0.4.1 | Path traversal in tar extraction |
| High | CVE-2026-3456 | http-parser | 1.2.3 | 1.2.4 | DoS via malformed Content-Length |
```

#### `stagefreight commit` — Interactive Item-by-Item Commit Flow

`git commit -m "stuff"` is the enemy of useful history. Developers write whatever comes to mind — inconsistent prefixes, no context, no categorization. Then when it's time to generate release notes, mine the log for breaking changes, or audit who changed what and why, the history is useless. Conventional Commits exists as a spec but nothing enforces it — it's an honor system that fails the moment someone is tired, rushed, or new to the project.

`stagefreight commit` replaces freeform commit messages with a guided, interactive flow. You select a commit type, compose structured notes line by line, and submit when done. The result is a machine-parseable, human-readable commit message that feeds directly into release notes, changelogs, and audit trails.

**The flow:**

```
$ stagefreight commit

  Staged: 3 files (src/build/buildx.go, src/build/engines/image.go, src/config/docker.go)

  Select commit type:
  ❯ fix        — Bug fix (patch version bump)
    feat       — New feature (minor version bump)
    refactor   — Code restructuring, no behavior change
    chore      — Build, tooling, dependency updates
    docs       — Documentation only
    test       — Adding or updating tests
    perf       — Performance improvement
    style      — Formatting, whitespace, semicolons
    ci         — CI/CD pipeline changes
    BREAKING   — Breaking change (major version bump)

  Scope (optional, tab-complete from changed paths): build
  Short summary: add registry auth with per-registry credentials

  Add detail lines (empty line to finish):
  > Login() method on Buildx authenticates before push
  > Credentials resolved from env var prefix set in .stagefreight.yml
  > Provider auto-detection from well-known domains
  >

  Commit mode:
  ❯ commit      — git commit directly
    mr          — commit + open merge request
    draft-mr    — commit + open draft merge request

  ✓ lint passed (3 files, 0.1s)
  ✓ committed: fix(build): add registry auth with per-registry credentials
```

The resulting commit message:

```
fix(build): add registry auth with per-registry credentials

- Login() method on Buildx authenticates before push
- Credentials resolved from env var prefix set in .stagefreight.yml
- Provider auto-detection from well-known domains
```

**Commit modules — standardized types with enforced language:**

Commit types are called **modules**. Each module defines:
- A **prefix** (the conventional commit type: `fix`, `feat`, `refactor`, etc.)
- A **description** shown in the selection menu
- A **version impact** (patch, minor, major, none) for automatic version bumping
- **Prompt templates** — what questions to ask, what format to enforce

Built-in modules cover the standard Conventional Commits types. But the key design: **everything is overrideable per repo.**

**Repo-level customization via `.stagefreight/commit.yml`:**

```yaml
# .stagefreight/commit.yml — override or extend commit modules per repo

# Override built-in module language/description
modules:
  fix:
    description: "Bug fix (triggers patch release)"
    prompts:
      summary: "What was broken?"
      detail_hint: "What caused it and how was it fixed?"
  feat:
    description: "New feature (triggers minor release)"
    prompts:
      summary: "What does the feature do?"
      detail_hint: "Implementation notes, design decisions"
  BREAKING:
    description: "Breaking change — requires migration (triggers major release)"
    prompts:
      summary: "What breaks?"
      detail_hint: "Migration steps, what consumers need to change"

  # Custom modules — repo-specific commit types
  migration:
    prefix: "migration"
    description: "Database schema migration"
    version_impact: patch
    prompts:
      summary: "What does the migration do?"
      detail_hint: "Tables affected, rollback strategy"
  security:
    prefix: "security"
    description: "Security fix or hardening"
    version_impact: patch
    prompts:
      summary: "What vulnerability or exposure?"
      detail_hint: "CVE reference, attack vector, mitigation"
  hotfix:
    prefix: "hotfix"
    description: "Emergency production fix"
    version_impact: patch
    prompts:
      summary: "What's on fire?"

# Commit modes — what happens after composing the message
modes:
  commit:
    enabled: true                    # always available
    default: true                    # selected by default
  mr:
    enabled: true                    # commit + open MR
    require_tests: true              # run tests before allowing MR mode
    require_lint: true               # run lint before allowing MR mode (default: true)
  draft-mr:
    enabled: true                    # commit + open draft MR
  force:
    enabled: false                   # disabled by default, skip all gates
    require_2fa: true                # when enabled, require 2FA confirmation

# Gates — conditions checked before the commit is finalized
gates:
  lint: true                         # run lint before commit (default: true)
  tests: false                       # run tests before commit (default: false)
  2fa: false                         # require 2FA for all commits (default: false)

# Scope auto-complete sources
scope:
  from_paths: true                   # suggest scopes from changed file paths (default)
  fixed:                             # additional fixed scope values
    - api
    - db
    - infra
    - ui
```

**Commit gates — dynamically configurable per mode:**

Each commit mode can independently require different gates:

| Gate | What it does | Default |
|---|---|---|
| `lint` | Run `stagefreight lint` on staged files before commit | **on** for all modes |
| `tests` | Run `stagefreight test --context source` before commit | **off** by default, can be required per mode |
| `2fa` | Require second-factor confirmation (TOTP, hardware key, or `stagefreight approve` flow) | **off** by default, useful for `force` mode |

Gates are checked in order. If any gate fails, the commit is blocked with a clear message about what failed and how to fix it. `force` mode exists as an escape hatch but can be locked behind `require_2fa: true` so it's deliberate, not accidental.

**How this feeds into release notes:**

Because every commit has a structured prefix and scope, `stagefreight release notes` can:
- **Categorize** changes by type (Features, Bug Fixes, Breaking Changes, etc.)
- **Sort** within categories by scope
- **Link** commits to MRs, issues, or external references
- **Highlight** breaking changes with migration instructions (from the detail lines)
- **Filter** noise — `chore` and `style` commits can be excluded from user-facing notes while remaining in the full changelog
- **Generate** accurate version bumps — if any commit since the last tag is `BREAKING`, bump major; if any is `feat`, bump minor; otherwise patch

```
## 1.3.0 — 2026-02-22

### Breaking Changes
- **build**: registry credentials now use explicit `credentials` field instead of URL-derived env vars
  Migration: add `credentials: DOCKERHUB` to your `.stagefreight.yml` registry config

### Features
- **build**: add branch filtering for multi-registry push targets
- **build**: auto-inject VERSION/COMMIT/BUILD_DATE build args from Dockerfile ARGs

### Bug Fixes
- **build**: fix registry auth running after build instead of before

### Documentation
- **test**: add database migration testing as built-in test type
```

This is generated automatically from the structured commit history — no manual release note writing, no parsing freeform messages with regex, no missed breaking changes.

**Two modes: interactive and flags-only.**

StageFreight auto-detects which mode to use. If a TTY is attached (developer terminal), it launches the guided flow. If no TTY (CI pipeline, script, cron), it requires all fields as flags and never pauses for input. You can also force either mode explicitly:

```bash
# Interactive (default when TTY detected, or forced with --interactive)
stagefreight commit
stagefreight commit --interactive

# Flags-only (default when no TTY, or forced with --no-interactive)
# All required fields as flags — zero prompts, zero pauses
stagefreight commit --type fix --scope build --summary "fix auth ordering" \
  --detail "Login() now runs before Build()" \
  --mode commit

# Multiple detail lines via repeated flags
stagefreight commit --type feat --scope api --summary "add user endpoints" \
  --detail "REST CRUD for /api/v1/users" \
  --detail "JWT auth middleware on all routes" \
  --mode mr

# Pipe detail lines from stdin
git diff --name-only | stagefreight commit --type chore --scope deps \
  --summary "update dependencies" --detail-stdin --mode commit
```

In flags-only mode, missing required fields (`--type`, `--summary`) cause an immediate error with usage — never a hanging prompt. This makes `stagefreight commit` safe to call from CI pipelines, git hooks, automation scripts, and other non-interactive contexts.

**`stagefreight commit --amend`** re-opens the interactive flow pre-populated with the previous commit's fields. Edit what you need, submit, and the commit is amended with the updated message. In flags-only mode, `--amend` replaces only the fields you pass — unspecified fields keep their existing values.

#### `stagefreight lint` — Built-In Code Quality Gate

Lint is not a feature you opt into — it's the floor. Every `stagefreight docker build`, `stagefreight build`, and `stagefreight dev build` runs lint first. Every commit through `stagefreight` passes through lint. It replaces pre-commit hooks, `.editorconfig` enforcement, and scattered CI lint jobs with a single, fast, cache-aware pass that developers never have to think about.

**Core philosophy: developer pace is sacred.** Lint exists to catch real problems, not to slow people down. Everything is cached. Only changed files are scanned. If your code didn't change and the rules didn't change, lint is a no-op instant pass. Developers should feel encouraged to build often, commit confidently, and never hesitate because "CI might fail on some formatting thing."

**What it checks:**

| Module | Detects | Default |
|---|---|---|
| `secrets` | Leaked credentials, API keys, tokens, private keys (TruffleHog + Gitleaks engine) | **on** |
| `unicode` | Invisible characters, bidi overrides, zero-width chars, homoglyphs, AI prompt injection, confusable whitespace (full `audit codebase` detection table) | **on** |
| `yaml` | YAML syntax errors, indentation issues, duplicate keys | **on** |
| `large-files` | Files exceeding configured size threshold (default 500KB) | **on** |
| `conflicts` | Unresolved merge conflict markers, case-sensitive filename collisions | **on** |
| `line-endings` | Mixed line endings, CRLF in non-Windows contexts, trailing whitespace, missing final newline | **on** |
| `tabs` | Tab characters in files that should use spaces (configurable per-language) | **on** |
| `kustomize` | Kustomize build validation — catches broken overlays before they hit the cluster | **off** (auto-enabled if `kustomization.yaml` detected) |
| `dockerfile` | Dockerfile lint — hadolint-style checks for best practices | **off** (auto-enabled if `Dockerfile` detected) |
| `shellcheck` | Shell script analysis — catches common bash/sh mistakes | **off** (auto-enabled if `.sh` files detected) |

Modules tagged **on** run by default with zero config. Modules tagged **off** auto-enable when relevant files are detected, but never force themselves on projects that don't have those file types.

**Tunable like logging verbosity:**

```
stagefreight lint                    # default: all default modules, changed files only
stagefreight lint --level full       # every module, every file, full scan
stagefreight lint --level changed    # default — only files changed since last clean lint
stagefreight lint --level off        # skip lint entirely (you asked for it)
stagefreight lint --module secrets   # run only the secrets module
stagefreight lint --no-module tabs   # run everything except tabs
```

Manifest config:
```yaml
lint:
  level: changed                    # full | changed (default) | off
  modules:
    secrets: true                   # individually toggle any module
    unicode: true
    yaml: true
    filesize:
      enabled: true
      options:
        max_bytes: 524288            # module-specific tuning (500 KB)
    conflicts: true
    line_endings: true
    tabs: false                     # example: disable for a project that uses tabs
    kustomize: true                 # force-enable even without auto-detection
    dockerfile: true
    shellcheck: true
  exclude:                          # paths to skip (glob patterns, respects .gitignore)
    - "vendor/**"
    - "generated/**"
    - "*.min.js"
```

If `lint:` is absent from the manifest, the default is `level: changed` with all default-on modules active. You get full protection by writing nothing. You tune or disable by being explicit.

**Delta-aware scanning:**
- `level: changed` (the default) computes the file delta from the last clean lint pass
- "Changed" means: file content changed, or the lint rules that apply to that file changed
- In CI: delta is computed against the target branch (MR diff)
- Locally: delta is computed against the last successful `stagefreight lint` run (cached state)
- `stagefreight lint --all` overrides delta and scans everything (useful for initial adoption or after rule changes)

**Cache-aware builds:**

Lint results are cached per-file as a content-addressed hash of `(file content, rule config, lint engine version)`. This means:
- **File unchanged + rules unchanged** → cached pass, zero work
- **File changed + rules unchanged** → re-lint only this file
- **Rules changed** → re-lint all files affected by that rule module (not everything — just the module that changed)
- **Lint engine version bumped** → full re-lint (rare, only on StageFreight upgrades)
- **Explicit cache bust** → `stagefreight lint --no-cache` for when you need a clean sweep

The cache lives in `.stagefreight/cache/lint/` (gitignored). CI runners benefit from persistent caches across pipeline runs. The daemon maintains its own cache per repo.

**Build integration:**

Every build command (`docker build`, `build`, `dev build`) runs lint as its first step. If lint fails, the build does not start. This is not a separate CI stage — it's baked into the build itself. The cache makes this effectively free on repeated builds where nothing relevant changed.

```
$ stagefreight docker build
  lint ✓ (3 files changed, 247 cached, 0.2s)
  build ✓ linux/amd64 (cached layers, 4.1s)
  push ✓ docker.io/prplanit/myapp:1.2.3
```

When lint is clean and nothing changed, you see:
```
$ stagefreight docker build
  lint ✓ (0 files changed, 250 cached, 0.0s)
  build ✓ linux/amd64 (fully cached, 0.8s)
```

When lint fails, you see exactly what and where:
```
$ stagefreight docker build
  lint ✗ 2 issues in 1 file
    src/auth.py:14  [secrets] Possible API key: AKIAIOSFODNN7EXAMPLE
    src/auth.py:31  [unicode] Zero-width space U+200B in string literal
  build skipped — lint failed
```

**Relationship to `audit codebase`:**

`stagefreight lint --module unicode` runs the same detection engine described in Phase 4's `audit codebase` section — same character tables, same severity levels, same fix mode. The difference is scope: `lint` runs on changed files during every build; `audit codebase` is a deep scan of the entire repository on demand. Think of lint as the per-commit guard and `audit codebase` as the periodic full sweep.

#### `stagefreight docker build` — Build Engine

The build subcommand is the workhorse. Registries have wildly different conventions for image naming, tagging, and manifest structure. StageFreight abstracts all of it behind a declarative config.

**Everything is Dockerfile-native.** StageFreight does not invent its own build format — it orchestrates `docker buildx` against standard Dockerfiles. The manifest declares *what* to build and *where* to push; the Dockerfile defines *how* the image is constructed. Multistage builds, scratch images, distroless bases, vendor S6-overlay patterns — whatever your Dockerfile does, StageFreight drives it.

**Core capabilities:**
- **Dockerfile-native** — standard Dockerfiles are the build definition. StageFreight never generates or rewrites Dockerfiles. Your Dockerfile is your Dockerfile.
- **Multistage build support** — first-class support for multistage Dockerfiles via `target` selection:
  - **Default:** builds the final stage (standard Docker behavior)
  - **Named targets:** `target: production` to build a specific stage — useful when the same Dockerfile has `dev`, `test`, and `production` stages
  - **Multiple targets from one Dockerfile:** define multiple build configs that each target a different stage, producing separate images from a single Dockerfile (e.g., `app` image from the `production` stage, `migrations` image from the `migrate` stage, `worker` image from the `worker` stage)
  - **Dev target:** `stagefreight dev build` can target a `dev` stage that includes debug tools, live-reload entrypoints, or relaxed permissions — while CI targets the `production` stage. Same Dockerfile, different output.
- **Multi-registry push** — build once, push to N registries in a single invocation. Each registry gets its own image name/path — no assumption that they match.
- **Multi-arch builds** — `linux/amd64`, `linux/arm64`, `linux/arm/v7`, etc. Builds all platforms and assembles OCI image indexes (manifest lists) automatically.
- **Multi-OS builds** — `linux`, `windows`, `freebsd` where base images support it. Rarely used but some projects need it (e.g., Windows Server Core variants).
- **Flexible tag strategies** — per-registry tag templates with variables: `{version}`, `{major}`, `{major}.{minor}`, `{branch}`, `{sha}`, `{date}`, `latest`. Each registry can have its own tag set. Supports vendor-specific conventions (e.g., LinuxServer.io uses `<version>-ls<build>` tags).
- **Registry-specific naming** — some registries expect `library/name`, others `org/name`, others `org/sub/name`. The config maps each freely.
- **Branch filtering** — control which branches trigger builds and which registries they push to. Main branch pushes to all registries, feature branches push only to dev registry, tags push release tags everywhere.
- **Scheduled/continuous rebuilds** — daemon mode can trigger rebuilds at configured intervals (daily, weekly, on-upstream-change) to pick up base image security patches without waiting for a code change. Frequency is per-repo configurable.
- **Build context detection** — auto-detect Dockerfile location, build args, target stages. Override everything in config.
- **Local dev mode** (`--local`) — builds for the current platform only, loads into local Docker daemon, skips registry push. A dev runs `stagefreight docker build --local` and gets the exact same image CI would produce, tagged for local use. No registry credentials needed.

**Semantic cache invalidation:**

Docker layer caching is fast but dumb. It tracks file changes per `COPY`/`ADD` instruction, not what those files *mean* to the build. Compiled languages with asset embedding create hidden dependencies that layer caching gets wrong:

| Pattern | Problem | What StageFreight does |
|---|---|---|
| Go `//go:embed static/*` | Frontend assets baked into the binary at compile time. CSS/JS changes don't invalidate the Go build layer — stale UI ships. | Parses `embed` directives, tracks referenced paths as cache keys for the Go build layer. Asset change → Go layer rebuilds. |
| Rust `include_str!("schema.sql")` | Compile-time file inclusion. Schema changes don't bust the Rust build cache. | Parses `include_str!`/`include_bytes!` macros, same treatment. |
| Webpack/Vite bundle → `COPY dist/ .` | Bundle is built in an earlier stage, copied into a later stage. If the bundle stage is cached but source changed, stale bundle ships. | Tracks stage dependencies — if stage A's inputs changed, stages that `COPY --from=A` invalidate too. |
| `ARG VERSION` baked into binary | Version string compiled into the binary via ldflags or build arg. Layer cache ignores build arg changes by default. | Treats build args as cache keys — different `VERSION` value → rebuild. |
| Generated code (`protoc`, `sqlc`, `go generate`) | `.proto` files change but generated `.go` files are committed and cached. Build uses stale generated code. | Optional `cache.watch` paths — changes to watched files invalidate specified layers regardless of what Docker thinks. |

This isn't a new build system. StageFreight doesn't replace buildx caching — it wraps it with a semantic layer that understands cross-layer and cross-file dependencies. When StageFreight detects that a cached layer would produce stale output, it injects `--no-cache-filter` for that specific stage. Everything else stays cached.

```yaml
docker:
  cache:
    # Explicit cache-busting rules for cross-layer dependencies
    watch:
      - paths: ["web/src/**", "web/public/**"]    # if frontend source changes...
        invalidates: [build-go]                    # ...rebuild the Go stage that embeds it
      - paths: ["proto/**"]                        # if proto files change...
        invalidates: [generate, build]             # ...regenerate and rebuild
    # Auto-detection (default: true) — StageFreight scans for embed directives,
    # include macros, and COPY --from references without manual config
    auto_detect: true
```

Most projects never write `cache:` config. Auto-detection handles the common cases (Go embed, Rust includes, multi-stage COPY). The explicit `watch` rules exist for project-specific patterns that auto-detection can't infer — custom code generation, templating engines, or config files that change build output without changing source.

**Manifest config for builds:**
```yaml
docker:
  # Build configuration
  context: .                        # build context path
  dockerfile: Dockerfile            # Dockerfile path (auto-detected if omitted)
  target: production                # multistage target (omit for final stage)
  platforms:                        # multi-arch/multi-OS targets
    - linux/amd64
    - linux/arm64
    - linux/arm/v7
  build_args:
    APP_VERSION: "{version}"

  # Registry targets — each registry is fully independent
  registries:
    - url: docker.io
      path: prplanit/stagefreight
      tags:
        - "{version}"               # e.g., 1.2.3
        - "{major}.{minor}"         # e.g., 1.2
        - "{major}"                 # e.g., 1
        - latest
      branches: [main, master]      # only push from these branches
      credentials: DOCKERHUB_TOKEN

    - url: ghcr.io
      path: sofmeright/stagefreight
      tags:
        - "{version}"
        - latest
      branches: [main]
      credentials: GITHUB_TOKEN

    - url: lscr.io
      path: sofmeright/stagefreight
      tags:
        - "{version}-ls{build}"     # LinuxServer.io convention
        - latest
      branches: [main]
      credentials: LSCR_TOKEN

    - url: registry.prplanit.com
      path: tools/stagefreight
      tags:
        - "{version}"
        - "{branch}-{sha:.7}"       # feature branch builds: dev-abc1234
        - "{branch}-latest"
      branches: ["*"]               # all branches push here
      credentials: GITLAB_TOKEN

  # Scheduled rebuilds (daemon mode)
  rebuild:
    schedule: weekly                 # daily | weekly | monthly | cron expression
    on_base_update: true             # rebuild when base image has new digest
    notify: true                     # alert on rebuild (via configured alert channels)
```

**Multiple images from one Dockerfile (multistage):**

For simple projects (one Dockerfile → one image), use `docker.registries` directly as shown above. For projects that produce multiple distinct images from a single Dockerfile, use `docker.images[]` instead — each entry is a full build config with its own target, registries, tags, and platforms. The two styles are mutually exclusive: use `registries` for single-image, `images` for multi-image.

```yaml
docker:
  dockerfile: Dockerfile
  platforms: [linux/amd64, linux/arm64]    # shared default, images can override

  images:
    # Main application — targets the "production" stage
    - name: app
      target: production
      registries:
        - url: docker.io
          path: prplanit/my-app
          tags: ["{version}", latest]
        - url: ghcr.io
          path: sofmeright/my-app
          tags: ["{version}", latest]

    # Database migrations — targets the "migrate" stage
    # Ships as a separate image, runs as an init container or Job
    - name: migrations
      target: migrate
      registries:
        - url: docker.io
          path: prplanit/my-app-migrations
          tags: ["{version}"]

    # Background worker — targets the "worker" stage
    - name: worker
      target: worker
      platforms: [linux/amd64]             # override: workers only run on amd64
      registries:
        - url: docker.io
          path: prplanit/my-app-worker
          tags: ["{version}", latest]
```

The corresponding Dockerfile:
```dockerfile
# ---- shared base ----
FROM node:22-alpine AS base
WORKDIR /app
COPY package*.json ./
RUN npm ci --production

# ---- migrations image ----
FROM base AS migrate
COPY migrations/ ./migrations/
ENTRYPOINT ["npm", "run", "migrate"]

# ---- worker image ----
FROM base AS worker
COPY src/worker/ ./src/worker/
ENTRYPOINT ["npm", "run", "worker"]

# ---- production image ----
FROM base AS production
COPY src/ ./src/
EXPOSE 8080
ENTRYPOINT ["npm", "start"]

# ---- dev image (never pushed, used by stagefreight dev build) ----
FROM production AS dev
RUN npm install --include=dev
COPY tests/ ./tests/
ENTRYPOINT ["npm", "run", "dev"]
```

`stagefreight docker build` builds all images in `docker.images`, sharing layer cache across targets. `stagefreight dev build` automatically targets the `dev` stage if one exists (configurable via `dev.target`), giving developers debug tools and test dependencies without bloating the production image.

#### Artifact Builds & Multi-Toolchain Pipelines

Not every build produces a container image. Some produce binaries, zips, installers, or tarballs. Some need multiple toolchains in sequence (Go + .NET, Rust + C, etc.). Some cross-compile for targets where containers don't apply (Windows `.exe`, macOS `.app`, embedded firmware).

Today these live as raw CI scripts — installing dependencies at runtime, fragile, slow, not reproducible locally. The entire point of StageFreight is to make these declarative and reproducible.

**`stagefreight build`** (not `docker build` — this is the general-purpose build command)

The key insight: **use Docker as the build environment even when the output isn't a Docker image.** A Dockerfile defines the reproducible toolchain, multistage builds compile the artifacts, and StageFreight extracts the outputs — binaries, zips, packages — from the final stage. The developer runs the same command locally and gets the same artifacts CI produces.

**Manifest config for artifact builds:**
```yaml
build:
  # Artifact builds — output is files, not container images
  artifacts:
    # Example: Beszel agent for Windows (Go + .NET cross-compile)
    - name: beszel-agent-windows
      dockerfile: build/Dockerfile.beszel-windows
      target: export                   # stage that holds the final artifacts
      platforms: [linux/amd64]         # build platform (cross-compiles TO windows inside)
      extract:                         # pull files OUT of the build container
        - from: /out/beszel-agent.exe
          to: dist/beszel-agent-windows/
        - from: /out/*.dll
          to: dist/beszel-agent-windows/
      package:
        - type: zip
          name: "beszel-agent-windows-{version}.zip"
          contents: dist/beszel-agent-windows/
      build_args:
        UPSTREAM_VERSION: "{version}"

    # Example: CLI tool for multiple OS/arch combos
    - name: my-cli
      dockerfile: build/Dockerfile.cli
      matrix:                          # build a matrix of targets
        - { goos: linux,   goarch: amd64, suffix: linux-amd64 }
        - { goos: linux,   goarch: arm64, suffix: linux-arm64 }
        - { goos: darwin,  goarch: amd64, suffix: darwin-amd64 }
        - { goos: darwin,  goarch: arm64, suffix: darwin-arm64 }
        - { goos: windows, goarch: amd64, suffix: windows-amd64, ext: .exe }
      extract:
        - from: "/out/my-cli{ext}"
          to: "dist/my-cli-{suffix}/"
      package:
        - type: tar.gz
          name: "my-cli-{version}-{suffix}.tar.gz"
          contents: "dist/my-cli-{suffix}/"
          if: "suffix != 'windows-amd64'"    # tar.gz for unix
        - type: zip
          name: "my-cli-{version}-{suffix}.zip"
          contents: "dist/my-cli-{suffix}/"
          if: "suffix == 'windows-amd64'"    # zip for windows

  # Where to upload release artifacts
  release_assets: true               # attach packages to the git release (via provider API)
```

The Beszel Windows example as a Dockerfile instead of a CI script:
```dockerfile
# ---- .NET SDK for LibreHardwareMonitor ----
FROM mcr.microsoft.com/dotnet/sdk:8.0 AS dotnet-build
WORKDIR /src
ARG UPSTREAM_VERSION
RUN git clone --depth 1 --branch ${UPSTREAM_VERSION} https://github.com/henrygd/beszel.git
WORKDIR /src/beszel/beszel
RUN dotnet build -c Release ./internal/agent/lhm/beszel_lhm.csproj

# ---- Go build with LHM DLLs ----
FROM golang:1.24-bullseye AS go-build
WORKDIR /src
COPY --from=dotnet-build /src/beszel /src/beszel
WORKDIR /src/beszel/beszel
RUN go mod tidy
WORKDIR /src/beszel/beszel/cmd/agent
RUN GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-w -s" -o /out/beszel-agent.exe

# ---- Collect all artifacts ----
FROM scratch AS export
COPY --from=go-build /out/beszel-agent.exe /out/
COPY --from=dotnet-build /src/beszel/beszel/internal/agent/lhm/bin/Release/net48/*.dll /out/
```

What was a 50-line CI script with `apt-get install` and `wget` is now a declarative, cached, reproducible Dockerfile. A developer runs `stagefreight build` locally and gets the same `beszel-agent-windows-v0.9.1.zip` that CI produces.

**How `stagefreight build` works:**
1. Reads `build.artifacts[]` from the manifest
2. For each artifact (or matrix expansion), runs `docker buildx build` targeting the specified stage
3. Uses `--output type=local` to extract files from the build container to the local filesystem
4. Runs `package` steps (zip, tar.gz, etc.) on extracted files
5. If `release_assets: true`, uploads packages to the git release via the provider API
6. All of this works identically for `stagefreight dev build` — devs get the same artifacts locally

**Matrix builds** expand a single artifact definition across multiple target combinations. Each matrix entry injects its variables as build args and template values. The build runs in parallel where possible (buildx cache sharing across matrix entries).

**Relationship to `docker build`:**
- `stagefreight docker build` — output is container images pushed to registries
- `stagefreight build` — output is files extracted from build containers, optionally packaged and attached to releases
- Both use Dockerfiles as the build definition. Both are reproducible locally. Both benefit from buildx caching. The only difference is what comes out.

#### `stagefreight dev` — Local Developer Experience

The entire point of codifying builds and tests in the manifest is that **every environment runs the same thing**. A dev on their laptop, CI in a pipeline, and the daemon in production all read the same `.stagefreight.yml` and execute the same logic. No "works on my machine" — the manifest IS the machine.

**`stagefreight dev build`**
- Detects the project type and runs the right build:
  - **If `docker:` config exists** (or Dockerfile found): builds a container image for the current platform, loads into local Docker daemon. Equivalent to `stagefreight docker build --local`.
  - **If `build.artifacts:` config exists**: builds artifacts for the current platform, extracts to `dist/`. Equivalent to `stagefreight build` scoped to the local OS/arch.
  - **If both exist**: builds both. Container images load locally, artifacts extract to disk.
- Tags images as `<app>:dev` (or `<app>:<branch>-dev` if on a feature branch)
- **Targets the `dev` stage** if the Dockerfile has one (configurable via `dev.target`). The dev stage typically extends the production stage with debug tools, test dependencies, and hot-reload entrypoints — stuff that never ships to production but makes local development ergonomic.
- Same Dockerfile, same build args as CI — just a different target stage and single platform
- No registry push, no credentials needed
- Fast iteration: rebuilds use layer cache, only changed layers rebuild

**`stagefreight dev test`**
- Spins up the locally built image using the `dev.test` config from the manifest
- Injects dev-mode environment variables, mounts, and test endpoints
- Runs health checks and sanity tests the developer has defined
- Reports pass/fail with clear output — same checks CI runs, same checks the daemon runs
- Tears down cleanly after tests complete (or keeps running with `--keep` for manual poking)

**`stagefreight dev up`**
- Brings up the full dev environment defined in the manifest (app + dependencies)
- Supports compose-style service dependencies (database, cache, etc.) via the `dev.services` block
- Wires up networking, volumes, and env vars per manifest config
- Stays running until `ctrl-c` or `stagefreight dev down`

**The dev workflow:**
```
$ cd my-project
$ stagefreight dev build        # build image for my platform
$ stagefreight dev test         # run health checks + sanity tests
$ stagefreight dev up           # spin up app + deps for manual testing
```

All three commands read the same `.stagefreight.yml`. A contributor clones the repo, installs the `stagefreight` binary, and has a working dev environment with validated tests — no README scavenger hunt for "how do I run this locally."

**Manifest config for dev/test:**
```yaml
dev:
  # Dockerfile stage to target for dev builds (auto-detected if "dev" stage exists)
  target: dev

  # Environment for local testing (overrides for dev context)
  env:
    DATABASE_URL: postgres://dev:dev@db:5432/app_dev
    REDIS_URL: redis://cache:6379
    LOG_LEVEL: debug
    SECRET_KEY: dev-secret-not-for-production

  # Service dependencies (spun up by `stagefreight dev up`)
  services:
    db:
      image: docker.io/library/postgres:17-alpine
      env:
        POSTGRES_USER: dev
        POSTGRES_PASSWORD: dev
        POSTGRES_DB: app_dev
      ports: [5432]
      healthcheck:
        test: pg_isready -U dev
        interval: 5s
        retries: 3

    cache:
      image: docker.io/library/redis:7-alpine
      ports: [6379]

  # Ports to expose from the app container during dev up
  ports:
    - "8080:8080"
    - "9090:9090"                  # metrics/debug port

  # Volume mounts for live reload / persistent dev data
  volumes:
    - ./src:/app/src               # live code mount for hot reload
    - dev-data:/app/data           # persistent dev data across restarts

  # Test suite — runs against the built image
  test:
    # Startup: bring up the app (and services if needed) before testing
    startup:
      timeout: 30s                 # max time to wait for app to be ready
      depends_on: [db, cache]      # ensure deps are healthy first

    # Health checks — HTTP/TCP probes that must pass
    healthchecks:
      - name: http-ready
        type: http
        url: http://localhost:8080/health
        expect_status: 200
        timeout: 5s

      - name: metrics-endpoint
        type: http
        url: http://localhost:9090/metrics
        expect_status: 200
        expect_body_contains: "app_requests_total"

      - name: db-connected
        type: tcp
        host: localhost
        port: 5432
        timeout: 3s

    # Sanity checks — custom commands run inside the app container
    # Devs package whatever validation they want here
    sanity:
      - name: migrations-current
        run: ./manage.py migrate --check
        description: "Verify all migrations are applied"

      - name: seed-data-loads
        run: ./manage.py loaddata testfixtures
        description: "Verify test fixtures load cleanly"

      - name: api-smoke-test
        run: curl -sf http://localhost:8080/api/v1/status | jq -e '.ok == true'
        description: "Basic API response validation"

      - name: unit-tests
        run: pytest tests/ -x --tb=short
        description: "Run unit test suite"

    # Exit behavior
    teardown: always               # always | on-success | never
```

**What `stagefreight dev test` actually does:**
1. Builds the image if not already built (or uses last `dev build`)
2. Starts service dependencies, waits for their healthchecks
3. Starts the app container with `dev.env` injected
4. Waits for `startup.timeout`, checking app healthchecks
5. Runs `test.healthchecks` — HTTP/TCP probes with expected results
6. Runs `test.sanity` — custom commands inside the container, in order, stops on first failure (or `--continue` to run all)
7. Reports results: pass/fail per check with timing
8. Tears down per `teardown` policy

**CI parity:** In a pipeline, `stagefreight dev test` runs identically — same manifest, same checks, same pass/fail criteria. The only difference is CI pushes the image to registries afterward if tests pass. The daemon uses the same test suite for scheduled rebuild validation before pushing patched images.

#### `stagefreight test` — Structured Testing Engine

The `dev.test` section above is the simple case — healthchecks and sanity commands against a running container. But real projects need more: unit tests that run before the build, integration tests that run against the binary, API contract tests that run against the container, and all of them should only run when relevant code changes. Today this means writing CI scripts until sunset with zero consistency between projects.

StageFreight's testing engine makes 80-90% of common test patterns declarative. You describe *what* to test, *when* it's relevant, and *what context* it runs in. StageFreight handles scheduling, scoping, parallelism, and reporting. The remaining 10-20% stays as custom scripts — but even those get structured metadata (name, scope, timeout, expected outcome) so they participate in the same delta-aware scheduling.

**Two dimensions: context and scope.**

Every test has a **context** (when it can run) and a **scope** (when it should run):

| Context | Runs against | Available when | Examples |
|---|---|---|---|
| `source` | Source files on disk | Always — no build required | Unit tests, static analysis, schema validation, config checks, contract tests against specs |
| `binary` | Built artifacts | After `stagefreight build` | CLI smoke tests, binary flag validation, output format checks, cross-compile verification |
| `container` | Running container | After `stagefreight docker build` | HTTP endpoint probes, healthchecks, API integration tests, database migration checks, e2e flows |

Context determines the *environment* the test executes in. `source` tests run on the host against files. `binary` tests run on the host against built artifacts. `container` tests run against (or inside) a running container with optional service dependencies.

**Scope** determines *whether* a test runs at all. A test declares what code it depends on — files, functions, packages, API routes, database schemas. StageFreight's delta engine (same one that powers lint) compares the scope against what changed. If nothing in the test's scope changed, the test is skipped with a cached pass. If something changed, the test runs.

```
$ stagefreight test
  source  ✓ 4 passed, 12 skipped (unchanged), 0.3s
  binary  ✓ 2 passed, 5 skipped (unchanged), 1.1s
  container ✓ 3 passed, 8 skipped (unchanged), 2.4s
  total: 9 passed, 25 skipped, 0 failed (3.8s)
```

On a change that only touches `src/auth/token.go`:

```
$ stagefreight test
  source  ✓ 2 passed (token-validation, token-expiry), 10 skipped, 0.2s
  binary  ✓ 1 passed (cli-auth-flag), 6 skipped, 0.4s
  container ✓ 1 passed (auth-api-endpoints), 10 skipped, 1.8s
  total: 4 passed, 26 skipped, 0 failed (2.4s)
```

Only tests whose scope overlaps with the changed file ran. Everything else was a cached skip.

**Declarative test types — the 80-90%:**

Most tests across most projects fall into a handful of patterns. StageFreight provides built-in test types that express these patterns as structured config instead of scripts:

```yaml
test:
  # --- Source context tests (pre-build) ---

  - name: go-unit-tests
    context: source
    type: go-test                     # built-in: runs `go test`
    scope:
      packages: ["./src/..."]         # only run when these packages change
    args: ["-race", "-count=1"]
    timeout: 2m

  - name: schema-valid
    context: source
    type: json-schema                 # built-in: validate files against JSON Schema
    scope:
      files: ["api/openapi.yml"]
    schema: api/openapi.yml
    validate:
      - "api/examples/*.json"

  - name: config-syntax
    context: source
    type: yaml-valid                  # built-in: YAML parse + optional schema check
    scope:
      files: [".stagefreight.yml", "config/*.yml"]
    files: [".stagefreight.yml", "config/*.yml"]

  - name: sql-migrations-ordered
    context: source
    type: file-check                  # built-in: assert files exist, match patterns, are ordered
    scope:
      files: ["migrations/*.sql"]
    checks:
      - sequential_names: true        # 001_init.sql, 002_users.sql — no gaps
      - no_down_without_up: true      # every down migration has a matching up

  - name: proto-compat
    context: source
    type: command                     # escape hatch — run any command
    scope:
      files: ["proto/**/*.proto"]
    run: "buf breaking --against .git#branch=main"
    expect:
      exit_code: 0

  # --- Binary context tests (post-build, pre-container) ---

  - name: cli-version-flag
    context: binary
    type: exec                        # built-in: run binary, check output
    scope:
      files: ["src/version/**", "src/cli/cmd/version.go"]
    command: "./dist/stagefreight version"
    expect:
      exit_code: 0
      stdout_contains: "stagefreight"
      stdout_not_contains: "unknown"

  - name: cli-help-all-commands
    context: binary
    type: exec
    scope:
      files: ["src/cli/cmd/*.go"]
    command: "./dist/stagefreight --help"
    expect:
      exit_code: 0
      stdout_contains:
        - "docker build"
        - "lint"
        - "version"

  - name: binary-size-check
    context: binary
    type: file-check
    scope:
      files: ["src/**/*.go", "go.mod", "go.sum"]
    checks:
      - path: "./dist/stagefreight"
        max_size: 50MB                # alert if binary bloats past threshold

  # --- Container context tests (running container) ---

  - name: health-endpoint
    context: container
    type: http                        # built-in: HTTP request + response validation
    scope:
      files: ["src/handlers/health.go", "src/server.go"]
    request:
      method: GET
      url: /health
    expect:
      status: 200
      max_latency: 500ms
      body_json:
        status: "ok"

  - name: auth-api-endpoints
    context: container
    type: http
    scope:
      files: ["src/auth/**", "src/handlers/auth*.go", "src/middleware/auth.go"]
      # Scope can reference API routes — StageFreight maps routes to handler files
      routes: ["/api/v1/auth/**"]
    requests:
      - name: login-valid
        method: POST
        url: /api/v1/auth/login
        body: '{"username": "testuser", "password": "testpass"}'
        headers:
          Content-Type: application/json
        expect:
          status: 200
          body_json_path:
            "$.token": { not_empty: true }
            "$.expires_in": { gte: 3600 }

      - name: login-invalid
        method: POST
        url: /api/v1/auth/login
        body: '{"username": "testuser", "password": "wrong"}'
        expect:
          status: 401

      - name: protected-without-token
        method: GET
        url: /api/v1/users/me
        expect:
          status: 401

      - name: protected-with-token
        method: GET
        url: /api/v1/users/me
        headers:
          Authorization: "Bearer {from_step: login-valid, json_path: $.token}"
        expect:
          status: 200
          body_json_path:
            "$.username": "testuser"

  - name: database-migrations
    context: container
    type: command
    scope:
      files: ["migrations/**"]
    depends_on: [db]                  # ensure db service is up first
    run: "./manage.py migrate --check"
    expect:
      exit_code: 0

  - name: grpc-reflection
    context: container
    type: grpc                        # built-in: gRPC endpoint probing
    scope:
      files: ["proto/**", "src/grpc/**"]
    endpoint: localhost:50051
    checks:
      - reflection: true              # server supports reflection
      - service: myapp.v1.UserService # service is registered
      - method: GetUser               # method exists
        request: '{"id": "test-1"}'
        expect:
          status: OK
          body_json_path:
            "$.name": { not_empty: true }
```

**Built-in test types:**

| Type | Context | What it does |
|---|---|---|
| `go-test` | source | Runs `go test` with package filtering and args. Knows Go test conventions. |
| `npm-test` | source | Runs `npm test` or `jest`/`vitest` with file filtering. |
| `pytest` | source | Runs `pytest` with path filtering, markers, and fixtures. |
| `cargo-test` | source | Runs `cargo test` with package filtering. |
| `json-schema` | source | Validates files against a JSON Schema. |
| `yaml-valid` | source | Parses YAML files, optionally validates against schema. |
| `file-check` | source, binary | Asserts file existence, size, permissions, content patterns, naming conventions. |
| `exec` | binary | Runs a binary, checks exit code + stdout/stderr content. |
| `http` | container | HTTP request with full response validation (status, headers, body, latency, JSON path assertions). |
| `tcp` | container | TCP port probe with timeout. |
| `grpc` | container | gRPC endpoint probing — reflection, service listing, method calls with assertions. |
| `websocket` | container | WebSocket connection, message exchange, assertions. |
| `migration` | container | Database migration testing — spins up a database, runs migrations, verifies schema and data integrity across upgrade paths. |
| `command` | any | Escape hatch — run any command. Structured metadata (scope, timeout, expect) still applies. |

Every built-in type understands its domain. `http` knows about status codes, headers, JSON path queries, and request chaining (use a token from step A in step B). `go-test` knows about Go package paths and test binary caching. `grpc` knows about reflection and protobuf. `migration` knows about database engines, migration tools, and schema state. The developer describes *what* to verify, not *how* to shell out to curl and parse the output.

**Scope mechanics — how delta-aware test selection works:**

Scope is the key differentiator. Every test declares what it depends on, and StageFreight skips tests whose dependencies haven't changed. Scope supports several selectors:

| Selector | What it means | Example |
|---|---|---|
| `files` | Glob patterns for source files | `["src/auth/**", "src/middleware/auth.go"]` |
| `packages` | Language-aware package paths | `["./src/handlers/...", "./src/models/..."]` |
| `functions` | Specific function names in specific files | `["src/auth/token.go:ValidateToken", "src/auth/token.go:RefreshToken"]` |
| `routes` | API route patterns | `["/api/v1/users/**", "/api/v1/auth/**"]` |
| `tags` | Arbitrary labels for manual grouping | `["auth", "payments", "critical"]` |
| `always` | Always run regardless of changes | `true` — for smoke tests, health checks |

How selectors resolve:

- **`files`** — compared directly against the git delta (same engine as lint). Glob match against changed file paths.
- **`packages`** — resolved to files via the language's module system (Go: `go list`, Node: package.json workspaces, Python: module paths). Then compared against the delta.
- **`functions`** — StageFreight parses the source file for the named function/method and tracks the line span. If the delta includes changes within that span (or to imports the function uses), the test runs. This uses tree-sitter or language-specific regex parsing — not a full compiler. Approximate is fine because false positives (test runs unnecessarily) are safe; false negatives (test doesn't run when it should) are caught by the `always` fallback tests.
- **`routes`** — StageFreight parses route definitions from the framework's conventions (Go: `http.HandleFunc`, `gin.GET`, `echo.POST`; Node: `express.get`, `fastify.route`; Python: `@app.route`, `@router.get`). A route maps to its handler file. If the handler file changed, tests scoped to that route run.
- **`tags`** — manual grouping. `stagefreight test --tag auth` runs all tests tagged `auth` regardless of delta. Useful for targeted debugging.
- **`always`** — the test always runs. Use for critical smoke tests where the cost of a false negative is too high.

When no `scope` is defined on a test, it defaults to `always: true` — backward compatible with the simple case where you just want tests to run every time.

**Scope composition — implicit dependencies:**

StageFreight also infers implicit scope from the test type:

- A `go-test` for `./src/auth/...` automatically includes `go.mod` and `go.sum` in its scope (dependency changes affect test results).
- An `http` test against a container automatically includes the Dockerfile and any files `COPY`'d into the image (if the image changed, the test is relevant).
- A `command` test that runs `./dist/myapp` automatically includes the binary itself (if the binary changed, the test is relevant).

Inferred scope is merged with explicit scope. You can disable it with `scope.infer: false` if needed.

**Test chaining and data flow:**

Container tests often need state from earlier steps — a login token, a created resource ID, a session cookie. Rather than forcing everything into a single script, StageFreight supports structured data flow between test steps:

```yaml
- name: user-crud-flow
  context: container
  type: http
  scope:
    routes: ["/api/v1/users/**"]
  requests:
    - name: create-user
      method: POST
      url: /api/v1/users
      body: '{"name": "Test User", "email": "test@example.com"}'
      expect:
        status: 201
        body_json_path:
          "$.id": { not_empty: true, capture_as: user_id }

    - name: get-user
      method: GET
      url: "/api/v1/users/{captured: user_id}"
      expect:
        status: 200
        body_json_path:
          "$.name": "Test User"

    - name: delete-user
      method: DELETE
      url: "/api/v1/users/{captured: user_id}"
      expect:
        status: 204

    - name: get-deleted-user
      method: GET
      url: "/api/v1/users/{captured: user_id}"
      expect:
        status: 404
```

Values captured in earlier steps are available to later steps via `{captured: name}` or `{from_step: step_name, json_path: $.field}`. This replaces the common pattern of piping curl output through jq in a shell script — same logic, structured and readable.

**Parallel execution and ordering:**

- Tests within the same context run in parallel by default (each gets its own goroutine/container).
- Tests with `depends_on` run after their dependencies pass.
- Tests with `sequential: true` run in declaration order (for stateful sequences like the CRUD flow above).
- Cross-context ordering is automatic: `source` tests run before `binary` tests, which run before `container` tests. If source tests fail, binary and container tests are skipped — no point testing a binary built from broken source.

```
stagefreight test
  │
  ├── source tests (parallel)     ← pre-build, against files
  │     ├── go-unit-tests
  │     ├── schema-valid
  │     ├── config-syntax
  │     └── sql-migrations-ordered
  │
  ├── [build happens here if binary/container tests exist]
  │
  ├── binary tests (parallel)     ← post-build, against artifacts
  │     ├── cli-version-flag
  │     ├── cli-help-all-commands
  │     └── binary-size-check
  │
  ├── [container starts here if container tests exist]
  │
  └── container tests (parallel, some sequential within)
        ├── health-endpoint
        ├── auth-api-endpoints (sequential internally)
        ├── database-migrations
        └── grpc-reflection
```

**Build integration:**

`stagefreight test` runs the full pipeline: source → build → binary → container. But each context also integrates with its natural build phase:

- `stagefreight docker build` runs source and binary tests automatically (same as lint — unless `--skip-test`). Container tests run if `--test` is passed.
- `stagefreight build` runs source tests before building, binary tests after.
- `stagefreight dev test` is the container-focused entrypoint — it builds, starts services, and runs all three contexts.
- `stagefreight test --context source` runs only source tests (fast, no build needed).
- `stagefreight test --context container` runs all contexts up to and including container.
- `stagefreight test --tag auth` runs only tests tagged `auth`, in whatever contexts they belong to.

**Cache integration:**

Test results are cached the same way lint results are cached — content-addressed by `(scope file hashes, test config hash, engine version)`. A test whose scope hasn't changed and whose config hasn't changed is a cached pass. The cache lives in `.stagefreight/cache/test/`. In CI, persistent caches across runs make the common case (nothing changed in this test's scope) effectively free.

**Manifest config:**

```yaml
test:
  # Global test settings
  parallel: true                    # run tests in parallel within each context (default: true)
  fail_fast: false                  # stop on first failure (default: false)
  timeout: 10m                      # global timeout for entire test suite

  # Container test settings (shared by all container-context tests)
  container:
    startup:
      timeout: 30s
      depends_on: [db, cache]       # services to start before container tests
    base_url: http://localhost:8080  # default base URL for HTTP tests
    teardown: always                # always | on-success | never

  # Test definitions (source, binary, and container contexts interleaved)
  cases:
    - name: go-unit-tests
      context: source
      type: go-test
      scope:
        packages: ["./src/..."]
      args: ["-race"]
      # ... (as shown above)
```

**Zero-config behavior:** Without a `test:` section, `stagefreight test` looks for conventional test commands:
- Go project → `go test ./...`
- Node project → `npm test`
- Python project → `pytest`
- Rust project → `cargo test`
- Dockerfile with HEALTHCHECK → HTTP probe against exposed ports

The conventional tests run without scoping (full run every time). Adding a `test:` section with explicit scopes is how you get delta-aware test selection.

#### `migration` Test Type — Declarative Database Migration Testing

Database migrations are the most common source of deployment failures and the hardest to test properly. A fresh `CREATE TABLE` works, but does `ALTER TABLE` preserve existing rows? Does a migration that adds a NOT NULL column with a default actually backfill a million rows without timing out? Does rolling back migration 47 leave the schema in a state that migration 46 expects? Today, teams either don't test migrations at all (pray on deploy), write fragile bash scripts that spin up databases in CI, or maintain a parallel test harness that drifts from the real migration path.

StageFreight's `migration` test type makes this declarative. You describe the migration path and the expected outcome. StageFreight handles database lifecycle, migration execution, and state verification.

**The problem:** A typical migration test requires:
1. Spin up a fresh database (right engine, right version)
2. Run migrations (which tool? what order? what flags?)
3. Verify schema state (tables exist, columns correct, constraints in place)
4. Optionally: seed data at an intermediate point, run remaining migrations, verify data survived
5. Tear down

This is 50-100 lines of bash per test scenario. Most teams don't bother. When they do, the test breaks when the migration tool changes or the CI environment drifts. StageFreight turns this into config.

**Declarative migration test workflow:**

```yaml
test:
  cases:
    # Fresh schema — all migrations from zero
    - name: db-migrations-fresh
      context: container
      type: migration
      scope:
        files: ["migrations/**", "sql/**"]
      engine: postgres
      image: docker.io/library/postgres:17
      steps:
        - action: migrate
          source: migrations/
        - action: query
          sql: "SELECT table_name FROM information_schema.tables WHERE table_schema = 'public';"
          expect:
            rows_contain: [users, sessions, audit_log]

    # Upgrade path — seed data at a midpoint, verify it survives later migrations
    - name: db-migrations-upgrade
      context: container
      type: migration
      scope:
        files: ["migrations/**"]
      engine: postgres
      image: docker.io/library/postgres:17
      steps:
        - action: migrate
          source: migrations/
          up_to: "003"
        - action: seed
          sql: |
            INSERT INTO users (id, name) VALUES (1, 'Test'), (2, 'Admin');
        - action: migrate
          source: migrations/
          from: "004"
        - action: query
          sql: "SELECT id, name FROM users ORDER BY id;"
          expect:
            row_count: 2
            rows:
              - { id: 1, name: "Test" }
              - { id: 2, name: "Admin" }

    # Rollback safety — apply all, rollback one, verify clean state
    - name: db-migrations-rollback
      context: container
      type: migration
      scope:
        files: ["migrations/**"]
      engine: postgres
      image: docker.io/library/postgres:17
      steps:
        - action: migrate
          source: migrations/
        - action: snapshot
          name: full-schema
        - action: rollback
          source: migrations/
          steps: 1
        - action: migrate
          source: migrations/
        - action: diff
          against: full-schema
          expect:
            identical: true

    # MySQL/MariaDB — same patterns, different engine
    - name: db-migrations-mysql
      context: container
      type: migration
      scope:
        files: ["migrations/**"]
      engine: mysql
      image: docker.io/library/mysql:8.4
      env:
        MYSQL_ROOT_PASSWORD: test
        MYSQL_DATABASE: testdb
      steps:
        - action: migrate
          source: migrations/
        - action: query
          sql: "SHOW TABLES;"
          expect:
            rows_contain: [users, sessions]
```

**Built-in actions:**

| Action | What it does |
|---|---|
| `migrate` | Run migrations from `source` directory. Supports `up_to` (stop at migration N), `from` (start from migration N), `direction: down` for rollback. |
| `seed` | Execute raw SQL against the database. Use for inserting test data at specific points in the migration sequence. |
| `query` | Execute a SELECT query and assert on the results. Supports `row_count`, `rows_contain`, `rows` (exact match), and `columns`. |
| `exec` | Execute arbitrary SQL (DDL/DML) without result assertions. For setup steps that aren't seeds (e.g., creating extensions). |
| `rollback` | Roll back N migration steps. Requires the migration tool to support down migrations. |
| `snapshot` | Capture the current schema state as a named snapshot. Used with `diff` to verify migrations are idempotent or reversible. |
| `diff` | Compare current schema against a named snapshot. Asserts `identical: true` or reports structural differences (added/removed tables, columns, constraints). |

**Migration tool auto-detection:**

StageFreight inspects the project to determine which migration tool to use. No explicit `tool:` config needed unless you want to override.

| Detected from | Tool | Command |
|---|---|---|
| `atlas.hcl` or `.atlas/` | Atlas | `atlas migrate apply` |
| `dbmate.yml` or `db/migrations/` with `-- migrate:up` comments | dbmate | `dbmate up` |
| Numbered `.sql` files (`001_*.up.sql`, `001_*.down.sql`) | golang-migrate | `migrate -path ... -database ... up` |
| `goose_db_version` table pattern or `-- +goose Up` comments | goose | `goose up` |
| `knexfile.js` or `knexfile.ts` | Knex | `knex migrate:latest` |
| `prisma/schema.prisma` | Prisma | `prisma migrate deploy` |
| `alembic.ini` or `alembic/` | Alembic | `alembic upgrade head` |
| `manage.py` + `django` in requirements | Django | `python manage.py migrate` |
| Plain numbered `.sql` files (fallback) | Raw SQL | Applied in lexicographic order via `psql`/`mysql` |

**Database lifecycle:**

StageFreight manages the database container automatically:

1. **Start** — pulls the specified `image`, starts it with ephemeral storage, waits for readiness (port probe + connection test)
2. **Connect** — generates a connection string from the engine type and passes it to the migration tool (or makes it available as `$DATABASE_URL`)
3. **Execute** — runs the `steps` in order against the live database
4. **Tear down** — stops and removes the container. On failure, optionally dumps the database state for debugging (`on_failure: dump`)

Engine-specific readiness probes:
- **postgres**: `pg_isready` + test connection
- **mysql/mariadb**: `mysqladmin ping` + test connection
- **sqlite**: file exists (no container needed — SQLite tests run directly on disk)

**Supported engines:**

| Engine | Images | Connection |
|---|---|---|
| `postgres` | `postgres:17`, `postgres:16`, `postgres:15`, etc. | `postgres://test:test@localhost:5432/testdb?sslmode=disable` |
| `mysql` | `mysql:8.4`, `mysql:8.0`, `mariadb:11`, etc. | `mysql://root:test@localhost:3306/testdb` |
| `sqlite` | (no container) | `sqlite3:///tmp/test.db` |

**Why this matters:**

- **Fresh install testing** — verify that running all migrations from zero produces a valid schema. Catches "works on my machine" where the dev database has manual tweaks.
- **Upgrade path testing** — verify that existing data survives migrations. Catches `ALTER TABLE DROP COLUMN` that silently destroys user data.
- **Rollback testing** — verify that down migrations actually undo what up migrations did. Catches irreversible migrations hiding behind a `down.sql` that doesn't actually work.
- **Seed persistence** — verify that reference data, default configs, or admin accounts survive the full migration path. Catches migrations that truncate tables or reset sequences.
- **Cross-version testing** — same test matrix, different database versions. Run migrations against Postgres 15, 16, and 17 to catch version-specific behavior.

#### `stagefreight approve` — Human-in-the-Loop Approval Gates

Some operations are too dangerous to run unattended. Pushing directly to a production registry, force-merging without review, running yolo mode in a pipeline — these are actions where automation should pause and wait for a human to explicitly say "yes, do it." Not a confirmation prompt in a terminal. A real approval flow: notification sent, identity verified, decision recorded.

**The problem:** CI/CD pipelines are all-or-nothing. Either the pipeline has credentials and auto-pushes (hope nobody pushes a bad commit), or every push requires manual intervention (defeats the point of automation). YOLO mode in StageFreight automates MR creation and merging — but "auto-merge everything" is a trust level that not every action deserves. Some operations need a human gate without losing the automation.

**What `stagefreight approve` does:**

1. The pipeline (or CLI) reaches an action that requires approval.
2. StageFreight sends an approval request via configured notification channel — SMTP, ntfy, SMS, or stdout.
3. The request contains a link to an approval page hosted by `stagefreight serve`.
4. The user clicks the link, authenticates, reviews the action, and accepts or denies.
5. The pipeline resumes or aborts based on the decision.
6. Everything is logged — who approved, when, what they approved, and any notes they attached.

**Notification delivery — how the approval request reaches you:**

| Channel | How it works |
|---|---|
| **SMTP** | Email with branded template — org logo, action summary, approval link. Works everywhere. |
| **ntfy** | Push notification via ntfy topic. Instant on mobile. Link opens in browser. |
| **SMS** | Via configurable SMS provider API (Twilio, etc.). Approval link in the message body. |
| **stdout** | Prints the approval URL to the terminal. For local dev and debugging. No external service needed. |

Channels are configured globally in the daemon config or per-repo in the manifest. Multiple channels can fire simultaneously (email + ntfy push). The notification is a one-way alert — authentication and approval happen on the approval page, never in the notification itself.

**The approval page — two-phase disclosure:**

The approval page is a web UI served by `stagefreight serve`. It uses a two-phase information model to balance security with usability:

**Phase 1 — Pre-authentication (public landing page):**

The page shows minimal, low-risk information. Enough for the user to know *something* needs their attention, not enough to leak details to an unauthorized viewer.

```
┌──────────────────────────────────────────────────┐
│  [Org Logo]  PrecisionPlanIT                     │
│                                                  │
│  An action requires your approval.               │
│                                                  │
│  Requested: 2 minutes ago                        │
│  Expires: in 28 minutes                          │
│                                                  │
│  ┌──────────────────────────────────────────┐    │
│  │  Sign in to review details               │    │
│  │                                          │    │
│  │  [Continue with SSO]                     │    │
│  │                                          │    │
│  └──────────────────────────────────────────┘    │
└──────────────────────────────────────────────────┘
```

No repo name, no branch, no action type. Just "something needs approval" and a sign-in button. If an attacker intercepts the notification link, they see nothing useful and can't act without valid credentials.

**Phase 2 — Post-authentication (full details):**

After OIDC SSO login (or other auth method), the authorized user sees everything:

```
┌──────────────────────────────────────────────────┐
│  [Org Logo]  PrecisionPlanIT                     │
│  Signed in as: kai@precisionplanit.com           │
│                                                  │
│  ── Action Details ──────────────────────────     │
│                                                  │
│  Action:  docker push (3 registries)             │
│  Repo:    sofmeright/stagefreight       │
│  Branch:  main                                   │
│  Commit:  abc1234 — "add multi-arch build"       │
│  Trigger: yolo mode — auto-push after merge      │
│                                                  │
│  Tags to push:                                   │
│    docker.io/prplanit/stagefreight:1.2.3         │
│    docker.io/prplanit/stagefreight:latest        │
│    ghcr.io/sofmeright/stagefreight:1.2.3         │
│                                                  │
│  Lint:  ✓ passed (0.3s)                          │
│  Test:  ✓ 12 passed (8.1s)                       │
│  Scan:  ✓ 0 critical, 2 low                      │
│                                                  │
│  ┌─────────────────────────────────────┐         │
│  │ Note (optional):                    │         │
│  │ __________________________________ │         │
│  │ __________________________________ │         │
│  └─────────────────────────────────────┘         │
│                                                  │
│  [Approve]              [Deny]                   │
│                                                  │
│  Expires: in 26 minutes                          │
└──────────────────────────────────────────────────┘
```

The user sees exactly what they're approving — the action, the context, the safety checks that passed, and the artifacts that will be produced. They can attach an optional note explaining why they approved or denied (for audit trail and accounting). Then they click Approve or Deny.

**Authentication methods:**

| Method | Priority | Notes |
|---|---|---|
| **OIDC SSO** | Primary, implement first | Preferred path. Uses existing identity provider (Zitadel, Keycloak, Auth0, etc.). Single sign-on — if you're already logged in, one click. No additional secret to manage. |
| **Passkey / WebAuthn** | High | FIDO2 hardware keys (YubiKey), platform authenticators (Touch ID, Windows Hello). Strongest security — phishing-resistant, no shared secret. No additional auth step if the key is the SSO credential. |
| **TOTP** | Medium | Time-based one-time password (Google Authenticator, Authy). Fallback for environments without SSO. Works offline. |
| **Email OTP** | Low | One-time code sent to email. Last-resort fallback. Depends on email delivery. |

The priority implementation path: OIDC SSO first, everything else later. SSO covers the primary use case (team with an identity provider). Passkey support follows naturally since most OIDC providers support WebAuthn as a second factor. TOTP and email OTP are escape hatches for environments that can't do SSO.

**Pipeline-side flow — how the waiting process works:**

```
stagefreight docker build --push
  │
  ├── lint ✓
  ├── build ✓
  ├── test ✓
  │
  ├── approval required (push to 3 registries)
  │     │
  │     ├── send notification (smtp + ntfy)
  │     ├── print: "⏳ Awaiting approval: https://sf.prplanit.com/approve/abc123"
  │     │
  │     ├── poll loop (5s interval)
  │     │     ├── GET /api/approve/abc123/status → "pending"
  │     │     ├── GET /api/approve/abc123/status → "pending"
  │     │     ├── GET /api/approve/abc123/status → "approved"
  │     │     └── response includes: approver, timestamp, note
  │     │
  │     └── approval received ✓
  │           approved by: kai@precisionplanit.com
  │           note: "Verified changelog, good to ship"
  │
  └── push ✓ (3 registries, 12s)
```

The process sleeps and polls at a configurable interval until one of three outcomes:
- **Approved** — pipeline continues. The approver's identity and optional note are logged.
- **Denied** — pipeline aborts with a clear error. The denier's identity and note are logged.
- **Timeout** — pipeline aborts. Default timeout is 30 minutes, configurable per-action and per-repo.

In local CLI mode, `stagefreight approve` can also print the URL to stdout and wait. No `stagefreight serve` needed — the approval endpoint can be a temporary local HTTP server that shuts down after the decision.

**Audit logging — who approved what:**

Every approval decision is recorded as a structured audit event:

```json
{
  "id": "approve-abc123",
  "action": "docker-push",
  "repo": "sofmeright/stagefreight",
  "branch": "main",
  "commit": "abc1234",
  "requested_at": "2026-02-22T14:30:00Z",
  "requested_by": "pipeline:gitlab-ci:12345",
  "decision": "approved",
  "decided_at": "2026-02-22T14:32:15Z",
  "decided_by": {
    "email": "kai@precisionplanit.com",
    "provider": "oidc",
    "issuer": "https://zitadel.precisionplanit.com"
  },
  "note": "Verified changelog, good to ship",
  "expires_at": "2026-02-22T15:00:00Z",
  "artifacts": [
    "docker.io/prplanit/stagefreight:1.2.3",
    "ghcr.io/sofmeright/stagefreight:1.2.3"
  ]
}
```

Audit events are stored locally (JSON lines file) and optionally forwarded to external systems (webhook, syslog, S3). The audit log is the compliance answer to "who authorized this production push at 2am?"

**What actions require approval — configurable per-repo:**

Approval gates are opt-in. By default, nothing requires approval (backward compatible). The manifest controls which actions are gated:

```yaml
approve:
  # Notification channels for this repo (inherits daemon defaults if not set)
  notify:
    - type: smtp
      to: kai@precisionplanit.com
    - type: ntfy
      topic: stagefreight-approvals

  # Authentication config
  auth:
    provider: oidc
    issuer: https://zitadel.precisionplanit.com
    client_id: stagefreight
    # Allowed approvers — email patterns or OIDC groups
    allowed:
      - kai@precisionplanit.com
      - group:platform-team

  # Which actions require approval
  gates:
    docker_push: true               # pushing images to registries
    release_create: true            # creating releases
    yolo_merge: true                # auto-merging MRs in yolo mode
    branch_delete: false            # cleaning up branches (low risk, skip)

  # Timing
  timeout: 30m                      # how long to wait before aborting
  poll_interval: 5s                 # how often to check for decisions
  reminder: 15m                     # send a reminder if still pending after this

  # Audit
  audit:
    store: local                    # local | s3 | webhook
    # s3_bucket: my-audit-bucket
    # webhook_url: https://audit.example.com/events
```

**Safe yolo mode — this is the key use case:**

The approval system makes yolo mode safe for production. Instead of "auto-merge everything blindly," you get "auto-merge everything, but pause for a human check before the push hits production registries." The workflow becomes:

1. Developer pushes to `main`.
2. Pipeline runs: lint, build, test, scan — all automated, all fast.
3. Pipeline reaches the push step. Approval gate fires.
4. Developer gets a push notification on their phone. Taps the link. Sees the build passed, scans are clean, tags look right. Taps Approve.
5. Push completes. Image is in the registry. Pipeline is green.

Total human involvement: one tap on a phone. Total automation: everything except the final "yes." That's safe yolo mode — the automation does the work, the human provides the authorization.

**Implementation priority — start with OIDC SSO:**

The approval UI panel is the first piece of `stagefreight serve`'s web interface. It starts minimal:

1. **Approval endpoint** — `/approve/{id}` serves the two-phase page (pre-auth landing → post-auth details).
2. **OIDC login** — redirect to the configured OIDC provider, receive the ID token, extract identity.
3. **Decision API** — `POST /api/approve/{id}/accept` and `POST /api/approve/{id}/deny` with optional note.
4. **Poll API** — `GET /api/approve/{id}/status` for the waiting pipeline to poll.
5. **Audit log** — write to a local JSON lines file.

No dashboard, no admin panel, no settings UI. Just the approval flow. Everything else in `stagefreight serve` builds on top of this foundation later.

**Web UI foundations — built into the approval page from day one:**

The approval page is the first web surface StageFreight ships. Every UI pattern established here carries forward to the full `stagefreight serve` dashboard later. Get it right once, reuse everywhere.

- **Language selector** — the approval page reads `Accept-Language` from the browser and renders in the user's preferred language by default. A language picker in the page header lets the user override. All UI strings come from the same docs module translation system that powers `--help` text and man pages — the approval page is just another render target. Setting `STAGEFREIGHT_LANG` or `LANG` in the environment works for CLI output; the web UI uses browser headers + user preference stored in a cookie.
- **Light/dark mode** — respects `prefers-color-scheme` from the OS by default. A toggle in the page header lets the user override. Stored in a cookie, not a database — the approval page has no user settings table. CSS custom properties make this trivial: one set of variables, two value sets.
- **Accessibility** — the approval page is the template for accessible UI across all of StageFreight's web surfaces:
  - **Screen reader support** — semantic HTML, ARIA labels on all interactive elements, logical heading hierarchy, live regions for status updates (approval pending → approved). A blind user navigating the approval page with a screen reader gets the same information flow as a sighted user: org name → action summary → sign in → details → approve/deny.
  - **Keyboard navigation** — full tab order through all interactive elements. Approve/Deny buttons are focusable and operable via Enter/Space. No mouse-only interactions.
  - **Visual alternatives for audio/timing cues** — no audio-only signals. The polling status ("waiting for approval...") uses a visible spinner with text, not a sound. Timeout warnings are displayed as text with sufficient contrast, not flashing or color-only indicators.
  - **High contrast** — color is never the sole differentiator. Approve (green) and Deny (red) buttons also have distinct text labels, icons, and ARIA descriptions. Works for colorblind users without a special mode.
  - **Reduced motion** — respects `prefers-reduced-motion`. Spinners and transitions degrade gracefully to static indicators.

These aren't afterthought checkboxes — they're structural decisions baked into the HTML/CSS/JS templates from the first commit. The approval page's templates become the foundation that every future `stagefreight serve` page inherits. Accessibility, i18n, and theming are free for every new page because they're in the base template, not bolted on per-page.

**Dogfooding — StageFreight translates itself with its own system:**

The approval page UI strings, the CLI `--help` text, the man pages, the README, the credits page — all of StageFreight's own user-facing text is managed through its own docs module + translation system. StageFreight is the first user of its own i18n pipeline:

1. English strings live in `docs/modules/` as structured YAML.
2. `stagefreight docs translate` runs LibreTranslate against them, producing `docs/translations/{lang}/` with per-item `source: auto` metadata and inline disclaimers.
3. Contributors PR better translations for specific items. Those items flip to `source: human`, the contributor gets credited in the module metadata, the release notes, and the credits module.
4. `go generate` bakes translations into the binary for `--help`. `docs render` produces translated man pages and docs. The web UI reads translations at runtime from the same module files.

This means every user of StageFreight gets the same system for their own projects. The translation workflow, the per-item provenance tracking, the credits module, the LibreTranslate integration, the inline disclaimers — all of it works identically whether you're translating StageFreight's own approval page or your project's README. StageFreight proves the system works by using it on itself, and ships the proof as a feature.

#### Structured Logging

Every CLI tool starts with `fmt.Printf` and regrets it six months later when someone needs to parse pipeline output, correlate a build failure with a specific step, or figure out what happened at 3am from a daemon log. StageFreight uses structured logging from the start — not as a library choice, but as an architectural decision that affects every subcommand.

**Two output modes, same log calls:**

| Mode | When | Format | Example |
|---|---|---|---|
| **Human** | Terminal (TTY detected), or `--format text` | Colored, concise, aligned | `  lint ✓ (3 files, 0.2s)` |
| **Machine** | Pipeline (no TTY), daemon mode, or `--format json` | JSON lines, one object per event | `{"level":"info","msg":"lint passed","files":3,"duration":"0.2s","cached":247}` |

Auto-detection: if stdout is a TTY, human mode. If not (piped, CI runner, daemon), JSON mode. `--format json` or `--format text` overrides detection. `STAGEFREIGHT_LOG_FORMAT=json` env var works too.

**Log levels:**

| Level | What it means | Example |
|---|---|---|
| `error` | Something failed, the operation cannot continue | `build failed: Dockerfile syntax error at line 12` |
| `warn` | Something unexpected but non-fatal, the operation continues | `cache miss for layer 3/7, rebuilding` |
| `info` | Normal operation milestones — the default level | `lint passed`, `image pushed to docker.io/prplanit/app:1.2.3` |
| `debug` | Internal details useful for troubleshooting | `resolved tag template "{version}" → "1.2.3"`, `buildx args: [--platform linux/amd64 ...]` |
| `trace` | Extremely verbose, for development only | `HTTP POST https://registry/v2/... → 201 (43ms)` |

Default level: `info`. Set via `--verbose` (enables `debug`), `--quiet` (only `error` + `warn`), or `STAGEFREIGHT_LOG_LEVEL=debug` env var. The daemon config has its own `log.level` field.

**Context propagation — every log line knows where it came from:**

Every log event carries structured context fields that identify the operation, not just the message:

```json
{
  "level": "info",
  "ts": "2026-02-22T14:32:15.123Z",
  "msg": "image pushed",
  "component": "build.image",
  "step": "push",
  "repo": "sofmeright/stagefreight",
  "branch": "main",
  "commit": "abc1234",
  "registry": "docker.io",
  "tag": "1.2.3",
  "duration": "4.1s",
  "request_id": "req-7f3a2b"
}
```

Context fields are inherited through the call chain. The build engine sets `component: "build.image"` once, and every log call within that engine includes it automatically. The daemon adds `request_id` for correlating logs across a multi-step job. The CLI adds `repo` and `branch` from detection. No manual threading of context — the logger carries it.

**Manifest and env config:**

```yaml
log:
  level: info                     # error | warn | info | debug | trace
  format: auto                    # auto | json | text
```

```bash
# Environment overrides (useful in CI)
STAGEFREIGHT_LOG_LEVEL=debug      # override log level
STAGEFREIGHT_LOG_FORMAT=json      # override format
```

**Why this matters for the approval system and daemon:**

The approval system logs structured audit events (who approved what, when). The daemon logs structured operational events (scan started, MR created, mirror pushed). Both need correlation IDs, timestamps, and context fields that machines can parse. Starting with structured logging means the audit log, the daemon log, and the CLI output all use the same format — a `jq` query that works on one works on all three.

#### Pipeline-First Philosophy

StageFreight works in two modes: **binary tool** (developer runs `stagefreight` on their laptop) and **pipeline** (CI/CD runs `stagefreight` in a controlled environment). Both execute the same manifest. Both produce the same output. But the pipeline is the golden path — and the experience should be so smooth that developers naturally prefer pushing a commit over building locally.

**Why pipelines win:**

| | Local Dev | Pipeline |
|---|---|---|
| **Secrets** | Developer needs registry credentials, API tokens, signing keys on their machine. Every laptop is an attack surface. | Secrets live in CI variables, never touch a developer's filesystem. Rotated centrally, audited, scoped per-branch. |
| **Environment** | "Works on my machine" — different Docker versions, OS quirks, stale caches, disk pressure, VPN issues. | Identical runner image every time. Clean environment, known state, no accumulated cruft. |
| **Multi-arch** | QEMU emulation is slow and painful. Most devs skip arm64 builds locally and hope CI catches it. | Runners can be native per-arch, or QEMU on dedicated build nodes. Cross-compilation happens once, correctly, without developer pain. |
| **Cache** | Local Docker cache is per-machine, diverges across team members, hard to share. | Pipeline cache is shared across runs. StageFreight's semantic cache invalidation works best with a persistent, centralized cache store. |
| **Security** | Lint and security scans run with the developer's permissions. A compromised dependency can access anything the developer can. | Sandboxed runner. Network-restricted. No access to production systems. Scan results are artifacts, not local files. |
| **Audit trail** | Nothing. No record of what was built, when, by whom, from what commit. | Every build is a pipeline run with commit SHA, manifest version, lint results, scan output, SBOM, and push receipt. Auditors love this. |

**The developer experience should make this feel obvious:**

```
$ git push origin feature/new-auth
  → Pipeline starts automatically
  → lint ✓ (0.3s, cached)
  → build ✓ linux/amd64,arm64 (12s, layer cache hit)
  → test ✓ healthchecks + 4 sanity checks (8s)
  → scan ✓ 0 critical, 2 low
  → push ✓ registry.prplanit.com/apps/myapp:feature-new-auth-abc1234
  → MR comment: "Build passed. Image ready at registry.prplanit.com/apps/myapp:feature-new-auth-abc1234"
```

A developer pushes, gets coffee, comes back to a green pipeline with a tested, scanned, tagged image waiting in the registry. No local Docker daemon needed. No credentials on their laptop. No QEMU. The pipeline did everything faster and more securely than they could have locally.

**Local dev still works and still matters.** You need it for interactive debugging, stepping through code, testing UI changes in real time. `stagefreight dev build` and `stagefreight dev up` exist specifically for this. But the feedback loop for "does my code pass all checks and produce a correct image" should be: push → pipeline → done. Local builds are for iteration. Pipelines are for validation.

**How StageFreight makes the pipeline feel effortless:**
- **Zero CI config authoring** — the GitLab CI component (or GitHub Actions wrapper) reads the `.stagefreight.yml` manifest. The pipeline definition is one line: `include: stagefreight`. No writing CI YAML by hand.
- **Same command everywhere** — `stagefreight docker build` in a pipeline is the same command a dev runs locally. No "CI does something different." If it passes locally, it passes in CI. If it fails in CI, you can reproduce locally with the same command.
- **Pipeline-aware caching** — StageFreight persists its lint cache, buildx cache, and scan results as CI artifacts. Subsequent pipeline runs on the same branch pick up where the last one left off. Feature branch builds benefit from main branch cache.
- **Branch-specific registries** — feature branches push to the dev registry with `{branch}-{sha:.7}` tags. Main branch pushes to all production registries. Developers see their feature branch image in the registry within minutes of pushing, without polluting production tags.
- **Inline MR feedback** — scan results, lint failures, and test output appear as MR comments. The developer never leaves the MR page to understand what happened.

The pipeline should feel like an extension of the developer's editor — push, and the rest happens. Local dev is the escape hatch for when you need hands-on iteration. The pipeline is where real builds happen.

**Manifest-driven pipelines — the manifest IS the config, not CLI flags:**

The whole point of `.stagefreight.yml` is that CI pipelines shouldn't need CLI flags. The manifest declares what the project needs — platforms, registries, tags, lint rules, test definitions, approval gates. CI jobs call bare subcommands. Environment variables provide the few things that differ between environments (secrets, runner-specific paths). Nothing else.

```yaml
# .gitlab-ci.yml — the entire pipeline
include: stagefreight

# That's it. The component reads .stagefreight.yml and generates stages.
# But if you want explicit control, the manual version is still trivial:

variables:
  # Global env — shared across all stages, safe for every phase to see
  STAGEFREIGHT_LOG_FORMAT: json
  STAGEFREIGHT_LOG_LEVEL: info

lint:
  stage: lint
  script: stagefreight lint
  # No --level, no --modules, no --exclude. The manifest has all of that.

build:
  stage: build
  variables:
    # Per-stage env — only this job sees these
    DOCKER_HOST: tcp://docker:2375
  script: stagefreight docker build
  # No --platform, no --tag, no --registry, no --target.
  # The manifest declares platforms, registries, tag templates.
  # Git detection resolves the version. Done.

test:
  stage: test
  script: stagefreight test
  # No --context, no --tag, no --scope. The manifest defines test cases,
  # scopes, and contexts. Delta detection skips unchanged tests.

push:
  stage: push
  variables:
    # Secrets scoped to the push stage only — not visible to lint/build/test
    REGISTRY_TOKEN: $CI_REGISTRY_TOKEN
  script: stagefreight docker push
  # No --registry, no --tag. Same registries and tags from the manifest.
  # Approval gates fire if configured. Credentials come from env.
```

**The layering model — how config resolves:**

```
Priority (highest wins):
  1. CLI flags           (--platform linux/arm64)     ← developer override
  2. Environment vars    (STAGEFREIGHT_PLATFORMS)      ← CI per-stage injection
  3. Manifest            (.stagefreight.yml)           ← project config (source of truth)
  4. Detection           (Dockerfile, go.mod, git)     ← inferred defaults
  5. Built-in defaults   (platform: linux/$GOARCH)     ← sensible fallback
```

In practice: the manifest handles layers 3-5, which covers 95% of cases. CI sets env vars for secrets and runner-specific config (layer 2). Developers use CLI flags for one-off local overrides (layer 1). Nobody types `--platform linux/amd64,linux/arm64 --tag "{version}" --tag latest --registry docker.io/prplanit/myapp --registry ghcr.io/sofmeright/myapp` in a CI script — that's what the manifest is for.

**Environment variable conventions:**

Every manifest field has a corresponding `STAGEFREIGHT_*` env var. The mapping is mechanical:

| Manifest field | Env var | Example |
|---|---|---|
| `docker.platforms` | `STAGEFREIGHT_PLATFORMS` | `linux/amd64,linux/arm64` |
| `log.level` | `STAGEFREIGHT_LOG_LEVEL` | `debug` |
| `log.format` | `STAGEFREIGHT_LOG_FORMAT` | `json` |
| `approve.timeout` | `STAGEFREIGHT_APPROVE_TIMEOUT` | `15m` |
| `lint.level` | `STAGEFREIGHT_LINT_LEVEL` | `full` |

Nested fields use underscores: `docker.registries[0].url` → not mapped (use the manifest for complex structures). Env vars are for scalar overrides — simple values that CI needs to inject per-stage. Complex config (registry lists, test definitions, approval gates) lives in the manifest where it belongs.

**What CI actually needs to provide via env:**

Secrets and runner-specific paths — that's it. Everything else comes from the manifest:

| What | Where | Why not in the manifest |
|---|---|---|
| Registry credentials | `REGISTRY_TOKEN`, `DOCKER_CONFIG` | Secrets don't belong in git |
| Docker daemon socket | `DOCKER_HOST` | Runner-specific |
| Cache paths | `STAGEFREIGHT_CACHE_DIR` | Runner filesystem layout |
| OIDC tokens (approval) | `STAGEFREIGHT_OIDC_CLIENT_SECRET` | Secret |
| Signing keys | `COSIGN_KEY`, `COSIGN_PASSWORD` | Secret |
| Log level override | `STAGEFREIGHT_LOG_LEVEL` | Convenience, not config |

CI stages inherit global env and add stage-specific env. StageFreight reads the manifest once, merges env overrides, and runs. The CI YAML never mentions platforms, tags, registries, lint rules, or test definitions — those are the project's concern, declared in the manifest, versioned in git alongside the code.

**Deliverables:**
- Go module with subcommand structure (cobra or similar)
- Binary embedded in OCI image alongside existing tools
- Updated GitLab CI component templates calling the binary instead of scripts
- Existing bash scripts remain as fallback until all parity is confirmed

### Phase 1 — Multi-Provider

Extract the GitLab-specific logic into a provider interface and add GitHub + Gitea/Forgejo support.

```go
type Provider interface {
    Repos(ctx context.Context, org string) ([]Repo, error)
    CreateMR(ctx context.Context, repo Repo, opts MROptions) (*MR, error)
    CommitFile(ctx context.Context, repo Repo, path string, content []byte, msg string) error
    CreateRelease(ctx context.Context, repo Repo, opts ReleaseOptions) (*Release, error)
    PipelineStatus(ctx context.Context, repo Repo, ref string) (Status, error)
    // ...
}
```

| Provider | Library | Notes |
|---|---|---|
| GitLab | `go-gitlab` | Existing logic extracted from bash, first-class support |
| GitHub | `go-github` | Second provider, enables cross-platform CLI usage |
| Gitea/Forgejo | `go-sdk` | Covers self-hosted Git forges |

**Deliverables:**
- Provider interface + three implementations
- CLI auto-detects provider from git remote or `--provider` flag
- GitHub Actions wrapper (equivalent of the GitLab CI component)

### Phase 2 — Cross-Provider Sync & Documentation Engine

New subcommands for keeping repo metadata consistent across providers and registries, plus a full documentation-as-data engine.

| Subcommand | Description |
|---|---|
| `stagefreight sync readme` | Push README to DockerHub, GHCR, Quay descriptions |
| `stagefreight sync metadata` | Mirror description, topics, homepage URLs across providers |
| `stagefreight lint links` | Crawl README/docs for broken links, report or auto-fix |
| `stagefreight flair` | Auto-manage shields.io badges in README |
| `stagefreight docs render` | Render documentation modules into markdown files (see below) |
| `stagefreight docs translate` | Generate best-effort translations of documentation modules |

Nothing good exists for cross-provider doc sync today. DockerHub readme sync has a few janky GitHub Actions but nothing unified. This is greenfield. The documentation engine is entirely novel — nobody treats repo docs as structured, reusable, translatable data.

#### `stagefreight docs` — Documentation Engine

Documentation in most repos is write-once, forget-forever markdown. Badges go stale. Examples drift from reality. The same explanation is copy-pasted across five files and three of them are outdated. Translations don't exist or were done once and never updated.

StageFreight treats documentation as **structured content**, not files you type into by hand. You maintain documentation as atomic, reusable modules in source files. StageFreight renders those modules into markdown wherever they're referenced — across multiple files, in multiple languages, updated every CI run.

**The model: documentation modules as data.**

Instead of writing markdown directly, you define documentation content in structured source files under a `docs/` directory (or wherever you want). Think of these like Kubernetes resource manifests — each file has a specific purpose and contains structured content that gets rendered into output.

```
docs/
  modules/
    badges.yml              # badge definitions (release, CI, social, compliance)
    installation.yml        # installation instructions (by platform, by method)
    configuration.yml       # config reference (env vars, CLI flags, file options)
    api-endpoints.yml       # API reference (routes, params, responses)
    faq.yml                 # FAQ entries
    examples.yml            # code examples (reused across README, docs, comments)
    changelog-entry.yml     # structured changelog data
    commands/               # CLI command documentation (→ --help, man pages, docs)
      root.yml              # stagefreight (top-level)
      lint.yml              # stagefreight lint
      docker-build.yml      # stagefreight docker build
      build.yml             # stagefreight build
      version.yml           # stagefreight version
      ...                   # one per command/subcommand
  translations/
    es/                     # Spanish translations of modules
      installation.yml
      faq.yml
      commands/             # translated CLI help text
        docker-build.yml
    fr/                     # French
      installation.yml
      commands/
        docker-build.yml
  templates/
    README.md.tpl           # template for root README
    docs/INSTALL.md.tpl     # template for install guide
    docs/API.md.tpl         # template for API reference
    man/                    # man page templates (troff format)
      stagefreight.1.tpl
      stagefreight-docker-build.1.tpl
      stagefreight-lint.1.tpl
      ...                   # one per command
```

**Module file format:**

```yaml
# docs/modules/badges.yml
kind: badges
entries:
  releases:
    - label: Release Version
      shield: github/v/release/{repo}
      link: "{repo_url}/releases/latest"
    - label: Artifact Hub
      shield: endpoint
      url: https://artifacthub.io/badge/repository/{name}
      link: "https://artifacthub.io/packages/helm/{org}/{name}"
    - label: SLSA 3
      image: https://slsa.dev/images/gh-badge-level3.svg
      link: https://slsa.dev

  code:
    - label: Integration tests
      shield: github/actions/workflow/status/{repo}/ci.yml?branch=main
      link: "{repo_url}/actions?query=workflow%3ACI"
    - label: codecov
      shield: codecov/c/gh/{repo}/main
      link: "https://codecov.io/gh/{repo}"
    - label: OpenSSF Scorecard
      shield: ossf-scorecard/{repo}
      link: "https://scorecard.dev/viewer/?uri=github.com/{repo}"

  social:
    - label: Slack
      shield: badge/slack-{name}-brightgreen.svg?logo=slack
      link: "{slack_invite_url}"
    - label: Bluesky
      shield: badge/Bluesky-{name}-blue.svg?style=social&logo=bluesky
      link: "https://bsky.app/profile/{bluesky_handle}"
```

```yaml
# docs/modules/installation.yml
kind: content
id: installation
title: Installation
sections:
  - id: docker
    title: Docker
    body: |
      ```bash
      docker pull {image}:{version}
      docker run -d -p 8080:8080 {image}:{version}
      ```
  - id: helm
    title: Helm
    body: |
      ```bash
      helm repo add {name} {helm_repo_url}
      helm install {name} {name}/{chart_name}
      ```
  - id: binary
    title: Binary
    body: |
      Download the latest release from the [releases page]({repo_url}/releases/latest).
      ```bash
      curl -L {repo_url}/releases/download/v{version}/{name}-$(uname -s)-$(uname -m) -o {name}
      chmod +x {name}
      sudo mv {name} /usr/local/bin/
      ```
```

```yaml
# docs/modules/examples.yml
kind: content
id: examples
title: Examples
sections:
  - id: quickstart
    title: Quick Start
    body: |
      ```yaml
      # Minimal configuration
      version: 1
      docker:
        registries:
          - url: docker.io
            path: myorg/myapp
      ```
    # This same block can be referenced in README.md, docs/GETTING_STARTED.md,
    # and even injected as a comment in .stagefreight.yml itself
```

**Template files with marker blocks:**

Templates reference modules by ID. StageFreight renders the module content between marker comments, preserving everything outside the markers.

```markdown
<!-- file: README.md.tpl -->
# My Project

One-liner description from the manifest.

<!-- stagefreight:module:badges group=releases -->
<!-- /stagefreight:module:badges -->

<!-- stagefreight:module:badges group=code -->
<!-- /stagefreight:module:badges -->

<!-- stagefreight:module:badges group=social -->
<!-- /stagefreight:module:badges -->

## Overview

Hand-written content here. StageFreight never touches text outside markers.

## Installation

<!-- stagefreight:module:installation -->
<!-- /stagefreight:module:installation -->

## Quick Start

<!-- stagefreight:module:examples section=quickstart -->
<!-- /stagefreight:module:examples -->

## Configuration

<!-- stagefreight:module:configuration -->
<!-- /stagefreight:module:configuration -->

## FAQ

<!-- stagefreight:module:faq -->
<!-- /stagefreight:module:faq -->

---
<!-- stagefreight:translations -->
<!-- /stagefreight:translations -->
```

Between markers, StageFreight owns the content. Outside markers, you own it. This means you can adopt incrementally — drop one marker pair into an existing README for badges, and the rest of the file stays untouched.

**Variables:**

Modules reference variables with `{variable}` syntax. Variables resolve from multiple sources in priority order:

1. Module-level `vars:` overrides
2. `docs.vars` in the manifest
3. Auto-detected from the repo (repo URL, name, org, version, default branch, license, etc.)

```yaml
# in .stagefreight.yml
docs:
  vars:
    slack_invite_url: https://myproject.slack.com/invite
    bluesky_handle: myproject.bsky.social
    helm_repo_url: https://charts.myproject.io
    chart_name: myproject
```

**Multi-file rendering:**

A single module can appear in multiple output files. Change the source module, every consumer updates on the next `stagefreight docs render` run (which runs in CI, delivered via MR or auto-merged if YOLO).

```yaml
# in .stagefreight.yml
docs:
  render:
    - template: README.md.tpl
      output: README.md
    - template: docs/INSTALL.md.tpl
      output: docs/INSTALL.md
    - template: docs/API.md.tpl
      output: docs/API.md
```

The quickstart example defined once in `examples.yml` can be referenced in `README.md`, `docs/GETTING_STARTED.md`, and `CONTRIBUTING.md`. Update it once, all three files update.

**Translations:**

```yaml
docs:
  translations:
    languages: [es, fr, de, ja]
    strategy: best-effort              # best-effort | manual-only
    output: docs/i18n/{lang}/          # where translated files land
```

- `best-effort`: StageFreight generates initial translations using **LibreTranslate** (self-hosted, no external API dependency, no data leaves your infrastructure). LibreTranslate handles prose and UI strings; technical terms (command names, flags, code identifiers) are preserved verbatim via a configurable glossary. Translation provenance is tracked **per-item** — each translated string carries metadata about its origin:
  - **Auto-translated items** render with an inline disclaimer **in the target language** appended to the end of the string — e.g. `"Construir imágenes de contenedor (*Traducido por IA*)"`, `"コンテナイメージをビルド (*AI翻訳*)"`. Small, inline, unambiguous — the reader knows which text is machine-generated.
  - **Human-contributed items** simply lose the disclaimer in the UI — the rendered string is clean with no tag. The contributor's identity lives in the translation module metadata only (not in the rendered output), keeping the UI decluttered. Attribution is discoverable by looking at the source YAML or any doc page that embeds the `credits` module (see below).
  - **Attribution flow** — when a contributor PRs a better translation for a specific item:
    1. The item's module metadata records `source: human` and `contributor: "@maria"` (or name, handle, link — whatever the contributor provides).
    2. The commit that lands the translation is auto-tagged for release notes — `stagefreight release notes` picks up translation contributions and credits contributors by name in the "Translations" section of the changelog.
    3. The contributor appears in the project's `credits` module (see **Credits module** below) under the translators section, alongside dependency authors, maintainers, and anyone else the project wants to recognize.
  - **Mixed pages are normal** — some items AI-translated, others human-provided. Each item stands on its own. A contributor who thinks one translation is poor can PR just that entry.
  - When the source English module changes, affected items reset to `source: auto` and re-translate — the disclaimer reappears until a human re-reviews.

  **Credits module — unified attribution for the entire project:**

  The credits module (`docs/modules/credits.yml`) is a general-purpose attribution registry. It's not just for translators — it's the single place where a project acknowledges everyone and everything that contributed to it:

  ```yaml
  kind: credits
  id: project-credits
  sections:
    - name: Maintainers
      entries:
        - name: SoFMeRight
          link: https://github.com/sofmeright
          role: Lead developer

    - name: Translators
      # Auto-populated from translation module metadata.
      # Human-contributed translations automatically add entries here.
      # Manual entries can be added too.
      auto_from: translations    # pulls contributor info from all translation modules
      entries:
        - name: María García
          link: https://github.com/maria
          languages: [es]
          items: 47               # auto-counted from translation modules

    - name: Dependencies
      # Auto-populated from go.mod / package.json / Cargo.toml.
      # Each dependency's author/maintainer is resolved from the package registry.
      auto_from: lockfiles
      entries: []                 # auto-populated, or manually curated

    - name: Special Thanks
      entries:
        - name: LibreTranslate
          link: https://libretranslate.com
          role: Machine translation engine
  ```

  The credits module renders wherever it's referenced — a `CREDITS.md` page, a footer section in the README, an "About" page in the web UI, or a `<!-- stagefreight:module:project-credits -->` marker anywhere. Projects configure what to show and where. `auto_from: translations` and `auto_from: lockfiles` mean the credits list stays current without manual maintenance — add a dependency or accept a translation PR, the credits update on the next render.

  Translations are linked from the source page. A `<!-- stagefreight:translations -->` block in the English page renders as:
  > Available in: [Español](docs/i18n/es/README.md) · [Français](docs/i18n/fr/README.md) · [Deutsch](docs/i18n/de/README.md) · [日本語](docs/i18n/ja/README.md)
- `manual-only`: only renders translation links for languages that have manually-provided module translations in `docs/translations/{lang}/`. No auto-generation.
- Translation modules mirror the English module structure. If `docs/translations/es/installation.yml` exists, it overrides `docs/modules/installation.yml` for Spanish output. Missing translations fall back to English with a notice.
- Translations regenerate when the source module changes — the MR shows the English diff and flags the translations as potentially stale.

**Microdocumentation — atomic content that weaves everywhere:**

The real power is that modules are atomic. A single `section` from a module is the smallest addressable unit. You can reference it anywhere:

- In a README badge block: `<!-- stagefreight:module:badges group=releases -->`
- In a specific doc page: `<!-- stagefreight:module:installation section=docker -->`
- As an inline code comment (experimental): StageFreight can inject a module section as a comment block in source files at a marked location
- In a changelog: structured changelog entries from `changelog-entry.yml` render into `CHANGELOG.md`

This means documentation is no longer copy-paste — it's defined once and woven into every place it's needed. A developer updates an installation step in `installation.yml`, and it propagates to the README, the install guide, and the contributor docs on the next CI run.

**CLI Help Text, Man Pages, and Translations — from the same modules.**

A CLI tool's help text is documentation. It appears in `--help` output, man pages, README usage sections, install guides, and tutorials. Today people copy-paste it across all of these and three of the five copies go stale after the next flag change. StageFreight solves this by making `--help` text and man pages just two more render targets from the same documentation modules — no new concepts, no new formats, no runtime overhead.

**The `command` module kind:**

Each CLI command gets a documentation module. The module defines the command's short description, long description, examples, and flag documentation — the same content that Cobra needs for `--help` and that man pages need for troff rendering.

```yaml
# docs/modules/commands/docker-build.yml
kind: command
id: docker-build
use: "build"
parent: docker
short: "Build and push container images"
long: |
  Build container images using docker buildx.

  Detects Dockerfiles, resolves tags from git, and pushes to configured
  registries. Runs lint as a pre-build gate unless --skip-lint is set.

  When no registries are configured, defaults to building for the current
  platform and loading into the local Docker daemon.
examples:
  - description: "Build and load locally"
    command: "stagefreight docker build --local"
  - description: "Build for multiple platforms"
    command: "stagefreight docker build --platform linux/amd64,linux/arm64"
  - description: "Preview the build plan"
    command: "stagefreight docker build --dry-run"
  - description: "Build without pre-build lint"
    command: "stagefreight docker build --skip-lint"
flags:
  - name: local
    type: bool
    default: "false"
    description: "Build for current platform, load into daemon"
  - name: platform
    type: strings
    description: "Override platforms (comma-separated)"
  - name: tag
    type: strings
    description: "Override/add tags"
  - name: target
    type: string
    description: "Override Dockerfile target stage"
  - name: skip-lint
    type: bool
    default: "false"
    description: "Skip pre-build lint"
  - name: dry-run
    type: bool
    default: "false"
    description: "Show the plan without executing"
```

This is the single source of truth. Every output — `--help`, man page, README, translated docs — reads from this one file.

**Build-time codegen for `--help` text:**

A small generator reads the command YAML modules and emits a Go source file with const strings that Cobra consumes. No runtime YAML parsing, no embedded files, no new dependencies — the help text is baked into the binary at compile time, same as ldflags for version.

```
docs/modules/commands/*.yml
        │
        │  go generate ./src/cli/cmd/...
        ▼
src/cli/cmd/help_gen.go          (generated, gitignored or committed — project's choice)
        │
        │  var dockerBuildShort = "Build and push container images"
        │  var dockerBuildLong  = "Build container images using..."
        │  var dockerBuildExample = "  # Build and load locally\n  stagefreight docker build --local\n..."
        │
        │  var helpStrings = map[string]map[string]CommandHelp{
        │      "en": { "docker-build": { Short: dockerBuildShort, Long: dockerBuildLong, ... } },
        │      "es": { "docker-build": { Short: "...", Long: "...", ... } },
        │  }
        ▼
cobra commands reference the generated strings
```

The generator directive lives in one file:

```go
// src/cli/cmd/generate.go
package cmd

//go:generate go run ../../../tools/helpgen/main.go -modules ../../../docs/modules/commands -translations ../../../docs/translations -out help_gen.go
```

The generator (`tools/helpgen/main.go`) is a ~100 line Go program: read YAML, write Go const strings, group by locale. It's part of the StageFreight repo and runs as a standard `go generate` step.

Each cobra command references the generated strings instead of inline literals:

```go
// Before (inline, drifts from docs):
var dockerBuildCmd = &cobra.Command{
    Use:   "build",
    Short: "Build and push container images",
    Long:  "Build container images using docker buildx...",
}

// After (generated from docs module):
var dockerBuildCmd = &cobra.Command{
    Use:     helpFor("docker-build").Use,
    Short:   helpFor("docker-build").Short,
    Long:    helpFor("docker-build").Long,
    Example: helpFor("docker-build").Example,
}
```

`helpFor()` is a one-line lookup into the generated map, selecting the locale from `$LANG`/`$LC_ALL` with fallback to English. The binary ships with all translations baked in — `LANG=es stagefreight docker build --help` shows Spanish help text with zero runtime overhead.

**Man page generation:**

Man pages are another render target. A template in `docs/templates/man/` references the same command modules:

```
docs/templates/man/stagefreight-docker-build.1.tpl
```

```troff
.TH STAGEFREIGHT-DOCKER-BUILD 1 "{build_date}" "stagefreight {version}" "StageFreight Manual"
.SH NAME
stagefreight-docker-build \- <!-- stagefreight:command:docker-build field=short -->
.SH SYNOPSIS
.B stagefreight docker build
[\fIOPTIONS\fR]
.SH DESCRIPTION
<!-- stagefreight:command:docker-build field=long -->
.SH OPTIONS
<!-- stagefreight:command:docker-build field=flags format=troff -->
.SH EXAMPLES
<!-- stagefreight:command:docker-build field=examples format=troff -->
```

`stagefreight docs render` produces the `.1` files from these templates. The same command module feeds the man page, the `--help` output, and the README — change the YAML, all three update.

Man pages install via the standard mechanism: the Makefile (or `stagefreight build`) copies rendered `.1` files to `dist/man/man1/`. Package managers (`brew`, `deb`, `rpm`) pick them up from there. The Docker image includes them at `/usr/share/man/man1/`.

**Translations — same system, no extra work:**

Translated command modules live in the existing translation directory:

```
docs/translations/es/commands/docker-build.yml
docs/translations/fr/commands/docker-build.yml
```

Each is a YAML file with the same structure as the English module — `short`, `long`, `examples`, `flags.description` — translated. The generator picks these up automatically and includes them in the generated Go source. Missing translations fall back to English.

Translated man pages work the same way — `docs/translations/es/templates/man/` contains locale-specific `.tpl` files if someone provides them, otherwise the English template is used with translated module content injected.

**The complete render pipeline:**

```
docs/modules/commands/*.yml                    (single source of truth)
docs/translations/{lang}/commands/*.yml        (translated overrides)
        │
        ├── go generate           →  src/cli/cmd/help_gen.go    (--help text, all locales)
        │
        ├── stagefreight docs render
        │   ├── README.md         →  <!-- stagefreight:command:docker-build field=short -->
        │   ├── docs/CLI.md       →  full command reference
        │   ├── man/stagefreight-docker-build.1  →  man page
        │   ├── docs/i18n/es/CLI.md              →  translated reference
        │   └── man/es/stagefreight-docker-build.1  →  translated man page
        │
        └── CI pre-build step     →  go generate runs before go build
                                      docs render runs after build (or in MR)
```

One YAML file per command. Six output targets. Zero copy-paste. Add a flag, update the YAML, every consumer updates automatically. Translate the YAML, every locale updates. The developer never touches help strings in Go source, never hand-writes a man page, never manually syncs the README's command reference.

**Manifest config:**

```yaml
docs:
  modules_dir: docs/modules          # where module source files live
  templates_dir: docs/templates      # where .tpl template files live (optional — can use markers in existing files)
  render:
    - template: README.md.tpl        # template input → output mapping
      output: README.md
    - template: docs/INSTALL.md.tpl
      output: docs/INSTALL.md
    # or: render markers in-place without a separate template file
    - file: CONTRIBUTING.md           # existing file with markers, rendered in-place
  vars:
    project_name: My Project
    slack_invite_url: https://...
  translations:
    languages: [es, fr]
    strategy: best-effort
    output: docs/i18n/{lang}/
  readme_sync: [dockerhub, ghcr]     # push rendered README to registry descriptions
  code_links: []                      # low priority — code-to-doc hyperlinking
```

**How it runs:**
- `stagefreight docs render` — render all templates and in-place marker files, output to disk (markdown, man pages, Go help source)
- `stagefreight docs render --check` — dry run, exit non-zero if output would change (CI gate)
- `stagefreight docs render --target help` — render only the Go help source (used by `go generate`)
- `stagefreight docs render --target man` — render only man pages
- `stagefreight docs translate` — regenerate translations for changed modules
- In pipeline: `go generate` runs before `go build` to bake help text into the binary; `docs render` runs after build for markdown and man pages, creates MR with doc changes (or auto-merges if YOLO)
- In daemon mode: runs on schedule or when source modules change

### Phase 3 — Fork Maintenance

**Unique differentiator — nothing like this exists.**

Keeping a patched fork current with upstream while preserving your changes and scanning for safety. People do this manually and it sucks.

| Subcommand | Description |
|---|---|
| `stagefreight fork sync` | Rebase or merge upstream changes, preserve local patches |
| `stagefreight fork status` | Divergence report — commits ahead/behind, conflict detection |
| `stagefreight fork audit` | Scan upstream changes for suspicious commits (new maintainers, obfuscation, unexpected binary additions, invisible characters, AI prompt injection payloads) |

**Design considerations:**
- `strategy: rebase` vs `strategy: merge` per-repo config
- Conflict detection creates an MR with conflict markers for human resolution
- Audit heuristics flag but never block — advisory only
- `fork audit` integrates with `audit codebase` — every upstream sync runs invisible character detection on the incoming diff, not just the full codebase. Catches Trojan Source and prompt injection payloads injected by compromised upstream maintainers.

### Phase 4 — Supply Chain & Security

Extends the existing `security scan` subcommand with pre-build dependency auditing, dependency provenance, and SBOM tracking.

| Subcommand | Description |
|---|---|
| `stagefreight deps pull` | Fetch all dependencies fresh from source, build from source, run all enabled checks. Highest trust, slowest. The default when no deps mode is configured. |
| `stagefreight deps verify` | Build from source once, produce a verified signature. On subsequent builds, trust downloads matching the signature — skip rebuilding. Falls back to full pull if signature doesn't match. |
| `stagefreight deps cache` | Store verified/built deps in a content-addressed cache (local, restic, S3). Fastest — if a dep is cached and matches its verified signature, use it directly. |
| `stagefreight docker verify` | Post-push and periodic cross-registry digest verification — confirms all endpoints serve what was built (see below) |
| `stagefreight audit deps` | Pre-build dependency auditing — scan lockfiles for known-bad versions before building |
| `stagefreight audit packages` | Cross-reference dependencies against known package-level attacks (typosquatting, maintainer takeovers) |
| `stagefreight audit codebase` | Scan source files for invisible/malicious character sequences (see below) |
| `stagefreight sbom diff` | Track SBOM changes between releases — what was added, removed, or changed |

#### `stagefreight deps` — Dependency Provenance & Secure Sourcing

Dependencies are the largest attack surface in modern software. Package registries serve pre-built binaries that you trust implicitly — you run `go mod download` or `npm install` and hope what you got matches what the source code would produce. StageFreight's `deps` subsystem makes that trust explicit, verifiable, and cached.

**Core philosophy: all checks are always on.** Every dependency that passes through StageFreight gets the full treatment by default. Individual checks can be opted out of — but the default is maximum scrutiny. Developers should never have to remember to enable security checks. They should have to actively decide to disable one.

**Three sourcing modes — how deps are obtained, not what checks run:**

All three modes run the same checks. The modes control the trust/speed tradeoff for *where the bytes come from*:

| Mode | Command | Trust Model | Speed | Storage |
|---|---|---|---|---|
| **Pull** | `deps pull` | Zero trust. Always fetch fresh from upstream, always build from source, always run every check. Nothing is reused. | Slowest | None — artifacts are ephemeral |
| **Verify** | `deps verify` | Trust-but-verify. First run builds from source and records a verified signature (content hash of the built artifact). Subsequent runs download from upstream and compare against the signature — if it matches, skip the rebuild. If it doesn't match, fall back to full pull and alert. | Medium | Signatures only (~KB per dep) |
| **Cache** | `deps cache` | Verified + stored. Same as verify, but built artifacts are stored in a content-addressed cache (local dir, restic repo, or S3 bucket). If a dep is in cache and its signature is valid, use it directly — no download, no build. | Fastest | Full artifacts (deduped, encrypted if restic/S3) |

**Checks (always on, individually opt-out):**

Every dependency — whether pulled fresh, verified, or cached — passes through these checks. Each check can be disabled per-project in the manifest. Checks run as part of the dependency resolution phase, before the project build starts.

| Check | Default | What it does |
|---|---|---|
| **License compliance** | on | Scans dependency licenses against project policy. Flags GPL in MIT projects, AGPL in proprietary, unknown licenses. Configurable policy levels: `permissive`, `copyleft-ok`, `strict`. |
| **Maintainer history** | on | Tracks maintainer changes between versions. Flags ownership transfers, new maintainers on dormant packages, rapid succession of maintainers (takeover pattern). |
| **Typosquatting** | on | Compares dependency names against known-good packages using edit distance, phonetic similarity, and registry-specific heuristics. Flags `lodasg` → `lodash`, `requets` → `requests`. |
| **Known-bad versions** | on | Cross-references against CVE databases, GitHub advisories, and curated blocklists. Flags dependencies with known vulnerabilities, malicious versions, or yanked releases. |
| **SBOM diff** | on | Compares the current dependency tree against the last known-good state. Flags added, removed, or changed dependencies with a clear diff. Surfaces transitive dependency changes that direct lockfile inspection misses. |
| **Source reproducibility** | on | Builds the dependency from source and compares the output against the pre-built artifact from the registry. Detects when upstream ships something their source code doesn't produce — the canonical supply chain attack. |

**Pre-build dependency scrubbing:**

The `deps` subsystem runs as a pre-build phase, before `docker build` or `build` starts. It operates like this:

1. **Scan** — walk the project tree for dependency manifests (`go.mod`, `package.json`, `Cargo.toml`, `requirements.txt`, `*.csproj`, etc.). Parse each to extract the full dependency tree including transitive deps.
2. **Resolve** — for each dependency in the tree, resolve it through the configured mode (pull/verify/cache). This is recursive — if a dependency has its own dependencies, those go through the same pipeline. Yes, this means building deps-of-deps-of-deps. The cache mode is what makes this tractable at scale — build once, cache forever.
3. **Check** — run all enabled checks against every resolved dependency. Checks run in parallel per-dependency. Failures are collected and reported together.
4. **Scrub** — replace dependency references in the build context with aliases pointing to the verified/cached copies. The actual project build (Dockerfile, `go build`, `npm run build`, etc.) consumes dependencies from a controlled local mirror rather than fetching from public registries. The build process never touches the network for dependencies — everything was pre-resolved.
5. **Build** — hand off to the build engine (`docker build` / `build`). The build environment sees only verified, checked, pre-resolved dependencies.

If any check fails at step 3, the build does not start. The developer sees exactly which dependency, which check, and why:

```
$ stagefreight docker build
  deps ✗ 2 issues
    lodash@4.17.21  [source-reproducibility] registry binary does not match source build (sha256 mismatch)
    event-stream@3.3.6  [known-bad] flagged: malicious flatmap-stream dependency (CVE-2018-16492)
  build skipped — dependency check failed
```

When everything passes:

```
$ stagefreight docker build
  deps ✓ (47 deps checked, 41 cached, 6 verified, 0 pulled, 1.2s)
  lint ✓ (3 files changed, 247 cached, 0.2s)
  build ✓ linux/amd64 (cached layers, 4.1s)
  push ✓ docker.io/prplanit/myapp:1.2.3
```

**Recursive dependency building (the inception problem):**

Real dependency trees are deep. A Go module might pull in 200 transitive dependencies. An npm project might pull in 2000. Building every one from source on every run is impractical — that's why the three modes exist as an escalation ladder:

- **First run / CI release builds:** `pull` mode. Build everything from source. Slow but you establish the full verified baseline.
- **Regular CI builds:** `verify` mode. Download from upstream, compare signatures. If upstream changed without a version bump (supply chain attack), you catch it. If signatures match, skip the build — just download and go.
- **Local dev / frequent iteration:** `cache` mode. Everything pre-built and stored. Dependency resolution is effectively a cache lookup — milliseconds per dep.

The cache is content-addressed: same source + same build config = same cache key. Deduplication happens automatically. A dep shared across 50 projects in your org is built once and cached once. The restic backend adds encryption and efficient remote storage (S3, B2, SFTP). The local backend is just a directory — point it at an NFS mount for shared team caches.

**Manifest config:**

```yaml
deps:
  # Sourcing mode (how deps are obtained)
  # pull: always fresh from source (default)
  # verify: trust signatures after initial build
  # cache: store in content-addressed cache
  mode: cache

  # Cache backend (only used when mode: cache or to store verify signatures)
  cache:
    backend: local              # local | restic | s3
    path: /home/user/.stagefreight/deps-cache  # local backend
    # repository: s3://bucket/deps-cache       # restic/s3 backend
    # password: ${DEPS_CACHE_PASSWORD}         # restic encryption password
    deduplicate: true           # content-addressed dedup (default: true)

  # Individual checks — all on by default, opt out as needed
  checks:
    license:
      enabled: true
      policy: permissive        # permissive | copyleft-ok | strict
    maintainer_history:
      enabled: true
    typosquatting:
      enabled: true
    known_bad:
      enabled: true
    sbom_diff:
      enabled: true
    source_reproducibility:
      enabled: true

  # When to run deps checks
  on_build: true                # run before every build (default: true)
  on_release: true              # full pull + verify before release builds (default: true)

  # Failure behavior
  fail_on: critical             # critical | warning | info — threshold for blocking builds

  # Allowlist for known-acceptable findings
  allow:
    - dep: "github.com/some/package"
      check: license
      reason: "Vendored with permission, see LICENSE-THIRD-PARTY"
    - dep: "lodash@4.17.21"
      check: source_reproducibility
      reason: "Known minification difference, verified manually"
```

**Zero-config behavior:** Without a `deps:` section, StageFreight defaults to `mode: verify` with all checks enabled and a local cache at `.stagefreight/deps-cache/`. The first build is slow (builds everything from source to establish signatures). Subsequent builds are fast (download + signature compare). The developer gets full supply chain protection without writing a single line of config.

**Integration with `audit deps` and `audit packages`:**

The `stagefreight audit deps` and `stagefreight audit packages` commands run the check pipeline *without* building or caching — they're diagnostic/reporting tools. `stagefreight deps` is the runtime pipeline that actually resolves, checks, and supplies dependencies to the build. Think of `audit` as the doctor's checkup and `deps` as the immune system — one is on-demand diagnostics, the other is always running.

**Build engine integration:**

`stagefreight docker build` and `stagefreight build` run the deps pipeline automatically before the build starts (unless `deps.on_build: false`). The pipeline:
1. Resolves all dependencies through the configured mode
2. Runs all checks
3. If checks pass, sets up a local mirror with verified deps
4. Injects the mirror into the build context (for Docker builds, this means a pre-build stage that populates a cache mount; for artifact builds, it means environment variables pointing at the local mirror)
5. Hands off to the build engine

The result: every built artifact has a verified, checked dependency chain — from source to binary to container image. Full provenance, full traceability, zero trust in upstream pre-built artifacts.

#### Image Attestation & Pull-Time Verification

The deps pipeline verifies everything *before* the build. The build engine produces a known-good image. But what happens after? The image sits in registries. Registries get compromised. Tags get overwritten. Digests get swapped. Between the moment StageFreight pushes an image and the moment Kubernetes pulls it, the image is unattended — and that gap is where supply chain attacks land.

StageFreight closes this gap by extending the verification chain from build time all the way to pull time. The model has three layers: **attest**, **distribute**, **enforce**.

**Layer 1: Attest — record what was built and where it landed.**

After every successful build+push, StageFreight produces an attestation record — a signed statement of fact:

```
Attestation:
  image: docker.io/prplanit/myapp
  digest: sha256:abc123...
  tag: 1.2.3
  source_commit: def456...
  source_repo: gitlab.prplanit.com/precisionplanit/myapp
  build_date: 2026-02-22T14:30:00Z
  builder: stagefreight v1.3.0
  deps_verified: true
  lint_passed: true
  registries_pushed:
    - url: docker.io/prplanit/myapp:1.2.3
      digest: sha256:abc123...
      pushed_at: 2026-02-22T14:30:12Z
    - url: ghcr.io/sofmeright/myapp:1.2.3
      digest: sha256:abc123...
      pushed_at: 2026-02-22T14:30:14Z
    - url: registry.prplanit.com/tools/myapp:1.2.3
      digest: sha256:abc123...
      pushed_at: 2026-02-22T14:30:15Z
  signature: <cosign/sigstore signature>
```

This attestation is:
- **Signed** using cosign/Sigstore (keyless or key-based, configurable). The signature proves StageFreight produced this attestation — not an attacker who compromised a registry.
- **Stored alongside the image** as an OCI artifact (using cosign `attest` — the attestation travels with the image across registries).
- **Stored in a transparency log** (Rekor, or a self-hosted instance) for tamper-evident audit trails.

The attestation includes the digest at every registry endpoint. This is the critical detail: StageFreight doesn't just record "I built sha256:abc123" — it records "I pushed sha256:abc123 to these three registries and verified the digest matched at each endpoint after the push." If a registry silently replaces the image later, the attestation is evidence of the divergence.

**Layer 2: Distribute — cross-registry digest verification.**

After pushing to all configured registries, StageFreight performs a post-push verification sweep:

1. For each registry, pull the manifest by tag and compare the digest against what was just pushed.
2. If any registry returns a different digest, alert immediately — something between push and verify changed the image.
3. Record the verification result in the attestation.

In daemon mode, this verification sweep runs on a schedule — not just at push time. The daemon periodically pulls manifests from all registries for all managed images and compares digests against the attestation. If a registry diverges from the attested digest, StageFreight alerts. This catches:
- Registry compromise (image replaced with malicious version)
- Tag mutation (someone re-pushed a different image to the same tag)
- Registry corruption (bit rot, storage failures)
- Mirror drift (pull-through cache serving stale or tampered content)

```
$ stagefreight docker verify myapp:1.2.3
  docker.io/prplanit/myapp:1.2.3          ✓ sha256:abc123... matches attestation
  ghcr.io/sofmeright/myapp:1.2.3          ✓ sha256:abc123... matches attestation
  registry.prplanit.com/tools/myapp:1.2.3  ✗ sha256:fff999... DOES NOT MATCH (expected sha256:abc123...)

  ALERT: registry.prplanit.com/tools/myapp:1.2.3 digest mismatch — image may have been tampered with
```

**Layer 3: Enforce — verify at pull time in Kubernetes.**

The attestation is only useful if something checks it before running the image. Kubernetes admission controllers are the enforcement point — they intercept pod creation and can accept, reject, or mutate based on policy.

StageFreight integrates with existing policy engines rather than reinventing one:

| Policy Engine | Integration |
|---|---|
| **Sigstore policy-controller** | Verifies cosign signatures and attestations against a CIP (ClusterImagePolicy). StageFreight's attestations are standard Sigstore format — the policy-controller consumes them natively. |
| **Kyverno** | `verifyImages` rules check cosign signatures and attestation predicates. StageFreight publishes attestations as in-toto predicates that Kyverno can match against (e.g., "require `deps_verified: true` and `lint_passed: true`"). |
| **OPA Gatekeeper** | External data provider or OCI artifact verification via rego policies. StageFreight attestations serve as the data source. |
| **Connaisseur** | Lightweight signature-only verification. Works with StageFreight's cosign signatures out of the box. |

The enforcement flow:

```
Developer pushes code
  → CI pipeline: stagefreight docker build
    → deps verified ✓
    → lint passed ✓
    → image built, signed, attested
    → pushed to 3 registries with verified digests
    → attestation stored as OCI artifact + transparency log

Kubernetes schedules a pod with that image
  → Admission controller intercepts
  → Pulls attestation from OCI artifact store
  → Verifies cosign signature (proves StageFreight made this attestation)
  → Checks attestation predicates:
      - digest matches? ✓
      - deps_verified: true? ✓
      - lint_passed: true? ✓
      - source_repo is in allowed list? ✓
      - build_date within acceptable age? ✓
  → Pod admitted

If ANY check fails:
  → Pod rejected with clear reason
  → Alert fires (webhook, Slack, PagerDuty)
  → StageFreight daemon picks up the rejection and logs it
```

**What this catches that nothing else does:**

| Scenario | Without StageFreight | With StageFreight |
|---|---|---|
| Registry compromise (image swapped) | Pod runs malicious image. Nobody notices until damage is done. | Admission controller rejects — digest doesn't match attestation. Alert fires. |
| Tag mutation (re-push to same tag) | Pod runs unexpected version. "It was working yesterday" debugging spiral. | Admission controller rejects — new digest has no attestation. Must go through build pipeline to get one. |
| Pull-through cache poisoning | JCR/Artifactory proxy serves tampered image transparently. All nodes pull the bad image. | Daemon's periodic digest sweep catches the divergence. Admission controller rejects pods using unattested digests. |
| Developer `docker push` to production tag | Bypasses CI entirely. Untested, unscanned image running in production. | No attestation exists for the manually pushed image. Admission controller blocks it. Only StageFreight-built images have valid attestations. |
| Stale image running for months | Base image has 47 unpatched CVEs. Nobody rebuilds because the code hasn't changed. | Attestation includes `build_date`. Policy can enforce maximum image age. Daemon's scheduled rebuilds produce fresh attestations. |

**`stagefreight init` generates the admission policy.** When `stagefreight init` detects a Kubernetes cluster context (or the manifest has `enforce:` config), it scaffolds the appropriate admission policy for the detected policy engine — a Kyverno `ClusterPolicy`, a Sigstore `ClusterImagePolicy`, or an OPA constraint. The policy is pre-configured to verify StageFreight attestations for the project's registries. Delivered via MR to the cluster's GitOps repo.

**Manifest config:**

```yaml
docker:
  attestation:
    # Sign images and attestations with cosign
    sign: true
    # Signing method
    method: keyless                   # keyless (Sigstore/Fulcio OIDC) | key (bring your own cosign key)
    # key: ${COSIGN_KEY}             # only used when method: key

    # Transparency log for tamper-evident audit trail
    transparency_log: rekor           # rekor (public) | rekor-self (self-hosted) | none
    # rekor_url: https://rekor.example.com  # only for rekor-self

    # Post-push verification
    verify_after_push: true           # pull manifests back and compare digests (default: true)

    # Periodic digest verification (daemon mode)
    verify_schedule: daily            # daily | weekly | cron expression
    verify_on_alert: abort            # abort | alert | both — what to do on mismatch
    #   abort: daemon marks the image as compromised, removes tag attestation (admission controller will block)
    #   alert: send alert but take no action (manual investigation)
    #   both: alert AND mark as compromised

  # Kubernetes admission enforcement (generates policy resources)
  enforce:
    engine: kyverno                   # kyverno | sigstore-policy-controller | gatekeeper | connaisseur
    # What the admission policy requires:
    require:
      - signed                        # image must have a valid cosign signature
      - attested                      # image must have a StageFreight attestation
      - deps_verified                 # attestation must confirm deps were verified
      - lint_passed                   # attestation must confirm lint passed
    # Optional constraints:
    max_image_age: 30d                # reject images older than 30 days (forces rebuilds)
    allowed_repos:                    # only allow images from these source repos
      - gitlab.prplanit.com/precisionplanit/*
      - github.com/sofmeright/*
    allowed_registries:               # only allow pulls from these registries
      - docker.io/prplanit/*
      - ghcr.io/sofmeright/*
      - registry.prplanit.com/tools/*
```

**Zero-config behavior:** Without an `attestation:` section, StageFreight records digests and registry endpoints in its build result but does not sign or store attestations. The verification data is available in the build log and can be promoted to full attestation later. Adding `attestation.sign: true` activates the full chain — sign, attest, store, verify.

**Relationship to existing Planned Features:**

The "Image provenance chain" entry in Planned Features → Container Image Lifecycle is subsumed by this. The attestation system *is* the provenance chain — but it goes further by making the chain enforceable at pull time, not just auditable after the fact.

#### `stagefreight audit codebase` — Invisible Character & Injection Scanning

> **Note:** The detection engine described here is the same one that powers `stagefreight lint --module unicode`. Lint runs it on changed files during every build; `audit codebase` runs it against the entire repository as a deep sweep. Use `audit codebase` for initial adoption audits, periodic full-repo sweeps, and fork sync verification. Use `lint` for the per-commit guard.

Source code is text, and text can lie. Characters exist that are invisible to humans but interpreted by compilers, rendered by terminals, or consumed by AI assistants. This is a real and growing attack surface — especially as AI coding tools read every file in a repo.

**What it detects:**

| Category | Characters / Patterns | Attack |
|---|---|---|
| **Trojan Source** (CVE-2021-42574) | Unicode bidi overrides: `U+202A` (LRE), `U+202B` (RLE), `U+202C` (PDF), `U+202D` (LRO), `U+202E` (RLO), `U+2066` (LRI), `U+2067` (RLI), `U+2068` (FSI), `U+2069` (PDI) | Code appears different in editors than what compilers execute. Reorders characters visually to hide malicious logic in plain sight. |
| **Zero-width characters** | `U+200B` (zero-width space), `U+200C` (ZWNJ), `U+200D` (ZWJ), `U+FEFF` (BOM mid-file), `U+2060` (word joiner), `U+180E` (Mongolian vowel separator) | Invisible characters that break string comparisons, hide payloads in identifiers, or smuggle data through copy-paste. Two variable names can look identical but be different. |
| **Homoglyph attacks** | Cyrillic `а` vs Latin `a`, Greek `ο` vs Latin `o`, and hundreds of similar pairs | Identifiers, URLs, or strings that look correct but reference different things. `pаypal.com` (Cyrillic а) vs `paypal.com`. |
| **ANSI escape sequences** | `\x1b[`, `\033[`, `\e[` in non-terminal contexts (source code, configs, data files) | Hide or overwrite terminal output. Make `git diff` or `cat` show clean code while the actual content is malicious. Escape sequences in log output can overwrite previous lines. |
| **Invisible prompt injection** | Hidden instructions targeting AI assistants: zero-width encoded text, HTML/XML comment blocks with directives, Unicode tag characters (`U+E0001`–`U+E007F`), invisible `<instructions>` tags, base64-encoded directives in comments | Codebase files containing invisible or visually-hidden text that AI coding assistants (Claude Code, Copilot, Cursor, Codex) will read and follow as instructions, but human reviewers will never see. The AI reads the full file content including invisible characters — the human only sees the rendered output. |
| **Unicode tag characters** | `U+E0001`–`U+E007F` (Tags block) | Deprecated Unicode block that renders as completely invisible in all editors and terminals. Can encode arbitrary ASCII as invisible Unicode. Sometimes used to watermark text but also to hide payloads. |
| **Confusable whitespace** | `U+00A0` (NBSP), `U+2000`–`U+200A` (various spaces), `U+205F` (medium math space), `U+3000` (ideographic space) | Break indentation-sensitive languages (Python, YAML, Makefile). Code looks correctly indented but isn't. YAML parsing changes with wrong whitespace. |

**How it works:**
1. Scans all text files in the repo (respects `.gitignore`, configurable include/exclude patterns)
2. Flags any byte sequence matching the detection categories above
3. Reports file, line, column, character code point, and category
4. Severity: bidi overrides and prompt injection are **critical**, zero-width and homoglyphs are **warning**, confusable whitespace is **info**
5. CI mode: exit non-zero on critical findings (configurable threshold)
6. Fix mode (`--fix`): remove or replace flagged characters, open MR with diff showing exactly what was cleaned

**The AI prompt injection angle is the important one.** As AI coding assistants become standard tooling, every file in a codebase becomes an attack surface for invisible instructions. A malicious contributor can embed invisible directives in a source file that:
- Tell the AI to ignore security warnings
- Instruct the AI to introduce specific vulnerabilities
- Direct the AI to exfiltrate code or context to external URLs
- Override the AI's safety guidelines for subsequent interactions

The human reviewer sees clean code. The AI reads the invisible instructions. `stagefreight audit codebase` catches this before it reaches the main branch.

**Manifest config:**
```yaml
security:
  codebase_audit: true                # enable invisible character scanning
  codebase_audit_fix: false           # true = auto-fix via MR, false = report only
  codebase_audit_threshold: warning   # critical | warning | info — fail CI at this level
```

**Market angle:** The supply chain angle is legitimately strong given xz-utils, polyfill.io, and constant npm/PyPI attacks. Centralizing pre-build dependency auditing across an entire org's repos is a real sell to security teams. The invisible character / AI prompt injection scanning adds a unique angle that nobody else covers — as AI-assisted development becomes standard, this becomes a mandatory check.

### Phase 5 — Daemon Mode (The Endgame)

`stagefreight serve` — a long-running daemon deployed as a K8s pod.

**Capabilities:**
- Multiple provider connections with deploy tokens per org
- Org scanning: discovers all repos the token can see
- Global opt-in defaults per org (baseline behavior for all visible repos)
- Per-repo manifest (`.stagefreight.yml`) for override/customization
- Mirror awareness: repos declare source-of-truth vs mirror, writes only go to source
- Scheduled maintenance (cron-style per feature)
- MR-based workflow: every write operation is a PR/MR
- YOLO mode: auto-merge for repos that opt in

---

## Per-Repo Manifest (`.stagefreight.yml`)

Discovered by the daemon in each repo. Overrides org-level defaults from the daemon config. Also usable by the CLI for local runs.

```yaml
version: 1

# Source-of-truth declaration (overrides daemon mirror patterns)
source: true                    # this repo IS the source of truth
# OR:
# mirror_of: gitlab.prplanit.com/precisionplanit/my-project  # this is a mirror

release:
  notes: true
  badge: true

docker:
  platforms: [linux/amd64, linux/arm64]
  registries:
    - url: docker.io
      path: prplanit/my-app
      tags: ["{version}", "{major}.{minor}", latest]
    - url: ghcr.io
      path: sofmeright/my-app
      tags: ["{version}", latest]
  rebuild:
    schedule: weekly
    on_base_update: true

lint:
  level: changed                  # full | changed (default) | off
  modules:
    secrets: true
    unicode: true
    yaml: true
    tabs: false                   # project uses tabs — disable this module
  exclude: ["vendor/**"]

docs:
  readme_inputs: true
  readme_sync: [dockerhub]      # push README to these registry descriptions

security:
  scan: true
  sbom: true
  fail_on_critical: false
  audit_deps: true
  secret_findings: alert          # alert (default, discrete) | mr (opt-in MR mode)
  codebase_audit: true            # invisible character + AI prompt injection scanning
  codebase_audit_threshold: warning   # critical | warning | info

license:
  compliance: true
  headers: true                   # enforce SPDX headers in source files
  spdx: true                     # generate SPDX documents

templates:
  golden_files: true              # sync .editorconfig, CONTRIBUTING.md, etc. from org template
  gitignore: true                 # lint and fix .gitignore for detected languages

fork:
  upstream: github.com/original/repo
  strategy: rebase              # or: merge
  auto_sync: true

dev:
  target: dev                 # Dockerfile stage for dev builds (auto-detected if "dev" stage exists)
  env:
    DATABASE_URL: postgres://dev:dev@db:5432/app_dev
    LOG_LEVEL: debug
  services:
    db:
      image: docker.io/library/postgres:17-alpine
      env: { POSTGRES_USER: dev, POSTGRES_PASSWORD: dev, POSTGRES_DB: app_dev }
      healthcheck: { test: "pg_isready -U dev", interval: 5s }
  ports: ["8080:8080"]
  test:
    startup: { timeout: 30s, depends_on: [db] }
    healthchecks:
      - { name: http-ready, type: http, url: "http://localhost:8080/health", expect_status: 200 }
    sanity:
      - { name: smoke-test, run: "curl -sf http://localhost:8080/api/status | jq -e '.ok'" }
      - { name: unit-tests, run: "pytest tests/ -x" }

flair:
  badges: auto                  # auto-manage shields.io badges in README

yolo: false                     # auto-merge MRs opened by stagefreight
```

**Resolution order:** Per-repo `.stagefreight.yml` > daemon org defaults > nothing (repo ignored if no manifest and no global opt-in).

---

## Daemon Config

What the `stagefreight serve` pod reads on startup. Defines provider connections, org-level defaults, scan intervals, and mirror rules.

```yaml
providers:
  - name: gitlab-prplanit
    type: gitlab
    url: https://gitlab.prplanit.com
    token: ${GITLAB_TOKEN}
    orgs: [precisionplanit, components]
    defaults:
      release: { notes: true, badge: true }
      security: { scan: true, secret_findings: alert }
      license: { compliance: true }
      templates: { golden_files: true, gitignore: true }

  - name: github-sofmeright
    type: github
    token: ${GITHUB_TOKEN}
    orgs: [sofmeright]
    defaults:
      release: { notes: true }
      security: { secret_findings: alert }

alerts:
  # Where discrete findings (secrets, critical CVEs) are sent
  - type: smtp
    to: security@precisionplanit.com
  - type: webhook
    url: ${ALERT_WEBHOOK_URL}

scan:
  interval: 15m
```

### Mirror Rules

Mirror rules define automated git push relationships between providers. The daemon evaluates these on every scan cycle.

```yaml
mirrors:
  # Mirror a group, flattening the path
  - source:
      provider: gitlab-prplanit
      match: "precisionplanit/.*"
      ignore: "precisionplanit/private-.*"
    target:
      provider: github-sofmeright
      path: "sofmeright/{name}"

  # Mirror components, flattening group structure
  - source:
      provider: gitlab-prplanit
      match: "components/.*"
    target:
      provider: github-sofmeright
      path: "sofmeright/{name}"

  # Broad match — everything readable, preserving structure
  - source:
      provider: gitlab-prplanit
      match: ".*"
      ignore: "(private-|internal/).*"
    target:
      provider: github-sofmeright
      path: "{group}/{name}"
```

**Template variables:** `{domain}`, `{group}`, `{name}`, `{path}` (full group/name).

**Mirror engine behavior:**
- Evaluates `match` regex against `{group}/{name}` of every discovered repo
- Skips repos matching `ignore` regex
- Applies `path` template to determine target repo location
- **Auto-push**: if a rule matches and the daemon has write access to the target, it pushes — the mirror config IS the opt-in
- Source-of-truth is always the `source` side — writes/MRs only go to source, git pushes go to target
- Per-repo `.stagefreight.yml` can override with `source: true` or `mirror_of:` to declare relationships explicitly
- A repo declaring `mirror_of:` in its manifest will never be treated as a source, even if a mirror rule would match it as one

---

## Planned Features (Beyond Core Phases)

Features accepted for implementation, to be slotted into phases as the core architecture matures.

### Template & Boilerplate Sync

| Feature | Description |
|---|---|
| **Golden file management** | Define template files (`.editorconfig`, `.gitattributes`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, `LICENSE`) in a template repo or daemon config. StageFreight ensures all repos in the org stay in sync — opens MRs when drift is detected. |
| **CI template drift** | When a shared CI template or reusable workflow is updated, detect which repos are still on the old version and open upgrade MRs. |
| **`.gitignore` drift** | Detect repos missing standard ignores for their detected language/framework. Lint and fix via MR. |

### Shared Codebase Inheritance — The Wireframe Model

Most internal projects share 60-80% of their scaffolding: authentication, routing, error handling, health checks, config loading, logging, database migrations, CI pipelines, Dockerfile patterns, deployment manifests. Today you either copy-paste this across repos (drift immediately), maintain a generator/template repo (generates once, never updates), or build a monorepo (organizational overhead, coupling, access control nightmares).

StageFreight introduces a different model: **a living wireframe repo that downstream projects fork and continuously inherit from.**

**The idea:**

1. You maintain a **wireframe repository** — a Go+React (or whatever stack) application skeleton with your org's standard patterns, UI look, shared components, auth integration, and infrastructure wiring. This is a real, runnable application — not a cookiecutter template.

2. Projects **fork the wireframe** and add their domain-specific code on top. The fork relationship is explicit — StageFreight tracks it via `fork sync`.

3. When the wireframe gets improvements (dependency updates, security patches, UI refresh, new shared components, CI pattern changes), `stagefreight fork sync` **automatically propagates those changes downstream** to every project that inherits from it — preserving each project's local modifications.

4. The wireframe itself uses StageFreight for its own lifecycle — dogfooding the entire release, scan, badge, and sync pipeline. This means the most simplistic possible demonstration of the tool's value: one repo's improvements flow to N repos automatically.

**What the wireframe contains:**

```
wireframe/
  src/
    backend/           # Go API server — auth, middleware, health, config
    frontend/          # React app — shared components, theming, layout
    migrations/        # Database migration framework (empty by default)
  deploy/
    kubernetes/        # Base Helm chart / kustomize for deployment
    docker/            # Multi-stage Dockerfile with org standards
  .stagefreight.yml    # Full manifest — lint, build, test, security, release
  .stagefreight/
    commit.yml         # Org-standard commit types and language
```

**What a downstream project adds:**

```
my-project/
  src/
    backend/handlers/  # Project-specific API routes
    frontend/pages/    # Project-specific UI pages
    migrations/001_*.sql  # Project schema
  .stagefreight.yml    # Inherits wireframe config, overrides project-specific values
```

**How fork sync handles this:**

StageFreight's `fork sync` already handles upstream tracking with local patch preservation. Applied to the wireframe model:

- Wireframe pushes a React component update → fork sync opens MRs in all downstream projects with the change, rebased on top of their local modifications
- Wireframe patches a security vulnerability in the shared auth middleware → every project gets the fix automatically
- Wireframe updates the Dockerfile base image → all projects rebuild with the patched base
- A downstream project has customized the navbar → fork sync preserves the customization when merging wireframe changes that touch other parts of the layout

**The multitenancy angle:**

The wireframe can be designed for multitenancy from the start — one codebase, per-tenant configuration, shared infrastructure. A new project (tenant) forks, sets its tenant config, and gets a fully functional application with org-standard auth, UI, CI, and deployment. The wireframe handles the shared majority; the fork handles the unique minority.

**Config inheritance:**

```yaml
# wireframe/.stagefreight.yml
inherit:
  from: wireframe              # this IS the wireframe
  expose:                      # what downstream projects can override
    - docker.registries
    - metadata.name
    - metadata.description
    - release.sync

# downstream/.stagefreight.yml
inherit:
  from: https://gitlab.prplanit.com/precisionplanit/wireframe
  pin: main                    # track main branch of wireframe
  override:
    metadata:
      name: my-project
      description: "My specific project"
    docker:
      registries:
        - url: docker.io
          path: prplanit/my-project
          credentials: DOCKERHUB
```

**Why this matters:**

The goal is to maintain shared functionality once, in one place, and have it flow to every project that needs it. The wireframe model makes the common case trivial (fork, configure, deploy) and the customization case explicit (local patches tracked and preserved through upstream updates). StageFreight is both the tool that enables this and the first project that demonstrates it — building itself with its own pipeline, inheriting patterns it enforces on others.

### License Management

| Feature | Description |
|---|---|
| **License compliance scanning** | Scan all repos' dependencies for license compatibility. Flag AGPL pulled into MIT projects, GPL in proprietary codebases, etc. |
| **License header enforcement** | Ensure source files have proper SPDX headers. Open MRs to add/fix them. |
| **SPDX/SBOM generation** | Full SPDX document generation per repo — extends Phase 4 SBOM work into a standalone capability. |

### Secrets & Credential Hygiene

| Feature | Description |
|---|---|
| **Org-wide secret scanning** | Sweep all repos the daemon can see for committed secrets (API keys, tokens, passwords, private keys). Unlike per-repo pre-commit hooks, this catches secrets that were committed before tooling was in place. |
| **CI variable staleness** | Detect expired tokens, references to deleted runners, or deprecated images in CI configs across the org. |

**Disclosure policy:** Discovered secrets are sensitive — StageFreight does NOT open MRs by default for secret findings. Instead:
- **Default (discrete mode):** Findings are logged to a configured alert channel (SMTP, webhook, Slack, syslog) visible only to repo owners/admins. The alert includes the file, line, secret type, and remediation steps — but never the secret value itself.
- **Opt-in MR mode:** Repos can set `security.secret_findings: mr` in `.stagefreight.yml` to have StageFreight open MRs that rotate/remove the secret. MRs are marked confidential where the provider supports it (GitLab confidential MRs, GitHub draft PRs with restricted visibility).
- **Never in public view:** Secret findings never appear in public issue trackers, commit messages, or non-confidential MRs regardless of configuration.

### Repository Hygiene

| Feature | Description |
|---|---|
| **Tag hygiene** | Detect inconsistent tag naming (`v1.0` vs `1.0` vs `release-1.0`), missing tags for releases, orphaned tags pointing at deleted branches. Normalize via MR or report. |
| **Stale issue/MR triage** | Auto-label, auto-close, or ping on issues/MRs that have gone cold. Cross-provider — works across GitLab, GitHub, Gitea simultaneously. Cross-repo sync for issues that span multiple repos. |

### Container Image Lifecycle

| Feature | Description |
|---|---|
| **Base image staleness** | Scan Dockerfiles across the org for pinned-to-old or EOL base images. Open upgrade MRs. |
| **Registry tag cleanup** | Enforce retention policies on container registries — delete tags older than N days, keep only the last N tags per branch. Registries balloon silently. |
| **Image provenance chain** | Full traceability from source commit → build → registry tag. **Subsumed by Phase 4's Image Attestation & Pull-Time Verification** — the attestation system provides the provenance chain plus enforceable verification at every pull. |
| **Security patch rebuilds** | Monitor published images for base-layer CVEs. When a security patch lands upstream, automatically rebuild and republish affected releases from their known-good build assets or repeatable build configs. Covers old releases that maintainers forget to patch — if the original build is reproducible (Dockerfile + pinned deps), StageFreight rebuilds it with the patched base and pushes a point release. Driven by `docker.rebuild.on_base_update` in the build engine. |
| **Continuous rebuild scheduling** | Daemon triggers rebuilds at configured intervals (`daily`, `weekly`, `monthly`, cron expression) to pick up base image patches without waiting for code changes. Frequency is per-repo via `docker.rebuild.schedule`. |

### Release Automation

| Feature | Description |
|---|---|
| **Changelog maintenance** | Keep `CHANGELOG.md` as a living file updated on every merge, not just at release time. |
| **Semantic version enforcement** | Based on conventional commits or commit categories, enforce/suggest the next version number. |
| **Release notifications** | Webhook, Slack, or email on new releases. Natural fit for daemon mode since it already watches every repo. |
| **Breaking change propagation** | When a dependency (internal or external) releases a major version, open MRs across all affected repos in the org. |

### Documentation Quality

| Feature | Description |
|---|---|
| **README completeness scoring** | Score repos on whether they have description, install instructions, usage examples, license section, etc. Open MRs with skeletons for what's missing. The "repo lander advice" feature. |
| **Broken cross-references** | Detect rotted links between your own repos' docs — internal org links are a separate beast from external URL linting. |

---

## Freshness Module — Versioning & Detection

> Tag classification, version comparison, family grouping, upgrade advisory,
> and vulnerability correlation for the freshness lint module.

### Freshness: Competitive Landscape

StageFreight competes with (or replaces the need for) these tools in the
container version tracking space:

| Tool | Approach | Where We Win | Where They Win |
|------|----------|--------------|----------------|
| **Renovate** | Config-driven, per-package versioning schemes | Auto-detection of families without user config; compound suffix handling; pre-release awareness | Massive ecosystem coverage (npm, pip, cargo, etc.); mature MR/PR workflow; `extractVersion` regex escape hatch |
| **Dependabot** | Opinionated, limited config | Any non-trivial Docker tag; family grouping; sha-pinned advisory | Zero-config GitHub integration; release notes linking via OCI `image.source` |
| **Flux Image Automation** | Per-image ImagePolicy CRDs with regex + policy | No per-image CRD boilerplate needed; works outside k8s | Clean two-phase design (`filterTags.extract` + policy); native k8s integration |
| **Watchtower** | Digest-only tracking on current tag | Version-aware cross-tag upgrades; advisory for sha-pinned images | Simpler mental model for "just keep my tag fresh" |
| **WUD (What's Up Docker)** | Registry polling + tag regex filters | Automatic family grouping; pre-release ranking | Nice dashboard UI; broad notification support |
| **Diun** | Regex include/exclude on tags | No regex config needed for common patterns | Lightweight; notification-focused |

#### Renovate's Known Weaknesses (Our Advantages)

1. **Compound suffixes break exact matching**
   `docker:24.0.7-dind-alpine3.18` — Renovate requires the entire
   `-dind-alpine3.18` to match exactly. When Alpine bumps to 3.19, Renovate
   finds zero candidates. Our `normalizeFamily` strips the Alpine version,
   producing family `dind-alpine`, so `24.0.7-dind-alpine3.19` groups correctly.
   ([Discussion #26820](https://github.com/renovatebot/renovate/discussions/26820))

2. **Pre-release tags misclassified as compatibility suffixes**
   `1.2.3-beta.1` — Renovate's Docker versioning treats everything after the
   hyphen as a compatibility suffix, not a pre-release indicator. It will never
   suggest upgrading from `beta.1` to the stable `1.2.3`. Our `detectPreRelease`
   and `PreRank` ordering handle this correctly.
   ([Discussion #34832](https://github.com/renovatebot/renovate/discussions/34832))

3. **LinuxServer `lsNNN` rebuild suffixes**
   `kasm:1.18.0-ls117` — Renovate's exact suffix match means `ls117` != `ls118`,
   so no updates are found. Our `normalizeFamily` strips the rebuild counter,
   grouping all `ls*` tags together.

4. **User config required for non-semver**
   MinIO `RELEASE.YYYY-MM-DDTHH-MM-SSZ`, calver `2026.1.30-hash` — Renovate
   requires per-package `packageRules` with custom `versioning: "regex:..."`.
   We detect these automatically in `decomposeTag` stages 1 and 3.

---

### Freshness: Known Bugs

#### BUG: Bitnami `rNN` Rebuild Suffix Leaves Stray `r` Prefix

**Severity:** Medium — affects all Bitnami images

Bitnami uses `6.4.3-debian-12-r10` where `rNN` is a rebuild counter (like
LinuxServer's `lsNNN`). Currently `normalizeFamily("debian-12-r10")` produces
`debian-r` instead of the expected `debian`.

The problem: `stripTrailingDigits("r10")` produces `"r"`, which passes through
`stripEmbeddedVersion` unchanged (no digit after alpha prefix since the digits
were already stripped).

**Fix:** Add `rNN` to the known rebuild pattern list alongside `lsNNN`. Either:
- Detect single-char prefix + digits as a rebuild pattern and strip entirely
- Add explicit known prefixes: `r`, `ls`, `rev`, `build`

#### BUG: `isHexHash` Only Checks Lowercase

**Severity:** Low — most images use lowercase hashes, but some don't

Our `isHexHash()` only matches `[a-f0-9]`. Tags with uppercase hex chars
(`A-F`) won't be recognized as hashes. Should lowercase the input before
checking, or extend the char range.

#### BUG: Release Depth / Precision Mismatch

**Severity:** Medium — affects major-only pins like `redis:7`, `postgres:15`

When someone pins `redis:7` (1 version segment), they want updates within
major 7 but NOT suggestions to jump to `7.4.2` (3 segments) which implies a
different pinning strategy. Currently `redis:7` with Family="" matches
`7.4.2` with Family="" — different precision levels grouped together.

Renovate handles this via "release array length matching": tags must have the
same number of version segments to be considered compatible. Controlled by
`adherence.freshness.versioning.precision` (default: `expand`) and
overridable per-package via `package_rules[].versioning.precision`.

---

### Freshness: Planned Improvements

#### Freshness P1: Fix Known Bugs

Address the three bugs listed above. Small, targeted fixes in `semver.go`.

#### Freshness P2: Implement Configuration API

**Status:** API designed (2026-02-24), implementation pending

Implement the new config shape to replace the current `FreshnessConfig` struct.
Key changes:

- Rename `SeverityConfig` fields to match `adherence.freshness.<axis>` shape
- Rename `vulnerability.min_severity` → `threshold`, `severity_override` → `zero_tolerance`
- Restructure `RegistryConfig` from ecosystem-keyed to host-keyed
- Add `match_files` to `PackageRule` match fields
- Add `versioning` struct to `PackageRule` (extract, compare, exclude, precision, allowed_versions)
- Add `Advisory` config (stable_upgrade, pin_suggestion, digest_pin, changelog_links, deduplicate_digests, suppress, digest_drift)
- Add `extends` config loading with merge strategy

#### Freshness P3: Custom Release Pattern Templates

**Status:** API designed as `package_rules[].versioning`

For images with truly unique tagging schemes that the classification pipeline
can't auto-detect, users define custom extraction and comparison in package rules:

```yaml
repository:
  package_rules:
    - match_packages: ["minio/minio"]
      versioning:
        extract: '^RELEASE\.(?P<version>\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2})Z$'
        compare: chronological
    - match_packages: ["internal.registry.io/myapp"]
      versioning:
        extract: '^build-(?P<version>\d+)$'
        compare: numerical
        exclude: ['^nightly-']
        allowed_versions: "<2.0.0"
```

The auto-detection pipeline always runs first; custom patterns are a fallback
for the long tail of weird tagging schemes.

#### Freshness P4: Auditable Failure Messages

**Status:** Gap identified

Current error handling is silent — registry timeouts, unparseable tags, and
zero-candidate families produce no user-visible output. Add structured
warnings for:

- Unparseable tags (with the raw tag string and why it failed)
- Registry failures (timeout, auth error, rate limit)
- Zero-candidate families (tag parsed but no comparable tags found)
- Custom regex with no matches
- Digest lock staleness

These are the findings that buy user trust even when the tool fails.

#### Freshness P5: OCI Manifest Annotation Fetching

**Status:** API designed as `repository.registries.<host>.fetch_annotations`

The OCI spec defines annotations on image manifests that provide ground truth
metadata we can use when tag parsing is ambiguous:

| Annotation | Use Case |
|------------|----------|
| `org.opencontainers.image.version` | Authoritative version string — disambiguates compound tags |
| `org.opencontainers.image.source` | Source repo URL — enables changelog/release notes linking |
| `org.opencontainers.image.created` | Build timestamp — resolves ordering when versions are equal |
| `org.opencontainers.image.base.name` | Base image reference — chain-of-custody for multi-stage builds |

**Implementation approach:**
- v2 manifest HEAD request per image (cheap — no full image pull, just headers + JSON)
- Cache annotations alongside tag list (same TTL as registry polling)
- Use `image.version` as a fallback when `decomposeTag()` returns `Version=nil`
- Use `image.source` to generate changelog links (controlled by `repository.advisory.changelog_links`)

**Differentiation:** Dependabot uses `image.source` for release notes but
nobody uses `image.version` for version detection. This would let us correctly
version images that have completely opaque tags but well-labeled manifests.

#### Freshness P6: Changelog & Release Notes Linking

**Status:** API designed as `repository.advisory.changelog_links`

Depends on P5. When an upgrade is available, include a link to the changelog
or release notes diff:
- Extract source repo from `org.opencontainers.image.source`
- For GitHub repos: use GitHub Releases API to find release matching the tag
- For GitLab repos: use Releases API similarly
- Include the link in the advisory message or as structured metadata on the Finding

#### Freshness P7: Multi-Registry Tag Aggregation

**Status:** Idea

Some images publish to multiple registries (Docker Hub, GHCR, Quay) with
different tag sets. When checking freshness, aggregate tags from all known
mirrors to get the most complete picture. This matters for images that
publish pre-releases only to GHCR but stable releases to Docker Hub.

#### Freshness P8: Digest-Based Rebuild Detection

**Status:** API designed as `repository.advisory.digest_drift.versioned`

Currently `lockfile.go` only tracks digests for non-versioned tags. But
versioned tags can also be rebuilt (security patches to `3.22.1` without a
version bump). Extend digest tracking to versioned tags as an optional
SeverityInfo advisory: "redis:7.4.2 has been rebuilt since last check."

This is what Watchtower and WUD do, but they can't do version comparison.
We'd be the first tool to do both.

#### Freshness P9: Digest Deduplication

**Status:** API designed as `repository.advisory.deduplicate_digests`

Suppress findings where current and suggested tags resolve to the same
manifest digest. Prevents noisy false positives when alias tags (e.g.
`redis:7.4` and `redis:7.4.2`) point to the same image. Requires a
HEAD request per candidate.

#### Freshness P10: Preset / Extends System

**Status:** API designed as top-level `extends`

For org-wide policy distribution across multiple repos. Presets are
fetched by URL or path at scan time. Merge strategy:
- Local rules prepend to inherited rules (first-match-wins)
- Local scalars replace inherited values
- Local maps merge with inherited maps

Enables the orchestrator (`stagefreight serve`) to push policy down to
agents via hosted preset URLs.

---

### Freshness: Configuration API Redesign

> **Status:** API designed (2026-02-24), implementation pending.
> See [Linter.md](Linter.md) for the currently implemented config schema.

The planned redesign restructures the freshness config around two top-level
keys separating policy from detection:

- **`adherence`** — policy layer (how strict am I about what I find)
- **`repository`** — detection layer (what to scan, where to look, how to report)

#### `extends`

| Type | Default |
|------|---------|
| list of string (URL or path) | `[]` |

Inherit configuration from external presets. Resolved in order — later
entries override earlier ones. Local config overrides all inherited values.

Merge strategy:
- Local rules prepend to inherited rules (first-match-wins semantics)
- Local scalars replace inherited values
- Local maps merge with inherited maps

#### `adherence`

Global policy layer. Controls how strictly StageFreight enforces findings.

##### `adherence.vulnerabilities`

| Field | Type | Default |
|-------|------|---------|
| `enabled` | bool | `true` |
| `threshold` | string | `"moderate"` |
| `zero_tolerance` | bool | `true` |

`threshold` — CVE severity floor. Values: `low`, `moderate`, `high`, `critical`.

`zero_tolerance` — Any finding associated with a CVE at or above threshold
is escalated to critical and reported regardless of module-specific tolerance.

##### `adherence.freshness.<major|minor|patch>`

| Field | Type | Major Default | Minor Default | Patch Default |
|-------|------|---------------|---------------|---------------|
| `severity` | int | `2` | `1` | `0` |
| `tolerance` | int | `0` | `0` | `1` |

##### `adherence.freshness.versioning`

| Field | Type | Default |
|-------|------|---------|
| `precision` | string | `"expand"` |

| Value | Behavior |
|-------|----------|
| `match` | Only suggest same segment count (`7` -> `8`, not `7.4.2`) |
| `expand` | Suggest highest at any precision |
| `ignore` | Skip freshness for this dependency |

##### `adherence.lint`

| Module | Severity Default |
|--------|-----------------|
| `tabs` | 0 (info) |
| `line_endings` | 0 (info) |
| `unicode` | 2 (critical) |
| `conflicts` | 2 (critical) |
| `filesize` | 1 (warning), default threshold: 500 KB |

#### `repository`

Detection configuration — what to scan, where to look, how to report.

##### `repository.sources`

Toggle dependency ecosystems. Omitted fields default to `true`.

| Field | Type | Default |
|-------|------|---------|
| `docker_images` | bool | `true` |
| `docker_tools` | bool | `true` |
| `go_modules` | bool | `true` |
| `rust_crates` | bool | `true` |
| `npm_packages` | bool | `true` |
| `alpine_apk` | bool | `true` |
| `debian_apt` | bool | `true` |
| `pip_packages` | bool | `true` |

##### `repository.registries`

Registry configuration keyed by host. StageFreight resolves the registry
from the image reference and looks up config by host match.

| Field | Type | Default |
|-------|------|---------|
| `auth_env` | string | _(none)_ |
| `fetch_annotations` | bool | `false` |

##### `repository.package_rules[]`

Ordered list. First match wins. All specified match fields are AND'd.

**Match fields:**

| Field | Type |
|-------|------|
| `match_packages` | list of string (glob) |
| `match_ecosystems` | list of string |
| `match_update_types` | list of string (`major`, `minor`, `patch`) |
| `match_vulnerability` | bool |
| `match_files` | list of string (glob) |

**Override fields:**

| Field | Type |
|-------|------|
| `enabled` | bool |
| `major` | object (`{ severity, tolerance }`) |
| `minor` | object (`{ severity, tolerance }`) |
| `patch` | object (`{ severity, tolerance }`) |
| `vulnerabilities` | object (`{ enabled, threshold, zero_tolerance }`) |
| `versioning` | object (`{ extract, compare, exclude, precision, allowed_versions }`) |
| `group` | string |
| `automerge` | bool |

##### `repository.advisory`

| Field | Type | Default |
|-------|------|---------|
| `stable_upgrade` | bool | `true` |
| `pin_suggestion` | bool | `true` |
| `digest_pin` | bool | `false` |
| `changelog_links` | bool | `false` |
| `deduplicate_digests` | bool | `false` |
| `suppress` | list of string (glob) | `[]` |

##### `repository.advisory.digest_drift`

| Field | Type | Default |
|-------|------|---------|
| `non_versioned` | bool | `true` |
| `versioned` | bool | `false` |

---

### Freshness: Tag Pattern Coverage Matrix

Tracking which real-world tag patterns we handle correctly. Drawn from the
dungeon cluster (323+ images) and popular Docker Hub images.

| Pattern | Example | Status | Notes |
|---------|---------|--------|-------|
| Standard semver | `alpine:3.22.1` | DONE | Basic case |
| Semver + distro suffix | `golang:1.25-alpine` | DONE | Family="alpine" |
| Semver + distro version | `golang:1.25-alpine3.22` | DONE | Family="alpine", Suffix preserved |
| Debian codename suffix | `golang:1.25-bookworm` | DONE | Family="bookworm" |
| Calver + commit hash | `searxng:2026.1.30-ad42b553b` | DONE | Hash stripped, Family="" |
| MinIO RELEASE | `minio:RELEASE.2025-09-07T16-13-09Z` | DONE | Stage 1 detection |
| Pre-release + number | `pelican:v1.0.0-beta17` | DONE | PreRank=2, PreNum=17 |
| sha- prefix | `actual-server:sha-37e807f-alpine` | DONE | Family="sha", Version=nil |
| Plex compound 4-dot | `pms-docker:1.40.2.8395-c67dce28e` | DONE | Progressive parse |
| LinuxServer rebuild | `kasm:1.18.0-ls117` | DONE | Family="ls" |
| Major-only pin | `redis:7` | BUG | Matches wrong precision |
| Bitnami rebuild | `redis:6.4.3-debian-12-r10` | BUG | Family="debian-r" not "debian" |
| Uppercase hex hash | `app:1.2.3-ABCDEF1` | BUG | isHexHash misses uppercase |
| Multi-version compound | `sd-webui:v2-cuda-12.1.1-base-22.04-v1.10.1` | UNTESTED | Needs investigation |
| Ubuntu codename | `ubuntu:noble` | DONE | Non-versioned, digest tracking |
| `latest` tag | `nginx:latest` | DONE | Digest tracking + advisory |
| Bare version no suffix | `postgres:15.3` | DONE | Family="" |
| Bitnami immutable | `redis:7.4.3-debian-12-r10` | BUG | Same as Bitnami rebuild |
| Node.js compound | `node:20.11.0-alpine3.18` | DONE | Family="alpine" |
| Distro + slim | `python:3.12-slim-bookworm` | DONE | Family="slim-bookworm" |
| RC dot notation | `app:1.0.0-rc.3` | DONE | PreRank=1, PreNum=3 |
| Digest pin | `golang@sha256:abc123...` | DONE | Advisory controlled by `repository.advisory.digest_pin` |

---

### Freshness: Completed Work

#### Tag Classification Overhaul (2026-02-24)

Replaced the naive "split on first hyphen, exact suffix match" with a
multi-stage classification pipeline.

**Files modified:** `semver.go`, `images.go`, `dependency.go`, `module.go`

**Enhanced `decomposedTag` struct:**
- `Family` — normalized grouping key (strips hashes, rebuild numbers, version-within-suffix)
- `PreRank` — pre-release ranking: 0=stable, 1=rc, 2=beta, 3=alpha, 4=dev
- `PreNum` — pre-release counter (beta17 -> 17)
- `Suffix` preserved as-is for downstream consumers

**Classification pipeline in `decomposeTag()`:**
1. MinIO RELEASE detection — `RELEASE.2025-09-07T16-13-09Z` encoded as semver `20250907.161309.0`
2. sha- prefix detection — `sha-37e807f-alpine` -> Version=nil, Family="sha"
3. Standard decomposition — split on first hyphen, semver parse, progressive fallback for 4+ dot versions

**`normalizeFamily()` — stable grouping keys:**
- Hex hashes (7-40 chars): `ad42b553b` -> stripped
- Pure numeric segments: `8395` -> stripped
- Trailing digit counters: `beta17` -> `beta`, `ls117` -> `ls`
- Embedded versions: `alpine3.22` -> `alpine`

**Pre-release-aware `latestInFamily()` + `tagNewer()`:**
- Stable releases beat pre-releases at the same version
- Higher pre-release rank wins (rc > beta > alpha > dev)
- Higher pre-release number wins within same rank (beta17 > beta13)

#### Configuration API Design (2026-02-24)

Designed the full configuration API through iterative refinement. Key
design decisions:

- **Two top-level keys** (`adherence` / `repository`) separate policy from detection
- **`adherence` is cross-cutting** — vulnerabilities and lint sit as peers to freshness
- **`package_rules[]` override fields are flat** — match fields (`match_*`) and override fields are visually distinct by naming convention
- **Registries keyed by host** — not by ecosystem; supports multiple private registries with different auth
- **`extends`** for org-wide preset inheritance — local rules prepend (first-match-wins)
- **`allowed_versions` only per-package** — global version constraints have no sensible use case
- **Max nesting depth of 4** everywhere

#### Cache TTL for External-State Modules (2026-02-25)

Content-hash caching is correct for deterministic modules but serves stale
results for modules depending on external state. Added `CacheTTLModule`
interface with clean contract: `>0` = cache with expiry, `0` = cache forever,
`<0` = never cache. See [Linter.md](Linter.md) for the full cache TTL reference.

#### Docker Hub Pagination & Version Normalization (2026-02-25)

Fixed four freshness bugs found during dogfooding:
- Added pagination loop for Docker Hub tag API (up to 10 pages / 1000 tags)
- Removed `-last_updated` ordering (returned ancient rebuilt tags, not newest)
- Strip v-prefix from GitHub release `tag_name` (Latest normalization)
- Strip v-prefix from Dockerfile ENV values (Current normalization)

---

### Freshness: References

- [Renovate Docker Versioning](https://docs.renovatebot.com/docker/)
- [Renovate Versioning Schemes](https://docs.renovatebot.com/modules/versioning/)
- [Renovate Regex Versioning](https://docs.renovatebot.com/modules/versioning/regex/)
- [Renovate Compound Suffix Bug #26820](https://github.com/renovatebot/renovate/discussions/26820)
- [Renovate Pre-Release Bug #34832](https://github.com/renovatebot/renovate/discussions/34832)
- [Flux ImagePolicy Docs](https://fluxcd.io/flux/components/image/imagepolicies/)
- [Dependabot Docker Limitations #3929](https://github.com/dependabot/dependabot-core/issues/3929)
- [OCI Image Annotations Spec](https://specs.opencontainers.org/image-spec/annotations/)
- [Bitnami Rolling Tags](https://techdocs.broadcom.com/us/en/vmware-tanzu/bitnami-secure-images/bitnami-secure-images/services/bsi-doc/apps-tutorials-understand-rolling-tags-containers-index.html)
- [LinuxServer.io Tag Conventions](https://www.linuxserver.io/blog/docker-tags-so-many-tags-so-little-time)

---

## Low Priority / Backlog

Ideas worth tracking but not yet committed to. May be promoted to Planned as the tool matures.

| Feature | Description |
|---|---|
| **Stale branch cleanup** | Branches merged but never deleted, branches with no activity for N months. Open MRs to delete or just clean in YOLO mode. |
| **Internal dependency graph** | Map which of your repos depend on which of your other repos. Visualize the internal dependency web across heterogeneous language ecosystems. |
| **Documentation platform export** | `stagefreight docs publish` — export the module tree to external documentation platforms (Wiki.js, Docusaurus, MkDocs, GitBook, ReadTheDocs, or standalone HTML5). The git repo stays the source of truth; platforms are rendered views. Outline templates (cli-tool, web-app, library, helm-chart, homelab, operator) define recommended section structures that map to platform-native navigation (sidebars, page trees, nav blocks). Platform adapters handle theming, i18n locale variants, and HTML5 presentation. Same content, same structure, different renderer — change a module in git, and your README, Wiki.js, and Docusaurus site all update in one pipeline run. |
| **Code-to-docs hyperlinking** | Documentation modules can link to specific symbols in source code (`src/handlers/auth.go:NewAuthHandler`). StageFreight resolves the symbol to a line number, generates a permalink, and auto-updates when the symbol moves. Broken references (deleted symbols) get flagged in the MR. |

---

## Feature Matrix vs Existing Tools

| Feature | Existing tools (fragmented) | StageFreight |
|---|---|---|
| Readme badges/flair management | Shields.io (manual) | `flair` — auto-managed |
| Dependency updates | Renovate, Dependabot (standalone) | `audit deps` — pre-build auditing |
| Security scanning | Snyk, Socket, Trivy (standalone) | `security scan` + `audit packages` |
| Cross-provider doc sync | Nothing good exists | `sync readme` + `sync metadata` |
| Documentation-as-data with reuse + i18n | **Nothing exists in-repo** (GitBook, Notion — external, drifts) | `docs render` — modules, templates, translations, microdocumentation |
| Fork maintenance with patch preservation | **Nothing exists** | `fork sync` + `fork status` + `fork audit` |
| Pre-build supply chain sleuthing | Socket.dev is closest | `audit deps` + `audit packages` |
| DockerHub readme sync | A few janky Actions | `sync readme` |
| SBOM tracking across releases | Manual diffing | `sbom diff` |
| Auto-MR with YOLO auto-accept | **Nothing exists as a unified feature** | MR-first workflow + `yolo: true` |
| Multi-provider daemon with org scanning | **Nothing exists** | `stagefreight serve` |
| Golden file / boilerplate sync | Manual copy-paste across repos | Golden file management + CI template drift |
| License compliance across org | FOSSA, Snyk (expensive, standalone) | License compliance + header enforcement + SPDX |
| Org-wide secret scanning | GitGuardian, TruffleHog (standalone, noisy) | Discrete alert-first scanning with opt-in MRs |
| Base image CVE rebuilds | **Nothing exists** | Security patch rebuilds — auto-rebuild old releases |
| Registry tag retention | Manual or per-registry policies | Registry tag cleanup across all registries |
| Changelog maintenance | Release-drafter, changelogen (standalone) | Living `CHANGELOG.md` updated on every merge |
| README quality scoring | **Nothing exists** | README completeness scoring + skeleton MRs |
| Invisible character / Trojan Source detection | `grep` for bidi (manual, misses most) | `audit codebase` — full Unicode threat scanning |
| AI prompt injection in codebases | **Nothing exists** | `audit codebase` — invisible instruction detection |

---

## Market Positioning

**"Hello World's a Stage"**

StageFreight does the tedious hygiene work that every repo needs but nobody wants to do. It replaces the patchwork of Renovate + Snyk + Trivy + manual badge maintenance + manual fork rebases with a single tool that understands your entire repo portfolio.

**Unique differentiators:**
- **Fork maintenance** — nothing like it exists. Keeping patched forks current with upstream while preserving changes and scanning for safety.
- **Cross-provider sync** — DockerHub readme, GitHub/GitLab mirror description parity, link verification.
- **Unified stewardship** — one tool, one manifest, one daemon watching everything.
- **Zero-config discovery** — daemon scans repos for opt-in manifests automatically, no per-repo registration.

**Supply chain angle:** xz-utils, polyfill.io, and constant npm/PyPI attacks make centralized pre-build dependency auditing across an entire org's repos a real sell to security teams.

**YOLO auto-accept** drives adoption from solo devs and homelab users fast. Low friction, high value.

---

## Branding

**StageFreight** absorbs the prplangit vision. The name is already established in CI pipelines via the GitLab component and OCI image. The stage fright + freight/shipping pun fits a tool that ships your code and manages the stage.

- **Image:** `docker.io/prplanit/stagefreight`
- **CLI:** `stagefreight`
- **Manifest:** `.stagefreight.yml`
- **Branch prefix:** `stagefreight/` (e.g., `stagefreight/update-badges`)
- **Bot identity:** StageFreight — every MR it opens, every comment it posts, is brand exposure embedded in developer workflows.
