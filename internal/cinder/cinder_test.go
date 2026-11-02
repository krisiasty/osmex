package cinder

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/ostest"
	"github.com/krisiasty/osmex/internal/poller"
)

func load[T any](t *testing.T, file, key string) []T {
	t.Helper()
	b, err := readTestdata(file)
	if err != nil {
		t.Fatal(err)
	}
	var page map[string][]T
	if err := json.Unmarshal(b, &page); err != nil {
		t.Fatal(err)
	}
	return page[key]
}

// readTestdata reads a fixture; os.Root keeps the name inside testdata/.
func readTestdata(name string) ([]byte, error) {
	root, err := os.OpenRoot("testdata")
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.ReadFile(name)
}

func TestVolumeStates(t *testing.T) {
	failed := []string{"error", "error_deleting", "error_backing-up", "error_restoring", "error_extending", "error_managing"}
	transitional := []string{"creating", "attaching", "detaching", "extending", "backing-up", "restoring-backup", "downloading",
		"uploading", "retyping", "reserved", "deleting", "reverting"}
	for _, s := range failed {
		if volumeOK(s) || volumeTransitional(s) {
			t.Errorf("%s: ok %v transitional %v", s, volumeOK(s), volumeTransitional(s))
		}
	}
	for _, s := range transitional {
		if !volumeOK(s) || !volumeTransitional(s) {
			t.Errorf("%s: ok %v transitional %v", s, volumeOK(s), volumeTransitional(s))
		}
	}
	for _, s := range []string{"available", "in-use", "awaiting-transfer", "maintenance"} {
		if !volumeOK(s) || volumeTransitional(s) {
			t.Errorf("%s: ok %v transitional %v", s, volumeOK(s), volumeTransitional(s))
		}
	}
}

func TestRenderGolden(t *testing.T) {
	vols := load[volume](t, "volumes.json", "volumes")
	got := slices.Concat(renderVolumes(vols),
		renderSnapshots(volumeIDs(vols), load[snapshot](t, "snapshots.json", "snapshots")),
		renderBackups(volumeIDs(vols), load[backup](t, "backups.json", "backups")))
	ostest.Golden(t, "testdata/cinder.golden", got)
}

type fakeCinder struct {
	cloud         *ostest.Cloud
	volumesDelay  atomic.Int64 // time.Duration
	volumesFail   atomic.Bool
	snapshotsFail atomic.Bool
	backupsFail   atomic.Bool
}

func newFakeCinder(t *testing.T) *fakeCinder {
	f := &fakeCinder{cloud: ostest.New(t, "block-storage")}
	serve := func(file string, fail *atomic.Bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("OpenStack-API-Version") != "volume 3.71" {
				t.Errorf("microversion header = %q", r.Header.Get("OpenStack-API-Version"))
			}
			if file == "volumes.json" {
				time.Sleep(time.Duration(f.volumesDelay.Load()))
			}
			if fail.Load() {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			b, err := readTestdata(file)
			if err != nil {
				t.Error(err)
			}
			_, _ = w.Write(b)
		}
	}
	f.cloud.Handle("GET /block-storage/v3/proj/volumes/detail", serve("volumes.json", &f.volumesFail))
	f.cloud.Handle("GET /block-storage/v3/proj/snapshots/detail", serve("snapshots.json", &f.snapshotsFail))
	f.cloud.Handle("GET /block-storage/v3/proj/backups/detail", serve("backups.json", &f.backupsFail))
	return f
}

// testPollers returns the cinder, cinder-snapshots and cinder-backups pollers on a shared clock, every minute.
func testPollers(t *testing.T, f *fakeCinder, clk func() time.Time) (volumes, snapshots, backups *poller.Poller) {
	ps := Pollers(f.cloud.Client(t).Service(Spec), Intervals{time.Minute, time.Minute, time.Minute}, poller.WithClock(clk))
	return ps[0], ps[1], ps[2]
}

