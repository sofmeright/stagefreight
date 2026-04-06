package badge

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Engine generates SVG badges using a specific font.
type Engine struct {
	metrics *FontMetrics
}

// New creates a badge engine with the given font metrics.
func New(metrics *FontMetrics) *Engine {
	return &Engine{metrics: metrics}
}

// NewDefault creates an engine with dejavu-sans 11pt (the standard).
func NewDefault() (*Engine, error) {
	metrics, err := LoadBuiltinFont("dejavu-sans", 11)
	if err != nil {
		return nil, fmt.Errorf("loading badge font: %w", err)
	}
	return New(metrics), nil
}

// NewForSpec creates an engine from font override parameters.
// Falls back to dejavu-sans if no overrides given.
func NewForSpec(font string, fontSize float64, fontFile string) (*Engine, error) {
	if fontSize == 0 {
		fontSize = 11
	}
	var metrics *FontMetrics
	var err error
	switch {
	case fontFile != "":
		metrics, err = LoadFontFile(fontFile, fontSize)
	case font != "":
		metrics, err = LoadBuiltinFont(font, fontSize)
	default:
		metrics, err = LoadBuiltinFont("dejavu-sans", fontSize)
	}
	if err != nil {
		return nil, fmt.Errorf("loading badge font: %w", err)
	}
	return New(metrics), nil
}

// Badge defines the content and appearance of a single badge.
type Badge struct {
	Label string // left side text
	Value string // right side text
	Color string // hex color for right side (e.g. "#4c1")
}

// Generate produces a shields.io-compatible SVG badge string.
func (e *Engine) Generate(b Badge) string {
	return e.renderSVG(b)
}

// ContrastColor returns a legible text color for the given hex background.
// Light backgrounds (luminance > 0.5) get near-black text; dark backgrounds get white.
func ContrastColor(hex string) string {
	r, g, b, ok := parseHex(hex)
	if !ok {
		return "#fff"
	}
	lin := func(c float64) float64 {
		c /= 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	L := 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
	if L > 0.5 {
		return "#111"
	}
	return "#fff"
}

// parseHex parses a CSS hex color (#rgb, #rrggbb) into float64 components.
func parseHex(hex string) (r, g, b float64, ok bool) {
	hex = strings.TrimPrefix(hex, "#")
	switch len(hex) {
	case 3:
		hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
	case 6:
		// ok
	default:
		return 0, 0, 0, false
	}
	ri, err1 := strconv.ParseInt(hex[0:2], 16, 32)
	gi, err2 := strconv.ParseInt(hex[2:4], 16, 32)
	bi, err3 := strconv.ParseInt(hex[4:6], 16, 32)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, 0, false
	}
	return float64(ri), float64(gi), float64(bi), true
}

// StatusColor maps a status keyword to a badge hex color.
func StatusColor(status string) string {
	switch status {
	case "passed", "success":
		return "#4c1"
	case "warning":
		return "#dfb317"
	case "critical", "failed":
		return "#e05d44"
	default:
		return "#4c1"
	}
}
