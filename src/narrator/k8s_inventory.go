package narrator

import (
	"context"

	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/diag"
	"github.com/PrPlanIT/StageFreight/src/gitops"
	"github.com/PrPlanIT/StageFreight/src/k8s"
	"github.com/PrPlanIT/StageFreight/src/runtime"
)

// K8sInventoryModule renders a cluster app inventory via live Kubernetes discovery.
// Module wiring only — all logic lives in src/k8s/.
type K8sInventoryModule struct {
	CatalogPath   string               // optional path to catalog (deprecated, use inline)
	CommitSHA     string               // optional git SHA for provenance
	RepoRoot      string               // for source link verification and Flux graph resolution
	ClusterConfig config.ClusterConfig  // for kubeconfig setup + exposure rules
}

// Render discovers workloads from the live cluster, groups by app identity,
// classifies, and produces stable markdown. Returns empty string on error
// (Module interface contract — errors are logged via diag).
func (m *K8sInventoryModule) Render() string {
	// Build kubeconfig from cluster config if configured.
	// Same auth path as reconcile — no special CI wiring needed.
	// If cluster config is declared, kubeconfig setup is REQUIRED — no silent degradation.
	if m.ClusterConfig.Name != "" {
		rctx := &runtime.RuntimeContext{}
		if err := gitops.BuildKubeconfig(m.ClusterConfig, rctx); err != nil {
			diag.Error("k8s-inventory: cluster %q configured but kubeconfig setup failed: %s", m.ClusterConfig.Name, err)
			return ""
		}
		defer rctx.Resolved.Cleanup()
	}

	result, err := k8s.Discover(context.Background(), m.CatalogPath, m.RepoRoot, m.ClusterConfig.Exposure)
	if err != nil {
		diag.Error("k8s-inventory: discovery failed: %s", err)
		return ""
	}

	return k8s.RenderOverview(result, m.CommitSHA)
}
