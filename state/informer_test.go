package state

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ridik-il/krsm/closure"
)

// TestSecretCachedMetadataOnly proves C3: a Secret reaches the cache through the
// metadata informer, so the cached projection carries its labels/finalizers but the
// Secret's `data` is never fetched or cached. (closure.Object has no data field at all,
// so data is *structurally* incapable of being cached — this test pins that the metadata
// path still populates the relation fields the closure needs.)
func TestSecretCachedMetadataOnly(t *testing.T) {
	secret := uobj("v1", "Secret", "prod", "db-creds", "uid:Secret/prod/db-creds",
		withLabels(map[string]string{"app": "db"}),
		withFinalizers("krsm.io/guard"),
		func(m map[string]any) { m["data"] = map[string]any{"password": "c3VwZXItc2VjcmV0"} }, // must NOT reach cache
	)
	p := buildProvider(t, nil, []*unstructured.Unstructured{secret})

	o, ok := p.Get(closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Secret"}, Namespace: "prod", Name: "db-creds"})
	if !ok {
		t.Fatal("Secret not indexed via the metadata informer")
	}
	if o.Labels["app"] != "db" {
		t.Errorf("cached Secret labels = %v, want app=db (metadata must carry labels)", o.Labels)
	}
	if len(o.Finalizers) != 1 || o.Finalizers[0] != "krsm.io/guard" {
		t.Errorf("cached Secret finalizers = %v, want [krsm.io/guard]", o.Finalizers)
	}
	// closure.Object has no Data field — data cannot be cached by construction.
}

// TestMetadataPodBindsServiceSelector proves a metadata-only object still participates
// in label-selector binding (PartialObjectMetadata carries labels): a Pod delivered via
// the metadata informer is bound by a Service's selector.
func TestMetadataPodBindsServiceSelector(t *testing.T) {
	svc := uobj("v1", "Service", "prod", "web", "uid:Service/prod/web", withServiceSelector(map[string]string{"app": "web"}))
	pod := uobj("v1", "Pod", "prod", "web-1", "uid:Pod/prod/web-1", withLabels(map[string]string{"app": "web"}))

	// Route the Pod through the METADATA informer (full=[svc], meta=[pod]).
	p := buildProvider(t, []*unstructured.Unstructured{svc}, []*unstructured.Unstructured{pod})

	podRef := closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Pod"}, Namespace: "prod", Name: "web-1"}
	svcRef := closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Service"}, Namespace: "prod", Name: "web"}
	assertSameRefSet(t, "SelectorsTargeting(metadata pod)", p.SelectorsTargeting(podRef), []closure.Ref{
		{GVK: closure.GVK{Version: "v1", Kind: "Service"}, Namespace: "prod", Name: "web", UID: "uid:Service/prod/web"},
	})
	got := p.PodsSelectedBy(svcRef)
	assertSameRefSet(t, "PodsSelectedBy(service)", got, []closure.Ref{
		{GVK: closure.GVK{Version: "v1", Kind: "Pod"}, Namespace: "prod", Name: "web-1", UID: "uid:Pod/prod/web-1"},
	})
}

// TestStateNoWriteVerb is the read-only source-guard, mirroring
// internal/cluster.TestPackageInvokesNoWriteVerbs: no mutating client verb call site may
// appear in the state package source. The Provider only get/list/watches.
func TestStateNoWriteVerb(t *testing.T) {
	writeCall := regexp.MustCompile(`\.(Create|Update|UpdateStatus|Patch|Delete|DeleteCollection|Apply|ApplyStatus)\s*\(`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if loc := writeCall.FindIndex(src); loc != nil {
			t.Errorf("%s invokes a write verb at byte %d — state/ must be read-only", name, loc[0])
		}
	}
}
