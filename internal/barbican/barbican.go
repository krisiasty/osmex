// Package barbican polls the Key Manager API for secret expiration dates. Payloads are never read.
package barbican

import (
	"context"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/krisiasty/osmex/internal/expo"
	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/poller"
)

// Spec identifies the Key Manager API.
var Spec = osclient.ServiceSpec{Types: []string{"key-manager"}, Suffix: "/v1"}

type secret struct {
	Ref        string `json:"secret_ref"`
	Name       string `json:"name"`
	SecretType string `json:"secret_type"`
	Algorithm  string `json:"algorithm"`
	Expiration string `json:"expiration"`
}

// secretID returns the last path segment of a secret_ref URL.
func secretID(ref string) string {
	if ref == "" {
		return ""
	}
	return path.Base(strings.TrimRight(ref, "/"))
}

// New returns the barbican poller. Only secrets with an expiration produce series.
func New(svc *osclient.ServiceClient, interval time.Duration, opts ...poller.Option) *poller.Poller {
	query := url.Values{"limit": {"100"}}
	return poller.New("barbican", func() time.Duration { return interval }, func(ctx context.Context) ([]byte, error) {
		secrets, err := osclient.ListAll[secret](ctx, svc, "/secrets", query, "secrets")
		if err != nil {
			return nil, err
		}
		var page expo.Page
		info := page.Gauge("openstack_secret_info", "Secret information for secrets with an expiration; the value is always 1.")
		expiration := page.Gauge("openstack_secret_expiration_timestamp_seconds", "Time after which the secret can no longer be retrieved.")
		for _, s := range secrets {
			t, err := osclient.ParseTime(s.Expiration)
			if err != nil || t.IsZero() {
				continue
			}
			id := secretID(s.Ref)
			info.Add(1, "id", id, "name", s.Name, "secret_type", s.SecretType, "algorithm", s.Algorithm)
			expiration.Add(float64(t.UnixNano())/1e9, "id", id)
		}
		return page.Bytes(), nil
	}, opts...)
}
