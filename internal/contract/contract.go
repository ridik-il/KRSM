// Package contract parses the TaskContract wire form into the scope package's Go
// TaskContract. It is the single parse path for every source of a contract: the
// scenario loader's taskcontract.yaml on disk and the JSON form of a TaskContract CR
// read from the cluster. JSON is a subset of YAML 1.2, so one YAML decoder serves
// both — an unstructured CR marshalled to JSON parses here unchanged.
//
// It lives under internal/ for the same reason internal/scenario does: the public
// scope and closure packages are stdlib-only and embeddable (ADR-0002/ADR-0005,
// enforced by internal/archguard), so the YAML dependency must not be reachable from
// them. scope.Compile takes the struct, never bytes.
//
// Parse is STRUCTURALLY STRICT: an unknown field is an error, not a silent drop. A
// contract is attacker-influenced input (an agent references it to claim a scope), and
// scope.Compile — the fail-closed authority on meaning — cannot reject a field a lax
// parser has already discarded. A typo'd `namesapce:` must fail loudly rather than
// compile to a clause that quietly matches something else. Parse does NOT semantically
// validate: it shapes the struct (resolving selectors and defaulting namespaces) and
// leaves every judgement about dimensions and clause consistency to scope.Compile.
package contract

import (
	"encoding/json"
	"fmt"

	"sigs.k8s.io/yaml"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/scope"
)

// rawTaskContract mirrors the TaskContract wire form (DESIGN §6) — the same shape the
// krsm.io/v1alpha1 CR carries.
type rawTaskContract struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   rawMetadata `json:"metadata"`
	Spec       rawSpec     `json:"spec"`
}

// rawMetadata is the contract's identity: the only two metadata fields a compiled
// scope needs.
type rawMetadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type rawSpec struct {
	Allow       []rawAllowClause `json:"allow"`
	MaxSeverity string           `json:"maxSeverity"`
}

// rawAllowClause is one spec.allow entry. Every key the wire form may carry is
// declared here — including the selector's matchLabels/matchExpressions and the
// ownership dimension's root — because strict decoding rejects anything undeclared.
// (An earlier shape read the selector out-of-band from the raw clause bytes; under
// DisallowUnknownFields that would reject the corpus's own selector contract.)
type rawAllowClause struct {
	Dim              string            `json:"dim"`
	GVK              rawRef            `json:"gvk"`
	Namespace        string            `json:"namespace"`
	Name             string            `json:"name"`
	MatchLabels      map[string]string `json:"matchLabels"`      // Dim == selector
	MatchExpressions []rawMatchExpr    `json:"matchExpressions"` // Dim == selector
	Root             *rawRef           `json:"root"`             // Dim == ownership
}

// rawRef is a group/version/kind (+ namespace/name for a root) reference.
type rawRef struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// rawMatchExpr is one set-based selector requirement.
type rawMatchExpr struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values"`
}

// Parse maps the AUTHORED TaskContract wire form — a taskcontract.yaml on disk, or any
// document nobody but its author has written to — to a scope.TaskContract, rejecting
// unknown fields ANYWHERE in the document. It shapes the struct only; every semantic
// judgement — apiVersion, kind, dimension support, clause consistency, maxSeverity —
// belongs to scope.Compile, which fails closed.
//
// Use ParseCR for an object read from a cluster: a stored object carries fields the API
// server owns, which this decode rejects by design.
func Parse(raw []byte) (scope.TaskContract, error) {
	var rtc rawTaskContract
	if err := yaml.UnmarshalStrict(raw, &rtc); err != nil {
		return scope.TaskContract{}, fmt.Errorf("parse taskcontract: %w", err)
	}
	return rtc.taskContract()
}

