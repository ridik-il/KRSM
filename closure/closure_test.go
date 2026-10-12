package closure

import (
	"testing"
	"time"
)

// TestJoinRefs pins the shared ref-rendering used by the CLI output and the webhook
// reason strings: ", "-joined Ref.String()s, and empty for no refs.
func TestJoinRefs(t *testing.T) {
	if got := JoinRefs(nil); got != "" {
		t.Errorf("JoinRefs(nil) = %q, want empty", got)
	}
	refs := []Ref{
		{GVK: GVK{Version: "v1", Kind: "Service"}, Namespace: "prod", Name: "svc"},
		{GVK: GVK{Version: "v1", Kind: "Secret"}, Namespace: "prod", Name: "db"},
	}
	if got, want := JoinRefs(refs), "Service/prod/svc, Secret/prod/db"; got != want {
		t.Errorf("JoinRefs = %q, want %q", got, want)
	}
}

// TestClosureTerminatesOnCyclicOwners asserts the BFS terminates and stays
// bounded when ownerReferences form a cycle (adversarial input, DESIGN §8).
// It builds state inline from exported constructors, so it is a pure closure
// property test with no scenario loader and no import on the scenario package.
//
// The golden-file scenario tests (TestScenarios) and the |C| ≤ |R| bound test
// live in package scenario: they drive closure through its exported API via the
// shared loader, which cannot be imported from an internal closure test without
// an import cycle.
func TestClosureTerminatesOnCyclicOwners(t *testing.T) {
	a := Object{Ref: Ref{GVK: GVK{Version: "v1", Kind: "Widget"}, Namespace: "x", Name: "a", UID: "uid:a"},
		Owners: []OwnerRef{{Kind: "Widget", Name: "b", UID: "uid:b"}}}
	b := Object{Ref: Ref{GVK: GVK{Version: "v1", Kind: "Widget"}, Namespace: "x", Name: "b", UID: "uid:b"},
		Owners: []OwnerRef{{Kind: "Widget", Name: "a", UID: "uid:a"}}}
	st := NewScanState([]Object{a, b})

	done := make(chan []Ref, 1)
	go func() {
		done <- Closure(st, Action{Verb: Delete, Cascade: true, Target: a.Ref})
	}()
	select {
	case c := <-done:
		if len(c) > 2 {
			t.Errorf("|C| = %d exceeds |R| = 2 on cyclic owners", len(c))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Closure did not terminate on cyclic ownerReferences")
	}
}
