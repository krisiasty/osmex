package app

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/krisiasty/osmex/internal/osclient"
)

func endpoint(iface, region, url string) osclient.Endpoint {
	return osclient.Endpoint{Interface: iface, Region: region, RegionID: region, URL: url}
}

var selectCatalog = osclient.Catalog{
	{Type: "compute", Endpoints: []osclient.Endpoint{endpoint("public", "RegionOne", "https://nova")}},
	{Type: "block-storage", Endpoints: []osclient.Endpoint{
		endpoint("public", "RegionOne", "https://cinder-1"),
		endpoint("public", "RegionTwo", "https://cinder-2"),
	}},
	{Type: "network", Endpoints: []osclient.Endpoint{endpoint("internal", "RegionOne", "https://neutron-internal")}},
	{Type: "load-balancer", Endpoints: []osclient.Endpoint{endpoint("public", "RegionTwo", "https://octavia")}},
	// key-manager is not in the catalog at all.
}

func selected(t *testing.T, region string, excluded ...string) ([]string, string) {
	t.Helper()
	var logs bytes.Buffer
	var names []string
	for _, m := range selectServices(selectCatalog, region, "public", excluded, slog.New(slog.NewTextHandler(&logs, nil))) {
		names = append(names, m.Name)
	}
	return names, logs.String()
}

func TestSelectServicesRequiresUsableEndpoint(t *testing.T) {
	names, logs := selected(t, "RegionOne")
	if !slices.Equal(names, []string{"nova", "cinder"}) {
		t.Errorf("selected %v, want [nova cinder]: neutron has only an internal endpoint, octavia only RegionTwo, barbican none", names)
	}
	for _, skipped := range []string{"neutron", "octavia", "barbican"} {
		if !strings.Contains(logs, "service="+skipped) {
			t.Errorf("skipping %s not logged:\n%s", skipped, logs)
		}
	}
	if strings.Contains(logs, "level=ERROR") {
		t.Errorf("unexpected error log:\n%s", logs)
	}
}

func TestSelectServicesAmbiguousRegion(t *testing.T) {
	// Without OS_REGION_NAME, cinder's two regions are a configuration mistake: keep the service so its modules fail
	// visibly, and log an error. Octavia has a single region and is selected.
	names, logs := selected(t, "")
	if !slices.Equal(names, []string{"nova", "cinder", "octavia"}) {
		t.Errorf("selected %v, want [nova cinder octavia]", names)
	}
	if !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "service=cinder") || !strings.Contains(logs, "OS_REGION_NAME") {
		t.Errorf("ambiguous region not logged as an error:\n%s", logs)
	}
}

func TestSelectServicesExcluded(t *testing.T) {
	// An excluded service is left out even when its endpoint is usable, and logged as excluded rather than as missing.
	names, logs := selected(t, "RegionOne", "cinder", "barbican")
	if !slices.Equal(names, []string{"nova"}) {
		t.Errorf("selected %v, want [nova]", names)
	}
	for _, m := range []string{"cinder", "barbican"} {
		if !strings.Contains(logs, `msg="service excluded" service=`+m) {
			t.Errorf("exclusion of %s not logged:\n%s", m, logs)
		}
	}
	if strings.Contains(logs, "service=barbican err=") {
		t.Errorf("excluded barbican logged as missing:\n%s", logs)
	}
}
