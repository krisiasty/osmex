package nova

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/ostest"
)

func TestRenderLimits(t *testing.T) {
	got := string(renderLimits(map[string]float64{
		"maxTotalCores": 20, "maxTotalRAMSize": 51200, "maxTotalInstances": 10,
		"totalCoresUsed": 4, "totalRAMUsed": 8192, "totalInstancesUsed": 2,
	}))
	want := "# HELP openstack_compute_quota_limit Compute quota limit by resource.\n" +
		"# TYPE openstack_compute_quota_limit gauge\n" +
		"openstack_compute_quota_limit{resource=\"cores\"} 20\n" +
		"openstack_compute_quota_limit{resource=\"instances\"} 10\n" +
		"openstack_compute_quota_limit{resource=\"ram_bytes\"} 5.36870912e+10\n" +
		"# HELP openstack_compute_quota_used Compute quota usage by resource.\n" +
		"# TYPE openstack_compute_quota_used gauge\n" +
		"openstack_compute_quota_used{resource=\"cores\"} 4\n" +
		"openstack_compute_quota_used{resource=\"instances\"} 2\n" +
		"openstack_compute_quota_used{resource=\"ram_bytes\"} 8.589934592e+09\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestUnlimitedOmitted(t *testing.T) {
	got := string(renderLimits(map[string]float64{"maxTotalCores": -1, "maxTotalRAMSize": -1, "maxTotalInstances": 10, "totalCoresUsed": 4}))
	if strings.Contains(got, `quota_limit{resource="cores"}`) || strings.Contains(got, `quota_limit{resource="ram_bytes"}`) {
		t.Errorf("unlimited quota rendered:\n%s", got)
	}
	if !strings.Contains(got, `openstack_compute_quota_used{resource="cores"} 4`) {
		t.Errorf("usage missing:\n%s", got)
	}
}

func TestRenderLimitsMissingFieldsOmitted(t *testing.T) {
	// A field Nova leaves out must be absent, not 0.
	got := string(renderLimits(map[string]float64{"maxTotalCores": 20, "totalInstancesUsed": 2}))
	for _, unwanted := range []string{
		`quota_limit{resource="instances"}`, `quota_limit{resource="ram_bytes"}`,
		`quota_used{resource="cores"}`, `quota_used{resource="ram_bytes"}`,
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("missing field rendered as %q:\n%s", unwanted, got)
		}
	}
	for _, want := range []string{`openstack_compute_quota_limit{resource="cores"} 20`, `openstack_compute_quota_used{resource="instances"} 2`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestLimitsPoll(t *testing.T) {
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/limits", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"limits": map[string]any{"rate": []any{}, "absolute": map[string]any{
			"maxTotalCores": 20, "maxTotalRAMSize": 51200, "maxTotalInstances": 10, "maxServerMeta": 128,
			"totalCoresUsed": 4, "totalRAMUsed": 8192, "totalInstancesUsed": 2, "maxTotalFloatingIps": nil,
		}}})
	})
	p := NewLimits(cloud.Client(t).Service(Spec), 5*time.Minute)
	p.RunOnce(t.Context())
	if p.Name() != "nova-limits" || !strings.Contains(string(p.Body(time.Now())), `openstack_compute_quota_limit{resource="cores"} 20`) {
		t.Errorf("name %q body\n%s", p.Name(), p.Body(time.Now()))
	}
}

func TestLimitsPollMissingAbsolute(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"missing": {"limits": map[string]any{}},
		"null":    {"limits": map[string]any{"absolute": nil}},
	} {
		t.Run(name, func(t *testing.T) {
			cloud := ostest.New(t, "compute")
			cloud.Handle("GET /compute/v2.1/limits", func(w http.ResponseWriter, r *http.Request) {
				ostest.WriteJSON(w, 200, body)
			})
			p := NewLimits(cloud.Client(t).Service(Spec), 5*time.Minute)
			p.RunOnce(t.Context())
			if st := p.Status(time.Now()); st.Up || st.Errors["decode"] != 1 {
				t.Errorf("a response without absolute limits: up %v, errors %v", st.Up, st.Errors)
			}
		})
	}
}
