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

// reRootState is the L1 fixture: Deployment web owns BOTH the ReplicaSet subtree
// (web-1 → pod web-1-a) and the Service svc, and svc selects the pod. Deleting the
// ReplicaSet closes over {rs, pod, svc}; the derived scope rooted at the ReplicaSet
// covers only {rs, pod}, so svc escapes — a verdict the request target alone cannot
// express. Re-rooting at the Deployment covers the whole closure.
func reRootState() closure.State {
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
		Owners:   []closure.OwnerRef{{Kind: "Deployment", Name: "web", UID: "uid-d"}},
		Selector: closure.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
	}
	return closure.NewScanState([]closure.Object{dep, rs, pod, svc})
}

// rsDeleteReview builds the DELETE of ReplicaSet prod/web-1 whose oldObject carries
// the given metadata annotations (nil → none).
func rsDeleteReview(uid string, annotations string) admissionv1.AdmissionReview {
	old := `{"apiVersion":"apps/v1","kind":"ReplicaSet","metadata":{"name":"web-1","namespace":"prod","uid":"uid-rs","resourceVersion":"7"` + annotations + `}}`
	return admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:       types.UID("req-" + uid),
			Operation: admissionv1.Delete,
			Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
			Namespace: "prod",
			Name:      "web-1",
			OldObject: raw(old),
		},
	}
}

// annotations renders a metadata.annotations fragment for rsDeleteReview.
func annotations(kv ...string) string {
	var b strings.Builder
	b.WriteString(`,"annotations":{`)
	for i := 0; i < len(kv); i += 2 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + kv[i] + `":"` + kv[i+1] + `"`)
	}
	b.WriteString("}")
	return b.String()
}

// TestResolveScopeNoAnnotationIsDerived (test 9): a request carrying no governing
// annotation resolves to the Level-0 derived ownership tree — provenance
// derived:ownership-tree, clauses equal to scope.Derive(target), so the Service that
// the target's own tree does not cover still escapes.
func TestResolveScopeNoAnnotationIsDerived(t *testing.T) {
	s := newTestServer(t, reRootState(), scope.ModeAudit)
	out := s.Handle(context.Background(), rsDeleteReview("l0", annotations("krsm.io/task", "t-1")))

	if out.Response == nil || !out.Response.Allowed {
		t.Fatalf("audit must allow with warnings, got %#v", out.Response)
	}
	warn := strings.Join(out.Response.Warnings, "\n")
	if !strings.Contains(warn, string(scope.ProvenanceDerivedOwner)) {
		t.Errorf("warnings %q must report the derived provenance", warn)
	}
	if !strings.Contains(warn, "svc") {
		t.Errorf("warnings %q must name the escaping Service", warn)
	}
}

// TestResolveScopeTargetAnnotationReRoots (test 10): krsm.io/target on oldObject
// re-roots the ownership tree at the named ref, so the Deployment-wide closure of a
// ReplicaSet delete is in scope — a verdict the request target alone could not
// express — and the reported provenance is `annotation`, not the derived one.
func TestResolveScopeTargetAnnotationReRoots(t *testing.T) {
	s := newTestServer(t, reRootState(), scope.ModeAudit)

	out := s.Handle(context.Background(), rsDeleteReview("l1", annotations(targetAnnotation, "Deployment/prod/web")))
	if out.Response == nil || !out.Response.Allowed {
		t.Fatalf("re-rooted delete must be allowed, got %#v", out.Response)
	}
	if len(out.Response.Warnings) != 0 {
		t.Errorf("re-rooted delete covers its closure, want no warnings, got %q", out.Response.Warnings)
	}

	// A re-root that does NOT cover the closure still reports an escape, and names
	// the annotation provenance so the operator can tell L1 from L0.
	out = s.Handle(context.Background(), rsDeleteReview("l1b", annotations(targetAnnotation, "ReplicaSet.apps/prod/web-1")))
	warn := strings.Join(out.Response.Warnings, "\n")
	if !strings.Contains(warn, string(scope.ProvenanceAnnotation)) {
		t.Errorf("warnings %q must report the annotation provenance", warn)
	}
	if strings.Contains(warn, string(scope.ProvenanceDerivedOwner)) {
		t.Errorf("warnings %q must not report the derived provenance for an L1 request", warn)
	}
}

