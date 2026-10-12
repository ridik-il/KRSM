package webhook

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/scope"
)

// fakeFresh is the freshness seam fake: records the call, optionally runs a hook (to
// model the cache reconciling), and returns (reconciled, err).
type fakeFresh struct {
	err        error
	reconciled bool
	called     bool
	gotRV      string
	gotRefs    []closure.Ref
	onCheck    func()
}

func (f *fakeFresh) CheckFreshness(_ context.Context, _ closure.Ref, rv string, refs []closure.Ref) (bool, error) {
	f.called, f.gotRV, f.gotRefs = true, rv, refs
	if f.onCheck != nil {
		f.onCheck()
	}
	return f.reconciled, f.err
}

// switchState delegates to the embedded State and can be re-pointed mid-request —
// modelling CheckFreshness reconciling the cache under the handler.
type switchState struct{ closure.State }

// TestHandleFreshnessErrorDeniesBothModes (design test 14a): any freshness-seam error
// is the C2 fail-closed path — krsm:stale deny, never softened by audit.
func TestHandleFreshnessErrorDeniesBothModes(t *testing.T) {
	for _, mode := range []scope.Mode{scope.ModeAudit, scope.ModeEnforce} {
		fresh := &fakeFresh{err: errors.New("unreconciled drift")}
		s := newTestServer(t, cascadeState(), mode, func(c *Config) { c.Fresh = fresh })
		out := s.Handle(context.Background(), deleteReview("req-drift"))
		if out.Response.Allowed {
			t.Errorf("mode %s: freshness failure must deny", mode)
		}
		if !strings.Contains(out.Response.Result.Message, "krsm:stale") {
			t.Errorf("mode %s: message %q must carry krsm:stale", mode, out.Response.Result.Message)
		}
		if !fresh.called || fresh.gotRV != "7" {
			t.Errorf("mode %s: guard called=%v rv=%q, want called with the request's resourceVersion 7", mode, fresh.called, fresh.gotRV)
		}
		if len(fresh.gotRefs) == 0 {
			t.Errorf("mode %s: guard must receive the closure neighbourhood", mode)
		}
	}
}

// TestHandleServesRecomputedDecisionAfterReconcile (design test 14b): after a
// successful freshness pass the verdict is RECOMPUTED over the (possibly reconciled)
// cache — proven by a state that flips from escaping to in-scope during the check.
func TestHandleServesRecomputedDecisionAfterReconcile(t *testing.T) {
	// After "reconciliation" the Service is gone: the delete's closure is exactly the
	// ownership tree → in-scope → allowed. Serving dec₁ (pre-reconcile) would deny.
	dep := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}, Namespace: "prod", Name: "web", UID: "uid-d"}}
	reconciled := closure.NewScanState([]closure.Object{dep})

	sw := &switchState{State: cascadeState()}
	fresh := &fakeFresh{reconciled: true, onCheck: func() { sw.State = reconciled }}
	s := newTestServer(t, sw, scope.ModeEnforce, func(c *Config) { c.Fresh = fresh })

	out := s.Handle(context.Background(), deleteReview("req-recompute"))
	if !fresh.called {
		t.Fatal("freshness guard was not consulted")
	}
	if !out.Response.Allowed {
		t.Errorf("the RECOMPUTED (reconciled, in-scope) decision must be served, got deny %q", out.Response.Result.Message)
	}
}

// TestHandleMissingResourceVersionDenies (design test 15): a request whose oldObject
// carries no resourceVersion cannot be confirmed current — krsm:stale deny, and the
// guard is never invoked with a made-up rv.
func TestHandleMissingResourceVersionDenies(t *testing.T) {
	fresh := &fakeFresh{}
	s := newTestServer(t, cascadeState(), scope.ModeAudit, func(c *Config) { c.Fresh = fresh })
	review := deleteReview("req-norv")
	review.Request.OldObject = raw(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"prod","uid":"uid-d"}}`)
	out := s.Handle(context.Background(), review)
	if out.Response.Allowed {
		t.Error("missing resourceVersion must fail closed")
	}
	if !strings.Contains(out.Response.Result.Message, "krsm:stale") {
		t.Errorf("message %q must carry krsm:stale", out.Response.Result.Message)
	}
	if fresh.called {
		t.Error("the guard must not run with an absent resourceVersion")
	}
}
