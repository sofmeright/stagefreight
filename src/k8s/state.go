package k8s

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const manifestsDir = ".stagefreight/manifests"

// InventoryManifest is the scoped k8s inventory manifest.
// Lives at .stagefreight/manifests/k8s-inventory-<cluster>.json
// Manifest accelerates truth — it never becomes truth.
type InventoryManifest struct {
	SchemaVersion   int                       `json:"schema_version"`
	Cluster         string                    `json:"cluster"`
	GeneratedAt     time.Time                 `json:"generated_at"`
	DiscoveryStatus DiscoveryStatus           `json:"discovery_status"`
	Apps            map[string]AppManifest    `json:"apps"` // key: namespace/identity
}

// DiscoveryStatus records whether discovery completed fully.
// Lifecycle mutations MUST be blocked unless Complete == true.
type DiscoveryStatus struct {
	Complete bool   `json:"complete"`
	Source   string `json:"source,omitempty"` // live_cluster
}

// AppManifest is the per-app section in the inventory manifest.
// Three clean planes: lifecycle (memory), observed (snapshot), identity_cache (enrichment).
type AppManifest struct {
	Lifecycle          AppLifecycle         `json:"lifecycle"`
	Observed           AppObserved          `json:"observed"`
	IdentityCache      IdentityCache        `json:"identity_cache"`
	IdentityCacheStatus CacheStatus         `json:"identity_cache_status"`
	EnrichmentMeta     map[string]EnrichMeta `json:"enrichment_meta,omitempty"`
}

// AppLifecycle tracks active/graveyard state transitions.
type AppLifecycle struct {
	State        string     `json:"state"`                   // active | graveyard
	LastSeen     time.Time  `json:"last_seen"`
	MissingSince *time.Time `json:"missing_since,omitempty"`
}

// AppObserved holds current discovery snapshot. Rewritten each successful run.
type AppObserved struct {
	Namespace    string   `json:"namespace"`
	Name         string   `json:"name"`
	Images       []string `json:"images"`
	ImageDigests []string `json:"image_digests"`
}

// IdentityCache holds enriched presentation fields. Cached, replaceable.
type IdentityCache struct {
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	Homepage    string `json:"homepage,omitempty"`
	Version     string `json:"version,omitempty"`
}

// CacheStatus tracks freshness of the identity cache.
type CacheStatus struct {
	State          string     `json:"state"`                      // fresh | stale
	Reason         string     `json:"reason,omitempty"`            // why stale
	LastAttemptedAt *time.Time `json:"last_attempted_at,omitempty"`
}

// EnrichMeta tracks provenance for a single cached field.
type EnrichMeta struct {
	Source    string `json:"source"`               // oci_label, helm_chart, annotation
	UpdatedAt string `json:"updated_at"`
	Basis     *EnrichBasis `json:"basis,omitempty"` // invalidation key
}

// EnrichBasis holds the content basis for cache invalidation.
type EnrichBasis struct {
	ImageDigest string `json:"image_digest,omitempty"`
}

// inventoryPath returns the scoped manifest path for a cluster.
func inventoryPath(repoRoot, clusterName string) string {
	return filepath.Join(repoRoot, manifestsDir, "k8s-inventory-"+clusterName+".json")
}

// LoadManifest reads the generated inventory manifest for a cluster.
func LoadManifest(repoRoot, clusterName string) (*InventoryManifest, error) {
	path := inventoryPath(repoRoot, clusterName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &InventoryManifest{
				SchemaVersion: 1,
				Cluster:       clusterName,
				Apps:          map[string]AppManifest{},
			}, nil
		}
		return nil, err
	}

	var m InventoryManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Apps == nil {
		m.Apps = map[string]AppManifest{}
	}
	return &m, nil
}

