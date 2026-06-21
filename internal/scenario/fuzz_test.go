package scenario

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzParseManifests fuzzes the three manifest parsers behind Load. They consume
// cluster/request/scope YAML that originates outside KRSM, so the invariant under
// test is robustness: for ANY input bytes each parser must return cleanly (a
// value or an error) and never panic. Correctness on well-formed input is covered
// by the golden tests (TestLoadBuildsCheckableInputs et al.); this guards the
// failure direction a safety gate must never get wrong.
func FuzzParseManifests(f *testing.F) {
	// Seed from the real golden manifests so the engine starts from valid,
	// structurally diverse YAML and mutates outward.
	seedRoot := filepath.Join("..", "..", "closure", "testdata", "scenarios")
	if entries, err := os.ReadDir(seedRoot); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			for _, name := range []string{"cluster.yaml", "request.yaml", "scope.yaml"} {
				if b, err := os.ReadFile(filepath.Join(seedRoot, e.Name(), name)); err == nil {
					f.Add(b)
				}
			}
		}
	}
	// Bare seeds so the corpus is non-empty even without testdata present.
	f.Add([]byte(""))
	f.Add([]byte("---\n"))
	f.Add([]byte("apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\n"))

	f.Fuzz(func(_ *testing.T, raw []byte) {
		// Any (result, error) pair is acceptable; a panic is not. Each parser
		// sees the same arbitrary bytes — they share the document-splitting and
		// YAML-to-JSON path, so all three must tolerate malformed input.
		_, _ = parseCluster(raw)
		_, _ = parseAction(raw)
		_, _ = parseScope(raw)
	})
}
