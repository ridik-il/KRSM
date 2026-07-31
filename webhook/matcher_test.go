package webhook

import (
	"context"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"

	"github.com/ridik-il/krsm/scope"
)

// TestHandleNonAgentRequestAdmitted (design test 10): KRSM gates ONLY agent-originated
// requests. With an annotation matcher configured, a request whose object lacks the
// annotation is admitted untouched — even one that would escape scope — while the SAME
// request WITH the annotation is denied. The default matcher (MatchAll) gates all.
func TestHandleNonAgentRequestAdmitted(t *testing.T) {
	withKey := func(c *Config) { c.Matcher = AnnotationMatcher{Key: "krsm.io/task"} }

	// Human-originated: no annotation anywhere → admitted despite the escape.
	s := newTestServer(t, cascadeState(), scope.ModeEnforce, withKey)
	out := s.Handle(context.Background(), deleteReview("req-human"))
	if !out.Response.Allowed {
		t.Errorf("non-agent request must be admitted untouched, got deny %q", out.Response.Result.Message)
	}

	// Agent-originated: oldObject carries the annotation → gated (denied, escaping).
	agent := deleteReview("req-agent")
	agent.Request.OldObject = raw(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"prod","uid":"uid-d","resourceVersion":"7","annotations":{"krsm.io/task":"t-42"}}}`)
	out = s.Handle(context.Background(), agent)
	if out.Response.Allowed {
		t.Error("agent-annotated escaping delete must be gated (denied in enforce)")
	}

	// The default matcher gates everything (newTestServer sets no matcher → MatchAll).
	all := newTestServer(t, cascadeState(), scope.ModeEnforce)
	if out := all.Handle(context.Background(), deleteReview("req-any")); out.Response.Allowed {
		t.Error("the default (MatchAll) matcher must gate every request")
	}
}

// matches decodes a request's payloads (as Handle does) and runs the matcher over
// them — the test-side mirror of the production decode→match sequence.
func matches(t *testing.T, m AgentMatcher, req *admissionv1.AdmissionRequest) bool {
	t.Helper()
	oldU, err := decodePayload(req.OldObject.Raw)
	if err != nil {
		t.Fatalf("decode oldObject: %v", err)
	}
	newU, err := decodePayload(req.Object.Raw)
	if err != nil {
		t.Fatalf("decode object: %v", err)
	}
	return m.Matches(req, newU, oldU)
}

// TestAnnotationMatcher pins the matcher contract directly: object first, oldObject as
// fallback, any value counts, empty key matches NOTHING (a disabled matcher).
func TestAnnotationMatcher(t *testing.T) {
	m := AnnotationMatcher{Key: "krsm.io/task"}
	cases := []struct {
		name string
		req  *admissionv1.AdmissionRequest
		want bool
	}{
		{"no payloads", &admissionv1.AdmissionRequest{}, false},
		{"annotation on object", &admissionv1.AdmissionRequest{
			Object: raw(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p","annotations":{"krsm.io/task":"x"}}}`)}, true},
		{"annotation on oldObject", &admissionv1.AdmissionRequest{
			OldObject: raw(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p","annotations":{"krsm.io/task":""}}}`)}, true},
		{"other annotation only", &admissionv1.AdmissionRequest{
			Object: raw(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p","annotations":{"team":"a"}}}`)}, false},
	}
	for _, c := range cases {
		if got := matches(t, m, c.req); got != c.want {
			t.Errorf("%s: Matches = %v, want %v", c.name, got, c.want)
		}
	}
	if matches(t, AnnotationMatcher{}, &admissionv1.AdmissionRequest{}) {
		t.Error(`AnnotationMatcher{Key:""} must match NOTHING (a disabled matcher)`)
	}
}
