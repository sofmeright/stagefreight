// Package termutil detects terminal constraints for output layout.
// It owns one thing: translating a writer into a content width budget.
package termutil

import (
	"io"
	"os"
	"strconv"

	"golang.org/x/term"

	"github.com/PrPlanIT/StageFreight/src/output/layout"
)

// ContentWidth returns the usable content width for rows written to w.
//
// Detection order:
//  1. Terminal size from w if it is an *os.File connected to a TTY.
//  2. $COLUMNS env var — set by most shells, survives pipes and subshells.
//  3. layout.DefaultContentWidth — safe fallback for CI pipes and file output.
//
// Width is derived from w itself first so it behaves correctly when output is
// redirected or captured, while $COLUMNS provides the actual terminal hint
// when the writer is a pipe (e.g. `stagefreight ci run docs | tee log.txt`).
func ContentWidth(w io.Writer) int {
	// 1. Try the writer's file descriptor directly.
	if f, ok := w.(*os.File); ok {
		if width, _, err := term.GetSize(int(f.Fd())); err == nil && width >= 40 {
			return clamp(width - layout.FramePrefix)
		}
	}

	// 2. $COLUMNS — shells export this; useful when stdout is piped.
	if cols := os.Getenv("COLUMNS"); cols != "" {
		if width, err := strconv.Atoi(cols); err == nil && width >= 40 {
			return clamp(width - layout.FramePrefix)
		}
	}

	// 3. Fallback.
	return layout.DefaultContentWidth
}

func clamp(budget int) int {
	if budget < layout.DefaultContentWidth {
		return layout.DefaultContentWidth
	}
	return budget
}
