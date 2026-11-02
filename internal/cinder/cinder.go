// Package cinder polls the Block Storage API: volumes, snapshots and backups.
package cinder

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/krisiasty/osmex/internal/expo"
	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/poller"
)

// Spec identifies the Block Storage API.
var Spec = osclient.ServiceSpec{Types: []string{"block-storage", "volumev3"}, Microversion: "volume 3.71"}

const gib = 1 << 30

type volume struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Size        int    `json:"size"` // GiB
	VolumeType  string `json:"volume_type"`
	AZ          string `json:"availability_zone"`
	Bootable    string `json:"bootable"`
	Attachments []struct {
		ServerID string `json:"server_id"`
		Device   string `json:"device"`
	} `json:"attachments"`
}

type snapshot struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	VolumeID  string `json:"volume_id"`
	Status    string `json:"status"`
	Size      int    `json:"size"` // GiB
	CreatedAt string `json:"created_at"`
}

// backup has the snapshot fields, where Size is the source volume size rather than stored bytes, plus whether it is
// incremental and the point in time its data represents.
type backup struct {
	snapshot
	IsIncremental bool   `json:"is_incremental"`
	DataTimestamp string `json:"data_timestamp"` // a backup taken from a snapshot holds data from the snapshot's time
}

// dataTime returns the point in time the backup's data represents: data_timestamp, or created_at when the
// service did not report one.
func (b backup) dataTime() string {
	if b.DataTimestamp != "" {
		return b.DataTimestamp
	}
	return b.CreatedAt
}

var (
	failedStates = []string{"error", "error_deleting", "error_backing-up", "error_restoring", "error_extending", "error_managing"}
	transitional = []string{"creating", "attaching", "detaching", "extending", "backing-up", "restoring-backup", "downloading",
		"uploading", "retyping", "reserved", "deleting", "reverting"}
)

func volumeOK(status string) bool           { return !slices.Contains(failedStates, status) }
func volumeTransitional(status string) bool { return slices.Contains(transitional, status) }

// firstVolumesWait bounds how long the snapshots and backups pollers wait for the first volume poll at startup;
// after it they start without zero summaries and pick them up on their next poll.
const firstVolumesWait = 30 * time.Second

// Intervals are the poll intervals of the Cinder list pollers.
type Intervals struct {
	Volumes, Snapshots, Backups time.Duration
}

// Pollers returns the cinder poller, which lists volumes, and the cinder-snapshots and cinder-backups pollers. Each
// makes one list call, so each fails, ages out and reports api_up on its own. The snapshots and backups pollers
// borrow the volume ids to emit zero summaries for volumes without copies, and wait for the first volume poll so
// their first output already has them.
func Pollers(svc *osclient.ServiceClient, iv Intervals, opts ...poller.Option) []*poller.Poller {
	var ids poller.Shared[[]string]
	volumes := poller.New("cinder", func() time.Duration { return iv.Volumes }, func(ctx context.Context) ([]byte, error) {
		vols, err := osclient.ListAll[volume](ctx, svc, "/volumes/detail", nil, "volumes")
		if err != nil {
			return nil, err
		}
		ids.Publish(ctx, volumeIDs(vols))
		return renderVolumes(vols), nil
	}, opts...)

	afterVolumes := append(slices.Clone(opts), poller.WaitForFirstPoll(firstVolumesWait, volumes))
	snapshots := listCopies("cinder-snapshots", iv.Snapshots, svc, "/snapshots/detail", "snapshots", &ids, renderSnapshots, afterVolumes)
	backups := listCopies("cinder-backups", iv.Backups, svc, "/backups/detail", "backups", &ids, renderBackups, afterVolumes)
	return []*poller.Poller{volumes, snapshots, backups}
}

