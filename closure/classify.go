package closure

// Kind groupings the relations key off of. Kept as Kind-only sets for v0.1; the
// fixtures and corpus identify these by Kind.
var (
	workloadKinds = map[string]bool{
		"Deployment": true, "ReplicaSet": true, "StatefulSet": true, "DaemonSet": true,
	}
	selectorKinds = map[string]bool{
		"Service": true, "PodDisruptionBudget": true, "NetworkPolicy": true,
	}
	mountableKinds = map[string]bool{
		"ConfigMap": true, "Secret": true, "PersistentVolumeClaim": true,
	}
)

// EffectClass is one way an action propagates effects. A single action can be
// several classes at once (deleting a Namespace is both CascadeDelete and
// Containment), so Classify returns a set.
type EffectClass int

const (
	CascadeDelete EffectClass = iota
	Containment
	Disruptive
	MutateSelector // the target's own selector changed → pods it binds (old∪new)
	MutateLabels   // the target's labels changed → selector-owners binding it (old∪new)
	MutateConfig
	ScaleEffect
	FinalizerRemoval
)

type effectSet map[EffectClass]bool

func (e effectSet) has(c EffectClass) bool { return e[c] }

// classify determines which relations an action licenses the closure to follow.
func classify(a Action) effectSet {
	s := effectSet{}
	kind := a.Target.GVK.Kind

	switch a.Verb {
	case Delete:
		if a.Cascade {
			s[CascadeDelete] = true
		}
		if kind == "Namespace" {
			s[Containment] = true
		}
		if workloadKinds[kind] {
			s[Disruptive] = true
		}
		// Deleting a ConfigMap/Secret/PVC affects every workload that consumes
		// it (broken mount/env on restart; data loss for a bound PVC), exactly
		// as a mutation does. The reference oracle treats delete as mutating;
		// omitting it here is a false negative for a safety gate.
		if mountableKinds[kind] {
			s[MutateConfig] = true
		}
	case Scale:
		if workloadKinds[kind] {
			s[Disruptive] = true
			s[ScaleEffect] = true
		}
	case Restart:
		if workloadKinds[kind] {
			s[Disruptive] = true
		}
	case Update, Patch:
		if mountableKinds[kind] {
			s[MutateConfig] = true
		}
		if selectorChanged(a) {
			s[MutateSelector] = true
		}
		if labelsChanged(a) {
			s[MutateLabels] = true
		}
		if finalizersRemoved(a) {
			s[FinalizerRemoval] = true
		}
	}
	return s
}

// selectorChanged reports whether a mutation alters the object's selector.
func selectorChanged(a Action) bool {
	if a.Old == nil || a.New == nil {
		return false
	}
	return !sameLabels(a.Old.Selector, a.New.Selector)
}

// labelsChanged reports whether a mutation alters the object's labels (which can
// move it in or out of other selectors).
func labelsChanged(a Action) bool {
	if a.Old == nil || a.New == nil {
		return false
	}
	return !sameLabels(a.Old.Labels, a.New.Labels)
}

// finalizersRemoved reports whether the mutation drops one or more finalizers.
func finalizersRemoved(a Action) bool {
	if a.Old == nil || a.New == nil {
		return false
	}
	kept := map[string]bool{}
	for _, f := range a.New.Finalizers {
		kept[f] = true
	}
	for _, f := range a.Old.Finalizers {
		if !kept[f] {
			return true
		}
	}
	return false
}

func sameLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
