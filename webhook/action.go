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
	// Tracked reports whether the informer set watches the target's kind (group+Kind,
	// version-insensitive). A kind the informers do not track has no closure the webhook
	// can compute — it fails closed with a distinct taxonomy code (round-2 finding 4).
	Tracked(gvk closure.GVK) bool
	// GVKsForKind returns every TRACKED GVK whose Kind is kind, across all groups —
	// the Kind→GVK direction resolveScope needs for the krsm.io/target annotation's
	// `<Kind>[.<group>]` token. KindFor cannot answer it (it maps a plural GVR) and
	// Tracked cannot (it is a yes/no over an already-known GVK). Returning the whole
	// set — not a best match — is what lets the caller fail closed on an ambiguous
	// bare Kind instead of choosing a group arbitrarily.
	GVKsForKind(kind string) []closure.GVK
	// GetByGVK resolves ONE tracked object by its exact Group+Kind+namespace+name.
	// closure.State.Get cannot serve this: a uid-less ref falls back to the
	// group-BLIND Kind/ns/name bucket, where two tracked kinds sharing a Kind in
	// different groups collide and the later one wins. resolveScope uses this to pin
	// an annotation-named ownership root to its REAL uid before building the clause,
	// so the subtree walk can never key on the ambiguous bucket (design rev 3,
	// "Group-blind human key"). A miss reports false and the caller fails closed.
	GetByGVK(gvk closure.GVK, namespace, name string) (closure.Object, bool)
}

// subResourceGate describes how one gated sub-resource maps to an Action. The table is
// the SINGLE source of truth consulted by all three dispatch sites — Handle's
// CREATE-admit branch, the resourceVersion precheck, and actionFromRequest — so they
// cannot drift (round-2 finding 8 + the three scattered "eviction" literals).
type subResourceGate struct {
	verb       closure.Verb // Scale | Delete | Update on the parent
	viaCreate  bool         // arrives as a CREATE (eviction) — exempt it from the CREATE admit
	carriesRV  bool         // oldObject carries the parent's resourceVersion (false for eviction)
	hasPayload bool         // project object/oldObject into Action.Old/New (ephemeralcontainers)
}

// gatedSubResources is the set of sub-resources KRSM evaluates: "scale" is a ScaleEffect
// on the parent, "eviction" is a pod delete in disguise (a CREATE), and
// "ephemeralcontainers" is the only path that injects an ephemeral container — a pod
// mutation carrying full Pod objects (round-2 finding 1). A sub-resource gated here
// without a switch arm in actionFromRequest fails closed, never silently admits.
var gatedSubResources = map[string]subResourceGate{
	"scale":               {verb: closure.Scale, carriesRV: true},
	"eviction":            {verb: closure.Delete, viaCreate: true},
	"ephemeralcontainers": {verb: closure.Update, carriesRV: true, hasPayload: true},
}

// gatedSubResource looks up a sub-resource's gate. The empty sub-resource ("") is the
// main resource and is never in the table.
func gatedSubResource(sub string) (subResourceGate, bool) {
	g, ok := gatedSubResources[sub]
	return g, ok
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
	if req.SubResource != "" {
		gate, ok := gatedSubResource(req.SubResource)
		if !ok {
			return closure.Action{}, fmt.Errorf("ungated sub-resource %q reached actionFromRequest", req.SubResource)
		}
		parent, ok := info.KindFor(req.Resource.Group, req.Resource.Resource)
		if !ok {
			return closure.Action{}, fmt.Errorf("unknown parent resource %s/%s for sub-resource %q", req.Resource.Group, req.Resource.Resource, req.SubResource)
		}
		target := closure.Ref{GVK: parent, Namespace: req.Namespace, Name: req.Name}
		action := closure.Action{Verb: gate.verb, Target: target, Cascade: true}
		if gate.verb == closure.Delete {
			// eviction (F8): the propagationPolicy lives on the Eviction body's
			// deleteOptions (newU); req.Options carries CreateOptions for the CREATE,
			// so cascadeFromOptions never sees it. Orphan → false, else conservative.
			action.Cascade = cascadeFromEviction(newU)
		}
		if gate.hasPayload {
			// ephemeralcontainers carries FULL Pod objects, so the payloads project
			// like a plain pod UPDATE and the target uid comes from oldObject.
			old, err := projectPayload(oldU, info)
			if err != nil {
				return closure.Action{}, fmt.Errorf("oldObject: %w", err)
			}
			newObj, err := projectPayload(newU, info)
			if err != nil {
				return closure.Action{}, fmt.Errorf("object: %w", err)
			}
			if old != nil {
				action.Target.UID = old.Ref.UID
			}
			action.Old, action.New = old, newObj
		}
		return action, nil
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

// cascadeFromEviction reads Cascade from a decoded Eviction body's
// deleteOptions.propagationPolicy (F8): Orphan leaves children alive → false; any other
// policy, an absent deleteOptions, or an unreadable field cascades → true (the
// conservative, larger closure). The Eviction body is the CREATE payload (newU), never
// req.Options.
func cascadeFromEviction(newU *unstructured.Unstructured) bool {
	if newU == nil {
		return true
	}
	pol, found, err := unstructured.NestedString(newU.Object, "deleteOptions", "propagationPolicy")
	if err != nil || !found {
		return true
	}
	return pol != string(metav1.DeletePropagationOrphan)
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
