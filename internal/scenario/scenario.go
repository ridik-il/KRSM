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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/ridik-il/krsm/closure"
)

// Scenario is a runnable check input: the three things closure.Safe needs.
type Scenario struct {
	State  closure.State
	Action closure.Action
	Scope  []closure.ScopeRef
}

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

	scopeRaw, err := read("scope.yaml")
	if err != nil {
		return nil, err
	}
	scope, err := parseScope(scopeRaw)
	if err != nil {
		return nil, err
	}

	return &Scenario{
		State:  closure.NewScanState(objs),
		Action: action,
		Scope:  scope,
	}, nil
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
	Volumes          []rawVolume    `json:"volumes"`
	Containers       []rawContainer `json:"containers"`
	InitContainers   []rawContainer `json:"initContainers"`
	ImagePullSecrets []struct {
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
func selectorFrom(kind string, spec rawSpec) closure.LabelSelector {
	switch kind {
	case "Service":
		if spec.Selector == nil {
			return closure.LabelSelector{} // absent → binds nothing
		}
		m := map[string]string{}
		_ = json.Unmarshal(spec.Selector, &m)
		return closure.LabelSelector{MatchLabels: m}
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
func matchLabels(raw json.RawMessage) closure.LabelSelector {
	if raw == nil {
		return closure.LabelSelector{}
	}
	var wrap struct {
		MatchLabels      map[string]string `json:"matchLabels"`
		MatchExpressions []struct {
			Key      string   `json:"key"`
			Operator string   `json:"operator"`
			Values   []string `json:"values"`
		} `json:"matchExpressions"`
	}
	_ = json.Unmarshal(raw, &wrap)
	sel := closure.LabelSelector{MatchLabels: wrap.MatchLabels}
	for _, e := range wrap.MatchExpressions {
		sel.MatchExpressions = append(sel.MatchExpressions, closure.SelectorRequirement{
			Key:      e.Key,
			Operator: closure.SelectorOperator(e.Operator),
			Values:   e.Values,
		})
	}
	if wrap.MatchLabels == nil && len(sel.MatchExpressions) == 0 {
		sel.MatchLabels = map[string]string{} // present but empty → matches all
	}
	return sel
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
	// initContainers consume ConfigMaps/Secrets exactly as regular containers do
	// (a broken mount/env fails the pod on its next start), so walk both.
	containers := append(append([]rawContainer{}, ps.Containers...), ps.InitContainers...)
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
		objs = append(objs, closure.Object{
			Ref:        ref,
			Labels:     m.Metadata.Labels,
			Selector:   selectorFrom(m.Kind, m.Spec),
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

func parseScope(raw []byte) ([]closure.ScopeRef, error) {
	var rs struct {
		Scope []rawRef `json:"scope"`
	}
	if err := yaml.Unmarshal(raw, &rs); err != nil {
		return nil, fmt.Errorf("parse scope: %w", err)
	}
	out := make([]closure.ScopeRef, 0, len(rs.Scope))
	for _, r := range rs.Scope {
		out = append(out, closure.ScopeRef{GVK: closure.GVK{Group: r.Group, Version: r.Version, Kind: r.Kind}, Namespace: nsOf(r.Kind, r.Namespace), Name: r.Name})
	}
	return out, nil
}
