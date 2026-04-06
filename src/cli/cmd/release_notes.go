package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/release"
)

var (
	rnFrom            string
	rnTo              string
	rnSecuritySummary string
	rnOutputFile      string
)

var releaseNotesCmd = &cobra.Command{
	Use:   "notes",
	Short: "Generate release notes from conventional commits",
	Long: `Generate markdown release notes from the git log between two refs.

Parses conventional commits (feat, fix, chore, etc.) and groups them
by category. Optionally embeds a security scan summary.

If --from is omitted, finds the previous tag automatically.
If --to is omitted, defaults to HEAD.`,
	RunE: runReleaseNotes,
}

func init() {
	releaseNotesCmd.Flags().StringVar(&rnFrom, "from", "", "start ref (default: previous tag)")
	releaseNotesCmd.Flags().StringVar(&rnTo, "to", "", "end ref (default: HEAD)")
	releaseNotesCmd.Flags().StringVar(&rnSecuritySummary, "security-summary", "", "path to security summary markdown to embed")
	releaseNotesCmd.Flags().StringVarP(&rnOutputFile, "output", "o", "", "write notes to file (default: stdout)")

	releaseCmd.AddCommand(releaseNotesCmd)
}

func runReleaseNotes(cmd *cobra.Command, args []string) error {
	rootDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	if len(args) > 0 {
		rootDir = args[0]
	}

	// Collect release tag patterns from versioning tag sources
	var tagPatterns []string
	for _, ts := range cfg.Versioning.TagSources {
		tagPatterns = append(tagPatterns, ts.Pattern)
	}

	input := release.NotesInput{
		RepoDir:     rootDir,
		FromRef:     rnFrom,
		ToRef:       rnTo,
		TagPatterns: tagPatterns,
		Config:      cfg,
	}

	// Load security summary from file if provided
	if rnSecuritySummary != "" {
		data, err := os.ReadFile(rnSecuritySummary)
		if err != nil {
			return fmt.Errorf("reading security summary: %w", err)
		}
		content := strings.TrimSpace(string(data))
		if content != "" {
			// First line is the tile, full content is the body
			parts := strings.SplitN(content, "\n", 2)
			input.SecurityTile = strings.TrimSpace(parts[0])
			input.SecurityBody = content
		}
	}

	notes, err := release.GenerateNotes(input)
	if err != nil {
		return fmt.Errorf("generating notes: %w", err)
	}

	if rnOutputFile != "" {
		if err := os.WriteFile(rnOutputFile, []byte(notes), 0o644); err != nil {
			return fmt.Errorf("writing notes: %w", err)
		}
		fmt.Printf("  release notes → %s\n", rnOutputFile)
		return nil
	}

	fmt.Print(notes)
	return nil
}
