package cluster

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/ridik-il/krsm/closure"
)

// Reader reads a live cluster READ-ONLY and assembles a closure.State. It lists the
// relevant GVKs via the dynamic client (verb "list" only — no create/update/patch/
// delete/deletecollection/apply anywhere in this package), records each GVK's
// namespaced/cluster scope from discovery, and feeds both into the pure BuildObjects
// projection (slice 1) → closure.NewScanState.
//
// It is fail-closed (docs/design/v0.4-live-cluster-reads.md §5): a discovery failure
// or a list error returns an ERROR, never a silently shrunk object set — a partial
// read is an unknown closure, which the safety gate must deny. Errors wrap only the
// API error; the *rest.Config (bearer token / client cert) is never logged or echoed.
type Reader struct {
	disc discovery.DiscoveryInterface
	dyn  dynamic.Interface
}

// NewReader builds a read-only Reader from a *rest.Config resolved from kubeconfig/
// context (slice 3 supplies one). It constructs a discovery client and a dynamic
// client; the listing logic stays behind newReader so tests inject fakes. The cfg is
// used only to build clients — never logged.
func NewReader(cfg *rest.Config) (*Reader, error) {
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build discovery client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}
	return newReader(disc, dyn), nil
}

// newReader is the injectable seam: it takes already-built clients so tests drive the
// Reader with discovery/fake + dynamic/fake and no real cluster.
func newReader(disc discovery.DiscoveryInterface, dyn dynamic.Interface) *Reader {
	return &Reader{disc: disc, dyn: dyn}
}

// readTargets are the kinds the four relations can traverse — the broad, safe default
// (plan decision 4): under-scoping is unsafe, over-inclusion is merely conservative.
// Selection intersects this set with what discovery actually reports; discovered CRDs
// (custom groups) are added on top so an ownerReference into a CRD is not missed.
var readTargets = map[string]bool{
	"Pod":                     true,
	"Service":                 true,
	"ConfigMap":               true,
	"Secret":                  true,
	"PersistentVolumeClaim":   true,
	"Namespace":               true,
	"Deployment":              true,
	"ReplicaSet":              true,
	"StatefulSet":             true,
	"DaemonSet":               true,
	"Job":                     true,
	"CronJob":                 true,
	"PodDisruptionBudget":     true,
	"NetworkPolicy":           true,
	"HorizontalPodAutoscaler": true,
}

// builtInGroups are the core Kubernetes API groups. A resource in any OTHER group is
// treated as a CRD and listed too, so the live read can follow ownerReferences into a
// custom resource (plan decision 4). The empty string is the core group ("v1").
var builtInGroups = map[string]bool{
	"":                          true,
	"apps":                      true,
	"batch":                     true,
	"policy":                    true,
	"networking.k8s.io":         true,
	"autoscaling":               true,
	"rbac.authorization.k8s.io": true,
	"storage.k8s.io":            true,
	"apiextensions.k8s.io":      true,
}

// listTargets are the GVRs the Reader lists. Scope (namespaced vs cluster) is carried
// separately by the ScopeInfo from scopeFromLists, so a target needs only its GVR.
type listTargets = []schema.GroupVersionResource

// State lists the relevant GVKs read-only, projects them through BuildObjects, and
// returns a closure.State. Fail-closed on any discovery or list error.
func (r *Reader) State(ctx context.Context) (closure.State, error) {
	// Scope (the per-GVK namespaced flag) needs EVERY served version, so it reads the
	// all-version ServerGroupsAndResources answer.
	lists, err := serverResources(r.disc)
	if err != nil {
		return nil, err
	}
	scope := scopeFromLists(lists)

	// LIST targets read the SERVER-PREFERRED version only (S3), so a resource served at
	// several versions (e.g. HPA at autoscaling/v1 and /v2) is listed once, not per version.
	preferred, err := serverPreferredResources(r.disc)
	if err != nil {
		return nil, err
	}
	targets, err := selectTargets(preferred)
	if err != nil {
		return nil, err
	}

	var objs []unstructured.Unstructured
	for _, t := range targets {
		items, err := r.listAll(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", t.Resource, err)
		}
		objs = append(objs, items...)
	}

	built, err := BuildObjects(objs, scope)
	if err != nil {
		return nil, fmt.Errorf("build objects: %w", err)
	}
	return closure.NewScanState(built), nil
}

