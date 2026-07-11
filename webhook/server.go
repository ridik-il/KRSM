package webhook

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/internal/cluster"
	"github.com/ridik-il/krsm/scope"
)

// reasonCode is the fail-closed taxonomy (design §Data model): every deny carries
// exactly one code so operators and the e2e can grep the class, and audit-mode
// softening applies ONLY to reasonEscape (the computed, explained case).
type reasonCode string

const (
	reasonNotReady reasonCode = "not-ready" // cache not synced — unknown closure
	reasonStale    reasonCode = "stale"     // staleness drift unresolved / missing rv
	reasonEscape   reasonCode = "escape"    // closure computed; scope verdict applies
	reasonInvalid  reasonCode = "invalid"   // malformed/unmappable request
	reasonInternal reasonCode = "internal"  // panic/encode/deadline
)

// reason renders one credential-free reason string: "krsm:<code>: <detail>". It is
// the ONLY reason format the webhook emits (approved plan lean 4).
func reason(code reasonCode, detail string) string {
	return fmt.Sprintf("krsm:%s: %s", code, detail)
}

// Config wires a Server. State/ScopeInfo/Synced are satisfied by *state.Provider.
type Config struct {
	State     closure.State
	ScopeInfo clusterInfo
	Synced    func() bool
	Mode      scope.Mode
	Matcher   AgentMatcher                     // nil → AnnotationMatcher{} (gate everything)
	Fresh     Freshness                        // nil → staleness guard disabled (hermetic tests only; serve always wires it)
	Timeout   time.Duration                    // per-request deadline; 0 → defaultRequestTimeout (must stay below the webhook config's timeoutSeconds)
	Logf      func(format string, args ...any) // nil → log.Printf
}

// Server evaluates AdmissionReviews against the indexed live state. Validating only:
// it never sets a patch and never mutates the request.
type Server struct {
	state   closure.State
	info    clusterInfo
	synced  func() bool
	mode    scope.Mode
	matcher AgentMatcher
	fresh   Freshness
	timeout time.Duration
	logf    func(string, ...any)
}

