package osclient

import (
	"errors"
	"strings"
	"testing"
)

var testCatalog = Catalog{
	{Type: "compute", Endpoints: []Endpoint{
		{Interface: "public", RegionID: "r1", Region: "r1", URL: "https://nova/v2.1"},
		{Interface: "internal", RegionID: "r1", Region: "r1", URL: "http://nova-int/v2.1"},
	}},
	{Type: "volumev3", Endpoints: []Endpoint{{Interface: "public", RegionID: "r1", URL: "https://cinder-old/v3/p"}}},
	{Type: "block-storage", Endpoints: []Endpoint{{Interface: "public", RegionID: "r1", URL: "https://cinder/v3/p"}}},
	{Type: "network", Endpoints: []Endpoint{
		{Interface: "public", RegionID: "r1", URL: "https://neutron-1"},
		{Interface: "public", RegionID: "r2", URL: "https://neutron-2"},
	}},
	{Type: "key-manager", Endpoints: []Endpoint{{Interface: "public", Region: "legacy", URL: "https://kms"}}},
}

func TestEndpointURL(t *testing.T) {
	cases := []struct {
		types         []string
		region, iface string
		want          string
	}{
		{[]string{"compute"}, "r1", "public", "https://nova/v2.1"},
		{[]string{"compute"}, "", "internal", "http://nova-int/v2.1"},
		{[]string{"block-storage", "volumev3"}, "r1", "public", "https://cinder/v3/p"},
		{[]string{"network"}, "r2", "public", "https://neutron-2"},
		{[]string{"key-manager"}, "legacy", "public", "https://kms"},
	}
	for _, c := range cases {
		got, err := testCatalog.EndpointURL(c.types, c.region, c.iface)
		if err != nil || got != c.want {
			t.Errorf("%v %q %q = %q, %v; want %q", c.types, c.region, c.iface, got, err, c.want)
		}
	}
}

func TestEndpointURLAmbiguousRegion(t *testing.T) {
	_, err := testCatalog.EndpointURL([]string{"network"}, "", "public")
	if err == nil || !strings.Contains(err.Error(), "OS_REGION_NAME") {
		t.Errorf("err = %v", err)
	}
	// A configuration mistake, counted as a catalog problem, but not "no endpoint": -all keeps such a module enabled.
	if !errors.Is(err, ErrAmbiguousEndpoint) || errors.Is(err, ErrNoEndpoint) || ErrorCode(err) != "catalog" {
		t.Errorf("err %v: ambiguous %v, no endpoint %v, code %q",
			err, errors.Is(err, ErrAmbiguousEndpoint), errors.Is(err, ErrNoEndpoint), ErrorCode(err))
	}
}

func TestEndpointURLSplitEntries(t *testing.T) {
	// Keystone allows several services of one type, for example one per region: ambiguity spans all of them.
	split := Catalog{
		{Type: "compute", Endpoints: []Endpoint{
			{Interface: "public", RegionID: "r1", URL: "https://nova-1"},
			{Interface: "internal", RegionID: "r2", URL: "http://nova-2-int"},
		}},
		{Type: "compute", Endpoints: []Endpoint{{Interface: "public", RegionID: "r2", URL: "https://nova-2"}}},
		{Type: "network", Endpoints: []Endpoint{{Interface: "public", RegionID: "r1", URL: "https://neutron-a"}}},
		{Type: "network", Endpoints: []Endpoint{{Interface: "public", RegionID: "r1", URL: "https://neutron-b"}}},
	}
	if _, err := split.EndpointURL([]string{"compute"}, "", "public"); !errors.Is(err, ErrAmbiguousEndpoint) {
		t.Errorf("compute entries in r1 and r2, no region: err = %v, want ambiguous", err)
	}
	for _, c := range []struct {
		types               []string
		region, iface, want string
	}{
		{[]string{"compute"}, "r1", "public", "https://nova-1"},
		{[]string{"compute"}, "r2", "public", "https://nova-2"},
		{[]string{"compute"}, "", "internal", "http://nova-2-int"}, // only r2 has an internal endpoint
		{[]string{"network"}, "", "public", "https://neutron-a"},   // both in r1: the first, as within one entry
	} {
		got, err := split.EndpointURL(c.types, c.region, c.iface)
		if err != nil || got != c.want {
			t.Errorf("%v %q %q = %q, %v; want %q", c.types, c.region, c.iface, got, err, c.want)
		}
	}
}

func TestEndpointURLMissing(t *testing.T) {
	for _, c := range []struct {
		types         []string
		region, iface string
	}{
		{[]string{"load-balancer"}, "r1", "public"},
		{[]string{"compute"}, "r9", "public"},
		{[]string{"compute"}, "r1", "admin"},
	} {
		_, err := testCatalog.EndpointURL(c.types, c.region, c.iface)
		if !errors.Is(err, ErrNoEndpoint) {
			t.Errorf("%v: err = %v", c, err)
		}
	}
}

func TestJoinURL(t *testing.T) {
	cases := []struct{ base, suffix, want string }{
		{"http://h/v2.1/", "", "http://h/v2.1"},
		{"http://h:9696", "/v2.0", "http://h:9696/v2.0"},
		{"http://h:9696/", "/v2.0", "http://h:9696/v2.0"},
		{"http://h:9696/v2.0/", "/v2.0", "http://h:9696/v2.0"},
		{"http://h/load-balancer", "/v2/lbaas", "http://h/load-balancer/v2/lbaas"},
		{"http://h/load-balancer/v2", "/v2/lbaas", "http://h/load-balancer/v2/lbaas"},
		{"http://h/kms/v1", "/v1", "http://h/kms/v1"},
	}
	for _, c := range cases {
		if got := JoinURL(c.base, c.suffix); got != c.want {
			t.Errorf("JoinURL(%q, %q) = %q, want %q", c.base, c.suffix, got, c.want)
		}
	}
}
