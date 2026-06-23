package cluster

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/internal/scenario"
)

// This file turns the golden corpus into a true parity oracle for the unstructured
// index builder. Rather than re-assert a single hand-built scenario, it:
//
//  1. reads a scenario's cluster.yaml and re-parses it into []unstructured.Unstructured,
//     injecting deterministic uids and ownerReferences[].uid that EXACTLY reproduce the
//     YAML loader's uidOf synthesis (uid:Kind/ns/name) — so the builder's real-uid path
//     produces byte-identical owner edges and cross-ref uids to internal/scenario;
//  2. feeds them through BuildObjects → closure.NewScanState (the builder's State);
//  3. takes the SAME action + scope the golden runner uses, via scenario.Load (so the
//     test harness's action/scope are guaranteed identical to internal/scenario_test);
//  4. runs the SAME closure.Safe(state, action, scope) and asserts the verdict +
//     closure/escaping/external sets equal the golden's expected.yaml.
//
// If the unstructured extractor diverges from the YAML loader on ANY relation, the
// resulting closure differs and the parity assertion fails — before a cluster is touched.

// corpusClusterScopedKinds mirrors internal/scenario.clusterScopedKinds: the static
// scope table the loader uses to namespace objects. The converter resolves namespaces
// through a ScopeInfo built from this set so the builder's Ref.Namespace (and therefore
// the injected uids) match the loader's nsOf exactly.
var corpusClusterScopedKinds = map[string]bool{
	"Namespace":                      true,
	"Node":                           true,
	"PersistentVolume":               true,
	"ClusterRole":                    true,
	"ClusterRoleBinding":             true,
	"StorageClass":                   true,
	"PriorityClass":                  true,
	"CustomResourceDefinition":       true,
	"IngressClass":                   true,
	"APIService":                     true,
	"ValidatingWebhookConfiguration": true,
	"MutatingWebhookConfiguration":   true,
	"RuntimeClass":                   true,
}

var corpusDocSep = regexp.MustCompile(`(?m)^---\s*$`)

// corpusNsOf reproduces internal/scenario.nsOf: cluster-scoped → "", else default "".
func corpusNsOf(kind, ns string) string {
	if corpusClusterScopedKinds[kind] {
		return ""
	}
	if ns == "" {
		return "default"
	}
	return ns
}

// corpusUIDOf reproduces internal/scenario.uidOf so the converter injects the SAME uid
// the loader synthesises; the builder then carries it as a real uid and the engine's
// uid-keyed owner/cross-ref matching is identical across both paths.
func corpusUIDOf(kind, ns, name string) string {
	return fmt.Sprintf("uid:%s/%s/%s", kind, ns, name)
}

// corpusStripComments drops comment-only lines, mirroring internal/scenario.stripComments
// so a doc that is only a comment block is treated as empty (skipped).
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

// clusterYAMLToUnstructured re-parses a scenario's cluster.yaml into unstructured
// objects with loader-equivalent uids injected. It splits documents and skips
// comment-only / kindless docs the same way internal/scenario.parseCluster does, so the
// object set is identical to the loader's, differing only in representation
// (unstructured map vs typed struct) and in carrying REAL uids the builder consumes.
func clusterYAMLToUnstructured(t *testing.T, raw []byte) []unstructured.Unstructured {
	t.Helper()
	var out []unstructured.Unstructured
	for _, doc := range corpusDocSep.Split(string(raw), -1) {
		if strings.TrimSpace(corpusStripComments(doc)) == "" {
			continue
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("convert manifest: %v\n%s", err, doc)
		}
		kind, _ := m["kind"].(string)
		if kind == "" {
			continue // mirror parseCluster: a kindless doc is skipped
		}
		meta, _ := m["metadata"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
			m["metadata"] = meta
		}
		name, _ := meta["name"].(string)
		nsRaw, _ := meta["namespace"].(string)
		resolvedNS := corpusNsOf(kind, nsRaw)

		// Inject the loader-equivalent real uid for this object.
		meta["uid"] = corpusUIDOf(kind, resolvedNS, name)

		// Inject ownerReferences[].uid using the child's resolved namespace, exactly as
		// the loader does (uidOf(o.Kind, ns, o.Name) with the child's ns). The builder's
		// OwnedChildren then matches the child to its parent by this real uid.
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
		out = append(out, unstructured.Unstructured{Object: m})
	}
	return out
}

