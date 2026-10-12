package webhook

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/scope"
)

// --- Review round 1, step 1: a live contract clause inherits the contract's namespace ---

// configConsumer builds a Pod that mounts ConfigMap prod/cfg. ns may differ from prod:
// the closure engine matches a cross-ref by the referenced object, so a consumer in
// another namespace is collateral of deleting prod/cfg — which is what lets a test place
// a same-named object in `default` and watch whether a contract authorises it.
func configConsumer(namespace, name, uid string) closure.Object {
	return closure.Object{
		Ref:    closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Pod"}, Namespace: namespace, Name: name, UID: uid},
		Labels: map[string]string{"app": "web"},
		CrossRefs: []closure.CrossRef{{
			Kind: closure.RefVolume,
			Ref:  closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "ConfigMap"}, Namespace: "prod", Name: "cfg"},
		}},
	}
}

// configState is ConfigMap prod/cfg plus the given consumers. Deleting the ConfigMap
// closes over every consumer; the derived scope rooted at the ConfigMap covers only
// itself, so each consumer is collateral a contract has to authorise.
func configState(consumers ...closure.Object) []closure.Object {
	cm := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "ConfigMap"}, Namespace: "prod", Name: "cfg", UID: "uid-cfg"}}
	return append([]closure.Object{cm}, consumers...)
}

// cmDeleteReview builds the DELETE of ConfigMap prod/cfg whose oldObject carries the
// given metadata annotations fragment (empty → none).
func cmDeleteReview(uid, annotations string) admissionv1.AdmissionReview {
	old := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cfg","namespace":"prod","uid":"uid-cfg","resourceVersion":"7"` + annotations + `}}`
	return admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:       types.UID("req-" + uid),
			Operation: admissionv1.Delete,
			Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
			Namespace: "prod",
			Name:      "cfg",
			OldObject: raw(old),
		},
	}
}

// TestContractResourceClauseInheritsContractNamespace (review-1 entry 1): a resource
// clause that omits `namespace` in a TaskContract CR living in `prod` grants objects in
// `prod` — the namespace its author wrote it in — exactly as an ownership root without a
// namespace already does. The offline loader's "default" is for a contract that has no
// namespace of its own; applied to a live CR it would grant a namespace nobody named and
// none of the one the author meant.
func TestContractResourceClauseInheritsContractNamespace(t *testing.T) {
	objs := configState(configConsumer("prod", "app", "uid-app"))
	cr := taskContractCR(t, "prod", "task-1",
		`{"dim":"resource","gvk":{"version":"v1","kind":"ConfigMap"},"name":"cfg"}`,
		`{"dim":"resource","gvk":{"version":"v1","kind":"Pod"},"name":"*"}`)

	s := newTestServer(t, closure.NewScanState(objs), scope.ModeEnforce, withObjects(objs), withContracts(contractsWith(cr)))
	out := s.Handle(context.Background(), cmDeleteReview("ns-inherit", annotations(DefaultScopeAnnotation, "prod/task-1")))
	if !out.Response.Allowed {
		t.Fatalf("a namespace-less clause in a prod contract must authorise the prod collateral, got deny %q", out.Response.Result.Message)
	}
	if got := out.Response.AuditAnnotations[provenanceAuditKey]; got != string(scope.ProvenanceContract) {
		t.Errorf("AuditAnnotations[%s] = %q, want %q", provenanceAuditKey, got, scope.ProvenanceContract)
	}
}

// TestContractResourceClauseDoesNotGrantDefault (review-1 entry 2): the mirror of
// entry 1. The same namespace-less clauses must NOT reach the same-named Pod in
// `default` — under the old defaulting that was exactly the object they granted. The
// default-namespace consumer therefore escapes, while its prod twin stays in scope.
func TestContractResourceClauseDoesNotGrantDefault(t *testing.T) {
	objs := configState(configConsumer("prod", "app", "uid-app"), configConsumer("default", "app", "uid-app-default"))
	cr := taskContractCR(t, "prod", "task-1",
		`{"dim":"resource","gvk":{"version":"v1","kind":"ConfigMap"},"name":"cfg"}`,
		`{"dim":"resource","gvk":{"version":"v1","kind":"Pod"},"name":"app"}`)

	for _, mode := range []scope.Mode{scope.ModeAudit, scope.ModeEnforce} {
		s := newTestServer(t, closure.NewScanState(objs), mode, withObjects(objs), withContracts(contractsWith(cr)))
		out := s.Handle(context.Background(), cmDeleteReview("ns-default-"+string(mode), annotations(DefaultScopeAnnotation, "prod/task-1")))
		var msg string
		switch mode {
		case scope.ModeEnforce:
			if out.Response.Allowed {
				t.Fatal("enforce: the default-namespace consumer must escape a prod contract, got allow")
			}
			msg = out.Response.Result.Message
		default:
			msg = strings.Join(out.Response.Warnings, "\n")
		}
		if !strings.Contains(msg, "Pod/default/app") {
			t.Errorf("mode %s: %q must name the default-namespace Pod as escaping", mode, msg)
		}
		if strings.Contains(msg, "Pod/prod/app") || strings.Contains(msg, "ConfigMap/prod/cfg") {
			t.Errorf("mode %s: %q must not report the prod objects the contract grants", mode, msg)
		}
	}
}

