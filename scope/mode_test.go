package scope_test

import (
	"testing"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/scope"
)

// escapeBlock is a scope-escape Block: the closure was computed and one member
// escaped scope (len(Escaping) > 0). This is the decision audit mode softens.
func escapeBlock() closure.Decision {
	svc := closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Service"}, Namespace: "prod", Name: "web-svc"}
	dep := closure.Ref{GVK: closure.GVK{Group: "apps", Version: "v1", Kind: "Deployment"}, Namespace: "prod", Name: "web"}
	return closure.Decision{
		Verdict:  closure.Block,
		Reason:   "affected-resource closure escapes task scope",
		Closure:  []closure.Ref{dep, svc},
		Escaping: []closure.Ref{svc},
	}
}

// Test 1: ModeAudit downgrades a scope-escape Block (len(Escaping) > 0) to Warn,
// preserving the Escaping/Closure detail so the report still shows what would block.
func TestApplyAuditDowngradesEscapeBlockToWarn(t *testing.T) {
	got := scope.ModeAudit.Apply(escapeBlock())

	if got.Verdict != closure.Warn {
		t.Errorf("audit verdict = %s, want Warn (escape Block downgraded)", got.Verdict)
	}
	if len(got.Escaping) != 1 || got.Escaping[0].Name != "web-svc" {
		t.Errorf("audit dropped the escaping detail: %v", got.Escaping)
	}
	if len(got.Closure) != 2 {
		t.Errorf("audit dropped the closure: %v", got.Closure)
	}
}

// failClosedBlock is a fail-closed Block: the closure could not be computed (target
// unresolvable), so Escaping is empty. Audit must NOT soften this (DESIGN §5).
func failClosedBlock() closure.Decision {
	return closure.Decision{
		Verdict: closure.Block,
		Reason:  "fail-closed: action target not found in tracked state; closure cannot be computed",
	}
}

func allowDecision() closure.Decision {
	return closure.Decision{Verdict: closure.Allow}
}

func warnDecision() closure.Decision {
	ext := closure.Ref{GVK: closure.GVK{Kind: "External"}, Namespace: "prod", Name: "lb"}
	return closure.Decision{Verdict: closure.Warn, Reason: "crosses cluster boundary", External: []closure.Ref{ext}}
}

// Test 2-4: Mode.Apply across every (mode, verdict) combination that is NOT the
// audit-escape downgrade (Test 1). Audit leaves a fail-closed Block, Allow, and Warn
// unchanged; enforce leaves every decision — including a scope-escape Block —
// unchanged.
func TestApplyLeavesDecisionUnchanged(t *testing.T) {
	tests := []struct {
		name string
		mode scope.Mode
		dec  closure.Decision
	}{
		{"audit: fail-closed Block stays Block", scope.ModeAudit, failClosedBlock()},
		{"audit: Allow passes through", scope.ModeAudit, allowDecision()},
		{"audit: Warn passes through", scope.ModeAudit, warnDecision()},
		{"enforce: scope-escape Block unchanged", scope.ModeEnforce, escapeBlock()},
		{"enforce: fail-closed Block unchanged", scope.ModeEnforce, failClosedBlock()},
		{"enforce: Allow unchanged", scope.ModeEnforce, allowDecision()},
		{"enforce: Warn unchanged", scope.ModeEnforce, warnDecision()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.dec
			got := tt.mode.Apply(tt.dec)
			if got.Verdict != want.Verdict {
				t.Errorf("verdict = %s, want %s (unchanged)", got.Verdict, want.Verdict)
			}
			if got.Reason != want.Reason {
				t.Errorf("reason = %q, want %q (unchanged)", got.Reason, want.Reason)
			}
		})
	}
}
