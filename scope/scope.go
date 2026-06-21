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

// ScopePredicate is a compiled TaskContract: the clauses closure.Safe consumes,
// plus the (unenforced) maxSeverity. Callers pass predicate.Clauses to Safe.
type ScopePredicate struct {
	Clauses     []closure.ScopeClause
	MaxSeverity Severity
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
	return ScopePredicate{Clauses: clauses, MaxSeverity: tc.Spec.MaxSeverity}, nil
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
