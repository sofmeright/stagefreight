package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/security"
)

var (
	secScanImage      string
	secScanOutputDir  string
	secScanSBOM       bool
	secScanFailCrit   bool
	secScanSkip       bool
	secScanDetail     string
	secScanStrict     bool
)

var securityScanCmd = &cobra.Command{
	Use:   "scan",
	Short: "Run vulnerability scan and generate SBOM",
	Long: `Scan a container image for vulnerabilities using Trivy and Grype,
then deduplicate results and optionally generate SBOM artifacts using Syft.

Individual scanners can be toggled via security.scanners in .stagefreight.yml.
Results are written to the output directory as JSON, SARIF, and SBOM files.
A markdown summary is generated at the configured detail level for embedding
in release notes.`,
	RunE: runSecurityScan,
}

func init() {
	securityScanCmd.Flags().StringVar(&secScanImage, "image", "", "image reference or tarball to scan (required)")
	securityScanCmd.Flags().StringVarP(&secScanOutputDir, "output", "o", "", "output directory for artifacts (default: from config)")
	securityScanCmd.Flags().BoolVar(&secScanSBOM, "sbom", true, "generate SBOM artifacts")
	securityScanCmd.Flags().BoolVar(&secScanFailCrit, "fail-on-critical", false, "exit non-zero if critical vulnerabilities found")
	securityScanCmd.Flags().BoolVar(&secScanSkip, "skip", false, "skip scan (for pipeline control)")
	securityScanCmd.Flags().StringVar(&secScanDetail, "security-detail", "", "override detail level for summary: none, counts, detailed, full")
	securityScanCmd.Flags().BoolVar(&secScanStrict, "strict", false,
		"fail if scan is partial, target lacks digest identity, or artifact verification fails")

	securityCmd.AddCommand(securityScanCmd)
}

// resolveTarget determines the scan target with full provenance tracking.
func resolveTarget(explicitImage string, positionalArgs []string) (security.ScanTarget, error) {
	// Priority 1: explicit --image flag
	if explicitImage != "" {
		stability := security.ClassifyRefStability(explicitImage, "")
		if stability == security.StabilityTag {
			fmt.Fprintf(os.Stderr, "security: explicit target %s is a tag reference — mutable, no digest guarantee\n", explicitImage)
		}
		return security.ScanTarget{
			Ref:             explicitImage,
			Source:          security.TargetExplicit,
			SelectionReason: "explicit --image flag",
			Stability:       stability,
		}, nil
	}

	// Priority 2: positional argument
	if len(positionalArgs) > 0 {
		ref := positionalArgs[0]
		stability := security.ClassifyRefStability(ref, "")
		if stability == security.StabilityTag {
			fmt.Fprintf(os.Stderr, "security: positional target %s is a tag reference — mutable, no digest guarantee\n", ref)
		}
		return security.ScanTarget{
			Ref:             ref,
			Source:          security.TargetPositionalArg,
			SelectionReason: "positional argument",
			Stability:       stability,
		}, nil
	}

	// Priority 3: auto-resolve from publish manifest
	rootDir, _ := os.Getwd()
	manifest, err := build.ReadPublishManifest(rootDir)
	if err != nil || len(manifest.Published) == 0 {
		return security.ScanTarget{}, fmt.Errorf("--image is required (or pass image ref as argument, or run after docker build)")
	}

	// Build candidate list
	var candidates []security.CandidateInfo
	for _, img := range manifest.Published {
		candidates = append(candidates, security.CandidateInfo{
			Ref:               img.Ref,
			Digest:            img.Digest,
			ObservedDigest:    img.ObservedDigest,
			ObservedDigestAlt: img.ObservedDigestAlt,
			Stability:         security.ClassifyRefStability(img.Ref, img.Digest),
		})
	}

	// Selection rules:
	// 1. Prefer candidates with digest (StabilityDigest or StabilityTagWithDigest)
	// 2. Among digest-resolved, prefer those where Digest == ObservedDigest
	// 3. Among bare tags, prefer first by manifest order
	var selected *build.PublishedImage
	var reason string

	// Find first digest-resolved candidate
	for i, img := range manifest.Published {
		if img.Digest != "" {
			selected = &manifest.Published[i]
			reason = fmt.Sprintf("first digest-resolved candidate (%d candidates)", len(candidates))
			break
		}
	}

	// Fallback to first candidate (bare tag)
	if selected == nil {
		selected = &manifest.Published[0]
		reason = fmt.Sprintf("first candidate by manifest order (%d candidates, all bare tags)", len(candidates))
	}

	// Build the execution ref — if we have a digest, use digest ref for scanning
	execRef := selected.Ref
	stability := security.ClassifyRefStability(selected.Ref, selected.Digest)
	if selected.Digest != "" {
		// Use digest ref for the actual scan execution
		repo := selected.Host + "/" + selected.Path
		execRef = repo + "@" + selected.Digest
		stability = security.StabilityDigest
	}

	// Check digest vs observed digest
	var digestMatch *bool
	if selected.Digest != "" && selected.ObservedDigest != "" {
		match := selected.Digest == selected.ObservedDigest
		digestMatch = &match
		if !match {
			fmt.Fprintf(os.Stderr, "security: registry propagation lag detected: expected %s, registry served %s\n",
				selected.Digest, selected.ObservedDigest)
		}
	}

	// Log candidate selection with resolution strength
	if selected.Digest != "" {
		fmt.Fprintf(os.Stderr, "security: resolved immutable scan target from publish manifest\n")
	} else {
		fmt.Fprintf(os.Stderr, "security: falling back to mutable tag target from publish manifest\n")
	}
	for _, c := range candidates {
		marker := "   "
		if c.Ref == selected.Ref {
			marker = " → "
		}
		fmt.Fprintf(os.Stderr, "%s%s (%s)\n", marker, c.Ref, c.Stability)
	}
	fmt.Fprintf(os.Stderr, "  selected: %s\n", reason)

	return security.ScanTarget{
		Ref:               execRef,
		DiscoveredTag:     selected.Tag,
		Digest:            selected.Digest,
		ObservedDigest:    selected.ObservedDigest,
		ObservedDigestAlt: selected.ObservedDigestAlt,
		DigestMatch:       digestMatch,
		Source:            security.TargetPublishManifest,
		SelectionReason:   reason,
		Stability:         stability,
		Candidates:        candidates,
		ExpectedTags:      selected.ExpectedTags,
		ExpectedCommit:    selected.ExpectedCommit,
		SigningAttempted:   selected.SigningAttempted,
	}, nil
}

