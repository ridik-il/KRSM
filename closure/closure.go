package closure

import "sort"

// Closure computes the affected-resource closure C(S,A): the set of in-cluster
// resources the action actually affects, found by a breadth-first walk from the
// action's target following only the relations the action's effect class
// licenses. The target itself is excluded from the result (it is the action's
// subject, not collateral).
//
// The walk is finite: a visited-set guard expands each resource at most once, so
// |C| ≤ |R| and it terminates even on cyclic ownerReferences (DESIGN §4).
// Cross-boundary effects (finalizer→external) are reported by ExternalEffects,
// not here.
func Closure(s State, a Action) []Ref {
	classes := classify(a)

	affected := map[string]Ref{}
	visited := map[string]bool{}
	queue := []Ref{a.Target}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur.key()] {
			continue
		}
		visited[cur.key()] = true
		affected[cur.key()] = cur

		// 1. ownerReference cascade on a cascading delete.
		if classes.has(CascadeDelete) {
			queue = append(queue, s.OwnedChildren(cur)...)
		}

		// 2. namespace containment: deleting a Namespace affects all it holds.
		//    Only expanded from the namespace the action targets.
		if classes.has(Containment) && cur.GVK.Kind == "Namespace" {
			queue = append(queue, s.NamespaceContents(cur.Name)...)
		}

		// 3. a disruptive verb on a workload disrupts the pods it selects.
		if classes.has(Disruptive) && workloadKinds[cur.GVK.Kind] {
			queue = append(queue, s.PodsSelectedBy(cur)...)
		}

		// 4. a scale action also pulls in the controllers referencing the
		//    target (HPA scaleTargetRef).
		if classes.has(ScaleEffect) && workloadKinds[cur.GVK.Kind] {
			queue = append(queue, s.ControllersTargeting(cur)...)
		}

		// 5. mutating a config/storage object affects its consumers.
		if classes.has(MutateConfig) && mountableKinds[cur.GVK.Kind] {
			queue = append(queue, s.Consumers(cur)...)
		}

		// 6. a selector/label mutation unions the old and new match-sets.
		if classes.has(MutateSelector) && cur.key() == a.Target.key() {
			queue = append(queue, selectorMutationAffected(s, a)...)
		}

		// 7. any binding object (Service/PDB/NetworkPolicy) selecting an
		//    affected pod is itself affected. We record the binding but do not
		//    recurse into the binding's *other* pods.
		if cur.GVK.Kind == "Pod" {
			for _, sel := range s.SelectorsTargeting(cur) {
				affected[sel.key()] = sel
			}
		}
	}

	delete(affected, a.Target.key())
	return sortedRefs(affected)
}

// selectorMutationAffected returns the union of pods matched by the old and new
// selectors of a mutated workload/Service, plus the bindings on those pods.
func selectorMutationAffected(s State, a Action) []Ref {
	out := map[string]Ref{}
	add := func(sel map[string]string) {
		if sel == nil {
			return
		}
		probe := Object{Ref: a.Target, Selector: sel}
		tmp := NewScanState(append(snapshot(s, a.Target), probe))
		for _, p := range tmp.PodsSelectedBy(a.Target) {
			out[p.key()] = p
		}
	}
	if a.Old != nil {
		add(a.Old.Selector)
	}
	if a.New != nil {
		add(a.New.Selector)
	}
	res := make([]Ref, 0, len(out))
	for _, r := range out {
		res = append(res, r)
	}
	return res
}

// snapshot returns all objects in the state except the target, so a probe object
// carrying the candidate selector can stand in for it.
func snapshot(s State, target Ref) []Object {
	ss, ok := s.(*scanState)
	if !ok {
		return nil
	}
	out := make([]Object, 0, len(ss.objs))
	for i := range ss.objs {
		if ss.objs[i].Ref.key() == target.key() {
			continue
		}
		out = append(out, ss.objs[i])
	}
	return out
}

// ExternalEffects returns cross-boundary effects an action triggers that cannot
// be confirmed from in-cluster state — currently, the external resources a
// removed finalizer was guarding (corpus #9). These drive a WARN, not a BLOCK.
func ExternalEffects(s State, a Action) []Ref {
	if _, ok := s.Get(a.Target); !ok {
		return nil
	}
	classes := classify(a)
	if !classes.has(FinalizerRemoval) {
		return nil
	}
	var out []Ref
	for _, f := range removedFinalizers(a) {
		out = append(out, Ref{
			GVK:       GVK{Kind: "External"},
			Namespace: a.Target.Namespace,
			Name:      f,
		})
	}
	return out
}

func removedFinalizers(a Action) []string {
	if a.Old == nil {
		return nil
	}
	kept := map[string]bool{}
	if a.New != nil {
		for _, f := range a.New.Finalizers {
			kept[f] = true
		}
	}
	var out []string
	for _, f := range a.Old.Finalizers {
		if !kept[f] {
			out = append(out, f)
		}
	}
	return out
}

func sortedRefs(m map[string]Ref) []Ref {
	out := make([]Ref, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].human() < out[j].human() })
	return out
}