// widgetState mirrors reRootState with the ownership root replaced by a CRD kind whose
// bare Kind ("Widget") is tracked in two groups — the ambiguity the token grammar has
// to resolve or refuse.
func widgetState() closure.State {
	wa := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Group: "a.example.com", Version: "v1", Kind: "Widget"}, Namespace: "prod", Name: "wa", UID: "uid-wa"}}
	rs := closure.Object{
		Ref:    closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "ReplicaSet"}, Namespace: "prod", Name: "web-1", UID: "uid-rs"},
		Owners: []closure.OwnerRef{{Kind: "Widget", Name: "wa", UID: "uid-wa"}},
	}
	pod := closure.Object{
		Ref:    closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Pod"}, Namespace: "prod", Name: "web-1-a", UID: "uid-p"},
		Owners: []closure.OwnerRef{{Kind: "ReplicaSet", Name: "web-1", UID: "uid-rs"}},
		Labels: map[string]string{"app": "web"},
	}
	svc := closure.Object{
		Ref:      closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Service"}, Namespace: "prod", Name: "svc", UID: "uid-s"},
		Owners:   []closure.OwnerRef{{Kind: "Widget", Name: "wa", UID: "uid-wa"}},
		Selector: closure.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
	}
	return closure.NewScanState([]closure.Object{wa, rs, pod, svc})
}

// TestResolveScopeGroupQualifiedKindReRoots (test 11): a group-qualified kind token
// "<Kind>.<group>/<ns>/<name>" resolves to that group's tracked GVK and re-roots the
// derivation there.
func TestResolveScopeGroupQualifiedKindReRoots(t *testing.T) {
	s := newTestServer(t, widgetState(), scope.ModeEnforce)
	out := s.Handle(context.Background(), rsDeleteReview("q", annotations(targetAnnotation, "Widget.a.example.com/prod/wa")))
	if out.Response == nil || !out.Response.Allowed {
		t.Fatalf("group-qualified re-root must resolve and cover the closure, got %#v", out.Response)
	}
}

// TestResolveScopeAmbiguousKindFailsClosed (test 12): an UNQUALIFIED kind token whose
// Kind is tracked in two groups is refused with scope-unresolved in BOTH modes — never
// resolved to an arbitrary group and never downgraded to L0 — while the qualified form
// of the same Kind resolves.
func TestResolveScopeAmbiguousKindFailsClosed(t *testing.T) {
	for _, mode := range []scope.Mode{scope.ModeAudit, scope.ModeEnforce} {
		out := newTestServer(t, widgetState(), mode).
			Handle(context.Background(), rsDeleteReview("amb", annotations(targetAnnotation, "Widget/prod/wa")))
		if out.Response.Allowed {
			t.Errorf("mode %s: an ambiguous kind token must fail closed, got allow", mode)
			continue
		}
		msg := out.Response.Result.Message
		if !strings.Contains(msg, string(reasonScopeUnresolved)) {
			t.Errorf("mode %s: deny %q must carry the scope-unresolved code", mode, msg)
		}
		if !strings.Contains(msg, "a.example.com") || !strings.Contains(msg, "b.example.com") {
			t.Errorf("mode %s: deny %q must name the colliding groups so the author can qualify", mode, msg)
		}
	}

	out := newTestServer(t, widgetState(), scope.ModeEnforce).
		Handle(context.Background(), rsDeleteReview("amb-ok", annotations(targetAnnotation, "Widget.a.example.com/prod/wa")))
	if !out.Response.Allowed {
		t.Errorf("the qualified form of the same Kind must resolve, got deny %q", out.Response.Result.Message)
	}
}

// TestResolveScopeUnknownKindFailsClosed (test 13): a kind token matching no tracked
// GVK — an untracked kind, or a tracked Kind named with the wrong group — fails closed
// scope-unresolved. The declared scope cannot be verified, so it is refused rather than
// quietly replaced by the derived tree.
func TestResolveScopeUnknownKindFailsClosed(t *testing.T) {
	for _, target := range []string{
		"Nonexistent/prod/wa",          // no such Kind anywhere
		"Widget.c.example.com/prod/wa", // tracked Kind, untracked group
		"ReplicaSet./prod/web-1",       // tracked Kind, explicitly core group
	} {
		out := newTestServer(t, widgetState(), scope.ModeEnforce).
			Handle(context.Background(), rsDeleteReview("unk", annotations(targetAnnotation, target)))
		if out.Response.Allowed {
			t.Errorf("%s: an unresolvable kind token must fail closed, got allow", target)
			continue
		}
		if msg := out.Response.Result.Message; !strings.Contains(msg, string(reasonScopeUnresolved)) {
			t.Errorf("%s: deny %q must carry the scope-unresolved code", target, msg)
		}
	}
}

