package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/atomicfile"
)

var ErrPublishManifestNotFound = errors.New("publish manifest not found")
var ErrPublishManifestInvalid = errors.New("publish manifest invalid")

// BuildInstance captures CI/build environment metadata for provenance.
type BuildInstance struct {
	Commit     string `json:"commit,omitempty"`
	PipelineID string `json:"pipeline_id,omitempty"`
	JobID      string `json:"job_id,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
}

// AttestationType identifies the signing/attestation mechanism.
type AttestationType string

const (
	AttestationCosign AttestationType = "cosign"
	AttestationInToto AttestationType = "in-toto"
	AttestationSLSA   AttestationType = "slsa"
)

// AttestationRecord captures signing and attestation metadata for a published image.
type AttestationRecord struct {
	Type           AttestationType `json:"type,omitempty"`
	SignatureRef   string          `json:"signature_ref,omitempty"`   // cosign signature digest ref
	AttestationRef string          `json:"attestation_ref,omitempty"` // DSSE provenance digest ref
	SignerIdentity string          `json:"signer_identity,omitempty"` // workload identity / key fingerprint
	VerifiedDigest string          `json:"verified_digest,omitempty"` // digest the signature covers
}

// PublishedImage records a single image that was successfully pushed.
type PublishedImage struct {
	Host             string             `json:"host"`                                // normalized registry host
	Path             string             `json:"path"`                                // image path
	Tag              string             `json:"tag"`                                 // resolved tag
	Provider         string             `json:"provider"`                            // canonical provider name
	Ref              string             `json:"ref"`                                 // full image ref (host/path:tag)
	Digest           string             `json:"digest,omitempty"`                    // image digest (immutable truth)
	CredentialRef    string             `json:"credential_ref,omitempty"`            // non-secret env var prefix for OCI auth resolution
	BuildInstance    BuildInstance       `json:"build_instance,omitempty"`            // CI/build metadata
	Registry         string             `json:"registry,omitempty"`                  // registry hostname
	ObservedDigest   string             `json:"observed_digest,omitempty"`           // what the registry returned post-push
	ObservedDigestAlt string            `json:"observed_digest_alt,omitempty"`       // second observation via registry API
	ObservedBy       string             `json:"observed_by,omitempty"`               // primary observation method (e.g., "buildx")
	ObservedByAlt    string             `json:"observed_by_alt,omitempty"`           // alternate observation method (e.g., "registry_api")
	ExpectedTags     []string           `json:"expected_tags,omitempty"`             // all tags this digest was published under
	ExpectedCommit   string             `json:"expected_commit,omitempty"`           // commit this digest was built from
	Attestation      *AttestationRecord `json:"attestation,omitempty"`               // signing/attestation record (nil = absent)
	SigningAttempted bool               `json:"signing_attempted,omitempty"`         // true if signing was attempted but failed
}

// PublishedBinary records a single binary that was successfully built.
type PublishedBinary struct {
	Name      string `json:"name"`                // logical binary name
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Path      string `json:"path"`                // local binary path
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	BuildID   string `json:"build_id"`
	Version   string `json:"version,omitempty"`
	Commit    string `json:"commit,omitempty"`
	Toolchain string `json:"toolchain,omitempty"` // "go1.24.1" — for audit + crucible verification
}

// PublishedArchive records a single archive that was successfully created.
type PublishedArchive struct {
	Name     string          `json:"name"`              // archive filename
	Format   string          `json:"format"`            // tar.gz | zip
	Path     string          `json:"path"`              // local archive path
	Size     int64           `json:"size"`
	SHA256   string          `json:"sha256"`
	Contents []string        `json:"contents,omitempty"` // files in archive
	BuildID  string          `json:"build_id"`
	Binary   PublishedBinary `json:"binary"`
}

// PublishManifest records all artifacts successfully produced during a build.
// Self-verifying: the Checksum field covers the canonical JSON representation
// of the manifest with Checksum set to empty. Single file = single atomic write.
type PublishManifest struct {
	Published []PublishedImage   `json:"published"`
	Binaries  []PublishedBinary  `json:"binaries,omitempty"`
	Archives  []PublishedArchive `json:"archives,omitempty"`
	Timestamp string             `json:"timestamp"`           // RFC3339
	Checksum  string             `json:"checksum,omitempty"`  // SHA-256 of manifest with this field empty
}

const PublishManifestPath = ".stagefreight/publish.json"


// normalizeHost strips scheme prefixes and trailing slashes from a registry host.
func normalizeHost(h string) string {
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	h = strings.TrimSuffix(h, "/")
	return strings.ToLower(h)
}

// canonicalJSON marshals a value in the canonical format used for checksum computation.
// This function is the single definition of "canonical" for all publish manifest
// integrity operations. If you change the format here, all existing checksums break.
func canonicalJSON(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// WritePublishManifest writes the publish manifest with an embedded SHA-256 checksum.
// Canonicalizes Ref, deduplicates by host/path:tag, sorts deterministically,
// and sets timestamp if empty.
func WritePublishManifest(dir string, manifest PublishManifest) error {
	// Canonicalize Ref from components
	for i := range manifest.Published {
		img := &manifest.Published[i]
		img.Host = normalizeHost(img.Host)
		img.Ref = img.Host + "/" + img.Path + ":" + img.Tag
	}

	// Dedup by host/path:tag
	type imageKey struct{ host, path, tag string }
	seen := make(map[imageKey]int) // key → index in deduped
	var deduped []PublishedImage
	for _, img := range manifest.Published {
		k := imageKey{img.Host, img.Path, img.Tag}
		if idx, exists := seen[k]; exists {
			// Same digest = skip, different digest = error
			if img.Digest != "" && deduped[idx].Digest != "" && img.Digest != deduped[idx].Digest {
				return fmt.Errorf("conflicting digests for %s: %s vs %s", img.Ref, deduped[idx].Digest, img.Digest)
			}
			// Prefer the entry with a digest
			if img.Digest != "" && deduped[idx].Digest == "" {
				deduped[idx] = img
			}
			continue
		}
		seen[k] = len(deduped)
		deduped = append(deduped, img)
	}

	// Sort deterministically: host → path → tag
	sort.Slice(deduped, func(i, j int) bool {
		if deduped[i].Host != deduped[j].Host {
			return deduped[i].Host < deduped[j].Host
		}
		if deduped[i].Path != deduped[j].Path {
			return deduped[i].Path < deduped[j].Path
		}
		return deduped[i].Tag < deduped[j].Tag
	})

	manifest.Published = deduped

	// Set timestamp if empty
	if manifest.Timestamp == "" {
		manifest.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	// Compute embedded checksum: canonical marshal without checksum, hash, then set it.
	manifest.Checksum = ""
	canonical, err := canonicalJSON(manifest)
	if err != nil {
		return fmt.Errorf("marshaling publish manifest: %w", err)
	}
	hash := sha256.Sum256(canonical)
	manifest.Checksum = hex.EncodeToString(hash[:])

	// Final marshal with checksum embedded.
	data, err := canonicalJSON(manifest)
	if err != nil {
		return fmt.Errorf("marshaling publish manifest: %w", err)
	}
	data = append(data, '\n')

	// Single file, single atomic write. No sidecar, no pair atomicity problem.
	manifestPath := filepath.Join(dir, PublishManifestPath)
	if err := atomicfile.WriteFile(manifestPath, data, 0o644); err != nil {
		return fmt.Errorf("writing publish manifest: %w", err)
	}

	return nil
}

// ReadPublishManifest reads and validates the publish manifest.
// Supports two verification modes:
//   - Embedded checksum (current): Checksum field inside the JSON covers the
//     canonical representation with Checksum="". Single file, fully atomic.
//   - Legacy sidecar (backward compat): Separate .sha256 file. Used only if
//     the manifest has no embedded Checksum field.
func ReadPublishManifest(dir string) (*PublishManifest, error) {
	manifestPath := filepath.Join(dir, PublishManifestPath)

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrPublishManifestNotFound
		}
		return nil, fmt.Errorf("%w: reading manifest: %v", ErrPublishManifestInvalid, err)
	}

	// Parse JSON first to check for embedded checksum.
	var manifest PublishManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("%w: parsing manifest: %v", ErrPublishManifestInvalid, err)
	}

	if manifest.Checksum != "" {
		// Embedded checksum: verify by canonical re-marshal without checksum.
		expectedHex := manifest.Checksum
		manifest.Checksum = ""
		canonical, err := canonicalJSON(manifest)
		if err != nil {
			return nil, fmt.Errorf("%w: re-marshaling for verification: %v", ErrPublishManifestInvalid, err)
		}
		actualHash := sha256.Sum256(canonical)
		actualHex := hex.EncodeToString(actualHash[:])
		if actualHex != expectedHex {
			return nil, fmt.Errorf("%w: embedded checksum mismatch (expected %s, got %s)", ErrPublishManifestInvalid, expectedHex, actualHex)
		}
		manifest.Checksum = expectedHex
		return &manifest, nil
	}

	// Legacy: verify from sidecar .sha256 file.
	checksumPath := manifestPath + ".sha256"
	checksumData, err := os.ReadFile(checksumPath)
	if err != nil {
		return nil, fmt.Errorf("%w: no embedded checksum and no sidecar file: %v", ErrPublishManifestInvalid, err)
	}
	checksumStr := strings.TrimSpace(string(checksumData))
	parts := strings.SplitN(checksumStr, " ", 2)
	if len(parts) == 0 || parts[0] == "" {
		return nil, fmt.Errorf("%w: malformed checksum sidecar", ErrPublishManifestInvalid)
	}
	actualHash := sha256.Sum256(data)
	actualHex := hex.EncodeToString(actualHash[:])
	if actualHex != parts[0] {
		return nil, fmt.Errorf("%w: sidecar checksum mismatch (expected %s, got %s)", ErrPublishManifestInvalid, parts[0], actualHex)
	}

	return &manifest, nil
}
