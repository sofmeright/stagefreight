package narrator

import (
	"github.com/PrPlanIT/StageFreight/src/manifest"
)

// BuildContentsModule renders a manifest section as markdown.
// Supports table, list, and kv renderers.
type BuildContentsModule struct {
	Manifest *manifest.Manifest
	Section  string
	Renderer string
	Columns  []string
}

// Render produces the markdown content for this manifest section.
func (b BuildContentsModule) Render() string {
	if b.Manifest == nil {
		return ""
	}

	content, err := manifest.RenderSection(b.Manifest, b.Section, b.Renderer, b.Columns)
	if err != nil {
		return ""
	}

	return content
}
