package keystone

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/ostest"
)

const expiryRoute = "GET /identity/v3/users/user-id/application_credentials/cred-id"

func poll(t *testing.T, body string) (up bool, metrics string, errors map[string]uint64) {
	t.Helper()
	cloud := ostest.New(t, "identity")
	cloud.Handle(expiryRoute, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	p := NewCredential(cloud.Client(t).Service(Spec), "cred-id", time.Hour)
	p.RunOnce(t.Context())
	if p.Name() != "keystone-credential" {
		t.Errorf("name %q", p.Name())
	}
	st := p.Status(time.Now())
	return st.Up, string(p.Body(time.Now())), st.Errors
}

func TestCredentialExpiry(t *testing.T) {
	up, metrics, _ := poll(t, `{"application_credential": {"id": "cred-id", "name": "osmex", "expires_at": "2026-12-31T00:00:00.000000", "unrestricted": false}}`)
	want := "# HELP openstack_exporter_credential_expiration_timestamp_seconds Time after which the application credential can no longer authenticate.\n" +
		"# TYPE openstack_exporter_credential_expiration_timestamp_seconds gauge\n" +
		"openstack_exporter_credential_expiration_timestamp_seconds 1.7986752e+09\n"
	if !up || metrics != want {
		t.Errorf("up %v, metrics\n%s\nwant\n%s", up, metrics, want)
	}
}

func TestCredentialWithoutExpiry(t *testing.T) {
	for _, body := range []string{
		`{"application_credential": {"id": "cred-id", "expires_at": null}}`,
		`{"application_credential": {"id": "cred-id"}}`,
	} {
		up, metrics, _ := poll(t, body)
		if !up || metrics != "" {
			t.Errorf("%s: up %v, metrics %q; want a successful poll without series", body, up, metrics)
		}
	}
}

func TestCredentialMalformed(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"application_credential": null}`,
		`{"application_credential": {"expires_at": ""}}`,
		`{"application_credential": {"expires_at": "next year"}}`,
		`{"application_credential": {"expires_at": 1798675200}}`,
	} {
		up, metrics, errs := poll(t, body)
		if up || metrics != "" || errs["decode"] != 1 {
			t.Errorf("%s: up %v, metrics %q, errors %v; want a decode error", body, up, metrics, errs)
		}
	}
}

func TestCredentialForbidden(t *testing.T) {
	cloud := ostest.New(t, "identity")
	cloud.Handle(expiryRoute, func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": 403, "message": "not allowed"}})
	})
	p := NewCredential(cloud.Client(t).Service(Spec), "cred-id", time.Hour)
	p.RunOnce(t.Context())
	if st := p.Status(time.Now()); st.Up || st.Errors["403"] != 1 || strings.Contains(string(p.Body(time.Now())), "credential_expiration") {
		t.Errorf("status %+v, body %q", st, p.Body(time.Now()))
	}
}
