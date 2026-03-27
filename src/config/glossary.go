package config

// GlossaryConfig defines the repo's shared change-language model.
// Consumed by commit authoring, tag planning, and release rendering.
type GlossaryConfig struct {
	Types    map[string]GlossaryType `yaml:"types"`
	Breaking BreakingConfig          `yaml:"breaking"`
	Filters  FilterConfig            `yaml:"filters"`
	Rewrites RewriteConfig           `yaml:"rewrites"`
	Render   RenderConfig            `yaml:"render"`
}

// GlossaryType defines a single change type's semantics.
type GlossaryType struct {
	Aliases        []string `yaml:"aliases,omitempty"`
	CanonicalAs    string   `yaml:"canonical_as,omitempty"` // normalize to this type (e.g., deps → chore)
	Priority       int      `yaml:"priority"`
	ReleaseVisible bool     `yaml:"release_visible"`
}

// BreakingConfig defines how breaking changes are detected and presented.
type BreakingConfig struct {
	Aliases       []string `yaml:"aliases,omitempty"`   // e.g., [b, break, bc]
	BangSuffix    bool     `yaml:"bang_suffix"`         // feat! syntax
	FooterKeys    []string `yaml:"footer_keys"`         // e.g., ["BREAKING CHANGE"]
	ForceHighlight bool    `yaml:"force_highlight"`
	PriorityBoost int     `yaml:"priority_boost"`
}

// FilterConfig defines sanitization rules for change summaries.
type FilterConfig struct {
	Summary             SummaryFilter `yaml:"summary"`
	Trailers            TrailerFilter `yaml:"trailers"`
	NormalizeWhitespace bool          `yaml:"normalize_whitespace"`
}

// SummaryFilter strips content from commit subjects.
type SummaryFilter struct {
	StripPhrases []string `yaml:"strip_phrases,omitempty"`
	StripRegex   []string `yaml:"strip_regex,omitempty"`
}

// TrailerFilter strips commit trailers/footers.
type TrailerFilter struct {
	StripKeys []string `yaml:"strip_keys,omitempty"`
}

// RewriteConfig defines deterministic text transformations.
type RewriteConfig struct {
	Phrases []PhraseRewrite `yaml:"phrases,omitempty"`
	Regex   []RegexRewrite  `yaml:"regex,omitempty"`
}

// PhraseRewrite replaces exact substrings.
type PhraseRewrite struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// RegexRewrite replaces regex matches.
type RegexRewrite struct {
	Pattern string `yaml:"pattern"`
	Replace string `yaml:"replace"`
}

// RenderConfig controls glossary-level output defaults.
// Surface-specific max entries live in presentation config.
type RenderConfig struct {
	EmptyStrategy string `yaml:"empty_strategy"` // prompt | fail | allow_empty
}

// DefaultGlossaryConfig returns sensible defaults for the glossary.
func DefaultGlossaryConfig() GlossaryConfig {
	return GlossaryConfig{
		Types: map[string]GlossaryType{
			"feat":     {Aliases: []string{"f", "feature"}, Priority: 90, ReleaseVisible: true},
			"fix":      {Aliases: []string{"fx", "bugfix"}, Priority: 80, ReleaseVisible: true},
			"perf":     {Aliases: []string{"p"}, Priority: 85, ReleaseVisible: true},
			"refactor": {Aliases: []string{"rf"}, Priority: 50, ReleaseVisible: true},
			"deps":     {Aliases: []string{"dep"}, CanonicalAs: "chore", Priority: 30, ReleaseVisible: false},
			"chore":    {Aliases: []string{"c"}, Priority: 20, ReleaseVisible: false},
			"docs":     {Aliases: []string{"d"}, Priority: 10, ReleaseVisible: false},
			"ci":       {Priority: 15, ReleaseVisible: false},
			"test":     {Aliases: []string{"t"}, Priority: 10, ReleaseVisible: false},
			"style":    {Priority: 5, ReleaseVisible: false},
		},
		Breaking: BreakingConfig{
			Aliases:        []string{"b", "break", "bc"},
			BangSuffix:     true,
			FooterKeys:     []string{"BREAKING CHANGE", "BREAKING-CHANGE"},
			ForceHighlight: true,
			PriorityBoost:  1000,
		},
		Filters: FilterConfig{
			Summary: SummaryFilter{
				StripPhrases: []string{"[skip ci]", "[ci skip]"},
			},
			Trailers: TrailerFilter{
				StripKeys: []string{"Co-Authored-By", "Signed-off-by"},
			},
			NormalizeWhitespace: true,
		},
		Render: RenderConfig{
			EmptyStrategy: "prompt",
		},
	}
}
