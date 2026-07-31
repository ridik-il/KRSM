package webhook

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/types"
)

// maxReviewBytes caps the request body: an AdmissionReview carries at most two object
// payloads (the API server's own object-size cap is ~1.5MiB each), so 8MiB is
// generous; anything larger is not an admission request the gate should trust.
const maxReviewBytes = 8 << 20

// DefaultRequestTimeout bounds one verdict (S1 #15). It must stay BELOW the
// ValidatingWebhookConfiguration's timeoutSeconds (max 30s) so KRSM answers with a
// fail-closed deny instead of the API server timing the webhook out. Exported so the
// serve command's --request-timeout default is the single source of this value.
const DefaultRequestTimeout = 10 * time.Second

// ServeHTTP is the impure edge around Handle: strict method/content-type/body gates,
// the per-request deadline, panic recovery to a fail-closed deny, and JSON encoding.
// A malformed body whose request.uid IS recoverable gets a 200 + deny(invalid) — the
// API server can address that verdict; a body with no recoverable uid is a 400 (there
// is nothing to address a response to). Every panic or internal failure with a known
// uid is a 200 + DENY — the API server must always receive a verdict, and that
// verdict is never an allow.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// Compare the media type, not the raw header: an intermediary adding a
	// parameter ("application/json; charset=utf-8") must not turn every admission
	// into a webhook failure (415 + failurePolicy would gate the whole cluster).
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxReviewBytes))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil {
		// Strict decode failed. If the request's uid is still recoverable from the
		// raw JSON, answer it with a fail-closed deny (design §Behavior); only a
		// body with no addressable uid is a 400.
		uid := peekRequestUID(body)
		if uid == "" {
			http.Error(w, "malformed AdmissionReview", http.StatusBadRequest)
			return
		}
		review = admissionv1.AdmissionReview{Request: &admissionv1.AdmissionRequest{UID: types.UID(uid)}}
		s.writeReview(w, respond(review, deny(reasonInvalid, "malformed AdmissionReview")))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	s.writeReview(w, s.handleSafe(ctx, review))
}

// peekRequestUID loosely extracts request.uid from a body that failed the strict
// AdmissionReview decode — the one field needed to address a fail-closed deny.
func peekRequestUID(body []byte) string {
	var peek struct {
		Request struct {
			UID string `json:"uid"`
		} `json:"request"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		return ""
	}
	return peek.Request.UID
}

// writeReview encodes one response review (respond has already stamped TypeMeta).
func (s *Server) writeReview(w http.ResponseWriter, out admissionv1.AdmissionReview) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		s.logf("krsm/webhook: encode response: %v", err)
	}
}

// handleSafe runs Handle with panic recovery: a panicking verdict path yields a
// fail-closed deny (reasonInternal), never a missing or allowing response.
func (s *Server) handleSafe(ctx context.Context, review admissionv1.AdmissionReview) (out admissionv1.AdmissionReview) {
	defer func() {
		if p := recover(); p != nil {
			s.logf("krsm/webhook: recovered panic serving admission: %v", p)
			out = respond(review, deny(reasonInternal, "internal error"))
		}
	}()
	return s.Handle(ctx, review)
}

// NewMux wires the Server's endpoints: /validate (the webhook), /readyz (200 only
// once the informer caches are synced — the deployment's readiness probe), /healthz
// (process liveness, never dependent on the API server).
func NewMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/validate", s)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.synced() {
			http.Error(w, "informer caches not synced", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
