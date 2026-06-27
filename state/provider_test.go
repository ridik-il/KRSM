package state

import (
	"testing"

	"github.com/ridik-il/krsm/closure"
)

// TestProviderImplementsState is the compile-time guard that the informer-backed
// Provider satisfies the SAME closure.State interface the engine consumes, so it
// drops in beside closure.NewScanState without any engine change (DESIGN §4/§7).
func TestProviderImplementsState(_ *testing.T) {
	var _ closure.State = (*Provider)(nil)
}
