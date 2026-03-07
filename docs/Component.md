# StageFreight — GitLab CI Component

StageFreight ships as a GitLab CI component that wraps the CLI into a thin,
reusable pipeline template. One `include:` gives you the full
detect → lint → build → push → scan → release → retention pipeline.

```yaml
include:
  - component: gitlab.prplanit.com/components/stagefreight/stagefreight@main
    inputs:
      security_scan: true
      release_enabled: true
```

## Inputs

<!-- sf:component:start -->
## `stagefreight`

### Ungrouped
| Name | Required | Default | Description |
|------|----------|---------|-------------|
| `stagefreight_image` | ❌ | `docker.io/prplanit/stagefreight:latest-dev` | StageFreight image to use for the build |
| `dind_image` | ❌ | `docker.io/docker:27-dind` | Docker-in-Docker service image |
| `stagefreight_args` | ❌ | - | Additional arguments passed to stagefreight docker build |
| `security_scan` | ❌ | `true` | Run security scan after build |
| `security_detail` | ❌ | `counts` | Security detail level for release notes (none, counts, detailed, full) |
| `release_enabled` | ❌ | `true` | Create a release on the forge after build |


<!-- sf:component:end -->
