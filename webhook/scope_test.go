package webhook

import (
	"context"
	"errors"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/scope"
)

// reRootState is the L1 fixture as a State; reRootObjects is the same set as objects,
// which the clusterInfo fake needs so an annotation-named root can be resolved
// group-aware (an unresolvable root now fails closed, so a server under an L1 test must
// know the objects its State knows).
func reRootState() closure.State { return closure.NewScanState(reRootObjects()) }

// reRootObjects: Deployment web owns BOTH the ReplicaSet subtree (web-1 → pod web-1-a)
// and the Service svc, and svc selects the pod. Deleting the ReplicaSet closes over
// {rs, pod, svc}; the derived scope rooted at the ReplicaSet covers only {rs, pod}, so
// svc escapes — a verdict the request target alone cannot express. Re-rooting at the
// Deployment covers the whole closure.
func reRootObjects() []closure.Object {
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
	return []closure.Object{dep, rs, pod, svc}
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
	s := newTestServer(t, reRootState(), scope.ModeAudit, withObjects(reRootObjects()))

	out := s.Handle(context.Background(), rsDeleteReview("l1", annotations(DefaultTargetAnnotation, "Deployment/prod/web")))
	if out.Response == nil || !out.Response.Allowed {
		t.Fatalf("re-rooted delete must be allowed, got %#v", out.Response)
	}
	if len(out.Response.Warnings) != 0 {
		t.Errorf("re-rooted delete covers its closure, want no warnings, got %q", out.Response.Warnings)
	}

	// A re-root that does NOT cover the closure still reports an escape, and names
	// the annotation provenance so the operator can tell L1 from L0.
	out = s.Handle(context.Background(), rsDeleteReview("l1b", annotations(DefaultTargetAnnotation, "ReplicaSet.apps/prod/web-1")))
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
func widgetState() closure.State { return closure.NewScanState(widgetObjects()) }

func widgetObjects() []closure.Object {
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
	return []closure.Object{wa, rs, pod, svc}
}

// TestResolveScopeGroupQualifiedKindReRoots (test 11): a group-qualified kind token
// "<Kind>.<group>/<ns>/<name>" resolves to that group's tracked GVK and re-roots the
// derivation there.
func TestResolveScopeGroupQualifiedKindReRoots(t *testing.T) {
	s := newTestServer(t, widgetState(), scope.ModeEnforce, withObjects(widgetObjects()))
	out := s.Handle(context.Background(), rsDeleteReview("q", annotations(DefaultTargetAnnotation, "Widget.a.example.com/prod/wa")))
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
			Handle(context.Background(), rsDeleteReview("amb", annotations(DefaultTargetAnnotation, "Widget/prod/wa")))
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

	out := newTestServer(t, widgetState(), scope.ModeEnforce, withObjects(widgetObjects())).
		Handle(context.Background(), rsDeleteReview("amb-ok", annotations(DefaultTargetAnnotation, "Widget.a.example.com/prod/wa")))
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
			Handle(context.Background(), rsDeleteReview("unk", annotations(DefaultTargetAnnotation, target)))
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

	out := s.Handle(context.Background(), rsDeleteReview("uid-a", annotations(DefaultTargetAnnotation, "Widget.a.example.com/prod/w")))
	if !out.Response.Allowed {
		t.Errorf("re-root at the group that OWNS the subtree must cover the closure, got deny %q", out.Response.Result.Message)
	}

	// The mirror: the same Kind/ns/name in the other group owns nothing, so the very
	// same closure escapes. Both re-roots resolving alike would prove the group is
	// being ignored.
	out = s.Handle(context.Background(), rsDeleteReview("uid-b", annotations(DefaultTargetAnnotation, "Widget.b.example.com/prod/w")))
	if out.Response.Allowed {
		t.Error("re-root at the group that owns NOTHING must not cover the closure, got allow")
	}
}

// absentRootCollisionState is the fixture that DISPROVES the "an absent root is
// harmless because it authorises only itself" claim: the a-group Widget the annotation
// names does NOT exist, while a b-group Widget with the SAME Kind/namespace/name does —
// and it owns the whole subtree the action touches. Both share one group-blind
// byHuman["Widget/prod/w"] bucket, so a uid-less re-root Ref resolves through it and
// closure's ownedSubtree normalises the walk's start to the b-group Widget: the ABSENT
// root would authorise another group's entire subtree. An empty index cannot show this
// — there the absent root really does authorise only itself — which is why the fixture
// has to carry the collision.
func absentRootCollisionState() []closure.Object {
	wb := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Group: "b.example.com", Version: "v1", Kind: "Widget"}, Namespace: "prod", Name: "w", UID: "uid-wb"}}
	rs := closure.Object{
		Ref:    closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "ReplicaSet"}, Namespace: "prod", Name: "web-1", UID: "uid-rs"},
		Owners: []closure.OwnerRef{{Kind: "Widget", Name: "w", UID: "uid-wb"}},
	}
	pod := closure.Object{
		Ref:    closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Pod"}, Namespace: "prod", Name: "web-1-a", UID: "uid-p"},
		Owners: []closure.OwnerRef{{Kind: "ReplicaSet", Name: "web-1", UID: "uid-rs"}},
		Labels: map[string]string{"app": "web"},
	}
	svc := closure.Object{
		Ref:      closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Service"}, Namespace: "prod", Name: "svc", UID: "uid-s"},
		Owners:   []closure.OwnerRef{{Kind: "Widget", Name: "w", UID: "uid-wb"}},
		Selector: closure.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
	}
	return []closure.Object{wb, rs, pod, svc}
}

