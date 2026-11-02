package neutron

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/krisiasty/osmex/internal/expo"
	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/poller"
)

// quotaResources maps each exported resource to its key in the quota details.
var quotaResources = []struct{ resource, key string }{
	{"floating_ips", "floatingip"},
	{"ports", "port"},
	{"security_groups", "security_group"},
	{"security_group_rules", "security_group_rule"},
	{"routers", "router"},
	{"networks", "network"},
	{"subnets", "subnet"},
}

type quotaDetail struct {
	Limit *float64 `json:"limit"`
	Used  *float64 `json:"used"`
}

// NewLimits returns the neutron-limits poller, which reports the project's network quota.
func NewLimits(svc *osclient.ServiceClient, interval time.Duration, opts ...poller.Option) *poller.Poller {
	return poller.New("neutron-limits", func() time.Duration { return interval }, func(ctx context.Context) ([]byte, error) {
		tok, err := svc.Token(ctx)
		if err != nil {
			return nil, err
		}
		if tok.Project.ID == "" {
			err := &osclient.DecodeError{Err: errors.New("token has no project id")}
			osclient.RecordErrorCode(ctx, osclient.ErrorCode(err))
			return nil, err
		}
		var out struct {
			Quota map[string]*quotaDetail `json:"quota"`
		}
		if err := svc.Get(ctx, "/quotas/"+url.PathEscape(tok.Project.ID)+"/details", nil, &out); err != nil {
			return nil, err
		}
		if out.Quota == nil {
			err := &osclient.DecodeError{Err: errors.New("response has no quota")}
			osclient.RecordErrorCode(ctx, osclient.ErrorCode(err))
			return nil, err
		}
		return renderLimits(out.Quota), nil
	}, opts...)
}

// renderLimits renders the quota details. Unlimited (-1) limits are omitted, and resources that Neutron leaves out are
// omitted rather than reported as 0.
func renderLimits(quota map[string]*quotaDetail) []byte {
	var page expo.Page
	limit := page.Gauge("openstack_network_quota_limit", "Network quota limit by resource.")
	used := page.Gauge("openstack_network_quota_used", "Network quota usage by resource.")
	for _, r := range quotaResources {
		q := quota[r.key]
		if q == nil {
			continue
		}
		if q.Limit != nil && *q.Limit >= 0 {
			limit.Add(*q.Limit, "resource", r.resource)
		}
		if q.Used != nil {
			used.Add(*q.Used, "resource", r.resource)
		}
	}
	return page.Bytes()
}
