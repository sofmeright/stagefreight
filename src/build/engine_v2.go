package build

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// EngineV2 is the universal engine interface for all build types.
// Engines are declarative planners — they convert config to steps and execute
// individual steps. All orchestration (ordering, concurrency, retries, logging,
// artifact recording, checksums, publish manifest) lives in core.
type EngineV2 interface {
	// Name returns the engine's identifier (e.g., "binary", "image").
	Name() string

	// Capabilities declares what this engine supports.
	Capabilities() Capabilities

	// Detect inspects a repo and returns build-relevant information.
	Detect(ctx context.Context, rootDir string) (*Detection, error)

	// Plan converts a build config into a list of universal steps.
	// Core owns config synthesis — Plan receives normalized config, not detection guesses.
	Plan(ctx context.Context, cfg BuildConfig) ([]UniversalStep, error)

	// ExecuteStep runs a single step and returns its result.
	// The engine must not: write publish manifests, attach release assets,
	// manage concurrency, resolve dependencies, or print logs directly.
	ExecuteStep(ctx context.Context, step UniversalStep) (*UniversalStepResult, error)
}

// BuildConfig is the normalized input to EngineV2.Plan().
// Core synthesizes this from config + detection; engines don't develop their
// own auto-config policy brains.
type BuildConfig struct {
	ID         string
	Kind       string
	Platforms  []Platform
	BuildMode  string
	SelectTags []string
	DependsOn  string

	// Version info for template resolution
	Version *VersionInfo

	// kind: docker fields
	Dockerfile string
	Context    string
	Target     string
	BuildArgs  map[string]string
	Tags       []string
	Registries []RegistryTarget

	// kind: binary fields
	Builder  string
	Command  string
	From     string
	Output   string
	Args     []string
	Env      map[string]string
	Compress bool
}

var (
	registryV2Mu sync.RWMutex
	registryV2   = map[string]func() EngineV2{}
)

// RegisterV2 adds an EngineV2 constructor to the global registry.
func RegisterV2(name string, constructor func() EngineV2) {
	registryV2Mu.Lock()
	defer registryV2Mu.Unlock()
	if _, exists := registryV2[name]; exists {
		panic(fmt.Sprintf("build: duplicate v2 engine registration: %s", name))
	}
	registryV2[name] = constructor
}

// GetV2 returns a new instance of the named v2 engine.
func GetV2(name string) (EngineV2, error) {
	registryV2Mu.RLock()
	defer registryV2Mu.RUnlock()
	ctor, ok := registryV2[name]
	if !ok {
		return nil, fmt.Errorf("build: unknown v2 engine: %s", name)
	}
	return ctor(), nil
}

// AllV2 returns sorted names of all registered v2 engines.
func AllV2() []string {
	registryV2Mu.RLock()
	defer registryV2Mu.RUnlock()
	names := make([]string, 0, len(registryV2))
	for name := range registryV2 {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
