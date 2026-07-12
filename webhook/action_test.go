package webhook

import (
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/ridik-il/krsm/closure"
)

// testScope is the minimal clusterInfo for webhook tests: the corpus kinds the
// admission tests use, namespaced unless listed cluster-scoped (mirrors the loader),
// with an exact GVR→GVK table for the sub-resource parents.
type testScope struct{}

var testClusterScoped = map[string]bool{"Namespace": true, "PersistentVolume": true}

func (testScope) Namespaced(gvk closure.GVK) (bool, bool) {
	return !testClusterScoped[gvk.Kind], true
}

var testKindFor = map[string]closure.GVK{
	"apps/deployments": {Group: "apps", Version: "v1", Kind: "Deployment"},
	"/pods":            {Version: "v1", Kind: "Pod"},
}

func (testScope) KindFor(group, resource string) (closure.GVK, bool) {
	gvk, ok := testKindFor[group+"/"+resource]
	return gvk, ok
}

func raw(json string) runtime.RawExtension { return runtime.RawExtension{Raw: []byte(json)} }

// actionFor decodes the request payloads exactly as Handle does (once), then maps the
// action — the test-side mirror of the production decode→map sequence.
func actionFor(req *admissionv1.AdmissionRequest) (closure.Action, error) {
	oldU, err := decodePayload(req.OldObject.Raw)
	if err != nil {
		return closure.Action{}, err
	}
	newU, err := decodePayload(req.Object.Raw)
	if err != nil {
		return closure.Action{}, err
	}
	return actionFromRequest(req, testScope{}, oldU, newU)
}

// TestActionFromDeleteRequest (design test 1, S2 #16 webhook half): a DELETE
// AdmissionRequest maps to Verb=Delete with the request's REAL GVK, namespace, name and
// the oldObject's uid as the target — no discovery-guess resolution anywhere.
func TestActionFromDeleteRequest(t *testing.T) {
	req := &admissionv1.AdmissionRequest{
		UID:       "req-1",
		Operation: admissionv1.Delete,
		Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
		Namespace: "prod",
		Name:      "web",
		OldObject: raw(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"prod","uid":"uid-dep"}}`),
	}

	a, err := actionFor(req)
	if err != nil {
		t.Fatalf("actionFromRequest(DELETE) = %v, want nil", err)
	}
	if a.Verb != closure.Delete {
		t.Errorf("Verb = %q, want %q", a.Verb, closure.Delete)
	}
	wantGVK := closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}
	if a.Target.GVK != wantGVK || a.Target.Namespace != "prod" || a.Target.Name != "web" {
		t.Errorf("Target = %+v, want %v prod/web", a.Target, wantGVK)
	}
	if a.Target.UID != "uid-dep" {
		t.Errorf("Target.UID = %q, want %q (the oldObject's real uid)", a.Target.UID, "uid-dep")
	}
	if a.Old == nil || a.Old.Ref.UID != "uid-dep" {
		t.Errorf("Old = %+v, want the projected oldObject", a.Old)
	}
}

// TestActionCascadeFromPropagationPolicy (design test 2, S4 #18): the DELETE options'
// propagationPolicy decides Cascade — Orphan means the children are NOT deleted
// (Cascade:false); Background/Foreground/absent cascade. The CLI hardcoded true; the
// AdmissionReview carries the real intent.
func TestActionCascadeFromPropagationPolicy(t *testing.T) {
	cases := []struct {
		options string
		want    bool
	}{
		{`{"apiVersion":"meta.k8s.io/v1","kind":"DeleteOptions","propagationPolicy":"Orphan"}`, false},
		{`{"apiVersion":"meta.k8s.io/v1","kind":"DeleteOptions","propagationPolicy":"Background"}`, true},
		{`{"apiVersion":"meta.k8s.io/v1","kind":"DeleteOptions","propagationPolicy":"Foreground"}`, true},
		{``, true}, // no options → server default (cascade)
	}
	for _, c := range cases {
		req := &admissionv1.AdmissionRequest{
			Operation: admissionv1.Delete,
			Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			Namespace: "prod", Name: "web",
			OldObject: raw(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"prod","uid":"uid-dep"}}`),
		}
		if c.options != "" {
			req.Options = raw(c.options)
		}
		a, err := actionFor(req)
		if err != nil {
			t.Fatalf("actionFromRequest(options=%s) = %v, want nil", c.options, err)
		}
		if a.Cascade != c.want {
			t.Errorf("Cascade with options %q = %v, want %v", c.options, a.Cascade, c.want)
		}
	}
}

// TestActionFromUpdateRequest (design test 3a): an UPDATE request projects BOTH
// payloads into Action.Old/New — the input the selector/finalizer/config-mutation
// verdicts need and the CLI could never express.
func TestActionFromUpdateRequest(t *testing.T) {
	req := &admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "Service"},
		Namespace: "prod", Name: "svc",
		OldObject: raw(`{"apiVersion":"v1","kind":"Service","metadata":{"name":"svc","namespace":"prod","uid":"uid-svc"},"spec":{"selector":{"app":"a"}}}`),
		Object:    raw(`{"apiVersion":"v1","kind":"Service","metadata":{"name":"svc","namespace":"prod","uid":"uid-svc"},"spec":{"selector":{"app":"b"}}}`),
	}
	a, err := actionFor(req)
	if err != nil {
		t.Fatalf("actionFromRequest(UPDATE) = %v, want nil", err)
	}
	if a.Verb != closure.Update {
		t.Errorf("Verb = %q, want %q", a.Verb, closure.Update)
	}
	if a.Old == nil || a.Old.Selector.MatchLabels["app"] != "a" {
		t.Errorf("Old = %+v, want the projected old selector app=a", a.Old)
	}
	if a.New == nil || a.New.Selector.MatchLabels["app"] != "b" {
		t.Errorf("New = %+v, want the projected new selector app=b", a.New)
	}
}

