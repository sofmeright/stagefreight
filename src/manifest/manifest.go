// Package manifest defines the StageFreight manifest schema and deterministic
// JSON serialization. The manifest is the normalized data bus between raw
// evidence (Dockerfile, SBOM, scans) and rendering consumers (narrator, inspect, diff).
package manifest

import (
	"bytes"
	"encoding/json"
	"sort"
)

// Manifest is the top-level StageFreight manifest (schema_version: 1).
type Manifest struct {
	SchemaVersion int      `json:"schema_version"`
	Kind          string   `json:"kind"`
	Metadata      Metadata `json:"metadata"`
	Repo          Repo     `json:"repo"`
	Scope         Scope    `json:"scope"`
	Release       *Release `json:"release,omitempty"`
	Build         Build    `json:"build"`
	Targets       []Target `json:"targets"`
	Image         Image    `json:"image"`
	Completeness  Complete `json:"completeness"`
	Inventories   Invs     `json:"inventories"`
	Security      Security `json:"security"`
}

// Metadata holds generation metadata.
type Metadata struct {
	GeneratedAt string `json:"generated_at,omitempty"` // omitted in commit/workspace modes
	Generator   string `json:"generator"`
	State       string `json:"state"` // "prebuild" or "postbuild"
	Mode        string `json:"mode"`
}

// Repo holds git repository metadata.
type Repo struct {
	URL           string `json:"url"`
	DefaultBranch string `json:"default_branch"`
	Commit        string `json:"commit"`
	Dirty         bool   `json:"dirty"`
}

// Scope identifies the manifest scope.
type Scope struct {
	Name     string    `json:"name"`
	BuildID  string    `json:"build_id"`
	Platform *Platform `json:"platform,omitempty"`
}

// Platform describes the target platform.
type Platform struct {
	OS      string  `json:"os"`
	Arch    string  `json:"arch"`
	Variant *string `json:"variant"`
}

// Release holds version/tag metadata.
type Release struct {
	Version string `json:"version"`
	Tag     string `json:"tag"`
}

// Build holds build configuration.
type Build struct {
	ConfigPath string            `json:"config_path"`
	BuildID    string            `json:"build_id"`
	Dockerfile string            `json:"dockerfile"`
	Context    string            `json:"context"`
	Target     *string           `json:"target"`
	BaseImage  string            `json:"base_image"`
	Args       map[string]string `json:"args"`
}

// Target holds distribution target metadata.
type Target struct {
	ID            string   `json:"id"`
	Kind          string   `json:"kind"`
	Provider      string   `json:"provider,omitempty"`
	URL           string   `json:"url"`
	Path          string   `json:"path"`
	Tags          []string `json:"tags,omitempty"`
	CredentialRef string   `json:"credential_ref,omitempty"`
}

// Image holds built image metadata.
type Image struct {
	Refs         []string `json:"refs"`
	Digest       *string  `json:"digest"`
	ConfigDigest *string  `json:"config_digest"`
}

// Complete tracks which data categories are populated.
type Complete struct {
	ImageMeta    bool `json:"image_metadata"`
	SecurityMeta bool `json:"security_metadata"`
	SBOMImported bool `json:"sbom_imported"`
}

// Invs groups inventory items by manager.
type Invs struct {
	Versions []InvItem `json:"versions"`
	Apk      []InvItem `json:"apk"`
	Apt      []InvItem `json:"apt"`
	Pip      []InvItem `json:"pip"`
	Galaxy   []InvItem `json:"galaxy"`
	Npm      []InvItem `json:"npm"`
	Go       []InvItem `json:"go"`
	Binaries []InvItem `json:"binaries"`
}

// InvItem represents a single inventory entry.
type InvItem struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Pinned     bool   `json:"pinned"`
	Source     string `json:"source"`
	SourceRef  string `json:"source_ref"`
	Manager    string `json:"manager"`
	Confidence string `json:"confidence,omitempty"`
	URL        string `json:"url,omitempty"`
}

// Security holds security-related metadata.
type Security struct {
	SBOM       SBOMInfo    `json:"sbom"`
	Signatures []SigInfo   `json:"signatures"`
	Scans      []ScanInfo  `json:"scans"`
}

// SBOMInfo describes SBOM availability.
type SBOMInfo struct {
	Present bool    `json:"present"`
	Format  *string `json:"format"`
	Path    *string `json:"path"`
	Digest  *string `json:"digest"`
}

