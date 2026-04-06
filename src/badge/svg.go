package badge

import (
	"encoding/base64"
	"fmt"
	"math"
	"strings"
)

// renderSVG produces a shields.io-compatible flat SVG badge with embedded font.
func (e *Engine) renderSVG(b Badge) string {
	labelWidth := int(math.Round(e.metrics.TextWidth(b.Label))) + 10
	valueWidth := int(math.Round(e.metrics.TextWidth(b.Value))) + 10
	totalWidth := labelWidth + valueWidth

	fontName := e.metrics.FontName()
	fontSize := e.metrics.FontSize()
	fontCSS := fontFaceCSS(fontName, e.metrics.FontData())

	label := xmlEscape(b.Label)
	value := xmlEscape(b.Value)

	var s strings.Builder

	s.WriteString(fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20">`, totalWidth))

	// Embedded font and gradient
	s.WriteString(`<defs>`)
	s.WriteString(fmt.Sprintf(`<style type="text/css">%s</style>`, fontCSS))
	s.WriteString(`<linearGradient id="b" x2="0" y2="100%">`)
	s.WriteString(`<stop offset="0" stop-color="#bbb" stop-opacity=".1"/>`)
	s.WriteString(`<stop offset="1" stop-opacity=".1"/>`)
	s.WriteString(`</linearGradient>`)
	s.WriteString(`</defs>`)

	// Mask and background
	s.WriteString(fmt.Sprintf(`<mask id="a"><rect width="%d" height="20" rx="3" fill="#fff"/></mask>`, totalWidth))
	s.WriteString(`<g mask="url(#a)">`)
	s.WriteString(fmt.Sprintf(`<rect width="%d" height="20" fill="#555"/>`, labelWidth))
	s.WriteString(fmt.Sprintf(`<rect x="%d" width="%d" height="20" fill="%s"/>`, labelWidth, valueWidth, xmlEscape(b.Color)))
	s.WriteString(fmt.Sprintf(`<rect width="%d" height="20" fill="url(#b)"/>`, totalWidth))
	s.WriteString(`</g>`)

	// Text — label side is always on #555 (dark), value side adapts to badge color.
	valueTextColor := ContrastColor(b.Color)
	fontFamily := fmt.Sprintf("'%s',Verdana,Geneva,sans-serif", fontName)
	s.WriteString(fmt.Sprintf(`<g fill="#fff" text-anchor="middle" font-family="%s" font-size="%g">`,
		xmlEscape(fontFamily), fontSize))
	s.WriteString(fmt.Sprintf(`<text x="%d" y="15" fill="#010101" fill-opacity=".3">%s</text>`, labelWidth/2, label))
	s.WriteString(fmt.Sprintf(`<text x="%d" y="14">%s</text>`, labelWidth/2, label))
	s.WriteString(fmt.Sprintf(`<text x="%d" y="15" fill="#010101" fill-opacity=".3">%s</text>`, labelWidth+valueWidth/2, value))
	s.WriteString(fmt.Sprintf(`<text x="%d" y="14" fill="%s">%s</text>`, labelWidth+valueWidth/2, valueTextColor, value))
	s.WriteString(`</g>`)

	s.WriteString(`</svg>`)
	return s.String()
}

// fontFaceCSS returns a CSS @font-face rule with the font embedded as base64.
func fontFaceCSS(name string, data []byte) string {
	format := detectFontFormat(data)
	encoded := base64.StdEncoding.EncodeToString(data)
	return fmt.Sprintf(
		`@font-face{font-family:'%s';src:url(data:font/%s;base64,%s) format('%s')}`,
		name, format, encoded, formatName(format),
	)
}

// detectFontFormat checks the first 4 bytes to determine TTF vs OTF.
func detectFontFormat(data []byte) string {
	if len(data) < 4 {
		return "ttf"
	}
	// OTF magic: "OTTO" (0x4F54544F)
	if data[0] == 0x4F && data[1] == 0x54 && data[2] == 0x54 && data[3] == 0x4F {
		return "otf"
	}
	return "ttf"
}

// formatName returns the CSS font format string for the given short format.
func formatName(format string) string {
	if format == "otf" {
		return "opentype"
	}
	return "truetype"
}

// xmlEscape escapes special XML characters in badge text.
func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
