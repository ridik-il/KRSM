// Package archguard holds architectural invariants enforced as tests. It has no
// production code; the constraints live entirely in *_test.go.
package archguard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestClosureIsStdlibOnly enforces ADR-0002/ADR-0005 and DESIGN §7: the public,
// embeddable `closure` SDK must not depend on anything under k8s.io/ (or any
// other non-stdlib module). client-go/apimachinery types leaking through its API
// would break embeddability and force every importer to take the Kubernetes
// dependency. The label-selector model (ADR-0005) exists precisely so selectors
// can be followed faithfully without importing k8s.io/apimachinery/pkg/labels.
func TestClosureIsStdlibOnly(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/ridik-il/krsm/closure/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	const ownModule = "github.com/ridik-il/krsm"
	for _, dep := range strings.Fields(string(out)) {
		// A non-stdlib import path has a dot in its first segment (a domain).
		first := dep
		if i := strings.IndexByte(dep, '/'); i >= 0 {
			first = dep[:i]
		}
		if !strings.Contains(first, ".") {
			continue // standard library
		}
		if strings.HasPrefix(dep, ownModule) {
			continue // our own packages are fine
		}
		t.Errorf("closure SDK must be stdlib-only, but depends on %q", dep)
	}
}
