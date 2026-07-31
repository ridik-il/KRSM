package webhook

// Round-2 review regression tests (PR #36, docs/design/v0.5-slice5-review2-fixes.md).
// Each test pins one finding's fix; the file mirrors review_test.go (round 1).

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/scope"
)

// ephemeralContainersReview is an agent UPDATE to pods/<name> via the
// ephemeralcontainers sub-resource — the ONLY way to inject an ephemeral container.
// The sub-resource carries full Pod objects in object/oldObject.
func ephemeralContainersReview(uid, pod, podUID string) admissionv1.AdmissionReview {
	podJSON := func(extra string) string {
		return `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"` + pod + `","namespace":"prod","uid":"` + podUID + `","resourceVersion":"4","labels":{"app":"web"}},"spec":{` + extra + `}}`
	}
	return admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:         types.UID(uid),
			Operation:   admissionv1.Update,
			SubResource: "ephemeralcontainers",
			Resource:    metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
			Kind:        metav1.GroupVersionKind{Version: "v1", Kind: "Pod"},
			Namespace:   "prod",
			Name:        pod,
			OldObject:   raw(podJSON(``)),
			Object:      raw(podJSON(`"ephemeralContainers":[{"name":"debug","image":"busybox"}]`)),
		},
	}
}

// TestHandleEphemeralContainersGated (round-2 finding 1, design test 1): injecting an
// ephemeral container is a pod mutation and must be evaluated, not admitted as an
// ungated sub-resource. A routing-bound pod's closure includes its binding Service
// (closure rule 7), which escapes the pod-rooted derived tree → flagged; an unbound
// pod's closure is itself → allowed.
func TestHandleEphemeralContainersGated(t *testing.T) {
	// cascadeState: Pod web-1-a (app:web) is selected by Service svc.
	enforce := newTestServer(t, cascadeState(), scope.ModeEnforce)
	out := enforce.Handle(context.Background(), ephemeralContainersReview("req-ec-e", "web-1-a", "uid-p"))
	if out.Response.Allowed {
		t.Error("enforce: ephemeral-container injection into a Service-bound pod must be denied (Service escapes the pod's tree)")
	} else if msg := out.Response.Result.Message; !strings.Contains(msg, "krsm:escape") || !strings.Contains(msg, "svc") {
		t.Errorf("enforce message %q must carry krsm:escape and the escaping Service", msg)
	}

	audit := newTestServer(t, cascadeState(), scope.ModeAudit)
	out = audit.Handle(context.Background(), ephemeralContainersReview("req-ec-a", "web-1-a", "uid-p"))
	if !out.Response.Allowed {
		t.Error("audit: the same injection is allowed-with-warnings, never denied")
	} else if warn := strings.Join(out.Response.Warnings, "\n"); !strings.Contains(warn, "svc") {
		t.Errorf("audit warnings %q must name the escaping Service", warn)
	}

	// An unbound pod: closure = the pod itself, inside its own derived tree.
	lonely := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Pod"}, Namespace: "prod", Name: "loner", UID: "uid-l"}}
	st := closure.NewScanState([]closure.Object{lonely})
	out = newTestServer(t, st, scope.ModeEnforce).Handle(context.Background(), ephemeralContainersReview("req-ec-ok", "loner", "uid-l"))
	if !out.Response.Allowed {
		t.Errorf("an unbound pod's ephemeral-container injection is in scope, got deny %q", out.Response.Result.Message)
	}
}