// ReconcileLifecycle updates lifecycle state from current discovery.
// Discovery is authoritative for active. Manifest is memory.
// HARD RULE: discovery_status.complete must be true to mutate lifecycle.
func ReconcileLifecycle(manifest *InventoryManifest, activeApps []AppRecord, discoveryComplete bool, now time.Time) bool {
	if !discoveryComplete {
		// Partial discovery — do NOT mutate lifecycle. Prevents false graveyarding.
		return false
	}

	changed := false
	manifest.DiscoveryStatus = DiscoveryStatus{Complete: true, Source: "live_cluster"}
	manifest.GeneratedAt = now

	// Build active set from discovery.
	activeKeys := map[string]AppRecord{}
	for _, app := range activeApps {
		key := app.Key.Namespace + "/" + app.Key.Identity
		activeKeys[key] = app
	}

	// Update existing entries.
	for key, entry := range manifest.Apps {
		if app, active := activeKeys[key]; active {
			if entry.Lifecycle.State != "active" {
				entry.Lifecycle.State = "active"
				entry.Lifecycle.MissingSince = nil
				changed = true
			}
			entry.Lifecycle.LastSeen = now

			// Check if image digests changed → invalidate enrichment cache.
			newObserved := buildObserved(app)
			if digestsChanged(entry.Observed.ImageDigests, newObserved.ImageDigests) {
				entry.IdentityCacheStatus = CacheStatus{
					State:  "stale",
					Reason: "image_digest_changed",
					// LastAttemptedAt stays nil — this is invalidation, not an enrichment attempt.
				}
				changed = true
			}

			entry.Observed = newObserved
			manifest.Apps[key] = entry
		} else {
			// App is missing. Transition: active → missing → graveyard.
			// Missing provides temporal buffering against flapping.
			switch entry.Lifecycle.State {
			case "active":
				entry.Lifecycle.State = "missing"
				missing := now
				entry.Lifecycle.MissingSince = &missing
				changed = true
				manifest.Apps[key] = entry
			case "missing":
				// Already missing — could promote to graveyard after threshold.
				// For now, stays missing until next run confirms absence.
				entry.Lifecycle.State = "graveyard"
				changed = true
				manifest.Apps[key] = entry
			case "graveyard":
				// Already in graveyard — no change.
			}
		}
	}

	// Add new apps.
	for key, app := range activeKeys {
		if _, exists := manifest.Apps[key]; !exists {
			manifest.Apps[key] = AppManifest{
				Lifecycle: AppLifecycle{
					State:    "active",
					LastSeen: now,
				},
				Observed: buildObserved(app),
				IdentityCacheStatus: CacheStatus{State: "stale", Reason: "new_app"},
			}
			changed = true
		}
	}

	return changed
}

func buildObserved(app AppRecord) AppObserved {
	var images, digests []string
	for _, img := range app.Images {
		images = append(images, img.Repository+":"+img.Tag)
		if img.Digest != "" {
			digests = append(digests, img.Digest)
		}
	}
	return AppObserved{
		Namespace:    app.Key.Namespace,
		Name:         app.Key.Identity,
		Images:       images,
		ImageDigests: digests,
	}
}

// digestsChanged returns true if the digest sets differ.
func digestsChanged(old, new []string) bool {
	if len(old) != len(new) {
		return true
	}
	oldSet := map[string]bool{}
	for _, d := range old {
		oldSet[d] = true
	}
	for _, d := range new {
		if !oldSet[d] {
			return true
		}
	}
	return false
}

// GraveyardFromManifest returns graveyard entries for rendering.
func GraveyardFromManifest(manifest *InventoryManifest) []GraveyardEntry {
	var entries []GraveyardEntry
	for _, entry := range manifest.Apps {
		if entry.Lifecycle.State != "graveyard" && entry.Lifecycle.State != "missing" {
			continue
		}
		reason := "no longer running in cluster"
		if entry.Lifecycle.MissingSince != nil {
			reason = "missing since " + entry.Lifecycle.MissingSince.UTC().Format("2006-01-02")
		}
		entries = append(entries, GraveyardEntry{
			Name:      entry.Observed.Name,
			Namespace: entry.Observed.Namespace,
			Reason:    reason,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	return entries
}

// SaveManifest writes the manifest with stable formatting.
// Sorted keys, indented JSON, no-op if unchanged.
func SaveManifest(repoRoot, clusterName string, manifest *InventoryManifest) error {
	dir := filepath.Join(repoRoot, manifestsDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	path := inventoryPath(repoRoot, clusterName)

	// Sort app keys for deterministic output. Go maps are unordered.
	// Use a wrapper to avoid mutating the in-memory manifest.
	sortedApps := make(map[string]AppManifest)
	keys := make([]string, 0, len(manifest.Apps))
	for k := range manifest.Apps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sortedApps[k] = manifest.Apps[k]
	}

	output := *manifest
	output.Apps = sortedApps
	data, err := json.MarshalIndent(&output, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	// No-op if unchanged.
	existing, readErr := os.ReadFile(path)
	if readErr == nil && string(existing) == string(data) {
		return nil
	}

	return os.WriteFile(path, data, 0644)
}
