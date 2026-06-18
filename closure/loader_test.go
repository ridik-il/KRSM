package closure

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// scenario is a loaded golden test case.
type scenario struct {
	name     string
	state    State
	action   Action
	scope    []ScopeRef
	expected expectedVerdict
}

type expectedVerdict struct {
	Verdict  string     `json:"verdict"`
	Reason   string     `json:"reason"` // optional: asserted as a substring when set
	Closure  []humanRef `json:"closure"`
	Escaping []humanRef `json:"escaping"`
	External []humanRef `json:"external"`
}

// humanRef is the Kind/namespace/name identity used in golden files (uid-free).
type humanRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (h humanRef) key() string {
	return fmt.Sprintf("%s/%s/%s", h.Kind, h.Namespace, h.Name)
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
// their ConfigMap/Secret/PVC consumption (finding M2). Parsing only the top level
// would make every workload's mounts invisible.
type rawPodSpec struct {
	Volumes []struct {
		ConfigMap *struct {
			Name string `json:"name"`
		} `json:"configMap"`
		Secret *struct {
			SecretName string `json:"secretName"`
		} `json:"secret"`
		PVC *struct {
			ClaimName string `json:"claimName"`
		} `json:"persistentVolumeClaim"`
	} `json:"volumes"`
	Containers []struct {
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
	} `json:"containers"`
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

func gvkOf(apiVersion, kind string) GVK {
	g := GVK{Kind: kind}
	if parts := strings.SplitN(apiVersion, "/", 2); len(parts) == 2 {
		g.Group, g.Version = parts[0], parts[1]
	} else {
		g.Version = apiVersion
	}
	return g
}

func nsOf(kind, ns string) string {
	if kind == "Namespace" {
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
func selectorFrom(kind string, spec rawSpec) map[string]string {
	switch kind {
	case "Service":
		if spec.Selector == nil {
			return nil
		}
		m := map[string]string{}
		_ = json.Unmarshal(spec.Selector, &m)
		return m
	case "NetworkPolicy":
		return matchLabels(spec.PodSelector)
	default:
		return matchLabels(spec.Selector)
	}
}

func matchLabels(raw json.RawMessage) map[string]string {
	if raw == nil {
		return nil
	}
	var wrap struct {
		MatchLabels map[string]string `json:"matchLabels"`
	}
	_ = json.Unmarshal(raw, &wrap)
	if wrap.MatchLabels == nil {
		return map[string]string{} // present but empty → matches all
	}
	return wrap.MatchLabels
}

func crossRefsFrom(ns string, spec rawSpec) []CrossRef {
	// Cross-refs come from the bare-Pod spec AND the workload pod template — a
	// Deployment/StatefulSet mounts its config under spec.template.spec (M2).
	out := podSpecCrossRefs(ns, spec.rawPodSpec)
	if spec.Template != nil {
		out = append(out, podSpecCrossRefs(ns, spec.Template.Spec)...)
	}
	if spec.ScaleTargetRef != nil {
		k, n := spec.ScaleTargetRef.Kind, spec.ScaleTargetRef.Name
		out = append(out, CrossRef{RefScaleTarget, Ref{GVK: GVK{Kind: k}, Namespace: ns, Name: n, UID: uidOf(k, ns, n)}})
	}
	return out
}

func podSpecCrossRefs(ns string, ps rawPodSpec) []CrossRef {
	var out []CrossRef
	for _, v := range ps.Volumes {
		switch {
		case v.ConfigMap != nil:
			out = append(out, CrossRef{RefVolume, Ref{GVK: GVK{Version: "v1", Kind: "ConfigMap"}, Namespace: ns, Name: v.ConfigMap.Name, UID: uidOf("ConfigMap", ns, v.ConfigMap.Name)}})
		case v.Secret != nil:
			out = append(out, CrossRef{RefVolume, Ref{GVK: GVK{Version: "v1", Kind: "Secret"}, Namespace: ns, Name: v.Secret.SecretName, UID: uidOf("Secret", ns, v.Secret.SecretName)}})
		case v.PVC != nil:
			out = append(out, CrossRef{RefVolume, Ref{GVK: GVK{Version: "v1", Kind: "PersistentVolumeClaim"}, Namespace: ns, Name: v.PVC.ClaimName, UID: uidOf("PersistentVolumeClaim", ns, v.PVC.ClaimName)}})
		}
	}
	for _, c := range ps.Containers {
		for _, e := range c.EnvFrom {
			if e.ConfigMapRef != nil {
				out = append(out, CrossRef{RefEnvFrom, Ref{GVK: GVK{Version: "v1", Kind: "ConfigMap"}, Namespace: ns, Name: e.ConfigMapRef.Name, UID: uidOf("ConfigMap", ns, e.ConfigMapRef.Name)}})
			}
			if e.SecretRef != nil {
				out = append(out, CrossRef{RefEnvFrom, Ref{GVK: GVK{Version: "v1", Kind: "Secret"}, Namespace: ns, Name: e.SecretRef.Name, UID: uidOf("Secret", ns, e.SecretRef.Name)}})
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				continue
			}
			if r := e.ValueFrom.ConfigMapKeyRef; r != nil {
				out = append(out, CrossRef{RefEnv, Ref{GVK: GVK{Version: "v1", Kind: "ConfigMap"}, Namespace: ns, Name: r.Name, UID: uidOf("ConfigMap", ns, r.Name)}})
			}
			if r := e.ValueFrom.SecretKeyRef; r != nil {
				out = append(out, CrossRef{RefEnv, Ref{GVK: GVK{Version: "v1", Kind: "Secret"}, Namespace: ns, Name: r.Name, UID: uidOf("Secret", ns, r.Name)}})
			}
		}
	}
	return out
}

func parseCluster(t *testing.T, raw []byte) []Object {
	t.Helper()
	var objs []Object
	for _, doc := range docSep.Split(string(raw), -1) {
		if strings.TrimSpace(stripComments(doc)) == "" {
			continue
		}
		var m rawManifest
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("parse manifest: %v\n%s", err, doc)
		}
		if m.Kind == "" {
			continue
		}
		ns := nsOf(m.Kind, m.Metadata.Namespace)
		ref := Ref{GVK: gvkOf(m.APIVersion, m.Kind), Namespace: ns, Name: m.Metadata.Name, UID: uidOf(m.Kind, ns, m.Metadata.Name)}
		owners := make([]OwnerRef, 0, len(m.Metadata.OwnerReferences))
		for _, o := range m.Metadata.OwnerReferences {
			owners = append(owners, OwnerRef{Kind: o.Kind, Name: o.Name, UID: uidOf(o.Kind, ns, o.Name)})
		}
		objs = append(objs, Object{
			Ref:        ref,
			Labels:     m.Metadata.Labels,
			Selector:   selectorFrom(m.Kind, m.Spec),
			Owners:     owners,
			CrossRefs:  crossRefsFrom(ns, m.Spec),
			Finalizers: m.Metadata.Finalizers,
		})
	}
	return objs
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

// --- action / scope / expected ---------------------------------------------

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

func parseAction(t *testing.T, raw []byte) Action {
	t.Helper()
	var ra rawAction
	if err := yaml.Unmarshal(raw, &ra); err != nil {
		t.Fatalf("parse action: %v", err)
	}
	ns := nsOf(ra.Target.Kind, ra.Target.Namespace)
	a := Action{
		Verb:    Verb(ra.Verb),
		Target:  Ref{GVK: GVK{Group: ra.Target.Group, Version: ra.Target.Version, Kind: ra.Target.Kind}, Namespace: ns, Name: ra.Target.Name, UID: uidOf(ra.Target.Kind, ns, ra.Target.Name)},
		Cascade: ra.Cascade == nil || *ra.Cascade,
	}
	if ra.Old != nil {
		a.Old = &Object{Labels: ra.Old.Labels, Selector: ra.Old.Selector, Finalizers: ra.Old.Finalizers}
	}
	if ra.New != nil {
		a.New = &Object{Labels: ra.New.Labels, Selector: ra.New.Selector, Finalizers: ra.New.Finalizers}
	}
	return a
}

func parseScope(t *testing.T, raw []byte) []ScopeRef {
	t.Helper()
	var rs struct {
		Scope []rawRef `json:"scope"`
	}
	if err := yaml.Unmarshal(raw, &rs); err != nil {
		t.Fatalf("parse scope: %v", err)
	}
	out := make([]ScopeRef, 0, len(rs.Scope))
	for _, r := range rs.Scope {
		out = append(out, ScopeRef{GVK: GVK{Group: r.Group, Version: r.Version, Kind: r.Kind}, Namespace: nsOf(r.Kind, r.Namespace), Name: r.Name})
	}
	return out
}

func loadScenario(t *testing.T, dir string) scenario {
	t.Helper()
	read := func(f string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		return b
	}
	var exp expectedVerdict
	if err := yaml.Unmarshal(read("expected.yaml"), &exp); err != nil {
		t.Fatalf("parse expected: %v", err)
	}
	return scenario{
		name:     filepath.Base(dir),
		state:    NewScanState(parseCluster(t, read("cluster.yaml"))),
		action:   parseAction(t, read("request.yaml")),
		scope:    parseScope(t, read("scope.yaml")),
		expected: exp,
	}
}
