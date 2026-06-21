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
