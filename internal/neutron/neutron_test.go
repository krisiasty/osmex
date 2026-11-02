package neutron

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/ostest"
)

func TestPortsQueryFields(t *testing.T) {
	cloud := ostest.New(t, "network")
	cloud.Handle("GET /network/v2.0/ports", func(w http.ResponseWriter, r *http.Request) {
		fields := r.URL.Query()["fields"]
		slices.Sort(fields)
		if !slices.Equal(fields, []string{"device_id", "device_owner", "id", "name", "network_id", "status"}) {
			t.Errorf("fields = %v", fields)
		}
		ostest.WriteJSON(w, 200, map[string]any{"ports": []any{}})
	})
	p := NewPorts(cloud.Client(t).Service(Spec), time.Minute)
	p.RunOnce(t.Context())
	if p.Name() != "neutron-ports" || !p.Status(time.Now()).Up {
		t.Errorf("name %q status %+v", p.Name(), p.Status(time.Now()))
	}
}

func TestPortsFilterAndRender(t *testing.T) {
	cloud := ostest.New(t, "network")
	cloud.Handle("GET /network/v2.0/ports", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"ports": []map[string]any{
			{"id": "p1", "name": "", "status": "ACTIVE", "device_id": "s1", "device_owner": "compute:nova", "network_id": "n1"},
			{"id": "p2", "name": "eth1", "status": "DOWN", "device_id": "s2", "device_owner": "compute:az2", "network_id": "n2"},
			{"id": "p3", "name": "", "status": "ACTIVE", "device_id": "r1", "device_owner": "network:router_interface", "network_id": "n1"},
		}})
	})
	p := NewPorts(cloud.Client(t).Service(Spec), time.Minute)
	p.RunOnce(t.Context())
	for _, want := range []string{
		`openstack_port_status{id="p1",status="ACTIVE"} 1`,
		`openstack_port_status{id="p2",status="DOWN"} 1`,
	} {
		if !strings.Contains(string(p.Body(time.Now())), want) {
			t.Errorf("missing %q", want)
		}
	}
	ostest.Golden(t, "testdata/ports.golden", p.Body(time.Now()))
}

func TestFloatingIPsRender(t *testing.T) {
	cloud := ostest.New(t, "network")
	cloud.Handle("GET /network/v2.0/floatingips", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"floatingips": []map[string]any{
			{"id": "f1", "floating_ip_address": "203.0.113.10", "port_id": "p1", "status": "ACTIVE"},
			{"id": "f2", "floating_ip_address": "203.0.113.11", "port_id": "p2", "status": "DOWN"},
			{"id": "f3", "floating_ip_address": "203.0.113.12", "port_id": nil, "status": "DOWN"},
		}})
	})
	p := NewFloatingIPs(cloud.Client(t).Service(Spec), 5*time.Minute)
	p.RunOnce(t.Context())
	if p.Name() != "neutron-fip" {
		t.Errorf("name %q", p.Name())
	}
	for _, want := range []string{
		`openstack_floating_ip_status{id="f1",status="ACTIVE"} 1`,
		`openstack_floating_ip_status{id="f3",status="DOWN"} 1`,
	} {
		if !strings.Contains(string(p.Body(time.Now())), want) {
			t.Errorf("missing %q", want)
		}
	}
	ostest.Golden(t, "testdata/floatingips.golden", p.Body(time.Now()))
}
