// Package neutron polls the Networking API: server ports and floating IPs.
package neutron

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/krisiasty/osmex/internal/expo"
	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/poller"
)

// Spec identifies the Networking API.
var Spec = osclient.ServiceSpec{Types: []string{"network"}, Suffix: "/v2.0"}

type port struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	DeviceID    string `json:"device_id"`
	DeviceOwner string `json:"device_owner"`
	NetworkID   string `json:"network_id"`
}

type floatingIP struct {
	ID      string `json:"id"`
	Address string `json:"floating_ip_address"`
	PortID  string `json:"port_id"`
	Status  string `json:"status"`
}

// NewPorts returns the neutron-ports poller. Only ports owned by servers are reported.
func NewPorts(svc *osclient.ServiceClient, interval time.Duration, opts ...poller.Option) *poller.Poller {
	query := url.Values{"fields": {"id", "name", "status", "device_id", "device_owner", "network_id"}}
	return poller.New("neutron-ports", func() time.Duration { return interval }, func(ctx context.Context) ([]byte, error) {
		ports, err := osclient.ListAll[port](ctx, svc, "/ports", query, "ports")
		if err != nil {
			return nil, err
		}
		var page expo.Page
		info := page.Gauge("openstack_port_info", "Server port information; device_id is the server id; the value is always 1.")
		up := page.Gauge("openstack_port_up", "1 if the port status is ACTIVE, else 0.")
		status := page.Gauge("openstack_port_status", "Current port status; the value is always 1.")
		for _, p := range ports {
			if !strings.HasPrefix(p.DeviceOwner, "compute:") {
				continue
			}
			info.Add(1, "id", p.ID, "name", p.Name, "device_id", p.DeviceID, "network_id", p.NetworkID)
			up.Add(expo.Bool(p.Status == "ACTIVE"), "id", p.ID)
			status.Add(1, "id", p.ID, "status", p.Status)
		}
		return page.Bytes(), nil
	}, opts...)
}

// NewFloatingIPs returns the neutron-fip poller.
func NewFloatingIPs(svc *osclient.ServiceClient, interval time.Duration, opts ...poller.Option) *poller.Poller {
	return poller.New("neutron-fip", func() time.Duration { return interval }, func(ctx context.Context) ([]byte, error) {
		fips, err := osclient.ListAll[floatingIP](ctx, svc, "/floatingips", nil, "floatingips")
		if err != nil {
			return nil, err
		}
		var page expo.Page
		info := page.Gauge("openstack_floating_ip_info", "Floating IP information; the value is always 1.")
		status := page.Gauge("openstack_floating_ip_status", "Current floating IP status; the value is always 1.")
		for _, f := range fips {
			info.Add(1, "id", f.ID, "floating_ip_address", f.Address, "port_id", f.PortID)
			status.Add(1, "id", f.ID, "status", f.Status)
		}
		return page.Bytes(), nil
	}, opts...)
}
