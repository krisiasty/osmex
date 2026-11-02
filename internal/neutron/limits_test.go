package neutron

import (
	"net/http"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/ostest"
)

func TestLimitsPoll(t *testing.T) {
	cloud := ostest.New(t, "network")
	cloud.Handle("GET /network/v2.0/quotas/proj-id/details", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"quota": map[string]any{
			"floatingip":          map[string]any{"limit": 5, "used": 3, "reserved": 1},
			"port":                map[string]any{"limit": 100, "used": 12, "reserved": 0},
			"security_group":      map[string]any{"limit": 10, "used": 2, "reserved": 0},
			"security_group_rule": map[string]any{"limit": -1, "used": 40, "reserved": 0},
			"router":              map[string]any{"limit": 3, "used": 1, "reserved": 0},
			"network":             map[string]any{"limit": 10, "used": 2, "reserved": 0},
			// subnet left out: resources Neutron doesn't return are omitted, not reported as 0.
			"rbac_policy": map[string]any{"limit": 10, "used": 0, "reserved": 0},
		}})
	})
	p := NewLimits(cloud.Client(t).Service(Spec), 5*time.Minute)
	p.RunOnce(t.Context())
	if p.Name() != "neutron-limits" || !p.Status(time.Now()).Up {
		t.Errorf("name %q status %+v", p.Name(), p.Status(time.Now()))
	}
	ostest.Golden(t, "testdata/limits.golden", p.Body(time.Now()))
}

func TestLimitsPollMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"no quota":   `{}`,
		"null quota": `{"quota": null}`,
		"not a map":  `{"quota": {"port": 5}}`,
	} {
		t.Run(name, func(t *testing.T) {
			cloud := ostest.New(t, "network")
			cloud.Handle("GET /network/v2.0/quotas/proj-id/details", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			})
			p := NewLimits(cloud.Client(t).Service(Spec), 5*time.Minute)
			p.RunOnce(t.Context())
			if st := p.Status(time.Now()); st.Up || st.Errors["decode"] != 1 {
				t.Errorf("up %v, errors %v; want a decode error", st.Up, st.Errors)
			}
		})
	}
}
