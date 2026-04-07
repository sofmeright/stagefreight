// Package trace implements the truth emission model for StageFreight.
//
// Every input, decision, mutation, and side effect must emit a structured
// Emission. Panels only render collected emissions — direct writes to output
// are forbidden for anything that affects operator truth.
//
// Security contract: secrets must never enter the emission system as raw values.
// Use the safe helpers (SecretPresent, Public, Masked, Decision) instead of
// constructing Emission{} directly. Collectors validate sensitivity at emit time.
//
// Enforcement layer: at end of run, Unrendered() detects contract violations.
// The Contract panel surfaces these — panels must call MarkRendered for each
// emission they surface, or the run is marked contract:incomplete.
package trace

import (
	"fmt"
	"strings"
	"sync"
)

// Category classifies what kind of influence point an emission represents.
type Category string

const (
	// CategoryInput covers config values, CLI flags, env vars, presets, defaults.
	CategoryInput Category = "input"
	// CategoryDecision covers branching logic, fallbacks, skips, mode switches.
	CategoryDecision Category = "decision"
	// CategoryMutation covers normalization, overrides, derived values, cache key resolution.
	CategoryMutation Category = "mutation"
	// CategorySideEffect covers builds, pushes, commits, releases, syncs — external mutations.
	CategorySideEffect Category = "side_effect"
)

// EmissionStatus is the outcome of the emitted fact.
type EmissionStatus string

const (
	StatusOK      EmissionStatus = "ok"
	StatusWarn    EmissionStatus = "warn"
	StatusFail    EmissionStatus = "fail"
	StatusInfo    EmissionStatus = "info"
	StatusSkipped EmissionStatus = "skipped"
)

// Sensitivity classifies whether an emission's value may be rendered/stored raw.
// Sanitization must happen BEFORE collection — never rely on render-time masking
// as the primary defense.
type Sensitivity string

const (
	// Public: safe to display exactly as emitted.
	Public Sensitivity = "public"
	// Masked: safe to display only through a masking function (e.g. partial hash).
	Masked Sensitivity = "masked"
	// Secret: must never store or display raw. Only presence/status is surfaced.
	// Value field MUST be empty. DisplayValue is the safe representation.
	Secret Sensitivity = "secret"
	// Opaque: do not display at all; only existence may be acknowledged.
	Opaque Sensitivity = "opaque"
)

// secretPatterns are heuristic guards for accidental secret leakage.
// If a Public emission's value matches any of these patterns, the emission
// is rejected at collect time (dev/CI mode). Not a primary defense — belt+suspenders.
var secretPatterns = []string{
	"-----BEGIN", // PEM blocks (private keys, certs)
	"eyJ",        // JWT header (base64 {"alg"...)
	"ghp_",       // GitHub PAT prefix
	"glpat-",     // GitLab PAT prefix
	"xoxb-",      // Slack bot token
	"xoxp-",      // Slack user token
}

// sensitiveKeywords are key names that should never carry Secret sensitivity
// with a non-empty Value. Any of these as a key + non-empty Value + non-Secret
// sensitivity is also rejected.
var sensitiveKeywords = []string{
	"password", "passwd", "token", "secret", "credential", "auth",
	"key", "private", "cert", "bearer", "api_key", "apikey",
}

// Emission is a structured signal from any influence point in the execution.
//
// Security rules:
//   - Sensitivity=Secret: Value must be empty; use DisplayValue for safe repr.
//   - Sensitivity=Public: Value must not match secretPatterns.
//   - Callers should use the safe helper methods instead of constructing this directly.
type Emission struct {
	idx          int            // assigned by Collector; used for index-based MarkRendered
	Domain       string         // PanelDomain string — which panel owns this
	Category     Category       // input / decision / mutation / side_effect
	Key          string         // logical name: "docker_readme", "build_date", "cosign_key"
	Value        string         // raw value — MUST be empty for Secret sensitivity
	DisplayValue string         // safe render representation (required for Masked/Secret)
	Status       EmissionStatus // ok | warn | fail | info | skipped
	Detail       string         // human-readable detail for degraded/failed states
	Source       string         // provenance: "env:VAR" | "config" | "default" | "inferred" | "cistate"
	Sensitivity  Sensitivity    // public | masked | secret | opaque
}

// RenderValue returns the safe value for panel rendering.
// For Secret/Opaque: returns DisplayValue. For Masked: returns DisplayValue if set.
// For Public: returns Value.
func (e Emission) RenderValue() string {
	switch e.Sensitivity {
	case Secret, Opaque:
		return e.DisplayValue
	case Masked:
		if e.DisplayValue != "" {
			return e.DisplayValue
		}
		return e.Value
	default:
		return e.Value
	}
}

// Collector accumulates emissions for a single execution run.
// Thread-safe for concurrent phase execution.
// Index-based tracking prevents key-collision false-positives in MarkRendered.
type Collector struct {
	mu        sync.Mutex
	emissions []Emission
	rendered  map[int]bool // index-based: no collision possible
}

// NewCollector returns an initialized Collector.
func NewCollector() *Collector {
	return &Collector{
		rendered: make(map[int]bool),
	}
}

