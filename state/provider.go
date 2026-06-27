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
	"sync/atomic"
	"time"

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

// Provider is the informer-backed indexed closure.State.
type Provider struct {
	idx    *index
	scope  cluster.ScopeInfo
	starts []func(stopCh <-chan struct{})
	syncs  []cache.InformerSynced
	synced atomic.Bool
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
	return newProvider(dynFactory, full, metaFactory, meta, scope)
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
) (*Provider, error) {
	p := &Provider{idx: newIndex(), scope: scope}

	for _, t := range fullTargets {
		inf := dynFactory.ForResource(t.GVR).Informer()
		reg, err := inf.AddEventHandler(p.dynamicHandler())
		if err != nil {
			return nil, fmt.Errorf("add handler for %s: %w", t.GVR, err)
		}
		p.syncs = append(p.syncs, reg.HasSynced)
	}
	for _, t := range metaTargets {
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
		p.idx.upsert(obj)
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
	if obj, ok := p.projectMetadata(o, gvk); ok {
		p.idx.upsert(obj)
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
