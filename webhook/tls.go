package webhook

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// defaultProbeInterval bounds how often GetCertificate stats the cert/key files for a
// rotation. Between probes the cached pair is served lock-free. cert-manager rotation
// windows are ~weeks, so a ≤10s delay in noticing a new pair is irrelevant, and the
// rate limit keeps the stat off the common handshake path.
const defaultProbeInterval = 10 * time.Second

// CertReloader serves the TLS certificate for tls.Config.GetCertificate, re-reading the
// cert/key pair when either file's mtime changes — so a cert-manager rotation (slice 7)
// is picked up without a restart. mtime polling is deliberate: no fsnotify dependency.
// The served pair lives in an atomic pointer so the hot GetCertificate path is
// lock-free; the mutex serialises only the (rate-limited) stat+reload.
type CertReloader struct {
	certPath, keyPath string
	probeInterval     time.Duration

	cert atomic.Pointer[tls.Certificate] // lock-free fast path

	mu        sync.Mutex // serialises probe + reload, held OFF the common path
	lastProbe time.Time
	certMtime time.Time // cert file mtime at last successful load
	keyMtime  time.Time // key file mtime at last successful load
}

// NewCertReloader loads the initial pair, failing fast on an unreadable or invalid cert
// (a webhook that cannot present its cert must not start).
func NewCertReloader(certPath, keyPath string) (*CertReloader, error) {
	r := &CertReloader{certPath: certPath, keyPath: keyPath, probeInterval: defaultProbeInterval}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// GetCertificate implements tls.Config.GetCertificate. It returns the cached pair
// lock-free; at most once per probeInterval it stats BOTH files under the mutex and, if
// either changed, reloads. A stat or reload error keeps the PREVIOUS pair serving (a
// half-written rotation must not kill live handshakes); the initial load already proved
// a valid pair exists.
func (r *CertReloader) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.maybeReload()
	return r.cert.Load(), nil
}

// maybeReload stats both files at most once per probeInterval and reloads on any mtime
// change. All errors are swallowed: the previously loaded pair keeps serving.
func (r *CertReloader) maybeReload() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if now.Sub(r.lastProbe) < r.probeInterval {
		return
	}
	r.lastProbe = now
	certFI, err := os.Stat(r.certPath)
	if err != nil {
		return
	}
	keyFI, err := os.Stat(r.keyPath)
	if err != nil {
		return
	}
	if certFI.ModTime().Equal(r.certMtime) && keyFI.ModTime().Equal(r.keyMtime) {
		return
	}
	_ = r.reloadLocked() // a half-written pair keeps the previous cert (already loaded)
}

// reload takes the lock and loads — used by the constructor, before any concurrency.
func (r *CertReloader) reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reloadLocked()
}

// reloadLocked reads the pair and, ONLY on success, records both files' mtimes and
// publishes the new certificate via the atomic pointer. Caller holds mu.
func (r *CertReloader) reloadLocked() error {
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return fmt.Errorf("load TLS key pair: %w", err)
	}
	certFI, err := os.Stat(r.certPath)
	if err != nil {
		return fmt.Errorf("stat cert: %w", err)
	}
	keyFI, err := os.Stat(r.keyPath)
	if err != nil {
		return fmt.Errorf("stat key: %w", err)
	}
	r.cert.Store(&cert)
	r.certMtime, r.keyMtime = certFI.ModTime(), keyFI.ModTime()
	return nil
}