// corpusScope returns a ScopeInfo driven by the same static cluster-scoped set the
// loader uses, so the builder resolves namespaces identically to nsOf.
func corpusScope() ScopeInfo {
	return fakeScope{clusterScoped: corpusClusterScopedKinds}
}

// runCorpusParity is the reusable parity assertion: build the unstructured State for a
// scenario, run closure.Safe with the loader's own action+scope, and compare to the
// golden expected.yaml. The verdict and all three result sets must match.
func runCorpusParity(t *testing.T, name string) {
	t.Helper()
	dir := filepath.Join("..", "..", "closure", "testdata", "scenarios", name)

	// Action + scope come from the loader itself, guaranteeing the harness inputs are
	// byte-identical to the golden runner's; only the State differs (builder vs loader).
	sc, err := scenario.Load(dir)
	if err != nil {
		t.Fatalf("scenario.Load(%s): %v", name, err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "cluster.yaml"))
	if err != nil {
		t.Fatalf("read cluster.yaml: %v", err)
	}
	objs, err := BuildObjects(clusterYAMLToUnstructured(t, raw), corpusScope())
	if err != nil {
		t.Fatalf("BuildObjects(%s): %v", name, err)
	}
	state := closure.NewScanState(objs)

	got := closure.Safe(state, sc.Action, sc.Scope)

	exp := loadGolden(t, dir)
	if got.Verdict.String() != exp.Verdict {
		t.Errorf("%s: verdict = %s, want %s", name, got.Verdict, exp.Verdict)
	}
	assertSet(t, name+" closure", got.Closure, exp.Closure)
	assertSet(t, name+" escaping", got.Escaping, exp.Escaping)
	assertSet(t, name+" external", got.External, exp.External)
}

// TestCorpusConverterMatchesLoaderObjectSet guards the parity oracle against a vacuous
// pass: it proves the generic converter feeds the builder the SAME object set the loader
// produces, identity-for-identity. For each scenario it builds the unstructured State and
// the loader's State (via scenario.Load), then asserts every built object's real-uid Ref
// is Get-able in the loader's State and the counts match. If the converter silently
// dropped or duplicated an object (which could make a simple verdict coincidentally
// match), this fails — so a green TestCorpusParity genuinely exercises every object.
func TestCorpusConverterMatchesLoaderObjectSet(t *testing.T) {
	for _, name := range []string{
		"01-memory-pressure-cascade",
		"06-scale-fights-hpa-pdb",
		"14-cluster-scoped-pv-not-contained",
		"16-projected-secret",
		"05-namespace-blast-radius",
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("..", "..", "closure", "testdata", "scenarios", name)
			sc, err := scenario.Load(dir)
			if err != nil {
				t.Fatalf("scenario.Load: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "cluster.yaml"))
			if err != nil {
				t.Fatalf("read cluster.yaml: %v", err)
			}
			built, err := BuildObjects(clusterYAMLToUnstructured(t, raw), corpusScope())
			if err != nil {
				t.Fatalf("BuildObjects: %v", err)
			}
			// Every built object resolves in the loader's State by its (injected, loader-
			// equivalent) uid Ref — same identity, same namespace resolution.
			for _, o := range built {
				if _, ok := sc.State.Get(o.Ref); !ok {
					t.Errorf("built object %s not present in loader State (converter/loader object-set divergence)", o.Ref.String())
				}
			}
			// And the converter dropped nothing: its object count equals an independent
			// recount of the kindful, non-comment docs in cluster.yaml — the same set
			// internal/scenario.parseCluster builds the loader State from. Built ⊆ loader
			// (above) plus equal doc counts gives built == loader on identities.
			if got, want := len(built), countKindfulDocs(raw); got != want {
				t.Errorf("converter built %d objects, cluster.yaml has %d kindful docs — converter dropped/added objects", got, want)
			}
		})
	}
}

