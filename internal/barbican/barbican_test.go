package barbican

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/ostest"
)

func TestSecretID(t *testing.T) {
	for in, want := range map[string]string{
		"https://kms:9311/v1/secrets/abc":  "abc",
		"https://kms:9311/v1/secrets/abc/": "abc",
		"":                                 "",
	} {
		if got := secretID(in); got != want {
			t.Errorf("secretID(%q) = %q, want %q", in, got, want)
		}
	}
}

func fakeBarbican(t *testing.T) *ostest.Cloud {
	cloud := ostest.New(t, "key-manager")
	cloud.Handle("GET /key-manager/v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "100" {
			t.Errorf("limit = %q", r.URL.Query().Get("limit"))
		}
		page := "testdata/secrets-page1.json"
		if r.URL.Query().Get("offset") == "2" {
			page = "testdata/secrets-page2.json"
		}
		b, err := os.ReadFile(page)
		if err != nil {
			t.Error(err)
		}
		_, _ = w.Write(b)
	})
	return cloud
}

func TestRenderGolden(t *testing.T) {
	p := New(fakeBarbican(t).Client(t).Service(Spec), time.Hour)
	p.RunOnce(t.Context())
	if p.Name() != "barbican" || !p.Status(time.Now()).Up {
		t.Errorf("name %q status %+v", p.Name(), p.Status(time.Now()))
	}
	ostest.Golden(t, "testdata/barbican.golden", p.Body(time.Now()))
}