// TestResolveScopeUnresolvableRootFailsClosed (test 15d, corrected): a krsm.io/target
// naming a well-formed, TRACKED kind whose object cannot be resolved group-aware is
// refused with scope-unresolved in BOTH modes — the annotation names a root KRSM cannot
// resolve, which is exactly what that taxonomy code means.
//
// The Ref must NOT be left uid-less and must NOT carry a synthesized uid: either way
// the engine's lookup falls through to the group-blind Kind/ns/name bucket and the walk
// starts from whatever object that bucket holds. With this fixture that is the b-group
// Widget, whose subtree covers the entire closure — so the ABSENT a-group root would
// return allowed=true, a mis-scope in the UNSAFE direction. Only refusing to resolve
// delivers the intended fail-closed deny.
func TestResolveScopeUnresolvableRootFailsClosed(t *testing.T) {
	objs := absentRootCollisionState()
	for _, mode := range []scope.Mode{scope.ModeAudit, scope.ModeEnforce} {
		s := newTestServer(t, closure.NewScanState(objs), mode, withObjects(objs))
		out := s.Handle(context.Background(), rsDeleteReview("ghost-"+string(mode), annotations(DefaultTargetAnnotation, "Widget.a.example.com/prod/w")))
		if out.Response.Allowed {
			t.Errorf("mode %s: a re-root at an object absent from its OWN group must fail closed, got allow (the other group's subtree authorised it)", mode)
			continue
		}
		if msg := out.Response.Result.Message; !strings.Contains(msg, string(reasonScopeUnresolved)) {
			t.Errorf("mode %s: deny %q must carry the scope-unresolved code", mode, msg)
		}
	}

	// The control: the same name in the group that DOES exist still resolves and
	// covers the closure, so the deny above is about resolution, not about the fixture
	// being unauthorisable.
	s := newTestServer(t, closure.NewScanState(objs), scope.ModeEnforce, withObjects(objs))
	out := s.Handle(context.Background(), rsDeleteReview("present", annotations(DefaultTargetAnnotation, "Widget.b.example.com/prod/w")))
	if !out.Response.Allowed {
		t.Errorf("the root that DOES exist must resolve and cover its subtree, got deny %q", out.Response.Result.Message)
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
			Handle(context.Background(), rsDeleteReview("bad", annotations(DefaultTargetAnnotation, value)))
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
	// well-formed and RESOLVES (it just does not cover this closure). The PV is added
	// to the tracked set so this really exercises the well-formed path — without it the
	// ref would fail closed as unresolved and the assertion would pass vacuously.
	pv := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "PersistentVolume"}, Name: "pv-1", UID: "uid-pv"}}
	objs := append(reRootObjects(), pv)
	out := newTestServer(t, closure.NewScanState(objs), scope.ModeEnforce, withObjects(objs)).
		Handle(context.Background(), rsDeleteReview("pv", annotations(DefaultTargetAnnotation, "PersistentVolume//pv-1")))
	msg := out.Response.Result.Message
	if strings.Contains(msg, string(reasonInvalid)) || strings.Contains(msg, string(reasonScopeUnresolved)) {
		t.Errorf("a cluster-scoped ref with an empty namespace is well-formed and resolvable, got %q", msg)
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
	reRoot := annotations(DefaultTargetAnnotation, "Deployment/prod/web")

	s := newTestServer(t, reRootState(), scope.ModeAudit, withObjects(reRootObjects()))
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
	s := newTestServer(t, reRootState(), scope.ModeAudit, withObjects(reRootObjects()))

	for name, tc := range map[string]struct {
		review admissionv1.AdmissionReview
		want   scope.Provenance
	}{
		"derived warn": {rsDeleteReview("prov-l0", ""), scope.ProvenanceDerivedOwner},
		"annotation allow": {rsDeleteReview("prov-l1", annotations(DefaultTargetAnnotation, "Deployment/prod/web")),
			scope.ProvenanceAnnotation},
	} {
		out := s.Handle(context.Background(), tc.review)
		if got := out.Response.AuditAnnotations[provenanceAuditKey]; got != string(tc.want) {
			t.Errorf("%s: AuditAnnotations[%s] = %q, want %q", name, provenanceAuditKey, got, tc.want)
		}
	}

	// The L3 half (step 4): a contract-resolved decision reports provenance `contract`,
	// so all three levels are distinguishable in an audit query.
	objs := reRootObjects()
	cr := taskContractCR(t, "prod", "task-1", `{"dim":"namespace","namespace":"prod"}`)
	l3 := newTestServer(t, closure.NewScanState(objs), scope.ModeAudit, withObjects(objs), withContracts(contractsWith(cr)))
	out := l3.Handle(context.Background(), rsDeleteReview("prov-l3", annotations(DefaultScopeAnnotation, "prod/task-1")))
	if got := out.Response.AuditAnnotations[provenanceAuditKey]; got != string(scope.ProvenanceContract) {
		t.Errorf("contract allow: AuditAnnotations[%s] = %q, want %q", provenanceAuditKey, got, scope.ProvenanceContract)
	}

	// A deny raised before the scope was resolved reports no provenance.
	out = s.Handle(context.Background(), admissionv1.AdmissionReview{Request: &admissionv1.AdmissionRequest{}})
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
	reRoot := annotations(DefaultTargetAnnotation, "Deployment.apps/prod/web")

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
		Handle(context.Background(), secretDeleteReview("l1-allow", annotations(DefaultTargetAnnotation, "Deployment.apps/prod/web")))
	if !out.Response.Allowed {
		t.Errorf("an L1 (annotation) decision must still be filtered by the allowlist, got deny %q", out.Response.Result.Message)
	}
}

