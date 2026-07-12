package webhook

import (
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// AgentMatcher decides whether a request is agent-originated. KRSM gates ONLY those;
// everything else is admitted untouched (DESIGN §5 — the webhook must never wedge
// human/controller traffic). Handle decodes the request payloads exactly once and
// hands them to the matcher, so implementations never re-parse raw JSON.
type AgentMatcher interface {
	Matches(req *admissionv1.AdmissionRequest, object, oldObject *unstructured.Unstructured) bool
}

// AnnotationMatcher matches when the request's object (or, for DELETE, its oldObject)
// carries the annotation Key with any value. The zero Key matches EVERYTHING — the
// e2e/test hook and the safe default when no matcher is configured (over-gating is
// conservative; under-gating lets an agent bypass).
//
// STRUCTURAL BLIND SPOT (PR #36 review finding 1): with a non-empty Key this matcher
// can never match scale-subresource UPDATEs (the autoscaling/v1 Scale wrapper's
// metadata carries no annotations), pods/eviction CREATEs (the Eviction body is
// client-synthesized), or CONNECT operations (exec/attach options objects) — and for
// DELETE it only matches objects that ALREADY carry the annotation. Deployments that
// need those paths gated must identify the agent by serviceaccount instead
// (ServiceAccountMatcher, or AnyMatcher combining both).
type AnnotationMatcher struct{ Key string }

func (m AnnotationMatcher) Matches(_ *admissionv1.AdmissionRequest, object, oldObject *unstructured.Unstructured) bool {
	if m.Key == "" {
		return true
	}
	return hasAnnotation(object, m.Key) || hasAnnotation(oldObject, m.Key)
}

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

// NewServiceAccountMatcher builds a matcher over the given usernames.
func NewServiceAccountMatcher(usernames []string) ServiceAccountMatcher {
	set := make(map[string]bool, len(usernames))
	for _, u := range usernames {
		if u != "" {
			set[u] = true
		}
	}
	return ServiceAccountMatcher{Usernames: set}
}

func (m ServiceAccountMatcher) Matches(req *admissionv1.AdmissionRequest, _, _ *unstructured.Unstructured) bool {
	return m.Usernames[req.UserInfo.Username]
}

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
