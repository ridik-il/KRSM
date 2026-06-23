// Package scenario loads a KRSM golden scenario — cluster.yaml, request.yaml and
// scope.yaml — into the closure types so it can be checked by closure.Safe.
//
// It is a dev/demo concern, not part of the embeddable SDK: it is the only place
// (outside tests) that depends on sigs.k8s.io/yaml, keeping the public closure
// package stdlib-only. Both the krsm CLI and the closure golden tests use it, so
// there is a single loader rather than two copies.
package scenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/scope"
)

// Scenario is a runnable check input: the three things closure.Safe needs, plus the
// human-readable provenance of the scope for the verdict report.
type Scenario struct {
	State  closure.State
	Action closure.Action
	Scope  []closure.ScopeClause
	// ScopeSource is the human provenance of Scope for the SCOPE line — one of
	// scopeSource* below ("taskcontract.yaml", "scope.yaml", "derived (ownership-tree)").
	ScopeSource string
}

// scopeSource* are the human-readable provenances reported on the SCOPE line. They
// mirror scope.Provenance but are phrased for the operator (the CLI prints them; the
// scope package's ProvenanceDerivedOwner is the machine form).
const (
	scopeSourceContract  = "taskcontract.yaml"
	scopeSourceScopeYAML = "scope.yaml"
	scopeSourceDerived   = "derived (ownership-tree)"
)

// Load reads cluster.yaml, request.yaml and scope.yaml from dir and builds a
// Scenario. It deliberately does not read expected.yaml — that is a test-only
// assertion artifact. It returns an error if a file is missing or malformed.
func Load(dir string) (*Scenario, error) {
	read := func(f string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, err
		}
		return b, nil
	}

	clusterRaw, err := read("cluster.yaml")
	if err != nil {
		return nil, err
	}
	objs, err := parseCluster(clusterRaw)
	if err != nil {
		return nil, err
	}

	requestRaw, err := read("request.yaml")
	if err != nil {
		return nil, err
	}
	action, err := parseAction(requestRaw)
	if err != nil {
		return nil, err
	}

	scopeClauses, source, err := loadScope(read, action.Target)
	if err != nil {
		return nil, err
	}

	return &Scenario{
		State:       closure.NewScanState(objs),
		Action:      action,
		Scope:       scopeClauses,
		ScopeSource: source,
	}, nil
}

// loadScope resolves a scenario's authorised scope and its human provenance, in
// descending order of declaredness (ADR-0011 progressive scope):
//
//  1. taskcontract.yaml → parseTaskContract → scope.Compile — the declared-contract path.
//  2. scope.yaml → parseScope — the legacy explicit-clause path (pre-v0.3 scenarios).
//  3. neither file present → scope.Derive(target) — the Level-0 derived default: a
//     single ownership clause rooted at the action target, the zero-config verdict.
//
// errors.Is(err, os.ErrNotExist) distinguishes "file absent, fall through" from a real
// read error for BOTH files, so a genuine I/O failure is never silently treated as
// "no scope" — and the check survives any future error-wrapping in read. target is the
// parsed action target Load already holds; it is only consulted on the derived path.
func loadScope(read func(string) ([]byte, error), target closure.Ref) ([]closure.ScopeClause, string, error) {
	contractRaw, err := read("taskcontract.yaml")
	if err == nil {
		tc, perr := parseTaskContract(contractRaw)
		if perr != nil {
			return nil, "", perr
		}
		pred, cerr := scope.Compile(tc)
		if cerr != nil {
			return nil, "", fmt.Errorf("compile taskcontract: %w", cerr)
		}
		return pred.Clauses, scopeSourceContract, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}

	scopeRaw, err := read("scope.yaml")
	if err == nil {
		clauses, perr := parseScope(scopeRaw)
		if perr != nil {
			return nil, "", perr
		}
		return clauses, scopeSourceScopeYAML, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}

	// No declared scope at all: synthesize the Level-0 ownership tree of the target.
	return scope.Derive(target).Clauses, scopeSourceDerived, nil
}