// Emit records a structured emission after security validation.
// Panics in development on contract violations (secret in Value, pattern match).
// Returns error to allow graceful handling in production paths.
func (c *Collector) Emit(e Emission) error {
	// Rule S1: Secrets must never carry raw values.
	if e.Sensitivity == Secret && e.Value != "" {
		return fmt.Errorf("trace: contract violation: Secret emission %q/%q has non-empty Value — use DisplayValue only", e.Domain, e.Key)
	}

	// Rule S1 (belt+suspenders): detect secret-bearing keys with wrong sensitivity.
	keyLower := strings.ToLower(e.Key)
	for _, kw := range sensitiveKeywords {
		if strings.Contains(keyLower, kw) && e.Value != "" && e.Sensitivity != Secret && e.Sensitivity != Masked && e.Sensitivity != Opaque {
			return fmt.Errorf("trace: contract violation: emission %q/%q has sensitive key name but Sensitivity=%q with non-empty Value — use Secret/Masked/Opaque", e.Domain, e.Key, e.Sensitivity)
		}
	}

	// Rule S1 (pattern guard): reject raw secret values in Public emissions.
	if e.Sensitivity == Public && e.Value != "" {
		for _, pat := range secretPatterns {
			if strings.Contains(e.Value, pat) {
				return fmt.Errorf("trace: contract violation: Public emission %q/%q value matches secret pattern %q", e.Domain, e.Key, pat)
			}
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	e.idx = len(c.emissions)
	c.emissions = append(c.emissions, e)
	return nil
}

// must is a panic-on-error wrapper for safe helpers where validation should never fail.
func must(err error) {
	if err != nil {
		panic(err)
	}
}

// — Safe helper methods ——————————————————————————————————————————————————————

// Public emits a public fact. Value is displayed as-is. Use for non-sensitive data.
func (c *Collector) Public(domain string, cat Category, key, value, source string, status EmissionStatus) {
	must(c.Emit(Emission{
		Domain: domain, Category: cat, Key: key,
		Value: value, Status: status, Source: source, Sensitivity: Public,
	}))
}

// PublicDetail emits a public fact with detail text (for warn/fail states).
func (c *Collector) PublicDetail(domain string, cat Category, key, value, detail, source string, status EmissionStatus) {
	must(c.Emit(Emission{
		Domain: domain, Category: cat, Key: key,
		Value: value, Status: status, Detail: detail, Source: source, Sensitivity: Public,
	}))
}

// Decision emits a branching decision (skip, fallback, mode switch).
// Value is the decision outcome (e.g. "skipped", "fallback", "ci-mode").
func (c *Collector) Decision(domain, key, value, detail, source string, status EmissionStatus) {
	must(c.Emit(Emission{
		Domain: domain, Category: CategoryDecision, Key: key,
		Value: value, Status: status, Detail: detail, Source: source, Sensitivity: Public,
	}))
}

// SecretPresent emits only the presence/absence of a secret — never its value.
// displayValue is the safe operator-facing representation (e.g. "configured", "missing").
func (c *Collector) SecretPresent(domain, key, displayValue, source string, status EmissionStatus) {
	must(c.Emit(Emission{
		Domain: domain, Category: CategoryInput, Key: key,
		Value: "", DisplayValue: displayValue,
		Status: status, Source: source, Sensitivity: Secret,
	}))
}

// SideEffect emits the outcome of an external mutation (commit, push, sync, release).
func (c *Collector) SideEffect(domain, key, value, detail, source string, status EmissionStatus) {
	must(c.Emit(Emission{
		Domain: domain, Category: CategorySideEffect, Key: key,
		Value: value, Status: status, Detail: detail, Source: source, Sensitivity: Public,
	}))
}

// — Collection and rendering ————————————————————————————————————————————————

// ForDomain returns all emissions for the given domain, in emission order.
func (c *Collector) ForDomain(domain string) []Emission {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result []Emission
	for _, e := range c.emissions {
		if e.Domain == domain {
			result = append(result, e)
		}
	}
	return result
}

// All returns all emissions in emission order.
func (c *Collector) All() []Emission {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]Emission, len(c.emissions))
	copy(cp, c.emissions)
	return cp
}

// MarkRendered records that an emission was surfaced in a panel.
// Uses index-based tracking — no key collision possible.
// Panels must call this for every emission they render.
// Accepts the Emission value directly so idx (unexported) is accessible
// without exposing it as a public field.
func (c *Collector) MarkRendered(e Emission) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rendered[e.idx] = true
}

// MarkAllRendered marks all emissions for a domain as rendered.
// Use when a panel renders all emissions for its domain in a loop.
func (c *Collector) MarkAllRendered(domain string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.emissions {
		if e.Domain == domain {
			c.rendered[e.idx] = true
		}
	}
}

// Unrendered returns emissions that were collected but never marked rendered.
// A non-empty result at end of run is a contract violation.
// info-severity emissions are excluded — they inform formatting, not rendering contract.
func (c *Collector) Unrendered() []Emission {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result []Emission
	for _, e := range c.emissions {
		if e.Status == StatusInfo {
			continue // info does not count toward rendering contract
		}
		if !c.rendered[e.idx] {
			result = append(result, e)
		}
	}
	return result
}

// — Status aggregation ——————————————————————————————————————————————————————

// HasFailure returns true if any emission in the given domain has StatusFail.
func (c *Collector) HasFailure(domain string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.emissions {
		if e.Domain == domain && e.Status == StatusFail {
			return true
		}
	}
	return false
}

// DomainStatus returns the aggregate status for all emissions in a domain.
// fail > warn > ok > info > skipped.
func (c *Collector) DomainStatus(domain string) EmissionStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	status := StatusSkipped
	for _, e := range c.emissions {
		if e.Domain != domain {
			continue
		}
		switch {
		case e.Status == StatusFail:
			return StatusFail
		case e.Status == StatusWarn && status != StatusFail:
			status = StatusWarn
		case e.Status == StatusOK && (status == StatusSkipped || status == StatusInfo):
			status = StatusOK
		case e.Status == StatusInfo && status == StatusSkipped:
			status = StatusInfo
		}
	}
	return status
}
