package osclient_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/ostest"
)

// attacker is a TLS server that records whether anything reached it. httptest gives every TLS server the same
// certificate, so clients that trust the fake cloud would trust this server too if a redirect were followed.
func attacker(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		ostest.WriteJSON(w, 200, map[string]any{})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestGetRefusesCrossHostRedirect(t *testing.T) {
	cloud := ostest.New(t, "compute")
	evil, hits := attacker(t)
	cloud.Handle("GET /compute/v2.1/servers", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/steal?x=1", http.StatusFound)
	})
	rec := &recorder{}
	err := cloud.Client(t).Service(computeSpec).Get(osclient.WithRecorder(t.Context(), rec), "/servers", nil, nil)
	if osclient.ErrorCode(err) != "redirect" || rec.String() != "[redirect]" {
		t.Errorf("err %v (code %q), recorded %v", err, osclient.ErrorCode(err), rec)
	}
	if hits.Load() != 0 {
		t.Error("the token was sent to the redirect target")
	}
}

func TestGetFollowsSameHostRedirect(t *testing.T) {
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/servers", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/compute/v2.1/servers/", http.StatusMovedPermanently)
	})
	cloud.Handle("GET /compute/v2.1/servers/", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"ok": true})
	})
	var out struct{ OK bool }
	if err := cloud.Client(t).Service(computeSpec).Get(t.Context(), "/servers", nil, &out); err != nil || !out.OK {
		t.Fatalf("out %+v, err %v", out, err)
	}
}

func TestAuthRefusesCrossHostRedirect(t *testing.T) {
	evil, hits := attacker(t)
	keystone := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 307 would make a following client re-send the POST body, including the credential secret.
		http.Redirect(w, r, evil.URL+"/v3/auth/tokens", http.StatusTemporaryRedirect)
	}))
	defer keystone.Close()
	a := osclient.NewAuth(osclient.AuthConfig{
		URL: keystone.URL, CredentialID: "cred-id", CredentialSecret: "cred-secret", HTTP: keystone.Client(),
	})
	_, err := a.Token(t.Context())
	if osclient.ErrorCode(err) != "redirect" {
		t.Errorf("err %v, code %q", err, osclient.ErrorCode(err))
	}
	if hits.Load() != 0 {
		t.Error("the credential secret was sent to the redirect target")
	}
}

func TestCheckRedirect(t *testing.T) {
	req := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Request{URL: u}
	}
	via := []*http.Request{req("https://nova.example:8774/v2.1/servers")}
	for target, allowed := range map[string]bool{
		"https://nova.example:8774/v2.1/servers/": true,
		"https://NOVA.example:8774/v2.1/servers/": true,
		"https://evil.example:8774/v2.1/servers":  false,
		"https://nova.example:9999/v2.1/servers":  false,
		"http://nova.example:8774/v2.1/servers":   false,
		"https://nova.example/v2.1/servers":       false,
	} {
		err := osclient.CheckRedirect(req(target), via)
		if allowed != (err == nil) {
			t.Errorf("%s: err = %v, want allowed=%v", target, err, allowed)
		}
	}
	// Go's own limit on redirect chains still applies.
	long := make([]*http.Request, 10)
	for i := range long {
		long[i] = via[0]
	}
	if osclient.CheckRedirect(req("https://nova.example:8774/v2.1/x"), long) == nil {
		t.Error("an endless same-host redirect chain was allowed")
	}
}
