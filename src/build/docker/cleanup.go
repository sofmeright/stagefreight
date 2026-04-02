package docker

import (
	"fmt"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/config"
)

// CleanupCommand is a single cleanup operation with its class context.
type CleanupCommand struct {
	Class   string // "images.dangling", "build_cache", "containers.exited", etc.
	Command string // raw docker command (no error suppression — executor handles that)
}

// CleanupResult records what happened during host hygiene.
type CleanupResult struct {
	Executed bool
	Results  []CleanupCommandResult
}

// CleanupCommandResult records the outcome of one cleanup command.
type CleanupCommandResult struct {
	Class   string
	Command string
	Output  string
	Error   error
}

// BuildCleanupCommands generates Docker cleanup commands from the prune policy.
// Commands are returned raw — no error suppression. The executor decides behavior
// based on enforcement mode (best_effort: continue + warn, required: fail immediately).
//
// Image protection uses a two-phase approach:
//   1. Resolve protected image IDs from ref patterns (docker images --filter=reference=...)
//   2. Prune with exclusion of resolved IDs
// This is because docker prune does not support ref-based exclusion natively.
func BuildCleanupCommands(cleanup config.HostCleanupConfig) []CleanupCommand {
	var commands []CleanupCommand

	prune := cleanup.Prune
	protectedRefs := cleanup.Protect.Images.Refs

	// If there are protected image refs, emit a resolve step first.
	// This sets a shell variable with image IDs to exclude.
	if len(protectedRefs) > 0 && (prune.Images.Unreferenced.OlderThan != "" || prune.Images.Dangling.OlderThan != "") {
		var filters []string
		for _, ref := range protectedRefs {
			filters = append(filters, fmt.Sprintf("--filter=reference=%q", ref))
		}
		resolveCmd := fmt.Sprintf(
			`SF_PROTECTED_IDS=$(docker images -q %s | sort -u | tr '\n' '|' | sed 's/|$//')`,
			strings.Join(filters, " "))
		commands = append(commands, CleanupCommand{
			Class:   "images.protect_resolve",
			Command: resolveCmd,
		})
	}

	// Images — dangling.
	if prune.Images.Dangling.OlderThan != "" {
		commands = append(commands, CleanupCommand{
			Class: "images.dangling",
			Command: fmt.Sprintf(`docker image prune -f --filter "until=%s" --filter "dangling=true"`,
				prune.Images.Dangling.OlderThan),
		})
	}

	// Images — unreferenced.
	// Uses selective removal: list candidates, exclude protected IDs, remove individually.
	if prune.Images.Unreferenced.OlderThan != "" {
		if len(protectedRefs) > 0 {
			// Two-phase: list then selectively remove, skipping protected.
			commands = append(commands, CleanupCommand{
				Class: "images.unreferenced",
				Command: fmt.Sprintf(
					`docker images -q --filter "dangling=false" --filter "until=%s" | while read id; do echo "$id" | grep -qvE "${SF_PROTECTED_IDS:-^$}" && docker rmi "$id"; done`,
					prune.Images.Unreferenced.OlderThan),
			})
		} else {
			commands = append(commands, CleanupCommand{
				Class: "images.unreferenced",
				Command: fmt.Sprintf(`docker image prune -af --filter "until=%s"`,
					prune.Images.Unreferenced.OlderThan),
			})
		}
	}

	// Build cache.
	if prune.BuildCache.OlderThan != "" || prune.BuildCache.KeepStorage != "" {
		parts := []string{"docker builder prune -f"}
		if prune.BuildCache.OlderThan != "" {
			parts = append(parts, fmt.Sprintf(`--filter "until=%s"`, prune.BuildCache.OlderThan))
		}
		if prune.BuildCache.KeepStorage != "" {
			parts = append(parts, fmt.Sprintf("--keep-storage %s", prune.BuildCache.KeepStorage))
		}
		commands = append(commands, CleanupCommand{
			Class:   "build_cache",
			Command: strings.Join(parts, " "),
		})
	}

	// Containers — exited.
	if prune.Containers.Exited.OlderThan != "" {
		commands = append(commands, CleanupCommand{
			Class: "containers.exited",
			Command: fmt.Sprintf(`docker container prune -f --filter "until=%s"`,
				prune.Containers.Exited.OlderThan),
		})
	}

	// Networks — unused.
	if prune.Networks.Unused {
		commands = append(commands, CleanupCommand{
			Class:   "networks.unused",
			Command: "docker network prune -f",
		})
	}

	return commands
}
