package octavia

import (
	"net/http"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/ostest"
)

func TestLimitsPoll(t *testing.T) {
	cloud := ostest.New(t, "load-balancer")
	cloud.Handle("GET /load-balancer/v2/lbaas/quotas/proj-id", func(w http.ResponseWriter, r *http.Request) {
		// listener is unlimited, health_monitor comes only under its deprecated name, pool is left out, and the L7
		// quotas are not exported.
		_, _ = w.Write([]byte(`{"quota": {"load_balancer": 10, "loadbalancer": 10, "listener": -1, "member": 200,
			"healthmonitor": 20, "l7policy": 50, "l7rule": 100}}`))
	})
	p := NewLimits(cloud.Client(t).Service(Spec), 5*time.Minute)
	p.RunOnce(t.Context())
	if p.Name() != "octavia-limits" || !p.Status(time.Now()).Up {
		t.Errorf("name %q status %+v", p.Name(), p.Status(time.Now()))
	}
	ostest.Golden(t, "testdata/limits.golden", p.Body(time.Now()))
}

func TestLimitsPollMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"no quota":     `{}`,
		"null quota":   `{"quota": null}`,
		"not a number": `{"quota": {"load_balancer": "ten"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			cloud := ostest.New(t, "load-balancer")
			cloud.Handle("GET /load-balancer/v2/lbaas/quotas/proj-id", func(w http.ResponseWriter, r *http.Request) {
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
