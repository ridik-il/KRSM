// Package scope is the user-facing scope contract and its compiler. It defines a
// Go TaskContract (the declarative, agent-referenced authorised scope of ADR-0003
// and DESIGN §6) and Compile, which lowers that contract to a ScopePredicate — the
// dimension-typed closure.ScopeClause values closure.Safe consumes.
//
// Like closure, scope is public, embeddable, and stdlib-only: it imports only the
// closure package and the standard library. Parsing the TaskContract's YAML wire
// form is a loader concern (internal/scenario), so an embedding agent-builder pulls
// in no YAML dependency. Compile takes the struct, not bytes.
//
// Compile fails closed: an unrecognised apiVersion/kind, an unsupported scope
// dimension, a structurally invalid clause, or an unknown maxSeverity is a hard
// error rather than a silently narrowed (allow-nothing) or partial scope.
package scope

import (
	"fmt"

	"github.com/ridik-il/krsm/closure"
)

// TaskContract is the declarative, agent-referenced authorised scope (ADR-0003,
// DESIGN §6) as a Go value. Parsing its YAML wire form is a loader concern; this
// package compiles the struct, so the embeddable SDK pulls in no YAML dependency.
type TaskContract struct {
	APIVersion string // must be "krsm.io/v1"
	Kind       string // must be "TaskContract"
	Metadata   Metadata
	Spec       Spec
}

// Metadata is the contract's name/namespace identity.
type Metadata struct {
	Name      string
	Namespace string
}

// Spec is the authorised scope: a list of allow-clauses and the maximum severity
// the task may incur.
type Spec struct {
	Allow       []AllowClause
	MaxSeverity Severity
}

// AllowClause is one declared scope dimension (the wire shape). Exactly one
// dimension's fields are meaningful, chosen by Dim — resource and selector now;
// ownership/namespace/reference fields are added by later slices.
type AllowClause struct {
	Dim       closure.ScopeDim // "resource" | "selector"
	GVK       closure.GVK
	Namespace string
	Name      string                // Dim == resource
	Selector  closure.LabelSelector // Dim == selector (matchLabels + matchExpressions)
}

// Severity mirrors DESIGN §6 maxSeverity. It is carried on the compiled predicate
// but NOT enforced in this slice (severity B is a later concern); it is validated
// against the known set so a typo is caught at compile.
type Severity string

