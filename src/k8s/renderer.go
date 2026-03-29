package k8s

import (
	"fmt"
	"strings"
)

const maxHostsDisplay = 3

// RenderOverview produces stable, deterministic markdown from a DiscoveryResult.
// Output is placed between narrator markers by the caller.
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

	// Apps section
	if len(result.Apps) > 0 {
		b.WriteString(fmt.Sprintf("## Apps & Services (%d)\n\n", len(result.Apps)))
		renderAppsByCategory(&b, result.Apps)
	}

	// Platform section
	if len(result.Platform) > 0 {
		b.WriteString(fmt.Sprintf("## Platform Components (%d)\n\n", len(result.Platform)))
		b.WriteString("| Component | Namespace | Version | Status |\n")
		b.WriteString("| --- | --- | --- | --- |\n")
		for _, r := range result.Platform {
			b.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n",
				escMD(r.FriendlyName),
				escMD(r.Key.Namespace),
				escMD(r.Version),
				escMD(string(r.Status)),
			))
		}
		b.WriteString("\n")
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

// renderAppsByCategory groups apps by category and renders each as a table.
func renderAppsByCategory(b *strings.Builder, apps []AppRecord) {
	var currentCat string
	for _, r := range apps {
		if r.Category != currentCat {
			if currentCat != "" {
				b.WriteString("\n")
			}
			currentCat = r.Category
			ns := r.Key.Namespace
			b.WriteString(fmt.Sprintf("### %s (%s)\n\n", currentCat, ns))
			b.WriteString("| App | Type | Version | Hosts | Description |\n")
			b.WriteString("| --- | --- | --- | --- | --- |\n")
		}

		b.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s |\n",
			escMD(r.FriendlyName),
			escMD(renderType(r.WorkloadKinds)),
			escMD(r.Version),
			escMD(renderHosts(r.Hosts)),
			escMD(r.Description),
		))
	}
	if currentCat != "" {
		b.WriteString("\n")
	}
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
// Sorted before truncation for deterministic output.
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
