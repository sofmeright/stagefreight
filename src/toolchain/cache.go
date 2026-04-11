package toolchain

import (
	"os"
	"path/filepath"
)

const (
	cacheSubdir    = ".stagefreight/toolchains"
	persistentRoot = "/stagefreight/toolchains"
)

// ReadRoots returns cache roots to search for existing toolchain installs,
// in priority order. Persistent mount is checked first (operator-preseeded
// or previously written), then workspace-local.
func ReadRoots(rootDir string) []string {
	var roots []string
	if info, err := os.Stat(persistentRoot); err == nil && info.IsDir() {
		roots = append(roots, persistentRoot)
	}
	roots = append(roots, filepath.Join(rootDir, cacheSubdir))
	return roots
}

// InstallRoot returns the directory where new toolchain installs are written.
// Prefers persistent mount if writable, otherwise workspace-local.
func InstallRoot(rootDir string) string {
	if isWritable(persistentRoot) {
		return persistentRoot
	}
	return filepath.Join(rootDir, cacheSubdir)
}

// CacheBinPathIn returns the binary path for a tool within a specific cache root.
func CacheBinPathIn(root, tool, version, binary string) string {
	return filepath.Join(root, tool, version, "bin", binary)
}

// CacheDirIn returns the versioned install directory within a specific cache root.
func CacheDirIn(root, tool, version string) string {
	return filepath.Join(root, tool, version)
}

// MetadataPathIn returns the metadata file path within a specific cache root.
func MetadataPathIn(root, tool, version string) string {
	return filepath.Join(root, tool, version, ".metadata.json")
}

// isWritable returns true if the directory exists, is a directory, and is writable.
func isWritable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	tmp := filepath.Join(dir, ".sf-probe")
	f, err := os.Create(tmp)
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(tmp)
	return true
}
