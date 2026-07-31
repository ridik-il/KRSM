package webhook

// Regression tests for the PR #36 review findings — each pins a fix so the defect
// cannot silently return. Numbering follows the review record appended to
// docs/design/v0.5-slice5-webhook-server.md.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/scope"
)

const agentSA = "system:serviceaccount:agents:remediator"

// scaleReview is a scale-subresource UPDATE by the given username: the payload is the
// autoscaling/v1 Scale wrapper, whose metadata structurally CANNOT carry the parent's
// annotations.
func scaleReview(uid, username string) admissionv1.AdmissionReview {
	return admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:         types.UID("req-" + uid),
			Operation:   admissionv1.Update,
			SubResource: "scale",
			Kind:        metav1.GroupVersionKind{Group: "autoscaling", Version: "v1", Kind: "Scale"},
			Resource:    metav1.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			Namespace:   "prod", Name: "web",
			UserInfo:  authenticationv1.UserInfo{Username: username},
			Object:    raw(`{"apiVersion":"autoscaling/v1","kind":"Scale","metadata":{"name":"web","namespace":"prod","resourceVersion":"7"},"spec":{"replicas":0}}`),
			OldObject: raw(`{"apiVersion":"autoscaling/v1","kind":"Scale","metadata":{"name":"web","namespace":"prod","resourceVersion":"7"},"spec":{"replicas":3}}`),
		},
	}
}

// TestServiceAccountMatcherCoversPayloadBlindRequests (review finding 1): the
// annotation matcher is structurally blind to scale/eviction payloads; the
// serviceaccount matcher gates them by request identity, and AnyMatcher combines both.
func TestServiceAccountMatcherCoversPayloadBlindRequests(t *testing.T) {
	req := scaleReview("sa", agentSA).Request

	if matches(t, AnnotationMatcher{Key: "krsm.io/task"}, req) {
		t.Error("annotation matcher must NOT match a Scale payload (structurally annotation-free) — if this starts matching, re-evaluate the ServiceAccountMatcher requirement")
	}
	sa := NewServiceAccountMatcher([]string{agentSA})
	if !matches(t, sa, req) {
		t.Error("serviceaccount matcher must match the agent's username")
	}
	req.UserInfo.Username = "system:serviceaccount:kube-system:hpa-controller"
	if matches(t, sa, req) {
		t.Error("serviceaccount matcher must not match other identities (HPA must stay ungated)")
	}
	both := AnyMatcher{sa, AnnotationMatcher{Key: "krsm.io/task"}}
	req.UserInfo.Username = agentSA
	if !matches(t, both, req) {
		t.Error("AnyMatcher must match when any member matches")
	}
}

// TestHandleScaleAndEvictionGatedByIdentity (review finding 1): with a
// serviceaccount matcher, agent scale/eviction requests reach the gate (proven by the
// fail-closed not-ready deny on an unsynced cache) while identical requests from any
// other identity are admitted untouched.
func TestHandleScaleAndEvictionGatedByIdentity(t *testing.T) {
	newServer := func() *Server {
		return newTestServer(t, cascadeState(), scope.ModeEnforce, func(c *Config) {
			c.Matcher = NewServiceAccountMatcher([]string{agentSA})
			c.Synced = func() bool { return false }
		})
	}

	eviction := admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID: "req-evict", Operation: admissionv1.Create, SubResource: "eviction",
			Kind:      metav1.GroupVersionKind{Group: "policy", Version: "v1", Kind: "Eviction"},
			Resource:  metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
			Namespace: "prod", Name: "web-1-a",
			UserInfo: authenticationv1.UserInfo{Username: agentSA},
			Object:   raw(`{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"web-1-a","namespace":"prod"}}`),
		},
	}

	for name, review := range map[string]admissionv1.AdmissionReview{
		"scale":    scaleReview("scale", agentSA),
		"eviction": eviction,
	} {
		out := newServer().Handle(context.Background(), review)
		if out.Response.Allowed {
			t.Errorf("%s by the agent SA must reach the gate (expected not-ready deny), got admit", name)
		} else if !strings.Contains(out.Response.Result.Message, "krsm:not-ready") {
			t.Errorf("%s: message %q, want the gate's krsm:not-ready", name, out.Response.Result.Message)
		}
	}

	other := scaleReview("other", "system:serviceaccount:kube-system:horizontal-pod-autoscaler")
	if out := newServer().Handle(context.Background(), other); !out.Response.Allowed {
		t.Errorf("a non-agent scale (HPA) must be admitted untouched, got %q", out.Response.Result.Message)
	}
}

