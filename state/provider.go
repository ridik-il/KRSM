// Package state is the informer-backed, incrementally indexed closure.State (v0.5,
// ADR-0004, DESIGN §4/§7): the SECOND State implementation beside closure.NewScanState
// and the one-shot internal/cluster reader. It serves the nine closure.State methods
// from four inverted indexes in O(c·d) (blast radius), not O(c·n) (cluster size), so a
// verdict needs zero synchronous API reads in the common case.
//
// It sits OUTSIDE the stdlib-only closure/ and scope/ trees (enforced by
// internal/archguard): client-go enters here, never in the embeddable SDK. It is
// READ-ONLY by construction — get/list/watch over the tracked GVRs, never a write verb
// (a source-guard test forbids it) — and watches Secrets/ConfigMaps METADATA-ONLY
// (PartialObjectMetadata) so their data is never fetched or cached (C3).
package state

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/internal/cluster"
)

// metadataKinds are the kinds whose ONLY relational role is being referenced
// (cross-ref relation), so they are watched metadata-only (PartialObjectMetadata) and
// their data is never cached (C3, #14). PartialObjectMetadata still carries
// labels/ownerReferences/finalizers — everything the relations read for these kinds.
var metadataKinds = map[string]bool{"Secret": true, "ConfigMap": true}

// Options configures the informer factories.
type Options struct {
	// Resync is the informer resync period (0 = no periodic resync; watch deltas only).
	Resync time.Duration
}

// objectGetter does bounded, single-object live GETs for the staleness guard (FreshGet):
// the dynamic client for normal kinds, the metadata client for metadata-only kinds. It is
// read-only (only Get) and never lists — the O(d) on-demand fallback of ADR-0004.
type objectGetter struct {
	dyn  dynamic.Interface
	meta metadata.Interface
}

// Provider is the informer-backed indexed closure.State.
type Provider struct {
	idx     *index
	scope   cluster.ScopeInfo
	starts  []func(stopCh <-chan struct{})
	syncs   []cache.InformerSynced
	synced  atomic.Bool
	getter  objectGetter                   // dynamic + metadata clients for FreshGet
	targets map[closure.GVK]cluster.Target // GVK → GVR/namespaced, for FreshGet resolution
}

var _ closure.State = (*Provider)(nil)

// New builds a Provider from a *rest.Config: it resolves the tracked GVRs + scope via
// the slice-1 C1-tolerant discovery, splits the metadata-only kinds (Secret/ConfigMap)
// from the full informers, and wires the four indexes' event handlers. It does NOT
// block — call Start then WaitForCacheSync before serving a verdict.
func New(cfg *rest.Config, opts Options) (*Provider, error) {
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build discovery client: %w", err)
	}
	targets, scope, err := cluster.DiscoverTargets(disc)
	if err != nil {
		return nil, err
	}
	var full, meta []cluster.Target
	for _, t := range targets {
		if metadataKinds[t.GVK.Kind] {
			meta = append(meta, t)
		} else {
			full = append(full, t)
		}
	}

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}
	metaClient, err := metadata.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build metadata client: %w", err)
	}

	dynFactory := dynamicinformer.NewDynamicSharedInformerFactory(dyn, opts.Resync)
	metaFactory := metadatainformer.NewSharedInformerFactory(metaClient, opts.Resync)
	return newProvider(dynFactory, full, metaFactory, meta, scope, objectGetter{dyn: dyn, meta: metaClient})
}