// countKindfulDocs counts the YAML documents in raw that carry a kind, applying the same
// doc-split + comment-strip rules as internal/scenario.parseCluster and the converter, so
// it is an independent yardstick for "objects the loader would build".
func countKindfulDocs(raw []byte) int {
	n := 0
	for _, doc := range corpusDocSep.Split(string(raw), -1) {
		if strings.TrimSpace(corpusStripComments(doc)) == "" {
			continue
		}
		var m map[string]any
		if yaml.Unmarshal([]byte(doc), &m) != nil {
			continue
		}
		if k, _ := m["kind"].(string); k != "" {
			n++
		}
	}
	return n
}

func loadGolden(t *testing.T, dir string) expectedVerdict {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "expected.yaml"))
	if err != nil {
		t.Fatalf("read golden expected.yaml: %v", err)
	}
	var exp expectedVerdict
	if err := yaml.Unmarshal(b, &exp); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	return exp
}

// TestCorpusParity drives the unstructured builder against the golden corpus through the
// generic converter. Each listed scenario asserts that BuildObjects → closure.Safe
// reproduces the YAML loader's verdict and closure/escaping/external sets exactly.
//
// Coverage rationale: every scenario whose verdict is decided by a relation the SLICE-1
// builder extracts (owner edges, namespace containment, label/expression selectors,
// every cross-ref kind, scaleTargetRef, finalizers) is included. Pure scope-parsing or
// load-error scenarios that exercise no builder relation are covered by their note in
// TestCorpusParitySkips.
func TestCorpusParity(t *testing.T) {
	for _, name := range []string{
		"01-memory-pressure-cascade",          // owner cascade + Service selector
		"02-label-rebind-breaks-routing",      // label mutation vs Service selector
		"03-pvc-delete-data-loss",             // PVC volume cross-ref → consumers
		"04-shared-secret-rotation",           // shared Secret cross-ref fan-out
		"05-namespace-blast-radius",           // namespace containment
		"06-scale-fights-hpa-pdb",             // scaleTargetRef + PDB selector
		"07-shared-configmap-rollout",         // shared ConfigMap cross-ref
		"08-networkpolicy-widening",           // NetworkPolicy podSelector (empty {})
		"09-finalizer-orphans-external",       // finalizers → external effects
		"10-same-name-wrong-tenant",           // namespace-scoped identity
		"12-workload-update-recreates-pods",   // workload selector → pods
		"13-in-scope-cascade-allowed",         // owner cascade, all in scope → Allow
		"14-cluster-scoped-pv-not-contained",  // cluster-scoped PV excluded
		"15-initcontainer-configmap",          // initContainer env/volume cross-ref
		"16-projected-secret",                 // projected volume secret source
		"17-imagepullsecret",                  // imagePullSecrets cross-ref
		"18-matchexpressions-precise-binding", // matchExpressions selector
		"19-ephemeralcontainer-configmap",     // ephemeralContainer cross-ref
		"11-unknown-target-fail-closed",       // target not in tracked state → deny
		"20-scope-selector-precision",         // selector-dim scope over built objects
		"21-taskcontract-selector-scope",      // taskcontract scope over built objects
		"23-namespace-scope",                  // namespace-dim scope over built objects
		"24-ownership-scope",                  // ownership-tree scope over owner edges
		"25-ownership-escape",                 // ownership scope escape via owner edges
		"26-derived-default",                  // derived (Level-0) scope over built objects
	} {
		t.Run(name, func(t *testing.T) {
			runCorpusParity(t, name)
		})
	}
}

// TestCorpusParitySkips documents the corpus scenarios deliberately excluded from the
// parity oracle and proves the reason holds: both are NEGATIVE goldens whose
// expected.yaml carries a loadError (scenario.Load must FAIL at scope/taskcontract
// parse time), so there is no verdict to compare and the failure is in the scope path,
// not the SLICE-1 cluster builder. We assert Load fails for each, keeping the skip
// honest rather than silent.
func TestCorpusParitySkips(t *testing.T) {
	for _, name := range []string{
		"22-taskcontract-fail-closed",             // unsupported scope dimension → Load error
		"27-ownership-scope-conflicting-identity", // malformed ownership clause → Load error
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("..", "..", "closure", "testdata", "scenarios", name)
			if _, err := scenario.Load(dir); err == nil {
				t.Fatalf("%s: scenario.Load = nil error, want a load failure (negative golden)", name)
			}
		})
	}
}
