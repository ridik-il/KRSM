package webhook

import (
	"context"
	"fmt"
	"log"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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
	// reasonUntracked: the target's kind is not watched by the informer set, so its
	// closure cannot be computed (round-2 finding 4). Distinct from not-ready
	// (transient sync) and escape (computed) — a correct install generates the webhook
	// rules from the tracked set, so this state is unreachable in production (slice 7).
	reasonUntracked reasonCode = "untracked"
	// reasonScopeUnresolved: the request DECLARED a scope (krsm.io/target, and later
	// krsm.io/scope) that is well-formed but cannot be resolved — an unknown or
	// ambiguous kind token, a missing/unreadable contract. Distinct from invalid
	// (malformed syntax) so operators can grep a typo apart from a broken reference.
	// It fails closed in BOTH modes: audit softens only a COMPUTED escape, never an
	// unverifiable scope claim, and falling back to L0 would hide the declared intent.
	reasonScopeUnresolved reasonCode = "scope-unresolved"
)

// provenanceAuditKey is the response AuditAnnotations key carrying the resolved scope
// provenance (ADR-0011). The API server copies it into its audit record, so operators
// can query how a verdict's scope arose — machine-readable, rather than only inside the
// human status message / warning text.
const provenanceAuditKey = "krsm.io/scope-provenance"

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
	Matcher   AgentMatcher                     // nil → MatchAll{} (gate everything — library-conservative)
	Fresh     Freshness                        // required (New rejects nil); NoFreshness explicitly disables the staleness guard
	Timeout   time.Duration                    // per-request deadline; 0 → DefaultRequestTimeout (must stay below the webhook config's timeoutSeconds)
	Allowlist Allowlist                        // cross-boundary escape hatch for derived scopes; zero value exempts nothing
	Logf      func(format string, args ...any) // nil → log.Printf
}

