package output

import "io"

// PanelDomain declares ownership boundaries for rendered data.
// No datum may appear outside its owning domain panel.
//
// This is typed guidance today: ContextBlock accepts []DomainKV (not the
// old []KV section-row type), which eliminates the structural leakage
// category. Domain value enforcement is not yet compile-time verified.
type PanelDomain string

const (
	// DomainCode owns: Commit SHA, Branch, Tag.
	// Rendered once per run as a plain ContextBlock (no box frame).
	DomainCode PanelDomain = "code"

	// DomainExecution owns: Engine, Pipeline ID, Runner name, Job ID,
	// Workflow (GitHub), Controller/Satellite (SF daemon future),
	// and all substrate facts (disk, memory, cpu, docker, buildkit).
	// Rendered as the "Runner" section box.
	DomainExecution PanelDomain = "execution"

	// DomainPlan owns: declared build intent — platforms, strategy,
	// build count, cache configuration, target declarations.
	// Registries appear here as declared push destinations (intent).
	DomainPlan PanelDomain = "plan"

	// DomainBuild owns: build execution — layers, timing, cache state,
	// builder identity. Builder info and cache info fold into Build
	// as subsections, not standalone panels.
	DomainBuild PanelDomain = "build"

	// DomainResult owns: produced artifacts and their distribution —
	// image digests, pushed tags, registry outcomes, release objects,
	// forge projections. Registries appear here as distribution outcomes.
	DomainResult PanelDomain = "result"

	// DomainSecurity owns: vulnerability findings, SBOM artifacts,
	// scanner aggregate summary. Raw scanner output is secondary artifact.
	DomainSecurity PanelDomain = "security"

	// DomainDeps owns: applied updates, skipped dependencies, CVEs fixed.
	DomainDeps PanelDomain = "deps"

	// DomainConfig owns: config source, presets, defaults applied, vars, resolution status.
	// Always rendered immediately after Runner. Upstream of all execution decisions.
	DomainConfig PanelDomain = "config"

	// DomainDocs owns: badges, narrator, reference docs, docker readme.
	// Universal postamble — rendered in every modality (generated or explicitly skipped).
	// Commit and mirror sync do NOT belong here.
	DomainDocs PanelDomain = "docs"

	// DomainSync owns: git mirror push outcomes, release projection outcomes.
	// One row per mirror accessory. Aggregate status drives PanelResult.
	DomainSync PanelDomain = "sync"

	// DomainSideEffects aggregates all external mutations for operator audit:
	// git commits, pushes, registry uploads, release creation, mirror sync.
	// Future panel — reserved for Summary-level accountability rendering.
	DomainSideEffects PanelDomain = "side_effects"

	// DomainContract is the enforcement panel.
	// Rendered only when unrendered emissions exist or when contract is partial.
	// Shows which emissions were not surfaced. Summary reflects contract status.
	DomainContract PanelDomain = "contract"
)

// DomainKV is a typed KV pair with domain ownership.
// ContextBlock accepts []DomainKV, which prevents the old []KV (section row)
// type from being passed accidentally — eliminating the entire category of
// unintentional structural leakage. Note: the Go type system does not verify
// the Domain field value at compile time, so callers can still construct a
// DomainKV{Domain: DomainPlan, ...} and pass it in. This is typed guidance,
// not true domain enforcement. Full prevention would require a sealed
// constructor interface — tracked for a future hardening pass.
type DomainKV struct {
	Domain PanelDomain
	Key    string
	Value  string
}

// CodeKV constructs a DomainCode KV pair. The conventional entry point for
// ContextBlock — signals intent and makes drift visible in code review.
func CodeKV(key, value string) DomainKV {
	return DomainKV{Domain: DomainCode, Key: key, Value: value}
}

// ContextBlock renders the DomainCode panel as a proper section box.
// Accepts only DomainCode KVs — pass CodeKV() values, not raw DomainKV.
// Pairs are rendered two-per-line. Elapsed is omitted (instantaneous by design).
func ContextBlock(w io.Writer, kv []DomainKV, color bool) {
	if len(kv) == 0 {
		return
	}
	sec := NewSection(w, "Code", 0, color)
	for i := 0; i < len(kv); i += 2 {
		if i+1 < len(kv) {
			sec.Row("%-12s%-22s%-11s%s",
				kv[i].Key, kv[i].Value, kv[i+1].Key, kv[i+1].Value)
		} else {
			sec.Row("%-12s%s", kv[i].Key, kv[i].Value)
		}
	}
	sec.Close()
}