// --- Step 4: L3 contract resolution (tests 21b, 22–28) ---

// fakeContracts is the contractGetter seam: a hermetic stand-in for the live GET against
// a TaskContract CR. It honours ctx first, exactly as a real client does, so an expired
// admission deadline is observable without a cluster; err (when set) stands for the whole
// class of GET failures — RBAC forbidden, API-server unreachable, an absent CRD.
type fakeContracts struct {
	objs map[string]*unstructured.Unstructured
	err  error
}

func (f fakeContracts) Get(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	u, ok := f.objs[namespace+"/"+name]
	if !ok {
		return nil, ErrContractNotFound
	}
	return u, nil
}

// withContracts installs the L3 contract seam.
func withContracts(c contractGetter) func(*Config) {
	return func(cfg *Config) { cfg.Contracts = c }
}

// taskContractCR builds a TaskContract CR as the API server really hands it back:
// unstructured, so the test exercises the same unstructured→JSON→parse path production
// uses rather than a Go struct short-cut, AND carrying the fields the SERVER owns —
// creationTimestamp, uid, resourceVersion, generation, managedFields, plus the status a
// subresource returns.
//
// Those server-populated fields are not decoration. An earlier version of this helper
// wrote metadata with only name and namespace while claiming to be "as the API server
// would hand it back"; it was not, and the whole L3 family passed against an object no
// cluster would ever produce, while a strict whole-document decode rejected every real
// one. A fixture that is cleaner than production is a blind spot, not a simplification.
func taskContractCR(t *testing.T, namespace, name string, allow ...string) *unstructured.Unstructured {
	t.Helper()
	body := `{"apiVersion":"krsm.io/v1alpha1","kind":"TaskContract","metadata":{"name":"` + name +
		`","namespace":"` + namespace + `","uid":"9f1c1b8e-0f1a-4a3a-9c2b-0b6a1f2d3e4f",` +
		`"resourceVersion":"4711","generation":1,"creationTimestamp":"2026-08-01T10:00:00Z",` +
		`"managedFields":[{"manager":"kubectl-client-side-apply","operation":"Update",` +
		`"apiVersion":"krsm.io/v1alpha1","time":"2026-08-01T10:00:00Z","fieldsType":"FieldsV1",` +
		`"fieldsV1":{"f:spec":{"f:allow":{}}}}]},"status":{},` +
		`"spec":{"allow":[` + strings.Join(allow, ",") + `]}}`
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON([]byte(body)); err != nil {
		t.Fatalf("taskContractCR: %v", err)
	}
	return u
}

// contractsWith is the common one-contract getter.
func contractsWith(u *unstructured.Unstructured) fakeContracts {
	return fakeContracts{objs: map[string]*unstructured.Unstructured{
		u.GetNamespace() + "/" + u.GetName(): u,
	}}
}

// TestResolveScopeContractAllowsCollateral (test 22): krsm.io/scope on oldObject names a
// TaskContract that is resolved by live GET, parsed and compiled; its clauses ARE the
// request's scope. A contract authorising the collateral therefore ALLOWS an action the
// derived scope blocks in enforce, and the response reports provenance `contract` — so an
// operator can tell a declared authorisation from a synthesized one.
func TestResolveScopeContractAllowsCollateral(t *testing.T) {
	objs := reRootObjects()
	st := closure.NewScanState(objs)

	// Control: with no contract the same delete is a hard Block — the Service escapes
	// the ReplicaSet-rooted derived scope.
	out := newTestServer(t, st, scope.ModeEnforce, withObjects(objs)).
		Handle(context.Background(), rsDeleteReview("l3-ctl", ""))
	if out.Response.Allowed {
		t.Fatal("control: derived enforce must block this delete, else test 22 proves nothing")
	}

	cr := taskContractCR(t, "prod", "task-1", `{"dim":"namespace","namespace":"prod"}`)
	s := newTestServer(t, st, scope.ModeEnforce, withObjects(objs), withContracts(contractsWith(cr)))
	out = s.Handle(context.Background(), rsDeleteReview("l3", annotations(DefaultScopeAnnotation, "prod/task-1")))
	if !out.Response.Allowed {
		t.Fatalf("a contract authorising the collateral must ALLOW, got deny %q", out.Response.Result.Message)
	}
	if got := out.Response.AuditAnnotations[provenanceAuditKey]; got != string(scope.ProvenanceContract) {
		t.Errorf("AuditAnnotations[%s] = %q, want %q", provenanceAuditKey, got, scope.ProvenanceContract)
	}
}