// repGatedRequests holds one representative AdmissionRequest per gated sub-resource, so
// the drift guard below iterates the table itself and fails loudly if a new entry is
// added without a representative (and thus without coverage).
func repGatedRequests() map[string]*admissionv1.AdmissionRequest {
	pod := func(extra string) runtime.RawExtension {
		return raw(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"web-1-a","namespace":"prod","uid":"uid-p","resourceVersion":"4","labels":{"app":"web"}},"spec":{` + extra + `}}`)
	}
	return map[string]*admissionv1.AdmissionRequest{
		"scale": {
			Operation: admissionv1.Update, SubResource: "scale",
			Kind:      metav1.GroupVersionKind{Group: "autoscaling", Version: "v1", Kind: "Scale"},
			Resource:  metav1.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			Namespace: "prod", Name: "web",
			OldObject: raw(`{"apiVersion":"autoscaling/v1","kind":"Scale","metadata":{"name":"web","namespace":"prod","resourceVersion":"7"},"spec":{"replicas":3}}`),
			Object:    raw(`{"apiVersion":"autoscaling/v1","kind":"Scale","metadata":{"name":"web","namespace":"prod","resourceVersion":"7"},"spec":{"replicas":0}}`),
		},
		"eviction": {
			Operation: admissionv1.Create, SubResource: "eviction",
			Kind:      metav1.GroupVersionKind{Group: "policy", Version: "v1", Kind: "Eviction"},
			Resource:  metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
			Namespace: "prod", Name: "web-1-a",
			Object: raw(`{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"web-1-a","namespace":"prod"}}`),
		},
		"ephemeralcontainers": {
			Operation: admissionv1.Update, SubResource: "ephemeralcontainers",
			Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "Pod"},
			Resource:  metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
			Namespace: "prod", Name: "web-1-a",
			OldObject: pod(``),
			Object:    pod(`"ephemeralContainers":[{"name":"debug","image":"busybox"}]`),
		},
	}
}

// TestGatedSubResourceTableNoDrift (design test 3): the gate table is the single source
// of truth. For every entry, (a) actionFromRequest has a live switch arm producing the
// gate's verb; (b) a viaCreate entry is NOT fast-admitted by the CREATE branch (it
// reaches the sync gate); (c) a !carriesRV entry (eviction) is not rv-guarded — it is
// evaluated, never stale-denied for a missing resourceVersion.
func TestGatedSubResourceTableNoDrift(t *testing.T) {
	reps := repGatedRequests()
	for sub, gate := range gatedSubResources {
		req, ok := reps[sub]
		if !ok {
			t.Fatalf("no representative request for gated sub-resource %q — add one so the drift guard covers it", sub)
		}
		// (a) the switch arm exists and yields the gate's verb.
		oldU, err := decodePayload(req.OldObject.Raw)
		if err != nil {
			t.Fatalf("%s: decode oldObject: %v", sub, err)
		}
		newU, err := decodePayload(req.Object.Raw)
		if err != nil {
			t.Fatalf("%s: decode object: %v", sub, err)
		}
		a, err := actionFromRequest(req, testScope{}, oldU, newU)
		if err != nil {
			t.Fatalf("%s: actionFromRequest: %v", sub, err)
		}
		if a.Verb != gate.verb {
			t.Errorf("%s: verb = %v, want the table's %v", sub, a.Verb, gate.verb)
		}

		// (b) viaCreate: a CREATE is not fast-admitted — it must reach the sync gate.
		if gate.viaCreate {
			s := newTestServer(t, cascadeState(), scope.ModeEnforce, func(c *Config) { c.Synced = func() bool { return false } })
			out := s.Handle(context.Background(), admissionv1.AdmissionReview{Request: req})
			if out.Response.Allowed {
				t.Errorf("%s: a viaCreate sub-resource must not be fast-admitted on CREATE", sub)
			} else if !strings.Contains(out.Response.Result.Message, "krsm:not-ready") {
				t.Errorf("%s: reached beyond the CREATE branch but not the sync gate: %q", sub, out.Response.Result.Message)
			}
		}

		// (c) !carriesRV: with a freshness guard configured and no request rv, the
		// action is still evaluated (never denied krsm:stale).
		if !gate.carriesRV {
			s := newTestServer(t, cascadeState(), scope.ModeEnforce, func(c *Config) { c.Fresh = &fakeFresh{} })
			out := s.Handle(context.Background(), admissionv1.AdmissionReview{Request: req})
			if out.Response.Result != nil && strings.Contains(out.Response.Result.Message, "krsm:stale") {
				t.Errorf("%s: a !carriesRV sub-resource must skip the rv precheck, got %q", sub, out.Response.Result.Message)
			}
		}
	}
}

// TestActionEvictionCascadeFromDeleteOptions (design test 4, F8): eviction Cascade comes
// from the Eviction body's deleteOptions.propagationPolicy — Orphan → false; any other
// policy, an absent deleteOptions, or garbage → true (conservative).
func TestActionEvictionCascadeFromDeleteOptions(t *testing.T) {
	evict := func(body string) *admissionv1.AdmissionRequest {
		return &admissionv1.AdmissionRequest{
			Operation: admissionv1.Create, SubResource: "eviction",
			Kind:      metav1.GroupVersionKind{Group: "policy", Version: "v1", Kind: "Eviction"},
			Resource:  metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
			Namespace: "prod", Name: "web-1-a",
			Object: raw(body),
		}
	}
	base := `{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"web-1-a","namespace":"prod"}`
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"orphan", base + `,"deleteOptions":{"propagationPolicy":"Orphan"}}`, false},
		{"background", base + `,"deleteOptions":{"propagationPolicy":"Background"}}`, true},
		{"foreground", base + `,"deleteOptions":{"propagationPolicy":"Foreground"}}`, true},
		{"absent", base + `}`, true},
	}
	for _, c := range cases {
		a, err := actionFor(evict(c.body))
		if err != nil {
			t.Fatalf("%s: actionFromRequest: %v", c.name, err)
		}
		if a.Verb != closure.Delete {
			t.Errorf("%s: verb = %v, want Delete", c.name, a.Verb)
		}
		if a.Cascade != c.want {
			t.Errorf("%s: Cascade = %v, want %v", c.name, a.Cascade, c.want)
		}
	}
}

// TestServiceAccountMatcherTrimsList (design test 5, F2): a comma-split flag value like
// "a, b" gates BOTH a and b — each entry is trimmed and empties dropped, so the
// untrimmed " b" is never a live (silently-ungated) identity.
func TestServiceAccountMatcherTrimsList(t *testing.T) {
	m := NewServiceAccountMatcher(strings.Split("a, b, ,", ","))
	for _, u := range []string{"a", "b"} {
		req := &admissionv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{Username: u}}
		if !m.Matches(req, nil, nil) {
			t.Errorf("username %q must be gated", u)
		}
	}
	untrimmed := &admissionv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{Username: " b"}}
	if m.Matches(untrimmed, nil, nil) {
		t.Error(`untrimmed " b" must not be a live identity`)
	}
	if len(m.Usernames) != 2 {
		t.Errorf("Usernames = %v, want exactly {a, b}", m.Usernames)
	}
}

// TestMatcherSemanticsAndNeedsPayload (design test 6): AnnotationMatcher{Key:""} matches
// nothing; MatchAll matches everything; NeedsPayload is false for identity/MatchAll,
// true for a keyed annotation, and the OR of children for AnyMatcher.
func TestMatcherSemanticsAndNeedsPayload(t *testing.T) {
	empty := &admissionv1.AdmissionRequest{}
	if (AnnotationMatcher{}).Matches(empty, nil, nil) {
		t.Error(`AnnotationMatcher{Key:""} must match nothing`)
	}
	if !(MatchAll{}).Matches(empty, nil, nil) {
		t.Error("MatchAll must match everything")
	}
	sa := NewServiceAccountMatcher([]string{"x"})
	annot := AnnotationMatcher{Key: "k"}
	needs := map[string]bool{
		"MatchAll":           (MatchAll{}).NeedsPayload(),
		"ServiceAccount":     sa.NeedsPayload(),
		"AnnotationDisabled": (AnnotationMatcher{}).NeedsPayload(),
		"AnnotationKeyed":    annot.NeedsPayload(),
		"AnyIdentityOnly":    AnyMatcher{sa}.NeedsPayload(),
		"AnyWithKeyedAnnot":  AnyMatcher{sa, annot}.NeedsPayload(),
	}
	want := map[string]bool{
		"MatchAll": false, "ServiceAccount": false, "AnnotationDisabled": false,
		"AnnotationKeyed": true, "AnyIdentityOnly": false, "AnyWithKeyedAnnot": true,
	}
	for k, w := range want {
		if needs[k] != w {
			t.Errorf("%s.NeedsPayload() = %v, want %v", k, needs[k], w)
		}
	}
}

// recordingMatcher records whether it was ever handed a non-nil payload (i.e. whether
// stage 2 ran) — the fake that proves the payload-free hot path.
type recordingMatcher struct {
	match, needs, sawPayload bool
}

func (r *recordingMatcher) Matches(_ *admissionv1.AdmissionRequest, object, oldObject *unstructured.Unstructured) bool {
	if object != nil || oldObject != nil {
		r.sawPayload = true
	}
	return r.match
}
func (r *recordingMatcher) NeedsPayload() bool { return r.needs }

// TestHandleTwoStageMatching (design test 7, F5): an identity-only matcher admits a
// non-agent request with ZERO payload decodes; a keyed annotation matcher admits an
// undecodable payload (it cannot carry the annotation); an identity-matched agent with
// an undecodable payload still fails closed (krsm:invalid).
func TestHandleTwoStageMatching(t *testing.T) {
	poison := raw(`{not json`)

	// (a) identity-only, non-agent: admitted, and the matcher never sees a payload.
	rm := &recordingMatcher{match: false, needs: false}
	review := deleteReview("req-2s-a")
	review.Request.OldObject = poison
	out := newTestServer(t, cascadeState(), scope.ModeEnforce, func(c *Config) { c.Matcher = rm }).
		Handle(context.Background(), review)
	if !out.Response.Allowed {
		t.Errorf("identity-miss must admit, got %q", out.Response.Result.Message)
	}
	if rm.sawPayload {
		t.Error("stage 2 must not run for a !NeedsPayload matcher — the payload was decoded")
	}

	// (b) keyed annotation matcher, undecodable payload: admitted.
	review = deleteReview("req-2s-b")
	review.Request.OldObject = poison
	out = newTestServer(t, cascadeState(), scope.ModeEnforce, func(c *Config) { c.Matcher = AnnotationMatcher{Key: "krsm.io/task"} }).
		Handle(context.Background(), review)
	if !out.Response.Allowed {
		t.Errorf("annotation matcher + undecodable payload must admit, got %q", out.Response.Result.Message)
	}

	// (c) SA-matched agent, undecodable payload: fail closed.
	sa := NewServiceAccountMatcher([]string{"system:serviceaccount:agents:remediator"})
	review = deleteReview("req-2s-c")
	review.Request.UserInfo = authenticationv1.UserInfo{Username: "system:serviceaccount:agents:remediator"}
	review.Request.OldObject = poison
	out = newTestServer(t, cascadeState(), scope.ModeEnforce, func(c *Config) { c.Matcher = sa }).
		Handle(context.Background(), review)
	if out.Response.Allowed {
		t.Error("an SA-matched agent with an undecodable payload must fail closed")
	} else if !strings.Contains(out.Response.Result.Message, "krsm:invalid") {
		t.Errorf("message %q must carry krsm:invalid", out.Response.Result.Message)
	}
}

// TestHandleConnectGating (design test 8): under the two-stage order an agent CONNECT is
// still denied krsm:invalid (an unclassifiable operation), while a non-agent CONNECT is
// admitted at stage 1 (payload-free miss).
func TestHandleConnectGating(t *testing.T) {
	connect := func(uid string) admissionv1.AdmissionReview {
		return admissionv1.AdmissionReview{Request: &admissionv1.AdmissionRequest{
			UID: types.UID(uid), Operation: admissionv1.Connect, SubResource: "exec",
			Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "Pod"},
			Resource:  metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
			Namespace: "prod", Name: "web-1-a",
		}}
	}
	sa := NewServiceAccountMatcher([]string{"agent"})
	agent := connect("req-conn-agent")
	agent.Request.UserInfo = authenticationv1.UserInfo{Username: "agent"}
	out := newTestServer(t, cascadeState(), scope.ModeEnforce, func(c *Config) { c.Matcher = sa }).
		Handle(context.Background(), agent)
	if out.Response.Allowed {
		t.Error("agent CONNECT must be denied (unclassifiable operation)")
	} else if !strings.Contains(out.Response.Result.Message, "krsm:invalid") {
		t.Errorf("message %q must carry krsm:invalid", out.Response.Result.Message)
	}

	human := connect("req-conn-human") // empty UserInfo → stage-1 miss
	out = newTestServer(t, cascadeState(), scope.ModeEnforce, func(c *Config) { c.Matcher = sa }).
		Handle(context.Background(), human)
	if !out.Response.Allowed {
		t.Errorf("non-agent CONNECT must be admitted, got %q", out.Response.Result.Message)
	}
}

// TestRespondEchoesRequestTypeMeta (design test 12, F6): the response echoes a complete
// request TypeMeta (a v1beta1 request gets a v1beta1 response) and is stamped
// admission/v1 when the request carried none.
func TestRespondEchoesRequestTypeMeta(t *testing.T) {
	v1beta1 := deleteReview("req-tm-beta")
	v1beta1.TypeMeta = metav1.TypeMeta{APIVersion: "admission.k8s.io/v1beta1", Kind: "AdmissionReview"}
	out := newTestServer(t, cascadeState(), scope.ModeEnforce).Handle(context.Background(), v1beta1)
	if out.APIVersion != "admission.k8s.io/v1beta1" || out.Kind != "AdmissionReview" {
		t.Errorf("response TypeMeta = %+v, want the echoed v1beta1", out.TypeMeta)
	}

	none := deleteReview("req-tm-none")
	none.TypeMeta = metav1.TypeMeta{}
	out = newTestServer(t, cascadeState(), scope.ModeEnforce).Handle(context.Background(), none)
	if out.APIVersion != "admission.k8s.io/v1" || out.Kind != "AdmissionReview" {
		t.Errorf("empty request TypeMeta must be stamped admission/v1, got %+v", out.TypeMeta)
	}
}

// TestHandleFreshnessFastPathServesFirstDecision (design test 10, F9): reconciled=false
// means the cache was already current, so the FIRST decision is served without a second
// closure walk — proven by a state that would flip to in-scope on a recompute yet the
// original escaping (deny) decision stands.
func TestHandleFreshnessFastPathServesFirstDecision(t *testing.T) {
	dep := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}, Namespace: "prod", Name: "web", UID: "uid-d"}}
	inScope := closure.NewScanState([]closure.Object{dep})
	sw := &switchState{State: cascadeState()}
	fresh := &fakeFresh{reconciled: false, onCheck: func() { sw.State = inScope }}
	s := newTestServer(t, sw, scope.ModeEnforce, func(c *Config) { c.Fresh = fresh })

	out := s.Handle(context.Background(), deleteReview("req-fast"))
	if !fresh.called {
		t.Fatal("freshness guard was not consulted")
	}
	if out.Response.Allowed {
		t.Error("reconciled=false must serve the first (escaping) decision, not recompute it to allow")
	}
}

// TestNewRejectsNilFresh (design test 11): Fresh is constructor-required — disabling the
// staleness guard must be spelled NoFreshness, never a nil default.
func TestNewRejectsNilFresh(t *testing.T) {
	_, err := New(Config{
		State: cascadeState(), ScopeInfo: testScope{},
		Synced: func() bool { return true }, Mode: scope.ModeEnforce,
	})
	if err == nil {
		t.Error("New must reject a nil Fresh")
	}
	if _, err := New(Config{
		State: cascadeState(), ScopeInfo: testScope{},
		Synced: func() bool { return true }, Mode: scope.ModeEnforce, Fresh: NoFreshness,
	}); err != nil {
		t.Errorf("New with NoFreshness must succeed, got %v", err)
	}
}

// TestNoFreshnessSkipsGuard (design test 11): with NoFreshness even a request carrying no
// resourceVersion is not stale-denied — the rv precheck and guard are disabled — while
// the closure verdict still stands (an escaping delete is still denied escape).
func TestNoFreshnessSkipsGuard(t *testing.T) {
	review := deleteReview("req-nofresh")
	review.Request.OldObject = raw(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"prod","uid":"uid-d"}}`)
	out := newTestServer(t, cascadeState(), scope.ModeEnforce).Handle(context.Background(), review)
	if out.Response.Result != nil && strings.Contains(out.Response.Result.Message, "krsm:stale") {
		t.Errorf("NoFreshness must skip the rv precheck, got %q", out.Response.Result.Message)
	}
	if out.Response.Allowed {
		t.Error("the closure verdict still stands: an escaping delete is denied escape")
	}
}