// New builds a Server, rejecting an incomplete Config: a webhook without state, a
// sync gate, or a valid mode cannot fail closed correctly, so it must not start.
func New(c Config) (*Server, error) {
	if c.State == nil || c.ScopeInfo == nil || c.Synced == nil {
		return nil, fmt.Errorf("webhook: Config needs State, ScopeInfo and Synced")
	}
	if c.Mode != scope.ModeAudit && c.Mode != scope.ModeEnforce {
		return nil, fmt.Errorf("webhook: invalid mode %q (want %q or %q)", c.Mode, scope.ModeAudit, scope.ModeEnforce)
	}
	logf := c.Logf
	if logf == nil {
		logf = log.Printf
	}
	matcher := c.Matcher
	if matcher == nil {
		matcher = AnnotationMatcher{} // gate everything — the conservative default
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	return &Server{state: c.State, info: c.ScopeInfo, synced: c.Synced, mode: c.Mode, matcher: matcher, fresh: c.Fresh, timeout: timeout, logf: logf}, nil
}

// Handle evaluates one decoded AdmissionReview and returns the response review. It is
// pure over its inputs, always echoes request.uid, and fails CLOSED on every
// uncertainty (DESIGN §5). ctx carries the per-request deadline.
func (s *Server) Handle(ctx context.Context, review admissionv1.AdmissionReview) admissionv1.AdmissionReview {
	req := review.Request
	if req == nil || (req.Kind == metav1.GroupVersionKind{}) {
		return respond(review, deny(reasonInvalid, "empty admission request"))
	}
	if !s.matcher.Matches(req) {
		// Not agent-originated: KRSM does not gate it (DESIGN §5).
		return respond(review, &admissionv1.AdmissionResponse{Allowed: true})
	}
	if req.Operation == admissionv1.Create && req.SubResource != "eviction" {
		// Nothing exists yet, so there is no closure to escape (closure.Verb has no
		// Create). pods/eviction is the one CREATE that is really a delete — gated.
		// Adoption-via-preset-ownerReference is the documented deferred vector
		// (umbrella design open Q7).
		return respond(review, &admissionv1.AdmissionResponse{Allowed: true})
	}
	if req.SubResource != "" && req.SubResource != "scale" && req.SubResource != "eviction" {
		// status/token/…: no relational effect — admitted (design §Data model).
		return respond(review, &admissionv1.AdmissionResponse{Allowed: true})
	}

	action, err := actionFromRequest(req, s.info)
	if err != nil {
		return respond(review, deny(reasonInvalid, err.Error()))
	}
	if !s.synced() {
		return respond(review, deny(reasonNotReady, "state not ready: informer caches not synced"))
	}

	pred := scope.Derive(action.Target)
	dec := closure.Safe(s.state, action, pred.Clauses)

	// Staleness guard (C2, ADR-0004): confirm the cache is current for the request,
	// bounded to the closure neighbourhood, then recompute ONCE over the (possibly
	// reconciled) cache. Eviction carries no resourceVersion (the Eviction object is
	// synthetic) and skips the rv comparison — the documented residual window applies.
	if s.fresh != nil && req.SubResource != "eviction" {
		rv := resourceVersionOf(req.OldObject.Raw)
		if rv == "" {
			return respond(review, deny(reasonStale, "could not confirm current state: request carries no resourceVersion"))
		}
		if err := s.fresh.CheckFreshness(ctx, action.Target, rv, dec.Closure); err != nil {
			// The error itself is not echoed (it may name internals); the code is enough.
			s.logf("krsm/webhook: staleness guard failed for %s: %v", action.Target.String(), err)
			return respond(review, deny(reasonStale, "could not confirm current state"))
		}
		dec = closure.Safe(s.state, action, pred.Clauses)
	}

	dec = s.mode.Apply(dec)
	return respond(review, decisionResponse(dec, pred.Provenance))
}

// decisionResponse maps the applied Decision to an AdmissionResponse: Block denies
// with the escape reason; Warn allows with warnings naming the refs + provenance;
// Allow admits silently. Reasons carry refs (Kind/ns/name), never object contents.
func decisionResponse(dec closure.Decision, prov scope.Provenance) *admissionv1.AdmissionResponse {
	switch dec.Verdict {
	case closure.Block:
		return deny(reasonEscape, fmt.Sprintf("%s [scope: %s]%s", dec.Reason, prov, refsSuffix(" escaping:", dec.Escaping)))
	case closure.Warn:
		return &admissionv1.AdmissionResponse{
			Allowed: true,
			Warnings: []string{
				reason(reasonEscape, fmt.Sprintf("%s [scope: %s]%s%s", dec.Reason, prov,
					refsSuffix(" escaping:", dec.Escaping), refsSuffix(" external:", dec.External))),
			},
		}
	default:
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
}

func refsSuffix(label string, refs []closure.Ref) string {
	if len(refs) == 0 {
		return ""
	}
	parts := make([]string, len(refs))
	for i, r := range refs {
		parts[i] = r.String()
	}
	return label + " " + strings.Join(parts, ", ")
}

// deny builds a fail-closed (or enforce) denial with one taxonomy reason.
func deny(code reasonCode, detail string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: false,
		Result:  &metav1.Status{Message: reason(code, detail), Reason: "KRSMDenied"},
	}
}

// respond stamps the response review: same TypeMeta, uid echoed from the request
// (required by the admission contract), never a patch.
func respond(review admissionv1.AdmissionReview, resp *admissionv1.AdmissionResponse) admissionv1.AdmissionReview {
	if review.Request != nil {
		resp.UID = review.Request.UID
	}
	return admissionv1.AdmissionReview{TypeMeta: review.TypeMeta, Response: resp}
}

// clusterInfo needs cluster.ScopeInfo embedded; keep the compile-time proof local.
var _ cluster.ScopeInfo = clusterInfo(nil)