// TestResolveScopeContractOwnershipRootDefaultsToContractNamespace (test 21b): an
// L3 `dim: ownership` root written WITHOUT a namespace belongs to the TaskContract CR's
// own namespace — which the same-namespace rule has already pinned to the request
// target's namespace — not to "default".
//
// The offline defaulting rule (NamespaceFor: a namespaced kind with no namespace is in
// "default") is right for a corpus file that has no namespace of its own, and wrong for
// a namespaced CR: it would build the key Deployment/default/web, miss, and walk an empty
// subtree — a confusing fail-closed Block for a contract whose author named a root that
// plainly exists beside it.
func TestResolveScopeContractOwnershipRootDefaultsToContractNamespace(t *testing.T) {
	objs := reRootObjects()
	cr := taskContractCR(t, "prod", "tree",
		`{"dim":"ownership","root":{"group":"apps","version":"v1","kind":"Deployment","name":"web"}}`)

	s := newTestServer(t, closure.NewScanState(objs), scope.ModeEnforce, withObjects(objs), withContracts(contractsWith(cr)))
	out := s.Handle(context.Background(), rsDeleteReview("l3-tree", annotations(DefaultScopeAnnotation, "prod/tree")))
	if !out.Response.Allowed {
		t.Fatalf("an ownership root with no namespace must resolve in the CONTRACT's namespace and cover its subtree, got deny %q", out.Response.Result.Message)
	}
	if got := out.Response.AuditAnnotations[provenanceAuditKey]; got != string(scope.ProvenanceContract) {
		t.Errorf("AuditAnnotations[%s] = %q, want %q", provenanceAuditKey, got, scope.ProvenanceContract)
	}
}

// TestResolveScopeContractOwnershipRootIsResolvedGroupAware (test 21b, miss-rule half):
// an L3 ownership root is resolved through the group-AWARE GetByGVK and pinned to the
// resolved object's real uid, and a MISS is scope-unresolved in both modes — the same
// corrected rule L1's re-root follows, for the same reason.
//
// contract.Parse stamps roots with a SyntheticUID, which is an offline convention: no
// live object carries it, so the engine's lookup falls straight through to the group-blind
// Kind/ns/name bucket. With this fixture that bucket holds the b-group Widget, whose
// subtree covers the whole closure — so a contract naming the ABSENT a-group Widget would
// be ALLOWED by another group's tree. A synthetic uid is therefore not merely useless
// here, it is actively unsafe, and the root has to be resolved or refused.
func TestResolveScopeContractOwnershipRootIsResolvedGroupAware(t *testing.T) {
	objs := absentRootCollisionState() // only Widget.b.example.com/prod/w exists
	st := closure.NewScanState(objs)
	root := func(group string) string {
		return `{"dim":"ownership","root":{"group":"` + group + `","version":"v1","kind":"Widget","name":"w"}}`
	}

	for _, mode := range []scope.Mode{scope.ModeAudit, scope.ModeEnforce} {
		cr := taskContractCR(t, "prod", "ghost", root("a.example.com"))
		out := newTestServer(t, st, mode, withObjects(objs), withContracts(contractsWith(cr))).
			Handle(context.Background(), rsDeleteReview("l3-ghost-"+string(mode), annotations(DefaultScopeAnnotation, "prod/ghost")))
		if out.Response.Allowed {
			t.Errorf("mode %s: a contract root absent from its OWN group must fail closed, got allow (the other group's subtree authorised it)", mode)
			continue
		}
		if msg := out.Response.Result.Message; !strings.Contains(msg, string(reasonScopeUnresolved)) {
			t.Errorf("mode %s: deny %q must carry the scope-unresolved code", mode, msg)
		}
	}

	// The control: the root that DOES exist resolves group-aware and covers its subtree,
	// so the denies above are about resolution, not about ownership roots being unusable.
	cr := taskContractCR(t, "prod", "real", root("b.example.com"))
	out := newTestServer(t, st, scope.ModeEnforce, withObjects(objs), withContracts(contractsWith(cr))).
		Handle(context.Background(), rsDeleteReview("l3-real", annotations(DefaultScopeAnnotation, "prod/real")))
	if !out.Response.Allowed {
		t.Errorf("the contract root that DOES exist must resolve and cover its subtree, got deny %q", out.Response.Result.Message)
	}
}

