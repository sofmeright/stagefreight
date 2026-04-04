package cistate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/PrPlanIT/StageFreight/src/ci"
)

// StatePath is the workspace-relative path where pipeline state is persisted.
const StatePath = ".stagefreight/pipeline.json"

// State is the per-run ledger for the current pipeline workspace.
// Each subsystem records what it did; downstream stages read the ledger
// instead of probing files.
type State struct {
	Version    int              `json:"version"`
	CI         CIState          `json:"ci"`
	Build      BuildState       `json:"build"`
	Security   SecurityState    `json:"security"`
	Release    ReleaseState     `json:"release"`
	Subsystems []SubsystemState `json:"subsystems,omitempty"`
}

// SubsystemState is the generic lifecycle phase record.
// All subsystems register here regardless of mode. The resolver
// uses this list — never hardcoded field names.
type SubsystemState struct {
	Name         string `json:"name"`
	Attempted    bool   `json:"attempted"`
	Completed    bool   `json:"completed"`
	Skipped      bool   `json:"skipped"`
	AllowFailure bool   `json:"allow_failure"` // true = non-vital; failure produces warning, not fail
	Required     bool   `json:"required"`      // true = failure is a hard pipeline fail
	Outcome      string `json:"outcome"`       // success | failed | skipped | warning | not_applicable | cancelled
	Reason       string `json:"reason,omitempty"`
}

// PipelineStatus derives the aggregate pipeline outcome from all subsystems.
// States: passing, warning, failing, unknown.
//
// Resolution rules (platform-agnostic, policy-aware):
//   - Any required subsystem with outcome "failed" → failing
//   - Any non-required subsystem with outcome "failed" + allow_failure → warning
//   - Any subsystem with outcome "warning" → warning
//   - Nothing attempted → unknown
//   - Otherwise → passing
//
// If the generic Subsystems list is populated, uses that.
// Otherwise falls back to the typed fields for backward compatibility.
func (st *State) PipelineStatus() string {
	subs := st.Subsystems
	if len(subs) == 0 {
		subs = st.synthesizeSubsystems()
	}

	anyAttempted := false
	hasWarning := false

	for _, s := range subs {
		if !s.Attempted {
			continue
		}
		anyAttempted = true

		switch s.Outcome {
		case "failed":
			if s.AllowFailure {
				hasWarning = true
			} else {
				return "failing"
			}
		case "warning":
			hasWarning = true
		case "skipped":
			// Intentional skip is neutral — not a warning unless the subsystem was required.
			if s.Required {
				hasWarning = true
			}
		case "not_applicable":
			// Subsystem doesn't apply to this lifecycle mode. Always neutral.
		}
	}

	if !anyAttempted {
		return "unknown"
	}
	if hasWarning {
		return "warning"
	}
	return "passing"
}

// synthesizeSubsystems builds a generic list from the typed fields.
// Temporary bridge — once all runners call RecordSubsystem, this goes away.
func (st *State) synthesizeSubsystems() []SubsystemState {
	var subs []SubsystemState
	if st.Build.Attempted {
		subs = append(subs, SubsystemState{
			Name: "build", Attempted: true, Required: true,
			Completed: st.Build.Completed,
			Outcome:   outcomeFromBool(st.Build.Completed, false),
		})
	}
	if st.Security.Attempted {
		subs = append(subs, SubsystemState{
			Name: "security", Attempted: true, AllowFailure: true,
			Completed: st.Security.Completed, Skipped: st.Security.Skipped,
			Outcome: outcomeFromBool(st.Security.Completed, st.Security.Skipped),
		})
	}
	if st.Release.Attempted {
		subs = append(subs, SubsystemState{
			Name: "release", Attempted: true, AllowFailure: true,
			Completed: st.Release.Completed, Skipped: st.Release.Skipped,
			Outcome: outcomeFromBool(st.Release.Completed, st.Release.Skipped),
		})
	}
	return subs
}

func outcomeFromBool(completed, skipped bool) string {
	if skipped {
		return "skipped"
	}
	if completed {
		return "success"
	}
	return "failed"
}

// CIState captures the CI environment for this pipeline run.
type CIState struct {
	Provider   string `json:"provider"`
	PipelineID string `json:"pipeline_id"`
	Ref        string `json:"ref,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Tag        string `json:"tag,omitempty"`
	SHA        string `json:"sha"`
}

// BuildState records what the build subsystem did.
type BuildState struct {
	Attempted      bool   `json:"attempted"`
	Completed      bool   `json:"completed"`
	ProducedImages bool   `json:"produced_images"`
	PublishedCount int    `json:"published_count"`
	ManifestPath   string `json:"manifest_path,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// SecurityState records what the security subsystem did.
type SecurityState struct {
	Attempted bool   `json:"attempted"`
	Completed bool   `json:"completed"`
	Skipped   bool   `json:"skipped"`
	Reason    string `json:"reason,omitempty"`
}

// ReleaseState records what the release subsystem did.
type ReleaseState struct {
	Eligible  bool   `json:"eligible"`
	Attempted bool   `json:"attempted"`
	Completed bool   `json:"completed"`
	Skipped   bool   `json:"skipped"`
	Reason    string `json:"reason,omitempty"`
}

// ReadState reads pipeline state from the workspace. Returns a zero State
// (Version: 1) on missing file — missing state is normal when the first
// subsystem hasn't run yet. Only errors on I/O or parse failures for an
// existing file.
func ReadState(rootDir string) (*State, error) {
	p := filepath.Join(rootDir, StatePath)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{Version: 1}, nil
		}
		return nil, fmt.Errorf("reading pipeline state: %w", err)
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parsing pipeline state: %w", err)
	}
	return &st, nil
}

// WriteState writes pipeline state atomically (tmp + rename).
// Normalizes Version to 1 on write.
func WriteState(rootDir string, st *State) error {
	st.Version = 1

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling pipeline state: %w", err)
	}
	data = append(data, '\n')

	p := filepath.Join(rootDir, StatePath)
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating pipeline state dir: %w", err)
	}

	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing pipeline state tmp: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming pipeline state: %w", err)
	}
	return nil
}

// RecordSubsystem upserts a subsystem entry by name.
func (st *State) RecordSubsystem(name string, attempted, completed, skipped bool, reason string) {
	for i, s := range st.Subsystems {
		if s.Name == name {
			st.Subsystems[i] = SubsystemState{
				Name: name, Attempted: attempted,
				Completed: completed, Skipped: skipped, Reason: reason,
			}
			return
		}
	}
	st.Subsystems = append(st.Subsystems, SubsystemState{
		Name: name, Attempted: attempted,
		Completed: completed, Skipped: skipped, Reason: reason,
	})
}

// UpdateState does read-modify-write. The caller mutates individual fields
// only — never rebuild nested structs wholesale to avoid clobbering prior
// state written by other subsystems.
func UpdateState(rootDir string, fn func(*State)) error {
	st, err := ReadState(rootDir)
	if err != nil {
		return err
	}
	fn(st)
	return WriteState(rootDir, st)
}

// InitFromCI populates a CIState from a ci.CIContext.
func InitFromCI(ciCtx *ci.CIContext) CIState {
	ref := ciCtx.Branch
	if ref == "" {
		ref = ciCtx.Tag
	}
	return CIState{
		Provider:   ciCtx.Provider,
		PipelineID: ciCtx.PipelineID,
		Ref:        ref,
		Branch:     ciCtx.Branch,
		Tag:        ciCtx.Tag,
		SHA:        ciCtx.SHA,
	}
}
