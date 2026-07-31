package state

import (
	"sync"
	"testing"

	"github.com/ridik-il/krsm/closure"
)

func ref(group, version, kind, ns, name, uid string) closure.Ref {
	return closure.Ref{GVK: closure.GVK{Group: group, Version: version, Kind: kind}, Namespace: ns, Name: name, UID: uid}
}

// TestConsumersAndControllersTargetingScaleTargetSplit pins the RefScaleTarget split:
// Consumers returns non-scaleTarget cross-ref consumers only; ControllersTargeting
// returns scaleTarget consumers only — matching scanState exactly.
func TestConsumersAndControllersTargetingScaleTargetSplit(t *testing.T) {
	ix := newIndex()
	dep := ref("apps", "v1", "Deployment", "prod", "web", "uid-dep")
	cfg := ref("", "v1", "ConfigMap", "prod", "cfg", "uid-cfg")
	hpa := ref("autoscaling", "v2", "HorizontalPodAutoscaler", "prod", "web-hpa", "uid-hpa")
	pod := ref("", "v1", "Pod", "prod", "web-1", "uid-pod")

	ix.upsert(closure.Object{Ref: dep})
	ix.upsert(closure.Object{Ref: cfg})
	ix.upsert(closure.Object{Ref: hpa, CrossRefs: []closure.CrossRef{
		{Kind: closure.RefScaleTarget, Ref: ref("apps", "v1", "Deployment", "prod", "web", "")},
	}})
	ix.upsert(closure.Object{Ref: pod, CrossRefs: []closure.CrossRef{
		{Kind: closure.RefVolume, Ref: ref("", "v1", "ConfigMap", "prod", "cfg", "")},
	}})

	if got := ix.consumers(dep); len(got) != 0 {
		t.Errorf("Consumers(Deployment) = %v, want none (scaleTarget excluded)", got)
	}
	assertSameRefSet(t, "ControllersTargeting(Deployment)", ix.controllersTargeting(dep), []closure.Ref{hpa})
	assertSameRefSet(t, "Consumers(ConfigMap)", ix.consumers(cfg), []closure.Ref{pod})
	if got := ix.controllersTargeting(cfg); len(got) != 0 {
		t.Errorf("ControllersTargeting(ConfigMap) = %v, want none", got)
	}
}

// TestNamespaceContentsExcludesNamespaceKind: a Namespace object is not its own
// content; namespaced objects in it are.
func TestNamespaceContentsExcludesNamespaceKind(t *testing.T) {
	ix := newIndex()
	nsObj := ref("", "v1", "Namespace", "", "prod", "uid-ns")
	pod := ref("", "v1", "Pod", "prod", "web-1", "uid-pod")
	ix.upsert(closure.Object{Ref: nsObj})
	ix.upsert(closure.Object{Ref: pod})

	assertSameRefSet(t, "NamespaceContents(prod)", ix.namespaceContents("prod"), []closure.Ref{pod})
}

// TestEmptySelectorKindAwareness: an empty (present) Service selector binds nothing; an
// empty NetworkPolicy selector binds every pod in the namespace — reusing
// closure.SelectorBinds through the index.
func TestEmptySelectorKindAwareness(t *testing.T) {
	ix := newIndex()
	pod := ref("", "v1", "Pod", "prod", "web-1", "uid-pod")
	ix.upsert(closure.Object{Ref: pod, Labels: map[string]string{"app": "web"}})

	empty := closure.LabelSelector{MatchLabels: map[string]string{}} // present-but-empty {}
	svc := ref("", "v1", "Service", "prod", "svc", "uid-svc")
	netpol := ref("networking.k8s.io", "v1", "NetworkPolicy", "prod", "np", "uid-np")
	ix.upsert(closure.Object{Ref: svc, Selector: empty})
	ix.upsert(closure.Object{Ref: netpol, Selector: empty})

	if got := ix.podsSelectedBy(svc); len(got) != 0 {
		t.Errorf("empty Service selector should bind nothing, got %v", got)
	}
	assertSameRefSet(t, "empty NetworkPolicy selector binds all ns pods", ix.podsSelectedBy(netpol), []closure.Ref{pod})
}

