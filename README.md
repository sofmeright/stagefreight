<p align="center">
  <img src="src/assets/logo.png" width="220" alt="StageFreight">
</p>

# StageFreight

> *Hello World's a Stage*

A declarative CI/CD automation CLI that detects, builds, scans, and releases container images across forges and registries — from a single manifest. StageFreight is open-source, self-building, and replaces fragile shell-script CI pipelines with a single Go binary driven by one [`.stagefreight.yml`](.stagefreight.yml) file.

<!-- sf:badges:start -->
<!-- sf:badges:end -->

### Features:

|                                |                                                                                                           |
| ------------------------------ | --------------------------------------------------------------------------------------------------------- |
| **Detect → Plan → Build**      | Finds Dockerfiles, resolves tags from git, builds multi-platform images via `docker buildx`               |
| **Multi-Registry Push**        | Docker Hub, GHCR, GitLab, Quay, Harbor, JFrog, Gitea — with branch/tag filtering via regex (`!` negation) |
| **Security Scanning**          | Trivy vulnerability scan + Syft SBOM generation, configurable detail levels per branch or tag pattern      |
| **Cross-Forge Releases**       | Create releases on GitLab, GitHub, or Gitea with auto-generated notes, badges, and cross-platform sync    |
| **Cache-Aware Linting**        | 7 lint modules run in parallel, delta-only on changed files, with JUnit reporting for CI                  |
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
| Manifest Examples   | [24 Example Configs](docs/config/README.md)                     |
| Roadmap             | [Full Vision](docs/RoadMap.md)                                  |
| GitLab CI Component | [Component Reference](docs/Component.md) · [Template](templates/stagefreight.yml) |

---

## Quick Start

```yaml
# .stagefreight.yml
version: 1

docker:
  platforms: [linux/amd64]
  registries:
    - url: docker.io
      path: yourorg/yourapp
      tags: ["{version}", "latest"]
      git_tags: ["^\\d+\\.\\d+\\.\\d+$"]
      credentials: DOCKER
```

```yaml
# .gitlab-ci.yml
build-image:
  image: docker.io/prplanit/stagefreight:latest
  services:
    - docker:27-dind
  script:
    - stagefreight docker build
  rules:
    - if: '$CI_COMMIT_TAG'
```

```bash
# or run locally
docker run --rm -v "$(pwd)":/src -w /src \
  -v /var/run/docker.sock:/var/run/docker.sock \
  docker.io/prplanit/stagefreight:latest \
  stagefreight docker build --local
```

---

## CLI Commands

```
stagefreight docker build    # detect → plan → lint → build → push → retention
stagefreight lint             # run lint modules on the working tree
stagefreight security scan    # trivy scan + SBOM generation
stagefreight release create   # create forge release with notes + sync
stagefreight release notes    # generate release notes from git log
stagefreight release badge    # generate/commit release status badge
stagefreight version          # print version info
```

---

## Image Contents

Based on **Alpine 3.22** with a statically compiled Go binary:

| Category | Tools |
|----------|-------|
| **CLI** | `stagefreight` (Go binary) |
| **Container** | `docker-cli`, `docker-buildx` |
| **Security** | `trivy`, `syft` |
| **SCM** | `git` |
| **Utilities** | `tree` |

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

Distributed under the [AGPL-3.0-only](LICENSE) License.
