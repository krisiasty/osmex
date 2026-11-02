package cinder

import (
	"context"
	"errors"
	"time"

	"github.com/krisiasty/osmex/internal/expo"
	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/poller"
)

// quotaResources maps each exported resource to its /limits fields and unit scale.
var quotaResources = []struct {
	resource    string
	limit, used string
	scale       float64
}{
	{"volumes", "maxTotalVolumes", "totalVolumesUsed", 1},
	{"snapshots", "maxTotalSnapshots", "totalSnapshotsUsed", 1},
	{"backups", "maxTotalBackups", "totalBackupsUsed", 1},
	{"volume_bytes", "maxTotalVolumeGigabytes", "totalGigabytesUsed", gib}, // volumes and snapshots together
	{"backup_bytes", "maxTotalBackupGigabytes", "totalBackupGigabytesUsed", gib},
}

// NewLimits returns the cinder-limits poller, which reads the project's block storage quota from GET /limits.
func NewLimits(svc *osclient.ServiceClient, interval time.Duration, opts ...poller.Option) *poller.Poller {
	return poller.New("cinder-limits", func() time.Duration { return interval }, func(ctx context.Context) ([]byte, error) {
		var out struct {
			Limits struct {
				Absolute map[string]*float64 `json:"absolute"`
			} `json:"limits"`
		}
		if err := svc.Get(ctx, "/limits", nil, &out); err != nil {
			return nil, err
		}
		if out.Limits.Absolute == nil {
			err := &osclient.DecodeError{Err: errors.New("response has no absolute limits")}
			osclient.RecordErrorCode(ctx, osclient.ErrorCode(err))
			return nil, err
		}
		abs := make(map[string]float64, len(out.Limits.Absolute))
		for k, v := range out.Limits.Absolute {
			if v != nil {
				abs[k] = *v
			}
		}
		return renderLimits(abs), nil
	}, opts...)
}

// renderLimits renders the quota from the absolute limits. Unlimited (-1) limits are omitted, and usage fields that
// Cinder leaves out (its policy can hide them) are omitted rather than reported as 0.
func renderLimits(abs map[string]float64) []byte {
	var page expo.Page
	limit := page.Gauge("openstack_volume_quota_limit", "Block storage quota limit by resource; absent when unlimited.")
	used := page.Gauge("openstack_volume_quota_used", "Block storage quota usage by resource.")
	for _, r := range quotaResources {
		if v, ok := abs[r.limit]; ok && v >= 0 {
			limit.Add(v*r.scale, "resource", r.resource)
		}
		if v, ok := abs[r.used]; ok {
			used.Add(v*r.scale, "resource", r.resource)
		}
	}
	return page.Bytes()
}
