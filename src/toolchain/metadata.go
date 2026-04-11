package toolchain

import "time"

const metadataFile = ".metadata.json"

// Metadata is the provenance record for a cached toolchain install.
// Immutable after write. If missing or checksum mismatch, the install
// is treated as corrupt and skipped (or re-downloaded to a writable root).
type Metadata struct {
	Tool      string `json:"tool"`
	Version   string `json:"version"`
	Platform  string `json:"platform"`
	SourceURL string `json:"source_url"`

	// SHA256 is the checksum of the downloaded archive (provenance).
	// Verified against official release checksums at download time.
	SHA256 string `json:"sha256"`

	// BinSHA256 is the checksum of the extracted binary (cache validation).
	// Used on cache hit to verify the binary hasn't been tampered with.
	BinSHA256 string `json:"bin_sha256"`

	InstalledAt string `json:"installed_at"`
	InstalledBy string `json:"installed_by"`
}

// StampMetadata sets the install timestamp and author.
func StampMetadata(m *Metadata) {
	m.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	m.InstalledBy = "stagefreight/toolchain"
}
