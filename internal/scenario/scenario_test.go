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