// --- raw manifest parsing ---------------------------------------------------

var docSep = regexp.MustCompile(`(?m)^---\s*$`)

type rawManifest struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   rawMetadata `json:"metadata"`
	Spec       rawSpec     `json:"spec"`
}

type rawMetadata struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	Labels          map[string]string `json:"labels"`
	Finalizers      []string          `json:"finalizers"`
	OwnerReferences []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"ownerReferences"`
}

// rawPodSpec is the subset of a pod spec the cross-reference relation reads. It
// appears both at the top level (bare Pods) and under spec.template.spec
// (workload pod templates) — the latter is where real workloads actually declare
// their ConfigMap/Secret/PVC consumption. Parsing only the top level would make
// every workload's mounts invisible.
type rawVolume struct {
	ConfigMap *struct {
		Name string `json:"name"`
	} `json:"configMap"`
	Secret *struct {
		SecretName string `json:"secretName"`
	} `json:"secret"`
	PVC *struct {
		ClaimName string `json:"claimName"`
	} `json:"persistentVolumeClaim"`
	Projected *struct {
		Sources []struct {
			ConfigMap *struct {
				Name string `json:"name"`
			} `json:"configMap"`
			Secret *struct {
				Name string `json:"name"`
			} `json:"secret"`
		} `json:"sources"`
	} `json:"projected"`
}

type rawContainer struct {
	EnvFrom []struct {
		ConfigMapRef *struct {
			Name string `json:"name"`
		} `json:"configMapRef"`
		SecretRef *struct {
			Name string `json:"name"`
		} `json:"secretRef"`
	} `json:"envFrom"`
	Env []struct {
		ValueFrom *struct {
			ConfigMapKeyRef *struct {
				Name string `json:"name"`
			} `json:"configMapKeyRef"`
			SecretKeyRef *struct {
				Name string `json:"name"`
			} `json:"secretKeyRef"`
		} `json:"valueFrom"`
	} `json:"env"`
}

type rawPodSpec struct {
	Volumes             []rawVolume    `json:"volumes"`
	Containers          []rawContainer `json:"containers"`
	InitContainers      []rawContainer `json:"initContainers"`
	EphemeralContainers []rawContainer `json:"ephemeralContainers"`
	ImagePullSecrets    []struct {
		Name string `json:"name"`
	} `json:"imagePullSecrets"`
}

type rawSpec struct {
	Selector       json.RawMessage `json:"selector"`    // Service: map; workload/PDB: {matchLabels}
	PodSelector    json.RawMessage `json:"podSelector"` // NetworkPolicy: {matchLabels}
	ScaleTargetRef *struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"scaleTargetRef"`
	rawPodSpec // bare-Pod top-level volumes/containers (promoted: spec.volumes, spec.containers)
	Template   *struct {
		Spec rawPodSpec `json:"spec"`
	} `json:"template"` // workload pod template: spec.template.spec.{volumes,containers}
}

func gvkOf(apiVersion, kind string) closure.GVK {
	g := closure.GVK{Kind: kind}
	if parts := strings.SplitN(apiVersion, "/", 2); len(parts) == 2 {
		g.Group, g.Version = parts[0], parts[1]
	} else {
		g.Version = apiVersion
	}
	return g
}

// clusterScopedKinds are the standard Kubernetes kinds that exist outside any
// namespace. A cluster-scoped object resolves to namespace "" regardless of input,
// so it is never counted as the contents of a namespace and matches scope clauses
// on "". Custom cluster-scoped CRDs need live discovery and are deferred to v0.4;
// YAML cannot distinguish an absent namespace from an explicit empty one.
//
// SAFETY INVARIANT: only add a kind here if it is *definitely* cluster-scoped. A
// namespaced kind listed here would resolve to namespace "" and so escape its
// namespace's containment — a Namespace delete would silently miss it (a false
// negative, the one error class a safety gate must not have). Over-inclusion of a
// genuinely cluster-scoped kind is merely conservative; under-scoping is unsafe.
// This static map stands in for API-discovery/RESTMapper scope until v0.4 reads it
// live. Guarded by TestClusterScopedKindsExcludesNamespaced.
var clusterScopedKinds = map[string]bool{
	"Namespace":                      true,
	"Node":                           true,
	"PersistentVolume":               true,
	"ClusterRole":                    true,
	"ClusterRoleBinding":             true,
	"StorageClass":                   true,
	"PriorityClass":                  true,
	"CustomResourceDefinition":       true,
	"IngressClass":                   true,
	"APIService":                     true,
	"ValidatingWebhookConfiguration": true,
	"MutatingWebhookConfiguration":   true,
	"RuntimeClass":                   true,
}

