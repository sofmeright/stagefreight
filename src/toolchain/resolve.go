// Package toolchain provides a governed execution substrate for external tools.
//
// StageFreight owns its toolchains. Every tool used is resolved, downloaded,
// verified, cached, and reported. No silent host fallback. No DinD. No
// containers-for-tools. No environment luck.
//
// Contract properties:
//   - Immutable installs — once cached, a version directory is never mutated
//   - Checksum verification required — every download verified against official checksums
//   - Explicit provenance — .metadata.json records source URL, checksum, install time
//   - Deterministic resolution — same version = same binary, always
//   - No silent host fallback — system binaries in PATH are not used
//   - Hard failure on verification miss — checksum mismatch = error, not warning
package toolchain

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Result is the outcome of a toolchain resolution. Every field is populated.
// Callers use Path for execution and can report provenance from the rest.
type Result struct {
	Tool      string // "go"
	Version   string // "1.26.1"
	Path      string // absolute path to binary
	CacheHit  bool   // true if served from cache, false if downloaded
	SourceURL string // where it was (or would be) fetched from
	SHA256    string // verified archive checksum (provenance)
	BinSHA256 string // extracted binary checksum (cache validation)
}

// Resolve ensures a tool at the requested version is available and verified.
// Returns the binary path and provenance. Downloads if not cached.
// Hard-fails on checksum mismatch, download error, or metadata write failure.
// No fallback. No stderr output — callers own presentation.
func Resolve(rootDir, tool, version string) (Result, error) {
	switch tool {
	case "go":
		return resolveGo(rootDir, version)
	default:
		return Result{}, fmt.Errorf("unsupported toolchain %q", tool)
	}
}

// resolveGo ensures a Go toolchain is cached and verified.
func resolveGo(rootDir, version string) (Result, error) {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	sourceURL := goDownloadURL(version, goos, goarch)

	// Search all read roots for a valid cached install.
	// Persistent mount is checked first (operator-preseeded or prior install),
	// then workspace-local. Read-only caches are valid — we only need the binary.
	for _, root := range ReadRoots(rootDir) {
		binPath := CacheBinPathIn(root, "go", version, "go")
		if _, err := os.Stat(binPath); err != nil {
			continue
		}
		meta, metaErr := readMetadataFrom(root, "go", version)
		if metaErr != nil || meta.BinSHA256 == "" {
			continue // no metadata — skip this root, don't delete (may be read-only)
		}
		actual, hashErr := fileSHA256(binPath)
		if hashErr != nil || actual != meta.BinSHA256 {
			continue // corrupt — skip, try next root
		}
		return Result{
			Tool:      "go",
			Version:   version,
			Path:      binPath,
			CacheHit:  true,
			SourceURL: meta.SourceURL,
			SHA256:    meta.SHA256,
			BinSHA256: meta.BinSHA256,
		}, nil
	}

	// No valid cache hit — download and install.
	// Write to the preferred writable root (persistent if available).
	installRoot := InstallRoot(rootDir)

	expectedSHA, err := fetchGoChecksum(version, goos, goarch)
	if err != nil {
		return Result{}, fmt.Errorf("toolchain go %s: fetching checksum: %w", version, err)
	}

	archivePath, err := downloadToTemp(sourceURL)
	if err != nil {
		return Result{}, fmt.Errorf("toolchain go %s: download failed: %w", version, err)
	}
	defer os.Remove(archivePath)

	// Verify archive checksum BEFORE extraction
	archiveSHA, err := fileSHA256(archivePath)
	if err != nil {
		return Result{}, fmt.Errorf("toolchain go %s: checksum computation failed: %w", version, err)
	}
	if archiveSHA != expectedSHA {
		return Result{}, fmt.Errorf("toolchain go %s: archive checksum mismatch\n  expected: %s\n  actual:   %s\n  source:   %s", version, expectedSHA, archiveSHA, sourceURL)
	}

	// Extract
	destDir := CacheDirIn(installRoot, "go", version)
	if err := extractGoArchive(archivePath, destDir); err != nil {
		os.RemoveAll(destDir)
		return Result{}, fmt.Errorf("toolchain go %s: extraction failed: %w", version, err)
	}

	binPath := CacheBinPathIn(installRoot, "go", version, "go")
	if _, err := os.Stat(binPath); err != nil {
		os.RemoveAll(destDir)
		return Result{}, fmt.Errorf("toolchain go %s: binary not found after extraction at %s", version, binPath)
	}

	binSHA, err := fileSHA256(binPath)
	if err != nil {
		os.RemoveAll(destDir)
		return Result{}, fmt.Errorf("toolchain go %s: binary checksum failed: %w", version, err)
	}

	// Write metadata — hard failure, install is incomplete without provenance
	meta := Metadata{
		Tool:      "go",
		Version:   version,
		Platform:  fmt.Sprintf("%s/%s", goos, goarch),
		SourceURL: sourceURL,
		SHA256:    archiveSHA,
		BinSHA256: binSHA,
	}
	if err := writeMetadataTo(installRoot, "go", version, meta); err != nil {
		os.RemoveAll(destDir)
		return Result{}, fmt.Errorf("toolchain go %s: metadata write failed (install aborted): %w", version, err)
	}

	return Result{
		Tool:      "go",
		Version:   version,
		Path:      binPath,
		CacheHit:  false,
		SourceURL: sourceURL,
		SHA256:    archiveSHA,
		BinSHA256: binSHA,
	}, nil
}

// readMetadataFrom reads metadata from a specific cache root.
func readMetadataFrom(root, tool, version string) (Metadata, error) {
	path := MetadataPathIn(root, tool, version)
	data, err := os.ReadFile(path)
	if err != nil {
		return Metadata{}, err
	}
	var m Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		return Metadata{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return m, nil
}

// writeMetadataTo writes metadata to a specific cache root atomically.
func writeMetadataTo(root, tool, version string, m Metadata) error {
	StampMetadata(&m)
	dir := CacheDirIn(root, tool, version)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	target := filepath.Join(dir, ".metadata.json")
	tmp := target + ".tmp"
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}
