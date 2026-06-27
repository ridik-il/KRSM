package state

import (
	"sync"

	"github.com/ridik-il/krsm/closure"
)

// referentKey is the cross-ref index key: Kind/namespace/name (closure.Ref.String()).
// The index-free projection (cluster.Project) leaves cross-ref referent uids EMPTY, so
// the engine matches a consumer to its referent by Kind/ns/name (closure.CrossRefMatches'
// fallback branch). The index keys by the SAME tuple, guaranteeing Consumers /
// ControllersTargeting return what a linear crossRefMatches scan would — the
// parity-critical invariant (design §"Parity-critical").
func referentKey(r closure.Ref) string { return r.String() }

// objKey is the stable per-object identity: the uid if present, else Kind/ns/name. It
// mirrors closure.Ref.key() so an object updates/deletes the exact entries it inserted.
func objKey(r closure.Ref) string {
	if r.UID != "" {
		return "uid:" + r.UID
	}
	return r.String()
}

// xrefEntry is one reverse cross-ref edge: a consumer and whether the edge is an HPA
// scaleTargetRef (which ControllersTargeting serves and Consumers excludes — matching
// scanState's RefScaleTarget split).
type xrefEntry struct {
	consumer    closure.Ref
	scaleTarget bool
}

// contribution records exactly which index edges one object added, so an update
// (delete-then-add) or a delete removes precisely what it inserted without rescanning.
type contribution struct {
	ref        closure.Ref
	uid        string
	inNS       bool
	ownerUIDs  []string
	inSelOwner bool
	xrefKeys   []string
}

// index is the four inverted indexes + the byUID/byHuman lookup maps that back the
// nine closure.State methods, under one RWMutex. It stores only closure.Object
// projections (never raw API objects), so Secret/ConfigMap data is structurally absent.
type index struct {
	mu       sync.RWMutex
	byUID    map[string]*closure.Object // uid          → object (Get fast path)
	byHuman  map[string]*closure.Object // Kind/ns/name → object (Get fallback)
	owner    map[string][]closure.Ref   // owner uid    → child refs
	ns       map[string][]closure.Ref   // namespace    → contained refs (excl. Kind=Namespace)
	selOwner map[string][]closure.Ref   // namespace    → Service/PDB/NetworkPolicy refs
	xref     map[string][]xrefEntry     // referentKey  → consumer entries
	contrib  map[string]*contribution   // objKey       → what this object contributed
	rv       map[string]string          // uid:UID and Kind/ns/name → resourceVersion (staleness guard)
}

func newIndex() *index {
	return &index{
		byUID:    map[string]*closure.Object{},
		byHuman:  map[string]*closure.Object{},
		owner:    map[string][]closure.Ref{},
		ns:       map[string][]closure.Ref{},
		selOwner: map[string][]closure.Ref{},
		xref:     map[string][]xrefEntry{},
		contrib:  map[string]*contribution{},
		rv:       map[string]string{},
	}
}

// upsert inserts or replaces o's edges in every index (delete-then-add for an existing
// key, since labels/owners/refs can change). The stored *Object is never mutated in
// place — an update installs a NEW pointer — so a reader holding an earlier pointer
// sees a consistent immutable snapshot.
func (ix *index) upsert(o closure.Object) { ix.upsertWithRV(o, "") }

// upsertWithRV is upsert plus the object's resourceVersion, tracked for the staleness
// guard. closure.Object carries no rv (and closure must stay unchanged), so rv lives in
// the state index keyed exactly like the lookup maps (uid and Kind/ns/name).
func (ix *index) upsertWithRV(o closure.Object, rv string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	key := objKey(o.Ref)
	if _, ok := ix.contrib[key]; ok {
		ix.removeLocked(key)
	}

	stored := o
	c := &contribution{ref: o.Ref}

	ix.byHuman[o.Ref.String()] = &stored
	if o.Ref.UID != "" {
		ix.byUID[o.Ref.UID] = &stored
		c.uid = o.Ref.UID
	}

	if o.Ref.GVK.Kind != "Namespace" {
		ix.ns[o.Ref.Namespace] = append(ix.ns[o.Ref.Namespace], o.Ref)
		c.inNS = true
	}

	for _, ow := range o.Owners {
		if ow.UID != "" {
			ix.owner[ow.UID] = append(ix.owner[ow.UID], o.Ref)
			c.ownerUIDs = append(c.ownerUIDs, ow.UID)
		}
	}

	if closure.IsSelectorKind(o.Ref.GVK.Kind) {
		ix.selOwner[o.Ref.Namespace] = append(ix.selOwner[o.Ref.Namespace], o.Ref)
		c.inSelOwner = true
	}

	seenX := map[string]bool{}
	for _, cr := range o.CrossRefs {
		k := referentKey(cr.Ref)
		ix.xref[k] = append(ix.xref[k], xrefEntry{consumer: o.Ref, scaleTarget: cr.Kind == closure.RefScaleTarget})
		if !seenX[k] {
			c.xrefKeys = append(c.xrefKeys, k)
			seenX[k] = true
		}
	}

	ix.contrib[key] = c
	if rv != "" {
		ix.rv[o.Ref.String()] = rv
		if o.Ref.UID != "" {
			ix.rv["uid:"+o.Ref.UID] = rv
		}
	}
}

// remove evicts the object identified by key and every edge it contributed.
func (ix *index) remove(key string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.removeLocked(key)
}

