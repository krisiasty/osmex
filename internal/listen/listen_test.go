package listen

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/ostest"
	"github.com/krisiasty/osmex/internal/secret"
)

// copyFile replaces to with the contents of from, as a certificate renewal does.
func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(from))
	if err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G703: both paths come from t.TempDir, not from input
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNoTLSConfigured(t *testing.T) {
	cfg, err := TLSConfig(Options{})
	if err != nil || cfg != nil {
		t.Errorf("TLSConfig without a certificate = %v, %v; want nil, nil", cfg, err)
	}
}

func TestTLSConfigErrors(t *testing.T) {
	dir := t.TempDir()
	cert, key := ostest.WriteCert(t, dir, "server")
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]Options{
		"client CA without certificate":      {ClientCAFile: cert},
		"missing certificate":                {CertFile: filepath.Join(dir, "none.crt"), KeyFile: key},
		"key does not match":                 {CertFile: cert, KeyFile: empty},
		"missing client CA":                  {CertFile: cert, KeyFile: key, ClientCAFile: filepath.Join(dir, "none.pem")},
		"client CA without certificate data": {CertFile: cert, KeyFile: key, ClientCAFile: empty},
	} {
		if _, err := TLSConfig(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// serveTLS serves h with cfg on a real listener; httptest.StartTLS would install a certificate of its own and leave cfg
// unexercised.
func serveTLS(t *testing.T, cfg *tls.Config, h http.Handler) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, TLSConfig: cfg, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return "https://" + ln.Addr().String()
}

func trustPool(t *testing.T, certFile string) *x509.CertPool {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(certFile))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		t.Fatal("no certificate in " + certFile)
	}
	return pool
}

func fetch(t *testing.T, c *http.Client, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.Do(req)
}

var ok = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

func TestServesOverTLS(t *testing.T) {
	cert, key := ostest.WriteCert(t, t.TempDir(), "server")
	cfg, err := TLSConfig(Options{CertFile: cert, KeyFile: key})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS12 || cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("MinVersion %v, ClientAuth %v", cfg.MinVersion, cfg.ClientAuth)
	}
	url := serveTLS(t, cfg, ok)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: trustPool(t, cert), MinVersion: tls.VersionTLS12}}}
	resp, err := fetch(t, client, url)
	if err != nil {
		t.Fatalf("scrape over TLS with the configured certificate: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %s", resp.Status)
	}
}

