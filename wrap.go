// Package layout provides terminal-aware text layout primitives.
// It owns: visual width measurement, ANSI-transparent wrapping, and value
// column detection. It has no I/O and no terminal detection — those are the
// caller's responsibility.
package layout

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

const (
	// FramePrefix is the visible column width of the section row prefix "    │ ".
	// Used by callers computing a content budget from terminal width.
	FramePrefix = 6

	// DefaultContentWidth is the fallback content budget when terminal width
	// cannot be detected (CI pipes, file output, etc.).
	// 120 is a safe width for CI log viewers and wide terminals.
	DefaultContentWidth = 120
)

// WrapContent wraps line at word boundaries so each segment fits within budget
// visible columns. ANSI escape sequences are transparent (zero visual width);
// emoji and other wide characters are measured at their true terminal width.
//
// Wrap pattern:
//
//	first line:        "key   value value value"
//	continuation:      "      value value value"
//
// Continuation lines are indented to the value column — the position after the
// first padding gap (2+ spaces) that follows key content.
//
// The "..." ellipsis is used ONLY for hard mid-token cuts (no word boundary
// available). Word-boundary wraps are clean — no decoration.
func WrapContent(line string, budget int) []string {
	if VisualWidth(line) <= budget {
		return []string{line}
	}

	indent := DetectValueIndent(line)
	indentStr := strings.Repeat(" ", indent)

	var result []string
	remaining := []rune(line)
	first := true

	for len(remaining) > 0 {
		var cutBudget int
		var prefix string
		if first {
			cutBudget = budget
		} else {
			prefix = indentStr
			cutBudget = budget - indent
		}

		// Check whether the remainder fits on this line without cutting.
		remWidth := runeSliceWidth(remaining)
		fitBudget := cutBudget
		if !first {
			fitBudget = budget - indent
		}
		if remWidth <= fitBudget {
			result = append(result, prefix+string(remaining))
			break
		}

		// Try to find a word boundary.
		cut := findWordBoundary(remaining, cutBudget)

		if cut < 0 {
			// No word boundary — hard cut with ellipsis.
			hardCut := findHardCut(remaining, cutBudget-3) // reserve 3 for "..."
			piece := remaining[:hardCut]
			remaining = remaining[hardCut:]
			result = append(result, prefix+string(piece)+"...")
			if first {
				first = false
			}
		} else {
			// Clean word-boundary cut — no decoration.
			piece := remaining[:cut]
			remaining = []rune(strings.TrimLeft(string(remaining[cut:]), " "))
			result = append(result, prefix+string(piece))
			if first {
				first = false
			}
		}
	}

	return result
}

// VisualWidth returns the visible column width of s.
// ANSI escape sequences contribute zero width; wide characters (emoji, CJK)
// are counted at their actual terminal column width via go-runewidth.
func VisualWidth(s string) int {
	return runeSliceWidth([]rune(s))
}

// DetectValueIndent returns the column position where the value starts in a
// formatted row string — immediately after the first padding gap of 2+ spaces
// that follows non-space content. Used to align continuation lines.
//
// For "key             value..." this returns the position of 'v' (e.g. 16).
// Returns 0 if no gap is found.
func DetectValueIndent(line string) int {
	runes := stripANSI([]rune(line))
	inContent := false
	spaceStart := -1
	for i, r := range runes {
		if r != ' ' {
			if inContent && spaceStart >= 0 && i-spaceStart >= 2 {
				return runewidth.StringWidth(string(runes[:i]))
			}
			spaceStart = -1
			inContent = true
		} else if inContent && spaceStart < 0 {
			spaceStart = i
		}
	}
	return 0
}

// findWordBoundary returns the rune index of the last space at or before
// maxVisual visible columns, skipping ANSI escape sequences.
// Returns -1 if no word boundary exists within the budget.
func findWordBoundary(runes []rune, maxVisual int) int {
	visual := 0
	lastSpace := -1
	i := 0
	for i < len(runes) {
		if isANSIStart(runes, i) {
			i = skipANSI(runes, i)
			continue
		}
		w := runewidth.RuneWidth(runes[i])
		if visual+w > maxVisual {
			break
		}
		if runes[i] == ' ' {
			lastSpace = i
		}
		visual += w
		i++
	}
	return lastSpace
}

// findHardCut returns the rune index at which to hard-cut so the piece fits
// within maxVisual columns. Used only when no word boundary exists.
func findHardCut(runes []rune, maxVisual int) int {
	visual := 0
	i := 0
	for i < len(runes) {
		if isANSIStart(runes, i) {
			i = skipANSI(runes, i)
			continue
		}
		w := runewidth.RuneWidth(runes[i])
		if visual+w > maxVisual {
			break
		}
		visual += w
		i++
	}
	if i == 0 {
		return 1 // always advance at least one rune
	}
	return i
}

func runeSliceWidth(runes []rune) int {
	width := 0
	i := 0
	for i < len(runes) {
		if isANSIStart(runes, i) {
			i = skipANSI(runes, i)
			continue
		}
		width += runewidth.RuneWidth(runes[i])
		i++
	}
	return width
}

// stripANSI removes ANSI escape sequences from a rune slice.
func stripANSI(runes []rune) []rune {
	var out []rune
	i := 0
	for i < len(runes) {
		if isANSIStart(runes, i) {
			i = skipANSI(runes, i)
			continue
		}
		out = append(out, runes[i])
		i++
	}
	return out
}

func isANSIStart(runes []rune, i int) bool {
	return runes[i] == '\033' && i+1 < len(runes) && runes[i+1] == '['
}

func skipANSI(runes []rune, i int) int {
	i += 2 // skip ESC [
	for i < len(runes) && runes[i] != 'm' {
		i++
	}
	if i < len(runes) {
		i++ // skip 'm'
	}
	return i
}
