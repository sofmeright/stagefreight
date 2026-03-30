package config

// BadgeConfig is the user-facing badge definition in .stagefreight.yml.
// Badge system owns this — narrator references badges by ID via badge_ref.
type BadgeConfig struct {
	ID       string  `yaml:"id"`                  // stable user-defined ID for narrator reference
	Text     string  `yaml:"text"`                // left side label
	Value    string  `yaml:"value"`               // right side value (templates: {env:*}, {sha}, {base}, etc.)
	Color    string  `yaml:"color"`               // hex color or "auto"
	Output   string  `yaml:"output"`              // SVG output path (required)
	Link     string  `yaml:"link,omitempty"`       // clickable URL
	Font     string  `yaml:"font,omitempty"`       // font name override
	FontSize int     `yaml:"font_size,omitempty"`  // font size override
}

// ToBadgeSpec converts config to the internal badge engine model.
func (b BadgeConfig) ToBadgeSpec() BadgeSpec {
	return BadgeSpec{
		Label:    b.Text,
		Value:    b.Value,
		Color:    b.Color,
		Output:   b.Output,
		Font:     b.Font,
		FontSize: float64(b.FontSize),
	}
}

// BadgeSpec is the reusable internal badge specification.
// Consumed by badge engine, CLI ad-hoc mode, release badge, and docker build badge.
// No YAML tags — this is an internal model, not a config surface.
type BadgeSpec struct {
	Label    string  // left side text (badge name / alt text)
	Value    string  // right side text (supports templates)
	Color    string  // hex color or "auto"
	Output   string  // SVG file output path
	Font     string  // built-in font name override
	FontSize float64 // font size override
	FontFile string  // custom font file override
}