// TestContractSelectorClauseInheritsContractNamespace (review-1 entry 3): a selector
// clause without a namespace gates the contract's namespace too — the selector picks
// Pods in `prod` by label, and the identically-labelled Pod in `default` escapes.
func TestContractSelectorClauseInheritsContractNamespace(t *testing.T) {
	objs := configState(configConsumer("prod", "app", "uid-app"), configConsumer("default", "app", "uid-app-default"))
	cr := taskContractCR(t, "prod", "task-1",
		`{"dim":"resource","gvk":{"version":"v1","kind":"ConfigMap"},"namespace":"prod","name":"cfg"}`,
		`{"dim":"selector","gvk":{"version":"v1","kind":"Pod"},"matchLabels":{"app":"web"}}`)

	s := newTestServer(t, closure.NewScanState(objs), scope.ModeEnforce, withObjects(objs), withContracts(contractsWith(cr)))
	out := s.Handle(context.Background(), cmDeleteReview("sel-ns", annotations(DefaultScopeAnnotation, "prod/task-1")))
	if out.Response.Allowed {
		t.Fatal("the default-namespace Pod must escape a prod selector clause, got allow")
	}
	msg := out.Response.Result.Message
	if !strings.Contains(msg, "Pod/default/app") {
		t.Errorf("deny %q must name the default-namespace Pod as escaping", msg)
	}
	if strings.Contains(msg, "Pod/prod/app") {
		t.Errorf("deny %q must not report the prod Pod the selector clause grants", msg)
	}
}

// --- Review round 1, step 2: the L1 re-root is the target or an in-namespace owner ancestor ---

// TestReRootAtTheTargetItselfGivesTheL0Verdict (review-1 entry 6): naming the action
// target itself is the degenerate ancestor — R == target — and is accepted. Its subtree
// is exactly the L0 derived tree, so the verdict is the L0 one: the Service still
// escapes, reported as an escape (not a resolution failure), only under the
// `annotation` provenance.
func TestReRootAtTheTargetItselfGivesTheL0Verdict(t *testing.T) {
	objs := reRootObjects()
	for _, mode := range []scope.Mode{scope.ModeAudit, scope.ModeEnforce} {
		s := newTestServer(t, closure.NewScanState(objs), mode, withObjects(objs))
		l0 := s.Handle(context.Background(), rsDeleteReview("self-l0", ""))
		l1 := s.Handle(context.Background(), rsDeleteReview("self-l1", annotations(DefaultTargetAnnotation, "ReplicaSet.apps/prod/web-1")))

		if l1.Response.Allowed != l0.Response.Allowed {
			t.Errorf("mode %s: re-root at the target allowed=%v, L0 allowed=%v — want the L0 verdict", mode, l1.Response.Allowed, l0.Response.Allowed)
		}
		msg := strings.Join(l1.Response.Warnings, "\n")
		if l1.Response.Result != nil {
			msg += l1.Response.Result.Message
		}
		if !strings.Contains(msg, string(reasonEscape)) || !strings.Contains(msg, "Service/prod/svc") {
			t.Errorf("mode %s: %q must report the same escape as L0 (Service/prod/svc)", mode, msg)
		}
		if got := l1.Response.AuditAnnotations[provenanceAuditKey]; got != string(scope.ProvenanceAnnotation) {
			t.Errorf("mode %s: provenance = %q, want %q", mode, got, scope.ProvenanceAnnotation)
		}
	}
}

