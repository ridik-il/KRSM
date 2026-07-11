package state

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	"k8s.io/client-go/metadata/metadatainformer"
	"sigs.k8s.io/yaml"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/internal/cluster"
)

// corpusGVR maps each Kind the golden corpus uses to its GVR. Used to register the
// fake informers and split metadata-only kinds. Pluralisation is explicit (never
// guessed) so the fakes serve the exact GVRs the Provider informs on.
var corpusGVR = map[string]schema.GroupVersionResource{
	"Pod":                     {Version: "v1", Resource: "pods"},
	"Service":                 {Version: "v1", Resource: "services"},
	"ConfigMap":               {Version: "v1", Resource: "configmaps"},
	"Secret":                  {Version: "v1", Resource: "secrets"},
	"PersistentVolumeClaim":   {Version: "v1", Resource: "persistentvolumeclaims"},
	"PersistentVolume":        {Version: "v1", Resource: "persistentvolumes"},
	"Namespace":               {Version: "v1", Resource: "namespaces"},
	"Deployment":              {Group: "apps", Version: "v1", Resource: "deployments"},
	"ReplicaSet":              {Group: "apps", Version: "v1", Resource: "replicasets"},
	"StatefulSet":             {Group: "apps", Version: "v1", Resource: "statefulsets"},
	"DaemonSet":               {Group: "apps", Version: "v1", Resource: "daemonsets"},
	"HorizontalPodAutoscaler": {Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"},
	"PodDisruptionBudget":     {Group: "policy", Version: "v1", Resource: "poddisruptionbudgets"},
	"NetworkPolicy":           {Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"},
	"MyCR":                    {Group: "example.com", Version: "v1", Resource: "mycrs"},
}

// corpusClusterScoped mirrors internal/scenario.clusterScopedKinds so the test resolves
// namespaces (and therefore injected uids) exactly as the loader does.
var corpusClusterScoped = map[string]bool{
	"Namespace": true, "PersistentVolume": true, "Node": true, "ClusterRole": true,
	"ClusterRoleBinding": true, "StorageClass": true, "PriorityClass": true,
	"CustomResourceDefinition": true,
}

var corpusDocSep = regexp.MustCompile(`(?m)^---\s*$`)

func corpusNsOf(kind, ns string) string {
	if corpusClusterScoped[kind] {
		return ""
	}
	if ns == "" {
		return "default"
	}
	return ns
}

func corpusUIDOf(kind, ns, name string) string {
	return fmt.Sprintf("uid:%s/%s/%s", kind, ns, name)
}

func corpusStripComments(doc string) string {
	var b strings.Builder
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// loadCorpusUnstructured re-parses a scenario's cluster.yaml into unstructured objects
// with loader-equivalent uids injected (metadata.uid and ownerReferences[].uid =
// uid:Kind/ns/name), exactly as internal/cluster's parity oracle does — so the Provider
// carries the SAME identities the YAML loader synthesises and owner/cross-ref matching
// is comparable edge-for-edge.
func loadCorpusUnstructured(t *testing.T, dir string) []*unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "cluster.yaml"))
	if err != nil {
		t.Fatalf("read cluster.yaml: %v", err)
	}
	var out []*unstructured.Unstructured
	for _, doc := range corpusDocSep.Split(string(raw), -1) {
		if strings.TrimSpace(corpusStripComments(doc)) == "" {
			continue
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("unmarshal manifest: %v\n%s", err, doc)
		}
		kind, _ := m["kind"].(string)
		if kind == "" {
			continue
		}
		meta, _ := m["metadata"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
			m["metadata"] = meta
		}
		name, _ := meta["name"].(string)
		nsRaw, _ := meta["namespace"].(string)
		resolvedNS := corpusNsOf(kind, nsRaw)
		meta["uid"] = corpusUIDOf(kind, resolvedNS, name)
		if owners, ok := meta["ownerReferences"].([]any); ok {
			for _, o := range owners {
				om, ok := o.(map[string]any)
				if !ok {
					continue
				}
				oKind, _ := om["kind"].(string)
				oName, _ := om["name"].(string)
				if oKind != "" && oName != "" {
					om["uid"] = corpusUIDOf(oKind, resolvedNS, oName)
				}
			}
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
	return out
}

// fakeScope is a cluster.ScopeInfo for the corpus kinds: known for any corpus kind,
// namespaced unless cluster-scoped.
type fakeScope struct{}

func (fakeScope) Namespaced(gvk closure.GVK) (bool, bool) {
	if _, ok := corpusGVR[gvk.Kind]; !ok {
		return false, false
	}
	return !corpusClusterScoped[gvk.Kind], true
}

// toPOM converts an unstructured object to a PartialObjectMetadata (TypeMeta stamped
// from the GVK), dropping everything but metadata — exactly what a metadata informer
// delivers, and structurally incapable of carrying Secret/ConfigMap data.
func toPOM(t *testing.T, u *unstructured.Unstructured, gvk closure.GVK) *metav1.PartialObjectMetadata {
	t.Helper()
	pom := &metav1.PartialObjectMetadata{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, pom); err != nil {
		t.Fatalf("to PartialObjectMetadata: %v", err)
	}
	pom.APIVersion = apiVersionOf(gvk)
	pom.Kind = gvk.Kind
	return pom
}

func apiVersionOf(gvk closure.GVK) string {
	if gvk.Group == "" {
		return gvk.Version
	}
	return gvk.Group + "/" + gvk.Version
}

// buildSyncedProvider wires the Provider over fake dynamic + metadata informers seeded
// with objs (Secrets/ConfigMaps go through the metadata informer, everything else the
// dynamic informer), starts them, and waits for cache sync.
func buildSyncedProvider(t *testing.T, objs []*unstructured.Unstructured) *Provider {
	t.Helper()
	var full, meta []*unstructured.Unstructured
	for _, u := range objs {
		if metadataKinds[u.GetKind()] {
			meta = append(meta, u)
		} else {
			full = append(full, u)
		}
	}
	return buildProvider(t, full, meta)
}

// buildProvider wires the Provider with an EXPLICIT split: full objects go through the
// dynamic informer, meta objects through the metadata informer (PartialObjectMetadata) —
// regardless of kind, so a test can route, e.g., a Pod metadata-only to prove the
// metadata projection still carries the labels selector binding needs.
func buildProvider(t *testing.T, full, meta []*unstructured.Unstructured) *Provider {
	t.Helper()
	p, _, _ := buildProviderC(t, full, meta)
	return p
}

// buildProviderC is buildProvider but also returns the fake clients, so a test can
// inspect the recorded actions (e.g. assert FreshGet hit the dynamic/metadata client and
// the hot path issued no GET).
func buildProviderC(t *testing.T, full, meta []*unstructured.Unstructured) (*Provider, *dynamicfake.FakeDynamicClient, *metadatafake.FakeMetadataClient) {
	t.Helper()
	gvrToListKind := map[schema.GroupVersionResource]string{}
	var dynObjs, metaObjs []runtime.Object
	var fullTargets, metaTargets []cluster.Target
	seenFull, seenMeta := map[schema.GroupVersionResource]bool{}, map[schema.GroupVersionResource]bool{}

	for _, u := range full {
		kind := u.GetKind()
		gvr, ok := corpusGVR[kind]
		if !ok {
			t.Fatalf("no GVR mapping for kind %q", kind)
		}
		gvk := closure.GVK{Group: gvr.Group, Version: gvr.Version, Kind: kind}
		dynObjs = append(dynObjs, u)
		if !seenFull[gvr] {
			fullTargets = append(fullTargets, cluster.Target{GVR: gvr, GVK: gvk, Namespaced: !corpusClusterScoped[kind]})
			gvrToListKind[gvr] = kind + "List"
			seenFull[gvr] = true
		}
	}
	for _, u := range meta {
		kind := u.GetKind()
		gvr, ok := corpusGVR[kind]
		if !ok {
			t.Fatalf("no GVR mapping for kind %q", kind)
		}
		gvk := closure.GVK{Group: gvr.Group, Version: gvr.Version, Kind: kind}
		metaObjs = append(metaObjs, toPOM(t, u, gvk))
		if !seenMeta[gvr] {
			metaTargets = append(metaTargets, cluster.Target{GVR: gvr, GVK: gvk, Namespaced: !corpusClusterScoped[kind]})
			seenMeta[gvr] = true
		}
	}

	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, dynObjs...)
	// AddMetaToScheme registers PartialObjectMetadata{,List} so the fake's ObjectTracker
	// recognises the type; it then maps each object to its real GVR via the stamped
	// TypeMeta (UnsafeGuessKindToResource on v1/Secret → secrets, v1/ConfigMap → configmaps).
	metaScheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(metaScheme); err != nil {
		t.Fatalf("AddMetaToScheme: %v", err)
	}
	metaClient := metadatafake.NewSimpleMetadataClient(metaScheme, metaObjs...)
	dynFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, 0)
	metaFactory := metadatainformer.NewSharedInformerFactory(metaClient, 0)

	p, err := newProvider(dynFactory, fullTargets, metaFactory, metaTargets, fakeScope{}, objectGetter{dyn: dynClient, meta: metaClient})
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	p.Start(ctx)
	if !p.WaitForCacheSync(ctx) {
		t.Fatalf("cache did not sync")
	}
	return p, dynClient, metaClient
}