// TestResolveScopeContractFailuresFailClosed (test 23): EVERY way contract resolution can
// fail — the seam not wired, the CR absent, the GET erroring, RBAC refusing, the deadline
// gone, the contract uncompilable, the CR carrying an unknown field — is one
// scope-unresolved deny, in BOTH modes.
//
// The mode-independence is the point. Audit exists to soften a COMPUTED escape so a day-0
// false positive does not get KRSM uninstalled; an authorisation claim that could not be
// verified is not a computed escape, and softening it would turn "I could not read your
// contract" into "you may proceed". Nor may any of these fall back to L0: the derived tree
// is a different scope from the one the author declared.
func TestResolveScopeContractFailuresFailClosed(t *testing.T) {
	objs := reRootObjects()
	st := closure.NewScanState(objs)
	present := contractsWith(taskContractCR(t, "prod", "task-1", `{"dim":"namespace","namespace":"prod"}`))

	expired, cancel := context.WithCancel(context.Background())
	cancel()

	cases := map[string]struct {
		contracts contractGetter // nil → the seam is not wired at all
		ctx       context.Context
		cr        *unstructured.Unstructured // when set, replaces the served contract
	}{
		"nil getter (L3 not wired)": {},
		"contract not found":        {contracts: fakeContracts{}},
		"GET error":                 {contracts: fakeContracts{err: errors.New("connection refused")}},
		"RBAC forbidden": {contracts: fakeContracts{err: apierrors.NewForbidden(
			schema.GroupResource{Group: "krsm.io", Resource: "taskcontracts"}, "task-1", errors.New("no permission"))}},
		"expired deadline": {contracts: present, ctx: expired},
		"uncompilable contract": {cr: taskContractCR(t, "prod", "task-1",
			`{"dim":"reference","name":"web"}`)},
		"unknown-field CR": {cr: taskContractCR(t, "prod", "task-1",
			`{"dim":"resource","gvk":{"version":"v1","kind":"Service"},"namesapce":"prod","name":"svc"}`)},
		"wrong apiVersion CR": {contracts: contractsWith(func() *unstructured.Unstructured {
			u := taskContractCR(t, "prod", "task-1", `{"dim":"namespace","namespace":"prod"}`)
			u.SetAPIVersion("krsm.io/v1")
			return u
		}())},
	}

	for name, tc := range cases {
		getter := tc.contracts
		if tc.cr != nil {
			getter = contractsWith(tc.cr)
		}
		ctx := tc.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		for _, mode := range []scope.Mode{scope.ModeAudit, scope.ModeEnforce} {
			opts := []func(*Config){withObjects(objs)}
			if getter != nil {
				opts = append(opts, withContracts(getter))
			}
			out := newTestServer(t, st, mode, opts...).
				Handle(ctx, rsDeleteReview("fail", annotations(DefaultScopeAnnotation, "prod/task-1")))
			if out.Response.Allowed {
				t.Errorf("%s / mode %s: an unresolvable contract must fail closed, got allow", name, mode)
				continue
			}
			if msg := out.Response.Result.Message; !strings.Contains(msg, string(reasonScopeUnresolved)) {
				t.Errorf("%s / mode %s: deny %q must carry the scope-unresolved code", name, mode, msg)
			}
		}
	}
}

// TestResolveScopeContractCrossNamespaceFailsClosed (test 24): the same-namespace rule is
// the ONLY bound on a bearer capability in v0.5, so it is enforced before the contract is
// even read. A reference to a contract in another namespace fails closed EVEN WHEN that
// contract exists and would authorise the action — otherwise a compromised agent in a
// permissive namespace's neighbour could simply point at the permissive contract. A
// cluster-scoped request has no namespace to be bound BY, so it cannot use L3 at all here.
func TestResolveScopeContractCrossNamespaceFailsClosed(t *testing.T) {
	objs := reRootObjects()
	// The contract EXISTS, in staging, and grants everything — so a pass here would be a
	// real capability leak, not a lookup miss.
	permissive := contractsWith(taskContractCR(t, "staging", "task-1", `{"dim":"namespace","namespace":"prod"}`))

	for _, mode := range []scope.Mode{scope.ModeAudit, scope.ModeEnforce} {
		out := newTestServer(t, closure.NewScanState(objs), mode, withObjects(objs), withContracts(permissive)).
			Handle(context.Background(), rsDeleteReview("xns-"+string(mode), annotations(DefaultScopeAnnotation, "staging/task-1")))
		if out.Response.Allowed {
			t.Errorf("mode %s: a cross-namespace contract reference must fail closed, got allow", mode)
			continue
		}
		if msg := out.Response.Result.Message; !strings.Contains(msg, string(reasonScopeUnresolved)) {
			t.Errorf("mode %s: deny %q must carry the scope-unresolved code", mode, msg)
		}
	}

	// The cluster-scoped mirror: a PersistentVolume delete has no namespace, so no
	// same-namespace bound can hold and L3 is refused rather than defaulted to some
	// namespace nobody named.
	//
	// The VERDICT here is subsumed by the same-namespace check above ("" never equals the
	// referenced namespace), so the assertion that earns the dedicated branch is the
	// DIAGNOSIS: reporting this as a cross-namespace reference would tell an operator to
	// go move a contract that no namespace could ever satisfy.
	pv := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "PersistentVolume"}, Name: "pv-1", UID: "uid-pv"}}
	pvObjs := []closure.Object{pv}
	clusterScoped := contractsWith(taskContractCR(t, "prod", "task-1", `{"dim":"namespace","namespace":"prod"}`))
	for _, mode := range []scope.Mode{scope.ModeAudit, scope.ModeEnforce} {
		out := newTestServer(t, closure.NewScanState(pvObjs), mode, withObjects(pvObjs), withContracts(clusterScoped)).
			Handle(context.Background(), pvDeleteReview("cs-l3-"+string(mode), annotations(DefaultScopeAnnotation, "prod/task-1")))
		if out.Response.Allowed {
			t.Errorf("mode %s: a cluster-scoped request must not be able to use L3, got allow", mode)
			continue
		}
		msg := out.Response.Result.Message
		if !strings.Contains(msg, string(reasonScopeUnresolved)) {
			t.Errorf("mode %s: deny %q must carry the scope-unresolved code", mode, msg)
		}
		if !strings.Contains(msg, "cluster-scoped") {
			t.Errorf("mode %s: deny %q must diagnose the CLUSTER-SCOPED request, not report a cross-namespace reference the operator could never fix", mode, msg)
		}
	}
}

