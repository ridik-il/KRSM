package webhook

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"time"
)

// CertReloader serves the TLS certificate for tls.Config.GetCertificate, re-reading
// the cert/key pair when the cert file's mtime changes — so a cert-manager rotation
// (slice 7) is picked up on the next handshake without a restart. mtime polling is
// deliberate: no fsnotify dependency, and rotation windows are ~weeks.
type CertReloader struct {
	certPath, keyPath string

	mu    sync.Mutex
	cert  *tls.Certificate
	mtime time.Time
}

// NewCertReloader loads the initial pair, failing fast on an unreadable or invalid
// cert (a webhook that cannot present its cert must not start).
func NewCertReloader(certPath, keyPath string) (*CertReloader, error) {
	r := &CertReloader{certPath: certPath, keyPath: keyPath}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// GetCertificate implements tls.Config.GetCertificate. On a stat or reload error the
// PREVIOUS cert keeps serving (a half-written rotation must not kill live handshakes);
// the initial load already proved a valid pair exists.
func (r *CertReloader) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fi, err := os.Stat(r.certPath); err == nil && fi.ModTime() != r.mtime {
		if err := r.load(); err != nil {
			return r.cert, nil
		}
	}
	return r.cert, nil
}

// load reads the pair and records the cert file's mtime. Caller holds mu (or is the
// constructor, before any concurrency).
func (r *CertReloader) load() error {
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return fmt.Errorf("load TLS key pair: %w", err)
	}
	fi, err := os.Stat(r.certPath)
	if err != nil {
		return fmt.Errorf("stat cert: %w", err)
	}
	r.cert, r.mtime = &cert, fi.ModTime()
	return nil
}
