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
<!-- sf:component:end -->
