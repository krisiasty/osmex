package cinder

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/ostest"
)

func TestRenderLimits(t *testing.T) {
	got := string(renderLimits(map[string]float64{
		"maxTotalVolumes": 10, "totalVolumesUsed": 4,
		"maxTotalSnapshots": 20, "totalSnapshotsUsed": 7,
		"maxTotalBackups": -1, "totalBackupsUsed": 3,
		"maxTotalVolumeGigabytes": 1000, "totalGigabytesUsed": 250,
		"maxTotalBackupGigabytes": 500, "totalBackupGigabytesUsed": 100,
	}))
	want := "# HELP openstack_volume_quota_limit Block storage quota limit by resource; absent when unlimited.\n" +
		"# TYPE openstack_volume_quota_limit gauge\n" +
		"openstack_volume_quota_limit{resource=\"backup_bytes\"} 5.36870912e+11\n" +
		"openstack_volume_quota_limit{resource=\"snapshots\"} 20\n" +
		"openstack_volume_quota_limit{resource=\"volume_bytes\"} 1.073741824e+12\n" +
		"openstack_volume_quota_limit{resource=\"volumes\"} 10\n" +
		"# HELP openstack_volume_quota_used Block storage quota usage by resource.\n" +
		"# TYPE openstack_volume_quota_used gauge\n" +
		"openstack_volume_quota_used{resource=\"backup_bytes\"} 1.073741824e+11\n" +
		"openstack_volume_quota_used{resource=\"backups\"} 3\n" +
		"openstack_volume_quota_used{resource=\"snapshots\"} 7\n" +
		"openstack_volume_quota_used{resource=\"volume_bytes\"} 2.68435456e+11\n" +
		"openstack_volume_quota_used{resource=\"volumes\"} 4\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestRenderLimitsWithoutUsage(t *testing.T) {
	// Cinder omits the usage fields when policy does not allow them; they must be absent, not 0.
	got := string(renderLimits(map[string]float64{"maxTotalVolumes": 10}))
	if strings.Contains(got, "openstack_volume_quota_used") || !strings.Contains(got, `openstack_volume_quota_limit{resource="volumes"} 10`) {
		t.Errorf("got:\n%s", got)
	}
}

func TestLimitsPoll(t *testing.T) {
	cloud := ostest.New(t, "block-storage")
	cloud.Handle("GET /block-storage/v3/proj/limits", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("OpenStack-API-Version") != "volume 3.71" {
			t.Errorf("microversion header = %q", r.Header.Get("OpenStack-API-Version"))
		}
		ostest.WriteJSON(w, 200, map[string]any{"limits": map[string]any{"rate": []any{}, "absolute": map[string]any{
			"maxTotalVolumes": 10, "totalVolumesUsed": 4,
		}}})
	})
	p := NewLimits(cloud.Client(t).Service(Spec), 5*time.Minute)
	p.RunOnce(t.Context())
	if p.Name() != "cinder-limits" || !strings.Contains(string(p.Body(time.Now())), `openstack_volume_quota_used{resource="volumes"} 4`) {
		t.Errorf("name %q body\n%s", p.Name(), p.Body(time.Now()))
	}
}

func TestLimitsPollMissingAbsolute(t *testing.T) {
	cloud := ostest.New(t, "block-storage")
	cloud.Handle("GET /block-storage/v3/proj/limits", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"limits": map[string]any{}})
	})
	p := NewLimits(cloud.Client(t).Service(Spec), 5*time.Minute)
	p.RunOnce(t.Context())
	if st := p.Status(time.Now()); st.Up {
		t.Error("a response without absolute limits counted as a successful poll")
	}
}
