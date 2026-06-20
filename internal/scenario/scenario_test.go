package scenario

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ridik-il/krsm/closure"
)

// scenarioDir returns the path to a golden scenario directory under the closure
// package's testdata, relative to this package.
func scenarioDir(name string) string {
	return filepath.Join("..", "..", "closure", "testdata", "scenarios", name)
}

// TestLoadBuildsCheckableInputs proves the extracted loader produces inputs that
// closure.Safe can act on: scenario 01 (the cascade) must come back as a Block.
func TestLoadBuildsCheckableInputs(t *testing.T) {
	sc, err := Load(scenarioDir("01-memory-pressure-cascade"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dec := closure.Safe(sc.State, sc.Action, sc.Scope)
	if dec.Verdict != closure.Block {
		t.Errorf("verdict = %s, want Block", dec.Verdict)
	}
}

// TestParseClusterRejectsUnknownOperator: a selector with an operator outside the
// four set-based operators must be rejected, not silently parsed into a selector
// that binds nothing. Silently binding nothing would drop a real
// NetworkPolicy/workload binding from the closure — a missed escape, the failure
// direction a safety gate must never take. Fail closed (error → exit 1) instead.
func TestParseClusterRejectsUnknownOperator(t *testing.T) {
	bad := "apiVersion: networking.k8s.io/v1\n" +
		"kind: NetworkPolicy\n" +
		"metadata: {name: np, namespace: prod}\n" +
		"spec:\n" +
		"  podSelector:\n" +
		"    matchExpressions:\n" +
		"      - {key: tier, operator: Exist}\n"
	if _, err := parseCluster([]byte(bad)); err == nil {
		t.Fatal("parseCluster(unknown operator) = nil error, want rejection")
	}
}

// TestClusterScopedKindsExcludesNamespaced is a safety invariant guard: a
// namespaced kind must never appear in clusterScopedKinds. If one did, nsOf would
// resolve it to namespace "", excluding it from its namespace's containment — so a
// Namespace delete would silently miss it (a false negative). Over-inclusion of a
// cluster-scoped kind is merely conservative; this direction is unsafe.
func TestClusterScopedKindsExcludesNamespaced(t *testing.T) {
	for _, kind := range []string{
		"Pod", "ConfigMap", "Secret", "Service", "Deployment", "ReplicaSet",
		"StatefulSet", "DaemonSet", "PersistentVolumeClaim", "NetworkPolicy",
		"PodDisruptionBudget", "Role", "RoleBinding", "Endpoints",
	} {
		if clusterScopedKinds[kind] {
			t.Errorf("%q is namespaced but is listed as cluster-scoped — unsafe (would escape namespace containment)", kind)
		}
	}
}

// TestLoadErrors covers the failure contract: a missing directory and malformed
// cluster YAML both surface as errors rather than a half-built Scenario.
func TestLoadErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Error("Load(missing dir) = nil error, want an error")
	}

	dir := t.TempDir()
	for _, f := range []string{"cluster.yaml", "request.yaml", "scope.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("[unterminated"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	if _, err := Load(dir); err == nil {
		t.Error("Load(malformed yaml) = nil error, want an error")
	}
}
