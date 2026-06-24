package cluster

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ridik-il/krsm/closure"
)

// crossRefsFromUnstructured mirrors internal/scenario.crossRefsFrom: cross-refs come
// from the bare-Pod spec AND the workload pod template (spec.template.spec), plus an
// HPA's spec.scaleTargetRef. Each referent's uid is resolved through the name→uid
// index over the listed objects (empty on a miss; crossRefMatches then falls back to
// Kind/ns/name). ns is the consumer's resolved namespace — every referent is assumed
// same-namespace, as the loader assumes.
func crossRefsFromUnstructured(o unstructured.Unstructured, ns string, ix nameUIDIndex) []closure.CrossRef {
	spec, found := nestedMap(o.Object, "spec")
	if !found {
		return nil
	}

	out := podSpecCrossRefs(spec, ns, ix)

	if tmpl, ok := nestedMap(spec, "template", "spec"); ok {
		out = append(out, podSpecCrossRefs(tmpl, ns, ix)...)
	}

	if str, ok := nestedMap(spec, "scaleTargetRef"); ok {
		k := nestedString(str, "kind")
		n := nestedString(str, "name")
		out = append(out, crossRef(closure.RefScaleTarget, closure.GVK{Kind: k}, ns, n, ix))
	}
	return out
}

// podSpecCrossRefs ports internal/scenario.podSpecCrossRefs to an unstructured pod
// spec: volumes (configMap/secret/persistentVolumeClaim/projected sources),
// containers+initContainers+ephemeralContainers (envFrom and env[].valueFrom), and
// imagePullSecrets. initContainers and ephemeralContainers consume config exactly as
// regular containers do, so all three are walked.
func podSpecCrossRefs(ps map[string]any, ns string, ix nameUIDIndex) []closure.CrossRef {
	var out []closure.CrossRef

	vols, _ := nestedSlice(ps, "volumes")
	for _, v := range vols {
		vm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		switch {
		case has(vm, "configMap"):
			out = append(out, crossRef(closure.RefVolume, cmGVK, ns, nestedString(vm, "configMap", "name"), ix))
		case has(vm, "secret"):
			out = append(out, crossRef(closure.RefVolume, secretGVK, ns, nestedString(vm, "secret", "secretName"), ix))
		case has(vm, "persistentVolumeClaim"):
			out = append(out, crossRef(closure.RefVolume, pvcGVK, ns, nestedString(vm, "persistentVolumeClaim", "claimName"), ix))
		case has(vm, "projected"):
			srcs, _ := nestedSlice(vm, "projected", "sources")
			for _, s := range srcs {
				sm, ok := s.(map[string]any)
				if !ok {
					continue
				}
				if has(sm, "configMap") {
					out = append(out, crossRef(closure.RefVolume, cmGVK, ns, nestedString(sm, "configMap", "name"), ix))
				}
				if has(sm, "secret") {
					out = append(out, crossRef(closure.RefVolume, secretGVK, ns, nestedString(sm, "secret", "name"), ix))
				}
			}
		}
	}

	for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
		cs, _ := nestedSlice(ps, field)
		for _, c := range cs {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, containerCrossRefs(cm, ns, ix)...)
		}
	}

	pulls, _ := nestedSlice(ps, "imagePullSecrets")
	for _, p := range pulls {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, crossRef(closure.RefImagePullSecret, secretGVK, ns, nestedString(pm, "name"), ix))
	}

	return out
}

func containerCrossRefs(c map[string]any, ns string, ix nameUIDIndex) []closure.CrossRef {
	var out []closure.CrossRef

	envFrom, _ := nestedSlice(c, "envFrom")
	for _, e := range envFrom {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if has(em, "configMapRef") {
			out = append(out, crossRef(closure.RefEnvFrom, cmGVK, ns, nestedString(em, "configMapRef", "name"), ix))
		}
		if has(em, "secretRef") {
			out = append(out, crossRef(closure.RefEnvFrom, secretGVK, ns, nestedString(em, "secretRef", "name"), ix))
		}
	}

	env, _ := nestedSlice(c, "env")
	for _, e := range env {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		vf, ok := nestedMap(em, "valueFrom")
		if !ok {
			continue
		}
		if has(vf, "configMapKeyRef") {
			out = append(out, crossRef(closure.RefEnv, cmGVK, ns, nestedString(vf, "configMapKeyRef", "name"), ix))
		}
		if has(vf, "secretKeyRef") {
			out = append(out, crossRef(closure.RefEnv, secretGVK, ns, nestedString(vf, "secretKeyRef", "name"), ix))
		}
	}
	return out
}

var (
	cmGVK     = closure.GVK{Version: "v1", Kind: "ConfigMap"}
	secretGVK = closure.GVK{Version: "v1", Kind: "Secret"}
	pvcGVK    = closure.GVK{Version: "v1", Kind: "PersistentVolumeClaim"}
)

// crossRef builds a CrossRef, resolving the referent's real uid through the index.
func crossRef(kind closure.RefKind, gvk closure.GVK, ns, name string, ix nameUIDIndex) closure.CrossRef {
	return closure.CrossRef{
		Kind: kind,
		Ref: closure.Ref{
			GVK:       gvk,
			Namespace: ns,
			Name:      name,
			UID:       ix.uidFor(gvk.Kind, ns, name),
		},
	}
}

// has reports whether a nested map exists at the given path (the unstructured
// equivalent of "this volume source / ref kind is present"). It uses the no-copy read
// so an adversarial non-copyable value under the path can never panic.
func has(obj map[string]any, fields ...string) bool {
	_, found := nestedMap(obj, fields...)
	return found
}