// uobj builds an unstructured object for the focused informer tests.
func uobj(apiVersion, kind, ns, name, uid string, mut ...func(map[string]any)) *unstructured.Unstructured {
	meta := map[string]any{"name": name}
	if ns != "" {
		meta["namespace"] = ns
	}
	if uid != "" {
		meta["uid"] = uid
	}
	m := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": meta}
	for _, f := range mut {
		f(m)
	}
	return &unstructured.Unstructured{Object: m}
}

func withLabels(l map[string]string) func(map[string]any) {
	return func(m map[string]any) {
		lm := map[string]any{}
		for k, v := range l {
			lm[k] = v
		}
		m["metadata"].(map[string]any)["labels"] = lm
	}
}

// withServiceSelector sets a Service's flat spec.selector map (Services do not use the
// matchLabels form).
func withServiceSelector(sel map[string]string) func(map[string]any) {
	return func(m map[string]any) {
		s := map[string]any{}
		for k, v := range sel {
			s[k] = v
		}
		m["spec"] = map[string]any{"selector": s}
	}
}

func withFinalizers(fs ...string) func(map[string]any) {
	return func(m map[string]any) {
		out := make([]any, len(fs))
		for i, f := range fs {
			out[i] = f
		}
		m["metadata"].(map[string]any)["finalizers"] = out
	}
}

func refIdent(r closure.Ref) string {
	if r.UID != "" {
		return "uid:" + r.UID
	}
	return r.String()
}

func refSet(rs []closure.Ref) map[string]bool {
	m := make(map[string]bool, len(rs))
	for _, r := range rs {
		m[refIdent(r)] = true
	}
	return m
}

// assertSameRefSet compares two Ref slices order-insensitively by identity.
func assertSameRefSet(t *testing.T, label string, got, want []closure.Ref) {
	t.Helper()
	g, w := refSet(got), refSet(want)
	if len(g) != len(w) {
		t.Errorf("%s: got %d refs %v, want %d refs %v", label, len(g), keys(g), len(w), keys(w))
		return
	}
	for k := range w {
		if !g[k] {
			t.Errorf("%s: missing %q (got %v, want %v)", label, k, keys(g), keys(w))
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
