package k8s

import (
	"fmt"
	"strings"
)

const maxHostsDisplay = 3
const maxSourceLinks = 3

// statusIcon returns a status emoji for the summary table.
func statusIcon(s Status) string {
	switch s {
	case StatusHealthy:
		return "✅"
	case StatusDegraded:
		return "⚠️"
	case StatusDown:
		return "❌"
	default:
		return "❓"
	}
}

// exposureIcon returns an exposure indicator.
func exposureIcon(e ExposureLevel) string {
	switch e {
	case ExposureInternet:
		return "🌐"
	case ExposureIntranet:
		return "🔒"
	case ExposureCluster:
		return "🏠"
	default:
		return "❓"
	}
}

// RenderOverview produces stable, deterministic markdown from a DiscoveryResult.
// Two-layer UX: minimal summary table + expandable details per app.
// Docs are scanned, not read — instant orientation in <3 seconds.
func RenderOverview(result *DiscoveryResult, commitSHA string) string {
	var b strings.Builder

	// Provenance header
	b.WriteString("> Generated from live Kubernetes state\n")
	b.WriteString(fmt.Sprintf("> Cluster: %s | %s",
		result.Cluster,
		result.ObservedAt.UTC().Format("2006-01-02T15:04:05Z")))
	if commitSHA != "" {
		b.WriteString(fmt.Sprintf(" | %s", commitSHA))
	}
	b.WriteString("\n\n")

	// Status aggregation
	if len(result.Apps) > 0 {
		healthy, degraded, down := 0, 0, 0
		for _, r := range result.Apps {
			switch r.Status {
			case StatusHealthy:
				healthy++
			case StatusDegraded:
				degraded++
			case StatusDown:
				down++
			}
		}
		b.WriteString(fmt.Sprintf("## Apps & Services (%d)\n\n", len(result.Apps)))
		b.WriteString(fmt.Sprintf("✅ %d healthy", healthy))
		if degraded > 0 {
			b.WriteString(fmt.Sprintf(" · ⚠️ %d degraded", degraded))
		}
		if down > 0 {
			b.WriteString(fmt.Sprintf(" · ❌ %d down", down))
		}
		b.WriteString("\n\n")
		renderAppsByCategory(&b, result.Apps)
	}

	// Platform section
	if len(result.Platform) > 0 {
		b.WriteString(fmt.Sprintf("## Platform Components (%d)\n\n", len(result.Platform)))
		b.WriteString("| Component | Namespace | Status |\n")
		b.WriteString("| --- | --- | --- |\n")
		for _, r := range result.Platform {
			name := escMD(r.FriendlyName)
			if len(r.Components) > 1 {
				name = fmt.Sprintf("%s (%d components)", name, len(r.Components))
			}
			b.WriteString(fmt.Sprintf("| %s | %s | %s |\n",
				name,
				escMD(r.Key.Namespace),
				statusIcon(r.Status),
			))
		}
		b.WriteString("\n")

		// Platform details (collapsed)
		b.WriteString("<details>\n<summary>Platform details</summary>\n\n")
		for _, r := range result.Platform {
			b.WriteString(fmt.Sprintf("**%s** — %s — %s\n",
				escMD(r.FriendlyName),
				escMD(renderType(r.WorkloadKinds)),
				escMD(r.Version)))
			b.WriteString(fmt.Sprintf("- Namespace: %s\n", r.Key.Namespace))
			b.WriteString(fmt.Sprintf("- Replicas: %s\n\n", r.Replicas))
		}
		b.WriteString("</details>\n\n")
	}

	// Graveyard section
	if len(result.Graveyard) > 0 {
		b.WriteString(fmt.Sprintf("<details>\n<summary>Retired / Graveyard (%d)</summary>\n\n", len(result.Graveyard)))
		b.WriteString("| Name | Namespace | Reason |\n")
		b.WriteString("| --- | --- | --- |\n")
		for _, g := range result.Graveyard {
			b.WriteString(fmt.Sprintf("| %s | %s | %s |\n",
				escMD(g.Name),
				escMD(g.Namespace),
				escMD(g.Reason),
			))
		}
		b.WriteString("\n</details>\n")
	}

	return b.String()
}