// Server evaluates AdmissionReviews against the indexed live state. Validating only:
// it never sets a patch and never mutates the request.
type Server struct {
	state     closure.State
	info      clusterInfo
	synced    func() bool
	mode      scope.Mode
	matcher   AgentMatcher
	fresh     Freshness
	timeout   time.Duration
	allowlist Allowlist
	logf      func(string, ...any)
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
	if c.Fresh == nil {
		return nil, fmt.Errorf("webhook: Config needs Fresh (use webhook.NoFreshness to disable the staleness guard)")
	}
	logf := c.Logf
	if logf == nil {
		logf = log.Printf
	}
	matcher := c.Matcher
	if matcher == nil {
		matcher = MatchAll{} // gate everything — the library-conservative default
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	return &Server{state: c.State, info: c.ScopeInfo, synced: c.Synced, mode: c.Mode, matcher: matcher, fresh: c.Fresh, timeout: timeout, allowlist: c.Allowlist, logf: logf}, nil
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
	if gate, ok := gatedSubResource(req.SubResource); req.Operation == admissionv1.Create && (!ok || !gate.viaCreate) {
		// Nothing exists yet, so there is no closure to escape (closure.Verb has no
		// Create). pods/eviction is the one CREATE that is really a delete (viaCreate)
		// — gated. Adoption-via-preset-ownerReference is the documented deferred vector
		// (umbrella design open Q7). Admitted before matcher/decode: the verdict is
		// identical for agents and non-agents, so this hottest path stays free.
		return respond(review, admit())
	}
	if _, gated := gatedSubResource(req.SubResource); req.SubResource != "" && !gated && req.Operation != admissionv1.Connect {
		// status/token/…: no relational effect — admitted (design §Data model),
		// payload-free. CONNECT always carries a sub-resource (exec/attach/…) and
		// must NOT be admitted here — it falls through to the agent-matched
		// fail-closed deny below.
		return respond(review, admit())
	}

	// Two-stage agent match (round-2 finding 5). Stage 1 is payload-free: an identity
	// matcher decides from req.userInfo alone, so the non-agent hot path never decodes.
	// A stage-1 miss decodes and re-matches ONLY when the matcher says the payload could
	// change the answer (NeedsPayload) — and an undecodable payload there cannot carry
	// the annotation, so it admits (restoring round-1 channel semantics), while an
	// identity-matched agent's undecodable payload still fails closed below.
	var oldU, newU *unstructured.Unstructured
	if s.matcher.Matches(req, nil, nil) {
		var err error
		if oldU, err = decodePayload(req.OldObject.Raw); err != nil {
			return respond(review, deny(reasonInvalid, "oldObject: "+err.Error()))
		}
		if newU, err = decodePayload(req.Object.Raw); err != nil {
			return respond(review, deny(reasonInvalid, "object: "+err.Error()))
		}
	} else if s.matcher.NeedsPayload() {
		var err error
		if oldU, err = decodePayload(req.OldObject.Raw); err != nil {
			return respond(review, admit())
		}
		if newU, err = decodePayload(req.Object.Raw); err != nil {
			return respond(review, admit())
		}
		if !s.matcher.Matches(req, newU, oldU) {
			return respond(review, admit())
		}
	} else {
		// Not agent-originated and the payload cannot change that: admit (DESIGN §5).
		return respond(review, admit())
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
	// Untracked precheck (finding 4): a kind the informers do not watch has no closure
	// to compute — fail closed with a distinct code, AFTER the sync gate (so a syncing
	// cache reports not-ready, never untracked) and BEFORE the closure walk. A correct
	// install (slice 7) generates the webhook rules from the tracked set, making this
	// state unreachable in production.
	if !s.info.Tracked(action.Target.GVK) {
		return respond(review, deny(reasonUntracked, fmt.Sprintf("kind %s/%s is not tracked by the informer set; a correct install generates webhook rules from the tracked set", action.Target.GVK.Group, action.Target.GVK.Kind)))
	}

	// Staleness precheck (C2, ADR-0004): the authoritative rv is the one the API
	// server stamped on the request's oldObject; without it currency cannot be
	// confirmed — deny BEFORE paying for the closure walk. A !carriesRV sub-resource
	// (eviction: the Eviction object is synthetic) skips the guard — the documented
	// residual window applies. NoFreshness disables it wholesale.
	carriesRV := true
	if gate, ok := gatedSubResource(req.SubResource); ok {
		carriesRV = gate.carriesRV
	}
	guarded := s.fresh != NoFreshness && carriesRV
	var targetRV string
	if guarded {
		if oldU != nil {
			targetRV = oldU.GetResourceVersion()
		}
		if targetRV == "" {
			return respond(review, deny(reasonStale, "could not confirm current state: request carries no resourceVersion"))
		}
	}

	// Scope channel (ADR-0011): L1 krsm.io/target re-root over L0 derived, read from
	// oldObject only. An unresolvable declared scope denies here — never a silent
	// downgrade to the derived tree.
	pred, rc, err := s.resolveScope(ctx, req, oldU, action.Target)
	if err != nil {
		return respond(review, deny(rc, err.Error()))
	}
	dec := closure.Safe(s.state, action, pred.Clauses)

	// Staleness guard: confirm the cache is current for the request, bounded to the
	// closure neighbourhood. Recompute ONLY when the guard actually reconciled the cache
	// (finding 9): a steady-state request whose cache was already current serves its
	// first decision without a second closure walk.
	if guarded {
		reconciled, err := s.fresh.CheckFreshness(ctx, action.Target, targetRV, dec.Closure)
		if err != nil {
			// The error itself is not echoed (it may name internals); the code is enough.
			s.logf("krsm/webhook: staleness guard failed for %s: %v", action.Target.String(), err)
			return respond(review, deny(reasonStale, "could not confirm current state"))
		}
		if reconciled {
			dec = closure.Safe(s.state, action, pred.Clauses)
		}
	}

	if ctx.Err() != nil {
		// Deadline expired outside the freshness seam (projection/closure walk):
		// the taxonomy's "else internal" branch — never serve a verdict computed
		// past its deadline as if it were timely.
		return respond(review, deny(reasonInternal, "request deadline exceeded"))
	}

	// Cross-boundary allowlist: for DERIVED scopes only, drop the exempt INDIRECT
	// escapes before the mode is applied, so audit's downgrade describes the RESIDUAL
	// escape set an operator actually has to act on.
	if allowlistApplies(pred.Provenance) {
		dec = s.allowlist.filterEscapes(dec, action.Target)
	}

	dec = s.mode.Apply(dec)
	resp := decisionResponse(dec, pred.Provenance)
	// Structured provenance on every DECIDED response (Allow included — an Allow is the
	// case whose message says nothing). Denials raised before resolveScope ran carry no
	// provenance: an audit record must not report a scope that was never resolved.
	resp.AuditAnnotations = map[string]string{provenanceAuditKey: string(pred.Provenance)}
	return respond(review, resp)
}

// decisionResponse maps the applied Decision to an AdmissionResponse: Block denies
// with the escape reason; Warn allows with warnings; Allow admits silently. Both
// non-Allow branches share ONE message grammar (verdict reason, provenance, escaping
// and external refs) so enforce denies and audit warnings name the same facts and the
// slice-8 e2e greps one format. Reasons carry refs (Kind/ns/name), never contents.
func decisionResponse(dec closure.Decision, prov scope.Provenance) *admissionv1.AdmissionResponse {
	if dec.Verdict == closure.Allow {
		return admit() // no message building on the common in-scope path
	}
	msg := fmt.Sprintf("%s [scope: %s]%s%s", dec.Reason, prov,
		refsSuffix(" escaping:", dec.Escaping), refsSuffix(" external:", dec.External))
	if dec.Verdict == closure.Block {
		return deny(reasonEscape, msg)
	}
	return &admissionv1.AdmissionResponse{ // Warn: allow with the same facts as a Block
		Allowed:  true,
		Warnings: []string{reason(reasonEscape, msg)},
	}
}

func refsSuffix(label string, refs []closure.Ref) string {
	if len(refs) == 0 {
		return ""
	}
	return label + " " + closure.JoinRefs(refs)
}

// deny builds a fail-closed (or enforce) denial with one taxonomy reason.
func deny(code reasonCode, detail string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: false,
		Result:  &metav1.Status{Message: reason(code, detail), Reason: "KRSMDenied"},
	}
}

// admit is the shared silent-allow response — every payload-free admit and the
// closure-Allow verdict return it (uid is stamped by respond).
func admit() *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{Allowed: true}
}

// respond stamps the response review: it ECHOES the request review's TypeMeta when the
// request carried a complete one (finding 6 — a v1beta1 request must get a v1beta1
// response, not a hardcoded v1), and stamps admission/v1 otherwise (the API server
// rejects a response without a TypeMeta). uid is echoed from the request (required by
// the admission contract); a patch is never set.
func respond(review admissionv1.AdmissionReview, resp *admissionv1.AdmissionResponse) admissionv1.AdmissionReview {
	if review.Request != nil {
		resp.UID = review.Request.UID
	}
	tm := review.TypeMeta
	if tm.APIVersion == "" || tm.Kind == "" {
		tm = metav1.TypeMeta{APIVersion: admissionv1.SchemeGroupVersion.String(), Kind: "AdmissionReview"}
	}
	return admissionv1.AdmissionReview{TypeMeta: tm, Response: resp}
}