func runAll(t *testing.T, ps ...*poller.Poller) {
	for _, p := range ps {
		p.RunOnce(t.Context())
	}
}

func TestPollersRenderAll(t *testing.T) {
	f := newFakeCinder(t)
	vp, sp, bp := testPollers(t, f, time.Now)
	runAll(t, vp, sp, bp)
	if vp.Name() != "cinder" || sp.Name() != "cinder-snapshots" || bp.Name() != "cinder-backups" {
		t.Errorf("names %q, %q, %q", vp.Name(), sp.Name(), bp.Name())
	}
	want, err := os.ReadFile("testdata/cinder.golden")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if got := string(vp.Body(now)) + string(sp.Body(now)) + string(bp.Body(now)); got != string(want) {
		t.Errorf("combined output differs from the golden file:\n%s", got)
	}
}

func TestBackupsFailureLeavesSnapshots(t *testing.T) {
	f := newFakeCinder(t)
	now := time.Unix(1_800_000_000, 0)
	vp, sp, bp := testPollers(t, f, func() time.Time { return now })
	f.backupsFail.Store(true) // e.g. no cinder-backup service in this cloud
	runAll(t, vp, sp, bp)
	if st := bp.Status(now); st.Up || st.HasData || st.Errors["403"] != 1 {
		t.Errorf("backups status %+v", st)
	}
	if st := sp.Status(now); !st.Up || !strings.Contains(string(sp.Body(now)), `openstack_volume_snapshots{volume_id="v1"} 2`) {
		t.Errorf("snapshot metrics lost with the backups list: status %+v body\n%s", st, sp.Body(now))
	}
	if !vp.Status(now).Up {
		t.Error("volume metrics lost with the backups list")
	}
}

func TestSnapshotsFailureLeavesBackups(t *testing.T) {
	f := newFakeCinder(t)
	now := time.Unix(1_800_000_000, 0)
	vp, sp, bp := testPollers(t, f, func() time.Time { return now })
	f.snapshotsFail.Store(true)
	runAll(t, vp, sp, bp)
	if st := sp.Status(now); st.Up || st.HasData {
		t.Errorf("snapshots status %+v", st)
	}
	if !bp.Status(now).Up || !strings.Contains(string(bp.Body(now)), `openstack_volume_backups{volume_id="v1"} 2`) {
		t.Errorf("backup metrics lost with the snapshots list:\n%s", bp.Body(now))
	}
}

func TestVolumesFailureLeavesCopies(t *testing.T) {
	f := newFakeCinder(t)
	now := time.Unix(1_800_000_000, 0)
	vp, sp, bp := testPollers(t, f, func() time.Time { return now })
	f.volumesFail.Store(true)
	runAll(t, vp, sp, bp)
	if vp.Status(now).Up {
		t.Error("volumes poller up despite a failing volume list")
	}
	for _, p := range []*poller.Poller{sp, bp} {
		body := string(p.Body(now))
		if !p.Status(now).Up || !strings.Contains(body, `volume_id="v1"`) {
			t.Errorf("%s did not publish its own data:\n%s", p.Name(), body)
		}
		// Without a volume list there is no way to know v3 exists and has no copies: no zero series.
		if strings.Contains(body, `volume_id="v3"`) {
			t.Errorf("%s emitted zero series without a volume list:\n%s", p.Name(), body)
		}
	}
}

func TestZeroCountsFromVolumeList(t *testing.T) {
	f := newFakeCinder(t)
	now := time.Unix(1_800_000_000, 0)
	vp, sp, bp := testPollers(t, f, func() time.Time { return now })
	runAll(t, vp, sp, bp)
	for p, want := range map[*poller.Poller][]string{
		sp: {`openstack_volume_snapshots{volume_id="v4"} 0`, `openstack_volume_snapshots{volume_id="vdel"} 1`},
		bp: {`openstack_volume_backups{volume_id="v3"} 0`, `openstack_volume_backups{volume_id="vdel2"} 1`},
	} {
		for _, w := range want {
			if !strings.Contains(string(p.Body(now)), w) {
				t.Errorf("%s: missing %q", p.Name(), w)
			}
		}
	}
}