// listCopies returns a poller that lists one kind of volume copy and renders it with the volume ids the cinder
// poller published. The ids only decide which volumes get zero summaries; once they are too old Get returns none,
// and only volumes that have copies are summarised.
func listCopies[T any](name string, interval time.Duration, svc *osclient.ServiceClient, path, key string,
	ids *poller.Shared[[]string], render func(known []string, items []T) []byte, opts []poller.Option) *poller.Poller {
	return poller.New(name, func() time.Duration { return interval }, func(ctx context.Context) ([]byte, error) {
		items, err := osclient.ListAll[T](ctx, svc, path, nil, key)
		if err != nil {
			return nil, err
		}
		known, _ := ids.Get(ctx)
		return render(known, items), nil
	}, opts...)
}

func volumeIDs(vols []volume) []string {
	ids := make([]string, len(vols))
	for i, v := range vols {
		ids[i] = v.ID
	}
	return ids
}

type copies struct {
	count, failed, inProgress float64
	size                      float64
	last                      time.Time
}

// aggregate summarises snapshots or backups of one volume. failed lists the error states.
func aggregate(statuses []snapshot, failed ...string) map[string]*copies {
	out := map[string]*copies{}
	for _, s := range statuses {
		c := out[s.VolumeID]
		if c == nil {
			c = &copies{}
			out[s.VolumeID] = c
		}
		switch {
		case s.Status == "available":
			c.count++
			c.size += float64(s.Size) * gib
			if t, err := osclient.ParseTime(s.CreatedAt); err == nil && t.After(c.last) {
				c.last = t
			}
		case s.Status == "creating":
			c.inProgress = 1
		case slices.Contains(failed, s.Status):
			c.failed++
		}
	}
	return out
}

func renderVolumes(vols []volume) []byte {
	var page expo.Page
	info := page.Gauge("openstack_volume_info", "Volume information; the value is always 1.")
	status := page.Gauge("openstack_volume_status", "Current volume status; the value is always 1.")
	ok := page.Gauge("openstack_volume_ok", "0 if the volume is in a failed state, else 1.")
	trans := page.Gauge("openstack_volume_transitional", "1 while the volume is in a transitional state, else 0.")
	size := page.Gauge("openstack_volume_size_bytes", "Volume size in bytes.")
	attach := page.Gauge("openstack_volume_attachment_info", "Volume attachment to a server; the value is always 1.")
	for _, v := range vols {
		info.Add(1, "id", v.ID, "name", v.Name, "volume_type", v.VolumeType, "availability_zone", v.AZ, "bootable", v.Bootable)
		status.Add(1, "id", v.ID, "status", v.Status)
		ok.Add(expo.Bool(volumeOK(v.Status)), "id", v.ID)
		trans.Add(expo.Bool(volumeTransitional(v.Status)), "id", v.ID)
		size.Add(float64(v.Size)*gib, "id", v.ID)
		for _, a := range v.Attachments {
			attach.Add(1, "id", v.ID, "server_id", a.ServerID, "device", a.Device)
		}
	}
	return page.Bytes()
}

// summaryIDs returns the volumes that get per-volume summaries: every known volume, with zeros for those without
// copies, plus every volume a copy refers to, including deleted ones.
func summaryIDs[T any](known []string, items []T, volumeID func(T) string) map[string]bool {
	ids := make(map[string]bool, len(known))
	for _, id := range known {
		ids[id] = true
	}
	for _, it := range items {
		ids[volumeID(it)] = true
	}
	return ids
}

