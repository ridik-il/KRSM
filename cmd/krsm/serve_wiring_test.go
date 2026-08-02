package main

// Regression tests for PR #36 review finding 9: the production webhook.Config
// assembly must be under hermetic test — webhook.New tolerates a nil Fresh (guard
// disabled), so only these tests catch a dropped field before it silently disables
// the staleness guard in a cluster.

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/scope"
	"github.com/ridik-il/krsm/state"
	"github.com/ridik-il/krsm/webhook"
)

// TestBuildWebhookConfig: every collaborator the webhook needs to fail closed is
// wired from the Provider, and the flags flow through unchanged.
func TestBuildWebhookConfig(t *testing.T) {
	o := serveOpts{
		agentAnnotation:         "krsm.io/task",
		agentServiceAccounts:    "system:serviceaccount:agents:remediator",
		requestTimeout:          7 * time.Second,
		allowNamespaces:         "infra",
		allowClusterScopedKinds: "Node",
		targetAnnotation:        "example.com/target",
		scopeAnnotation:         "example.com/scope",
	}
	contracts := stubContracts{}
	cfg, err := buildWebhookConfig(o, scope.ModeEnforce, (*state.Provider)(nil), contracts)
	if err != nil {
		t.Fatalf("buildWebhookConfig: %v", err)
	}

	if cfg.State == nil || cfg.ScopeInfo == nil || cfg.Synced == nil {
		t.Error("Config must wire State, ScopeInfo and Synced from the Provider")
	}
	if cfg.Fresh == nil {
		t.Error("Config.Fresh must be wired — a nil Fresh silently disables the staleness guard (C2)")
	}
	if cfg.Mode != scope.ModeEnforce {
		t.Errorf("Mode = %q, want enforce", cfg.Mode)
	}
	if cfg.Timeout != o.requestTimeout {
		t.Errorf("Timeout = %v, want the --request-timeout value %v", cfg.Timeout, o.requestTimeout)
	}
	if _, ok := cfg.Matcher.(webhook.AnyMatcher); !ok {
		t.Errorf("Matcher = %T, want AnyMatcher (serviceaccount OR annotation) when --agent-serviceaccount is set", cfg.Matcher)
	}

	// Entry 30 — the dropped-field regression guard, extended to the slice-6
	// collaborators. Each of these is a SILENT safety change if it goes missing: no
	// contract getter turns every declared L3 scope into a fail-closed deny, a dropped
	// allowlist re-blocks the shared boundaries an operator exempted, and dropped
	// annotation keys move both scope channels back to krsm.io/* without saying so.
	// webhook.New accepts a Config missing any of them, so only this assembly test
	// catches the omission before a cluster does.
	if cfg.Contracts == nil {
		t.Error("Config.Contracts must carry the injected getter — without it every krsm.io/scope request fails closed")
	}
	if !cfg.Allowlist.Namespaces["infra"] || !cfg.Allowlist.ClusterScopedGroupKinds[closure.GVK{Kind: "Node"}] {
		t.Errorf("Config.Allowlist = %#v, want the --allow-namespace/--allow-cluster-scoped-kind entries", cfg.Allowlist)
	}
	if cfg.TargetAnnotation != o.targetAnnotation || cfg.ScopeAnnotation != o.scopeAnnotation {
		t.Errorf("Config annotation keys = %q/%q, want the --target-annotation/--scope-annotation values %q/%q",
			cfg.TargetAnnotation, cfg.ScopeAnnotation, o.targetAnnotation, o.scopeAnnotation)
	}

	o.agentServiceAccounts = ""
	cfg, err = buildWebhookConfig(o, scope.ModeAudit, (*state.Provider)(nil), nil)
	if err != nil {
		t.Fatalf("buildWebhookConfig (annotation only): %v", err)
	}
	if m, ok := cfg.Matcher.(webhook.AnnotationMatcher); !ok || m.Key != "krsm.io/task" {
		t.Errorf("Matcher = %#v, want AnnotationMatcher{krsm.io/task} without --agent-serviceaccount", cfg.Matcher)
	}

	// --gate-all wires the explicit MatchAll, overriding the other signals.
	o.gateAll = true
	cfg, err = buildWebhookConfig(o, scope.ModeAudit, (*state.Provider)(nil), nil)
	if err != nil {
		t.Fatalf("buildWebhookConfig (gate-all): %v", err)
	}
	if _, ok := cfg.Matcher.(webhook.MatchAll); !ok {
		t.Errorf("Matcher = %T, want MatchAll with --gate-all", cfg.Matcher)
	}

	// No signal at all is a usage error, never a silent gate-all.
	o.gateAll, o.agentAnnotation, o.agentServiceAccounts = false, "", ""
	if _, err := buildWebhookConfig(o, scope.ModeAudit, (*state.Provider)(nil), nil); err == nil {
		t.Error("no gating signal (no annotation, no serviceaccount, no --gate-all) must be a usage error")
	}
}

