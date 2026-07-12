// Package webhook is KRSM's ValidatingWebhook server (v0.5 slice 5, ADR-0011,
// DESIGN §5/§9): it evaluates agent-originated admission requests against the
// informer-backed indexed state and flags (audit) or denies (enforce) actions whose
// affected-resource closure escapes the task's scope — before they persist. It lives
// OUTSIDE the stdlib-only closure/ and scope/ trees (archguard); k8s.io/api enters
// here and in state/, never in the embeddable SDK.
package webhook

import (
	"encoding/json"
	"fmt"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/internal/cluster"
)

// clusterInfo is what actionFromRequest needs from the live state: the discovery
// scope for projection and an EXACT GVR→GVK lookup for sub-resource parents (scale/
// eviction name the parent by resource, not kind — guessing a Kind from a plural is
// not acceptable in a safety gate). *state.Provider satisfies it.
type clusterInfo interface {
	cluster.ScopeInfo
	KindFor(group, resource string) (closure.GVK, bool)
}

// gatedSubResource is the single source of truth for which sub-resources KRSM gates:
// "scale" is a ScaleEffect on the parent, "eviction" is a pod delete in disguise.
// Handle's admit branch and actionFromRequest's mapping both consult it, so the two
// dispatch layers cannot drift — a sub-resource gated here without a mapping below
// fails closed in actionFromRequest, never silently admits.
func gatedSubResource(sub string) bool {
	return sub == "scale" || sub == "eviction"
}

// decodePayload decodes one request payload (RawExtension JSON) into unstructured,
// exactly once per payload per request — the matcher, the resourceVersion peek, and
// the projection all read from the decoded value. Empty payload → nil (DELETE has no
// object; DELETE on old API servers may omit oldObject — the staleness guard then
// fails closed on the missing resourceVersion).
func decodePayload(raw []byte) (*unstructured.Unstructured, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var u unstructured.Unstructured
	if err := u.UnmarshalJSON(raw); err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	return &u, nil
}

// actionFromRequest maps an AdmissionRequest (with its payloads decoded once by the
// caller) to the closure.Action, fail-closed: an unmappable request returns an error
// and the caller denies. The request carries the REAL GVK and the oldObject's uid
// (S2 #16 — no discovery-guess resolution), and the payloads are projected through
// the SAME cluster.Project the informers use (parity).
//
// Sub-resources: "scale" is a ScaleEffect on the PARENT (Verb Scale); "eviction" is a
// pod delete in disguise (Verb Delete) — admitting it as a CREATE would bypass the
// gate. Every other sub-resource is the CALLER's job to admit before calling here.
func actionFromRequest(req *admissionv1.AdmissionRequest, info clusterInfo, oldU, newU *unstructured.Unstructured) (closure.Action, error) {
	switch {
	case req.SubResource == "":
		// fall through to the main-resource mapping below
	case gatedSubResource(req.SubResource):
		parent, ok := info.KindFor(req.Resource.Group, req.Resource.Resource)
		if !ok {
			return closure.Action{}, fmt.Errorf("unknown parent resource %s/%s for sub-resource %q", req.Resource.Group, req.Resource.Resource, req.SubResource)
		}
		verb := closure.Scale
		if req.SubResource == "eviction" {
			verb = closure.Delete
		}
		return closure.Action{
			Verb:    verb,
			Target:  closure.Ref{GVK: parent, Namespace: req.Namespace, Name: req.Name},
			Cascade: true,
		}, nil
	default:
		return closure.Action{}, fmt.Errorf("ungated sub-resource %q reached actionFromRequest", req.SubResource)
	}

	var verb closure.Verb
	switch req.Operation {
	case admissionv1.Delete:
		verb = closure.Delete
	case admissionv1.Update:
		verb = closure.Update
	default:
		return closure.Action{}, fmt.Errorf("unmappable operation %q", req.Operation)
	}

	old, err := projectPayload(oldU, info)
	if err != nil {
		return closure.Action{}, fmt.Errorf("oldObject: %w", err)
	}
	newObj, err := projectPayload(newU, info)
	if err != nil {
		return closure.Action{}, fmt.Errorf("object: %w", err)
	}

	target := closure.Ref{
		GVK:       closure.GVK{Group: req.Kind.Group, Version: req.Kind.Version, Kind: req.Kind.Kind},
		Namespace: req.Namespace,
		Name:      req.Name,
	}
	if old != nil {
		target.UID = old.Ref.UID
	}

	return closure.Action{
		Verb:    verb,
		Target:  target,
		Cascade: cascadeFromOptions(req.Options.Raw),
		Old:     old,
		New:     newObj,
	}, nil
}

// cascadeFromOptions maps DeleteOptions.propagationPolicy to Action.Cascade (S4 #18):
// Orphan leaves the children alive → false; Background/Foreground/absent cascade →
// true (the API server's default). Options that fail to decode keep the conservative
// default (cascade true — the LARGER closure, never the smaller one).
func cascadeFromOptions(raw []byte) bool {
	if len(raw) == 0 {
		return true
	}
	var opts metav1.DeleteOptions
	if err := json.Unmarshal(raw, &opts); err != nil {
		return true
	}
	return opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationOrphan
}

// projectPayload projects a decoded request payload into a closure.Object via
// cluster.Project — identical field paths to the informer index, so the webhook's
// view of the object matches the cache's. A nil payload stays nil.
func projectPayload(u *unstructured.Unstructured, scopeInfo cluster.ScopeInfo) (*closure.Object, error) {
	if u == nil {
		return nil, nil
	}
	obj, err := cluster.Project(*u, scopeInfo)
	if err != nil {
		return nil, fmt.Errorf("project payload: %w", err)
	}
	return &obj, nil
}