func nsOf(kind, ns string) string {
	if clusterScopedKinds[kind] {
		return ""
	}
	if ns == "" {
		return "default"
	}
	return ns
}

func uidOf(kind, ns, name string) string {
	return fmt.Sprintf("uid:%s/%s/%s", kind, ns, name)
}

// selectorFrom resolves the flattened selector for a kind: Service uses a flat
// map, NetworkPolicy uses spec.podSelector.matchLabels, others use
// spec.selector.matchLabels. A present-but-empty selector is a non-nil empty map
// (matches all); an absent selector is nil.
func selectorFrom(kind string, spec rawSpec) (closure.LabelSelector, error) {
	switch kind {
	case "Service":
		if spec.Selector == nil {
			return closure.LabelSelector{}, nil // absent → binds nothing
		}
		m := map[string]string{}
		if err := json.Unmarshal(spec.Selector, &m); err != nil {
			return closure.LabelSelector{}, fmt.Errorf("parse Service selector: %w", err)
		}
		return closure.LabelSelector{MatchLabels: m}, nil
	case "NetworkPolicy":
		return matchLabels(spec.PodSelector)
	default:
		return matchLabels(spec.Selector)
	}
}

// matchLabels resolves a `{matchLabels, matchExpressions}` selector wrapper. An
// absent wrapper yields the nil selector (binds nothing); a wrapper carrying
// neither field yields a non-nil empty map (present-empty → kind decides).
// matchExpressions are captured so set-based requirements (In/NotIn/Exists/
// DoesNotExist) bind precisely rather than collapsing to the empty selector.
func matchLabels(raw json.RawMessage) (closure.LabelSelector, error) {
	if raw == nil {
		return closure.LabelSelector{}, nil
	}
	var wrap struct {
		MatchLabels      map[string]string `json:"matchLabels"`
		MatchExpressions []struct {
			Key      string   `json:"key"`
			Operator string   `json:"operator"`
			Values   []string `json:"values"`
		} `json:"matchExpressions"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return closure.LabelSelector{}, fmt.Errorf("parse selector: %w", err)
	}
	sel := closure.LabelSelector{MatchLabels: wrap.MatchLabels}
	for _, e := range wrap.MatchExpressions {
		op := closure.SelectorOperator(e.Operator)
		// Reject an unrecognised operator instead of letting it fall through to
		// matches (which would treat it as "matches nothing"): for a binding
		// object that silently drops a real binding from the closure — a missed
		// escape. Fail closed at the parse boundary.
		if !op.Valid() {
			return closure.LabelSelector{}, fmt.Errorf("invalid selector operator %q for key %q (want In, NotIn, Exists or DoesNotExist)", e.Operator, e.Key)
		}
		sel.MatchExpressions = append(sel.MatchExpressions, closure.SelectorRequirement{
			Key:      e.Key,
			Operator: op,
			Values:   e.Values,
		})
	}
	if wrap.MatchLabels == nil && len(sel.MatchExpressions) == 0 {
		sel.MatchLabels = map[string]string{} // present but empty → matches all
	}
	return sel, nil
}

// scopeSelectorFrom parses a scope/allow clause's selector, then collapses a
// present-but-empty selector back to the nil selector. matchLabels turns `{}` into a
// non-nil empty map (apimachinery "matches all"), but for an *authorisation* selector
// that would be a silent namespace-wide over-grant; the engine treats the nil
// selector as match-nothing (fail-safe, DESIGN §5). Both the legacy scope.yaml path
// (parseScope) and the TaskContract path (parseTaskContract) need this identical
// collapse, so it lives in one place.
func scopeSelectorFrom(raw json.RawMessage) (closure.LabelSelector, error) {
	sel, err := matchLabels(raw)
	if err != nil {
		return closure.LabelSelector{}, err
	}
	if len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0 {
		return closure.LabelSelector{}, nil
	}
	return sel, nil
}

func crossRefsFrom(ns string, spec rawSpec) []closure.CrossRef {
	// Cross-refs come from the bare-Pod spec AND the workload pod template — a
	// Deployment/StatefulSet mounts its config under spec.template.spec.
	out := podSpecCrossRefs(ns, spec.rawPodSpec)
	if spec.Template != nil {
		out = append(out, podSpecCrossRefs(ns, spec.Template.Spec)...)
	}
	if spec.ScaleTargetRef != nil {
		k, n := spec.ScaleTargetRef.Kind, spec.ScaleTargetRef.Name
		out = append(out, closure.CrossRef{Kind: closure.RefScaleTarget, Ref: closure.Ref{GVK: closure.GVK{Kind: k}, Namespace: ns, Name: n, UID: uidOf(k, ns, n)}})
	}
	return out
}

func podSpecCrossRefs(ns string, ps rawPodSpec) []closure.CrossRef {
	var out []closure.CrossRef
	for _, v := range ps.Volumes {
		switch {
		case v.ConfigMap != nil:
			out = append(out, closure.CrossRef{Kind: closure.RefVolume, Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "ConfigMap"}, Namespace: ns, Name: v.ConfigMap.Name, UID: uidOf("ConfigMap", ns, v.ConfigMap.Name)}})
		case v.Secret != nil:
			out = append(out, closure.CrossRef{Kind: closure.RefVolume, Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Secret"}, Namespace: ns, Name: v.Secret.SecretName, UID: uidOf("Secret", ns, v.Secret.SecretName)}})
		case v.PVC != nil:
			out = append(out, closure.CrossRef{Kind: closure.RefVolume, Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "PersistentVolumeClaim"}, Namespace: ns, Name: v.PVC.ClaimName, UID: uidOf("PersistentVolumeClaim", ns, v.PVC.ClaimName)}})
		case v.Projected != nil:
			for _, src := range v.Projected.Sources {
				if src.ConfigMap != nil {
					out = append(out, closure.CrossRef{Kind: closure.RefVolume, Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "ConfigMap"}, Namespace: ns, Name: src.ConfigMap.Name, UID: uidOf("ConfigMap", ns, src.ConfigMap.Name)}})
				}
				if src.Secret != nil {
					out = append(out, closure.CrossRef{Kind: closure.RefVolume, Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Secret"}, Namespace: ns, Name: src.Secret.Name, UID: uidOf("Secret", ns, src.Secret.Name)}})
				}
			}
		}
	}
	// initContainers and ephemeralContainers consume ConfigMaps/Secrets exactly as
	// regular containers do (a broken mount/env fails the container on its next
	// start), so walk all three.
	containers := append(append(append([]rawContainer{}, ps.Containers...), ps.InitContainers...), ps.EphemeralContainers...)
	for _, c := range containers {
		for _, e := range c.EnvFrom {
			if e.ConfigMapRef != nil {
				out = append(out, closure.CrossRef{Kind: closure.RefEnvFrom, Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "ConfigMap"}, Namespace: ns, Name: e.ConfigMapRef.Name, UID: uidOf("ConfigMap", ns, e.ConfigMapRef.Name)}})
			}
			if e.SecretRef != nil {
				out = append(out, closure.CrossRef{Kind: closure.RefEnvFrom, Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Secret"}, Namespace: ns, Name: e.SecretRef.Name, UID: uidOf("Secret", ns, e.SecretRef.Name)}})
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				continue
			}
			if r := e.ValueFrom.ConfigMapKeyRef; r != nil {
				out = append(out, closure.CrossRef{Kind: closure.RefEnv, Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "ConfigMap"}, Namespace: ns, Name: r.Name, UID: uidOf("ConfigMap", ns, r.Name)}})
			}
			if r := e.ValueFrom.SecretKeyRef; r != nil {
				out = append(out, closure.CrossRef{Kind: closure.RefEnv, Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Secret"}, Namespace: ns, Name: r.Name, UID: uidOf("Secret", ns, r.Name)}})
			}
		}
	}
	for _, ips := range ps.ImagePullSecrets {
		out = append(out, closure.CrossRef{Kind: closure.RefImagePullSecret, Ref: closure.Ref{GVK: closure.GVK{Version: "v1", Kind: "Secret"}, Namespace: ns, Name: ips.Name, UID: uidOf("Secret", ns, ips.Name)}})
	}
	return out
}

func parseCluster(raw []byte) ([]closure.Object, error) {
	var objs []closure.Object
	for _, doc := range docSep.Split(string(raw), -1) {
		if strings.TrimSpace(stripComments(doc)) == "" {
			continue
		}
		var m rawManifest
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			return nil, fmt.Errorf("parse manifest: %w\n%s", err, doc)
		}
		if m.Kind == "" {
			continue
		}
		ns := nsOf(m.Kind, m.Metadata.Namespace)
		ref := closure.Ref{GVK: gvkOf(m.APIVersion, m.Kind), Namespace: ns, Name: m.Metadata.Name, UID: uidOf(m.Kind, ns, m.Metadata.Name)}
		owners := make([]closure.OwnerRef, 0, len(m.Metadata.OwnerReferences))
		for _, o := range m.Metadata.OwnerReferences {
			owners = append(owners, closure.OwnerRef{Kind: o.Kind, Name: o.Name, UID: uidOf(o.Kind, ns, o.Name)})
		}
		sel, err := selectorFrom(m.Kind, m.Spec)
		if err != nil {
			return nil, fmt.Errorf("%s/%s: %w", m.Kind, m.Metadata.Name, err)
		}
		objs = append(objs, closure.Object{
			Ref:        ref,
			Labels:     m.Metadata.Labels,
			Selector:   sel,
			Owners:     owners,
			CrossRefs:  crossRefsFrom(ns, m.Spec),
			Finalizers: m.Metadata.Finalizers,
		})
	}
	return objs, nil
}

func stripComments(doc string) string {
	var b strings.Builder
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// --- action / scope ---------------------------------------------------------

type rawRef struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type rawPayload struct {
	Labels     map[string]string `json:"labels"`
	Selector   map[string]string `json:"selector"`
	Finalizers []string          `json:"finalizers"`
}

type rawAction struct {
	Verb    string      `json:"verb"`
	Cascade *bool       `json:"cascade"`
	Target  rawRef      `json:"target"`
	Old     *rawPayload `json:"old"`
	New     *rawPayload `json:"new"`
}

// payloadSelector wraps an action payload's flat selector map as a LabelSelector.
// A nil map stays the nil selector (binds nothing); the request format carries
// only matchLabels-style selectors.
func payloadSelector(m map[string]string) closure.LabelSelector {
	return closure.LabelSelector{MatchLabels: m}
}

func parseAction(raw []byte) (closure.Action, error) {
	var ra rawAction
	if err := yaml.Unmarshal(raw, &ra); err != nil {
		return closure.Action{}, fmt.Errorf("parse action: %w", err)
	}
	ns := nsOf(ra.Target.Kind, ra.Target.Namespace)
	a := closure.Action{
		Verb:    closure.Verb(ra.Verb),
		Target:  closure.Ref{GVK: closure.GVK{Group: ra.Target.Group, Version: ra.Target.Version, Kind: ra.Target.Kind}, Namespace: ns, Name: ra.Target.Name, UID: uidOf(ra.Target.Kind, ns, ra.Target.Name)},
		Cascade: ra.Cascade == nil || *ra.Cascade,
	}
	if ra.Old != nil {
		a.Old = &closure.Object{Labels: ra.Old.Labels, Selector: payloadSelector(ra.Old.Selector), Finalizers: ra.Old.Finalizers}
	}
	if ra.New != nil {
		a.New = &closure.Object{Labels: ra.New.Labels, Selector: payloadSelector(ra.New.Selector), Finalizers: ra.New.Finalizers}
	}
	return a, nil
}

// rawScopeClause is a dimension-typed scope clause. A clause with no `dim` (every
// v0.1/v0.2 scope.yaml) loads as DimResource and matches by name exactly as before.
// A `dim: selector` clause carries a `{matchLabels, matchExpressions}` selector,
// built by the same matchLabels conversion the cluster loader uses for
// Object.Selector — one selector parse path.
type rawScopeClause struct {
	Dim       string  `json:"dim"`
	Group     string  `json:"group"`
	Version   string  `json:"version"`
	Kind      string  `json:"kind"`
	Namespace string  `json:"namespace"`
	Name      string  `json:"name"`
	Root      *rawRef `json:"root"` // DimOwnership only: the subtree root
}

func parseScope(raw []byte) ([]closure.ScopeClause, error) {
	var rs struct {
		Scope []json.RawMessage `json:"scope"`
	}
	if err := yaml.Unmarshal(raw, &rs); err != nil {
		return nil, fmt.Errorf("parse scope: %w", err)
	}
	out := make([]closure.ScopeClause, 0, len(rs.Scope))
	for _, rawClause := range rs.Scope {
		var rc rawScopeClause
		if err := json.Unmarshal(rawClause, &rc); err != nil {
			return nil, fmt.Errorf("parse scope clause: %w", err)
		}
		var clause closure.ScopeClause
		if closure.ScopeDim(rc.Dim) == closure.DimOwnership {
			// An ownership clause carries identity only on Root: resolve the root's
			// synthetic uid via uidOf (exactly as the action target is resolved) and
			// nsOf for its namespace, so ownedSubtree membership matches by uid like
			// the closure does. No clause-level GVK/Namespace/Name (Validate rejects them).
			if rc.Root == nil {
				return nil, fmt.Errorf("invalid scope clause: ownership dimension requires a root")
			}
			// Reject any clause-level identity/selector field BEFORE building the clause:
			// an ownership clause's identity may live ONLY on `root`, so a conflicting
			// top-level field must fail closed at the parse boundary (ADR-0010) rather
			// than be silently dropped here (which would let a malformed scope.yaml pass).
			if rc.Group != "" || rc.Version != "" || rc.Kind != "" {
				return nil, fmt.Errorf("invalid scope clause: ownership dimension must not carry a clause-level GVK (group/version/kind); identity lives on root")
			}
			if rc.Namespace != "" {
				return nil, fmt.Errorf("invalid scope clause: ownership dimension must not carry a clause-level namespace; identity lives on root")
			}
			if rc.Name != "" {
				return nil, fmt.Errorf("invalid scope clause: ownership dimension must not carry a clause-level name; identity lives on root")
			}
			if sel, serr := scopeSelectorFrom(rawClause); serr != nil {
				return nil, fmt.Errorf("parse ownership scope clause selector: %w", serr)
			} else if len(sel.MatchLabels) > 0 || len(sel.MatchExpressions) > 0 {
				return nil, fmt.Errorf("invalid scope clause: ownership dimension must not carry a clause-level selector; identity lives on root")
			}
			rootNS := nsOf(rc.Root.Kind, rc.Root.Namespace)
			clause = closure.OwnershipClause(closure.Ref{
				GVK:       closure.GVK{Group: rc.Root.Group, Version: rc.Root.Version, Kind: rc.Root.Kind},
				Namespace: rootNS,
				Name:      rc.Root.Name,
				UID:       uidOf(rc.Root.Kind, rootNS, rc.Root.Name),
			})
		} else {
			// `root` is meaningful only for dim: ownership. A stray root on a resource/
			// selector/namespace clause must fail closed at the parse boundary (ADR-0010)
			// rather than be silently dropped — Validate now treats a stray Root as a hard
			// error, but the dropped value would never reach it.
			if rc.Root != nil {
				return nil, fmt.Errorf("invalid scope clause: only the ownership dimension may carry a root; dim %q must not", rc.Dim)
			}
			clause = closure.ScopeClause{
				Dim:       closure.ScopeDim(rc.Dim),
				GVK:       closure.GVK{Group: rc.Group, Version: rc.Version, Kind: rc.Kind},
				Namespace: nsOf(rc.Kind, rc.Namespace),
				Name:      rc.Name,
			}
			if closure.ScopeDim(rc.Dim) == closure.DimSelector {
				sel, err := scopeSelectorFrom(rawClause)
				if err != nil {
					return nil, fmt.Errorf("parse selector scope clause: %w", err)
				}
				clause.Selector = sel
			}
		}
		// Validate structural consistency (and reject an unknown dimension) at load
		// time so a typo'd or malformed scope.yaml fails loudly here rather than
		// misbehaving in the engine (closure.ScopeClause.Validate; F1 fail-closed).
		if err := clause.Validate(); err != nil {
			return nil, fmt.Errorf("invalid scope clause: %w", err)
		}
		out = append(out, clause)
	}
	return out, nil
}

// --- taskcontract -----------------------------------------------------------

// rawTaskContract mirrors the TaskContract YAML wire form (DESIGN §6). Parsing it
// here, in the dev/demo loader, keeps the public scope package YAML-free: scope only
// ever sees the compiled struct. Each allow-clause's selector reuses the same
// matchLabels conversion the cluster loader uses for Object.Selector — one selector
// parse path — and namespace defaulting reuses nsOf, exactly as parseScope does.
type rawTaskContract struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Metadata   rawTCMetadata       `json:"metadata"`
	Spec       rawTaskContractSpec `json:"spec"`
}

type rawTCMetadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type rawTaskContractSpec struct {
	Allow       []json.RawMessage `json:"allow"`
	MaxSeverity string            `json:"maxSeverity"`
}

// rawAllowClause is one spec.allow entry. The selector dim's `{matchLabels,
// matchExpressions}` lives at the clause top level (mirroring scope.yaml), so the
// whole raw clause is handed to matchLabels.
type rawAllowClause struct {
	Dim       string `json:"dim"`
	GVK       rawRef `json:"gvk"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// parseTaskContract maps the YAML wire form to a scope.TaskContract. It does not
// validate the contract — that is scope.Compile's fail-closed job; the loader only
// shapes the struct (resolving selectors and defaulting namespaces).
func parseTaskContract(raw []byte) (scope.TaskContract, error) {
	var rtc rawTaskContract
	if err := yaml.Unmarshal(raw, &rtc); err != nil {
		return scope.TaskContract{}, fmt.Errorf("parse taskcontract: %w", err)
	}
	allow := make([]scope.AllowClause, 0, len(rtc.Spec.Allow))
	for _, rawClause := range rtc.Spec.Allow {
		var rc rawAllowClause
		if err := json.Unmarshal(rawClause, &rc); err != nil {
			return scope.TaskContract{}, fmt.Errorf("parse allow clause: %w", err)
		}
		ac := scope.AllowClause{
			Dim:       closure.ScopeDim(rc.Dim),
			GVK:       closure.GVK{Group: rc.GVK.Group, Version: rc.GVK.Version, Kind: rc.GVK.Kind},
			Namespace: nsOf(rc.GVK.Kind, rc.Namespace),
			Name:      rc.Name,
		}
		if closure.ScopeDim(rc.Dim) == closure.DimSelector {
			sel, err := scopeSelectorFrom(rawClause)
			if err != nil {
				return scope.TaskContract{}, fmt.Errorf("parse selector allow clause: %w", err)
			}
			ac.Selector = sel
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