// untrackedDeleteReview deletes a CRD kind the informer set does not watch.
func untrackedDeleteReview(uid, group, version, kind string) admissionv1.AdmissionReview {
	return admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID: types.UID(uid), Operation: admissionv1.Delete,
			Kind:      metav1.GroupVersionKind{Group: group, Version: version, Kind: kind},
			Namespace: "prod", Name: "thing",
			OldObject: raw(`{"apiVersion":"` + group + `/` + version + `","kind":"` + kind + `","metadata":{"name":"thing","namespace":"prod","uid":"uid-x","resourceVersion":"2"}}`),
		},
	}
}

// TestHandleUntrackedKind (design test 9, F4): a target whose kind the informers do not
// watch is denied krsm:untracked in BOTH modes (fail-closed, distinct from escape and
// not-ready); a syncing cache reports not-ready (the sync gate wins); a tracked kind
// addressed via a NON-preferred served version is not denied untracked (group+Kind).
func TestHandleUntrackedKind(t *testing.T) {
	for _, mode := range []scope.Mode{scope.ModeAudit, scope.ModeEnforce} {
		out := newTestServer(t, cascadeState(), mode).
			Handle(context.Background(), untrackedDeleteReview("req-untracked", "example.com", "v1", "Widget"))
		if out.Response.Allowed {
			t.Errorf("mode %s: an untracked kind must fail closed", mode)
		} else if !strings.Contains(out.Response.Result.Message, "krsm:untracked") {
			t.Errorf("mode %s: message %q must carry krsm:untracked", mode, out.Response.Result.Message)
		}
	}

	// The sync gate wins: a syncing cache reports not-ready, never untracked.
	syncing := newTestServer(t, cascadeState(), scope.ModeEnforce, func(c *Config) { c.Synced = func() bool { return false } })
	out := syncing.Handle(context.Background(), untrackedDeleteReview("req-syncing", "example.com", "v1", "Widget"))
	if !strings.Contains(out.Response.Result.Message, "krsm:not-ready") {
		t.Errorf("a syncing cache must report not-ready, got %q", out.Response.Result.Message)
	}

	// A tracked kind (Pod) addressed via a non-preferred served version is NOT untracked.
	nonPreferred := untrackedDeleteReview("req-nonpref", "", "v2", "Pod")
	out = newTestServer(t, cascadeState(), scope.ModeEnforce).Handle(context.Background(), nonPreferred)
	if out.Response.Result != nil && strings.Contains(out.Response.Result.Message, "krsm:untracked") {
		t.Errorf("a non-preferred served version of a tracked kind must not be untracked, got %q", out.Response.Result.Message)
	}
}

// TestHandleEphemeralContainersRVGuarded (design test 2): the sub-resource carries the
// pod's resourceVersion, so the staleness precheck applies — a review whose oldObject
// has no rv cannot be confirmed current and denies krsm:stale.
func TestHandleEphemeralContainersRVGuarded(t *testing.T) {
	review := ephemeralContainersReview("req-ec-rv", "web-1-a", "uid-p")
	review.Request.OldObject = raw(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"web-1-a","namespace":"prod","uid":"uid-p","labels":{"app":"web"}}}`)
	s := newTestServer(t, cascadeState(), scope.ModeAudit, func(c *Config) { c.Fresh = &fakeFresh{} })
	out := s.Handle(context.Background(), review)
	if out.Response.Allowed {
		t.Error("missing resourceVersion on a guarded sub-resource must deny")
	} else if msg := out.Response.Result.Message; !strings.Contains(msg, "krsm:stale") {
		t.Errorf("message %q must carry krsm:stale", msg)
	}
}
