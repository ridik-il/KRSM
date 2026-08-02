package webhook

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// TaskContractGVR is the resource the Level-3 scope channel reads: the krsm.io/v1alpha1
// TaskContract CR whose compiled allow-clauses become a request's scope. It is the
// contract between this seam and the CRD manifest (slice 7); the version literal moves
// only when the CRD's does.
var TaskContractGVR = schema.GroupVersionResource{Group: "krsm.io", Version: "v1alpha1", Resource: "taskcontracts"}

// DynamicContractGetter is the PRODUCTION contractGetter: one deadline-bounded LIVE GET
// of the named TaskContract through a dynamic client. It is deliberately not backed by
// an informer or any other cache — a contract that has been deleted or tightened must
// stop authorising on the very next request, and a cache would keep answering from the
// author's revoked intent. That staleness window is a false ALLOW, the one error class
// this gate must not have; one synchronous read on the rare L3 path is the price.
//
// It lives in webhook/ rather than in cmd/krsm because everything it encodes is
// admission-security semantics that belong beside the seam it implements: which resource
// is authoritative (TaskContractGVR), that an absent CRD and an absent CR are the same
// "not found" outcome, and that an expired deadline must not issue a read. Package main
// would then own three rules the webhook's fail-closed behaviour depends on, with no test
// in this package pinning them.
//
// It performs NO discovery and holds no cache, so a server wired with one starts and
// serves L0/L1 normally when the TaskContract CRD is absent entirely: the absence
// surfaces as a not-found on the L3 path only, and a CRD installed later is picked up
// with no restart.
type DynamicContractGetter struct {
	client dynamic.Interface
}

// NewDynamicContractGetter builds the live-GET contract seam over a dynamic client.
// It never contacts the cluster — construction must not be able to fail-close a webhook
// whose L3 channel nobody uses.
func NewDynamicContractGetter(client dynamic.Interface) *DynamicContractGetter {
	return &DynamicContractGetter{client: client}
}

// Get reads the TaskContract CR live. A missing CR — and equally a missing CRD, which the
// API server answers with the same 404 — is reported as ErrContractNotFound; every other
// failure (RBAC forbidden, transport, deadline) is returned as itself, so a caller can
// never mistake "you may not read it" for "it does not exist". resolveContract fails
// closed on all of them alike, but an operator reading the log must be able to tell them
// apart.
func (g *DynamicContractGetter) Get(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	// The deadline is checked BEFORE the read, not only inside the client: a contract
	// fetched after the admission deadline can never be delivered, so issuing it would
	// spend an API call on a request already destined to fail closed.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("taskcontract %s/%s: %w", namespace, name, err)
	}
	if g == nil || g.client == nil {
		// A mis-wired serve path must surface as a fail-closed deny, never as a panic
		// inside the admission handler.
		return nil, errors.New("taskcontract getter has no dynamic client")
	}
	u, err := g.client.Resource(TaskContractGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("taskcontract %s/%s: %w", namespace, name, ErrContractNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("taskcontract %s/%s: %w", namespace, name, err)
	}
	return u, nil
}
