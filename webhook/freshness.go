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
type Freshness interface {
	CheckFreshness(ctx context.Context, target closure.Ref, targetRV string, neighbourhood []closure.Ref) error
}