// renderSnapshots renders the per-snapshot metrics and the per-volume snapshot summaries.
func renderSnapshots(known []string, snaps []snapshot) []byte {
	ids := summaryIDs(known, snaps, func(s snapshot) string { return s.VolumeID })
	var page expo.Page
	info := page.Gauge("openstack_volume_snapshot_info", "Volume snapshot information; the value is always 1.")
	status := page.Gauge("openstack_volume_snapshot_status", "Current volume snapshot status; the value is always 1.")
	for _, s := range snaps {
		info.Add(1, "id", s.ID, "name", s.Name, "volume_id", s.VolumeID)
		status.Add(1, "id", s.ID, "status", s.Status)
	}
	agg := aggregate(snaps, "error", "error_deleting")
	count := page.Gauge("openstack_volume_snapshots", "Available snapshots of the volume.")
	failed := page.Gauge("openstack_volume_snapshots_failed", "Snapshots of the volume in an error state.")
	total := page.Gauge("openstack_volume_snapshots_size_bytes", "Total size of available snapshots of the volume in bytes.")
	last := page.Gauge("openstack_volume_last_snapshot_timestamp_seconds", "Creation time of the newest available snapshot of the volume.")
	for id := range ids {
		c := agg[id]
		if c == nil {
			c = &copies{}
		}
		count.Add(c.count, "volume_id", id)
		failed.Add(c.failed, "volume_id", id)
		total.Add(c.size, "volume_id", id)
		if !c.last.IsZero() {
			last.Add(float64(c.last.UnixNano())/1e9, "volume_id", id)
		}
	}
	return page.Bytes()
}

// renderBackups renders the per-backup metrics and the per-volume backup summaries.
func renderBackups(known []string, backups []backup) []byte {
	ids := summaryIDs(known, backups, func(b backup) string { return b.VolumeID })
	var page expo.Page
	// Backups are summarised like snapshots, but dated by the time their data represents, which is what a
	// recovery-point check needs.
	summarised := make([]snapshot, len(backups))
	for i, b := range backups {
		summarised[i] = b.snapshot
		summarised[i].CreatedAt = b.dataTime()
	}
	info := page.Gauge("openstack_volume_backup_info", "Volume backup information; the value is always 1.")
	status := page.Gauge("openstack_volume_backup_status", "Current volume backup status; the value is always 1.")
	lastFull := map[string]time.Time{}
	for _, b := range backups {
		info.Add(1, "id", b.ID, "name", b.Name, "volume_id", b.VolumeID, "incremental", strconv.FormatBool(b.IsIncremental))
		status.Add(1, "id", b.ID, "status", b.Status)
		if b.Status == "available" && !b.IsIncremental {
			if t, err := osclient.ParseTime(b.dataTime()); err == nil && t.After(lastFull[b.VolumeID]) {
				lastFull[b.VolumeID] = t
			}
		}
	}
	agg := aggregate(summarised, "error", "error_deleting")
	count := page.Gauge("openstack_volume_backups", "Available backups of the volume.")
	failed := page.Gauge("openstack_volume_backups_failed", "Backups of the volume in an error state.")
	inProgress := page.Gauge("openstack_volume_backup_in_progress", "1 while a backup of the volume is being created, else 0.")
	total := page.Gauge("openstack_volume_backups_size_bytes", "Total size of available backups of the volume in bytes (source volume size).")
	last := page.Gauge("openstack_volume_last_backup_timestamp_seconds",
		"Point in time captured by the newest available backup of the volume (data_timestamp: the snapshot's time for a backup taken from a snapshot, otherwise its start).")
	full := page.Gauge("openstack_volume_last_full_backup_timestamp_seconds",
		"Point in time captured by the newest available full (not incremental) backup of the volume; restoring an incremental backup needs every backup back to this one.")
	for id := range ids {
		c := agg[id]
		if c == nil {
			c = &copies{}
		}
		count.Add(c.count, "volume_id", id)
		failed.Add(c.failed, "volume_id", id)
		inProgress.Add(c.inProgress, "volume_id", id)
		total.Add(c.size, "volume_id", id)
		if !c.last.IsZero() {
			last.Add(float64(c.last.UnixNano())/1e9, "volume_id", id)
		}
		if t, ok := lastFull[id]; ok {
			full.Add(float64(t.UnixNano())/1e9, "volume_id", id)
		}
	}
	return page.Bytes()
}
