package webhook

import (
	"encoding/json"

	admissionv1 "k8s.io/api/admission/v1"
)

// AgentMatcher decides whether a request is agent-originated. KRSM gates ONLY those;
// everything else is admitted untouched (DESIGN §5 — the webhook must never wedge
// human/controller traffic). Slice 6 may add serviceaccount-identity matching behind
// this same interface.
type AgentMatcher interface {
	Matches(req *admissionv1.AdmissionRequest) bool
}

// AnnotationMatcher matches when the request's object (or, for DELETE, its oldObject)
// carries the annotation Key with any value. The zero Key matches EVERYTHING — the
// e2e/test hook and the safe default when no matcher is configured (over-gating is
// conservative; under-gating lets an agent bypass).
type AnnotationMatcher struct{ Key string }

func (m AnnotationMatcher) Matches(req *admissionv1.AdmissionRequest) bool {
	if m.Key == "" {
		return true
	}
	return hasAnnotation(req.Object.Raw, m.Key) || hasAnnotation(req.OldObject.Raw, m.Key)
}

// hasAnnotation peeks metadata.annotations[key] existence without a full projection —
// the matcher runs BEFORE actionFromRequest, on requests of any (even ungated) kind.
func hasAnnotation(raw []byte, key string) bool {
	if len(raw) == 0 {
		return false
	}
	var peek struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return false
	}
	_, ok := peek.Metadata.Annotations[key]
	return ok
}
