package server

import (
	"maps"
	"slices"
	"time"

	"github.com/krisiasty/osmex/internal/expo"
	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/poller"
)

func renderSelf(o Options, pollers []*poller.Poller, now time.Time) []byte {
	var page expo.Page
	page.Gauge("openstack_exporter_build_info", "Build information of the exporter.").
		Add(1, "version", o.Version, "goversion", o.GoVersion)
	if o.AuthUp != nil {
		page.Gauge("openstack_exporter_auth_up", "Whether the exporter holds a valid Keystone token.").Add(expo.Bool(o.AuthUp()))
	}
	up := page.Gauge("openstack_exporter_api_up", "Whether the last poll succeeded.")
	lastSuccess := page.Gauge("openstack_exporter_last_success_timestamp_seconds", "Time of the last successful poll.")
	dataAge := page.Gauge("openstack_exporter_data_age_seconds", "Age of the data being served.")
	duration := page.Gauge("openstack_exporter_poll_duration_seconds", "Duration of the last poll.")
	errorsTotal := page.Counter("openstack_exporter_api_errors_total", "Failed OpenStack API requests by error code.")
	for _, p := range pollers {
		st := p.Status(now)
		name := p.Name()
		up.Add(expo.Bool(st.Up), "poller", name)
		if !st.LastSuccess.IsZero() {
			lastSuccess.Add(float64(st.LastSuccess.UnixNano())/1e9, "poller", name)
		}
		if st.HasData {
			dataAge.Add(st.DataAge.Seconds(), "poller", name)
		}
		duration.Add(st.Duration.Seconds(), "poller", name)
		for _, code := range osclient.BaselineErrorCodes {
			errorsTotal.Add(float64(st.Errors[code]), "poller", name, "code", code)
		}
		for _, code := range slices.Sorted(maps.Keys(st.Errors)) {
			if !slices.Contains(osclient.BaselineErrorCodes, code) {
				errorsTotal.Add(float64(st.Errors[code]), "poller", name, "code", code)
			}
		}
	}
	if o.Runtime != nil {
		o.Runtime(&page)
	}
	return page.Bytes()
}