// TestResolveScopeMalformedContractRefFailsClosedInvalid (test 25): a krsm.io/scope whose
// SHAPE is wrong — not exactly two non-empty, whitespace-free segments — is `invalid`, the
// code distinct from the scope-unresolved one a well-formed-but-unresolvable reference
// gets. Operators grep a typo apart from a broken or revoked reference.
func TestResolveScopeMalformedContractRefFailsClosedInvalid(t *testing.T) {
	objs := reRootObjects()
	present := contractsWith(taskContractCR(t, "prod", "task-1", `{"dim":"namespace","namespace":"prod"}`))

	for name, value := range map[string]string{
		"one segment":         "task-1",
		"three segments":      "prod/task-1/extra",
		"empty value":         "",
		"empty namespace":     "/task-1",
		"empty name":          "prod/",
		"whitespace in ns":    "pr od/task-1",
		"whitespace in name":  "prod/task 1",
		"both empty segments": "/",
	} {
		out := newTestServer(t, closure.NewScanState(objs), scope.ModeEnforce, withObjects(objs), withContracts(present)).
			Handle(context.Background(), rsDeleteReview("badref", annotations(DefaultScopeAnnotation, value)))
		if out.Response.Allowed {
			t.Errorf("%s (%q): a malformed contract reference must fail closed, got allow", name, value)
			continue
		}
		msg := out.Response.Result.Message
		if !strings.Contains(msg, string(reasonInvalid)) {
			t.Errorf("%s (%q): deny %q must carry the invalid code", name, value, msg)
		}
	}
}

// TestL3UnavailabilityDoesNotDisableL0AndL1 (test 26): a broken contract channel fails
// only the requests that USE it. On one server whose getter errors on every read, an L0
// request still gets its derived verdict and an L1 request still re-roots — because
// neither path touches the getter at all.
//
// This is the availability half of "fail closed": a gate that denied every request the
// moment an optional dependency broke would be uninstalled after its first CRD hiccup,
// and a KRSM that is not installed protects nothing.
func TestL3UnavailabilityDoesNotDisableL0AndL1(t *testing.T) {
	objs := reRootObjects()
	broken := fakeContracts{err: errors.New("apiserver unreachable")}
	s := newTestServer(t, closure.NewScanState(objs), scope.ModeAudit, withObjects(objs), withContracts(broken))

	// L0: the derived verdict is computed and reported exactly as with a healthy L3.
	out := s.Handle(context.Background(), rsDeleteReview("avail-l0", ""))
	warn := strings.Join(out.Response.Warnings, "\n")
	if !out.Response.Allowed || !strings.Contains(warn, "Service/prod/svc") {
		t.Errorf("L0 must still decide while L3 is down; got allowed=%v warnings=%q", out.Response.Allowed, out.Response.Warnings)
	}
	if !strings.Contains(warn, string(scope.ProvenanceDerivedOwner)) {
		t.Errorf("L0 warnings %q must still report the derived provenance", warn)
	}

	// L1: the re-root still resolves and covers the closure.
	out = s.Handle(context.Background(), rsDeleteReview("avail-l1", annotations(DefaultTargetAnnotation, "Deployment/prod/web")))
	if !out.Response.Allowed || len(out.Response.Warnings) != 0 {
		t.Errorf("L1 must still re-root while L3 is down; got allowed=%v warnings=%q", out.Response.Allowed, out.Response.Warnings)
	}

	// The control: an L3 request on the SAME server does fail closed, so the two
	// assertions above are about isolation, not about the getter being ignored.
	out = s.Handle(context.Background(), rsDeleteReview("avail-l3", annotations(DefaultScopeAnnotation, "prod/task-1")))
	if out.Response.Allowed {
		t.Error("the L3 request on the same server must fail closed, got allow")
	}
}

// TestResolveScopeContractOutranksTargetAnnotation (test 27): with both governing
// annotations present the contract wins outright — the levels are a strict priority, not
// a union.
//
// The fixture makes the contract the NARROWER of the two on purpose: the target
// annotation re-roots at the Deployment (covering the whole closure, an Allow) while the
// contract authorises only the ReplicaSet's own subtree (the Service escapes, a Block).
// A "most permissive wins" or a merging implementation would Allow here. Only true
// priority denies — and reports provenance `contract`, so the operator sees which
// statement of intent was honoured.
func TestResolveScopeContractOutranksTargetAnnotation(t *testing.T) {
	objs := reRootObjects()
	narrow := contractsWith(taskContractCR(t, "prod", "narrow",
		`{"dim":"ownership","root":{"group":"apps","version":"v1","kind":"ReplicaSet","name":"web-1"}}`))
	s := newTestServer(t, closure.NewScanState(objs), scope.ModeEnforce, withObjects(objs), withContracts(narrow))

	// Control: the target annotation ALONE allows this delete.
	out := s.Handle(context.Background(), rsDeleteReview("prio-ctl", annotations(DefaultTargetAnnotation, "Deployment/prod/web")))
	if !out.Response.Allowed {
		t.Fatalf("control: the target annotation alone must allow, else test 27 proves nothing; got deny %q", out.Response.Result.Message)
	}

	out = s.Handle(context.Background(), rsDeleteReview("prio", annotations(
		DefaultTargetAnnotation, "Deployment/prod/web",
		DefaultScopeAnnotation, "prod/narrow")))
	if out.Response.Allowed {
		t.Fatal("the NARROWER contract must win over the target annotation, got allow (the levels were merged or the widest won)")
	}
	msg := out.Response.Result.Message
	if !strings.Contains(msg, string(scope.ProvenanceContract)) {
		t.Errorf("deny %q must report the contract provenance", msg)
	}
	if strings.Contains(msg, string(scope.ProvenanceAnnotation)) {
		t.Errorf("deny %q must not report the annotation provenance — the contract governed", msg)
	}
	if !strings.Contains(msg, "Service/prod/svc") {
		t.Errorf("deny %q must name the escape the CONTRACT's scope leaves uncovered", msg)
	}
}

