package build

import "strings"

// SlugifyBuildID sanitizes a build ID for use as a filename.
// Lowercase, [a-z0-9-] only, spaces/underscores → hyphens, slashes forbidden.
func SlugifyBuildID(id string) string {
	id = strings.ToLower(id)
	id = strings.ReplaceAll(id, " ", "-")
	id = strings.ReplaceAll(id, "_", "-")

	var b strings.Builder
	for _, c := range id {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		}
	}

	// Collapse multiple hyphens
	result := b.String()
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	return strings.Trim(result, "-")
}
