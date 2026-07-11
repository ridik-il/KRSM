package webhook

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/scope"
)

// cascadeState is the scenario-01 shape as literal objects: Deployment web owns
// ReplicaSet web-1 owns Pod web-1-a (labels app:web), and Service svc selects app:web.
// Deleting the Deployment closes over the ownership tree AND the Service — the Service
// escapes the derived (ownership-tree) scope.
func cascadeState() closure.State {
	dep := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}, Namespace: "prod", Name: "web", UID: "uid-d"}}
	rs := closure.Object{
		Ref:    closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "ReplicaSet"}, Namespace: "prod", Name: "web-1", UID: "uid-rs"},
		Owners: []closure.OwnerRef{{Kind: "Deployment", Name: "web", UID: "uid-d"}},
	}
	pod := closure.Object{
		Ref:    closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Pod"}, Namespace: "prod", Name: "web-1-a", UID: "uid-p"},
		Owners: []closure.OwnerRef{{Kind: "ReplicaSet", Name: "web-1", UID: "uid-rs"}},
		Labels: map[string]string{"app": "web"},
	}
	svc := closure.Object{
		Ref:      closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Service"}, Namespace: "prod", Name: "svc", UID: "uid-s"},
		Selector: closure.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
	}
	return closure.NewScanState([]closure.Object{dep, rs, pod, svc})
}

func deleteReview(uid string) admissionv1.AdmissionReview {
	return admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       types.UID(uid),
			Operation: admissionv1.Delete,
			Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			Namespace: "prod",
			Name:      "web",
			OldObject: raw(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"prod","uid":"uid-d","resourceVersion":"7"}}`),
		},
	}
}

// newTestServer builds a Server over the given state with sane test defaults.
func newTestServer(t *testing.T, st closure.State, mode scope.Mode, opts ...func(*Config)) *Server {
	t.Helper()
	c := Config{
		State:     st,
		ScopeInfo: testScope{},
		Synced:    func() bool { return true },
		Mode:      mode,
		Logf:      func(string, ...any) {},
	}
	for _, o := range opts {
		o(&c)
	}
	s, err := New(c)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestHandleEscapingDeleteEnforceAndAudit (design test 8): the same escaping delete is
// DENIED in enforce (reason names krsm:escape + the escaping Service) and ALLOWED with
// warnings in audit (same refs + the scope provenance) — the ADR-0011 split.
func TestHandleEscapingDeleteEnforceAndAudit(t *testing.T) {
	enforce := newTestServer(t, cascadeState(), scope.ModeEnforce)
	out := enforce.Handle(context.Background(), deleteReview("req-e"))
	if out.Response == nil {
		t.Fatal("Handle returned no response")
	}
	if out.Response.Allowed {
		t.Error("enforce: escaping delete must be denied")
	}
	msg := out.Response.Result.Message
	if !strings.Contains(msg, "krsm:escape") || !strings.Contains(msg, "svc") {
		t.Errorf("enforce message %q must carry krsm:escape and the escaping Service", msg)
	}

	audit := newTestServer(t, cascadeState(), scope.ModeAudit)
	out = audit.Handle(context.Background(), deleteReview("req-a"))
	if !out.Response.Allowed {
		t.Error("audit: an escaping delete is allowed-with-warnings, never denied")
	}
	warn := strings.Join(out.Response.Warnings, "\n")
	if !strings.Contains(warn, "svc") || !strings.Contains(warn, "derived:ownership-tree") {
		t.Errorf("audit warnings %q must name the escaping Service and the provenance", warn)
	}
}

// TestHandleInScopeDeleteAdmitted (design test 9): deleting an object with no
// collateral (closure = target only, covered by the derived scope) is admitted in
// both modes.
func TestHandleInScopeDeleteAdmitted(t *testing.T) {
	lonely := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "ConfigMap"}, Namespace: "prod", Name: "lonely", UID: "uid-cm"}}
	st := closure.NewScanState([]closure.Object{lonely})
	review := admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID: "req-ok", Operation: admissionv1.Delete,
			Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
			Namespace: "prod", Name: "lonely",
			OldObject: raw(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"lonely","namespace":"prod","uid":"uid-cm","resourceVersion":"3"}}`),
		},
	}
	for _, mode := range []scope.Mode{scope.ModeAudit, scope.ModeEnforce} {
		out := newTestServer(t, st, mode).Handle(context.Background(), review)
		if !out.Response.Allowed {
			t.Errorf("mode %s: in-scope delete must be admitted, got deny %q", mode, out.Response.Result.Message)
		}
	}
}

