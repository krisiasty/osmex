package osclient

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrRedirect means a request was redirected somewhere it must not follow.
var ErrRedirect = errors.New("refusing redirect")

// maxRedirects matches net/http's default limit.
const maxRedirects = 10

// checkRedirect allows a redirect only to the same scheme, host and port as the original request. Requests carry
// X-Auth-Token, and a 307/308 re-sends the Keystone request body with the credential secret; net/http strips
// neither on a cross-host redirect, so following one could hand them to another server or downgrade to http.
// Only scheme and host are named in the error, never the target's path or query.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("%w: stopped after %d redirects", ErrRedirect, maxRedirects)
	}
	orig := via[0].URL
	if !strings.EqualFold(req.URL.Scheme, orig.Scheme) || !strings.EqualFold(req.URL.Host, orig.Host) {
		return fmt.Errorf("%w from %s://%s to %s://%s", ErrRedirect, orig.Scheme, orig.Host, req.URL.Scheme, req.URL.Host)
	}
	return nil
}

// withRedirectPolicy returns a copy of c that follows only same-origin redirects, so the policy holds whichever
// client the caller supplies.
func withRedirectPolicy(c *http.Client) *http.Client {
	if c == nil {
		c = http.DefaultClient
	}
	cp := *c
	cp.CheckRedirect = checkRedirect
	return &cp
}
