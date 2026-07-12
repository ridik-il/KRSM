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
// uncertainty (DESIGN §5). ctx carries the per-request deadline; if it expires outside
// the freshness seam the verdict is a deny(reasonInternal), never a silent overrun
// (design §Behavior: deadline → stale if in freshness, else internal).
//
// Gate order (cheapest first; every admit before payload decode is payload-free):
// invalid → CREATE admit → ungated-sub-resource admit → decode payloads once →
// agent match → CONNECT deny → action → sync gate → rv precheck → Safe → staleness
// guard → recompute → deadline check → mode → response.
func (s *Server) Handle(ctx context.Context, review admissionv1.AdmissionReview) admissionv1.AdmissionReview {
	req := review.Request
	if req == nil || (req.Kind == metav1.GroupVersionKind{}) {
		return respond(review, deny(reasonInvalid, "empty admission request"))
	}
	if req.Operation == admissionv1.Create && req.SubResource != "eviction" {
		// Nothing exists yet, so there is no closure to escape (closure.Verb has no
		// Create). pods/eviction is the one CREATE that is really a delete — gated.
		// Adoption-via-preset-ownerReference is the documented deferred vector
		// (umbrella design open Q7). Admitted before matcher/decode: the verdict is
		// identical for agents and non-agents, so this hottest path stays free.
		return respond(review, &admissionv1.AdmissionResponse{Allowed: true})
	}
	if req.SubResource != "" && !gatedSubResource(req.SubResource) && req.Operation != admissionv1.Connect {
		// status/token/…: no relational effect — admitted (design §Data model),
		// payload-free. CONNECT always carries a sub-resource (exec/attach/…) and
		// must NOT be admitted here — it falls through to the agent-matched
		// fail-closed deny below.
		return respond(review, &admissionv1.AdmissionResponse{Allowed: true})
	}

	// Decode each payload exactly once; the matcher, the resourceVersion precheck,
	// and the projection all read from these. Undecodable → fail closed (design
	// test 7): a payload the gate cannot read is an unknown closure.
	oldU, err := decodePayload(req.OldObject.Raw)
	if err != nil {
		return respond(review, deny(reasonInvalid, "oldObject: "+err.Error()))
	}
	newU, err := decodePayload(req.Object.Raw)
	if err != nil {
		return respond(review, deny(reasonInvalid, "object: "+err.Error()))
	}

	if !s.matcher.Matches(req, newU, oldU) {
		// Not agent-originated: KRSM does not gate it (DESIGN §5).
		return respond(review, &admissionv1.AdmissionResponse{Allowed: true})
	}
	if req.Operation == admissionv1.Connect {
		// An agent operation the gate cannot classify (exec/attach/portforward/…)
		// is an unknown closure — fail closed (approved lean 3). Non-agent CONNECTs
		// were admitted at the matcher above.
		return respond(review, deny(reasonInvalid, fmt.Sprintf("unclassifiable operation CONNECT (sub-resource %q)", req.SubResource)))
	}

	action, err := actionFromRequest(req, s.info, oldU, newU)
	if err != nil {
		return respond(review, deny(reasonInvalid, err.Error()))
	}
	if !s.synced() {
		return respond(review, deny(reasonNotReady, "state not ready: informer caches not synced"))
	}

	// Staleness precheck (C2, ADR-0004): the authoritative rv is the one the API
	// server stamped on the request's oldObject; without it currency cannot be
	// confirmed — deny BEFORE paying for the closure walk. Eviction carries no
	// resourceVersion (the Eviction object is synthetic) and skips the guard — the
	// documented residual window applies.
	guarded := s.fresh != nil && req.SubResource != "eviction"
	var targetRV string
	if guarded {
		if oldU != nil {
			targetRV = oldU.GetResourceVersion()
		}
		if targetRV == "" {
			return respond(review, deny(reasonStale, "could not confirm current state: request carries no resourceVersion"))
		}
	}

	pred := scope.Derive(action.Target)
	dec := closure.Safe(s.state, action, pred.Clauses)

	// Staleness guard: confirm the cache is current for the request, bounded to the
	// closure neighbourhood, then recompute ONCE over the (possibly reconciled) cache.
	if guarded {
		if err := s.fresh.CheckFreshness(ctx, action.Target, targetRV, dec.Closure); err != nil {
			// The error itself is not echoed (it may name internals); the code is enough.
			s.logf("krsm/webhook: staleness guard failed for %s: %v", action.Target.String(), err)
			return respond(review, deny(reasonStale, "could not confirm current state"))
		}
		dec = closure.Safe(s.state, action, pred.Clauses)
	}

	if ctx.Err() != nil {
		// Deadline expired outside the freshness seam (projection/closure walk):
		// the taxonomy's "else internal" branch — never serve a verdict computed
		// past its deadline as if it were timely.
		return respond(review, deny(reasonInternal, "request deadline exceeded"))
	}

	dec = s.mode.Apply(dec)
	return respond(review, decisionResponse(dec, pred.Provenance))
}

// decisionResponse maps the applied Decision to an AdmissionResponse: Block denies
// with the escape reason; Warn allows with warnings; Allow admits silently. Both
// non-Allow branches share ONE message grammar (verdict reason, provenance, escaping
// and external refs) so enforce denies and audit warnings name the same facts and the
// slice-8 e2e greps one format. Reasons carry refs (Kind/ns/name), never contents.
func decisionResponse(dec closure.Decision, prov scope.Provenance) *admissionv1.AdmissionResponse {
	msg := fmt.Sprintf("%s [scope: %s]%s%s", dec.Reason, prov,
		refsSuffix(" escaping:", dec.Escaping), refsSuffix(" external:", dec.External))
	switch dec.Verdict {
	case closure.Block:
		return deny(reasonEscape, msg)
	case closure.Warn:
		return &admissionv1.AdmissionResponse{
			Allowed:  true,
			Warnings: []string{reason(reasonEscape, msg)},
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

// respond stamps the response review: explicit admission/v1 TypeMeta (the API server
// rejects a response without one — stamped HERE so every Handle caller gets a complete
// review, not only ServeHTTP), uid echoed from the request (required by the admission
// contract), never a patch.
func respond(review admissionv1.AdmissionReview, resp *admissionv1.AdmissionResponse) admissionv1.AdmissionReview {
	if review.Request != nil {
		resp.UID = review.Request.UID
	}
	return admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: admissionv1.SchemeGroupVersion.String(), Kind: "AdmissionReview"},
		Response: resp,
	}
}