func runSecurityScan(cmd *cobra.Command, args []string) error {
	if secScanSkip {
		fmt.Println("  security scan skipped")
		return nil
	}

	target, err := resolveTarget(secScanImage, args)
	if err != nil {
		return err
	}
	imageRef := target.Ref

	// Merge CLI flags with config defaults
	scanCfg := security.ScanConfig{
		Enabled:        !secScanSkip,
		TrivyEnabled:   cfg.Security.Scanners.TrivyEnabled(),
		GrypeEnabled:   cfg.Security.Scanners.GrypeEnabled(),
		SBOMEnabled:    secScanSBOM,
		FailOnCritical: secScanFailCrit || cfg.Security.FailOnCritical,
		ImageRef:       imageRef,
		OutputDir:      secScanOutputDir,
	}
	if scanCfg.OutputDir == "" {
		scanCfg.OutputDir = cfg.Security.OutputDir
	}

	// Ensure output directory exists
	if err := os.MkdirAll(scanCfg.OutputDir, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	color := output.UseColor()
	w := os.Stdout

	ctx := context.Background()

	scanCfg.SectionWriter = os.Stderr

	start := time.Now()
	result, err := security.Scan(ctx, scanCfg)
	elapsed := time.Since(start)

	if err != nil {
		return fmt.Errorf("security scan: %w", err)
	}

	// Set target on result
	result.Target = target

	// Run verification if target has a digest
	var verifyResult *security.VerificationResult
	if target.Digest != "" {
		verifyResult = security.Verify(ctx, security.VerifyOpts{
			ExpectedDigest:    target.Digest,
			ActualRef:         target.Ref,
			ActualTag:         target.DiscoveredTag,
			ObservedDigest:    target.ObservedDigest,
			ObservedDigestAlt: target.ObservedDigestAlt,
			ExpectedTags:      target.ExpectedTags,
			ExpectedCommit:    target.ExpectedCommit,
			SigningAttempted:   target.SigningAttempted,
		})
	}

	// Collect artifacts
	artifacts := append([]string{}, result.Artifacts...)

	// Resolve detail level from rules (CLI override > tag/branch rules > default)
	detail := security.ResolveDetailLevel(cfg.Security, secScanDetail, cfg.Policies)

	// Build and write summary
	_, summaryBody := security.BuildSummary(result, detail)
	var summaryPath string
	if summaryBody != "" {
		summaryPath = scanCfg.OutputDir + "/summary.md"
		if wErr := os.WriteFile(summaryPath, []byte(summaryBody), 0o644); wErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write summary: %v\n", wErr)
			summaryPath = ""
		} else {
			artifacts = append(artifacts, fmt.Sprintf("%s (detail: %s)", summaryPath, detail))
		}
	}

	// Determine status
	var status, statusDetail string
	switch result.Status {
	case "passed":
		status = "success"
		statusDetail = "passed"
	case "warning":
		status = "skipped" // yellow icon
		statusDetail = fmt.Sprintf("%d high vulnerabilities", result.High)
	case "critical":
		status = "failed"
		statusDetail = fmt.Sprintf("%d critical vulnerabilities", result.Critical)
	default:
		status = "success"
		statusDetail = result.Status
	}

	// Build SecurityUX from config + env overrides + defaults.
	ux := buildSecurityUX(cfg.Security.OverwhelmMessage, cfg.Security.OverwhelmLink)

	// ── Security Scan section ──
	output.SectionStart(w, "sf_security", "Security Scan")
	sec := output.NewSection(w, "Security Scan", elapsed, color)

	sec.Row("%-16s%s", "image", imageRef)
	sec.Row("%-16s%s", "source", target.Source)
	sec.Row("%-16s%s", "selection", target.SelectionReason)
	sec.Row("%-16s%s", "stability", stabilityLabel(target.Stability))

	if target.Digest != "" {
		sec.Row("%-16s%s", "digest", target.Digest)
		if target.ObservedDigest != "" {
			if target.DigestMatch != nil && *target.DigestMatch {
				sec.Row("%-16s%s (match)", "observed", target.ObservedDigest)
			} else if target.DigestMatch != nil && !*target.DigestMatch {
				sec.Row("%-16s%s \u26a0 mismatch", "observed", target.ObservedDigest)
			} else {
				sec.Row("%-16s%s", "observed", target.ObservedDigest)
			}
		}
	}

	if verifyResult != nil {
		sec.Row("%-16s%s", "verification", security.ConfidenceLabel(verifyResult.Confidence))
		for _, f := range verifyResult.Failures {
			sec.Row("%-16s%s", "", "\u26a0 "+f)
		}
	}

	output.ScanAuditRows(sec, output.ScanAudit{
		Engine: result.EngineVersion,
		OS:     result.OS,
	})

	// Scanner tracking
	if len(result.ScannersRun) > 0 {
		var scannerNames []string
		for _, s := range result.ScannersRun {
			if s.Version != "" {
				scannerNames = append(scannerNames, s.Name+" "+s.Version)
			} else {
				scannerNames = append(scannerNames, s.Name)
			}
		}
		sec.Row("%-16s%s", "scanners", strings.Join(scannerNames, ", "))
	}
	if len(result.ScannersFailed) > 0 {
		var failedNames []string
		for _, s := range result.ScannersFailed {
			if s.Version != "" {
				failedNames = append(failedNames, s.Name+" "+s.Version)
			} else {
				failedNames = append(failedNames, s.Name)
			}
		}
		sec.Row("%-16s%s", "failed", strings.Join(failedNames, ", "))
		sec.Row("%-16s\u26a0 scan incomplete \u2014 %d scanner(s) failed; results may under-report", "", len(result.ScannersFailed))
	}

	// Vuln table gated on detail level.
	switch detail {
	case "none":
		// skip entirely
	case "counts":
		total := result.Critical + result.High + result.Medium + result.Low
		if total > 0 {
			sec.Row("")
			sec.Row("%-16s%d total (%d critical, %d high, %d medium, %d low)",
				"vulnerabilities", total, result.Critical, result.High, result.Medium, result.Low)
		}
	case "detailed":
		vulnRows := toVulnRows(result.Vulnerabilities)
		output.SectionVulns(sec, vulnRows, color, output.SoftBudget, ux)
	case "full":
		vulnRows := toVulnRows(result.Vulnerabilities)
		output.SectionVulns(sec, vulnRows, color, output.HardBudget, ux)
	default:
		// unrecognized → treat as counts
		total := result.Critical + result.High + result.Medium + result.Low
		if total > 0 {
			sec.Row("")
			sec.Row("%-16s%d total (%d critical, %d high, %d medium, %d low)",
				"vulnerabilities", total, result.Critical, result.High, result.Medium, result.Low)
		}
	}

	sec.Separator()
	output.RowStatus(sec, "status", statusDetail, status, color)
	sec.Separator()

	for _, a := range artifacts {
		sec.Row("artifact  %s", a)
	}

	sec.Close()
	output.SectionEnd(w, "sf_security")

	// Print verbose summary to stdout
	if verbose && summaryBody != "" {
		fmt.Println()
		fmt.Print(summaryBody)
	}

	// Fail if configured and critical vulns found
	if scanCfg.FailOnCritical && result.Critical > 0 {
		return fmt.Errorf("security scan failed: %d critical vulnerabilities", result.Critical)
	}

	// Strict mode checks
	if secScanStrict && result.Partial {
		return fmt.Errorf("strict mode: scan is partial — %d scanner(s) failed", len(result.ScannersFailed))
	}
	if secScanStrict && result.Target.Stability == security.StabilityTag {
		return fmt.Errorf("strict mode: scan target is a bare tag reference — cannot guarantee artifact identity")
	}
	if secScanStrict && result.Target.DigestMatch != nil && !*result.Target.DigestMatch {
		return fmt.Errorf("strict mode: registry digest mismatch — expected %s, observed %s",
			result.Target.Digest, result.Target.ObservedDigest)
	}
	if verifyResult != nil {
		if secScanStrict && result.Target.SigningAttempted && verifyResult.SignatureValid == nil {
			return fmt.Errorf("strict mode: signing was configured but failed — artifact is unsigned despite key availability")
		}
		if secScanStrict && verifyResult.Confidence == security.ConfidenceNone {
			return fmt.Errorf("strict mode: artifact verification failed — confidence: none (%s)",
				strings.Join(verifyResult.Failures, "; "))
		}
	}

	return nil
}