// newProvider is the injectable seam: it takes already-built informer factories (real
// in New, fakes in tests) plus the full/metadata target splits and the scope, registers
// each informer's event handler, and records the per-handler sync funcs. It uses the
// REGISTRATION's HasSynced (not the informer's) so WaitForCacheSync waits until each
// handler has actually drained the initial list into the indexes.
func newProvider(
	dynFactory dynamicinformer.DynamicSharedInformerFactory,
	fullTargets []cluster.Target,
	metaFactory metadatainformer.SharedInformerFactory,
	metaTargets []cluster.Target,
	scope cluster.ScopeInfo,
	getter objectGetter,
) (*Provider, error) {
	p := &Provider{idx: newIndex(), scope: scope, getter: getter, targets: map[closure.GVK]cluster.Target{}}

	for _, t := range fullTargets {
		p.targets[t.GVK] = t
		inf := dynFactory.ForResource(t.GVR).Informer()
		reg, err := inf.AddEventHandler(p.dynamicHandler())
		if err != nil {
			return nil, fmt.Errorf("add handler for %s: %w", t.GVR, err)
		}
		p.syncs = append(p.syncs, reg.HasSynced)
	}
	for _, t := range metaTargets {
		p.targets[t.GVK] = t
		inf := metaFactory.ForResource(t.GVR).Informer()
		reg, err := inf.AddEventHandler(p.metadataHandler(t.GVK))
		if err != nil {
			return nil, fmt.Errorf("add metadata handler for %s: %w", t.GVR, err)
		}
		p.syncs = append(p.syncs, reg.HasSynced)
	}
	if dynFactory != nil {
		p.starts = append(p.starts, dynFactory.Start)
	}
	if metaFactory != nil {
		p.starts = append(p.starts, metaFactory.Start)
	}
	return p, nil
}

// Start launches every informer. Non-blocking.
func (p *Provider) Start(ctx context.Context) {
	for _, start := range p.starts {
		start(ctx.Done())
	}
}

// WaitForCacheSync blocks until every handler has processed its informer's initial
// list. The caller MUST NOT serve a verdict before this returns true — an unsynced
// cache is an unknown closure (fail-closed, DESIGN §5). Returns false if ctx is
// cancelled first.
func (p *Provider) WaitForCacheSync(ctx context.Context) bool {
	ok := cache.WaitForCacheSync(ctx.Done(), p.syncs...)
	if ok {
		p.synced.Store(true)
	}
	return ok
}

// HasSynced reports whether the indexes are populated and safe to serve. Before it is
// true the caller must fail closed; the Provider never renders a verdict itself.
func (p *Provider) HasSynced() bool { return p.synced.Load() }

// --- event handlers: project the informer object, maintain the indexes ---

func (p *Provider) dynamicHandler() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { p.ingestUnstructured(o) },
		UpdateFunc: func(_, n any) { p.ingestUnstructured(n) },
		DeleteFunc: func(o any) { p.evictUnstructured(o) },
	}
}

func (p *Provider) ingestUnstructured(o any) {
	u, ok := o.(*unstructured.Unstructured)
	if !ok {
		return
	}
	if obj, err := cluster.Project(*u, p.scope); err == nil {
		p.idx.upsertWithRV(obj, u.GetResourceVersion())
	}
}

func (p *Provider) evictUnstructured(o any) {
	if tomb, ok := o.(cache.DeletedFinalStateUnknown); ok {
		o = tomb.Obj
	}
	u, ok := o.(*unstructured.Unstructured)
	if !ok {
		return
	}
	if obj, err := cluster.Project(*u, p.scope); err == nil {
		p.idx.remove(objKey(obj.Ref))
	}
}

func (p *Provider) metadataHandler(gvk closure.GVK) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { p.ingestMetadata(o, gvk) },
		UpdateFunc: func(_, n any) { p.ingestMetadata(n, gvk) },
		DeleteFunc: func(o any) { p.evictMetadata(o, gvk) },
	}
}

func (p *Provider) ingestMetadata(o any, gvk closure.GVK) {
	pom, ok := o.(*metav1.PartialObjectMetadata)
	if !ok {
		return
	}
	if obj, ok := p.projectMetadata(pom, gvk); ok {
		p.idx.upsertWithRV(obj, pom.GetResourceVersion())
	}
}

func (p *Provider) evictMetadata(o any, gvk closure.GVK) {
	if tomb, ok := o.(cache.DeletedFinalStateUnknown); ok {
		o = tomb.Obj
	}
	if obj, ok := p.projectMetadata(o, gvk); ok {
		p.idx.remove(objKey(obj.Ref))
	}
}

