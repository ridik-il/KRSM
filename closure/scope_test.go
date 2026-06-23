package closure

import (
	"sort"
	"testing"
)

// scopeDecision runs Safe over a one-pod state with the given pod labels and a
// single scope clause, and reports whether the pod escaped scope. The action is a
// no-cascade restart of the pod itself, so the closure is exactly that pod and the
// only thing matchScope decides is whether the clause covers it.
//
// It observes selector/scope behaviour through the public Safe API (Decision), not
// the unexported matchScope.
func podRef(name string, labels map[string]string) Object {
	return Object{
		Ref:    Ref{GVK: GVK{Version: "v1", Kind: "Pod"}, Namespace: "prod", Name: name, UID: "uid:Pod/prod/" + name},
		Labels: labels,
	}
}

// escapedNames returns the sorted Name of every ref in Escaping.
func escapedNames(d Decision) []string {
	out := make([]string, 0, len(d.Escaping))
	for _, r := range d.Escaping {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

// TestResourceDimExactName: a resource-dim clause naming a member exactly admits
// it (not in Escaping) — the v0.1 identity behaviour, preserved through the rename.
func TestResourceDimExactName(t *testing.T) {
	objs := []Object{podRef("web-1", map[string]string{"app": "web"})}
	s := NewScanState(objs)
	a := Action{Verb: Restart, Target: objs[0].Ref, Cascade: false}
	scope := []ScopeClause{{Dim: DimResource, GVK: GVK{Kind: "Pod"}, Namespace: "prod", Name: "web-1"}}

	d := Safe(s, a, scope)
	if d.Verdict != Allow {
		t.Fatalf("verdict = %s, want Allow; escaping=%v", d.Verdict, escapedNames(d))
	}
}

// TestResourceDimGlobAndEmptyDim: a resource-dim clause admits by path.Match glob,
// and a clause with an empty Dim is read as resource (back-compat for dim-less
// scopes) — both observed as an Allow with nothing escaping.
func TestResourceDimGlobAndEmptyDim(t *testing.T) {
	tests := []struct {
		name  string
		scope []ScopeClause
	}{
		{
			name:  "glob",
			scope: []ScopeClause{{Dim: DimResource, GVK: GVK{Kind: "Pod"}, Namespace: "prod", Name: "web-*"}},
		},
		{
			name:  "empty dim treated as resource",
			scope: []ScopeClause{{GVK: GVK{Kind: "Pod"}, Namespace: "prod", Name: "web-1"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []Object{podRef("web-1", map[string]string{"app": "web"})}
			s := NewScanState(objs)
			a := Action{Verb: Restart, Target: objs[0].Ref, Cascade: false}

			d := Safe(s, a, tt.scope)
			if d.Verdict != Allow {
				t.Fatalf("verdict = %s, want Allow; escaping=%v", d.Verdict, escapedNames(d))
			}
		})
	}
}

// TestScopeClauseValidate checks structural consistency only: Dim must be one of
// {"", DimResource, DimSelector}; a resource clause must not carry a Selector; a
// selector clause must not carry a Name. An empty selector is deliberately valid
// (match-nothing is a fail-safe, not a malformed clause).
func TestScopeClauseValidate(t *testing.T) {
	tests := []struct {
		name    string
		clause  ScopeClause
		wantErr bool
	}{
		{"valid resource", ScopeClause{Dim: DimResource, GVK: GVK{Kind: "Pod"}, Namespace: "prod", Name: "web-1"}, false},
		{"valid empty dim", ScopeClause{GVK: GVK{Kind: "Pod"}, Namespace: "prod", Name: "web-1"}, false},
		{"valid selector", ScopeClause{Dim: DimSelector, GVK: GVK{Kind: "Pod"}, Namespace: "prod", Selector: LabelSelector{MatchLabels: map[string]string{"app": "web"}}}, false},
		{"valid selector empty (match-nothing)", ScopeClause{Dim: DimSelector, GVK: GVK{Kind: "Pod"}, Namespace: "prod"}, false},
		{"unknown dim", ScopeClause{Dim: "reference", GVK: GVK{Kind: "Pod"}, Namespace: "prod", Name: "web-1"}, true},
		{"resource with selector", ScopeClause{Dim: DimResource, GVK: GVK{Kind: "Pod"}, Namespace: "prod", Name: "web-1", Selector: LabelSelector{MatchLabels: map[string]string{"app": "web"}}}, true},
		{"selector with name", ScopeClause{Dim: DimSelector, GVK: GVK{Kind: "Pod"}, Namespace: "prod", Name: "web-1", Selector: LabelSelector{MatchLabels: map[string]string{"app": "web"}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.clause.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

// TestConstructorsHonoredBySafe: the safe constructors build clauses Safe respects
// — a SelectorClause admits a matching member, a ResourceClause admits by name —
// observed through Decision (no unexported access).
func TestConstructorsHonoredBySafe(t *testing.T) {
	t.Run("ResourceClause admits by name", func(t *testing.T) {
		pod := podRef("web-1", map[string]string{"app": "web"})
		scope := []ScopeClause{ResourceClause(GVK{Kind: "Pod"}, "prod", "web-1")}
		if !admits(t, pod, scope) {
			t.Error("ResourceClause(Pod/prod/web-1) did not admit web-1")
		}
	})
	t.Run("SelectorClause admits a matching member", func(t *testing.T) {
		pod := podRef("web-1", map[string]string{"app": "web"})
		scope := []ScopeClause{SelectorClause(GVK{Kind: "Pod"}, "prod", LabelSelector{MatchLabels: map[string]string{"app": "web"}})}
		if !admits(t, pod, scope) {
			t.Error("SelectorClause(Pod/prod, app=web) did not admit a matching pod")
		}
	})
}

// TestUnknownDimMatchesNothing: a clause with an unrecognised Dim (a typo like
// "Selector", or a not-yet-implemented dimension like "reference") must cover
// NOTHING — even when, read as a resource clause, its Name would match the member
// exactly. For a fail-closed safety control an unknown dimension must never be
// silently coerced into a resource grant; the member escapes (Block).
func TestUnknownDimMatchesNothing(t *testing.T) {
	objs := []Object{podRef("web-1", map[string]string{"app": "web"})}
	s := NewScanState(objs)
	a := Action{Verb: Restart, Target: objs[0].Ref, Cascade: false}

	// As a resource clause this Name ("web-1") would match the only closure member;
	// but the unknown dim must mean the clause grants nothing.
	scope := []ScopeClause{{Dim: "reference", GVK: GVK{Kind: "Pod"}, Namespace: "prod", Name: "web-1"}}

	d := Safe(s, a, scope)
	if d.Verdict != Block || len(d.Escaping) != 1 {
		t.Fatalf("verdict = %s escaping = %v, want Block with the member escaping (unknown dim grants nothing)", d.Verdict, escapedNames(d))
	}
}

// selectorScope is a single DimSelector clause gating Pods in prod by sel.
func selectorScope(sel LabelSelector) []ScopeClause {
	return []ScopeClause{{Dim: DimSelector, GVK: GVK{Kind: "Pod"}, Namespace: "prod", Selector: sel}}
}

// admits runs a no-cascade restart of the single given pod under scope and reports
// whether the pod stayed in scope (Allow, nothing escaping).
func admits(t *testing.T, pod Object, scope []ScopeClause) bool {
	t.Helper()
	s := NewScanState([]Object{pod})
	a := Action{Verb: Restart, Target: pod.Ref, Cascade: false}
	d := Safe(s, a, scope)
	return d.Verdict == Allow && len(d.Escaping) == 0
}

// TestSelectorDimMatchLabels: a selector clause with matchLabels admits a member
// whose labels match and lets a same-kind/same-namespace non-matching member escape.
func TestSelectorDimMatchLabels(t *testing.T) {
	scope := selectorScope(LabelSelector{MatchLabels: map[string]string{"app": "web"}})

	if !admits(t, podRef("web-1", map[string]string{"app": "web"}), scope) {
		t.Error("matching pod (app=web) escaped; want admitted")
	}
	if admits(t, podRef("db-1", map[string]string{"app": "db"}), scope) {
		t.Error("non-matching pod (app=db) admitted; want escaped")
	}
}

// TestSelectorDimMatchExpressions: a selector clause with matchExpressions admits
// or escapes correctly across the four operators, including the absence-sensitive
// cases (key missing) for NotIn and DoesNotExist.
func TestSelectorDimMatchExpressions(t *testing.T) {
	tests := []struct {
		name   string
		req    SelectorRequirement
		labels map[string]string
		admit  bool
	}{
		{"In hit", SelectorRequirement{Key: "app", Operator: OpIn, Values: []string{"web"}}, map[string]string{"app": "web"}, true},
		{"In miss", SelectorRequirement{Key: "app", Operator: OpIn, Values: []string{"web"}}, map[string]string{"app": "db"}, false},
		{"In absent", SelectorRequirement{Key: "app", Operator: OpIn, Values: []string{"web"}}, map[string]string{"tier": "x"}, false},
		{"NotIn hit", SelectorRequirement{Key: "app", Operator: OpNotIn, Values: []string{"web"}}, map[string]string{"app": "db"}, true},
		{"NotIn miss", SelectorRequirement{Key: "app", Operator: OpNotIn, Values: []string{"web"}}, map[string]string{"app": "web"}, false},
		{"NotIn absent admits", SelectorRequirement{Key: "app", Operator: OpNotIn, Values: []string{"web"}}, map[string]string{"tier": "x"}, true},
		{"Exists hit", SelectorRequirement{Key: "app", Operator: OpExists}, map[string]string{"app": "web"}, true},
		{"Exists absent escapes", SelectorRequirement{Key: "app", Operator: OpExists}, map[string]string{"tier": "x"}, false},
		{"DoesNotExist absent admits", SelectorRequirement{Key: "app", Operator: OpDoesNotExist}, map[string]string{"tier": "x"}, true},
		{"DoesNotExist present escapes", SelectorRequirement{Key: "app", Operator: OpDoesNotExist}, map[string]string{"app": "web"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope := selectorScope(LabelSelector{MatchExpressions: []SelectorRequirement{tt.req}})
			if got := admits(t, podRef("p", tt.labels), scope); got != tt.admit {
				t.Errorf("admits = %v, want %v", got, tt.admit)
			}
		})
	}
}

// TestSelectorDimGating: a selector clause does not match a candidate of a
// different GVK.Kind or namespace, even when its labels satisfy the selector — the
// gate is preserved exactly as for resource clauses.
func TestSelectorDimGating(t *testing.T) {
	sel := LabelSelector{MatchLabels: map[string]string{"app": "web"}}

	t.Run("wrong kind escapes", func(t *testing.T) {
		// A Service with app=web: the Pod selector clause must not cover it.
		svc := Object{Ref: Ref{GVK: GVK{Version: "v1", Kind: "Service"}, Namespace: "prod", Name: "web", UID: "uid:Service/prod/web"}, Labels: map[string]string{"app": "web"}}
		if admits(t, svc, selectorScope(sel)) {
			t.Error("Service admitted by a Pod selector clause; gate not applied")
		}
	})

	t.Run("wrong namespace escapes", func(t *testing.T) {
		// A Pod with app=web in staging: the prod-gated clause must not cover it.
		pod := Object{Ref: Ref{GVK: GVK{Version: "v1", Kind: "Pod"}, Namespace: "staging", Name: "web-1", UID: "uid:Pod/staging/web-1"}, Labels: map[string]string{"app": "web"}}
		scope := []ScopeClause{{Dim: DimSelector, GVK: GVK{Kind: "Pod"}, Namespace: "prod", Selector: sel}}
		if admits(t, pod, scope) {
			t.Error("staging Pod admitted by a prod-gated clause; namespace gate not applied")
		}
	})
}

// TestSelectorDimEmptyMatchesNothing: an empty/nil selector clause matches nothing,
// so its in-gate candidate escapes. This is the fail-safe rule — an empty scope
// selector must never silently authorise the whole namespace.
func TestSelectorDimEmptyMatchesNothing(t *testing.T) {
	// Both the nil selector and the present-but-empty `{}` must match nothing.
	for _, sel := range []LabelSelector{
		{},
		{MatchLabels: map[string]string{}},
	} {
		pod := podRef("web-1", map[string]string{"app": "web"})
		if admits(t, pod, selectorScope(sel)) {
			t.Errorf("empty selector %+v admitted a candidate; want match-nothing (escape)", sel)
		}
	}
}

// TestNamespaceDimCoversNamespaceMember: a namespace-dim clause for prod admits a
// prod member (Allow, nothing escaping) — the gate alone is the membership test, no
// name or selector required.
func TestNamespaceDimCoversNamespaceMember(t *testing.T) {
	pod := podRef("web-1", map[string]string{"app": "web"})
	scope := []ScopeClause{{Dim: DimNamespace, Namespace: "prod"}}
	if !admits(t, pod, scope) {
		t.Error("namespace clause (prod) did not admit a prod member")
	}
}

// TestNamespaceDimWrongNamespaceEscapes: the same prod clause does not cover a
// staging member — the namespace gate lets it escape (Block).
func TestNamespaceDimWrongNamespaceEscapes(t *testing.T) {
	staging := Object{Ref: Ref{GVK: GVK{Version: "v1", Kind: "Pod"}, Namespace: "staging", Name: "web-1", UID: "uid:Pod/staging/web-1"}, Labels: map[string]string{"app": "web"}}
	scope := []ScopeClause{{Dim: DimNamespace, Namespace: "prod"}}
	if admits(t, staging, scope) {
		t.Error("namespace clause (prod) admitted a staging member; namespace gate not applied")
	}
}

// TestNamespaceDimGVKGateExcludesWrongKind: a namespace clause carrying an optional
// GVK gate (Pod) does not cover a Service in the same namespace — the Kind gate is
// applied exactly as for the other dimensions.
func TestNamespaceDimGVKGateExcludesWrongKind(t *testing.T) {
	svc := Object{Ref: Ref{GVK: GVK{Version: "v1", Kind: "Service"}, Namespace: "prod", Name: "web", UID: "uid:Service/prod/web"}, Labels: map[string]string{"app": "web"}}
	scope := []ScopeClause{{Dim: DimNamespace, GVK: GVK{Kind: "Pod"}, Namespace: "prod"}}
	if admits(t, svc, scope) {
		t.Error("Pod-gated namespace clause admitted a Service; GVK gate not applied")
	}
}

// TestNamespaceClauseValidate checks structural consistency of a namespace clause:
// the Namespace must be non-empty, and a Name or a Selector is a hard error (either
// would be silently ignored). A well-formed clause (GVK optional) is accepted.
func TestNamespaceClauseValidate(t *testing.T) {
	tests := []struct {
		name    string
		clause  ScopeClause
		wantErr bool
	}{
		{"valid namespace", ScopeClause{Dim: DimNamespace, Namespace: "prod"}, false},
		{"valid namespace with gvk gate", ScopeClause{Dim: DimNamespace, GVK: GVK{Kind: "Pod"}, Namespace: "prod"}, false},
		{"empty namespace rejected", ScopeClause{Dim: DimNamespace}, true},
		{"namespace with name rejected", ScopeClause{Dim: DimNamespace, Namespace: "prod", Name: "web-1"}, true},
		{"namespace with selector rejected", ScopeClause{Dim: DimNamespace, Namespace: "prod", Selector: LabelSelector{MatchLabels: map[string]string{"app": "web"}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.clause.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

// --- ownership dimension ----------------------------------------------------

// TestOwnershipDimCoversSubtree: an ownership clause rooted at a Deployment covers
// the Deployment, its direct child (ReplicaSet) and a grandchild (Pod), via a
// cascade delete. A sibling tree's resources and a non-owned same-namespace Service
// (the scenario-01 property: the Service selects the pods, so it is in the closure,
// but the Deployment does not own it) both escape.
func TestOwnershipDimCoversSubtree(t *testing.T) {
	root := Ref{GVK: GVK{Kind: "Deployment"}, Namespace: "prod", Name: "web", UID: "uid:Deployment/prod/web"}
	objs := []Object{
		{Ref: root},
		// ReplicaSet owned by the Deployment; Pod owned by the ReplicaSet (grandchild).
		{Ref: Ref{GVK: GVK{Kind: "ReplicaSet"}, Namespace: "prod", Name: "web-rs", UID: "uid:ReplicaSet/prod/web-rs"},
			Owners: []OwnerRef{{Kind: "Deployment", Name: "web", UID: "uid:Deployment/prod/web"}}},
		{Ref: Ref{GVK: GVK{Kind: "Pod"}, Namespace: "prod", Name: "web-1", UID: "uid:Pod/prod/web-1"},
			Labels: map[string]string{"app": "web"},
			Owners: []OwnerRef{{Kind: "ReplicaSet", Name: "web-rs", UID: "uid:ReplicaSet/prod/web-rs"}}},
		// A Service selecting the pod: in the closure (pod→selector binding) but NOT
		// owned by the Deployment, so it must escape the ownership tree.
		{Ref: Ref{GVK: GVK{Version: "v1", Kind: "Service"}, Namespace: "prod", Name: "web-svc", UID: "uid:Service/prod/web-svc"},
			Selector: LabelSelector{MatchLabels: map[string]string{"app": "web"}}},
		// An unrelated sibling tree, never reached by this action's closure.
		{Ref: Ref{GVK: GVK{Kind: "ReplicaSet"}, Namespace: "prod", Name: "db-rs", UID: "uid:ReplicaSet/prod/db-rs"}},
	}
	s := NewScanState(objs)
	a := Action{Verb: Delete, Target: root, Cascade: true}
	scope := []ScopeClause{OwnershipClause(root)}

	d := Safe(s, a, scope)
	got := escapedNames(d)
	want := []string{"web-svc"} // only the non-owned Service escapes
	if d.Verdict != Block || len(got) != 1 || got[0] != want[0] {
		t.Fatalf("verdict = %s escaping = %v, want Block with only the non-owned Service (web-svc) escaping", d.Verdict, got)
	}
}

// TestOwnershipDimCyclicOwnersTerminate: when owners form a cycle (A owns B, B owns
// A), ownedSubtree's visited-set guard terminates and every cycle member is in the
// subtree, so a cascade delete rooted at A escapes nothing.
func TestOwnershipDimCyclicOwnersTerminate(t *testing.T) {
	a := Ref{GVK: GVK{Kind: "Widget"}, Namespace: "prod", Name: "a", UID: "uid:Widget/prod/a"}
	b := Ref{GVK: GVK{Kind: "Widget"}, Namespace: "prod", Name: "b", UID: "uid:Widget/prod/b"}
	objs := []Object{
		{Ref: a, Owners: []OwnerRef{{Kind: "Widget", Name: "b", UID: b.UID}}},
		{Ref: b, Owners: []OwnerRef{{Kind: "Widget", Name: "a", UID: a.UID}}},
	}
	s := NewScanState(objs)
	act := Action{Verb: Delete, Target: a, Cascade: true}
	scope := []ScopeClause{OwnershipClause(a)}

	d := Safe(s, act, scope)
	if d.Verdict != Allow || len(d.Escaping) != 0 {
		t.Fatalf("verdict = %s escaping = %v, want Allow with the whole cycle in subtree", d.Verdict, escapedNames(d))
	}
}

// TestOwnershipClauseValidate checks structural consistency of an ownership clause:
// Root must carry a Kind and a Name; a clause-level Name/Selector/GVK/Namespace is a
// hard error (identity lives on Root). A well-formed clause is accepted.
func TestOwnershipClauseValidate(t *testing.T) {
	root := Ref{GVK: GVK{Kind: "Deployment"}, Namespace: "prod", Name: "web"}
	tests := []struct {
		name    string
		clause  ScopeClause
		wantErr bool
	}{
		{"valid ownership", OwnershipClause(root), false},
		{"empty root rejected", ScopeClause{Dim: DimOwnership}, true},
		{"root without name rejected", ScopeClause{Dim: DimOwnership, Root: Ref{GVK: GVK{Kind: "Deployment"}}}, true},
		{"root without kind rejected", ScopeClause{Dim: DimOwnership, Root: Ref{Name: "web"}}, true},
		{"clause-level name rejected", ScopeClause{Dim: DimOwnership, Root: root, Name: "x"}, true},
		{"clause-level selector rejected", ScopeClause{Dim: DimOwnership, Root: root, Selector: LabelSelector{MatchLabels: map[string]string{"app": "web"}}}, true},
		{"clause-level gvk rejected", ScopeClause{Dim: DimOwnership, Root: root, GVK: GVK{Kind: "Pod"}}, true},
		{"clause-level namespace rejected", ScopeClause{Dim: DimOwnership, Root: root, Namespace: "prod"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.clause.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

// untrackedState is a State whose closure contains a member that Get cannot
// resolve. The target IS resolvable (so the closure seeds and Safe proceeds), and
// a cascade delete pulls in a *child* ref via OwnedChildren — but Get returns
// ok=false for that child, modelling a provider that yields a closure member whose
// labels are unresolvable. This drives the fail-closed selector path (DESIGN §5)
// through the public Safe API without a concrete scanState.
type untrackedState struct {
	target Ref
	child  Ref // returned by OwnedChildren but NOT resolvable via Get
}

func (u untrackedState) Get(r Ref) (*Object, bool) {
	if r.key() == u.target.key() {
		return &Object{Ref: u.target, Labels: map[string]string{"app": "web"}}, true
	}
	return nil, false // the child is unresolvable
}
func (u untrackedState) OwnedChildren(r Ref) []Ref {
	if r.key() == u.target.key() {
		return []Ref{u.child}
	}
	return nil
}
func (u untrackedState) NamespaceContents(string) []Ref                          { return nil }
func (u untrackedState) PodsSelectedBy(Ref) []Ref                                { return nil }
func (u untrackedState) PodsMatching(string, LabelSelector, string) []Ref        { return nil }
func (u untrackedState) SelectorsTargeting(Ref) []Ref                            { return nil }
func (u untrackedState) SelectorsMatchingLabels(string, map[string]string) []Ref { return nil }
func (u untrackedState) Consumers(Ref) []Ref                                     { return nil }
func (u untrackedState) ControllersTargeting(Ref) []Ref                          { return nil }

// TestSelectorDimUntrackedCandidateEscapes: a selector clause does not match a
// closure member whose labels cannot be resolved from State — it escapes
// (fail-closed, DESIGN §5; never grant scope to a resource we cannot fully see).
func TestSelectorDimUntrackedCandidateEscapes(t *testing.T) {
	target := Ref{GVK: GVK{Version: "v1", Kind: "Pod"}, Namespace: "prod", Name: "owner", UID: "uid:Pod/prod/owner"}
	child := Ref{GVK: GVK{Version: "v1", Kind: "Pod"}, Namespace: "prod", Name: "ghost", UID: "uid:Pod/prod/ghost"}
	s := untrackedState{target: target, child: child}
	a := Action{Verb: Delete, Target: target, Cascade: true} // cascade → walk OwnedChildren

	// A selector that the child's (unknown) labels can never satisfy because they
	// are unresolvable; the in-scope target is covered, so only the ghost escapes.
	scope := []ScopeClause{
		{Dim: DimResource, GVK: GVK{Kind: "Pod"}, Namespace: "prod", Name: "owner"},
		{Dim: DimSelector, GVK: GVK{Kind: "Pod"}, Namespace: "prod", Selector: LabelSelector{MatchLabels: map[string]string{"app": "web"}}},
	}

	d := Safe(s, a, scope)
	got := escapedNames(d)
	if d.Verdict != Block || len(got) != 1 || got[0] != "ghost" {
		t.Fatalf("verdict = %s escaping = %v, want Block with only the unresolvable child (ghost) escaping", d.Verdict, got)
	}
}