// ParseCR maps a TaskContract CR as an API SERVER returns it — the JSON an unstructured
// object marshals to — to a scope.TaskContract.
//
// It exists because Parse's whole-document strictness is unsatisfiable for a stored
// object: every one carries server-owned ObjectMeta the wire form does not declare
// (creationTimestamp — which ObjectMeta emits even as null — uid, resourceVersion,
// generation, managedFields) and, when the CRD has a status subresource, a top-level
// status. Decoding that strictly rejects EVERY real TaskContract, which would leave the
// Level-3 channel denying every request it was built to authorise. The failure is
// fail-closed, so it hides rather than announces itself.
//
// Strictness is kept exactly where it earns its keep: `spec` is decoded strictly, because
// that is the attacker-influenced half — an agent authors the contract it then references,
// and scope.Compile cannot reject a field a lax parser already discarded. The relaxed part
// carries nothing that can widen a scope: only name and namespace are read from metadata,
// and apiVersion/kind are still handed to Compile, which fails closed on either being
// wrong. At the top level the API server's own schema pruning is the strict gate.
func ParseCR(raw []byte) (scope.TaskContract, error) {
	// Pass 1, NON-strict: identity and the untouched spec bytes. Unknown members of
	// metadata (and a status subresource) are the server's, and are ignored.
	var cr struct {
		APIVersion string          `json:"apiVersion"`
		Kind       string          `json:"kind"`
		Metadata   rawMetadata     `json:"metadata"`
		Spec       json.RawMessage `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &cr); err != nil {
		return scope.TaskContract{}, fmt.Errorf("parse taskcontract CR: %w", err)
	}
	// Pass 2, STRICT: the spec, the half an agent authors. An absent spec decodes to the
	// zero value and is left to Compile, which rejects a contract that authorises nothing.
	rtc := rawTaskContract{APIVersion: cr.APIVersion, Kind: cr.Kind, Metadata: cr.Metadata}
	if len(cr.Spec) > 0 {
		if err := yaml.UnmarshalStrict(cr.Spec, &rtc.Spec); err != nil {
			return scope.TaskContract{}, fmt.Errorf("parse taskcontract CR spec: %w", err)
		}
	}
	return rtc.taskContract()
}

// taskContract lowers the decoded wire form to the scope package's TaskContract. It is
// shared by both parse paths so a contract read from a file and the same contract read
// from the cluster can never compile to different scopes.
func (rtc rawTaskContract) taskContract() (scope.TaskContract, error) {
	allow := make([]scope.AllowClause, 0, len(rtc.Spec.Allow))
	for _, rc := range rtc.Spec.Allow {
		ac := scope.AllowClause{
			Dim:       closure.ScopeDim(rc.Dim),
			GVK:       closure.GVK{Group: rc.GVK.Group, Version: rc.GVK.Version, Kind: rc.GVK.Kind},
			Namespace: clauseNamespace(rc),
			Name:      rc.Name,
		}
		sel, err := rc.selector()
		if err != nil {
			return scope.TaskContract{}, fmt.Errorf("parse allow clause selector: %w", err)
		}
		ac.Selector = sel
		if rc.Root != nil {
			rootNS := rootNamespace(rc.Root.Kind, rc.Root.Namespace, rtc.Metadata.Namespace)
			ac.Root = closure.Ref{
				GVK:       closure.GVK{Group: rc.Root.Group, Version: rc.Root.Version, Kind: rc.Root.Kind},
				Namespace: rootNS,
				Name:      rc.Root.Name,
				UID:       SyntheticUID(rc.Root.Kind, rootNS, rc.Root.Name),
			}
		}
		allow = append(allow, ac)
	}

	return scope.TaskContract{
		APIVersion: rtc.APIVersion,
		Kind:       rtc.Kind,
		Metadata:   scope.Metadata{Name: rtc.Metadata.Name, Namespace: rtc.Metadata.Namespace},
		Spec:       scope.Spec{Allow: allow, MaxSeverity: scope.Severity(rtc.Spec.MaxSeverity)},
	}, nil
}

// rootNamespace resolves an ownership root's namespace. A root that names no namespace
// lives in the CONTRACT's own namespace when the contract has one — a TaskContract CR
// read from the cluster always does, and "the root beside me" is what an author who
// omitted the field meant. Only when the contract itself is namespace-less (an offline
// corpus file, which has no namespace to inherit) does NamespaceFor's "default" apply.
//
// The alternative — always defaulting to "default" — is a silent mis-resolution for the
// live path: a root in a `prod` CR would build the key Kind/default/name, miss, and
// authorise an empty subtree. That direction is fail-closed rather than dangerous, but it
// is a confusing deny for a contract whose author named a root that plainly exists, and
// the same-namespace rule already pins a contract's namespace to the request's.
// NamespaceFor still wins for a cluster-scoped kind, which has no namespace to inherit.
func rootNamespace(kind, declared, contractNamespace string) string {
	if declared == "" {
		declared = contractNamespace
	}
	return NamespaceFor(kind, declared)
}

// clauseNamespace resolves a clause's namespace field, and applies the loader's
// namespace defaulting ONLY to the dimensions where that field is an object gate.
//
// For a resource or selector clause the GVK+namespace pair gates candidate objects,
// so it must default exactly as the objects themselves do (NamespaceFor) or a clause
// omitting the namespace would gate on "" and match nothing.
//
// For the ownership dimension the clause-level namespace must stay EMPTY — identity
// lives on Root, and closure.ScopeClause.Validate rejects a clause-level namespace.
// Defaulting it to "default" would turn every ownership contract into a structural
// error. For the namespace dimension the field is not a gate but the SUBJECT of the
// grant: defaulting an absent one to "default" would silently authorise the whole
// default namespace where Validate is supposed to reject the clause as malformed.
// Both must therefore pass through literally and let Compile judge them.
func clauseNamespace(rc rawAllowClause) string {
	switch closure.ScopeDim(rc.Dim) {
	case "", closure.DimResource, closure.DimSelector:
		return NamespaceFor(rc.GVK.Kind, rc.Namespace)
	default:
		return rc.Namespace
	}
}

// selector builds the clause's authorisation LabelSelector. A present-but-empty
// selector collapses back to the NIL selector: apimachinery reads `{}` as "matches
// all", but for an *authorisation* selector that would be a silent namespace-wide
// over-grant, and the engine treats the nil selector as match-nothing (fail-safe,
// DESIGN §5). An unrecognised operator is rejected rather than allowed to fall
// through to "matches nothing", so a typo cannot silently narrow the granted scope.
func (rc rawAllowClause) selector() (closure.LabelSelector, error) {
	sel := closure.LabelSelector{MatchLabels: rc.MatchLabels}
	for _, e := range rc.MatchExpressions {
		op := closure.SelectorOperator(e.Operator)
		if !op.Valid() {
			return closure.LabelSelector{}, fmt.Errorf("invalid selector operator %q for key %q (want In, NotIn, Exists or DoesNotExist)", e.Operator, e.Key)
		}
		sel.MatchExpressions = append(sel.MatchExpressions, closure.SelectorRequirement{
			Key:      e.Key,
			Operator: op,
			Values:   e.Values,
		})
	}
	if len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0 {
		return closure.LabelSelector{}, nil
	}
	return sel, nil
}
