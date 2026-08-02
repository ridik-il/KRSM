package webhook

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ridik-il/krsm/internal/contract"
	"github.com/ridik-il/krsm/scope"
)

// contractScheme + listKinds give the fake dynamic client the one GVR the getter reads.
// Nothing else is registered, so a test that reaches for another resource fails loudly
// rather than silently succeeding against a kind production never touches.
func contractScheme() *runtime.Scheme { return runtime.NewScheme() }

func contractListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{TaskContractGVR: "TaskContractList"}
}

// crObject renders a TaskContract CR as the API server really stores and returns one:
// the author's spec PLUS the server-owned metadata every stored object carries and the
// status a subresource returns. A stripped fixture here would rebuild the very blind
// spot that let a whole-document strict decode reject every live contract while the
// suite stayed green.
func crObject(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "krsm.io/v1alpha1",
		"kind":       "TaskContract",
		"metadata": map[string]any{
			"name":              name,
			"namespace":         namespace,
			"uid":               "9f1c1b8e-0f1a-4a3a-9c2b-0b6a1f2d3e4f",
			"resourceVersion":   "4711",
			"generation":        int64(1),
			"creationTimestamp": "2026-08-01T10:00:00Z",
			"managedFields": []any{map[string]any{
				"manager": "kubectl-client-side-apply", "operation": "Update",
				"apiVersion": "krsm.io/v1alpha1", "fieldsType": "FieldsV1",
			}},
		},
		"spec":   map[string]any{"allow": []any{map[string]any{"dim": "namespace", "namespace": namespace}}},
		"status": map[string]any{},
	}}
}

// TestDynamicContractGetterReadsTheNamedContract: the production L3 seam reads the CR
// the annotation names, from the namespace it names, through a LIVE GET of the
// krsm.io/v1alpha1 taskcontracts resource — the whole point being that no cache sits
// between a revoked contract and the verdict it would still authorise.
func TestDynamicContractGetterReadsTheNamedContract(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(contractScheme(), contractListKinds(),
		crObject("prod", "task-1"), crObject("staging", "task-1"))
	g := NewDynamicContractGetter(client)

	u, err := g.Get(context.Background(), "prod", "task-1")
	if err != nil {
		t.Fatalf("Get(prod/task-1): %v", err)
	}
	if u.GetNamespace() != "prod" || u.GetName() != "task-1" {
		t.Errorf("Get returned %s/%s, want prod/task-1 — the getter must read the NAMED namespace, not any namespace holding that name",
			u.GetNamespace(), u.GetName())
	}
	if got := u.GetAPIVersion(); got != "krsm.io/v1alpha1" {
		t.Errorf("apiVersion = %q, want krsm.io/v1alpha1", got)
	}

	// What the live getter RETURNS must be what the parse path ACCEPTS. This is the seam
	// the two halves meet at, and the one where a whole-document strict decode silently
	// turned every real contract into a scope-unresolved deny: the getter worked, the
	// parse rejected, and no test spanned both because the L3 fixtures were minimal
	// objects no server produces.
	raw, err := u.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	tc, err := contract.ParseCR(raw)
	if err != nil {
		t.Fatalf("the object the live getter returned must parse: %v", err)
	}
	if _, err := scope.Compile(tc); err != nil {
		t.Fatalf("the object the live getter returned must compile: %v", err)
	}
}

