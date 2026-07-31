package webhook

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWebhookNoWriteVerb is the read-only source-guard for the webhook package, mirroring
// state.TestStateNoWriteVerb and internal/cluster's guard (umbrella design §Security
// names both state/ and webhook/). A ValidatingWebhook decides admission; it must never
// invoke a mutating client verb — no non-test source in this package may call one.
func TestWebhookNoWriteVerb(t *testing.T) {
	writeCall := regexp.MustCompile(`\.(Create|Update|UpdateStatus|Patch|Delete|DeleteCollection|Apply|ApplyStatus)\s*\(`)
	// scope.Mode.Apply(dec) applies the audit/enforce verdict to a Decision — it is not a
	// client-go server-side Apply. Neutralise it so the guard stays about cluster writes.
	verdictApply := regexp.MustCompile(`\bmode\.Apply\s*\(`)
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
		src = verdictApply.ReplaceAll(src, []byte("mode.applyVerdict("))
		if loc := writeCall.FindIndex(src); loc != nil {
			t.Errorf("%s invokes a write verb at byte %d — webhook/ must not mutate the cluster", name, loc[0])
		}
	}
}
