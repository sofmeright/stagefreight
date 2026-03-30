package config

// NarratorFile defines narrator composition for a single file target.
// v2 flattens the old files[] > sections[] > items[] hierarchy into
// a 2-level structure: file targets with self-describing items.
type NarratorFile struct {
	// File is the path to the target file (required).
	File string `yaml:"file"`

	// LinkBase is the base URL for relative link rewriting.
	LinkBase string `yaml:"link_base,omitempty"`

	// Items are the composable content items for this file.
	Items []NarratorItem `yaml:"items"`
}

// NarratorItem defines a single composable content item.
// Each item self-describes its kind and placement.
type NarratorItem struct {
	// ID is the item identifier (unique within file).
	ID string `yaml:"id"`

	// Kind is the item type: badge, shield, text, component, break, include.
	Kind string `yaml:"kind"`

	// Placement declares where this item goes in the target file.
	Placement NarratorPlacement `yaml:"placement"`

	// ── kind: badge ───────────────────────────────────────────────────────

	// Text is the badge label (left side text).
	Text string `yaml:"text,omitempty"`

	// Value is the badge value (right side text, supports templates).
	Value string `yaml:"value,omitempty"`

	// Color is the badge color (hex or "auto").
	Color string `yaml:"color,omitempty"`

	// Font is the badge font name override.
	Font string `yaml:"font,omitempty"`

	// FontSize is the badge font size override.
	FontSize int `yaml:"font_size,omitempty"`

	// Output is the SVG output path for badge generation.
	Output string `yaml:"output,omitempty"`

	// Link is the clickable URL (kind: badge, shield).
	Link string `yaml:"link,omitempty"`

	// ── kind: shield ──────────────────────────────────────────────────────

	// Shield is the shields.io path (kind: shield).
	Shield string `yaml:"shield,omitempty"`

	// ── kind: text ────────────────────────────────────────────────────────

	// Content is raw text/markdown content (kind: text).
	Content string `yaml:"content,omitempty"`

	// ── kind: component ───────────────────────────────────────────────────

	// Spec is the component spec file path (kind: component).
	Spec string `yaml:"spec,omitempty"`

	// ── kind: include ────────────────────────────────────────────────────

	// Path is the file path to include verbatim (kind: include).
	Path string `yaml:"path,omitempty"`

	// ── kind: build-contents ────────────────────────────────────────────

	// Source is an optional path to a manifest JSON file (kind: build-contents).
	// If omitted, uses the current scope manifest.
	Source string `yaml:"source,omitempty"`

	// Section is the dot-path into the manifest (kind: build-contents).
	// e.g., "inventories.pip", "build.args", "security.sbom"
	Section string `yaml:"section,omitempty"`

	// Renderer is the rendering format (kind: build-contents).
	// Supported: "table", "list", "kv".
	Renderer string `yaml:"renderer,omitempty"`

	// Columns selects which columns to render (kind: build-contents, renderer: table).
	Columns []string `yaml:"columns,omitempty"`

	// OutputFile is an optional standalone file output path (kind: build-contents).
	// When set, the rendered content is written as a standalone file.
	// Can be used alongside placement (section embedding) or alone.
	OutputFile string `yaml:"output_file,omitempty"`

	// Wrap wraps the rendered output in a container element (kind: build-contents).
	// Supported: "details" (wraps in <details>/<summary>).
	Wrap string `yaml:"wrap,omitempty"`

	// Summary is the summary text when wrap=details (required when wrap is set).
	Summary string `yaml:"summary,omitempty"`

	// ── kind: props ──────────────────────────────────────────────────────

	// Type is the props resolver type ID (kind: props).
	Type string `yaml:"type,omitempty"`

	// Params are provider-semantic inputs for the props resolver.
	Params map[string]string `yaml:"params,omitempty"`

	// Label overrides the auto-derived alt text for props.
	Label string `yaml:"label,omitempty"`

	// Style is a presentation override for shields.io badge style.
	Style string `yaml:"style,omitempty"`

	// Logo is a presentation override for shields.io logo name.
	Logo string `yaml:"logo,omitempty"`

	// ── kind: k8s-inventory ──────────────────────────────────────────────

	// CatalogPath is the path to a catalog metadata file (kind: k8s-inventory).
	// Optional — provides descriptions, friendly names, graveyard, tier overrides.
	CatalogPath string `yaml:"catalog,omitempty"`

	// ── kind: badge_ref ─────────────────────────────────────────────────

	// Ref is the badge ID to reference (kind: badge_ref).
	// Must match an ID defined in the top-level badges: config.
	// Narrator does NOT own badge generation — only composition.
	Ref string `yaml:"ref,omitempty"`
}

// HasGeneration returns true if this badge item should trigger SVG generation.
// Requires kind: badge + output set.
func (n NarratorItem) HasGeneration() bool {
	return n.Kind == "badge" && n.Output != ""
}

// ToBadgeSpec extracts badge generation fields into a reusable BadgeSpec.
func (n NarratorItem) ToBadgeSpec() BadgeSpec {
	return BadgeSpec{
		Label:    n.Text,
		Value:    n.Value,
		Color:    n.Color,
		Output:   n.Output,
		Font:     n.Font,
		FontSize: float64(n.FontSize),
	}
}

// NarratorPlacement declares where an item goes in its target file.
// Exactly one selector must be set (Between is the v2 primary selector).
type NarratorPlacement struct {
	// Between is a two-element array: [start_marker, end_marker].
	// Content is placed relative to these markers.
	Between [2]string `yaml:"between,omitempty"`

	// After is a regex/literal line match (reserved for future use).
	After string `yaml:"after,omitempty"`

	// Before is a regex/literal line match (reserved for future use).
	Before string `yaml:"before,omitempty"`

	// Heading is a markdown heading match (reserved for future use).
	Heading string `yaml:"heading,omitempty"`

	// Mode controls how content is placed:
	// replace (default), append, prepend, above, below.
	Mode string `yaml:"mode,omitempty"`

	// Inline renders items side-by-side when true (default: false).
	Inline bool `yaml:"inline,omitempty"`
}

// validNarratorItemKinds enumerates all recognized narrator item kinds.
var validNarratorItemKinds = map[string]bool{
	"badge":          true,
	"shield":         true,
	"text":           true,
	"component":      true,
	"break":          true,
	"include":        true,
	"props":          true,
	"build-contents": true,
	"k8s-inventory":  true,
	"badge_ref":      true,
}

// validPlacementModes enumerates all recognized placement modes.
var validPlacementModes = map[string]bool{
	"":        true, // default = replace
	"replace": true,
	"append":  true,
	"prepend": true,
	"above":   true,
	"below":   true,
}
