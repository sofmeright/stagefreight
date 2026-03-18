package docker

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/build"
)

// layerState tracks in-progress state for a single buildx step.
type layerState struct {
	stage       string
	stageStep   string
	instruction string
	detail      string
	cached      bool
	done        bool
	seconds     float64
	image       string
}

// Regex patterns for buildx --progress=plain output.
var (
	// #N [stage M/N] INSTRUCTION args...
	layerStartRe = regexp.MustCompile(`^#(\d+) \[([^\]]*?) (\d+/\d+)\] (\w+)\s*(.*)`)
	// #N [internal] load build definition from Dockerfile (skip internal steps)
	internalRe = regexp.MustCompile(`^#\d+ \[internal\]`)
	// #N CACHED
	cachedRe = regexp.MustCompile(`^#(\d+) CACHED`)
	// #N DONE 44.8s
	doneRe = regexp.MustCompile(`^#(\d+) DONE (\d+\.?\d*)s`)
	// FROM image@sha256:... — extract image name
	fromImageRe = regexp.MustCompile(`FROM\s+(\S+?)(?:@sha256:[a-f0-9]+)?(?:\s+AS\s+\S+)?$`)
)

// ParseBuildxOutput parses captured buildx --progress=plain output into layer events.
// Only meaningful build layers are returned (FROM, COPY, RUN, etc.).
// Internal steps (load build definition, load .dockerignore, metadata) are filtered out.
func ParseBuildxOutput(output string) []build.LayerEvent {
	layers := make(map[int]*layerState)
	lines := strings.Split(output, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Skip internal steps
		if internalRe.MatchString(line) {
			continue
		}

		// Layer start: #N [stage M/N] INSTRUCTION args
		if m := layerStartRe.FindStringSubmatch(line); m != nil {
			stepNum, _ := strconv.Atoi(m[1])
			stage := m[2]
			stageStep := m[3]
			instruction := m[4]
			detail := m[5]

			// Extract base image name for FROM instructions
			var image string
			if instruction == "FROM" {
				if fm := fromImageRe.FindStringSubmatch(instruction + " " + detail); fm != nil {
					image = fm[1]
				}
			}

			// Truncate long details
			if len(detail) > 60 {
				detail = detail[:57] + "..."
			}

			layers[stepNum] = &layerState{
				stage:       stage,
				stageStep:   stageStep,
				instruction: instruction,
				detail:      detail,
				image:       image,
			}
			continue
		}

		// CACHED: #N CACHED
		if m := cachedRe.FindStringSubmatch(line); m != nil {
			stepNum, _ := strconv.Atoi(m[1])
			if ls, ok := layers[stepNum]; ok {
				ls.cached = true
				ls.done = true
			}
			continue
		}

		// DONE: #N DONE Ns
		if m := doneRe.FindStringSubmatch(line); m != nil {
			stepNum, _ := strconv.Atoi(m[1])
			seconds, _ := strconv.ParseFloat(m[2], 64)
			if ls, ok := layers[stepNum]; ok {
				ls.seconds = seconds
				ls.done = true
			}
			continue
		}
	}

	// Collect completed layers in step order.
	var events []build.LayerEvent
	// Find max step number to iterate in order.
	maxStep := 0
	for k := range layers {
		if k > maxStep {
			maxStep = k
		}
	}
	for i := 0; i <= maxStep; i++ {
		ls, ok := layers[i]
		if !ok || !ls.done {
			continue
		}
		// Skip exporting/writing steps
		if ls.instruction == "" {
			continue
		}

		event := build.LayerEvent{
			Stage:       ls.stage,
			StageStep:   ls.stageStep,
			Instruction: ls.instruction,
			Detail:      ls.detail,
			Cached:      ls.cached,
			Image:       ls.image,
		}
		if ls.seconds > 0 {
			event.Duration = time.Duration(ls.seconds * float64(time.Second))
		}
		events = append(events, event)
	}

	return events
}
