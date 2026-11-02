package osclient

import (
	"fmt"
	"strings"
)

// Endpoint is one endpoint of a catalog entry.
type Endpoint struct {
	Interface string `json:"interface"`
	Region    string `json:"region"`
	RegionID  string `json:"region_id"`
	URL       string `json:"url"`
}

// CatalogEntry is one service in the Keystone service catalog.
type CatalogEntry struct {
	Type      string     `json:"type"`
	Endpoints []Endpoint `json:"endpoints"`
}

// Catalog is the Keystone service catalog returned with a token.
type Catalog []CatalogEntry

// EndpointURL returns the endpoint URL of the first of types present in the catalog, for the given
// interface and region. An empty region is accepted only when the service has a single region, counting
// every catalog entry of the type: Keystone allows several services of one type, for example one per region.
func (c Catalog) EndpointURL(types []string, region, iface string) (string, error) {
	for _, typ := range types {
		var urls []string
		regions := map[string]bool{}
		for _, e := range c {
			if e.Type != typ {
				continue
			}
			for _, ep := range e.Endpoints {
				if ep.Interface != iface {
					continue
				}
				if region != "" && ep.RegionID != region && ep.Region != region {
					continue
				}
				urls = append(urls, ep.URL)
				regions[ep.RegionID+"/"+ep.Region] = true
			}
		}
		switch {
		case len(urls) == 0:
			continue
		case len(regions) > 1:
			return "", fmt.Errorf("%w: %s; set OS_REGION_NAME", ErrAmbiguousEndpoint, typ)
		}
		return urls[0], nil
	}
	return "", fmt.Errorf("%w: type %s, interface %s, region %q", ErrNoEndpoint, strings.Join(types, "/"), iface, region)
}

// JoinURL appends a version suffix such as "/v2.0" to a catalog URL. Trailing slashes are removed,
// and leading suffix segments the base already ends with are not repeated.
func JoinURL(base, suffix string) string {
	base = strings.TrimRight(base, "/")
	segs := strings.Split(strings.Trim(suffix, "/"), "/")
	if suffix == "" {
		return base
	}
	for k := len(segs); k > 0; k-- {
		if strings.HasSuffix(base, "/"+strings.Join(segs[:k], "/")) {
			return base + joinSegs(segs[k:])
		}
	}
	return base + joinSegs(segs)
}

func joinSegs(segs []string) string {
	if len(segs) == 0 {
		return ""
	}
	return "/" + strings.Join(segs, "/")
}
