package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/release"
)

var (
	tagBumpPatch bool
	tagBumpMinor bool
	tagBumpMajor bool
	tagTarget    string
	tagFrom      string
	tagPush      bool
	tagDryRun    bool
	tagMessage   string
	tagYes       bool
	tagJSON      bool
)

var tagCmd = &cobra.Command{
	Use:   "tag [version]",
	Short: "Plan, validate, and create a release tag",
	Long: `Release tag planner with policy enforcement, semantic highlights,
and interactive approval.

Modes:
  stagefreight tag v0.5.0        Explicit version
  stagefreight tag --patch       Bump from previous release
  stagefreight tag --minor
  stagefreight tag --major
  stagefreight tag               Interactive selection (TTY only)

The tag is validated against versioning.tags before creation.
Highlights are generated from the glossary pipeline or prompted
when in interactive mode.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runTag,
}

func init() {
	tagCmd.Flags().BoolVar(&tagBumpPatch, "patch", false, "bump patch from previous release")
	tagCmd.Flags().BoolVar(&tagBumpMinor, "minor", false, "bump minor from previous release")
	tagCmd.Flags().BoolVar(&tagBumpMajor, "major", false, "bump major from previous release")
	tagCmd.Flags().StringVar(&tagTarget, "target", "", "ref to tag (default: HEAD)")
	tagCmd.Flags().StringVar(&tagFrom, "from", "", "override previous release boundary")
	tagCmd.Flags().BoolVar(&tagPush, "push", false, "push tag to origin after creation")
	tagCmd.Flags().BoolVar(&tagDryRun, "dry-run", false, "preview only, do not create tag")
	tagCmd.Flags().StringVarP(&tagMessage, "message", "m", "", "override tag message")
	tagCmd.Flags().BoolVarP(&tagYes, "yes", "y", false, "skip approval prompt")
	tagCmd.Flags().BoolVar(&tagJSON, "json", false, "output plan as JSON (implies --dry-run)")

	rootCmd.AddCommand(tagCmd)
}

func runTag(cmd *cobra.Command, args []string) error {
	start := time.Now()
	rootDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	// Resolve intent
	explicitVersion := ""
	if len(args) > 0 {
		explicitVersion = args[0]
	}

	bumpKind := ""
	bumpCount := 0
	if tagBumpPatch {
		bumpKind = "patch"
		bumpCount++
	}
	if tagBumpMinor {
		bumpKind = "minor"
		bumpCount++
	}
	if tagBumpMajor {
		bumpKind = "major"
		bumpCount++
	}

	if bumpCount > 1 {
		return fmt.Errorf("only one of --patch, --minor, --major may be specified")
	}
	if explicitVersion != "" && bumpKind != "" {
		return fmt.Errorf("cannot use explicit version and --patch/--minor/--major together")
	}

	// Non-TTY requires explicit version or bump
	isTTY := output.IsTTY()
	if !isTTY && explicitVersion == "" && bumpKind == "" {
		return fmt.Errorf("non-interactive mode requires explicit version or --patch/--minor/--major")
	}

	// Interactive version selection
	if explicitVersion == "" && bumpKind == "" && isTTY {
		selected, err := interactiveVersionSelect(rootDir)
		if err != nil {
			return err
		}
		if selected.bump != "" {
			bumpKind = selected.bump
		} else {
			explicitVersion = selected.version
		}
	}

	// Collect tag patterns from versioning tag sources
	var tagPatterns []string
	for _, ts := range cfg.Versioning.TagSources {
		tagPatterns = append(tagPatterns, ts.Pattern)
	}

	// Resolve target
	target := tagTarget
	if target == "" {
		target = cfg.Tag.Defaults.Target
	}
	if target == "" {
		target = "HEAD"
	}

	// Build plan
	plan, err := release.BuildTagPlan(rootDir, release.BuildTagPlanOptions{
		ExplicitVersion: explicitVersion,
		BumpKind:        bumpKind,
		TargetRef:       target,
		FromRef:         tagFrom,
		MessageOverride: tagMessage,
		TagPatterns:     tagPatterns,
		Glossary:        cfg.Glossary,
		Presentation:    cfg.Presentation.Tag,
	})
	if err != nil {
		return err
	}

	// Validate tag against policy
	if plan.NextTag != "" && len(tagPatterns) > 0 {
		matched := false
		for _, p := range tagPatterns {
			if config.MatchPatterns([]string{p}, plan.NextTag) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("tag %q does not match any release tag pattern in versioning.tags", plan.NextTag)
		}
	}

	// Handle message prompting
	if plan.Message == "" && tagMessage == "" {
		mode := cfg.Tag.Message.Mode
		if mode == "" {
			if isTTY {
				mode = "prompt_if_missing"
			} else {
				mode = "auto"
			}
		}

		switch mode {
		case "prompt_if_missing":
			if isTTY {
				msg, err := promptForMessage()
				if err != nil {
					return err
				}
				plan.Message = msg
			}
			// non-TTY with prompt_if_missing: auto-generated message stays (may be empty)
		case "require_manual":
			return fmt.Errorf("tag message required (use -m or provide interactively)")
		case "auto":
			// already generated, accept whatever we got
		}

		// Final empty check
		if plan.Message == "" {
			strategy := cfg.Tag.Message.EmptyStrategy
			if strategy == "" {
				strategy = cfg.Glossary.Render.EmptyStrategy
			}
			switch strategy {
			case "fail":
				return fmt.Errorf("no highlights could be generated and no message provided")
			case "prompt":
				if isTTY {
					msg, err := promptForMessage()
					if err != nil {
						return err
					}
					plan.Message = msg
				} else {
					return fmt.Errorf("no highlights generated — provide -m in non-interactive mode")
				}
			case "allow_empty":
				plan.Message = "- Maintenance release"
			}
		}
	}

	// JSON output
	if tagJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(plan)
	}

	// Preview
	elapsed := time.Since(start)
	w := os.Stdout
	color := output.UseColor()
	sec := output.NewSection(w, "Tag Plan", elapsed, color)

	sec.Row("%-16s%s", "target ref", plan.TargetRef)
	sec.Row("%-16s%s", "target sha", shortSHA(plan.TargetSHA))
	if plan.PreviousTag != "" {
		sec.Row("%-16s%s", "previous", plan.PreviousTag)
	}
	sec.Row("%-16s%s", "next tag", plan.NextTag)
	if plan.CommitCount > 0 {
		sec.Row("%-16s%d commits, %d files, %d(+), %d(-)",
			"changes", plan.CommitCount, plan.FilesChanged, plan.Insertions, plan.Deletions)
	}
	sec.Separator()
	fmt.Fprintln(w)
	fmt.Fprintln(w, plan.Message)
	fmt.Fprintln(w)
	sec.Close()

	if tagDryRun {
		return nil
	}

	// Approval
	if !tagYes && cfg.Tag.Defaults.RequireApproval && isTTY {
		choice, err := promptApproval(tagPush || cfg.Tag.Defaults.Push)
		if err != nil {
			return err
		}
		switch choice {
		case "reject":
			fmt.Println("  tag rejected")
			return nil
		case "approve_push":
			tagPush = true
		case "approve":
			// continue without push
		}
	}

	// Create tag
	if err := release.CreateAnnotatedTag(rootDir, plan.NextTag, plan.TargetSHA, plan.Message); err != nil {
		return err
	}
	fmt.Printf("  tag %s created on %s\n", plan.NextTag, shortSHA(plan.TargetSHA))

	// Push
	if tagPush || cfg.Tag.Defaults.Push {
		if err := release.PushTag(rootDir, "origin", plan.NextTag); err != nil {
			return err
		}
		fmt.Printf("  pushed %s to origin\n", plan.NextTag)
	}

	return nil
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

type versionSelection struct {
	version string
	bump    string
}

func interactiveVersionSelect(repoDir string) (*versionSelection, error) {
	// Find previous tag for context
	var tagPatterns []string
	for _, ts := range cfg.Versioning.TagSources {
		tagPatterns = append(tagPatterns, ts.Pattern)
	}
	prev, _ := release.PreviousReleaseTag(repoDir, "HEAD", tagPatterns)

	if prev != "" {
		fmt.Printf("  Previous release: %s\n\n", prev)

		patch, _ := release.BumpVersion(prev, "patch")
		minor, _ := release.BumpVersion(prev, "minor")
		major, _ := release.BumpVersion(prev, "major")

		fmt.Printf("  1. %s  (patch)\n", patch)
		fmt.Printf("  2. %s  (minor)\n", minor)
		fmt.Printf("  3. %s  (major)\n", major)
		fmt.Println("  4. Enter custom version")
		fmt.Println("  5. Cancel")
		fmt.Print("\n  > ")

		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return nil, fmt.Errorf("cancelled")
		}
		choice := strings.TrimSpace(scanner.Text())

		switch choice {
		case "1":
			return &versionSelection{bump: "patch"}, nil
		case "2":
			return &versionSelection{bump: "minor"}, nil
		case "3":
			return &versionSelection{bump: "major"}, nil
		case "4":
			fmt.Print("  Version: ")
			if !scanner.Scan() {
				return nil, fmt.Errorf("cancelled")
			}
			return &versionSelection{version: strings.TrimSpace(scanner.Text())}, nil
		default:
			return nil, fmt.Errorf("cancelled")
		}
	}

	// No previous tag — ask for initial version
	fmt.Println("  No previous release tag found.")
	fmt.Print("  Enter initial version: ")
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return nil, fmt.Errorf("cancelled")
	}
	return &versionSelection{version: strings.TrimSpace(scanner.Text())}, nil
}

func promptForMessage() (string, error) {
	fmt.Println("  Enter bulleted highlights for tag message.")
	fmt.Println("  Start each line with '- '. Finish with empty line:")
	fmt.Println()

	scanner := bufio.NewScanner(os.Stdin)
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			break
		}
		if !strings.HasPrefix(line, "- ") {
			line = "- " + line
		}
		lines = append(lines, line)
	}

	if len(lines) == 0 {
		return "", fmt.Errorf("no message provided")
	}
	return strings.Join(lines, "\n"), nil
}

func promptApproval(pushRequested bool) (string, error) {
	if pushRequested {
		fmt.Println("  1. Approve and push tag")
		fmt.Println("  2. Reject and exit")
	} else {
		fmt.Println("  1. Approve tag")
		fmt.Println("  2. Approve and push tag")
		fmt.Println("  3. Reject and exit")
	}
	fmt.Print("\n  > ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return "reject", nil
	}
	choice := strings.TrimSpace(scanner.Text())

	if pushRequested {
		switch choice {
		case "1":
			return "approve_push", nil
		default:
			return "reject", nil
		}
	}

	switch choice {
	case "1":
		return "approve", nil
	case "2":
		return "approve_push", nil
	default:
		return "reject", nil
	}
}