// collidingWidgetState is the group-collision fixture: two tracked kinds sharing
// Kind + namespace + name ("Widget/prod/w") and differing ONLY in API group. Only
// wa (group a.example.com) owns the ReplicaSet subtree the action touches; wb owns
// nothing. Both live in the same group-BLIND Kind/ns/name bucket of every index, and
// wb is stored LAST — so a re-root resolved through that bucket walks wb's empty tree
// no matter which group the annotation named.
func collidingWidgetState() []closure.Object {
	wa := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Group: "a.example.com", Version: "v1", Kind: "Widget"}, Namespace: "prod", Name: "w", UID: "uid-wa"}}
	wb := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Group: "b.example.com", Version: "v1", Kind: "Widget"}, Namespace: "prod", Name: "w", UID: "uid-wb"}}
	rs := closure.Object{
		Ref:    closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "ReplicaSet"}, Namespace: "prod", Name: "web-1", UID: "uid-rs"},
		Owners: []closure.OwnerRef{{Kind: "Widget", Name: "w", UID: "uid-wa"}},
	}
	pod := closure.Object{
		Ref:    closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Pod"}, Namespace: "prod", Name: "web-1-a", UID: "uid-p"},
		Owners: []closure.OwnerRef{{Kind: "ReplicaSet", Name: "web-1", UID: "uid-rs"}},
		Labels: map[string]string{"app": "web"},
	}
	svc := closure.Object{
		Ref:      closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Service"}, Namespace: "prod", Name: "svc", UID: "uid-s"},
		Owners:   []closure.OwnerRef{{Kind: "Widget", Name: "w", UID: "uid-wa"}},
		Selector: closure.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
	}
	return []closure.Object{wa, wb, rs, pod, svc}
}

// TestResolveScopeReRootIsPinnedByUID (test 15c): before the ownership clause is built,
// the re-root Ref is stamped with the RESOLVED object's real uid, so the subtree walk
// keys on "uid:" and never consults the group-blind Kind/ns/name bucket. With two
// colliding Widgets present, "Widget.a.example.com/prod/w" must authorise wa's subtree
// — the group the annotation named — not wb's, whichever the index happened to store
// last. Resolving through the blind bucket here would be a mis-scope in the UNSAFE
// direction (it can authorise a tree the author never named).
func TestResolveScopeReRootIsPinnedByUID(t *testing.T) {
	objs := collidingWidgetState()
	s := newTestServer(t, closure.NewScanState(objs), scope.ModeEnforce, withObjects(objs))

	out := s.Handle(context.Background(), rsDeleteReview("uid-a", annotations(targetAnnotation, "Widget.a.example.com/prod/w")))
	if !out.Response.Allowed {
		t.Errorf("re-root at the group that OWNS the subtree must cover the closure, got deny %q", out.Response.Result.Message)
	}

	// The mirror: the same Kind/ns/name in the other group owns nothing, so the very
	// same closure escapes. Both re-roots resolving alike would prove the group is
	// being ignored.
	out = s.Handle(context.Background(), rsDeleteReview("uid-b", annotations(targetAnnotation, "Widget.b.example.com/prod/w")))
	if out.Response.Allowed {
		t.Error("re-root at the group that owns NOTHING must not cover the closure, got allow")
	}
}

// TestResolveScopeUnresolvableRootAuthorizesOnlyItself (test 15d): a krsm.io/target
// naming a WELL-FORMED, tracked kind whose object is absent leaves the re-root Ref
// uid-less — a uid is never invented for a root that was not found. The subtree walk
// then starts from a nonexistent root and authorises only that root, so every closure
// member escapes and the verdict is a fail-closed Block. The failure direction matters:
// an unresolvable root must never widen the scope (falling back to the derived tree
// would silently authorise part of the closure the annotation never named).
func TestResolveScopeUnresolvableRootAuthorizesOnlyItself(t *testing.T) {
	objs := collidingWidgetState()
	s := newTestServer(t, closure.NewScanState(objs), scope.ModeEnforce, withObjects(objs))

	out := s.Handle(context.Background(), rsDeleteReview("ghost", annotations(targetAnnotation, "Widget.a.example.com/prod/ghost")))
	if out.Response.Allowed {
		t.Fatal("a re-root at an absent object must fail closed, got allow")
	}
	msg := out.Response.Result.Message
	if strings.Contains(msg, string(reasonScopeUnresolved)) {
		t.Errorf("an absent object of a TRACKED kind is a computed empty subtree, not an unresolved reference; got %q", msg)
	}
	// Every closure member escapes: the authorised set is the root alone, never a
	// wider tree borrowed from some other object.
	for _, want := range []string{"ReplicaSet/prod/web-1", "Pod/prod/web-1-a", "Service/prod/svc"} {
		if !strings.Contains(msg, want) {
			t.Errorf("deny %q must report %s as escaping — an unresolved root authorises only itself", msg, want)
		}
	}
}

