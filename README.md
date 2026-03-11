<p align="center">
  <img src="src/assets/logo.png" width="220" alt="StageFreight">
</p>

# StageFreight

> *Hello World's a Stage*

A declarative CI/CD automation CLI that detects, builds, scans, and releases container images across forges and registries — from a single manifest. StageFreight is open-source, self-building, and replaces fragile shell-script CI pipelines with a single Go binary driven by one [`.stagefreight.yml`](.stagefreight.yml) file.

<!-- sf:badges:start -->
[![release](https://raw.githubusercontent.com/sofmeright/stagefreight/main/.stagefreight/badges/release.svg)](https://github.com/sofmeright/stagefreight/releases) [![build](https://raw.githubusercontent.com/sofmeright/stagefreight/main/.stagefreight/badges/build.svg)](https://github.com/sofmeright/stagefreight/actions) [![license](https://raw.githubusercontent.com/sofmeright/stagefreight/main/.stagefreight/badges/license.svg)](https://github.com/sofmeright/stagefreight/blob/main/LICENSE) ![updated](https://raw.githubusercontent.com/sofmeright/stagefreight/main/.stagefreight/badges/updated.svg) [![docker/pulls/prplanit/stagefreight](https://img.shields.io/docker/pulls/prplanit/stagefreight)](https://hub.docker.com/r/prplanit/stagefreight)
<!-- sf:badges:end -->
<!-- sf:props:start -->
[![Go Report Card](https://goreportcard.com/badge/github.com/{var:github_org}/{var:repo})](https://goreportcard.com/report/github.com/{var:github_org}/{var:repo}) [![Go Reference](https://pkg.go.dev/badge/github.com/{var:github_org}/{var:repo}.svg)](https://pkg.go.dev/github.com/{var:github_org}/{var:repo}) [![GitHub Release](https://img.shields.io/github/v/release/{var:github_org}/{var:repo}?style=flat-square&logo=github)](https://github.com/{var:github_org}/{var:repo}/releases/latest) [![Docker Image Size](https://img.shields.io/docker/image-size/{var:org}/{var:repo}/latest?logo=docker)](https://hub.docker.com/r/{var:org}/{var:repo}) [![Last Commit](https://img.shields.io/github/last-commit/{var:github_org}/{var:repo})](https://github.com/{var:github_org}/{var:repo}/commits)
<!-- sf:props:end -->

### Features:

|                                |                                                                                                           |
| ------------------------------ | --------------------------------------------------------------------------------------------------------- |
| **Detect → Plan → Build**      | Finds Dockerfiles, resolves tags from git, builds multi-platform images via `docker buildx`               |
| **Multi-Registry Push**        | Docker Hub, GHCR, GitLab, Quay, Harbor, JFrog, Gitea — with branch/tag filtering via regex (`!` negation) |
| **Security Scanning**          | Trivy + Grype vulnerability scan, Syft SBOM generation, configurable detail levels per branch or tag      |
| **Cross-Forge Releases**       | Create releases on GitLab, GitHub, or Gitea with auto-generated notes, badges, and cross-platform sync    |
| **Cache-Aware Linting**        | 9 lint modules run in parallel, delta-only on changed files, with JUnit reporting for CI                  |
| **Retention Policies**         | Restic-style tag retention (keep_last, daily, weekly, monthly, yearly) across all registry providers       |
| **Self-Building**              | StageFreight builds itself — this image is produced by `stagefreight docker build`                        |

### Public Resources:

|                  |                                                                                          |
| ---------------- | ---------------------------------------------------------------------------------------- |
| Docker Images    | [Docker Hub](https://hub.docker.com/r/prplanit/stagefreight)                             |
| Source Code      | [GitHub](https://github.com/sofmeright/stagefreight) / [GitLab](https://gitlab.prplanit.com/precisionplanit/stagefreight) |

### Documentation:

|                     |                                                                 |
| ------------------- | --------------------------------------------------------------- |
| CLI Reference       | [Full Command Reference](docs/reference/CLI.md)                |
| Config Reference    | [Full Config Schema](docs/reference/Config.md)                 |
| Manifest Examples   | [24 Example Configs](docs/config/README.md) · [Quick Examples](docs/examples/) |
| Roadmap             | [Full Vision](docs/RoadMap.md)                                  |
| GitLab CI Component | [Component Reference](docs/Component.md) · [Template](templates/stagefreight.yml) |

---

## Quick Start

```yaml
# .stagefreight.yml
version: 1

builds:
  - id: myapp
    kind: docker
    platforms: [linux/amd64]

targets:
  - id: dockerhub
    kind: registry
    build: myapp
    url: docker.io
    path: yourorg/yourapp
    tags: ["{version}", "latest"]
    when: { events: [tag] }
    credentials: DOCKER
```

```yaml
# .gitlab-ci.yml
build-image:
  image: docker.io/prplanit/stagefreight:latest-dev
  services:
    - docker.io/library/docker:27-dind
  script:
    - stagefreight docker build
  rules:
    - if: '$CI_COMMIT_TAG'
```

```bash
# or run locally
docker run --rm -v "$(pwd)":/src -w /src \
  -v /var/run/docker.sock:/var/run/docker.sock \
  docker.io/prplanit/stagefreight:latest-dev \
  sh -c 'git config --global --add safe.directory /src && stagefreight docker build --local'
```

---

## CLI Commands

```
stagefreight docker build       # detect → plan → lint → build → push → retention
stagefreight docker readme      # sync README to container registries
stagefreight lint                # run lint modules on the working tree
stagefreight security scan      # trivy + grype scan + SBOM generation
stagefreight release create     # create forge release with notes + sync
stagefreight release notes      # generate release notes from git log
stagefreight release badge      # generate/commit release status badge SVG
stagefreight release prune      # prune old releases via retention policy
stagefreight badge generate     # generate SVG badges from config
stagefreight narrator run       # compose narrator items into target files
stagefreight narrator compose   # ad-hoc CLI-driven composition
stagefreight docs generate      # generate CLI + config reference docs
stagefreight component docs     # generate component input documentation
stagefreight dependency update  # update dependencies with freshness analysis
stagefreight migrate            # migrate config to latest schema version
stagefreight version            # print version info
```

See [CLI Reference](docs/reference/CLI.md) for full flag documentation.

---

## Image Contents

Based on **Alpine 3.23** with a statically compiled Go binary:

| Category | Tools |
|----------|-------|
| **CLI** | `stagefreight` (Go binary) |
| **Container** | `docker-cli`, `docker-buildx` |
| **Security** | `trivy`, `syft`, `grype`, `osv-scanner` |
| **SCM** | `git` |
| **Utilities** | `tree`, `chafa` |

### Looking for a minimal image?

| Image | Purpose |
|-------|---------|
| [`prplanit/stagefreight:0.1.1`](https://hub.docker.com/r/prplanit/stagefreight) | Last pre-CLI release — vanilla DevOps toolchain (bash, docker-cli, buildx, python3, yq, jq, etc.) |
| [`prplanit/ansible-oci`](https://hub.docker.com/r/prplanit/ansible-oci) | Ansible-native image — Python 3.13 + Alpine 3.22, ansible-core, ansible-lint, sops, rage, pywinrm, kubernetes.core, community.docker, community.sops |

Starting from **0.2.0**, `prplanit/stagefreight` includes the Go CLI binary and is purpose-built for `stagefreight docker build` workflows.

---

## Contributing

- Fork the repository
- Submit Pull Requests / Merge Requests
- Open issues with ideas, bugs, or feature requests

## Support / Sponsorship

If you'd like to help support this project and others like it, I have this donation link:

[![ko-fi](https://ko-fi.com/img/githubbutton_sm.svg)](https://ko-fi.com/T6T41IT163)

---

## Disclaimer

The Software provided hereunder is licensed "as-is," without warranties of any kind. The developer makes no promises about functionality, performance, or availability. Not responsible if StageFreight replaces your entire CI pipeline and you find yourself with free time you didn't expect, your retention policies work so well your registry bill drops and finance gets confused, or your release notes become more detailed than the actual features they describe.

Any resemblance to working software is entirely intentional but not guaranteed. The developer claims no credit for anything that actually goes right — that's all you and the unstoppable force of the Open Source community.

## License

Distributed under the [AGPL-3.0-only](LICENSE) License. See [LICENSING.md](docs/LICENSING.md) for commercial licensing.