// projectMetadata builds a closure.Object from a PartialObjectMetadata, reusing the
// SAME cluster.Project as the full path. It stamps the TypeMeta from the known GVK
// (the metadata API may omit it) and converts to unstructured; the type carries no
// `data`, so a Secret/ConfigMap projected here can never leak its data into the cache.
func (p *Provider) projectMetadata(o any, gvk closure.GVK) (closure.Object, bool) {
	pom, ok := o.(*metav1.PartialObjectMetadata)
	if !ok {
		return closure.Object{}, false
	}
	pom = pom.DeepCopy()
	pom.APIVersion = gvk.Version
	if gvk.Group != "" {
		pom.APIVersion = gvk.Group + "/" + gvk.Version
	}
	pom.Kind = gvk.Kind

	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pom)
	if err != nil {
		return closure.Object{}, false
	}
	obj, err := cluster.Project(unstructured.Unstructured{Object: m}, p.scope)
	if err != nil {
		return closure.Object{}, false
	}
	return obj, true
}

// --- the nine closure.State methods, served from the indexes ---

func (p *Provider) Get(r closure.Ref) (*closure.Object, bool)  { return p.idx.get(r) }
func (p *Provider) OwnedChildren(r closure.Ref) []closure.Ref  { return p.idx.ownedChildren(r) }
func (p *Provider) NamespaceContents(ns string) []closure.Ref  { return p.idx.namespaceContents(ns) }
func (p *Provider) PodsSelectedBy(r closure.Ref) []closure.Ref { return p.idx.podsSelectedBy(r) }
func (p *Provider) PodsMatching(ns string, sel closure.LabelSelector, ownerKind string) []closure.Ref {
	return p.idx.podsMatching(ns, sel, ownerKind)
}
func (p *Provider) SelectorsTargeting(pod closure.Ref) []closure.Ref {
	return p.idx.selectorsTargeting(pod)
}
func (p *Provider) SelectorsMatchingLabels(ns string, labels map[string]string) []closure.Ref {
	return p.idx.selectorsMatchingLabels(ns, labels)
}
func (p *Provider) Consumers(target closure.Ref) []closure.Ref { return p.idx.consumers(target) }
func (p *Provider) ControllersTargeting(r closure.Ref) []closure.Ref {
	return p.idx.controllersTargeting(r)
}

// --- staleness guard (C2, slice 4): bounded on-demand GET, never a re-list ---

// FreshGet does a bounded, on-demand live GET for one ref (the ADR-0004 fallback): the
// METADATA client for metadata-only kinds (Secret/ConfigMap) so their data is never
// fetched, the dynamic client otherwise, projected through the SAME cluster.Project the
// informers use. Never a re-list. Returns (object, found, error); a NotFound is
// (zero, false, nil) so the caller distinguishes "gone" from "error".
func (p *Provider) FreshGet(ctx context.Context, ref closure.Ref) (closure.Object, bool, error) {
	obj, _, found, err := p.freshGet(ctx, ref)
	return obj, found, err
}

// freshGet is FreshGet plus the live object's resourceVersion, which the staleness guard
// uses to confirm the cache was reconciled to the request's view.
func (p *Provider) freshGet(ctx context.Context, ref closure.Ref) (closure.Object, string, bool, error) {
	t, ok := p.targetFor(ref.GVK)
	if !ok {
		return closure.Object{}, "", false, nil
	}
	ns := ref.Namespace
	if !t.Namespaced {
		ns = ""
	}

	if metadataKinds[ref.GVK.Kind] {
		var ri metadata.ResourceInterface = p.getter.meta.Resource(t.GVR)
		if t.Namespaced {
			ri = p.getter.meta.Resource(t.GVR).Namespace(ns)
		}
		pom, err := ri.Get(ctx, ref.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return closure.Object{}, "", false, nil
		}
		if err != nil {
			return closure.Object{}, "", false, fmt.Errorf("fresh get %s/%s: %w", ref.GVK.Kind, ref.Name, err)
		}
		obj, ok := p.projectMetadata(pom, t.GVK)
		if !ok {
			return closure.Object{}, "", false, nil
		}
		return obj, pom.GetResourceVersion(), true, nil
	}

	var ri dynamic.ResourceInterface = p.getter.dyn.Resource(t.GVR)
	if t.Namespaced {
		ri = p.getter.dyn.Resource(t.GVR).Namespace(ns)
	}
	u, err := ri.Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return closure.Object{}, "", false, nil
	}
	if err != nil {
		return closure.Object{}, "", false, fmt.Errorf("fresh get %s/%s: %w", ref.GVK.Kind, ref.Name, err)
	}
	obj, err := cluster.Project(*u, p.scope)
	if err != nil {
		return closure.Object{}, "", false, fmt.Errorf("project %s/%s: %w", ref.GVK.Kind, ref.Name, err)
	}
	return obj, u.GetResourceVersion(), true, nil
}

