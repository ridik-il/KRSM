package webhook

import (
	"context"
	"encoding/json"

	"github.com/ridik-il/krsm/closure"
)

// Freshness is the per-request staleness-guard seam (C2, ADR-0004): confirm the cache
// is current for the request's target at targetRV, reconciling via bounded FreshGets
// over the closure neighbourhood, or return an error — the caller fails closed.
// *state.Provider.CheckFreshness satisfies it verbatim (state/provider.go).
type Freshness interface {
	CheckFreshness(ctx context.Context, target closure.Ref, targetRV string, neighbourhood []closure.Ref) error
}

// resourceVersionOf peeks metadata.resourceVersion from a raw request payload. It is
// deliberately independent of cluster.Project: closure.Object carries no rv, and the
// guard needs the AUTHORITATIVE rv the API server stamped on the request's object.
func resourceVersionOf(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var peek struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return ""
	}
	return peek.Metadata.ResourceVersion
}
