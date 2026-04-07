package layout_test

import (
	"strings"
	"testing"

	"github.com/PrPlanIT/StageFreight/src/output/layout"
)

// ── VisualWidth ──────────────────────────────────────────────────────────────

func TestVisualWidth_PlainASCII(t *testing.T) {
	if got := layout.VisualWidth("hello"); got != 5 {
		t.Errorf("got %d, want 5", got)
	}
}

func TestVisualWidth_ANSIStripped(t *testing.T) {
	// "\033[32m✓\033[0m" — ANSI codes have zero width; ✓ is 1 wide.
	colored := "\033[32m✓\033[0m"
	if got := layout.VisualWidth(colored); got != 1 {
		t.Errorf("got %d, want 1 (ANSI codes must not contribute width)", got)
	}
}

func TestVisualWidth_EmojiWide(t *testing.T) {
	// 📋 is a wide emoji: 2 terminal columns.
	if got := layout.VisualWidth("📋"); got != 2 {
		t.Errorf("got %d, want 2 (emoji must be measured at true terminal width)", got)
	}
}

func TestVisualWidth_MixedANSIAndEmoji(t *testing.T) {
	// "active 📋 \033[32m✓\033[0m" = 7 + 2 + 1 + 1 = 11
	// "active " = 7, "📋" = 2, " " = 1, "✓" (inside ANSI) = 1 → total 11
	s := "active 📋 \033[32m✓\033[0m"
	if got := layout.VisualWidth(s); got != 11 {
		t.Errorf("got %d, want 11", got)
	}
}

