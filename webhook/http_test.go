package webhook

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"

	"github.com/ridik-il/krsm/closure"
	"github.com/ridik-il/krsm/scope"
)

func postReview(t *testing.T, h http.Handler, review admissionv1.AdmissionReview) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(review)
	if err != nil {
		t.Fatalf("marshal review: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/validate", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) admissionv1.AdmissionReview {
	t.Helper()
	var out admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response (status %d, body %q): %v", rec.Code, rec.Body.String(), err)
	}
	return out
}

// TestServeHTTPStrictness (design test 17): non-POST → 405; wrong content-type → 415;
// an undecodable body (no uid recoverable) → 400. A well-formed review round-trips
// with 200 and the verdict.
func TestServeHTTPStrictness(t *testing.T) {
	mux := NewMux(newTestServer(t, cascadeState(), scope.ModeEnforce))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/validate", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /validate = %d, want 405", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/validate", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "text/plain")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain POST = %d, want 415", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/validate", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("garbage body = %d, want 400 (no uid to address a deny to)", rec.Code)
	}

	rec = postReview(t, mux, deleteReview("req-http"))
	if rec.Code != http.StatusOK {
		t.Fatalf("valid review = %d, want 200", rec.Code)
	}
	out := decodeResponse(t, rec)
	if out.Response == nil || out.Response.Allowed || out.Response.UID != "req-http" {
		t.Errorf("response = %+v, want a uid-echoed deny of the escaping delete", out.Response)
	}
}

// panicState poisons one State method so the handler panics mid-verdict.
type panicState struct{ closure.State }

func (panicState) OwnedChildren(closure.Ref) []closure.Ref { panic("poisoned index") }

// TestServeHTTPPanicFailsClosed (design test 18): a handler panic is a 200 +
// krsm:internal DENY — the API server must receive a verdict, and that verdict is
// never an allow.
func TestServeHTTPPanicFailsClosed(t *testing.T) {
	mux := NewMux(newTestServer(t, panicState{cascadeState()}, scope.ModeEnforce))
	rec := postReview(t, mux, deleteReview("req-panic"))
	if rec.Code != http.StatusOK {
		t.Fatalf("panic path = %d, want 200 with a deny body", rec.Code)
	}
	out := decodeResponse(t, rec)
	if out.Response.Allowed {
		t.Error("a panicking handler must fail closed")
	}
	if !strings.Contains(out.Response.Result.Message, "krsm:internal") {
		t.Errorf("message %q must carry krsm:internal", out.Response.Result.Message)
	}
	if out.Response.UID != "req-panic" {
		t.Errorf("uid = %q, want echoed req-panic", out.Response.UID)
	}
}