// listPageLimit is the per-page bound for the dynamic List (C3): a single check must not
// pull every object of a kind in one unbounded call. 500 matches kubectl's default page
// size — large enough to keep round-trips few, small enough to bound memory/latency.
const listPageLimit = 500

// listAll lists every object of gvr READ-ONLY across pages, following the Continue token
// until the server reports no more. It is the ONLY place this package reads objects and
// uses only Resource(...).List — no write verb. Any page error is returned (the caller
// fails closed); a partial accumulation is never returned alongside an error.
func (r *Reader) listAll(ctx context.Context, gvr schema.GroupVersionResource) ([]unstructured.Unstructured, error) {
	var out []unstructured.Unstructured
	cont := ""
	for {
		ul, err := r.dyn.Resource(gvr).List(ctx, metav1.ListOptions{Limit: listPageLimit, Continue: cont})
		if err != nil {
			return nil, err
		}
		out = append(out, ul.Items...)
		cont = ul.GetContinue()
		if cont == "" {
			return out, nil
		}
	}
}

// ResolveKind maps a user-supplied <Kind> token (optionally qualified by an API group)
// to the canonical Kind discovery reports. The CLI target has no uid, so the live State
// resolves it by its human key (Kind/ns/name) and closure.Ref.human renders GVK.Kind
// verbatim — so the returned Kind must equal the live object's `kind` field exactly. An
// operator may type the canonical Kind, a lowercased kind, or the plural/singular resource
// name; matching is case-insensitive.
//
// group disambiguates a Kind served by more than one API group — the CRD-heavy-cluster
// collision S2 (#16) addresses:
//   - group != "": resolve the resource in THAT group only; fail closed if no such Kind is
//     served there (never fall back to another group).
//   - group == "" and the Kind is served by exactly one group: resolve it (the common case,
//     unchanged — multiple served VERSIONS of one group still count as one group).
//   - group == "" and the Kind is served by MORE THAN ONE group: FAIL CLOSED, listing the
//     candidate groups, rather than silently picking whichever discovery enumerated first.
//
// It FAILS CLOSED throughout: a discovery error is returned (never a guessed Kind), and a
// token matching no discovered resource is an error.
//
// Residual (documented): the returned target carries only the canonical Kind —
// closure.Ref.human is group-agnostic by design (the goldens depend on it), so two
// same-Kind objects in different groups sharing a namespace/name still share a human key in
// the engine. The qualifier removes the *silent wrong-GVR* footgun on the CLI; the webhook's
// uid-based match (a later slice) is the complete fix.
func (r *Reader) ResolveKind(_ context.Context, kind, group string) (string, error) {
	// Discovery (ServerGroupsAndResources) takes no context today; the ctx parameter
	// keeps the signature uniform with State and ready for a context-aware RESTMapper.
	lists, err := serverResources(r.disc)
	if err != nil {
		return "", err
	}
	want := strings.ToLower(kind)

	// Collect the canonical Kind keyed by API group for every discovered resource the token
	// matches (canonical Kind, lowercased kind, or plural/singular resource name), skipping
	// subresources. Keying by group collapses multiple served versions of one group to a
	// single entry, so multi-version (not multi-group) is never treated as ambiguous.
	matches := map[string]string{} // group → canonical Kind
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, res := range list.APIResources {
			if isSubresource(res.Name) {
				continue
			}
			if want == strings.ToLower(res.Kind) ||
				want == strings.ToLower(res.Name) ||
				(res.SingularName != "" && want == strings.ToLower(res.SingularName)) {
				matches[gv.Group] = res.Kind
			}
		}
	}

	if group != "" {
		if canonical, ok := matches[group]; ok {
			return canonical, nil
		}
		return "", fmt.Errorf("kind %q is not served by API group %q in the cluster's API discovery", kind, group)
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("unknown kind %q: no such resource in the cluster's API discovery", kind)
	case 1:
		for _, canonical := range matches {
			return canonical, nil
		}
	}

	// Ambiguous across groups: require an explicit qualifier rather than guess.
	groups := make([]string, 0, len(matches))
	for g := range matches {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	qualified := make([]string, len(groups))
	for i, g := range groups {
		qualified[i] = kind + "." + g
	}
	return "", fmt.Errorf("kind %q is ambiguous across API groups %v; qualify the target as one of %v", kind, groups, qualified)
}