func TestVolumeListOnlyGatesZeroRows(t *testing.T) {
	f := newFakeCinder(t)
	now := time.Unix(1_800_000_000, 0)
	vp, sp, bp := testPollers(t, f, func() time.Time { return now })
	vp.RunOnce(t.Context())
	f.volumesFail.Store(true)

	now = now.Add(2 * time.Minute)
	runAll(t, sp, bp)
	// The copies come from this poll's own calls; the 2-minute-old volume list must not age them.
	if st := bp.Status(now); st.DataAge != 0 {
		t.Errorf("backups data age = %v, want 0", st.DataAge)
	}

	now = now.Add(time.Minute) // the volume list is exactly 3× the volumes interval old: still used
	runAll(t, sp, bp)
	if !strings.Contains(string(bp.Body(now)), `openstack_volume_backups{volume_id="v3"} 0`) {
		t.Error("zero series missing while the volume list is within 3× its interval")
	}
	now = now.Add(time.Second)
	runAll(t, sp, bp)
	if strings.Contains(string(bp.Body(now)), `volume_id="v3"`) {
		t.Error("zero series emitted from a volume list older than 3× its interval")
	}
}

func TestVolumeListFailureKeepsCopies(t *testing.T) {
	f := newFakeCinder(t)
	now := time.Unix(1_800_000_000, 0)
	vp, sp, bp := testPollers(t, f, func() time.Time { return now })
	vp.RunOnce(t.Context())
	f.volumesFail.Store(true)

	now = now.Add(170 * time.Second) // ids still usable: this poll has zero rows
	runAll(t, sp, bp)
	now = now.Add(15 * time.Second) // the volume list is now past its 3× limit, the copies data is 15s old
	for _, p := range []*poller.Poller{sp, bp} {
		if body := p.Body(now); body == nil || !strings.Contains(string(body), `volume_id="v1"`) {
			t.Errorf("%s lost its own fresh data because the volume list aged out", p.Name())
		}
	}
}

func TestCopiesWaitForFirstVolumePoll(t *testing.T) {
	f := newFakeCinder(t)
	f.volumesDelay.Store(int64(300 * time.Millisecond))
	_, sp, bp := runPollers(t, f)
	waitFor(t, func() bool { return sp.Status(time.Now()).HasData && bp.Status(time.Now()).HasData })
	// The very first copies output already has zero summaries for volumes without copies.
	if !strings.Contains(string(bp.Body(time.Now())), `openstack_volume_backups{volume_id="v3"} 0`) {
		t.Errorf("first backups output lacks zero summaries:\n%s", bp.Body(time.Now()))
	}
}

func TestEmptyProject(t *testing.T) {
	if got := renderVolumes(nil); len(got) != 0 {
		t.Errorf("empty volume list rendered:\n%s", got)
	}
	if got := slices.Concat(renderSnapshots(nil, nil), renderBackups(nil, nil)); len(got) != 0 {
		t.Errorf("empty copy lists rendered:\n%s", got)
	}
}