// toVulnRows converts security.Vulnerability slice to output.VulnRow slice.
func toVulnRows(vulns []security.Vulnerability) []output.VulnRow {
	rows := make([]output.VulnRow, len(vulns))
	for i, v := range vulns {
		rows[i] = output.VulnRow{
			ID:        v.ID,
			Severity:  v.Severity,
			Package:   v.Package,
			Installed: v.Installed,
			FixedIn:   v.FixedIn,
			Title:     v.Description,
		}
	}
	return rows
}

// stabilityLabel returns a human-readable label for a ref stability level.
func stabilityLabel(s security.RefStability) string {
	switch s {
	case security.StabilityDigest:
		return "digest (content-addressed, immutable)"
	case security.StabilityTagWithDigest:
		return "tag_with_digest (resolved immutable instance)"
	case security.StabilityTag:
		return "tag (mutable — tag references can be repushed)"
	default:
		return string(s)
	}
}

// extractTagFromRef extracts the tag component from an image reference.
func extractTagFromRef(ref string) string {
	// Digest refs have no tag
	if strings.Contains(ref, "@") {
		return ""
	}
	if idx := strings.LastIndex(ref, ":"); idx >= 0 {
		slash := strings.LastIndex(ref, "/")
		if idx > slash {
			return ref[idx+1:]
		}
	}
	return ""
}