// selectTargets picks the GVRs to list: every discovered resource whose kind is a
// read target OR whose group is a custom (CRD) group, AND that actually supports the
// `list` verb. Subresources (names with a "/") are skipped. The discovery answer is
// the source of truth, so a kind the cluster does not report is never listed.
//
// The list-verb filter is essential on a REAL cluster: groups like authorization.k8s.io
// / authentication.k8s.io expose create-only "virtual" resources (subjectaccessreviews,
// tokenreviews) that are non-built-in and so would be swept in by the broad CRD rule,
// but support only `create` — listing them errors and would fail-close every read. A
// resource the four relations care about always supports `list`, so filtering on it is
// safe (it never drops a closure member) and necessary.
func selectTargets(lists []*metav1.APIResourceList) (listTargets, error) {
	var out listTargets
	seen := map[schema.GroupVersionResource]bool{}
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			return nil, fmt.Errorf("parse groupVersion %q: %w", list.GroupVersion, err)
		}
		for _, res := range list.APIResources {
			if isSubresource(res.Name) {
				continue
			}
			if !readTargets[res.Kind] && builtInGroups[gv.Group] {
				continue
			}
			if !supportsList(res.Verbs) {
				continue
			}
			r := schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: res.Name}
			if seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
		}
	}
	return out, nil
}

// supportsList reports whether a discovered resource's verb set includes "list".
//
// Safety direction: under-scoping the read (dropping a kind that IS a closure member)
// is the unsafe error a safety gate must avoid; over-inclusion is merely conservative
// (an extra List). So an EMPTY verb set errs toward INCLUSION (treated as listable):
// some fakes and older discovery answers omit Verbs, and the four-relation readTargets
// are all genuinely listable built-ins, so an empty set must not silently drop a real
// kind. If an empty-verb kind turns out to be genuinely non-listable, its List then
// fails — and State fails CLOSED on that error (it never proceeds on a partial read),
// so the conservative inclusion is still safe. A NON-empty set lacking "list" (a
// create-only virtual resource like subjectaccessreviews) is correctly skipped.
func supportsList(verbs metav1.Verbs) bool {
	if len(verbs) == 0 {
		return true
	}
	for _, v := range verbs {
		if v == "list" {
			return true
		}
	}
	return false
}

func isSubresource(name string) bool {
	for i := 0; i < len(name); i++ {
		if name[i] == '/' {
			return true
		}
	}
	return false
}

// scopeFromLists builds a ScopeInfo from the discovery answer: each (group, version,
// kind) → its Namespaced flag. A GVK absent from discovery is unknown (ok=false), so
// the caller fails closed rather than guess.
func scopeFromLists(lists []*metav1.APIResourceList) ScopeInfo {
	m := make(discoveryScope)
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue // a malformed groupVersion contributes no scope; List parsing fails elsewhere
		}
		for _, res := range list.APIResources {
			if isSubresource(res.Name) {
				continue
			}
			m[closure.GVK{Group: gv.Group, Version: gv.Version, Kind: res.Kind}] = res.Namespaced
		}
	}
	return m
}

// newDiscoveryScope queries discovery once and returns a ScopeInfo over the preferred
// resources. It FAILS CLOSED on a discovery error — never returning a partial/empty
// scope that would silently treat every GVK as unknown.
func newDiscoveryScope(disc discovery.DiscoveryInterface) (ScopeInfo, error) {
	lists, err := serverResources(disc)
	if err != nil {
		return nil, err
	}
	return scopeFromLists(lists), nil
}

