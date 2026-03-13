package diag

import (
	"fmt"
	"os"
)

// Warn writes a warning diagnostic to stderr.
// Use for non-fatal degradation the user should know about.
func Warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// Info writes an informational diagnostic to stderr.
// Use for notable fallback paths that succeeded but via a secondary method.
func Info(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// Debug writes a verbose trace to stderr when enabled.
// Use for exec traces, internal state, fallback reasoning.
func Debug(verbose bool, format string, args ...any) {
	if !verbose {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