const (
	SeverityNone     Severity = ""
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// valid reports whether s is one of the known severities (including the empty
// SeverityNone). Compile rejects any other value so a typo is caught now.
func (s Severity) valid() bool {
	switch s {
	case SeverityNone, SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical:
		return true
	default:
		return false
	}
}

// Provenance records how a ScopePredicate's scope was obtained, so the verdict
// report can distinguish a declared scope from a synthesized one (ADR-0011). It does
// not affect the closure decision — only how the operator is told the scope arose.
type Provenance string

const (
	// ProvenanceContract marks a scope lowered from a declared TaskContract by Compile
	// (ADR-0009): a human (or agent) authored the allow-clauses.
	ProvenanceContract Provenance = "contract"
	// ProvenanceDerivedOwner marks a Level-0 scope synthesized by Derive from the
	// action's target alone — the target's ownership tree — with no declared scope.
	ProvenanceDerivedOwner Provenance = "derived:ownership-tree"
)

// ScopePredicate is a compiled TaskContract: the clauses closure.Safe consumes,
// plus the (unenforced) maxSeverity and the Provenance of the scope. Callers pass
// predicate.Clauses to Safe.
type ScopePredicate struct {
	Clauses     []closure.ScopeClause
	MaxSeverity Severity
	Provenance  Provenance
}

// Derive synthesizes a Level-0 ScopePredicate from an action target that has no
// declared scope (ADR-0011): a single ownership clause rooted at the target, covering
// the target plus everything it transitively owns. It is deliberately NOT OR'd with a
// namespace clause — under the union semantics of C ⊆ scope a "everything in the
// target's namespace" clause would re-admit the same-namespace collateral the
// ownership tree is meant to flag (it would Allow scenario 01), and it is redundant
// besides (a cross-namespace member is already outside the tree).
//
// Derive is pure and stdlib-only: the state-dependence of the ownership dimension
// lives in the engine's matchScope (it walks the live owner→child relation), exactly
// as for the selector dimension — so Derive needs no State and stays embeddable.
func Derive(target closure.Ref) ScopePredicate {
	return ScopePredicate{
		Clauses:    []closure.ScopeClause{closure.OwnershipClause(target)},
		Provenance: ProvenanceDerivedOwner,
	}
}

// Mode governs how a scope-escape verdict is reported (ADR-0011), applied above the
// unchanged closure.Safe. ModeAudit is the install default: a scope-escape Block is
// surfaced as a Warn (never a deny) so a day-0 false positive does not get KRSM
// uninstalled; ModeEnforce is opt-in and leaves the verdict as Safe decided it.
type Mode string

const (
	// ModeAudit downgrades a scope-escape Block to Warn (the install default).
	ModeAudit Mode = "audit"
	// ModeEnforce returns Safe's decision unchanged.
	ModeEnforce Mode = "enforce"
)

// Apply maps a decision for the mode, above the unchanged closure.Safe. In ModeAudit
// a scope-escape Block (len(Escaping) > 0 — the closure was computed and a member
// escaped) is downgraded to Warn, preserving Escaping/Closure/External and rewriting
// Reason to name the audit downgrade, so the report still shows what would block. A
// fail-closed Block (len(Escaping) == 0 — the closure could not be computed, DESIGN
// §5) is NOT softened: an unbounded blast radius stays a deny even in audit.
// ModeEnforce returns the decision unchanged.
func (m Mode) Apply(d closure.Decision) closure.Decision {
	if m != ModeAudit {
		return d
	}
	if d.Verdict != closure.Block || len(d.Escaping) == 0 {
		return d
	}
	d.Verdict = closure.Warn
	d.Reason = "audit: " + d.Reason + " (would Block under --mode enforce)"
	return d
}

// Compile lowers a TaskContract to a ScopePredicate, or fails closed. It errors on
// an unrecognised apiVersion/kind, an unsupported scope dimension, an invalid
// clause (closure.ScopeClause.Validate), or an unknown maxSeverity — an
// uncompilable contract must never silently yield an empty (allow-nothing) or
// partial scope.
func Compile(tc TaskContract) (ScopePredicate, error) {
	const (
		wantAPIVersion = "krsm.io/v1"
		wantKind       = "TaskContract"
	)
	if tc.APIVersion != wantAPIVersion {
		return ScopePredicate{}, fmt.Errorf("unrecognised apiVersion %q (want %q)", tc.APIVersion, wantAPIVersion)
	}
	if tc.Kind != wantKind {
		return ScopePredicate{}, fmt.Errorf("unrecognised kind %q (want %q)", tc.Kind, wantKind)
	}
	if !tc.Spec.MaxSeverity.valid() {
		return ScopePredicate{}, fmt.Errorf("unknown maxSeverity %q", tc.Spec.MaxSeverity)
	}

	clauses := make([]closure.ScopeClause, 0, len(tc.Spec.Allow))
	for _, ac := range tc.Spec.Allow {
		clause, err := compileClause(ac)
		if err != nil {
			return ScopePredicate{}, err
		}
		clauses = append(clauses, clause)
	}
	return ScopePredicate{Clauses: clauses, MaxSeverity: tc.Spec.MaxSeverity, Provenance: ProvenanceContract}, nil
}

// compileClause lowers one AllowClause to a closure.ScopeClause, failing closed on
// an unsupported dimension or a structurally inconsistent clause. The compiler is
// the one place that knows the dim→clause mapping; adding a dimension later is a new
// case here plus new AllowClause fields. An empty Dim is read as resource for parity
// with closure's back-compat default; any other value (a typo, or a not-yet-built
// ownership/namespace/reference) is a hard error so the author learns immediately
// rather than getting a silently narrowed scope.
//
// The clause is built preserving every field the author declared, then run through
// closure.ScopeClause.Validate — so an inconsistent contract (e.g. a selector clause
// also carrying a name) is surfaced rather than silently dropped by a constructor.
func compileClause(ac AllowClause) (closure.ScopeClause, error) {
	switch ac.Dim {
	case "", closure.DimResource, closure.DimSelector:
	default:
		return closure.ScopeClause{}, fmt.Errorf("unsupported scope dimension %q", ac.Dim)
	}
	clause := closure.ScopeClause{
		Dim:       ac.Dim,
		GVK:       ac.GVK,
		Namespace: ac.Namespace,
		Name:      ac.Name,
		Selector:  ac.Selector,
	}
	if err := clause.Validate(); err != nil {
		return closure.ScopeClause{}, fmt.Errorf("invalid allow clause: %w", err)
	}
	return clause, nil
}