// TestAgentMatcherIdentity: the assembled matcher gates the named serviceaccounts by
// request identity (payload-free — the scale/eviction/CONNECT signal) and everyone
// else only via the annotation.
func TestAgentMatcherIdentity(t *testing.T) {
	m, err := agentMatcher("krsm.io/task", "system:serviceaccount:agents:remediator,system:serviceaccount:agents:scaler", false)
	if err != nil {
		t.Fatalf("agentMatcher: %v", err)
	}

	agent := &admissionv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{Username: "system:serviceaccount:agents:scaler"}}
	if !m.Matches(agent, nil, nil) {
		t.Error("a listed serviceaccount must match with no payload at all")
	}
	human := &admissionv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{Username: "kubernetes-admin"}}
	if m.Matches(human, nil, nil) {
		t.Error("an unlisted identity without the annotation must not match")
	}
}

// --- Step 5: serve wiring (test-list entries 29, 30, 31) ---

// captureServeOpts runs `krsm serve` with the given flags against a stubbed impure tail,
// returning the serveOpts the flag parser produced. It drives the REAL flag set, so the
// flag NAMES an operator types are part of what these tests pin — a renamed flag is a
// broken deployment, not a refactor.
func captureServeOpts(t *testing.T, args ...string) serveOpts {
	t.Helper()
	previous := serveWebhook
	t.Cleanup(func() { serveWebhook = previous })

	var got serveOpts
	serveWebhook = func(o serveOpts, _ scope.Mode, _, _ io.Writer) error {
		got = o
		return nil
	}
	if err := runServe(append([]string{"--tls-cert", "c", "--tls-key", "k"}, args...), io.Discard, io.Discard); err != nil {
		t.Fatalf("runServe(%v): %v", args, err)
	}
	return got
}

// TestServeAllowlistFlagsBuildTheAllowlist (entry 29): --allow-namespace and
// --allow-cluster-scoped-kind assemble the cross-boundary Allowlist. Cluster-scoped
// exemptions key on {Group, Kind}: a BARE Kind means the core group and nothing else, so
// listing "Node" can never exempt some CRD group's own Node — the identity confusion a
// safety gate must not have. List items are trimmed and empty items dropped, because a
// generated flag ("--allow-namespace $NS_LIST") routinely carries a trailing comma and an
// operator must not have to guess whether that silently exempted the "" namespace.
func TestServeAllowlistFlagsBuildTheAllowlist(t *testing.T) {
	o := captureServeOpts(t,
		"--allow-namespace", " infra , shared ,,",
		"--allow-cluster-scoped-kind", "Node, Foo.example.com ,, Bar.multi.example.com ,Baz.")

	cfg, err := buildWebhookConfig(o, scope.ModeAudit, (*state.Provider)(nil), nil)
	if err != nil {
		t.Fatalf("buildWebhookConfig: %v", err)
	}

	wantNS := map[string]bool{"infra": true, "shared": true}
	if !reflect.DeepEqual(cfg.Allowlist.Namespaces, wantNS) {
		t.Errorf("Allowlist.Namespaces = %v, want %v (trimmed, empties dropped)", cfg.Allowlist.Namespaces, wantNS)
	}
	wantGK := map[closure.GVK]bool{
		{Kind: "Node"}:                            true, // bare Kind ⇒ CORE group
		{Group: "example.com", Kind: "Foo"}:       true,
		{Group: "multi.example.com", Kind: "Bar"}: true, // group keeps its dots
		{Kind: "Baz"}:                             true, // trailing dot qualifies core explicitly
	}
	if !reflect.DeepEqual(cfg.Allowlist.ClusterScopedGroupKinds, wantGK) {
		t.Errorf("Allowlist.ClusterScopedGroupKinds = %v, want %v", cfg.Allowlist.ClusterScopedGroupKinds, wantGK)
	}
	// The claim above, asserted directly: a bare Kind exempts the CORE group's kind only.
	if cfg.Allowlist.ClusterScopedGroupKinds[closure.GVK{Group: "other.example.com", Kind: "Node"}] {
		t.Error("a bare Kind must not exempt that Kind in another API group")
	}

	// No flags at all: the zero Allowlist, which exempts nothing.
	cfg, err = buildWebhookConfig(captureServeOpts(t), scope.ModeAudit, (*state.Provider)(nil), nil)
	if err != nil {
		t.Fatalf("buildWebhookConfig (no allowlist flags): %v", err)
	}
	if len(cfg.Allowlist.Namespaces) != 0 || len(cfg.Allowlist.ClusterScopedGroupKinds) != 0 {
		t.Errorf("without the flags the allowlist must exempt nothing, got %#v", cfg.Allowlist)
	}
}