// assertScopeUnresolved runs one L1 review in BOTH modes and requires a
// scope-unresolved deny whose message contains want. Audit softens only a computed
// escape, so a re-root the bound refuses is a deny in audit too — never a Warn.
func assertScopeUnresolved(t *testing.T, objs []closure.Object, review func(mode scope.Mode) admissionv1.AdmissionReview, want string) {
	t.Helper()
	for _, mode := range []scope.Mode{scope.ModeAudit, scope.ModeEnforce} {
		out := newTestServer(t, closure.NewScanState(objs), mode, withObjects(objs)).Handle(context.Background(), review(mode))
		if out.Response.Allowed {
			t.Errorf("mode %s: the re-root must be refused, got allow (warnings %q)", mode, out.Response.Warnings)
			continue
		}
		msg := out.Response.Result.Message
		if !strings.Contains(msg, string(reasonScopeUnresolved)) {
			t.Errorf("mode %s: deny %q must carry the scope-unresolved code", mode, msg)
		}
		if !strings.Contains(msg, want) {
			t.Errorf("mode %s: deny %q must name the broken rule (%q)", mode, msg, want)
		}
	}
}

// TestReRootAtUnrelatedObjectFailsClosed (review-1 entry 7): krsm.io/target may only
// re-root at an OWNER of the action target. An unrelated object in the same namespace
// is not one, so the annotation is refused outright — in both modes — rather than
// re-rooting and leaving the target to "escape" its own scope, which audit would only
// have softened to a Warn.
func TestReRootAtUnrelatedObjectFailsClosed(t *testing.T) {
	other := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "ConfigMap"}, Namespace: "prod", Name: "other", UID: "uid-other"}}
	objs := append(reRootObjects(), other)
	assertScopeUnresolved(t, objs, func(mode scope.Mode) admissionv1.AdmissionReview {
		return rsDeleteReview("unrelated-"+string(mode), annotations(DefaultTargetAnnotation, "ConfigMap/prod/other"))
	}, "is not an owner of the target")
}

// TestReRootAtClusterScopedOwnerFailsClosed (review-1 entry 8): a cluster-scoped object
// that really does OWN the namespaced target is still refused. Its subtree can span
// every namespace it reaches, so re-rooting there would turn one namespaced action's
// scope into a cross-namespace one. The namespace rule — R in the target's own
// namespace — is checked as well as ancestry, and names itself in the deny.
func TestReRootAtClusterScopedOwnerFailsClosed(t *testing.T) {
	pv := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "PersistentVolume"}, Name: "pv-1", UID: "uid-pv"}}
	objs := reRootObjects()
	for i := range objs {
		if objs[i].Ref.Name == "web-1" || objs[i].Ref.Name == "svc" {
			objs[i].Owners = []closure.OwnerRef{{Kind: "PersistentVolume", Name: "pv-1", UID: "uid-pv"}}
		}
	}
	objs = append(objs, pv)
	assertScopeUnresolved(t, objs, func(mode scope.Mode) admissionv1.AdmissionReview {
		return rsDeleteReview("cs-owner-"+string(mode), annotations(DefaultTargetAnnotation, "PersistentVolume//pv-1"))
	}, `re-root must be in the target's namespace "prod"`)
}

// TestReRootInAnotherNamespaceFailsClosed (review-1 entry 9): a re-root in a namespace
// other than the target's is refused by the namespace rule. The fixture is deliberately
// hostile — state records the staging Deployment as the target's owner (ownerReferences
// cannot legitimately cross namespaces, but the gate must not depend on that) — so
// ancestry alone would accept it, and only the namespace rule stands in the way.
func TestReRootInAnotherNamespaceFailsClosed(t *testing.T) {
	foreign := closure.Object{Ref: closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}, Namespace: "staging", Name: "web", UID: "uid-d-staging"}}
	objs := reRootObjects()
	for i := range objs {
		if objs[i].Ref.Name == "web-1" && objs[i].Ref.GVK.Kind == "ReplicaSet" {
			objs[i].Owners = []closure.OwnerRef{{Kind: "Deployment", Name: "web", UID: "uid-d-staging"}}
		}
	}
	objs = append(objs, foreign)
	assertScopeUnresolved(t, objs, func(mode scope.Mode) admissionv1.AdmissionReview {
		return rsDeleteReview("xns-"+string(mode), annotations(DefaultTargetAnnotation, "Deployment/staging/web"))
	}, `re-root must be in the target's namespace "prod"`)
}

