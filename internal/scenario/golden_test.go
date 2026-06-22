package scenario

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ridik-il/krsm/closure"
)

// expectedVerdict is the asserted outcome stored in a scenario's expected.yaml.
type expectedVerdict struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"` // optional: asserted as a substring when set
	// LoadError, when non-empty, marks a NEGATIVE golden: Load(dir) must FAIL and
	// its error message must contain this substring (fail-closed proof, ADR-0009).
	// The verdict/closure/escaping/external fields are then unused — there is no
	// computable closure for a contract that never compiles.
	LoadError string     `json:"loadError"`
	Closure   []humanRef `json:"closure"`
	Escaping  []humanRef `json:"escaping"`
	External  []humanRef `json:"external"`
}

// humanRef is the Kind/namespace/name identity used in golden files (uid-free).
type humanRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (h humanRef) key() string {
	return h.Kind + "/" + h.Namespace + "/" + h.Name
}

func scenariosRoot() string {
	return filepath.Join("..", "..", "closure", "testdata", "scenarios")
}

func loadExpected(t *testing.T, dir string) expectedVerdict {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "expected.yaml"))
	if err != nil {
		t.Fatalf("read expected.yaml: %v", err)
	}
	var exp expectedVerdict
	if err := yaml.Unmarshal(b, &exp); err != nil {
		t.Fatalf("parse expected: %v", err)
	}
	return exp
}

// TestScenarios runs every golden scenario through the shared loader and
// closure.Safe, asserting the closure, escaping set, external set, and verdict.
// It is the regression guard that the loader extraction preserved behavior.
func TestScenarios(t *testing.T) {
	entries, err := os.ReadDir(scenariosRoot())
	if err != nil {
		t.Fatalf("read scenarios: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(scenariosRoot(), e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			// expected.yaml is the source of truth for whether this is a negative
			// case, so load it first. A non-empty loadError flips the scenario from
			// "assert a verdict" to "assert Load fails closed".
			exp := loadExpected(t, dir)
			if exp.LoadError != "" {
				_, err := Load(dir)
				if err == nil {
					t.Fatalf("Load: expected error containing %q, got nil", exp.LoadError)
				}
				if !strings.Contains(err.Error(), exp.LoadError) {
					t.Fatalf("Load error = %q, want it to contain %q", err.Error(), exp.LoadError)
				}
				return
			}
			sc, err := Load(dir)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := closure.Safe(sc.State, sc.Action, sc.Scope)

			if got.Verdict.String() != exp.Verdict {
				t.Errorf("verdict = %s, want %s", got.Verdict, exp.Verdict)
			}
			if exp.Reason != "" && !strings.Contains(got.Reason, exp.Reason) {
				t.Errorf("reason = %q, want it to contain %q", got.Reason, exp.Reason)
			}
			assertSet(t, "closure", got.Closure, exp.Closure)
			assertSet(t, "escaping", got.Escaping, exp.Escaping)
			assertSet(t, "external", got.External, exp.External)
		})
	}
}

func assertSet(t *testing.T, label string, got []closure.Ref, want []humanRef) {
	t.Helper()
	gotKeys := make([]string, 0, len(got))
	for _, r := range got {
		gotKeys = append(gotKeys, r.String())
	}
	wantKeys := make([]string, 0, len(want))
	for _, w := range want {
		wantKeys = append(wantKeys, w.key())
	}
	sort.Strings(gotKeys)
	sort.Strings(wantKeys)
	if !equalStrings(gotKeys, wantKeys) {
		t.Errorf("%s set mismatch\n got: %v\nwant: %v", label, gotKeys, wantKeys)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestClosureBoundedByInventory asserts the decidability property |C| ≤ |R|
// across every scenario (DESIGN §4, §8). |R| is the parsed object inventory,
// obtained via this package's own parseCluster so no closure internals leak.
func TestClosureBoundedByInventory(t *testing.T) {
	entries, err := os.ReadDir(scenariosRoot())
	if err != nil {
		t.Fatalf("read scenarios: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(scenariosRoot(), e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			// A negative golden (loadError) has no computable closure — Load fails
			// by design — so the |C| ≤ |R| bound is undefined. Skip before requiring
			// parseCluster/Load to succeed.
			if loadExpected(t, dir).LoadError != "" {
				return
			}
			raw, err := os.ReadFile(filepath.Join(dir, "cluster.yaml"))
			if err != nil {
				t.Fatalf("read cluster.yaml: %v", err)
			}
			objs, err := parseCluster(raw)
			if err != nil {
				t.Fatalf("parseCluster: %v", err)
			}
			sc, err := Load(dir)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := len(closure.Closure(sc.State, sc.Action)); got > len(objs) {
				t.Errorf("|C| = %d exceeds |R| = %d", got, len(objs))
			}
		})
	}
}
