package narrator

import (
	"context"

	"github.com/PrPlanIT/StageFreight/src/diag"
	"github.com/PrPlanIT/StageFreight/src/k8s"
)

// K8sInventoryModule renders a cluster app inventory via live Kubernetes discovery.
// Module wiring only — all logic lives in src/k8s/.
type K8sInventoryModule struct {
	CatalogPath string // optional path to .stagefreight-catalog.yml
	CommitSHA   string // optional git SHA for provenance
}

// Render discovers workloads from the live cluster, groups by app identity,
// classifies, and produces stable markdown. Returns empty string on error
// (Module interface contract — errors are logged via diag).
func (m *K8sInventoryModule) Render() string {
	result, err := k8s.Discover(context.Background(), m.CatalogPath)
	if err != nil {
		// Module contract: Render returns string, no error.
		// Use diag.Warn — structured diagnostic, not raw stderr.
		diag.Warn("k8s-inventory: %s", err)
		return ""
	}

	return k8s.RenderOverview(result, m.CommitSHA)
}
