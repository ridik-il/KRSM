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
	p := &Provider{targets: map[closure.GVK]cluster.Target{
		depGVK: {GVR: corpusGVR["Deployment"], GVK: depGVK, Namespaced: true},
	}}
	gvk, ok := p.KindFor("apps", "deployments")
	if !ok || gvk != depGVK {
		t.Errorf("KindFor(apps, deployments) = (%v, %v), want (%v, true)", gvk, ok, depGVK)
	}
	if _, ok := p.KindFor("", "widgets"); ok {
		t.Error("KindFor(unknown) must report false")
	}
}