// TestReRootThroughUntrackedIntermediateFailsClosed (review-1 entry 10): ancestry is
// only provable through objects KRSM tracks. Here the Deployment really owns X and X
// owns the ReplicaSet, but X's kind is not watched, so the link from the Deployment's
// subtree to the target is missing. The re-root is refused — fail closed, with the
// rule named — rather than approximated: the operator's fix is to track the kind.
func TestReRootThroughUntrackedIntermediateFailsClosed(t *testing.T) {
	objs := reRootObjects()
	for i := range objs {
		if objs[i].Ref.Name == "web-1" && objs[i].Ref.GVK.Kind == "ReplicaSet" {
			objs[i].Owners = []closure.OwnerRef{{Kind: "Rollout", Name: "web", UID: "uid-untracked-x"}}
		}
	}
	assertScopeUnresolved(t, objs, func(mode scope.Mode) admissionv1.AdmissionReview {
		return rsDeleteReview("gap-"+string(mode), annotations(DefaultTargetAnnotation, "Deployment/prod/web"))
	}, "is not an owner of the target")
}

// TestReRootOwnershipCycleTerminates (review-1 entry 11): state is attacker-shaped input
// — ownerReferences are written by whoever can write the object — so a cycle (A owns B,
// B owns A) must not hang the ancestry walk past the admission deadline. The walk keeps
// a visited set; the cycle does not reach the target, so the re-root is refused.
func TestReRootOwnershipCycleTerminates(t *testing.T) {
	cm := func(name, uid, owner, ownerUID string) closure.Object {
		return closure.Object{
			Ref:    closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "ConfigMap"}, Namespace: "prod", Name: name, UID: uid},
			Owners: []closure.OwnerRef{{Kind: "ConfigMap", Name: owner, UID: ownerUID}},
		}
	}
	objs := append(reRootObjects(), cm("a", "uid-a", "b", "uid-b"), cm("b", "uid-b", "a", "uid-a"))
	assertScopeUnresolved(t, objs, func(mode scope.Mode) admissionv1.AdmissionReview {
		return rsDeleteReview("cycle-"+string(mode), annotations(DefaultTargetAnnotation, "ConfigMap/prod/a"))
	}, "is not an owner of the target")
}

// --- Review round 1, step 5: the empty-allow contract ---

// TestEmptyAllowContractDeniesCollateral (review-1 entry 18): a TaskContract that
// authorises NOTHING — an empty `allow`, or no spec at all — is a valid, deny-all scope,
// not a resolution failure. Compile accepts it; every collateral object then escapes, so
// enforce Blocks and audit Warns with provenance `contract`. That is fail-safe: the
// author declared an empty scope and gets exactly that.
func TestEmptyAllowContractDeniesCollateral(t *testing.T) {
	objs := reRootObjects()
	noSpec := taskContractCR(t, "prod", "nospec")
	delete(noSpec.Object, "spec")
	getter := fakeContracts{objs: map[string]*unstructured.Unstructured{
		"prod/empty":  taskContractCR(t, "prod", "empty"),
		"prod/nospec": noSpec,
	}}

	for _, ref := range []string{"prod/empty", "prod/nospec"} {
		enforce := newTestServer(t, closure.NewScanState(objs), scope.ModeEnforce, withObjects(objs), withContracts(getter)).
			Handle(context.Background(), rsDeleteReview("empty-e", annotations(DefaultScopeAnnotation, ref)))
		if enforce.Response.Allowed {
			t.Fatalf("%s: enforce must deny the collateral of a deny-all contract, got allow", ref)
		}
		if msg := enforce.Response.Result.Message; !strings.Contains(msg, string(reasonEscape)) || !strings.Contains(msg, "Service/prod/svc") {
			t.Errorf("%s: enforce deny %q must be a computed escape naming the collateral, not a resolution failure", ref, msg)
		}

		audit := newTestServer(t, closure.NewScanState(objs), scope.ModeAudit, withObjects(objs), withContracts(getter)).
			Handle(context.Background(), rsDeleteReview("empty-a", annotations(DefaultScopeAnnotation, ref)))
		warn := strings.Join(audit.Response.Warnings, "\n")
		if !audit.Response.Allowed || !strings.Contains(warn, "Service/prod/svc") {
			t.Errorf("%s: audit must allow with a warning naming the collateral, got allowed=%v warnings=%q", ref, audit.Response.Allowed, warn)
		}
		if got := audit.Response.AuditAnnotations[provenanceAuditKey]; got != string(scope.ProvenanceContract) {
			t.Errorf("%s: provenance = %q, want %q", ref, got, scope.ProvenanceContract)
		}
	}
}
