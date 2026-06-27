package state

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/internal/cluster"
	"github.com/ridik-il/krsm/internal/scenario"
)

// corpusScenarios are the golden scenarios whose verdict is decided by a relation the
// indexed Provider serves (owner edges, namespace containment, selectors, every
// cross-ref kind, scaleTargetRef, finalizers). The two negative load-error goldens
// (22, 27) are excluded — they fail at scope parse time, before any State is consulted.
var corpusScenarios = []string{
	"01-memory-pressure-cascade",
	"02-label-rebind-breaks-routing",
	"03-pvc-delete-data-loss",
	"04-shared-secret-rotation",
	"05-namespace-blast-radius",
	"06-scale-fights-hpa-pdb",
	"07-shared-configmap-rollout",
	"08-networkpolicy-widening",
	"09-finalizer-orphans-external",
	"10-same-name-wrong-tenant",
	"11-unknown-target-fail-closed",
	"12-workload-update-recreates-pods",
	"13-in-scope-cascade-allowed",
	"14-cluster-scoped-pv-not-contained",
	"15-initcontainer-configmap",
	"16-projected-secret",
	"17-imagepullsecret",
	"18-matchexpressions-precise-binding",
	"19-ephemeralcontainer-configmap",
	"20-scope-selector-precision",
	"21-taskcontract-selector-scope",
	"23-namespace-scope",
	"24-ownership-scope",
	"25-ownership-escape",
	"26-derived-default",
}

func scenarioDir(name string) string {
	return filepath.Join("..", "closure", "testdata", "scenarios", name)
}

// TestProviderParityOracle is the central correctness proof: for every corpus scenario,
// the informer-backed indexed Provider must produce the SAME closure.Safe decision
// (verdict + closure + escaping + external) as the linear-scan NewScanState the goldens
// are pinned to. It uses the loader's own Action+Scope (scenario.Load), so only the
// State differs — Provider vs scanState. This proves the index reproduces the engine's
// verdict despite leaving cross-ref uids empty (the Kind/ns/name fallback path).
func TestProviderParityOracle(t *testing.T) {
	for _, name := range corpusScenarios {
		t.Run(name, func(t *testing.T) {
			dir := scenarioDir(name)
			sc, err := scenario.Load(dir)
			if err != nil {
				t.Fatalf("scenario.Load: %v", err)
			}
			p := buildSyncedProvider(t, loadCorpusUnstructured(t, dir))

			want := closure.Safe(sc.State, sc.Action, sc.Scope)
			got := closure.Safe(p, sc.Action, sc.Scope)

			if got.Verdict != want.Verdict {
				t.Errorf("verdict = %s, want %s", got.Verdict, want.Verdict)
			}
			assertSameRefSet(t, "closure", got.Closure, want.Closure)
			assertSameRefSet(t, "escaping", got.Escaping, want.Escaping)
			assertSameRefSet(t, "external", got.External, want.External)
		})
	}
}

// TestProviderMethodsMatchScanState pins each State method directly (not just through
// Safe): for every object in every corpus scenario, the Provider's Get / OwnedChildren /
// Consumers / ControllersTargeting / SelectorsTargeting / PodsSelectedBy and every
// namespace's NamespaceContents return the same Refs as NewScanState.
func TestProviderMethodsMatchScanState(t *testing.T) {
	for _, name := range corpusScenarios {
		t.Run(name, func(t *testing.T) {
			dir := scenarioDir(name)
			sc, err := scenario.Load(dir)
			if err != nil {
				t.Fatalf("scenario.Load: %v", err)
			}
			objs := loadCorpusUnstructured(t, dir)
			p := buildSyncedProvider(t, objs)

			namespaces := map[string]bool{}
			for _, u := range objs {
				o, err := cluster.Project(*u, fakeScope{})
				if err != nil {
					t.Fatalf("project: %v", err)
				}
				r := o.Ref
				namespaces[r.Namespace] = true

				if _, gotOK := p.Get(r); !gotOK {
					t.Errorf("Get(%s): provider missing object", r)
				}
				if _, wantOK := sc.State.Get(r); !wantOK {
					t.Errorf("Get(%s): loader missing object (test bug)", r)
				}
				assertSameRefSet(t, "OwnedChildren("+r.String()+")", p.OwnedChildren(r), sc.State.OwnedChildren(r))
				assertSameRefSet(t, "Consumers("+r.String()+")", p.Consumers(r), sc.State.Consumers(r))
				assertSameRefSet(t, "ControllersTargeting("+r.String()+")", p.ControllersTargeting(r), sc.State.ControllersTargeting(r))
				if r.GVK.Kind == "Pod" {
					assertSameRefSet(t, "SelectorsTargeting("+r.String()+")", p.SelectorsTargeting(r), sc.State.SelectorsTargeting(r))
				}
				assertSameRefSet(t, "PodsSelectedBy("+r.String()+")", p.PodsSelectedBy(r), sc.State.PodsSelectedBy(r))
			}
			for ns := range namespaces {
				assertSameRefSet(t, "NamespaceContents("+ns+")", p.NamespaceContents(ns), sc.State.NamespaceContents(ns))
			}
		})
	}
}

// TestServeBeforeSyncFailsClosed: until WaitForCacheSync completes, HasSynced is false —
// the observable, distinct not-ready signal the caller maps to a fail-closed deny
// (DESIGN §5). A Provider that has not synced never silently reports a populated cache.
func TestServeBeforeSyncFailsClosed(t *testing.T) {
	p, err := newProvider(nil, nil, nil, nil, fakeScope{}, objectGetter{})
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}
	if p.HasSynced() {
		t.Fatal("HasSynced() = true before WaitForCacheSync; must be false (fail-closed)")
	}
	// With no informers, WaitForCacheSync returns immediately true and flips HasSynced.
	if !p.WaitForCacheSync(context.Background()) {
		t.Fatal("WaitForCacheSync with no informers should return true")
	}
	if !p.HasSynced() {
		t.Fatal("HasSynced() = false after WaitForCacheSync")
	}
}
