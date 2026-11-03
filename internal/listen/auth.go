package listen

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/krisiasty/osmex/internal/secret"
)

// BasicAuth requires a username and password on every request except those for the exempt paths, which are matched
// exactly. The exemption is for the probes: a Kubernetes probe sends no credentials, and requiring them turns a health
// check into a restart loop.
func BasicAuth(next http.Handler, username string, password *secret.Credential, log *slog.Logger, exempt ...string) http.Handler {
	return newBasicAuth(username, password, log, time.Now).handler(next, exempt...)
}

// basicAuth checks credentials against a password that can rotate underneath it. A request re-reads the password file,
// but at most once per reloadInterval across all requests: anyone who can reach the port decides when a request
// arrives, and an unconditional re-read would turn every request into a file read.
type basicAuth struct {
	username string
	password *secret.Credential
	log      *slog.Logger
	now      func() time.Time

	mu       sync.Mutex
	reloaded time.Time
}

func newBasicAuth(username string, password *secret.Credential, log *slog.Logger, now func() time.Time) *basicAuth {
	// The password was read a moment ago, at startup.
	return &basicAuth{username: username, password: password, log: log, now: now, reloaded: now()}
}

func (a *basicAuth) handler(next http.Handler, exempt ...string) http.Handler {
	skip := make(map[string]bool, len(exempt))
	for _, p := range exempt {
		skip[p] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || !a.allow(user, pass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="osmex"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allow reports whether the credentials match, re-reading the password file first when a re-read is due. The re-read
// comes before the comparison whether or not the credentials match the password in memory: re-reading only after a
// rejection would let the old password in for as long as nobody sent different credentials. A rotated password is thus
// accepted, and the old one refused, from the first request after the interval.
func (a *basicAuth) allow(user, pass string) bool {
	if a.due() {
		a.reload()
	}
	return matches(user, pass, a.username, a.password.Value())
}

// reload re-reads the password file. A failed read keeps the password in memory: a half-written file must not lock out
// a scraper that was working a moment ago.
func (a *basicAuth) reload() {
	changed, err := a.password.Reload()
	switch {
	case err != nil:
		a.log.Warn("scrape password reload failed", "user", a.username, "err", err)
	case changed:
		a.log.Warn("scrape password rotated", "user", a.username)
	}
}

// due reports whether a re-read is allowed now, and claims it if so; claiming under the lock makes concurrent
// rejections share one read.
func (a *basicAuth) due() bool {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if now.Sub(a.reloaded) < reloadInterval {
		return false
	}
	a.reloaded = now
	return true
}

// matches compares credentials in constant time. Both halves are always compared: returning early on a wrong username
// would let the response time reveal whether the username was right.
func matches(gotUser, gotPass, wantUser, wantPass string) bool {
	u := subtle.ConstantTimeCompare([]byte(gotUser), []byte(wantUser))
	p := subtle.ConstantTimeCompare([]byte(gotPass), []byte(wantPass))
	return u&p == 1
}