// TestHandleReportsAllowlistExemptionsInAuditAnnotations (tests 27b + 28): when the
// cross-boundary allowlist drops escapes, the response records WHICH refs it dropped —
// machine-readable, beside the provenance. The key appears ONLY when an exemption actually
// occurred: an always-present (or empty) key would make "the operator's allowlist silently
// widened this verdict" indistinguishable from "nothing was exempted" in an audit query,
// and the exemption is the fact worth reviewing.
//
// It names the refs because a verdict softened by a flag is the one an auditor must be able
// to reconstruct: the residual message deliberately reports only what is LEFT to act on, so
// without this key the exempted collateral appears nowhere at all.
func TestHandleReportsAllowlistExemptionsInAuditAnnotations(t *testing.T) {
	objs := secretState(
		secretConsumer(gatewayGVK, "infra", "gw", "uid-gw"),
		secretConsumer(gatewayGVK, "staging", "gw2", "uid-gw2"),
	)
	st := closure.NewScanState(objs)

	// One of the two indirect escapes is exempted: the key names exactly that one, and
	// the residual message still names the other.
	out := newTestServer(t, st, scope.ModeEnforce, withObjects(objs),
		withAllowlist(Allowlist{Namespaces: map[string]bool{"infra": true}})).
		Handle(context.Background(), secretDeleteReview("ex-1", ""))
	got := out.Response.AuditAnnotations[exemptedAuditKey]
	if !strings.Contains(got, "Gateway/infra/gw") {
		t.Errorf("AuditAnnotations[%s] = %q, must name the exempted ref", exemptedAuditKey, got)
	}
	if strings.Contains(got, "Gateway/staging/gw2") {
		t.Errorf("AuditAnnotations[%s] = %q, must name only the EXEMPTED refs, not the residual escape", exemptedAuditKey, got)
	}

	// No allowlist at all: nothing was exempted, so the key is absent — not empty.
	out = newTestServer(t, st, scope.ModeEnforce, withObjects(objs)).
		Handle(context.Background(), secretDeleteReview("ex-2", ""))
	if v, ok := out.Response.AuditAnnotations[exemptedAuditKey]; ok {
		t.Errorf("with no allowlist the exemption key must be ABSENT, got %q", v)
	}

	// An allowlist that matches nothing in this closure is equally no exemption.
	out = newTestServer(t, st, scope.ModeEnforce, withObjects(objs),
		withAllowlist(Allowlist{Namespaces: map[string]bool{"unrelated": true}})).
		Handle(context.Background(), secretDeleteReview("ex-3", ""))
	if v, ok := out.Response.AuditAnnotations[exemptedAuditKey]; ok {
		t.Errorf("an allowlist that exempted nothing must leave the key ABSENT, got %q", v)
	}

	// The provenance key is always there regardless — the two are independent facts.
	if got := out.Response.AuditAnnotations[provenanceAuditKey]; got != string(scope.ProvenanceDerivedOwner) {
		t.Errorf("AuditAnnotations[%s] = %q, want %q", provenanceAuditKey, got, scope.ProvenanceDerivedOwner)
	}
}

