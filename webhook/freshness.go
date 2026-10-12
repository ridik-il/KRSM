package webhook

import (
	"context"

	"github.com/ridik-il/krsm/closure"
)

// Freshness is the per-request staleness-guard seam (C2, ADR-0004): confirm the cache
// is current for the request's target at targetRV, reconciling via bounded FreshGets
// over the closure neighbourhood, or return an error — the caller fails closed.
// *state.Provider.CheckFreshness satisfies it verbatim (state/provider.go). The
// authoritative targetRV is read from the request's decoded oldObject (the rv the API
// server stamped on the object the action addresses), never from the cache.
//
// reconciled reports whether the guard actually changed the cache (any upsert). When it
// is false the cache was already current, so the caller can serve its first decision
// without a second closure walk (round-2 finding 9's steady-state fast path); only a
// true reconciled warrants a recompute.
type Freshness interface {
	CheckFreshness(ctx context.Context, target closure.Ref, targetRV string, neighbourhood []closure.Ref) (reconciled bool, err error)
}

// NoFreshness is the explicit "no staleness guard" sentinel: New REJECTS a nil Fresh,
// so disabling the C2 guard (hermetic tests, the CLI one-shot path) must be spelled out,
// never defaulted — the same constructor altitude as State/Synced/Mode.
var NoFreshness Freshness = noFreshness{}

type noFreshness struct{}

func (noFreshness) CheckFreshness(context.Context, closure.Ref, string, []closure.Ref) (bool, error) {
	return false, nil
}