// targetFor resolves the tracked GVR/namespaced for a ref's GVK: exact GVK first, then a
// Kind fallback (a closure ref may carry a non-preferred group/version). An untracked
// kind yields false → FreshGet reports not-found.
func (p *Provider) targetFor(gvk closure.GVK) (cluster.Target, bool) {
	if t, ok := p.targets[gvk]; ok {
		return t, true
	}
	for k, t := range p.targets {
		if k.Kind == gvk.Kind {
			return t, true
		}
	}
	return cluster.Target{}, false
}

// stalenessReason is the single, credential-free reason a verdict is denied because the
// cache could not be confirmed current against the request (ADR-0004 fail-closed).
const stalenessReason = "could not confirm current state"

// StalenessError is returned by CheckFreshness when drift against the request cannot be
// reconciled — the caller must fail closed. Its message never echoes credentials or data.
type StalenessError struct{ Ref closure.Ref }

func (e *StalenessError) Error() string { return stalenessReason }

// CheckFreshness is the per-request staleness guard (C2, ADR-0004). Given the request
// target and its authoritative resourceVersion (from the admission request) plus the
// closure neighbourhood (the O(d) members the verdict depends on), it confirms the index
// is current enough to trust:
//
//   - in sync (cache ≥ request rv) → returns nil with NO API call (the read-free hot path);
//   - drift (cache older/absent) → a bounded FreshGet over {target} ∪ neighbourhood to
//     reconcile the cache (never a re-list); if the target still cannot be reconciled to
//     the request rv → *StalenessError (the caller fails closed).
func (p *Provider) CheckFreshness(ctx context.Context, target closure.Ref, targetRV string, neighbourhood []closure.Ref) error {
	if cachedRV, ok := p.idx.rvFor(target); ok && inSync(cachedRV, targetRV) {
		return nil
	}
	targetKey := objKey(target)
	var reconciledRV string
	var targetFound bool
	seen := map[string]bool{}
	for _, r := range append([]closure.Ref{target}, neighbourhood...) {
		k := objKey(r)
		if seen[k] {
			continue
		}
		seen[k] = true
		obj, rv, found, err := p.freshGet(ctx, r)
		if err != nil {
			return &StalenessError{Ref: target}
		}
		if found {
			p.idx.upsertWithRV(obj, rv)
		}
		if k == targetKey {
			targetFound, reconciledRV = found, rv
		}
	}
	if !targetFound || !inSync(reconciledRV, targetRV) {
		return &StalenessError{Ref: target}
	}
	return nil
}

// inSync reports whether a cache entry at cachedRV is current for a request whose
// authoritative object is at reqRV. resourceVersion is an opaque, server-assigned token;
// the only guarantees are per-object monotonicity and (for etcd) a decimal-uint64 form.
// Equal strings are trivially in sync. Otherwise we compare numerically when BOTH parse;
// a cache strictly newer than the request is still in sync (it holds at least the
// request's state). If either side does not parse, any difference is treated as drift —
// the fail-closed-safe direction (prefer a bounded FreshGet over trusting a stale cache).
func inSync(cachedRV, reqRV string) bool {
	if cachedRV == reqRV {
		return true
	}
	c, errC := strconv.ParseUint(cachedRV, 10, 64)
	r, errR := strconv.ParseUint(reqRV, 10, 64)
	if errC == nil && errR == nil {
		return c >= r
	}
	return false
}