// TestHandleConnectFailsClosedForAgents (review finding 2): a real CONNECT always
// carries a sub-resource (exec/attach/…); for an agent-matched request it must reach
// the fail-closed krsm:invalid deny, not the ungated-sub-resource admit.
func TestHandleConnectFailsClosedForAgents(t *testing.T) {
	review := admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID: "req-exec", Operation: admissionv1.Connect, SubResource: "exec",
			Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "PodExecOptions"},
			Resource:  metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
			Namespace: "prod", Name: "web-1-a",
			Object: raw(`{"apiVersion":"v1","kind":"PodExecOptions","command":["sh"]}`),
		},
	}
	// Gate-everything matcher (the default): the agent CONNECT is unclassifiable →
	// fail closed.
	out := newTestServer(t, cascadeState(), scope.ModeAudit).Handle(context.Background(), review)
	if out.Response.Allowed {
		t.Error("an agent-matched CONNECT must fail closed (unknown closure)")
	}
	if !strings.Contains(out.Response.Result.Message, "krsm:invalid") {
		t.Errorf("message %q must carry krsm:invalid", out.Response.Result.Message)
	}
	// Non-agent CONNECT (annotation matcher cannot see exec options): admitted —
	// the documented residual until identity matching is configured.
	s := newTestServer(t, cascadeState(), scope.ModeEnforce, func(c *Config) { c.Matcher = AnnotationMatcher{Key: "krsm.io/task"} })
	if out := s.Handle(context.Background(), review); !out.Response.Allowed {
		t.Error("a non-agent CONNECT must be admitted untouched")
	}
}

// TestHandleDeadlineExceededDeniesInternal (review finding 5): a deadline that expires
// OUTSIDE the freshness seam (here: before/during the closure walk) yields the
// taxonomy's krsm:internal deny, never a verdict served as if timely.
func TestHandleDeadlineExceededDeniesInternal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // expired before Handle runs any compute
	out := newTestServer(t, cascadeState(), scope.ModeAudit).Handle(ctx, deleteReview("req-deadline"))
	if out.Response.Allowed {
		t.Error("an expired deadline must fail closed in every mode")
	}
	if !strings.Contains(out.Response.Result.Message, "krsm:internal") {
		t.Errorf("message %q must carry krsm:internal (deadline outside freshness)", out.Response.Result.Message)
	}
}

// TestDecisionResponseNamesExternalRefsOnBlock (review finding 10): an enforce-mode
// Block reports external effects with the SAME message grammar the audit warning uses.
func TestDecisionResponseNamesExternalRefsOnBlock(t *testing.T) {
	ext := closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "PersistentVolume"}, Name: "pv-1"}
	dec := closure.Decision{Verdict: closure.Block, Reason: "finalizer removal orphans external resources", External: []closure.Ref{ext}}
	resp := decisionResponse(dec, scope.ProvenanceDerivedOwner)
	if resp.Allowed {
		t.Fatal("Block must deny")
	}
	if !strings.Contains(resp.Result.Message, "external:") || !strings.Contains(resp.Result.Message, "pv-1") {
		t.Errorf("Block message %q must name the external refs like the Warn branch does", resp.Result.Message)
	}
}

// TestHandleStampsResponseTypeMeta (review nit): every Handle caller receives a
// complete admission/v1 review — the TypeMeta invariant lives in respond, not only
// on the HTTP path.
func TestHandleStampsResponseTypeMeta(t *testing.T) {
	review := deleteReview("req-typemeta")
	review.TypeMeta = metav1.TypeMeta{} // hermetic callers often omit it
	out := newTestServer(t, cascadeState(), scope.ModeAudit).Handle(context.Background(), review)
	if out.APIVersion != "admission.k8s.io/v1" || out.Kind != "AdmissionReview" {
		t.Errorf("response TypeMeta = %q/%q, want admission.k8s.io/v1 AdmissionReview", out.APIVersion, out.Kind)
	}
}

// TestServeHTTPDecodeFailureRecoversUID (review finding 4): a body that fails the
// strict AdmissionReview decode but still carries request.uid gets a 200 +
// krsm:invalid deny addressed to that uid — only a uid-less body is a 400.
func TestServeHTTPDecodeFailureRecoversUID(t *testing.T) {
	mux := NewMux(newTestServer(t, cascadeState(), scope.ModeEnforce))
	// dryRun is *bool in admission/v1 — the string makes the typed decode fail
	// while the loose uid peek still succeeds.
	body := `{"request":{"uid":"u-recover","dryRun":"yes"}}`
	req := httptest.NewRequest(http.MethodPost, "/validate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("decode failure with recoverable uid = %d, want 200 + deny", rec.Code)
	}
	out := decodeResponse(t, rec)
	if out.Response.Allowed || out.Response.UID != "u-recover" {
		t.Errorf("response = %+v, want a deny addressed to u-recover", out.Response)
	}
	if !strings.Contains(out.Response.Result.Message, "krsm:invalid") {
		t.Errorf("message %q must carry krsm:invalid", out.Response.Result.Message)
	}
}

// TestServeHTTPAcceptsMediaTypeParams (review finding 7): a Content-Type with a
// parameter ("application/json; charset=utf-8") is still application/json — rejecting
// it would turn every admission into a webhook failure behind such an intermediary.
func TestServeHTTPAcceptsMediaTypeParams(t *testing.T) {
	mux := NewMux(newTestServer(t, cascadeState(), scope.ModeEnforce))
	body, err := json.Marshal(deleteReview("req-charset"))
	if err != nil {
		t.Fatalf("marshal review: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/validate", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("charset param = %d, want 200", rec.Code)
	}
	if out := decodeResponse(t, rec); out.Response.UID != "req-charset" {
		t.Errorf("uid = %q, want req-charset", out.Response.UID)
	}
}