// serverResources lists the cluster's API resources with PARTIAL-DISCOVERY TOLERANCE
// (C1, docs/design/v0.5-c1-partial-discovery.md). ServerGroupsAndResources returns a
// non-nil *discovery.ErrGroupDiscoveryFailed whenever an aggregated APIService is
// momentarily unavailable (metrics-server rolling, a flaky custom-metrics adapter, a
// webhook-backed group unreachable) WHILE STILL returning every group it could resolve.
// Treating that as fatal — the pre-C1 behaviour — denied every action the moment an
// unrelated aggregated API hiccupped, the single biggest real-world blocker.
//
// So: on ErrGroupDiscoveryFailed, KEEP the resolved lists and fail closed ONLY if a
// closure-relevant (built-in) group is among the failed set — an aggregated metrics API
// failing must not deny a `delete deployment`. Any OTHER error type stays fatal
// (unchanged fail-closed behaviour). The error names only the failed groups, never any
// credential material (the *rest.Config is never echoed).
func serverResources(disc discovery.DiscoveryInterface) ([]*metav1.APIResourceList, error) {
	_, lists, err := disc.ServerGroupsAndResources()
	if err != nil {
		var gdf *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &gdf) {
			return nil, fmt.Errorf("discover server resources: %w", err)
		}
		if relevant := closureRelevantFailures(gdf); len(relevant) > 0 {
			return nil, fmt.Errorf("discover server resources: closure-relevant API group(s) unavailable: %v", relevant)
		}
		// Only aggregated/add-on groups failed; proceed on the groups discovery resolved.
	}
	return lists, nil
}

// serverPreferredResources lists one APIResourceList per resource at its SERVER-PREFERRED
// version (S3), with the SAME partial-discovery tolerance as serverResources (C1): keep the
// resolved lists, fail closed only if a closure-relevant (built-in) group is among the
// failed set. It is used to pick LIST targets so a multi-version resource is listed once;
// the all-version ServerGroupsAndResources answer still feeds the namespaced-scope map.
func serverPreferredResources(disc discovery.DiscoveryInterface) ([]*metav1.APIResourceList, error) {
	lists, err := disc.ServerPreferredResources()
	if err != nil {
		var gdf *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &gdf) {
			return nil, fmt.Errorf("discover preferred resources: %w", err)
		}
		if relevant := closureRelevantFailures(gdf); len(relevant) > 0 {
			return nil, fmt.Errorf("discover preferred resources: closure-relevant API group(s) unavailable: %v", relevant)
		}
		// Only aggregated/add-on groups failed; proceed on the groups discovery resolved.
	}
	return lists, nil
}

// closureRelevantFailures returns the built-in GroupVersions among a partial-discovery
// failure. A built-in group hosts the closure relations (readTargets) and is served by
// the core kube-apiserver, so its discovery failing signals a real incompleteness and
// must fail closed. Non-built-in failures (aggregated metrics APIs, add-on/webhook
// groups) carry no closure relation and are tolerated (the accepted C1 trade-off: a
// simultaneously-failing CRD group that hosts an ownerReference participant degrades to a
// tolerated partial read rather than the unusable fail-closed-on-any-hiccup behaviour —
// see the design note's residual-risk section).
func closureRelevantFailures(gdf *discovery.ErrGroupDiscoveryFailed) []schema.GroupVersion {
	var relevant []schema.GroupVersion
	for gv := range gdf.Groups {
		if builtInGroups[gv.Group] {
			relevant = append(relevant, gv)
		}
	}
	return relevant
}

// discoveryScope is a ScopeInfo backed by a discovery-built GVK→namespaced map. A GVK
// not present is unknown (ok=false): the live replacement for the loader's static
// clusterScopedKinds map (internal/scenario/scenario.go:249).
type discoveryScope map[closure.GVK]bool

func (d discoveryScope) Namespaced(gvk closure.GVK) (bool, bool) {
	namespaced, ok := d[gvk]
	return namespaced, ok
}
