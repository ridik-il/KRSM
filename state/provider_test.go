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

// TestProviderGVKsForKind (design rev 3, D2): the webhook's krsm.io/target annotation
// names a Kind, not a plural resource, so the Provider exposes the Kind→GVK direction
// over the SAME tracked set Tracked walks. It returns EVERY tracked group carrying that
// Kind — the caller needs the collision to fail closed on an ambiguous bare Kind —
// and nothing for an untracked Kind.
func TestProviderGVKsForKind(t *testing.T) {
	widgetA := closure.GVK{Group: "a.example.com", Version: "v1", Kind: "Widget"}
	widgetB := closure.GVK{Group: "b.example.com", Version: "v1", Kind: "Widget"}
	p := &Provider{
		targets: map[closure.GVK]cluster.Target{
			depGVK:  {GVK: depGVK, Namespaced: true},
			widgetA: {GVK: widgetA, Namespaced: true},
			widgetB: {GVK: widgetB, Namespaced: true},
		},
	}

	if got := p.GVKsForKind("Deployment"); len(got) != 1 || got[0] != depGVK {
		t.Errorf("GVKsForKind(Deployment) = %v, want [%v]", got, depGVK)
	}

	got := p.GVKsForKind("Widget")
	if len(got) != 2 {
		t.Fatalf("GVKsForKind(Widget) = %v, want both tracked groups", got)
	}
	seen := map[closure.GVK]bool{got[0]: true, got[1]: true}
	if !seen[widgetA] || !seen[widgetB] {
		t.Errorf("GVKsForKind(Widget) = %v, want %v and %v in any order", got, widgetA, widgetB)
	}

	if got := p.GVKsForKind("Untracked"); len(got) != 0 {
		t.Errorf("GVKsForKind(untracked kind) = %v, want empty", got)
	}
}

// TestProviderGetByGVKIsGroupAware (test 15b, design rev 3 "Group-blind human key"):
// the ownership-root lookup must be resolved by GROUP, not by the group-blind
// Kind/ns/name bucket Get falls back to. Two tracked kinds sharing Kind + namespace +
// name and differing only in API group are distinct objects with distinct subtrees, so
// a re-root naming one of them must never be answered with the other — that would walk
// the wrong tree and authorise the wrong blast radius (a mis-scope in the UNSAFE
// direction). Namespace and name are part of the key too, and a miss reports false so
// the caller can fail closed rather than invent an identity.
func TestProviderGetByGVKIsGroupAware(t *testing.T) {
	widgetA := closure.GVK{Group: "a.example.com", Version: "v1", Kind: "Widget"}
	widgetB := closure.GVK{Group: "b.example.com", Version: "v1", Kind: "Widget"}
	p := &Provider{idx: newIndex()}
	p.idx.upsert(closure.Object{Ref: closure.Ref{GVK: widgetA, Namespace: "prod", Name: "w", UID: "uid-wa"}})
	p.idx.upsert(closure.Object{Ref: closure.Ref{GVK: widgetB, Namespace: "prod", Name: "w", UID: "uid-wb"}})

	for _, tc := range []struct {
		gvk     closure.GVK
		wantUID string
	}{
		{widgetA, "uid-wa"},
		{widgetB, "uid-wb"},
	} {
		got, ok := p.GetByGVK(tc.gvk, "prod", "w")
		if !ok {
			t.Errorf("GetByGVK(%v, prod, w) reported not found, want the tracked object", tc.gvk)
			continue
		}
		if got.Ref.UID != tc.wantUID {
			t.Errorf("GetByGVK(%v, prod, w).UID = %q, want %q — the group must decide, not the shared Kind/ns/name bucket", tc.gvk, got.Ref.UID, tc.wantUID)
		}
		if got.Ref.GVK.Group != tc.gvk.Group {
			t.Errorf("GetByGVK(%v, prod, w).GVK = %v, want group %q", tc.gvk, got.Ref.GVK, tc.gvk.Group)
		}
	}

	for name, miss := range map[string]struct {
		gvk       closure.GVK
		namespace string
		name      string
	}{
		"untracked group": {closure.GVK{Group: "c.example.com", Version: "v1", Kind: "Widget"}, "prod", "w"},
		"other namespace": {widgetA, "staging", "w"},
		"other name":      {widgetA, "prod", "other"},
		"other kind":      {closure.GVK{Group: "a.example.com", Version: "v1", Kind: "Gadget"}, "prod", "w"},
	} {
		if _, ok := p.GetByGVK(miss.gvk, miss.namespace, miss.name); ok {
			t.Errorf("%s: GetByGVK(%v, %s, %s) must report not found", name, miss.gvk, miss.namespace, miss.name)
		}
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
