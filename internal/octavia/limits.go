package octavia

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/krisiasty/osmex/internal/expo"
	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/poller"
)

// quotaResources maps each exported resource to its quota fields, current name first, then the deprecated one.
var quotaResources = []struct {
	resource string
	keys     []string
}{
	{"load_balancers", []string{"load_balancer", "loadbalancer"}},
	{"listeners", []string{"listener"}},
	{"pools", []string{"pool"}},
	{"members", []string{"member"}},
	{"health_monitors", []string{"health_monitor", "healthmonitor"}},
}

// NewLimits returns the octavia-limits poller, which reports the project's load balancer quota. Octavia fills in its
// configured defaults for quotas the project doesn't set, and returns limits only; usage comes from the octavia
// poller's series.
func NewLimits(svc *osclient.ServiceClient, interval time.Duration, opts ...poller.Option) *poller.Poller {
	return poller.New("octavia-limits", func() time.Duration { return interval }, func(ctx context.Context) ([]byte, error) {
		tok, err := svc.Token(ctx)
		if err != nil {
			return nil, err
		}
		if tok.Project.ID == "" {
			return nil, decodeError(ctx, "token has no project id")
		}
		var out struct {
			Quota map[string]*float64 `json:"quota"`
		}
		if err := svc.Get(ctx, "/quotas/"+url.PathEscape(tok.Project.ID), nil, &out); err != nil {
			return nil, err
		}
		if out.Quota == nil {
			return nil, decodeError(ctx, "response has no quota")
		}
		var page expo.Page
		limit := page.Gauge("openstack_lb_quota_limit", "Load balancer quota limit by resource.")
		for _, r := range quotaResources {
			for _, key := range r.keys {
				if v := out.Quota[key]; v != nil {
					if *v >= 0 { // -1 is unlimited
						limit.Add(*v, "resource", r.resource)
					}
					break
				}
			}
		}
		return page.Bytes(), nil
	}, opts...)
}

func decodeError(ctx context.Context, msg string) error {
	err := &osclient.DecodeError{Err: errors.New(msg)}
	osclient.RecordErrorCode(ctx, osclient.ErrorCode(err))
	return err
}