// TestServeRejectsMalformedClusterScopedKind: a kind token with no Kind (".example.com")
// can never match a live ref, so accepting it would leave the operator believing they
// had exempted something. It is a usage error, refused BEFORE any cluster contact — the
// same fail-fast posture as a missing --tls-cert or an unknown --mode.
func TestServeRejectsMalformedClusterScopedKind(t *testing.T) {
	previous := serveWebhook
	t.Cleanup(func() { serveWebhook = previous })
	serveWebhook = func(serveOpts, scope.Mode, io.Writer, io.Writer) error {
		t.Error("serve must not start with a malformed --allow-cluster-scoped-kind")
		return nil
	}
	err := runServe([]string{"--tls-cert", "c", "--tls-key", "k", "--allow-cluster-scoped-kind", ".example.com"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("a kind token with an empty Kind must be a usage error")
	}
	if !strings.Contains(err.Error(), "allow-cluster-scoped-kind") {
		t.Errorf("error %q must name the offending flag", err)
	}
}

// stubContracts stands in for the live-GET contract seam: buildWebhookConfig must stay
// PURE (no cluster clients), so the getter is injected and the wiring test only has to
// prove the field arrives — not that a dynamic client works, which webhook's own
// contract-getter tests cover.
type stubContracts struct {
	u   *unstructured.Unstructured
	err error
}

func (s stubContracts) Get(context.Context, string, string) (*unstructured.Unstructured, error) {
	return s.u, s.err
}

// wiringScope is the clusterInfo a hermetic Server needs: the corpus kinds this file's
// fixture uses, resolved group-aware. It stands in for *state.Provider, which cannot be
// driven without a cluster.
type wiringScope struct{ objs []closure.Object }

func (wiringScope) Namespaced(closure.GVK) (bool, bool) { return true, true }

func (wiringScope) KindFor(string, string) (closure.GVK, bool) { return closure.GVK{}, false }

var wiringTracked = []closure.GVK{
	{Group: "apps", Version: "v1", Kind: "Deployment"},
	{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
	{Version: "v1", Kind: "Pod"},
	{Version: "v1", Kind: "Service"},
}

func (wiringScope) Tracked(gvk closure.GVK) bool {
	for _, k := range wiringTracked {
		if k.Group == gvk.Group && k.Kind == gvk.Kind {
			return true
		}
	}
	return false
}

func (wiringScope) GVKsForKind(kind string) []closure.GVK {
	var out []closure.GVK
	for _, k := range wiringTracked {
		if k.Kind == kind {
			out = append(out, k)
		}
	}
	return out
}

func (s wiringScope) GetByGVK(gvk closure.GVK, namespace, name string) (closure.Object, bool) {
	for _, o := range s.objs {
		if o.Ref.GVK.Group == gvk.Group && o.Ref.GVK.Kind == gvk.Kind &&
			o.Ref.Namespace == namespace && o.Ref.Name == name {
			return o, true
		}
	}
	return closure.Object{}, false
}

// wiringObjects: Deployment web owns the ReplicaSet subtree AND the Service svc, and svc
// selects the pod. Deleting the ReplicaSet closes over {rs, pod, svc}, which the derived
// scope rooted at the ReplicaSet does NOT cover — so the Service escapes unless a
// governing annotation re-roots at (or a contract authorises) the wider tree. That gap is
// what makes the provenance observable end-to-end.
func wiringObjects() []closure.Object {
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

// wiringReview is a DELETE of ReplicaSet prod/web-1 whose oldObject carries one
// annotation — the request shape the scope channels are read from.
func wiringReview(uid, key, value string) admissionv1.AdmissionReview {
	old := `{"apiVersion":"apps/v1","kind":"ReplicaSet","metadata":{"name":"web-1","namespace":"prod",` +
		`"uid":"uid-rs","resourceVersion":"7","annotations":{"krsm.io/task":"t-1","` + key + `":"` + value + `"}}}`
	return admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:       types.UID(uid),
			Operation: admissionv1.Delete,
			Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
			Namespace: "prod",
			Name:      "web-1",
			OldObject: runtime.RawExtension{Raw: []byte(old)},
		},
	}
}

// wiringContractCR is a TaskContract as an API SERVER returns it — server-owned metadata
// and a status subresource included. Anything leaner would let this test pass against an
// object no cluster produces.
func wiringContractCR(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	body := `{"apiVersion":"krsm.io/v1alpha1","kind":"TaskContract",
	  "metadata":{"name":"task-1","namespace":"prod","uid":"9f1c","resourceVersion":"4711",
	    "generation":1,"creationTimestamp":"2026-08-01T10:00:00Z"},
	  "status":{},
	  "spec":{"allow":[{"dim":"namespace","namespace":"prod"}]}}`
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON([]byte(body)); err != nil {
		t.Fatalf("wiringContractCR: %v", err)
	}
	return u
}

// newWiringServer builds a real webhook.Server from the REAL flag→Config assembly, with
// only the cluster-backed collaborators (State, ScopeInfo, Synced, Fresh) swapped for
// hermetic stand-ins — a nil *state.Provider cannot answer a request. Everything on the
// scope-resolution path, the annotation keys included, comes from buildWebhookConfig
// exactly as production gets it.
func newWiringServer(t *testing.T, o serveOpts, contracts contractGetter) *webhook.Server {
	t.Helper()
	cfg, err := buildWebhookConfig(o, scope.ModeAudit, (*state.Provider)(nil), contracts)
	if err != nil {
		t.Fatalf("buildWebhookConfig: %v", err)
	}
	objs := wiringObjects()
	cfg.State = closure.NewScanState(objs)
	cfg.ScopeInfo = wiringScope{objs: objs}
	cfg.Synced = func() bool { return true }
	cfg.Fresh = webhook.NoFreshness
	cfg.Logf = func(string, ...any) {}

	srv, err := webhook.New(cfg)
	if err != nil {
		t.Fatalf("webhook.New: %v", err)
	}
	return srv
}

// TestServeAnnotationFlagsReachScopeResolution (entry 31): --target-annotation and
// --scope-annotation are not decorative Config fields — they change which annotation a
// live admission request is GOVERNED by. This drives the whole path an operator does:
// flag string → serveOpts → buildWebhookConfig → webhook.New → Handle → resolveScope,
// and reads the answer off the response's structured scope-provenance, which is the
// machine-readable record of which level decided.
//
// The pairing is the point. A configured key must govern AND the default key must go
// inert; asserting only the first would pass on an implementation that simply honoured
// both keys, which is a wider attack surface than the operator configured. The inert
// direction is safe — an unrecognised key falls to the L0 tree of the request's own
// target, strictly narrower than the re-root or contract it named — so the failure mode
// of a mis-typed flag is over-restriction, never a widened scope.
func TestServeAnnotationFlagsReachScopeResolution(t *testing.T) {
	const provenanceKey = "krsm.io/scope-provenance"
	o := captureServeOpts(t,
		"--target-annotation", "example.com/re-root",
		"--scope-annotation", "example.com/contract")
	srv := newWiringServer(t, o, stubContracts{u: wiringContractCR(t)})

	for name, tc := range map[string]struct{ key, value, want string }{
		"configured target key re-roots":   {"example.com/re-root", "Deployment/prod/web", "annotation"},
		"default target key is inert":      {webhook.DefaultTargetAnnotation, "Deployment/prod/web", "derived:ownership-tree"},
		"configured scope key resolves L3": {"example.com/contract", "prod/task-1", "contract"},
		"default scope key is inert":       {webhook.DefaultScopeAnnotation, "prod/task-1", "derived:ownership-tree"},
	} {
		out := srv.Handle(context.Background(), wiringReview("req-"+name, tc.key, tc.value))
		if out.Response == nil {
			t.Fatalf("%s: no response", name)
		}
		if got := out.Response.AuditAnnotations[provenanceKey]; got != tc.want {
			t.Errorf("%s: scope provenance = %q, want %q", name, got, tc.want)
		}
	}

	// Left at their defaults, the flags reproduce the documented krsm.io/* behaviour —
	// so the override path cannot be "working" only because nothing else ever matched.
	def := newWiringServer(t, captureServeOpts(t), stubContracts{u: wiringContractCR(t)})
	out := def.Handle(context.Background(), wiringReview("req-default", webhook.DefaultTargetAnnotation, "Deployment/prod/web"))
	if got := out.Response.AuditAnnotations[provenanceKey]; got != "annotation" {
		t.Errorf("with no flags the default key must govern: provenance = %q, want %q", got, "annotation")
	}
}
