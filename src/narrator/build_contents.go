package narrator

import (
	"fmt"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/diag"
	"github.com/PrPlanIT/StageFreight/src/manifest"
)

// BuildContentsModule renders a manifest section as markdown.
// Supports table, list, kv, badges, and versions renderers.
// Optional Wrap/Summary fields control container wrapping.
type BuildContentsModule struct {
	Manifest *manifest.Manifest
	Section  string
	Renderer string
	Columns  []string
	Wrap     string // "details" wraps in <details>/<summary>
	Summary  string // required when Wrap is set
}

// Render produces the markdown content for this manifest section.
// The Module interface does not permit error returns, so invalid state
// (e.g., unknown Wrap value) is treated as an invariant violation:
// config validation must reject it before construction. If it reaches
// runtime anyway, rendering refuses to proceed and returns empty output.
func (b BuildContentsModule) Render() string {
	if b.Manifest == nil {
		return ""
	}

	content, err := manifest.RenderSection(b.Manifest, b.Section, b.Renderer, b.Columns)
	if err != nil {
		return ""
	}

	// Apply container wrapping if configured.
	// Config validation guarantees only "details" or "" reach here.
	switch b.Wrap {
	case "details":
		var w strings.Builder
		w.WriteString("<details>\n")
		w.WriteString(fmt.Sprintf("<summary>%s</summary>\n\n", b.Summary))
		w.WriteString(content)
		w.WriteString("\n\n</details>")
		return w.String()
	case "":
		// No wrapping.
	default:
		// Unreachable with valid config. Refuse to render rather than silently degrade.
		diag.Warn("narrator: BUG: unknown wrap value %q reached runtime (config validation should have rejected this)", b.Wrap)
		return ""
	}

	return content
}