// SigInfo describes an image signature.
type SigInfo struct {
	Tool   string `json:"tool"`
	KeyRef string `json:"key_ref,omitempty"`
}

// ScanInfo describes a security scan result.
type ScanInfo struct {
	Tool string `json:"tool"`
	Path string `json:"path"`
}

// MarshalDeterministic produces canonical JSON output: sorted map keys,
// two-space indentation, trailing newline. Identical inputs → byte-identical output.
func MarshalDeterministic(m *Manifest) ([]byte, error) {
	// Sort map keys in Build.Args
	if m.Build.Args == nil {
		m.Build.Args = map[string]string{}
	}

	// Sort inventory arrays
	sortInvItems(m.Inventories.Versions)
	sortInvItems(m.Inventories.Apk)
	sortInvItems(m.Inventories.Apt)
	sortInvItems(m.Inventories.Pip)
	sortInvItems(m.Inventories.Galaxy)
	sortInvItems(m.Inventories.Npm)
	sortInvItems(m.Inventories.Go)
	sortInvItems(m.Inventories.Binaries)

	// Sort scans and signatures
	sort.Slice(m.Security.Scans, func(i, j int) bool {
		if m.Security.Scans[i].Tool != m.Security.Scans[j].Tool {
			return m.Security.Scans[i].Tool < m.Security.Scans[j].Tool
		}
		return m.Security.Scans[i].Path < m.Security.Scans[j].Path
	})
	sort.Slice(m.Security.Signatures, func(i, j int) bool {
		return m.Security.Signatures[i].Tool < m.Security.Signatures[j].Tool
	})

	// Ensure empty arrays are present (never null)
	ensureArrays(m)

	// Use ordered map encoding for Build.Args to get sorted keys
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}

	// Replace the "args" object with sorted keys version
	data = sortJSONMapKeys(data)

	// Ensure trailing newline
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}

	return data, nil
}

func sortInvItems(items []InvItem) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].Version < items[j].Version
	})
}

func ensureArrays(m *Manifest) {
	if m.Targets == nil {
		m.Targets = []Target{}
	}
	if m.Image.Refs == nil {
		m.Image.Refs = []string{}
	}
	if m.Inventories.Versions == nil {
		m.Inventories.Versions = []InvItem{}
	}
	if m.Inventories.Apk == nil {
		m.Inventories.Apk = []InvItem{}
	}
	if m.Inventories.Apt == nil {
		m.Inventories.Apt = []InvItem{}
	}
	if m.Inventories.Pip == nil {
		m.Inventories.Pip = []InvItem{}
	}
	if m.Inventories.Galaxy == nil {
		m.Inventories.Galaxy = []InvItem{}
	}
	if m.Inventories.Npm == nil {
		m.Inventories.Npm = []InvItem{}
	}
	if m.Inventories.Go == nil {
		m.Inventories.Go = []InvItem{}
	}
	if m.Inventories.Binaries == nil {
		m.Inventories.Binaries = []InvItem{}
	}
	if m.Security.Signatures == nil {
		m.Security.Signatures = []SigInfo{}
	}
	if m.Security.Scans == nil {
		m.Security.Scans = []ScanInfo{}
	}
}

// sortJSONMapKeys re-encodes any JSON object's keys in sorted order.
// Go's encoding/json already sorts map keys as of Go 1.12, but we explicitly
// ensure it for determinism across Go versions.
func sortJSONMapKeys(data []byte) []byte {
	var raw interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return data // fallback: return unsorted
	}

	sorted, err := marshalSorted(raw)
	if err != nil {
		return data
	}

	// Re-indent
	var buf bytes.Buffer
	if err := json.Indent(&buf, sorted, "", "  "); err != nil {
		return data
	}

	return buf.Bytes()
}

// marshalSorted produces compact JSON with sorted map keys.
func marshalSorted(v interface{}) ([]byte, error) {
	switch val := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			keyBytes, _ := json.Marshal(k)
			buf.Write(keyBytes)
			buf.WriteByte(':')
			valBytes, err := marshalSorted(val[k])
			if err != nil {
				return nil, err
			}
			buf.Write(valBytes)
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil

	case []interface{}:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			itemBytes, err := marshalSorted(item)
			if err != nil {
				return nil, err
			}
			buf.Write(itemBytes)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil

	default:
		return json.Marshal(v)
	}
}