// TestAllowlistIgnoredForContractProvenanceEndToEnd (test 21, end-to-end half): the
// step-3 test could only assert the allowlistApplies gate directly, because L3 could not
// be driven through Handle before the contractGetter seam existed. It can now, so the
// property is pinned where it matters: a decision reached under a DECLARED contract is
// never widened by an operator flag.
//
// The allowlist exists because a DERIVED scope cannot know which shared boundaries are
// legitimate. A contract has no such gap — its author said exactly what is in scope — so
// letting --allow-namespace soften it would overrule that author from the server command
// line, silently and invisibly to them.
func TestAllowlistIgnoredForContractProvenanceEndToEnd(t *testing.T) {
	objs := secretState(secretConsumer(gatewayGVK, "infra", "gw", "uid-gw"))
	st := closure.NewScanState(objs)
	// The contract authorises the Secret alone, so the Gateway escapes it.
	cr := taskContractCR(t, "prod", "narrow",
		`{"dim":"resource","gvk":{"version":"v1","kind":"Secret"},"namespace":"prod","name":"tls"}`)
	wide := Allowlist{Namespaces: map[string]bool{"infra": true}}

	// Control: under the DERIVED scope that very allowlist exempts the escape and admits.
	out := newTestServer(t, st, scope.ModeEnforce, withObjects(objs), withAllowlist(wide)).
		Handle(context.Background(), secretDeleteReview("l3-al-ctl", ""))
	if !out.Response.Allowed {
		t.Fatalf("control: the allowlist must exempt this escape under a derived scope, else the test proves nothing; got deny %q", out.Response.Result.Message)
	}

	out = newTestServer(t, st, scope.ModeEnforce, withObjects(objs), withAllowlist(wide), withContracts(contractsWith(cr))).
		Handle(context.Background(), secretDeleteReview("l3-al", annotations(DefaultScopeAnnotation, "prod/narrow")))
	if out.Response.Allowed {
		t.Fatal("a contract-provenance decision must ignore the allowlist, got allow (a server flag widened a declared scope)")
	}
	msg := out.Response.Result.Message
	if !strings.Contains(msg, "Gateway/infra/gw") {
		t.Errorf("deny %q must still name the escape the allowlist would have exempted", msg)
	}
	if _, ok := out.Response.AuditAnnotations[exemptedAuditKey]; ok {
		t.Errorf("no exemption may be recorded for a contract decision, got %v", out.Response.AuditAnnotations)
	}
}

// --- Step 5: configurable annotation keys (Config.TargetAnnotation/ScopeAnnotation) ---

// TestConfiguredAnnotationKeysGovern: the scope-governing annotation KEYS belong to the
// server's configuration, not to package constants — an operator running KRSM beside
// another controller (or on their own domain) sets --target-annotation/--scope-annotation
// and BOTH channels move to those keys. The behavioural claim is a pair: the configured
// key governs, and the default key becomes INERT, because a key that stayed live after
// being reconfigured would let anyone who knows "krsm.io/target" re-root a scope the
// operator believed they had moved.
//
// Note the direction of the inert half: an unrecognised key falls to L0, the derived
// tree of the request's own target — strictly NARROWER than the re-root it names. A
// mis-configured key can therefore only over-restrict, never widen.
func TestConfiguredAnnotationKeysGovern(t *testing.T) {
	objs := reRootObjects()
	st := closure.NewScanState(objs)
	const customTarget, customScope = "example.com/target", "example.com/scope"

	s := newTestServer(t, st, scope.ModeAudit, withObjects(objs),
		withContracts(contractsWith(taskContractCR(t, "prod", "task-1", `{"dim":"namespace","namespace":"prod"}`))),
		withAnnotationKeys(customTarget, customScope))

	for name, tc := range map[string]struct {
		key, value string
		want       scope.Provenance
	}{
		"configured target key re-roots":   {customTarget, "Deployment/prod/web", scope.ProvenanceAnnotation},
		"default target key is inert":      {DefaultTargetAnnotation, "Deployment/prod/web", scope.ProvenanceDerivedOwner},
		"configured scope key resolves L3": {customScope, "prod/task-1", scope.ProvenanceContract},
		"default scope key is inert":       {DefaultScopeAnnotation, "prod/task-1", scope.ProvenanceDerivedOwner},
	} {
		out := s.Handle(context.Background(), rsDeleteReview("cfg-"+name, annotations(tc.key, tc.value)))
		if got := out.Response.AuditAnnotations[provenanceAuditKey]; got != string(tc.want) {
			t.Errorf("%s: provenance = %q, want %q", name, got, tc.want)
		}
	}

	// A deny names the CONFIGURED key, not the default one: an operator who moved the
	// channel must be able to grep the message for the key they actually configured.
	out := s.Handle(context.Background(), rsDeleteReview("cfg-bad", annotations(customTarget, "no-slashes")))
	if out.Response.Allowed {
		t.Fatalf("a malformed re-root must fail closed, got allow")
	}
	if msg := out.Response.Result.Message; !strings.Contains(msg, customTarget) {
		t.Errorf("deny %q must name the configured key %q", msg, customTarget)
	}
}

// TestNewRejectsCollidingAnnotationKeys: configuring both channels on ONE key is refused
// at startup. It is not silently harmless — resolveScope tries L3 first, so every L1
// re-root value would be read as a contract reference and denied as malformed. That is
// fail-closed, but it is an unserveable configuration, and a webhook that cannot serve
// its documented behaviour must not start (the same posture as a missing mode/TLS).
func TestNewRejectsCollidingAnnotationKeys(t *testing.T) {
	_, err := New(Config{
		State: reRootState(), ScopeInfo: testScope{}, Synced: func() bool { return true },
		Mode: scope.ModeAudit, Fresh: NoFreshness,
		TargetAnnotation: "example.com/scope", ScopeAnnotation: "example.com/scope",
	})
	if err == nil {
		t.Fatal("New must reject one key configured for both scope channels")
	}

	// The collision is checked AFTER defaulting, so half-configuring onto the other
	// channel's default is caught too.
	if _, err := New(Config{
		State: reRootState(), ScopeInfo: testScope{}, Synced: func() bool { return true },
		Mode: scope.ModeAudit, Fresh: NoFreshness,
		TargetAnnotation: DefaultScopeAnnotation,
	}); err == nil {
		t.Fatal("New must reject a target key that collides with the DEFAULT scope key")
	}
}