// TestResolveScopeMalformedTargetFailsClosedInvalid (test 14): a krsm.io/target whose
// SHAPE is wrong — not exactly three segments, an empty or whitespace-bearing segment,
// a missing namespace for a namespaced kind or a namespace on a cluster-scoped one —
// is refused with `invalid` (malformed syntax), the code distinct from the
// scope-unresolved one a well-formed-but-unresolvable reference gets.
func TestResolveScopeMalformedTargetFailsClosedInvalid(t *testing.T) {
	for name, value := range map[string]string{
		"too few segments":       "Deployment/prod",
		"too many segments":      "Deployment/prod/web/extra",
		"empty value":            "",
		"empty kind":             "/prod/web",
		"empty name":             "Deployment/prod/",
		"whitespace in kind":     "Deploy ment/prod/web",
		"whitespace in ns":       "Deployment/ prod/web",
		"whitespace in name":     "Deployment/prod/we b",
		"namespaced without ns":  "Deployment//web",
		"cluster-scoped with ns": "PersistentVolume/prod/pv-1",
	} {
		out := newTestServer(t, reRootState(), scope.ModeEnforce).
			Handle(context.Background(), rsDeleteReview("bad", annotations(targetAnnotation, value)))
		if out.Response.Allowed {
			t.Errorf("%s (%q): a malformed target must fail closed, got allow", name, value)
			continue
		}
		msg := out.Response.Result.Message
		if !strings.Contains(msg, string(reasonInvalid)) {
			t.Errorf("%s (%q): deny %q must carry the invalid code", name, value, msg)
		}
	}

	// The mirror image: a cluster-scoped kind with the empty namespace segment is
	// well-formed and resolves (it just does not cover this closure).
	out := newTestServer(t, reRootState(), scope.ModeEnforce).
		Handle(context.Background(), rsDeleteReview("pv", annotations(targetAnnotation, "PersistentVolume//pv-1")))
	if msg := out.Response.Result.Message; strings.Contains(msg, string(reasonInvalid)) {
		t.Errorf("a cluster-scoped ref with an empty namespace is well-formed, got %q", msg)
	}
}

// TestResolveScopeTargetAnnotationIsNotSelfAuthorizing (test 15): an UPDATE that ADDS
// krsm.io/target in the incoming object does not authorize itself — governing
// annotations are read from oldObject only, so the request falls to L0 and the escape
// is still reported. The pre-existing object carrying the same annotation DOES govern.
func TestResolveScopeTargetAnnotationIsNotSelfAuthorizing(t *testing.T) {
	// Relabelling the Pod moves it out of the Service's selector, so the Service is in
	// the closure while the derived (pod-rooted) scope covers only the Pod itself.
	podRelabel := func(uid, oldAnn, newAnn string) admissionv1.AdmissionReview {
		body := func(ann, labels string) string {
			return `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"web-1-a","namespace":"prod","uid":"uid-p","resourceVersion":"7","labels":{"app":"` + labels + `"}` + ann + `}}`
		}
		return admissionv1.AdmissionReview{
			Request: &admissionv1.AdmissionRequest{
				UID:       types.UID("req-" + uid),
				Operation: admissionv1.Update,
				Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "Pod"},
				Namespace: "prod",
				Name:      "web-1-a",
				OldObject: raw(body(oldAnn, "web")),
				Object:    raw(body(newAnn, "parked")),
			},
		}
	}
	reRoot := annotations(targetAnnotation, "Deployment/prod/web")

	s := newTestServer(t, reRootState(), scope.ModeAudit)
	out := s.Handle(context.Background(), podRelabel("self", "", reRoot))
	warn := strings.Join(out.Response.Warnings, "\n")
	if !strings.Contains(warn, string(scope.ProvenanceDerivedOwner)) {
		t.Errorf("an annotation present only in the incoming object must not govern; warnings = %q", warn)
	}

	// Control: the same annotation already on the stored object does govern.
	out = s.Handle(context.Background(), podRelabel("pre", reRoot, reRoot))
	if !out.Response.Allowed || len(out.Response.Warnings) != 0 {
		t.Errorf("a pre-existing annotation must re-root the derivation and cover the Service; got allowed=%v warnings=%q",
			out.Response.Allowed, out.Response.Warnings)
	}
}