// TestActionFromScaleSubresource (design test 4): an UPDATE on the "scale"
// sub-resource is a ScaleEffect on the PARENT resource — Verb=Scale, target from the
// request's parent kind/name, no payload projection (the Scale kind is not a workload).
func TestActionFromScaleSubresource(t *testing.T) {
	req := &admissionv1.AdmissionRequest{
		Operation:   admissionv1.Update,
		SubResource: "scale",
		Kind:        metav1.GroupVersionKind{Group: "autoscaling", Version: "v1", Kind: "Scale"},
		Resource:    metav1.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		Namespace:   "prod", Name: "web",
		Object:    raw(`{"apiVersion":"autoscaling/v1","kind":"Scale","metadata":{"name":"web","namespace":"prod"},"spec":{"replicas":0}}`),
		OldObject: raw(`{"apiVersion":"autoscaling/v1","kind":"Scale","metadata":{"name":"web","namespace":"prod"},"spec":{"replicas":3}}`),
	}
	a, err := actionFor(req)
	if err != nil {
		t.Fatalf("actionFromRequest(scale) = %v, want nil", err)
	}
	if a.Verb != closure.Scale {
		t.Errorf("Verb = %q, want %q", a.Verb, closure.Scale)
	}
	if a.Target.GVK.Kind == "Scale" {
		t.Errorf("Target.GVK = %+v — must be the PARENT resource, not the Scale wrapper", a.Target.GVK)
	}
	if a.Target.Namespace != "prod" || a.Target.Name != "web" {
		t.Errorf("Target = %+v, want prod/web", a.Target)
	}
}

// TestActionFromEviction (design test 5): a CREATE of pods/eviction IS a pod delete —
// admitting it as a mere CREATE would be a gate bypass. It maps to Verb=Delete on the
// pod named by the request.
func TestActionFromEviction(t *testing.T) {
	req := &admissionv1.AdmissionRequest{
		Operation:   admissionv1.Create,
		SubResource: "eviction",
		Kind:        metav1.GroupVersionKind{Group: "policy", Version: "v1", Kind: "Eviction"},
		Resource:    metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
		Namespace:   "prod", Name: "web-1",
		Object: raw(`{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"web-1","namespace":"prod"}}`),
	}
	a, err := actionFor(req)
	if err != nil {
		t.Fatalf("actionFromRequest(eviction) = %v, want nil", err)
	}
	if a.Verb != closure.Delete {
		t.Errorf("Verb = %q, want %q (eviction is a delete in disguise)", a.Verb, closure.Delete)
	}
	if a.Target.GVK.Kind != "Pod" || a.Target.Namespace != "prod" || a.Target.Name != "web-1" {
		t.Errorf("Target = %+v, want Pod prod/web-1", a.Target)
	}
}

// TestActionUnknownOperationFailsClosed (design test 6, approved lean 3): CONNECT and
// any unknown operation are errors — an operation the gate cannot classify is an
// unknown closure, never a silent admit.
func TestActionUnknownOperationFailsClosed(t *testing.T) {
	for _, op := range []admissionv1.Operation{admissionv1.Connect, admissionv1.Operation("FUTURE-OP")} {
		req := &admissionv1.AdmissionRequest{
			Operation: op,
			Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "Pod"},
			Namespace: "prod", Name: "web-1",
		}
		if _, err := actionFor(req); err == nil {
			t.Errorf("actionFromRequest(%s) = nil error, want fail-closed", op)
		}
	}
	// An unknown sub-resource parent and an ungated sub-resource are equally errors.
	req := &admissionv1.AdmissionRequest{
		Operation: admissionv1.Update, SubResource: "scale",
		Resource: metav1.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"},
	}
	if _, err := actionFor(req); err == nil {
		t.Error("scale on an untracked parent resource must fail closed")
	}
}

// TestActionUnparsablePayloadFailsClosed (design test 7): garbage payload JSON or a
// projection failure (unrecognised selector operator) is an error → the caller denies.
func TestActionUnparsablePayloadFailsClosed(t *testing.T) {
	base := func() *admissionv1.AdmissionRequest {
		return &admissionv1.AdmissionRequest{
			Operation: admissionv1.Delete,
			Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			Namespace: "prod", Name: "web",
		}
	}
	garbage := base()
	garbage.OldObject = raw(`{not json`)
	if _, err := actionFor(garbage); err == nil {
		t.Error("garbage oldObject JSON must fail closed")
	}
	badSelector := base()
	badSelector.OldObject = raw(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"prod","uid":"u"},"spec":{"selector":{"matchExpressions":[{"key":"app","operator":"Bogus"}]}}}`)
	if _, err := actionFor(badSelector); err == nil {
		t.Error("a projection failure must fail closed")
	}
}