func TestCopyInfo(t *testing.T) {
	got := renderFixtures(t)
	for _, want := range []string{
		"# TYPE openstack_volume_snapshot_info gauge",
		`openstack_volume_snapshot_info{id="sn1",name="nightly-1006",volume_id="v1"} 1`,
		`openstack_volume_snapshot_info{id="sn3",name="",volume_id="v2"} 1`,
		`openstack_volume_snapshot_info{id="sn4",name="before \"upgrade\"",volume_id="vdel"} 1`,
		"# TYPE openstack_volume_backup_info gauge",
		`openstack_volume_backup_info{id="b1",name="weekly-full",volume_id="v1",incremental="false"} 1`,
		`openstack_volume_backup_info{id="b3",name="nightly-1008",volume_id="v1",incremental="true"} 1`,
		`openstack_volume_backup_info{id="b4",name="archive",volume_id="vdel2",incremental="false"} 1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func unix(t *testing.T, ts string) string {
	t.Helper()
	tm, err := time.Parse("2006-01-02T15:04:05", ts)
	if err != nil {
		t.Fatal(err)
	}
	return strconv.FormatFloat(float64(tm.Unix()), 'g', -1, 64)
}

func renderFixtures(t *testing.T) string {
	ids := volumeIDs(load[volume](t, "volumes.json", "volumes"))
	return string(renderSnapshots(ids, load[snapshot](t, "snapshots.json", "snapshots"))) +
		string(renderBackups(ids, load[backup](t, "backups.json", "backups")))
}

func TestBackupTimestamps(t *testing.T) {
	got := renderFixtures(t)
	for _, want := range []string{
		// Newest available backup of v1 is the incremental b5.
		`openstack_volume_last_backup_timestamp_seconds{volume_id="v1"} ` + unix(t, "2026-10-08T02:00:00"),
		// Newest full backup of v1 is b1; its data dates from the snapshot it was taken from, before the backup started.
		`openstack_volume_last_full_backup_timestamp_seconds{volume_id="v1"} ` + unix(t, "2026-10-06T23:00:00"),
		// b4 has no data_timestamp: created_at is used.
		`openstack_volume_last_backup_timestamp_seconds{volume_id="vdel2"} ` + unix(t, "2026-09-01T00:00:00"),
		`openstack_volume_last_full_backup_timestamp_seconds{volume_id="vdel2"} ` + unix(t, "2026-09-01T00:00:00"),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(got, `openstack_volume_last_full_backup_timestamp_seconds{volume_id="v2"}`) {
		t.Error("last full backup timestamp emitted for a volume without full backups")
	}
}

func TestCopyStatus(t *testing.T) {
	got := renderFixtures(t)
	for _, want := range []string{
		"# TYPE openstack_volume_snapshot_status gauge",
		`openstack_volume_snapshot_status{id="sn1",status="available"} 1`,
		`openstack_volume_snapshot_status{id="sn3",status="error"} 1`,
		"# TYPE openstack_volume_backup_status gauge",
		`openstack_volume_backup_status{id="b2",status="error"} 1`,
		`openstack_volume_backup_status{id="b3",status="creating"} 1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestLastBackupUsesDataTimestamp(t *testing.T) {
	b := backup{ID: "b", VolumeID: "v", Status: "available", CreatedAt: "2026-10-07T02:00:00", DataTimestamp: "2026-10-06T23:00:00"}
	got := string(renderBackups([]string{"v"}, []backup{b}))
	want := `openstack_volume_last_backup_timestamp_seconds{volume_id="v"} ` + unix(t, "2026-10-06T23:00:00")
	if !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
}

func TestBackupErrorDeletingCountsAsFailed(t *testing.T) {
	b := backup{ID: "b", VolumeID: "v", Status: "error_deleting", Size: 1}
	got := string(renderBackups([]string{"v"}, []backup{b}))
	for _, want := range []string{
		`openstack_volume_backups_failed{volume_id="v"} 1`,
		`openstack_volume_backups{volume_id="v"} 0`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// runPollers starts all three pollers with Run, as the exporter does, and stops them when the test ends.
func runPollers(t *testing.T, f *fakeCinder) (volumes, snapshots, backups *poller.Poller) {
	ps := Pollers(f.cloud.Client(t).Service(Spec), Intervals{time.Hour, time.Hour, time.Hour})
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	for _, p := range ps {
		wg.Go(func() { p.Run(ctx) })
	}
	t.Cleanup(func() { cancel(); wg.Wait() })
	return ps[0], ps[1], ps[2]
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
