package osclient_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/ostest"
)

func TestAuthToken(t *testing.T) {
	cloud := ostest.New(t, "compute")
	a := cloud.Auth(t)
	tok, err := a.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if tok.ID != "tok-1" {
		t.Errorf("token id = %q", tok.ID)
	}
	if _, err := tok.Catalog.EndpointURL([]string{"compute"}, "RegionOne", "public"); err != nil {
		t.Errorf("catalog = %+v", tok.Catalog)
	}
	if time.Until(tok.ExpiresAt) < 59*time.Minute {
		t.Errorf("expires = %v", tok.ExpiresAt)
	}
	if !a.Up() {
		t.Error("Up false")
	}
	select {
	case <-a.Ready():
	default:
		t.Error("Ready not closed")
	}
}

func TestAuthCachesToken(t *testing.T) {
	cloud := ostest.New(t)
	a := cloud.Auth(t)
	for range 2 {
		if _, err := a.Token(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if n := cloud.AuthCalls.Load(); n != 1 {
		t.Errorf("auth calls = %d", n)
	}
}

func TestAuthSingleFlight(t *testing.T) {
	cloud := ostest.New(t)
	a := cloud.Auth(t)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := a.Token(t.Context()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := cloud.AuthCalls.Load(); n != 1 {
		t.Errorf("auth calls = %d", n)
	}
}

func TestAuthBadCredentials(t *testing.T) {
	cloud := ostest.New(t)
	cloud.FailAuth.Store(true)
	a := cloud.Auth(t)
	_, err := a.Token(t.Context())
	if code := osclient.ErrorCode(err); code != "auth" {
		t.Errorf("code = %q (err %v)", code, err)
	}
	if a.Up() {
		t.Error("Up true")
	}
}

func TestAuthAttempted(t *testing.T) {
	for _, fail := range []bool{false, true} {
		cloud := ostest.New(t)
		cloud.FailAuth.Store(fail)
		a := cloud.Auth(t)
		select {
		case <-a.Attempted():
			t.Fatal("Attempted closed before Run")
		default:
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { a.Run(ctx); close(done) }()
		select {
		case <-a.Attempted():
		case <-time.After(5 * time.Second):
			t.Fatalf("fail=%v: Attempted not closed after the first authentication", fail)
		}
		select {
		case <-a.Ready():
			if fail {
				t.Error("Ready closed after a failed authentication")
			}
		default:
			if !fail {
				t.Error("Ready not closed when Attempted closed after a success")
			}
		}
		cancel()
		<-done
	}
}

func TestInvalidate(t *testing.T) {
	cloud := ostest.New(t)
	a := cloud.Auth(t)
	tok, _ := a.Token(t.Context())
	a.Invalidate("stale")
	if tok2, _ := a.Token(t.Context()); tok2.ID != tok.ID {
		t.Errorf("Invalidate(stale) replaced token")
	}
	a.Invalidate(tok.ID)
	tok3, err := a.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if tok3.ID == tok.ID || cloud.AuthCalls.Load() != 2 {
		t.Errorf("token not refreshed: %q, calls %d", tok3.ID, cloud.AuthCalls.Load())
	}
}

func TestAuthRunRefreshesShortLivedToken(t *testing.T) {
	cloud := ostest.New(t)
	cloud.TokenTTL = 3 * time.Second // shorter than the 5 minute margin: refresh at half the lifetime
	a := cloud.Auth(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2500*time.Millisecond)
	defer cancel()
	a.Run(ctx)
	// expected: initial auth, then a refresh after ~1.5s; a tight loop would make many more calls
	if n := cloud.AuthCalls.Load(); n < 2 || n > 3 {
		t.Errorf("auth calls = %d", n)
	}
}

func TestRefreshDelay(t *testing.T) {
	now := time.Now()
	cases := []struct {
		expires time.Time
		want    time.Duration
	}{
		{now.Add(time.Hour), 55 * time.Minute},
		{now.Add(4 * time.Minute), 2 * time.Minute},
		{now.Add(-time.Minute), 0},
	}
	for _, c := range cases {
		if got := osclient.RefreshDelay(now, c.expires); got != c.want {
			t.Errorf("RefreshDelay(%v) = %v, want %v", c.expires.Sub(now), got, c.want)
		}
	}
}

func TestTokensURL(t *testing.T) {
	for _, in := range []string{"http://k:5000", "http://k:5000/", "http://k:5000/v3", "http://k:5000/v3/"} {
		if got := osclient.TokensURL(in); got != "http://k:5000/v3/auth/tokens" {
			t.Errorf("TokensURL(%q) = %q", in, got)
		}
	}
}

func hangingKeystone(t *testing.T, hang func() bool) *httptest.Server {
	cloud := ostest.New(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hang() {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		cloud.Server.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	return srv
}

func TestAuthTimeoutOnHungKeystone(t *testing.T) {
	var hang atomic.Bool
	hang.Store(true)
	srv := hangingKeystone(t, hang.Load)
	a := osclient.NewAuth(osclient.AuthConfig{
		URL: srv.URL + "/identity", CredentialID: "cred-id", CredentialSecret: "cred-secret",
		HTTP: srv.Client(), Timeout: 200 * time.Millisecond, AllowInsecure: true,
	})
	start := time.Now()
	if _, err := a.Token(t.Context()); err == nil {
		t.Fatal("hung Keystone produced a token")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("authentication took %v despite a 200ms timeout", time.Since(start))
	}
	hang.Store(false)
	if _, err := a.Token(t.Context()); err != nil {
		t.Fatalf("authentication did not recover: %v", err)
	}
}

func TestAuthLeaderCancelDoesNotFailWaiters(t *testing.T) {
	var slow atomic.Bool
	slow.Store(true)
	cloud := ostest.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slow.Load() {
			time.Sleep(300 * time.Millisecond)
		}
		cloud.Server.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	a := osclient.NewAuth(osclient.AuthConfig{
		URL: srv.URL + "/identity", CredentialID: "cred-id", CredentialSecret: "cred-secret", HTTP: srv.Client(),
		AllowInsecure: true,
	})
	leaderCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	leaderErr := make(chan error, 1)
	go func() { _, err := a.Token(leaderCtx); leaderErr <- err }()
	time.Sleep(10 * time.Millisecond)
	if _, err := a.Token(t.Context()); err != nil {
		t.Errorf("waiter failed because the leader's context expired: %v", err)
	}
	if err := <-leaderErr; err == nil {
		t.Error("leader with an expired context got a token")
	}
	if n := cloud.AuthCalls.Load(); n != 1 {
		t.Errorf("auth calls = %d", n)
	}
}

func TestTokenProject(t *testing.T) {
	cloud := ostest.New(t)
	a := cloud.Auth(t)
	if p := a.Project(); p != (osclient.Project{}) {
		t.Errorf("project before authentication = %+v", p)
	}
	tok, err := a.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := osclient.Project{ID: "proj-id", Name: "demo-project", DomainID: "dom-id", DomainName: "Customers"}
	if tok.Project != want || a.Project() != want {
		t.Errorf("token project %+v, Auth.Project %+v, want %+v", tok.Project, a.Project(), want)
	}
	if tok.UserID != "user-id" {
		t.Errorf("token user id %q", tok.UserID)
	}
}

func TestAuthRefusesPlainHTTP(t *testing.T) {
	cloud := ostest.NewInsecure(t)
	a := osclient.NewAuth(osclient.AuthConfig{
		URL: cloud.AuthURL(), CredentialID: "cred-id", CredentialSecret: "cred-secret", HTTP: cloud.Server.Client(),
	})
	_, err := a.Token(t.Context())
	if osclient.ErrorCode(err) != "insecure" {
		t.Errorf("err = %v, code %q", err, osclient.ErrorCode(err))
	}
	if n := cloud.AuthCalls.Load(); n != 0 {
		t.Errorf("credentials were sent over plain HTTP: %d auth calls", n)
	}
}

func TestAuthAllowsPlainHTTPWhenInsecure(t *testing.T) {
	cloud := ostest.NewInsecure(t)
	a := osclient.NewAuth(osclient.AuthConfig{
		URL: cloud.AuthURL(), CredentialID: "cred-id", CredentialSecret: "cred-secret", HTTP: cloud.Server.Client(),
		AllowInsecure: true,
	})
	if _, err := a.Token(t.Context()); err != nil {
		t.Fatal(err)
	}
}