// TestHandleCreateAdmitted (design test 11): CREATE (except pods/eviction) is admitted
// BEFORE any state access — a to-be-created object has no existing closure, and
// closure.Verb deliberately has no Create. Proven by an UNSYNCED server: only a path
// that never consults state can admit here.
func TestHandleCreateAdmitted(t *testing.T) {
	s := newTestServer(t, cascadeState(), scope.ModeEnforce, func(c *Config) { c.Synced = func() bool { return false } })
	review := admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID: "req-create", Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "Pod"},
			Namespace: "prod", Name: "new-pod",
			Object: raw(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"new-pod","namespace":"prod"}}`),
		},
	}
	out := s.Handle(context.Background(), review)
	if !out.Response.Allowed {
		t.Errorf("CREATE must be admitted without touching state, got deny %q", out.Response.Result.Message)
	}
}

// TestHandleUnsyncedFailsClosedBothModes (design test 12): an unsynced cache is an
// unknown closure — a HARD deny with krsm:not-ready even in audit (Mode.Apply never
// softens a fail-closed deny).
func TestHandleUnsyncedFailsClosedBothModes(t *testing.T) {
	for _, mode := range []scope.Mode{scope.ModeAudit, scope.ModeEnforce} {
		s := newTestServer(t, cascadeState(), mode, func(c *Config) { c.Synced = func() bool { return false } })
		out := s.Handle(context.Background(), deleteReview("req-ns"))
		if out.Response.Allowed {
			t.Errorf("mode %s: unsynced cache must deny", mode)
		}
		if !strings.Contains(out.Response.Result.Message, "krsm:not-ready") {
			t.Errorf("mode %s: message %q must carry krsm:not-ready", mode, out.Response.Result.Message)
		}
	}
}

// TestHandleUnresolvableTargetFailsClosedThroughAudit (design test 13): a target the
// state does not know is Safe's fail-closed Block (empty Escaping) — audit must NOT
// soften it into a warn.
func TestHandleUnresolvableTargetFailsClosedThroughAudit(t *testing.T) {
	st := closure.NewScanState(nil) // knows nothing
	review := deleteReview("req-gone")
	out := newTestServer(t, st, scope.ModeAudit).Handle(context.Background(), review)
	if out.Response.Allowed {
		t.Error("audit: an unresolvable target must stay a hard deny (unknown closure)")
	}
}

// TestHandleSelectorChangeUpdate (design test 3b/14): a Service selector UPDATE reaches
// the old∪new match-set through the UNCHANGED closure.Safe — the pods the service is
// re-pointed at (and away from) enter the closure, escaping the derived scope (which
// covers only the service itself). The verdict the CLI could never express.
func TestHandleSelectorChangeUpdate(t *testing.T) {
	review := admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID: "req-sel", Operation: admissionv1.Update,
			Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "Service"},
			Namespace: "prod", Name: "svc",
			OldObject: raw(`{"apiVersion":"v1","kind":"Service","metadata":{"name":"svc","namespace":"prod","uid":"uid-s","resourceVersion":"5"},"spec":{"selector":{"app":"web"}}}`),
			Object:    raw(`{"apiVersion":"v1","kind":"Service","metadata":{"name":"svc","namespace":"prod","uid":"uid-s","resourceVersion":"5"},"spec":{"selector":{"app":"api"}}}`),
		},
	}
	out := newTestServer(t, cascadeState(), scope.ModeEnforce).Handle(context.Background(), review)
	if out.Response.Allowed {
		t.Fatal("re-pointing a Service selector must be gated: the old match-set pods escape the derived scope")
	}
	if !strings.Contains(out.Response.Result.Message, "web-1-a") {
		t.Errorf("message %q must name the old-selector pod entering the closure", out.Response.Result.Message)
	}
}

// TestHandleResponseInvariants (design test 16): request.uid is echoed on EVERY path,
// patch is never set, and no reason/warning ever carries object data — even when the
// closure includes a data-bearing Secret.
func TestHandleResponseInvariants(t *testing.T) {
	secret := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Secret"}, Namespace: "prod", Name: "db", UID: "uid-sec"}}
	pod := closure.Object{
		Ref:       closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Pod"}, Namespace: "prod", Name: "user", UID: "uid-pod"},
		CrossRefs: []closure.CrossRef{{Kind: closure.RefVolume, Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Secret"}, Namespace: "prod", Name: "db", UID: "uid-sec"}}},
	}
	st := closure.NewScanState([]closure.Object{secret, pod})

	secretDelete := admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID: "req-sec", Operation: admissionv1.Delete,
			Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "Secret"},
			Namespace: "prod", Name: "db",
			OldObject: raw(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","namespace":"prod","uid":"uid-sec","resourceVersion":"9"},"data":{"password":"VE9QU0VDUkVU"}}`),
		},
	}

	reviews := map[string]admissionv1.AdmissionReview{
		"escape-deny":  deleteReview("uid-1"),
		"secret-deny":  secretDelete,
		"unmappable":   {Request: &admissionv1.AdmissionRequest{UID: "uid-2", Operation: admissionv1.Connect, Kind: metav1.GroupVersionKind{Version: "v1", Kind: "Pod"}, Name: "p", Namespace: "prod"}},
		"create-allow": {Request: &admissionv1.AdmissionRequest{UID: "uid-3", Operation: admissionv1.Create, Kind: metav1.GroupVersionKind{Version: "v1", Kind: "Pod"}, Name: "p", Namespace: "prod"}},
	}
	for name, review := range reviews {
		s := newTestServer(t, st, scope.ModeEnforce)
		if name == "escape-deny" {
			s = newTestServer(t, cascadeState(), scope.ModeEnforce)
		}
		out := s.Handle(context.Background(), review)
		if out.Response == nil {
			t.Fatalf("%s: nil response", name)
		}
		if out.Response.UID != review.Request.UID {
			t.Errorf("%s: uid = %q, want %q echoed", name, out.Response.UID, review.Request.UID)
		}
		if out.Response.Patch != nil || out.Response.PatchType != nil {
			t.Errorf("%s: a validating webhook must never set a patch", name)
		}
		blob := strings.Join(out.Response.Warnings, " ")
		if out.Response.Result != nil {
			blob += " " + out.Response.Result.Message
		}
		if strings.Contains(blob, "VE9QU0VDUkVU") || strings.Contains(blob, "TOPSECRET") {
			t.Errorf("%s: response leaks secret data: %q", name, blob)
		}
	}
}