func TestCertificateIsReloadedWhenItChanges(t *testing.T) {
	cert, key := ostest.WriteCert(t, t.TempDir(), "server")
	c, err := newCertificate(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	first := c.get(time.Now())

	renewed, renewedKey := ostest.WriteCert(t, t.TempDir(), "renewed")
	copyFile(t, renewed, cert)
	copyFile(t, renewedKey, key)

	if got := c.get(time.Now()); got != first {
		t.Error("the files were checked before the interval passed")
	}
	got := c.get(time.Now().Add(reloadInterval))
	if got == first {
		t.Fatal("the renewed certificate was not picked up")
	}
	leaf, err := x509.ParseCertificate(got.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "renewed" {
		t.Errorf("serving CN=%q, want the renewed certificate", leaf.Subject.CommonName)
	}
}

func TestBrokenReloadKeepsTheWorkingCertificate(t *testing.T) {
	cert, key := ostest.WriteCert(t, t.TempDir(), "server")
	c, err := newCertificate(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	good := c.get(time.Now())
	//nolint:gosec // G306: a certificate is not a secret
	if err := os.WriteFile(cert, []byte("half-written"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := c.get(time.Now().Add(reloadInterval)); got != good {
		t.Error("a broken certificate on disk replaced the working one")
	}
}

func TestClientCertificateIsRequired(t *testing.T) {
	dir := t.TempDir()
	serverCert, serverKey := ostest.WriteCert(t, dir, "server")
	clientCert, clientKey := ostest.WriteCert(t, dir, "client")
	otherCert, otherKey := ostest.WriteCert(t, dir, "other")
	cfg, err := TLSConfig(Options{CertFile: serverCert, KeyFile: serverKey, ClientCAFile: clientCert})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	url := serveTLS(t, cfg, ok)
	pool := trustPool(t, serverCert)
	client := func(certs ...tls.Certificate) *http.Client {
		return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: certs, MinVersion: tls.VersionTLS12}}}
	}
	pair := func(cert, key string) tls.Certificate {
		p, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	for name, c := range map[string]*http.Client{"no certificate": client(), "untrusted certificate": client(pair(otherCert, otherKey))} {
		if resp, err := fetch(t, c, url); err == nil {
			_ = resp.Body.Close()
			t.Errorf("%s: served", name)
		}
	}
	resp, err := fetch(t, client(pair(clientCert, clientKey)), url)
	if err != nil {
		t.Fatalf("trusted client certificate refused: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %s", resp.Status)
	}
}

// The passwords are long enough that finding one in the log means it leaked.
const (
	oldPassword = "old-scrape-secret-0b1c" //nolint:gosec // G101: a test fixture, not a credential
	newPassword = "new-scrape-secret-7e9d" //nolint:gosec // G101: a test fixture, not a credential
)

// rotating is basic auth over a password file, with a clock and a log the test controls.
type rotating struct {
	t    *testing.T
	path string
	now  time.Time
	h    http.Handler
	logs *bytes.Buffer
}

func newRotating(t *testing.T) *rotating {
	t.Helper()
	r := &rotating{t: t, path: filepath.Join(t.TempDir(), "pw"), now: time.Now(), logs: &bytes.Buffer{}}
	r.write(oldPassword + "\n")
	password, err := secret.NewFileCredential(r.path)
	if err != nil {
		t.Fatal(err)
	}
	// Debug, so a password logged at any level shows up.
	log := slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r.h = newBasicAuth("prometheus", password, log, func() time.Time { return r.now }).handler(ok, "/livez", "/readyz")
	return r
}

// write replaces the password file, as the kubelet does when a secret rotates.
func (r *rotating) write(body string) {
	r.t.Helper()
	if err := os.WriteFile(r.path, []byte(body), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rotating) call(path, user, pass string, withAuth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(r.t.Context(), http.MethodGet, path, nil)
	if withAuth {
		req.SetBasicAuth(user, pass)
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func (r *rotating) scrape(pass string) int { return r.call("/metrics", "prometheus", pass, true).Code }

func (r *rotating) noPasswordsLogged() {
	r.t.Helper()
	for _, pw := range []string{oldPassword, newPassword} {
		if strings.Contains(r.logs.String(), pw) {
			r.t.Errorf("a password was logged:\n%s", r.logs)
		}
	}
}

func TestBasicAuth(t *testing.T) {
	r := newRotating(t)
	for name, c := range map[string]struct {
		path, user, pass string
		withAuth         bool
		want             int
	}{
		"correct":          {"/metrics", "prometheus", oldPassword, true, http.StatusOK},
		"no credentials":   {"/metrics", "", "", false, http.StatusUnauthorized},
		"wrong password":   {"/metrics", "prometheus", "wrong", true, http.StatusUnauthorized},
		"wrong username":   {"/metrics", "someone", oldPassword, true, http.StatusUnauthorized},
		"line ending kept": {"/metrics", "prometheus", oldPassword + "\n", true, http.StatusUnauthorized},
		"livez":            {"/livez", "", "", false, http.StatusOK},
		"readyz":           {"/readyz", "", "", false, http.StatusOK},
		// The exemption matches the path exactly, so nothing reaches /metrics through it.
		"livez prefix":    {"/livez/../metrics", "", "", false, http.StatusUnauthorized},
		"livez subpath":   {"/livez/x", "", "", false, http.StatusUnauthorized},
		"doubled slashes": {"//livez", "", "", false, http.StatusUnauthorized},
	} {
		rec := r.call(c.path, c.user, c.pass, c.withAuth)
		if rec.Code != c.want {
			t.Errorf("%s: %d, want %d", name, rec.Code, c.want)
		}
		if rec.Code == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") != `Basic realm="osmex"` {
			t.Errorf("%s: WWW-Authenticate %q", name, rec.Header().Get("WWW-Authenticate"))
		}
	}
	r.noPasswordsLogged()
}

// Nothing here calls Reload: the middleware has to re-read the file on its own.
func TestBasicAuthFollowsARotatedPassword(t *testing.T) {
	r := newRotating(t)
	if got := r.scrape(oldPassword); got != http.StatusOK {
		t.Fatalf("the current password: %d, want 200", got)
	}
	r.write(newPassword)
	r.now = r.now.Add(reloadInterval)
	// The first scrape with the new password finds it, and succeeds.
	if got := r.scrape(newPassword); got != http.StatusOK {
		t.Errorf("the rotated password: %d, want 200", got)
	}
	if got := r.scrape(oldPassword); got != http.StatusUnauthorized {
		t.Errorf("the old password: %d, want 401", got)
	}
	if !strings.Contains(r.logs.String(), "scrape password rotated") {
		t.Errorf("the rotation was not logged:\n%s", r.logs)
	}
	r.noPasswordsLogged()
}

// A burst of bad credentials costs one read per interval, not one per request.
func TestBasicAuthThrottlesRereads(t *testing.T) {
	r := newRotating(t)

	// Inside the interval since startup, a rotated file is not read.
	r.write(newPassword)
	r.now = r.now.Add(reloadInterval - time.Second)
	for range 10 {
		if got := r.scrape(newPassword); got != http.StatusUnauthorized {
			t.Fatalf("inside the interval: %d, want 401", got)
		}
	}

	// After it, a burst of wrong passwords spends the one read and finds nothing changed...
	r.write(oldPassword)
	r.now = r.now.Add(time.Second)
	for range 10 {
		if got := r.scrape("wrong"); got != http.StatusUnauthorized {
			t.Fatalf("a wrong password: %d, want 401", got)
		}
	}
	// ...so a rotation right after it waits for the next interval.
	r.write(newPassword)
	if got := r.scrape(newPassword); got != http.StatusUnauthorized {
		t.Errorf("a second read inside one interval: %d, want 401", got)
	}
	r.now = r.now.Add(reloadInterval)
	if got := r.scrape(newPassword); got != http.StatusOK {
		t.Errorf("after the next interval: %d, want 200", got)
	}
	r.noPasswordsLogged()
}

func TestBasicAuthKeepsThePasswordWhenTheRereadFails(t *testing.T) {
	for name, broken := range map[string]func(r *rotating){
		"empty": func(r *rotating) { r.write("\n") },
		"missing": func(r *rotating) {
			if err := os.Remove(r.path); err != nil {
				r.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRotating(t)
			broken(r)
			r.now = r.now.Add(reloadInterval)
			if got := r.scrape("wrong"); got != http.StatusUnauthorized {
				t.Fatalf("a wrong password: %d, want 401", got)
			}
			if got := r.scrape(oldPassword); got != http.StatusOK {
				t.Errorf("the password in memory: %d, want 200", got)
			}
			if !strings.Contains(r.logs.String(), "scrape password reload failed") {
				t.Errorf("the failed re-read was not logged:\n%s", r.logs)
			}
			r.noPasswordsLogged()
		})
	}
}