// TestDynamicContractGetterMapsNotFound: an absent CR — and equally an absent CRD, which
// the API server answers with the same 404 — is reported as ErrContractNotFound, so a
// caller can say "absent" without matching on an error string. Every other failure is
// passed through UNWRAPPED into a distinct value: resolveContract must not be able to
// mistake an RBAC denial or a transport failure for "the contract does not exist".
func TestDynamicContractGetterMapsNotFound(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(contractScheme(), contractListKinds())
	g := NewDynamicContractGetter(client)

	_, err := g.Get(context.Background(), "prod", "missing")
	if !errors.Is(err, ErrContractNotFound) {
		t.Errorf("absent CR: err = %v, want ErrContractNotFound", err)
	}

	// The absent-CRD shape: the API server 404s the whole resource path. The webhook
	// must keep serving (L0/L1 are untouched) and report the same absent outcome.
	noCRD := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(contractScheme(), contractListKinds())
	noCRD.PrependReactor("get", "taskcontracts", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "krsm.io", Resource: "taskcontracts"}, "task-1")
	})
	if _, err := NewDynamicContractGetter(noCRD).Get(context.Background(), "prod", "task-1"); !errors.Is(err, ErrContractNotFound) {
		t.Errorf("absent CRD: err = %v, want ErrContractNotFound", err)
	}

	// A FORBIDDEN read is not an absence. Reporting it as one would let an operator
	// read "no such contract" when the truth is "the reader may not see it".
	forbidden := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(contractScheme(), contractListKinds())
	forbidden.PrependReactor("get", "taskcontracts", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "krsm.io", Resource: "taskcontracts"}, "task-1", errors.New("no"))
	})
	_, err = NewDynamicContractGetter(forbidden).Get(context.Background(), "prod", "task-1")
	if err == nil || errors.Is(err, ErrContractNotFound) {
		t.Errorf("forbidden read: err = %v, want a non-not-found error", err)
	}
}

// TestDynamicContractGetterHonoursDeadline: an already-expired request deadline fails
// BEFORE the GET is issued. A contract read that outlives the admission deadline cannot
// be delivered, so spending the API call is pure latency on a request that is already
// destined to fail closed — and the guard must not depend on the client's own ctx
// handling, which is why it is asserted through a client that would happily answer.
func TestDynamicContractGetterHonoursDeadline(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(contractScheme(), contractListKinds(),
		crObject("prod", "task-1"))
	var issued bool
	client.PrependReactor("get", "taskcontracts", func(clienttesting.Action) (bool, runtime.Object, error) {
		issued = true
		return false, nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewDynamicContractGetter(client).Get(ctx, "prod", "task-1")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if issued {
		t.Error("a cancelled request must not issue the live GET")
	}
}

// TestDynamicContractGetterReadsTaskContractsGVR pins the resource the getter reads.
// The GVR is the contract between this seam and the CRD slice 7 installs; a silent
// change here (a wrong version, a singular resource) would make every L3 request fail
// closed in production while every hermetic test above still passed.
func TestDynamicContractGetterReadsTaskContractsGVR(t *testing.T) {
	want := schema.GroupVersionResource{Group: "krsm.io", Version: "v1alpha1", Resource: "taskcontracts"}
	if TaskContractGVR != want {
		t.Errorf("TaskContractGVR = %v, want %v", TaskContractGVR, want)
	}

	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(contractScheme(), contractListKinds(),
		crObject("prod", "task-1"))
	var got clienttesting.GetAction
	client.PrependReactor("get", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		got = a.(clienttesting.GetAction)
		return false, nil, nil
	})
	if _, err := NewDynamicContractGetter(client).Get(context.Background(), "prod", "task-1"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.GetResource() != want || got.GetNamespace() != "prod" || got.GetName() != "task-1" {
		t.Errorf("issued GET %#v, want %v prod/task-1", got, want)
	}
}

// TestDynamicContractGetterWithoutAClientFailsClosed: a getter holding no client
// reports an error rather than panicking. resolveContract turns any error into a
// scope-unresolved deny, so the fail-closed outcome survives a mis-wired serve path;
// a nil-pointer panic in the admission handler would not.
func TestDynamicContractGetterWithoutAClientFailsClosed(t *testing.T) {
	var g *DynamicContractGetter
	if _, err := g.Get(context.Background(), "prod", "task-1"); err == nil {
		t.Error("a getter with no dynamic client must return an error, not a contract")
	}
	if _, err := NewDynamicContractGetter(nil).Get(context.Background(), "prod", "task-1"); err == nil {
		t.Error("a getter built from a nil client must return an error, not a contract")
	}
}
