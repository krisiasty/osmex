// Package listen secures the endpoint osmex serves: TLS toward the scraper, and authentication of the scraper.
package listen

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// reloadInterval bounds how often the certificate files are checked and the password file is re-read, so there is no
// stat on every handshake and no read on every request. Neither runs on a timer: a renewed certificate is picked up by
// the first handshake after the interval, a rotated password by the first request after it.
const reloadInterval = 30 * time.Second

// Options describes the listener's TLS.
type Options struct {
	CertFile     string
	KeyFile      string
	ClientCAFile string // when set, clients must present a certificate signed by this bundle
}

// TLSConfig builds the server's TLS configuration, or returns nil when no certificate is configured.
func TLSConfig(o Options) (*tls.Config, error) {
	if o.CertFile == "" && o.KeyFile == "" {
		if o.ClientCAFile != "" {
			return nil, errors.New("a client CA requires a server certificate")
		}
		return nil, nil //nolint:nilnil // no TLS configured is a valid outcome
	}
	cert, err := newCertificate(o.CertFile, o.KeyFile)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert.get(time.Now()), nil },
	}
	if o.ClientCAFile != "" {
		pem, err := os.ReadFile(filepath.Clean(o.ClientCAFile))
		if err != nil {
			return nil, fmt.Errorf("read client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("client CA %s contains no certificate", o.ClientCAFile)
		}
		cfg.ClientCAs = pool
		// Verify, not just request: accepting a certificate that does not verify would be authentication in appearance only.
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// certificate is the server certificate, reloaded when its files change. cert-manager renews a certificate without
// restarting anything, so a certificate read only at startup would expire in memory while a valid one sat on disk.
type certificate struct {
	certFile, keyFile string

	mu      sync.RWMutex
	current *tls.Certificate
	stamp   stamp
	checked time.Time
}

// stamp is what a change to the files looks like without reading them.
type stamp struct {
	certSize, keySize int64
	certMod, keyMod   time.Time
}

func newCertificate(certFile, keyFile string) (*certificate, error) {
	c := &certificate{certFile: certFile, keyFile: keyFile}
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *certificate) load() error {
	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return fmt.Errorf("load certificate: %w", err)
	}
	s, err := c.stat()
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = &cert
	c.stamp = s
	c.checked = time.Now()
	return nil
}

func (c *certificate) stat() (stamp, error) {
	ci, err := os.Stat(c.certFile)
	if err != nil {
		return stamp{}, fmt.Errorf("stat certificate: %w", err)
	}
	ki, err := os.Stat(c.keyFile)
	if err != nil {
		return stamp{}, fmt.Errorf("stat key: %w", err)
	}
	return stamp{ci.Size(), ki.Size(), ci.ModTime(), ki.ModTime()}, nil
}

// get returns the certificate for a handshake, first reloading it if the interval has passed and the files changed.
// A failed reload keeps the certificate in memory: a half-written pair during a renewal must not take the endpoint down.
func (c *certificate) get(now time.Time) *tls.Certificate {
	c.mu.RLock()
	due := now.Sub(c.checked) >= reloadInterval
	cur := c.current
	c.mu.RUnlock()
	if !due {
		return cur
	}

	s, err := c.stat()
	c.mu.Lock()
	c.checked = now
	changed := err == nil && s != c.stamp
	c.mu.Unlock()
	if !changed || c.load() != nil {
		return cur
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current
}