func TestVisualWidth_EmptyString(t *testing.T) {
	if got := layout.VisualWidth(""); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

// ── DetectValueIndent ────────────────────────────────────────────────────────

func TestDetectValueIndent_StandardKeyValue(t *testing.T) {
	// "%-16s%s" style: key padded to 16, then value.
	line := "active          builds 📋   versioning 📋"
	if got := layout.DetectValueIndent(line); got != 16 {
		t.Errorf("got %d, want 16", got)
	}
}

func TestDetectValueIndent_NarrowKey(t *testing.T) {
	// "source   /src/.stagefreight.yml" — key 6 chars + 3 spaces gap = indent 9
	line := "source   /src/.stagefreight.yml"
	if got := layout.DetectValueIndent(line); got != 9 {
		t.Errorf("got %d, want 9", got)
	}
}

func TestDetectValueIndent_NoGap(t *testing.T) {
	// Single-space separation doesn't count as a padding gap → returns 0.
	line := "key value more value"
	if got := layout.DetectValueIndent(line); got != 0 {
		t.Errorf("got %d, want 0 (single-space gap must not be detected as indent)", got)
	}
}

func TestDetectValueIndent_ANSIInKey(t *testing.T) {
	// ANSI codes in the value should not affect indent detection.
	line := "resolution      \033[32mok ✓\033[0m"
	if got := layout.DetectValueIndent(line); got != 16 {
		t.Errorf("got %d, want 16", got)
	}
}

func TestDetectValueIndent_Empty(t *testing.T) {
	if got := layout.DetectValueIndent(""); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

// ── WrapContent ──────────────────────────────────────────────────────────────

func TestWrapContent_FitsWithinBudget(t *testing.T) {
	line := "source          /src/.stagefreight.yml"
	result := layout.WrapContent(line, 80)
	if len(result) != 1 {
		t.Fatalf("got %d lines, want 1 (line fits in budget)", len(result))
	}
	if result[0] != line {
		t.Errorf("got %q, want original line unchanged", result[0])
	}
}

func TestWrapContent_WrapsAtWordBoundary(t *testing.T) {
	// 16-char key + space-separated tokens; budget forces a wrap.
	line := "active          alpha   beta   gamma   delta   epsilon   zeta   eta"
	// Budget of 40 forces a wrap somewhere in the token list.
	result := layout.WrapContent(line, 40)
	if len(result) < 2 {
		t.Fatalf("got %d lines, want ≥2 (line must wrap)", len(result))
	}
	// Word-boundary wraps are CLEAN — no "..." decoration.
	for i, l := range result {
		if strings.HasSuffix(l, " ...") || strings.HasSuffix(l, "...") {
			t.Errorf("line %d %q must not have ellipsis — word-boundary wrap is clean", i, l)
		}
	}
}

func TestWrapContent_ContinuationIndentAligned(t *testing.T) {
	line := "active          alpha   beta   gamma   delta   epsilon   zeta"
	result := layout.WrapContent(line, 40)
	if len(result) < 2 {
		t.Skip("line did not wrap at budget 40")
	}
	// Continuation must be indented to column 16 (key width).
	cont := result[1]
	leadingSpaces := len(cont) - len(strings.TrimLeft(cont, " "))
	if leadingSpaces != 16 {
		t.Errorf("continuation leading spaces = %d, want 16 (value column)", leadingSpaces)
	}
}

func TestWrapContent_ANSITransparent(t *testing.T) {
	// ANSI codes must not be counted toward visual width for wrap decisions.
	// A line that is visually 30 chars but has long ANSI codes should not wrap at 40.
	ansiIcon := "\033[32m✓\033[0m"
	line := "source          /src/.stagefreight.yml " + ansiIcon
	// Visual width: 16 + 22 + 1 + 1 = 40. Should fit in budget 40.
	result := layout.WrapContent(line, 40)
	if len(result) != 1 {
		t.Errorf("ANSI codes inflated visual width: got %d lines, want 1", len(result))
	}
}

func TestWrapContent_EmojiWidth(t *testing.T) {
	// Each 📋 is 2 wide. A row with many emoji should wrap correctly.
	line := "active          alpha 📋   beta 📋   gamma 📋   delta 📋   epsilon 📋"
	result := layout.WrapContent(line, 50)
	// Should produce multiple lines, each within budget visually.
	for i, l := range result {
		w := layout.VisualWidth(l)
		if w > 50 {
			t.Errorf("line %d visual width %d exceeds budget 50: %q", i, w, l)
		}
	}
}

func TestWrapContent_NoWordBoundary_UsesEllipsis(t *testing.T) {
	// A single unbroken token with no spaces — no word boundary anywhere.
	// Hard cut: first piece ends with "...", remainder continues on next line.
	line := "superlongsingletokenwithnospaces"
	result := layout.WrapContent(line, 15)
	if len(result) < 2 {
		t.Fatalf("got %d lines, want ≥2 (token must hard-cut)", len(result))
	}
	// Hard-cut line must end with "..."
	if !strings.HasSuffix(result[0], "...") {
		t.Errorf("hard-cut line %q must end with '...'", result[0])
	}
	// All content must be present across lines (no data dropped).
	var all strings.Builder
	for _, l := range result {
		all.WriteString(strings.TrimSuffix(l, "..."))
	}
	if !strings.Contains(all.String(), "superlong") {
		t.Error("content missing from hard-cut output")
	}
}

func TestWrapContent_MultipleWraps(t *testing.T) {
	// Very long row that needs 3+ lines — word boundary wraps, clean output.
	tokens := make([]string, 20)
	for i := range tokens {
		tokens[i] = "token"
	}
	line := "active          " + strings.Join(tokens, "   ")
	result := layout.WrapContent(line, 40)
	if len(result) < 3 {
		t.Fatalf("expected ≥3 wrapped lines for very long row, got %d", len(result))
	}
	// No line should have ellipsis — all wraps are at word boundaries.
	for i, l := range result {
		if strings.Contains(l, "...") {
			t.Errorf("line %d %q must not contain ellipsis (word-boundary wrap)", i, l)
		}
	}
}

func TestWrapContent_ExactBudget(t *testing.T) {
	// Line whose visual width exactly equals budget — no wrap needed.
	line := strings.Repeat("a", 40)
	result := layout.WrapContent(line, 40)
	if len(result) != 1 {
		t.Errorf("line at exact budget should not wrap: got %d lines", len(result))
	}
}