// TestReadyzHealthz (design test 20): /readyz reflects cache sync (503 → 200);
// /healthz is always 200 (liveness must not depend on the API server).
func TestReadyzHealthz(t *testing.T) {
	synced := false
	s := newTestServer(t, cascadeState(), scope.ModeAudit, func(c *Config) { c.Synced = func() bool { return synced } })
	mux := NewMux(s)

	get := func(path string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	if got := get("/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("/readyz unsynced = %d, want 503", got)
	}
	if got := get("/healthz"); got != http.StatusOK {
		t.Errorf("/healthz = %d, want 200 always", got)
	}
	synced = true
	if got := get("/readyz"); got != http.StatusOK {
		t.Errorf("/readyz synced = %d, want 200", got)
	}
}

// blockingFresh models a hung API server inside the staleness guard: it returns only
// when the request context is cancelled.
type blockingFresh struct{}

func (blockingFresh) CheckFreshness(ctx context.Context, _ closure.Ref, _ string, _ []closure.Ref) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

// TestServeHTTPDeadlineDeniesInsteadOfHanging (design test 19, S1 #15): the
// per-request deadline bounds a hung freshness GET — the API server receives a
// fail-closed deny well before its own timeoutSeconds, not a hang.
func TestServeHTTPDeadlineDeniesInsteadOfHanging(t *testing.T) {
	s := newTestServer(t, cascadeState(), scope.ModeEnforce, func(c *Config) {
		c.Fresh = blockingFresh{}
		c.Timeout = 50 * time.Millisecond
	})
	done := make(chan admissionv1.AdmissionReview, 1)
	go func() {
		rec := postReview(t, NewMux(s), deleteReview("req-hang"))
		done <- decodeResponse(t, rec)
	}()
	select {
	case out := <-done:
		if out.Response.Allowed {
			t.Error("a deadline inside the guard must deny")
		}
		if !strings.Contains(out.Response.Result.Message, "krsm:stale") {
			t.Errorf("message %q must carry krsm:stale (the guard could not confirm)", out.Response.Result.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request hung past the deadline")
	}
}

// TestCertReloaderPicksUpRotation (design test 21): after the cert file changes on
// disk, the next handshake serves the NEW certificate — cert-manager rotation must
// not require a restart.
func TestCertReloaderPicksUpRotation(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := dir+"/tls.crt", dir+"/tls.key"

	writeSelfSigned(t, certPath, keyPath, "first.example")
	r, err := NewCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	r.probeInterval = 0 // probe on every handshake (production rate-limits to 10s)
	first, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}

	time.Sleep(1100 * time.Millisecond) // mtime granularity on coarse filesystems
	writeSelfSigned(t, certPath, keyPath, "second.example")
	second, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate after rotation: %v", err)
	}
	if string(first.Certificate[0]) == string(second.Certificate[0]) {
		t.Error("rotated certificate was not picked up")
	}
	leaf, err := x509.ParseCertificate(second.Certificate[0])
	if err != nil {
		t.Fatalf("parse rotated leaf: %v", err)
	}
	if leaf.Subject.CommonName != "second.example" {
		t.Errorf("served CN = %q, want the rotated second.example", leaf.Subject.CommonName)
	}
}

// TestCertReloaderStatsKeyFile (design test 15): a rotation the cert file's mtime does
// NOT reflect (only the key file changed) is still picked up — both files are stat'd.
func TestCertReloaderStatsKeyFile(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := dir+"/tls.crt", dir+"/tls.key"
	writeSelfSigned(t, certPath, keyPath, "first.example")
	r, err := NewCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	r.probeInterval = 0
	first, _ := r.GetCertificate(nil)

	certInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("stat cert: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	writeSelfSigned(t, certPath, keyPath, "second.example")
	// Reset the cert file's mtime so ONLY the key file's mtime differs from the load.
	if err := os.Chtimes(certPath, certInfo.ModTime(), certInfo.ModTime()); err != nil {
		t.Fatalf("reset cert mtime: %v", err)
	}
	second, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if string(first.Certificate[0]) == string(second.Certificate[0]) {
		t.Error("a key-file-only mtime change must be picked up (both files are stat'd)")
	}
}

// TestCertReloaderHalfWrittenKeepsPrevious (design test 15): a half-written rotation (a
// cert file replaced before its matching key) must not kill live handshakes — the
// previous pair keeps serving and GetCertificate never errors.
func TestCertReloaderHalfWrittenKeepsPrevious(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := dir+"/tls.crt", dir+"/tls.key"
	writeSelfSigned(t, certPath, keyPath, "first.example")
	r, err := NewCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	r.probeInterval = 0
	first, _ := r.GetCertificate(nil)

	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(certPath, []byte("-----BEGIN CERTIFICATE-----\nnot a real cert\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatalf("write half-rotation: %v", err)
	}
	second, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate must not error on a half-written pair: %v", err)
	}
	if string(first.Certificate[0]) != string(second.Certificate[0]) {
		t.Error("a half-written rotation must keep serving the previous certificate")
	}
}

// TestCertReloaderConcurrentGet (design test 15): concurrent GetCertificate calls during
// a rotation are race-free (run under -race) — the atomic pointer + rate-limited stat.
func TestCertReloaderConcurrentGet(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := dir+"/tls.crt", dir+"/tls.key"
	writeSelfSigned(t, certPath, keyPath, "first.example")
	r, err := NewCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	r.probeInterval = 0

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.GetCertificate(nil); err != nil {
				t.Errorf("GetCertificate: %v", err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		writeSelfSigned(t, certPath, keyPath, "second.example")
	}()
	wg.Wait()
}

// writeSelfSigned writes a minimal self-signed cert+key pair for cn.
func writeSelfSigned(t *testing.T, certPath, keyPath, cn string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}
