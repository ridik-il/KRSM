package state

import (
	"testing"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/internal/cluster"
)

// TestProviderImplementsState is the compile-time guard that the informer-backed
// Provider satisfies the SAME closure.State interface the engine consumes, so it
// drops in beside closure.NewScanState without any engine change (DESIGN §4/§7).
func TestProviderImplementsState(_ *testing.T) {
	var _ closure.State = (*Provider)(nil)
}

// TestProviderKindFor: the webhook resolves sub-resource parents (scale/eviction)
// through the Provider's discovery-derived GVR→GVK table — exact matches only, no
// pluralisation guess, unknown resources fail closed (false).
func TestProviderKindFor(t *testing.T) {
	p := &Provider{
		targets: map[closure.GVK]cluster.Target{
			depGVK: {GVR: corpusGVR["Deployment"], GVK: depGVK, Namespaced: true},
		},
		gvrToGVK: map[groupResource]closure.GVK{
			{"apps", "deployments"}: depGVK,
		},
	}
	gvk, ok := p.KindFor("apps", "deployments")
	if !ok || gvk != depGVK {
		t.Errorf("KindFor(apps, deployments) = (%v, %v), want (%v, true)", gvk, ok, depGVK)
	}
	if _, ok := p.KindFor("", "widgets"); ok {
		t.Error("KindFor(unknown) must report false")
	}
}

// TestSplitTargets (PR #32 review debt, design test 24): the state.New wiring routes
// Secrets/ConfigMaps to the METADATA-ONLY informer factory (their data never enters
// the process) and everything else to the full dynamic factory — exactly the
// metadataKinds split, order-preserving.
func TestSplitTargets(t *testing.T) {
	in := []cluster.Target{
		{GVR: corpusGVR["Deployment"], GVK: depGVK, Namespaced: true},
		{GVR: corpusGVR["Secret"], GVK: closure.GVK{Version: "v1", Kind: "Secret"}, Namespaced: true},
		{GVR: corpusGVR["ConfigMap"], GVK: closure.GVK{Version: "v1", Kind: "ConfigMap"}, Namespaced: true},
		{GVR: corpusGVR["Pod"], GVK: closure.GVK{Version: "v1", Kind: "Pod"}, Namespaced: true},
	}
	full, meta := splitTargets(in)
	if len(full) != 2 || full[0].GVK.Kind != "Deployment" || full[1].GVK.Kind != "Pod" {
		t.Errorf("full = %+v, want [Deployment Pod]", full)
	}
	if len(meta) != 2 || meta[0].GVK.Kind != "Secret" || meta[1].GVK.Kind != "ConfigMap" {
		t.Errorf("meta = %+v, want [Secret ConfigMap] (metadata-only, C3)", meta)
	}
}