// buildSecurityUX resolves overwhelm message/link from config + env overrides + defaults.
func buildSecurityUX(cfgMessage []string, cfgLink string) output.SecurityUX {
	ux := output.SecurityUX{
		OverwhelmMessage: cfgMessage,
		OverwhelmLink:    cfgLink,
	}

	// Env overrides (LookupEnv — empty string = explicit disable).
	envMsg, envMsgSet := os.LookupEnv("STAGEFREIGHT_SECURITY_OVERWHELM_MESSAGE")
	if envMsgSet {
		if envMsg == "" {
			ux.OverwhelmMessage = nil
		} else {
			lines := strings.Split(envMsg, "\n")
			for i := range lines {
				lines[i] = strings.TrimRight(lines[i], "\r")
			}
			for len(lines) > 0 && lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
			ux.OverwhelmMessage = lines
		}
	}

	envLink, envLinkSet := os.LookupEnv("STAGEFREIGHT_SECURITY_OVERWHELM_LINK")
	if envLinkSet {
		ux.OverwhelmLink = envLink
	}

	// Defaults only when nothing configured AND nothing overridden.
	if !envMsgSet && !envLinkSet && len(cfgMessage) == 0 && cfgLink == "" {
		ux.OverwhelmMessage = output.DefaultOverwhelmMessage
		ux.OverwhelmLink = output.DefaultOverwhelmLink
	}

	return ux
}