// TestHandleReportsScopeProvenanceInAuditAnnotations (test 28, L0/L1 half): every
// DECIDED response carries the resolved provenance as a structured audit annotation, so
// the API server's audit log records how the scope arose without parsing the human
// message. Responses that never reached scope resolution carry none — an audit record
// must not claim a provenance that was never computed.
func TestHandleReportsScopeProvenanceInAuditAnnotations(t *testing.T) {
	s := newTestServer(t, reRootState(), scope.ModeAudit)

	for name, tc := range map[string]struct {
		review admissionv1.AdmissionReview
		want   scope.Provenance
	}{
		"derived warn": {rsDeleteReview("prov-l0", ""), scope.ProvenanceDerivedOwner},
		"annotation allow": {rsDeleteReview("prov-l1", annotations(targetAnnotation, "Deployment/prod/web")),
			scope.ProvenanceAnnotation},
	} {
		out := s.Handle(context.Background(), tc.review)
		if got := out.Response.AuditAnnotations[provenanceAuditKey]; got != string(tc.want) {
			t.Errorf("%s: AuditAnnotations[%s] = %q, want %q", name, provenanceAuditKey, got, tc.want)
		}
	}

	// A deny raised before the scope was resolved reports no provenance.
	out := s.Handle(context.Background(), admissionv1.AdmissionReview{Request: &admissionv1.AdmissionRequest{}})
	if _, ok := out.Response.AuditAnnotations[provenanceAuditKey]; ok {
		t.Errorf("a pre-resolution deny must not claim a provenance, got %v", out.Response.AuditAnnotations)
	}
}

// --- Step 3: the cross-boundary allowlist (tests 16–21) ---

// secretConsumer builds an object that cross-references Secret prod/tls from OUTSIDE
// that Secret's own namespace (or from cluster scope) — the Gateway API's
// cross-namespace certificateRef and cert-manager's ClusterIssuer secretRef are the
// canonical shapes. Deleting the Secret therefore pulls a resource across a boundary
// the derived, target-rooted scope can never cover.
func secretConsumer(gvk closure.GVK, namespace, name, uid string) closure.Object {
	return closure.Object{
		Ref: closure.Ref{GVK: gvk, Namespace: namespace, Name: name, UID: uid},
		CrossRefs: []closure.CrossRef{{
			Kind: closure.RefVolume,
			Ref:  closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Secret"}, Namespace: "prod", Name: "tls"},
		}},
	}
}

// secretState is Secret prod/tls plus the given cross-boundary consumers. Deleting the
// Secret closes over every consumer (a delete of a mountable kind is a MutateConfig
// effect), while the derived scope rooted at the Secret covers the Secret alone — so
// each consumer is an INDIRECT escape, which is exactly what the allowlist may exempt.
func secretState(consumers ...closure.Object) []closure.Object {
	secret := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Secret"}, Namespace: "prod", Name: "tls", UID: "uid-tls"}}
	return append([]closure.Object{secret}, consumers...)
}