func (ix *index) removeLocked(key string) {
	c := ix.contrib[key]
	if c == nil {
		return
	}
	delete(ix.byHuman, c.ref.String())
	if c.uid != "" {
		delete(ix.byUID, c.uid)
	}
	if c.inNS {
		ix.ns[c.ref.Namespace] = removeRef(ix.ns[c.ref.Namespace], c.ref)
	}
	for _, ou := range c.ownerUIDs {
		ix.owner[ou] = removeRef(ix.owner[ou], c.ref)
	}
	if c.inSelOwner {
		ix.selOwner[c.ref.Namespace] = removeRef(ix.selOwner[c.ref.Namespace], c.ref)
	}
	for _, xk := range c.xrefKeys {
		ix.xref[xk] = removeXref(ix.xref[xk], c.ref)
	}
	delete(ix.rv, c.ref.String())
	if c.uid != "" {
		delete(ix.rv, "uid:"+c.uid)
	}
	delete(ix.contrib, key)
}

// --- the nine closure.State reads, each under one read lock, returning copies ---

func (ix *index) get(r closure.Ref) (*closure.Object, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.lookupLocked(r)
}

// rvFor returns the cached resourceVersion for r (uid first, then Kind/ns/name), if the
// object is tracked and was stored with one.
func (ix *index) rvFor(r closure.Ref) (string, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if r.UID != "" {
		if rv, ok := ix.rv["uid:"+r.UID]; ok {
			return rv, true
		}
	}
	rv, ok := ix.rv[r.String()]
	return rv, ok
}

// lookupLocked reproduces scanState.lookup: uid first, then Kind/ns/name.
func (ix *index) lookupLocked(r closure.Ref) (*closure.Object, bool) {
	if r.UID != "" {
		if o, ok := ix.byUID[r.UID]; ok {
			return o, true
		}
	}
	o, ok := ix.byHuman[r.String()]
	return o, ok
}

func (ix *index) ownedChildren(r closure.Ref) []closure.Ref {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	o, ok := ix.lookupLocked(r)
	if !ok {
		return nil
	}
	return cloneRefs(ix.owner[o.Ref.UID])
}

func (ix *index) namespaceContents(ns string) []closure.Ref {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return cloneRefs(ix.ns[ns])
}

func (ix *index) podsMatching(ns string, sel closure.LabelSelector, ownerKind string) []closure.Ref {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var out []closure.Ref
	for _, ref := range ix.ns[ns] {
		if ref.GVK.Kind != "Pod" {
			continue
		}
		o, ok := ix.lookupLocked(ref)
		if !ok {
			continue
		}
		if closure.SelectorBinds(ownerKind, sel, o.Labels) {
			out = append(out, ref)
		}
	}
	return out
}

func (ix *index) podsSelectedBy(r closure.Ref) []closure.Ref {
	ix.mu.RLock()
	o, ok := ix.lookupLocked(r)
	ix.mu.RUnlock()
	if !ok {
		return nil
	}
	return ix.podsMatching(o.Ref.Namespace, o.Selector, o.Ref.GVK.Kind)
}

func (ix *index) selectorsMatchingLabels(ns string, labels map[string]string) []closure.Ref {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var out []closure.Ref
	for _, ref := range ix.selOwner[ns] {
		o, ok := ix.lookupLocked(ref)
		if !ok {
			continue
		}
		if closure.SelectorBinds(o.Ref.GVK.Kind, o.Selector, labels) {
			out = append(out, ref)
		}
	}
	return out
}

func (ix *index) selectorsTargeting(pod closure.Ref) []closure.Ref {
	ix.mu.RLock()
	p, ok := ix.lookupLocked(pod)
	ix.mu.RUnlock()
	if !ok {
		return nil
	}
	return ix.selectorsMatchingLabels(pod.Namespace, p.Labels)
}

func (ix *index) consumers(target closure.Ref) []closure.Ref {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	t, ok := ix.lookupLocked(target)
	if !ok {
		return nil
	}
	return ix.collectXrefLocked(t.Ref, false)
}

func (ix *index) controllersTargeting(r closure.Ref) []closure.Ref {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	t, ok := ix.lookupLocked(r)
	if !ok {
		return nil
	}
	return ix.collectXrefLocked(t.Ref, true)
}

// collectXrefLocked returns the distinct consumers of target via cross-refs of the
// requested flavour (scaleTarget true → HPA scaleTargetRef only; false → all other
// cross-refs). It dedups by consumer key, matching scanState's "append each consumer
// once" semantics.
func (ix *index) collectXrefLocked(target closure.Ref, scaleTarget bool) []closure.Ref {
	var out []closure.Ref
	seen := map[string]bool{}
	for _, e := range ix.xref[referentKey(target)] {
		if e.scaleTarget != scaleTarget {
			continue
		}
		k := objKey(e.consumer)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e.consumer)
	}
	return out
}

func cloneRefs(in []closure.Ref) []closure.Ref {
	if len(in) == 0 {
		return nil
	}
	out := make([]closure.Ref, len(in))
	copy(out, in)
	return out
}

// removeRef filters every entry whose identity equals r out of s, in place. Safe to
// mutate the backing array because reads always hand callers a clone (cloneRefs).
func removeRef(s []closure.Ref, r closure.Ref) []closure.Ref {
	k := objKey(r)
	out := s[:0]
	for _, e := range s {
		if objKey(e) != k {
			out = append(out, e)
		}
	}
	return out
}

func removeXref(s []xrefEntry, consumer closure.Ref) []xrefEntry {
	k := objKey(consumer)
	out := s[:0]
	for _, e := range s {
		if objKey(e.consumer) != k {
			out = append(out, e)
		}
	}
	return out
}
