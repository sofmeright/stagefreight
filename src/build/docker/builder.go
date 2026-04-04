package docker

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/output"
)

// BuilderInfo holds structured builder state for narration.
type BuilderInfo struct {
	Name              string
	Driver            string
	Status            string // "running", "stopped", "not found"
	Action            string // "reused", "created"
	BuildKit          string // version
	Platforms         string
	Endpoint          string
	BootstrapOK       bool
	BootstrapDuration time.Duration
	GCRules           []GCRule
	RawOutput         string // fallback if parsing fails
	ParseFailed       bool
}

// GCRule is a parsed BuildKit garbage collection policy rule.
type GCRule struct {
	Scope        string // "source/cachemount/git", "general cache", etc.
	All          bool
	KeepDuration string
	MaxUsed      string
	Reserved     string
	MinFree      string
}

// ResolveBuilderInfo queries the active buildx builder, bootstraps it,
// and returns structured facts. Captures raw output as fallback.
func ResolveBuilderInfo() BuilderInfo {
	info := BuilderInfo{Name: "sf-builder"}

	// Bootstrap and capture output (suppress from CI log).
	bootstrapStart := time.Now()
	bootstrapOut, bootstrapErr := exec.Command("docker", "buildx", "inspect", "--bootstrap", "sf-builder").CombinedOutput()
	info.BootstrapDuration = time.Since(bootstrapStart)
	info.BootstrapOK = bootstrapErr == nil
	info.RawOutput = string(bootstrapOut)

	// Inspect for structured facts.
	out, err := exec.Command("docker", "buildx", "inspect", "sf-builder").CombinedOutput()
	if err != nil {
		info.Status = "not found"
		info.ParseFailed = true
		return info
	}

	text := string(out)
	var currentRule *GCRule
	foundDriver := false

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Driver:"):
			info.Driver = strings.TrimSpace(strings.TrimPrefix(line, "Driver:"))
			foundDriver = true
		case strings.HasPrefix(line, "Status:"):
			info.Status = strings.TrimSpace(strings.TrimPrefix(line, "Status:"))
		case strings.HasPrefix(line, "BuildKit version:"):
			info.BuildKit = strings.TrimSpace(strings.TrimPrefix(line, "BuildKit version:"))
		case strings.HasPrefix(line, "Platforms:"):
			info.Platforms = strings.TrimSpace(strings.TrimPrefix(line, "Platforms:"))
		case strings.HasPrefix(line, "Endpoint:"):
			info.Endpoint = strings.TrimSpace(strings.TrimPrefix(line, "Endpoint:"))
		case strings.HasPrefix(line, "GC Policy rule#"):
			if currentRule != nil {
				info.GCRules = append(info.GCRules, *currentRule)
			}
			currentRule = &GCRule{}
		case currentRule != nil && strings.HasPrefix(line, "All:"):
			currentRule.All = strings.TrimSpace(strings.TrimPrefix(line, "All:")) == "true"
		case currentRule != nil && strings.HasPrefix(line, "Filters:"):
			currentRule.Scope = strings.TrimSpace(strings.TrimPrefix(line, "Filters:"))
		case currentRule != nil && strings.HasPrefix(line, "Keep Duration:"):
			currentRule.KeepDuration = strings.TrimSpace(strings.TrimPrefix(line, "Keep Duration:"))
		case currentRule != nil && strings.HasPrefix(line, "Max Used Space:"):
			currentRule.MaxUsed = strings.TrimSpace(strings.TrimPrefix(line, "Max Used Space:"))
		case currentRule != nil && strings.HasPrefix(line, "Reserved Space:"):
			currentRule.Reserved = strings.TrimSpace(strings.TrimPrefix(line, "Reserved Space:"))
		case currentRule != nil && strings.HasPrefix(line, "Min Free Space:"):
			currentRule.MinFree = strings.TrimSpace(strings.TrimPrefix(line, "Min Free Space:"))
		}
	}
	if currentRule != nil {
		info.GCRules = append(info.GCRules, *currentRule)
	}

	// Read authoritative builder record from transport.
	// Skeleton writes structured JSON to .stagefreight/runtime/docker/builder.json.
	if recordBytes, err := os.ReadFile(".stagefreight/runtime/docker/builder.json"); err == nil {
		var record struct {
			Name     string `json:"name"`
			Action   string `json:"action"`
			Driver   string `json:"driver"`
			Endpoint string `json:"endpoint"`
		}
		if json.Unmarshal(recordBytes, &record) == nil {
			if record.Action == "created" || record.Action == "reused" {
				info.Action = record.Action
			}
			if record.Driver != "" {
				info.Driver = record.Driver
			}
			if record.Endpoint != "" {
				info.Endpoint = record.Endpoint
			}
		}
	}

	// Parse quality check — if critical fields are missing, mark as failed.
	if !foundDriver || info.Status == "" || info.Endpoint == "" {
		info.ParseFailed = true
	}

	return info
}

// RenderBuilderInfo prints structured builder state.
// Falls back to raw output if parsing failed.
func RenderBuilderInfo(w io.Writer, color bool, info BuilderInfo) {
	sec := output.NewSection(w, "Builder", info.BootstrapDuration, color)

	if info.ParseFailed {
		sec.Row("%-14s%s", "status", "parse failed — raw output below")
		if info.RawOutput != "" {
			for _, line := range strings.Split(strings.TrimSpace(info.RawOutput), "\n") {
				sec.Row("  %s", line)
			}
		}
		sec.Close()
		return
	}

	sec.Row("%-14s%s", "builder", info.Name)
	if info.Driver != "" {
		sec.Row("%-14s%s", "driver", info.Driver)
	}
	if info.Endpoint != "" {
		sec.Row("%-14s%s", "endpoint", info.Endpoint)
	}
	sec.Row("%-14s%s", "status", info.Status)
	if info.Action != "" {
		sec.Row("%-14s%s", "action", info.Action)
	}

	// Bootstrap result.
	if info.BootstrapOK {
		sec.Row("%-14s%s %s", "bootstrap", output.StatusIcon("success", color), formatDuration(info.BootstrapDuration))
	} else {
		sec.Row("%-14s%s failed", "bootstrap", output.StatusIcon("failed", color))
	}

	if info.BuildKit != "" {
		sec.Row("%-14s%s", "buildkit", info.BuildKit)
	}
	if info.Platforms != "" {
		sec.Row("%-14s%s", "platforms", info.Platforms)
	}

	// GC policy summary.
	if len(info.GCRules) > 0 {
		sec.Row("")
		sec.Row("gc policy")
		for _, rule := range info.GCRules {
			scope := rule.Scope
			if scope == "" {
				if rule.All {
					scope = "all (fallback)"
				} else {
					scope = "general cache"
				}
			} else {
				scope = strings.ReplaceAll(scope, "type==source.local,type==exec.cachemount,type==source.git.checkout", "source/cachemount/git")
			}
			parts := []string{}
			if rule.KeepDuration != "" {
				parts = append(parts, fmt.Sprintf("keep %s", rule.KeepDuration))
			}
			if rule.MaxUsed != "" {
				parts = append(parts, fmt.Sprintf("max %s", rule.MaxUsed))
			}
			if rule.MinFree != "" {
				parts = append(parts, fmt.Sprintf("min free %s", rule.MinFree))
			}
			sec.Row("  %-34s %s", scope, strings.Join(parts, "  "))
		}
	}

	sec.Close()
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