// TestUpdateAndDeleteMaintainIndexes: an add→update(owner/labels change)→delete leaves
// every index consistent (no stale edges).
func TestUpdateAndDeleteMaintainIndexes(t *testing.T) {
	ix := newIndex()
	pod := ref("", "v1", "Pod", "prod", "web-1", "uid-pod")
	rsOld := ref("apps", "v1", "ReplicaSet", "prod", "web-old", "uid-rs-old")
	rsNew := ref("apps", "v1", "ReplicaSet", "prod", "web-new", "uid-rs-new")
	// OwnedChildren resolves the owner object first (scanState semantics), so the owners
	// must be indexed too.
	ix.upsert(closure.Object{Ref: rsOld})
	ix.upsert(closure.Object{Ref: rsNew})

	ix.upsert(closure.Object{Ref: pod, Labels: map[string]string{"app": "web"},
		Owners: []closure.OwnerRef{{Kind: "ReplicaSet", Name: "web-old", UID: "uid-rs-old"}}})
	assertSameRefSet(t, "owner old", ix.ownedChildren(rsOld), []closure.Ref{pod})

	// Update: re-parent and relabel.
	ix.upsert(closure.Object{Ref: pod, Labels: map[string]string{"app": "db"},
		Owners: []closure.OwnerRef{{Kind: "ReplicaSet", Name: "web-new", UID: "uid-rs-new"}}})
	if got := ix.ownedChildren(ref("apps", "v1", "ReplicaSet", "prod", "web-old", "uid-rs-old")); len(got) != 0 {
		t.Errorf("stale owner edge after update: %v", got)
	}
	assertSameRefSet(t, "owner new", ix.ownedChildren(ref("apps", "v1", "ReplicaSet", "prod", "web-new", "uid-rs-new")), []closure.Ref{pod})
	if o, ok := ix.get(pod); !ok || o.Labels["app"] != "db" {
		t.Errorf("update did not replace labels: %+v ok=%v", o, ok)
	}

	// Delete: every trace gone.
	ix.remove(objKey(pod))
	if _, ok := ix.get(pod); ok {
		t.Error("Get after delete should miss")
	}
	// Only the pod is gone; the two ReplicaSets remain.
	assertSameRefSet(t, "NamespaceContents after pod delete", ix.namespaceContents("prod"), []closure.Ref{rsOld, rsNew})
	if got := ix.ownedChildren(ref("apps", "v1", "ReplicaSet", "prod", "web-new", "uid-rs-new")); len(got) != 0 {
		t.Errorf("stale owner edge after delete: %v", got)
	}
}

// TestConcurrentReadsDuringHandlerWrites exercises the RWMutex under -race: many
// readers while writers upsert/remove. Correctness is asserted by `go test -race`.
func TestConcurrentReadsDuringHandlerWrites(t *testing.T) {
	ix := newIndex()
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				r := ref("", "v1", "Pod", "prod", "p", "uid-pod")
				ix.upsert(closure.Object{Ref: r, Labels: map[string]string{"app": "web"},
					Owners: []closure.OwnerRef{{Kind: "ReplicaSet", Name: "rs", UID: "uid-rs"}}})
				if i%2 == 0 {
					ix.remove(objKey(r))
				}
			}
		}()
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				_, _ = ix.get(ref("", "v1", "Pod", "prod", "p", "uid-pod"))
				_ = ix.namespaceContents("prod")
				_ = ix.ownedChildren(ref("apps", "v1", "ReplicaSet", "prod", "rs", "uid-rs"))
				_ = ix.podsMatching("prod", closure.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, "ReplicaSet")
			}
		}()
	}
	wg.Wait()

	// Final deterministic state: one upsert, then the object must be present.
	r := ref("", "v1", "Pod", "prod", "p", "uid-pod")
	ix.upsert(closure.Object{Ref: r})
	if _, ok := ix.get(r); !ok {
		t.Fatal("index inconsistent after concurrent access")
	}
}
