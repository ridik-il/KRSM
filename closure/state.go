package closure

// State is the G(S) seam: read-only access to the live relational graph. The
// v0.1 implementation (NewScanState) scans linearly; a later informer-backed
// indexed implementation satisfies the same interface without changing callers.
type State interface {
	// Get returns the live object for a ref, if tracked.
	Get(Ref) (*Object, bool)
	// OwnedChildren returns objects whose ownerReferences match r by uid.
	OwnedChildren(r Ref) []Ref
	// NamespaceContents returns every namespaced object in ns.
	NamespaceContents(ns string) []Ref
	// PodsSelectedBy returns pods matching the selector of r (same namespace).
	PodsSelectedBy(r Ref) []Ref
	// PodsMatching returns pods in ns bound by the given selector, with
	// ownerKind determining empty-selector semantics. It lets callers evaluate
	// a candidate selector (e.g. the old/new selector of a mutation) without
	// reconstructing state — keeping closure free of any concrete State type.
	PodsMatching(ns string, selector map[string]string, ownerKind string) []Ref
	// SelectorsTargeting returns Service/PDB/NetworkPolicy whose selector
	// matches the given pod.
	SelectorsTargeting(pod Ref) []Ref
	// Consumers returns objects referencing target via volume/env/envFrom.
	Consumers(target Ref) []Ref
	// ControllersTargeting returns controllers (e.g. HPA via scaleTargetRef)
	// referencing r.
	ControllersTargeting(r Ref) []Ref
}

// scanState is the v0.1 linear-scan State. Correct, not yet fast.
type scanState struct {
	objs    []Object
	byUID   map[string]*Object
	byHuman map[string]*Object
}

// NewScanState builds a State over an in-memory object set. Lookups are by uid
// (falling back to GVK/namespace/name), so same-name objects in different
// namespaces never collide.
func NewScanState(objs []Object) State {
	s := &scanState{
		objs:    make([]Object, len(objs)),
		byUID:   make(map[string]*Object, len(objs)),
		byHuman: make(map[string]*Object, len(objs)),
	}
	copy(s.objs, objs)
	for i := range s.objs {
		o := &s.objs[i]
		if o.Ref.UID != "" {
			s.byUID[o.Ref.UID] = o
		}
		s.byHuman[o.Ref.human()] = o
	}
	return s
}

func (s *scanState) lookup(r Ref) (*Object, bool) {
	if r.UID != "" {
		if o, ok := s.byUID[r.UID]; ok {
			return o, true
		}
	}
	o, ok := s.byHuman[r.human()]
	return o, ok
}

func (s *scanState) Get(r Ref) (*Object, bool) { return s.lookup(r) }

func (s *scanState) OwnedChildren(r Ref) []Ref {
	owner, ok := s.lookup(r)
	if !ok {
		return nil
	}
	var out []Ref
	for i := range s.objs {
		o := &s.objs[i]
		for _, ow := range o.Owners {
			if ow.UID != "" && ow.UID == owner.Ref.UID {
				out = append(out, o.Ref)
				break
			}
		}
	}
	return out
}

func (s *scanState) NamespaceContents(ns string) []Ref {
	var out []Ref
	for i := range s.objs {
		o := &s.objs[i]
		if o.Ref.Namespace == ns && o.Ref.GVK.Kind != "Namespace" {
			out = append(out, o.Ref)
		}
	}
	return out
}

func (s *scanState) PodsSelectedBy(r Ref) []Ref {
	owner, ok := s.lookup(r)
	if !ok {
		return nil
	}
	return s.PodsMatching(owner.Ref.Namespace, owner.Selector, owner.Ref.GVK.Kind)
}

func (s *scanState) PodsMatching(ns string, selector map[string]string, ownerKind string) []Ref {
	var out []Ref
	for i := range s.objs {
		o := &s.objs[i]
		if o.Ref.GVK.Kind == "Pod" && o.Ref.Namespace == ns && selectorBinds(ownerKind, selector, o.Labels) {
			out = append(out, o.Ref)
		}
	}
	return out
}

func (s *scanState) SelectorsTargeting(pod Ref) []Ref {
	p, ok := s.lookup(pod)
	if !ok {
		return nil
	}
	var out []Ref
	for i := range s.objs {
		o := &s.objs[i]
		if !selectorKinds[o.Ref.GVK.Kind] || o.Ref.Namespace != pod.Namespace {
			continue
		}
		if selectorBinds(o.Ref.GVK.Kind, o.Selector, p.Labels) {
			out = append(out, o.Ref)
		}
	}
	return out
}

func (s *scanState) Consumers(target Ref) []Ref {
	tgt, ok := s.lookup(target)
	if !ok {
		return nil
	}
	var out []Ref
	for i := range s.objs {
		o := &s.objs[i]
		for _, cr := range o.CrossRefs {
			if cr.Kind == RefScaleTarget {
				continue
			}
			if crossRefMatches(cr.Ref, tgt.Ref) {
				out = append(out, o.Ref)
				break
			}
		}
	}
	return out
}

func (s *scanState) ControllersTargeting(r Ref) []Ref {
	tgt, ok := s.lookup(r)
	if !ok {
		return nil
	}
	var out []Ref
	for i := range s.objs {
		o := &s.objs[i]
		for _, cr := range o.CrossRefs {
			if cr.Kind == RefScaleTarget && crossRefMatches(cr.Ref, tgt.Ref) {
				out = append(out, o.Ref)
				break
			}
		}
		// A PodDisruptionBudget guards the workload's pods; it is reported via
		// the pod→selector path, so it is not duplicated here.
	}
	return out
}

// selectorBinds reports whether a selector owned by ownerKind binds a pod with
// the given labels. Empty-selector semantics are kind-aware: an empty (or nil)
// Service selector binds nothing (a selector-less Service has no endpoints),
// while an empty NetworkPolicy/PodDisruptionBudget/workload selector binds every
// pod in the namespace (corpus #8, the degenerate `podSelector: {}`). A nil
// selector binds nothing for any kind.
func selectorBinds(ownerKind string, selector, labels map[string]string) bool {
	if selector == nil {
		return false
	}
	if len(selector) == 0 {
		return ownerKind != "Service"
	}
	return subsetOf(selector, labels)
}

// subsetOf reports whether every key/value in sel is present in labels.
func subsetOf(sel, labels map[string]string) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// crossRefMatches compares a cross-reference target against a resource, by uid
// when available, else by Kind/namespace/name.
func crossRefMatches(refd, target Ref) bool {
	if refd.UID != "" && target.UID != "" {
		return refd.UID == target.UID
	}
	return refd.GVK.Kind == target.GVK.Kind &&
		refd.Namespace == target.Namespace &&
		refd.Name == target.Name
}
