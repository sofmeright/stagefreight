# StageFreight — To-Do / Banked Ideas

This file holds ideas that are NOT committed to the current roadmap. Items
here are candidates for future phases. Banked ≠ planned — each item needs
its own design pass before implementation begins.

## Branch promotion governance (banked idea — naming provisional)

StageFreight could grow a first-class notion of branch-to-branch promotion —
a way to enforce "main is protected; promotion happens through review/merge
from lanes" without leaking that concern into targets or versioning.

### Proposed shape (tentative — section name TBD, `promotion` is a placeholder)

```yaml
promotion:
  mode: gated
  protected_branch: main

  lanes:
    - id: development
      match: develop
      publish:
        registry_tags: ["dev-{sha:8}", "latest-dev"]

    - id: release-candidate
      match: release
      publish:
        registry_tags: ["rc-{sha:8}"]

    - id: production
      match: main
      source_lanes: [development, release-candidate]
      require:
        - ci_green
        - review_approved
      action:
        merge_only: true
        direct_commits: false
```

### Core invariants for this future feature

- **Promotion decides whether code may advance. Versioning decides how it
  identifies itself.** Never mix the two.
- Tag classification (`versioning.tag_sources`) and promotion lanes are
  orthogonal. A lane does not define tag eligibility; branch rules do.
- Matchers stay the primitive for matching branches; lanes reference matchers.
- `targets` remain pure behavior. Lanes may *reference* targets but never
  replace them.

### Why this sits cleanly on top of the Phase 3 versioning model

Phase 3 established three independent axes:

| Axis      | System     | Owns                           |
|-----------|------------|--------------------------------|
| Identity  | versioning | tag_sources, base_from, format |
| Behavior  | targets    | build, push, release           |
| Authority | promotion  | advancement, gating (future)   |

Promotion slots in as the third axis. It does not read from or mutate the
other two. A promotion layer can be added without touching `gitver`,
`versioning`, or `targets` — only the runtime entry point learns to check
promotion gates before executing behavior.

### Open questions before promoting this from "banked" to "designed"

- Is `promotion` the right top-level name? Alternatives: `lanes`, `flow`,
  `governance`, `advance`. Decide only when the feature is seriously scoped.
- How does this compose with the forge accessories sync idea (mirrors)?
- Do lanes carry their own matchers, or reference a global `matchers:` section?
- Is CI-green / review-approved enforcement in scope for StageFreight, or
  delegated to the forge?

### Rule: do not implement until after v0.6.0

Phase 3 locks versioning. Phase 4 locks matchers. Phase 5 locks repo branches.
Promotion is strictly post-v0.6.0 territory. Shipping it earlier risks
conflating authority with identity — exactly what the current phase structure
prevents.
