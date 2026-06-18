package closure

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestScenarios runs every golden scenario under testdata/scenarios and asserts
// the closure, the escaping set, the external set, and the verdict.
func TestScenarios(t *testing.T) {
	root := filepath.Join("testdata", "scenarios")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read scenarios: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			sc := loadScenario(t, dir)
			got := Safe(sc.state, sc.action, sc.scope)

			if got.Verdict.String() != sc.expected.Verdict {
				t.Errorf("verdict = %s, want %s", got.Verdict, sc.expected.Verdict)
			}
			if sc.expected.Reason != "" && !strings.Contains(got.Reason, sc.expected.Reason) {
				t.Errorf("reason = %q, want it to contain %q", got.Reason, sc.expected.Reason)
			}
			assertSet(t, "closure", got.Closure, sc.expected.Closure)
			assertSet(t, "escaping", got.Escaping, sc.expected.Escaping)
			assertSet(t, "external", got.External, sc.expected.External)
		})
	}
}

func assertSet(t *testing.T, label string, got []Ref, want []humanRef) {
	t.Helper()
	gotKeys := make([]string, 0, len(got))
	for _, r := range got {
		gotKeys = append(gotKeys, r.human())
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
// across every scenario (DESIGN §4, §8).
func TestClosureBoundedByInventory(t *testing.T) {
	root := filepath.Join("testdata", "scenarios")
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			sc := loadScenario(t, filepath.Join(root, e.Name()))
			ss := sc.state.(*scanState)
			if got := len(Closure(sc.state, sc.action)); got > len(ss.objs) {
				t.Errorf("|C| = %d exceeds |R| = %d", got, len(ss.objs))
			}
		})
	}
}

// TestClosureTerminatesOnCyclicOwners asserts the BFS terminates and stays
// bounded when ownerReferences form a cycle (adversarial input, DESIGN §8).
func TestClosureTerminatesOnCyclicOwners(t *testing.T) {
	a := Object{Ref: Ref{GVK: GVK{Version: "v1", Kind: "Widget"}, Namespace: "x", Name: "a", UID: "uid:a"},
		Owners: []OwnerRef{{Kind: "Widget", Name: "b", UID: "uid:b"}}}
	b := Object{Ref: Ref{GVK: GVK{Version: "v1", Kind: "Widget"}, Namespace: "x", Name: "b", UID: "uid:b"},
		Owners: []OwnerRef{{Kind: "Widget", Name: "a", UID: "uid:a"}}}
	st := NewScanState([]Object{a, b})

	done := make(chan []Ref, 1)
	go func() {
		done <- Closure(st, Action{Verb: Delete, Cascade: true, Target: a.Ref})
	}()
	select {
	case c := <-done:
		if len(c) > 2 {
			t.Errorf("|C| = %d exceeds |R| = 2 on cyclic owners", len(c))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Closure did not terminate on cyclic ownerReferences")
	}
}