// renderAppsByCategory groups apps and renders Layer 1 (summary table) + Layer 2 (details).
func renderAppsByCategory(b *strings.Builder, apps []AppRecord) {
	// Layer 1: Summary table per category
	var currentCat string
	for _, r := range apps {
		if r.Category != currentCat {
			if currentCat != "" {
				b.WriteString("\n")
			}
			currentCat = r.Category
			ns := r.Key.Namespace
			b.WriteString(fmt.Sprintf("### %s (%s)\n\n", currentCat, ns))
			b.WriteString("| App | Status | Exposure | Links |\n")
			b.WriteString("| --- | --- | --- | --- |\n")
		}

		// Exposure cell: icon + first host
		exposure := exposureIcon(r.Exposure.Declared)
		if len(r.Hosts) > 0 {
			exposure += " " + escMD(r.Hosts[0])
		}

		// Links cell: action-oriented, max 3
		links := renderSourceLinks(r)

		// App name with component count for multi-component families.
		appName := escMD(r.FriendlyName)
		if len(r.Components) > 1 {
			appName = fmt.Sprintf("%s (%d components)", appName, len(r.Components))
		}

		b.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n",
			appName,
			statusIcon(r.Status),
			exposure,
			links,
		))
	}

	if currentCat != "" {
		b.WriteString("\n")
	}

	// Layer 2: Details per app (collapsed)
	b.WriteString("<details>\n<summary>App details</summary>\n\n")
	for _, r := range apps {
		header := escMD(r.FriendlyName)
		if len(r.Components) > 1 {
			header = fmt.Sprintf("%s — %d components", header, len(r.Components))
		}
		b.WriteString(fmt.Sprintf("**%s** — %s — %s\n",
			header,
			escMD(renderType(r.WorkloadKinds)),
			escMD(r.Version)))
		b.WriteString(fmt.Sprintf("- Namespace: %s\n", r.Key.Namespace))
		b.WriteString(fmt.Sprintf("- Replicas: %s\n", r.Replicas))

		if len(r.Hosts) > 0 {
			b.WriteString(fmt.Sprintf("- Hosts: %s\n", strings.Join(r.Hosts, ", ")))
		}
		if r.Exposure.Gateway != "" {
			b.WriteString(fmt.Sprintf("- Gateway: %s\n", r.Exposure.Gateway))
		}
		if r.Description != "" {
			b.WriteString(fmt.Sprintf("- Description: %s\n", r.Description))
		}

		// Component breakdown for multi-component families.
		if len(r.Components) > 1 {
			b.WriteString("- Components:\n")
			for _, c := range r.Components {
				b.WriteString(fmt.Sprintf("  - %s (%s)\n", c.Name, c.Kind))
			}
		}

		// Full source breakdown
		if len(r.Sources) > 0 {
			b.WriteString("- Sources:\n")
			for _, src := range r.Sources {
				label := src.Relation
				if src.Primary {
					label += " (primary)"
				}
				b.WriteString(fmt.Sprintf("  - %s → `%s`\n", label, src.RepoPath))
			}
		}

		b.WriteString("\n")
	}
	b.WriteString("</details>\n")
}

// renderSourceLinks builds action-oriented link labels from DeclaredSources.
// Order: open → deploy → policy. Max 3 links.
func renderSourceLinks(r AppRecord) string {
	var links []string

	// [open] — first hostname (for any exposed app, not just internet).
	// Exposure icon communicates reachability; link enables navigation.
	if len(r.Hosts) > 0 {
		links = append(links, fmt.Sprintf("[open](https://%s)", r.Hosts[0]))
	}

	// Source links: deploy first, then policy, then others
	linkOrder := []string{SourceRelationDeploys, SourceRelationSecures, SourceRelationConfigures, SourceRelationDependsOn}
	for _, relation := range linkOrder {
		if len(links) >= maxSourceLinks {
			break
		}
		for _, src := range r.Sources {
			if src.Relation == relation {
				label := "deploy"
				switch relation {
				case SourceRelationSecures:
					label = "policy"
				case SourceRelationConfigures:
					label = "config"
				case SourceRelationDependsOn:
					label = "deps"
				}
				links = append(links, fmt.Sprintf("[%s](%s)", label, src.RepoPath))
				break // one per relation
			}
		}
	}

	if len(links) == 0 {
		return ""
	}
	return strings.Join(links, " · ")
}

// renderType produces a compact type string from workload kinds.
func renderType(kinds []string) string {
	if len(kinds) == 0 {
		return ""
	}
	if len(kinds) == 1 {
		return kinds[0]
	}
	return "Mixed"
}

// renderHosts produces a compact, truncated hostname list.
func renderHosts(hosts []string) string {
	if len(hosts) == 0 {
		return ""
	}
	if len(hosts) <= maxHostsDisplay {
		return strings.Join(hosts, ", ")
	}
	shown := hosts[:maxHostsDisplay]
	return strings.Join(shown, ", ") + fmt.Sprintf(", +%d more", len(hosts)-maxHostsDisplay)
}

// escMD escapes markdown table-breaking characters.
func escMD(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	return s
}
