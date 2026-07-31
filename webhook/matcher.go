package webhook

import (
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// AgentMatcher decides whether a request is agent-originated. KRSM gates ONLY those;
// everything else is admitted untouched (DESIGN §5 — the webhook must never wedge
// human/controller traffic).
//
// Handle uses a TWO-STAGE protocol (round-2 finding 5) so the non-agent hot path never
// pays a payload decode: stage 1 calls Matches with NIL payloads (identity matchers
// answer from req.userInfo alone); a stage-1 miss consults NeedsPayload and, only if it
// reports true, decodes the payloads and calls Matches again. Implementations MUST
// therefore answer conservatively with nil payloads — a nil-payload miss means "cannot
// tell from the request alone", never "not an agent".
type AgentMatcher interface {
	Matches(req *admissionv1.AdmissionRequest, object, oldObject *unstructured.Unstructured) bool
	// NeedsPayload reports whether a nil-payload miss is inconclusive and stage 2
	// (decode + re-match) is required to decide.
	NeedsPayload() bool
}

// MatchAll gates every request — the explicit spelling of "evaluate all agent traffic"
// (the library-conservative default when no matcher is configured, and the --gate-all
// opt-in). It needs no payload: identity is irrelevant when everything matches.
type MatchAll struct{}

func (MatchAll) Matches(*admissionv1.AdmissionRequest, *unstructured.Unstructured, *unstructured.Unstructured) bool {
	return true
}
func (MatchAll) NeedsPayload() bool { return false }

// AnnotationMatcher matches when the request's object (or, for DELETE, its oldObject)
// carries the annotation Key with any value. The zero Key matches NOTHING — a disabled
// annotation matcher (round-2 finding 3; match-everything is now the explicit MatchAll).
//
// STRUCTURAL BLIND SPOT (PR #36 review finding 1): this matcher can never match
// scale-subresource UPDATEs (the autoscaling/v1 Scale wrapper's metadata carries no
// annotations), pods/eviction CREATEs (the Eviction body is client-synthesized), or
// CONNECT operations (exec/attach options objects) — and for DELETE it only matches
// objects that ALREADY carry the annotation. Deployments that need those paths gated
// must identify the agent by serviceaccount instead (ServiceAccountMatcher, or
// AnyMatcher combining both).
type AnnotationMatcher struct{ Key string }

func (m AnnotationMatcher) Matches(_ *admissionv1.AdmissionRequest, object, oldObject *unstructured.Unstructured) bool {
	if m.Key == "" {
		return false
	}
	return hasAnnotation(object, m.Key) || hasAnnotation(oldObject, m.Key)
}

// NeedsPayload: a keyed annotation matcher reads the payload, so a stage-1 (nil) miss is
// inconclusive; the disabled (Key "") matcher never matches and needs no decode.
func (m AnnotationMatcher) NeedsPayload() bool { return m.Key != "" }

func hasAnnotation(u *unstructured.Unstructured, key string) bool {
	if u == nil {
		return false
	}
	_, ok := u.GetAnnotations()[key]
	return ok
}

// ServiceAccountMatcher matches when request.userInfo.username is one of the given
// usernames (e.g. "system:serviceaccount:agents:remediator"). Identity comes from the
// API server's authentication layer, not the payload, so it covers every operation the
// annotation matcher is structurally blind to (scale, eviction, CONNECT, deletes of
// unannotated objects) and cannot be spoofed by anything that can merely edit objects.
// This is the production-grade agent identity signal (the slice-6 scope channel builds
// on the same interface).
type ServiceAccountMatcher struct{ Usernames map[string]bool }

// NewServiceAccountMatcher builds a matcher over the given usernames, trimming
// surrounding whitespace and dropping empty entries — so a comma-split flag value like
// "a, b" yields two LIVE identities, not "a" and " b" (round-2 finding 2: the untrimmed
// " b" silently gated nothing).
func NewServiceAccountMatcher(usernames []string) ServiceAccountMatcher {
	set := make(map[string]bool, len(usernames))
	for _, u := range usernames {
		if u = strings.TrimSpace(u); u != "" {
			set[u] = true
		}
	}
	return ServiceAccountMatcher{Usernames: set}
}

func (m ServiceAccountMatcher) Matches(req *admissionv1.AdmissionRequest, _, _ *unstructured.Unstructured) bool {
	return m.Usernames[req.UserInfo.Username]
}

// NeedsPayload: identity is on the request (userInfo), so a stage-1 miss is conclusive.
func (m ServiceAccountMatcher) NeedsPayload() bool { return false }

// AnyMatcher matches when any of its matchers match — used to gate on serviceaccount
// identity OR the task annotation.
type AnyMatcher []AgentMatcher

func (ms AnyMatcher) Matches(req *admissionv1.AdmissionRequest, object, oldObject *unstructured.Unstructured) bool {
	for _, m := range ms {
		if m.Matches(req, object, oldObject) {
			return true
		}
	}
	return false
}

// NeedsPayload is the OR over the children: stage 2 is required if ANY child needs the
// payload to decide (an identity child may still match at stage 1 and short-circuit).
func (ms AnyMatcher) NeedsPayload() bool {
	for _, m := range ms {
		if m.NeedsPayload() {
			return true
		}
	}
	return false
}