// secretDeleteReview builds the DELETE of Secret prod/tls whose oldObject carries the
// given metadata annotations fragment (empty → none).
func secretDeleteReview(uid, annotations string) admissionv1.AdmissionReview {
	old := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"tls","namespace":"prod","uid":"uid-tls","resourceVersion":"7"` + annotations + `}}`
	return admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:       types.UID("req-" + uid),
			Operation: admissionv1.Delete,
			Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "Secret"},
			Namespace: "prod",
			Name:      "tls",
			OldObject: raw(old),
		},
	}
}

var gatewayGVK = closure.GVK{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"}

// TestAllowlistExemptsIndirectNamespaceEscape (test 16): an escape reached INDIRECTLY
// into an operator-allowlisted namespace is dropped from the escape set before the
// verdict is reported — audit emits no warning and enforce admits — while the same
// action without the allowlist still reports it. The allowlist is a post-Safe filter,
// not a scope clause: it removes named collateral, it does not widen scope(T).
func TestAllowlistExemptsIndirectNamespaceEscape(t *testing.T) {
	objs := secretState(secretConsumer(gatewayGVK, "infra", "gw", "uid-gw"))
	st := closure.NewScanState(objs)

	audit := newTestServer(t, st, scope.ModeAudit, withObjects(objs))
	warn := strings.Join(audit.Handle(context.Background(), secretDeleteReview("nl-0", "")).Response.Warnings, "\n")
	if !strings.Contains(warn, "Gateway/infra/gw") {
		t.Fatalf("without an allowlist the cross-namespace escape must be reported, got %q", warn)
	}

	allow := Allowlist{Namespaces: map[string]bool{"infra": true}}
	out := newTestServer(t, st, scope.ModeAudit, withObjects(objs), withAllowlist(allow)).
		Handle(context.Background(), secretDeleteReview("nl-1", ""))
	if !out.Response.Allowed || len(out.Response.Warnings) != 0 {
		t.Errorf("audit: an exempted indirect escape must warn about nothing, got allowed=%v warnings=%q", out.Response.Allowed, out.Response.Warnings)
	}

	out = newTestServer(t, st, scope.ModeEnforce, withObjects(objs), withAllowlist(allow)).
		Handle(context.Background(), secretDeleteReview("nl-2", ""))
	if !out.Response.Allowed {
		t.Errorf("enforce: an exempted indirect escape must be admitted, got deny %q", out.Response.Result.Message)
	}
}

var clusterIssuerGVK = closure.GVK{Group: "cert-manager.io", Version: "v1", Kind: "ClusterIssuer"}

// TestAllowlistExemptsClusterScopedGroupKind (test 17): a cluster-scoped indirect
// escape is exempted by its {Group, Kind} — GroupKind, never Kind alone, because a
// bare Kind collides across CRD groups and would exempt an unrelated group's resource
// (the S2 class). A namespaced ref is NOT reachable through this list at all: the
// namespace list governs it.
func TestAllowlistExemptsClusterScopedGroupKind(t *testing.T) {
	objs := secretState(secretConsumer(clusterIssuerGVK, "", "ca", "uid-ci"))
	st := closure.NewScanState(objs)

	// The allowlist key is Group+Kind with the version zeroed: an exemption must not
	// have to be restated for every served version of a CRD.
	matching := Allowlist{ClusterScopedGroupKinds: map[closure.GVK]bool{
		{Group: "cert-manager.io", Kind: "ClusterIssuer"}: true,
	}}
	out := newTestServer(t, st, scope.ModeEnforce, withObjects(objs), withAllowlist(matching)).
		Handle(context.Background(), secretDeleteReview("cs-1", ""))
	if !out.Response.Allowed {
		t.Errorf("an allowlisted cluster-scoped GroupKind must be exempted, got deny %q", out.Response.Result.Message)
	}

	// Same Kind, different group — not the resource the operator allowlisted.
	otherGroup := Allowlist{ClusterScopedGroupKinds: map[closure.GVK]bool{
		{Group: "other.example.com", Kind: "ClusterIssuer"}: true,
	}}
	out = newTestServer(t, st, scope.ModeEnforce, withObjects(objs), withAllowlist(otherGroup)).
		Handle(context.Background(), secretDeleteReview("cs-2", ""))
	if out.Response.Allowed {
		t.Error("the same Kind in a DIFFERENT group must not be exempted, got allow")
	}

	// A NAMESPACED escape is governed by the namespace list only: listing its
	// GroupKind here must not exempt it.
	nsObjs := secretState(secretConsumer(gatewayGVK, "infra", "gw", "uid-gw"))
	byGroupKind := Allowlist{ClusterScopedGroupKinds: map[closure.GVK]bool{
		{Group: gatewayGVK.Group, Kind: gatewayGVK.Kind}: true,
	}}
	out = newTestServer(t, closure.NewScanState(nsObjs), scope.ModeEnforce, withObjects(nsObjs), withAllowlist(byGroupKind)).
		Handle(context.Background(), secretDeleteReview("cs-3", ""))
	if out.Response.Allowed {
		t.Error("a namespaced escape must not be exempted by the cluster-scoped GroupKind list, got allow")
	}
}

// pvDeleteReview builds the DELETE of the cluster-scoped PersistentVolume pv-1 whose
// oldObject carries the given metadata annotations fragment.
func pvDeleteReview(uid, annotations string) admissionv1.AdmissionReview {
	old := `{"apiVersion":"v1","kind":"PersistentVolume","metadata":{"name":"pv-1","uid":"uid-pv","resourceVersion":"7"` + annotations + `}}`
	return admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:       types.UID("req-" + uid),
			Operation: admissionv1.Delete,
			Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "PersistentVolume"},
			Name:      "pv-1",
			OldObject: raw(old),
		},
	}
}

// TestAllowlistNeverExemptsTheActionTarget (test 18): the allowlist exempts INDIRECT
// collateral only. A direct DELETE whose own target sits in an allowlisted namespace —
// or is an allowlisted cluster-scoped GroupKind — still Warns/Blocks. A target-blind
// filter would be a direct authorization bypass: "allowlist the shared namespace" would
// silently become "anyone may delete anything in it".
//
// The target only reaches the escape set at all when the scope is re-rooted away from
// it (L1), which is exactly the configuration an attacker would choose.
func TestAllowlistNeverExemptsTheActionTarget(t *testing.T) {
	dep := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}, Namespace: "prod", Name: "web", UID: "uid-d"}}
	reRoot := annotations(targetAnnotation, "Deployment.apps/prod/web")

	objs := append(secretState(), dep)
	st := closure.NewScanState(objs)
	byNamespace := Allowlist{Namespaces: map[string]bool{"prod": true}}

	out := newTestServer(t, st, scope.ModeEnforce, withObjects(objs), withAllowlist(byNamespace)).
		Handle(context.Background(), secretDeleteReview("tgt-1", reRoot))
	if out.Response.Allowed {
		t.Fatal("enforce: deleting the target itself must not be exempted by its namespace, got allow")
	}
	if msg := out.Response.Result.Message; !strings.Contains(msg, "Secret/prod/tls") {
		t.Errorf("deny %q must still name the target as escaping", msg)
	}

	out = newTestServer(t, st, scope.ModeAudit, withObjects(objs), withAllowlist(byNamespace)).
		Handle(context.Background(), secretDeleteReview("tgt-2", reRoot))
	if len(out.Response.Warnings) == 0 {
		t.Error("audit: deleting the target itself must still warn, got no warnings")
	}

	// The cluster-scoped mirror: an allowlisted GroupKind does not license deleting an
	// instance of it directly.
	pv := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "PersistentVolume"}, Name: "pv-1", UID: "uid-pv"}}
	pvObjs := []closure.Object{pv, dep}
	byGroupKind := Allowlist{ClusterScopedGroupKinds: map[closure.GVK]bool{{Kind: "PersistentVolume"}: true}}
	out = newTestServer(t, closure.NewScanState(pvObjs), scope.ModeEnforce, withObjects(pvObjs), withAllowlist(byGroupKind)).
		Handle(context.Background(), pvDeleteReview("tgt-3", reRoot))
	if out.Response.Allowed {
		t.Error("enforce: deleting an allowlisted cluster-scoped target must not be exempted, got allow")
	}
}

// TestAllowlistPartialAndFullExemption (test 19): with a mixed escape set the verdict
// stands on what is LEFT — one non-allowlisted escape still Warns/Blocks, and the
// residual report names only the escapes an operator still has to act on. Once every
// indirect escape is exempted the Block flips to Allow.
func TestAllowlistPartialAndFullExemption(t *testing.T) {
	objs := secretState(
		secretConsumer(gatewayGVK, "infra", "gw", "uid-gw"),
		secretConsumer(gatewayGVK, "staging", "gw2", "uid-gw2"),
	)
	st := closure.NewScanState(objs)

	partial := Allowlist{Namespaces: map[string]bool{"infra": true}}
	out := newTestServer(t, st, scope.ModeEnforce, withObjects(objs), withAllowlist(partial)).
		Handle(context.Background(), secretDeleteReview("mix-1", ""))
	if out.Response.Allowed {
		t.Fatal("enforce: a non-allowlisted escape must still block, got allow")
	}
	msg := out.Response.Result.Message
	if !strings.Contains(msg, "Gateway/staging/gw2") {
		t.Errorf("deny %q must name the escape that is NOT exempted", msg)
	}
	if strings.Contains(msg, "Gateway/infra/gw") {
		t.Errorf("deny %q must report the RESIDUAL escape set only — the exempted ref is not an operator's problem", msg)
	}

	full := Allowlist{Namespaces: map[string]bool{"infra": true, "staging": true}}
	out = newTestServer(t, st, scope.ModeEnforce, withObjects(objs), withAllowlist(full)).
		Handle(context.Background(), secretDeleteReview("mix-2", ""))
	if !out.Response.Allowed {
		t.Errorf("enforce: exempting every indirect escape must flip Block to Allow, got deny %q", out.Response.Result.Message)
	}
}

// TestAllowlistNeverSoftensFailClosedBlock (test 20): a Block carrying an EMPTY
// Escaping set is closure.Safe's fail-closed deny — the target could not be resolved,
// so the blast radius is unknown rather than computed. The allowlist must leave it
// alone in both modes: "nothing escaped" and "we could not tell what escapes" look
// alike in the escape set but mean opposite things, and only the first may be admitted.
func TestAllowlistNeverSoftensFailClosedBlock(t *testing.T) {
	// The Secret the request targets is absent from the tracked state, so its closure
	// cannot be computed. Everything else is allowlisted as widely as possible.
	objs := []closure.Object{secretConsumer(gatewayGVK, "infra", "gw", "uid-gw")}
	st := closure.NewScanState(objs)
	wide := Allowlist{
		Namespaces:              map[string]bool{"prod": true, "infra": true},
		ClusterScopedGroupKinds: map[closure.GVK]bool{{Kind: "PersistentVolume"}: true},
	}

	for _, mode := range []scope.Mode{scope.ModeAudit, scope.ModeEnforce} {
		out := newTestServer(t, st, mode, withObjects(objs), withAllowlist(wide)).
			Handle(context.Background(), secretDeleteReview("fc-"+string(mode), ""))
		if out.Response.Allowed {
			t.Errorf("mode %s: a fail-closed Block must never be softened by the allowlist, got allow", mode)
			continue
		}
		if msg := out.Response.Result.Message; !strings.Contains(msg, "fail-closed") {
			t.Errorf("mode %s: deny %q must still report the fail-closed reason", mode, msg)
		}
	}
}

// TestAllowlistAppliesToDerivedProvenanceOnly (test 21): the allowlist is an
// approximation the operator supplies BECAUSE a derived scope cannot know which
// boundaries are legitimate. A contract-provenance decision has no such gap — the
// author already declared the scope — so a declared scope is never widened behind the
// author's back by a server flag.
//
// The gate is asserted directly on the provenance because L3 cannot yet be driven
// through Handle (the contractGetter seam is step 4); the end-to-end half below pins
// that the two DERIVED provenances really are filtered, so the gate is not vacuous.
func TestAllowlistAppliesToDerivedProvenanceOnly(t *testing.T) {
	for prov, want := range map[scope.Provenance]bool{
		scope.ProvenanceDerivedOwner: true,
		scope.ProvenanceAnnotation:   true,
		scope.ProvenanceContract:     false,
	} {
		if got := allowlistApplies(prov); got != want {
			t.Errorf("allowlistApplies(%q) = %v, want %v", prov, got, want)
		}
	}

	// L1 (annotation provenance) is filtered: a re-root that covers the target still
	// lets the allowlist drop the indirect cross-namespace escape.
	dep := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}, Namespace: "prod", Name: "web", UID: "uid-d"}}
	secret := closure.Object{
		Ref:    closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Secret"}, Namespace: "prod", Name: "tls", UID: "uid-tls"},
		Owners: []closure.OwnerRef{{Kind: "Deployment", Name: "web", UID: "uid-d"}},
	}
	objs := []closure.Object{dep, secret, secretConsumer(gatewayGVK, "infra", "gw", "uid-gw")}
	out := newTestServer(t, closure.NewScanState(objs), scope.ModeEnforce, withObjects(objs),
		withAllowlist(Allowlist{Namespaces: map[string]bool{"infra": true}})).
		Handle(context.Background(), secretDeleteReview("l1-allow", annotations(targetAnnotation, "Deployment.apps/prod/web")))
	if !out.Response.Allowed {
		t.Errorf("an L1 (annotation) decision must still be filtered by the allowlist, got deny %q", out.Response.Result.Message)
	}
}
