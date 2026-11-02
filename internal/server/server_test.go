package server

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/expo"
	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/ostest"
	"github.com/krisiasty/osmex/internal/poller"
)

var t0 = time.Unix(1_800_000_000, 0)

func fixedPoller(name, body string, now func() time.Time) *poller.Poller {
	return poller.New(name, func() time.Duration { return time.Minute }, func(ctx context.Context) ([]byte, error) {
		if body == "" {
			osclient.RecordErrorCode(ctx, "503")
			return nil, errors.New("down")
		}
		return []byte(body), nil
	}, poller.WithClock(now))
}

func get(t *testing.T, h http.Handler, target string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), "GET", target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func opts(pollers []*poller.Poller, now func() time.Time) Options {
	names := make([]string, len(pollers))
	for i, p := range pollers {
		names[i] = p.Name()
	}
	return Options{
		Pollers:   func() []*poller.Poller { return pollers },
		Modules:   names,
		AuthUp:    func() bool { return true },
		Version:   "v1.0.0",
		GoVersion: "go1.27.3",
		Now:       now,
	}
}

func TestMetricsNoModules(t *testing.T) {
	o := opts(nil, func() time.Time { return t0 })
	o.AuthUp = nil
	rr := get(t, Handler(o), "/metrics")
	want := "# HELP openstack_exporter_build_info Build information of the exporter.\n" +
		"# TYPE openstack_exporter_build_info gauge\n" +
		"openstack_exporter_build_info{version=\"v1.0.0\",goversion=\"go1.27.3\"} 1\n"
	if rr.Code != 200 || rr.Body.String() != want {
		t.Errorf("code %d body\n%s", rr.Code, rr.Body)
	}
}

func TestMetricsConcatenates(t *testing.T) {
	now := t0
	clk := func() time.Time { return now }
	a := fixedPoller("a", "a_metric 1\n", clk)
	b := fixedPoller("b", "", clk)
	a.RunOnce(t.Context())
	b.RunOnce(t.Context())
	now = now.Add(15 * time.Second)
	rr := get(t, Handler(opts([]*poller.Poller{a, b}, clk)), "/metrics")
	ostest.Golden(t, "testdata/metrics.golden", rr.Body.Bytes())
}

func TestModuleFilter(t *testing.T) {
	clk := func() time.Time { return t0 }
	a := fixedPoller("a", "a_metric 1\n", clk)
	b := fixedPoller("b", "b_metric 1\n", clk)
	a.RunOnce(t.Context())
	b.RunOnce(t.Context())
	body := get(t, Handler(opts([]*poller.Poller{a, b}, clk)), "/metrics?module=b").Body.String()
	if strings.Contains(body, "a_metric") || strings.Contains(body, `poller="a"`) {
		t.Errorf("filtered output contains poller a:\n%s", body)
	}
	if !strings.Contains(body, "b_metric 1\n") || !strings.Contains(body, `openstack_exporter_api_up{poller="b"} 1`) ||
		!strings.Contains(body, "openstack_exporter_build_info") || !strings.Contains(body, "openstack_exporter_auth_up 1") {
		t.Errorf("filtered output missing poller b or global series:\n%s", body)
	}
	both := get(t, Handler(opts([]*poller.Poller{a, b}, clk)), "/metrics?module=a&module=b").Body.String()
	if !strings.Contains(both, "a_metric") || !strings.Contains(both, "b_metric") {
		t.Errorf("repeated module parameter:\n%s", both)
	}
}

func TestUnknownModule(t *testing.T) {
	a := fixedPoller("a", "a_metric 1\n", time.Now)
	rr := get(t, Handler(opts([]*poller.Poller{a}, time.Now)), "/metrics?module=zzz")
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "a") {
		t.Errorf("code %d body %q", rr.Code, rr.Body)
	}
}

func TestStaleOmitsDataAge(t *testing.T) {
	now := t0
	clk := func() time.Time { return now }
	a := fixedPoller("a", "a_metric 1\n", clk)
	a.RunOnce(t.Context())
	now = now.Add(4 * time.Minute)
	body := get(t, Handler(opts([]*poller.Poller{a}, clk)), "/metrics").Body.String()
	if strings.Contains(body, "a_metric") || strings.Contains(body, "openstack_exporter_data_age_seconds") {
		t.Errorf("stale data served:\n%s", body)
	}
	if !strings.Contains(body, `openstack_exporter_last_success_timestamp_seconds{poller="a"} 1.8e+09`) {
		t.Errorf("last success missing:\n%s", body)
	}
}

func TestGzip(t *testing.T) {
	clk := func() time.Time { return t0 }
	h := Handler(opts(nil, clk))
	plain := get(t, h, "/metrics").Body.String()
	rr := get(t, h, "/metrics", "Accept-Encoding", "gzip")
	if rr.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", rr.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(rr.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != plain {
		t.Errorf("gzip body differs:\n%s\nvs\n%s", got, plain)
	}
}

func TestAcceptEncoding(t *testing.T) {
	h := Handler(opts(nil, func() time.Time { return t0 }))
	for header, gzipped := range map[string]bool{
		"":                        false,
		"gzip":                    true,
		"GZIP":                    true,
		"x-gzip":                  true,
		"gzip;q=0.5":              true,
		"gzip ; q=1.0":            true,
		"deflate, gzip":           true,
		"*":                       true,
		"identity, *;q=0.1":       true,
		"gzip;q=0":                false,
		"gzip;q=0.000":            false,
		"gzip;q=0, identity;q=1":  false,
		"identity":                false,
		"deflate":                 false,
		"x-gzip-foo":              false,
		"*;q=0":                   false,
		"*, gzip;q=0":             false,
		"gzip;q=invalid":          false,
		"gzip;q=-1, *":            false,
		"br;q=1, gzip;q=0.001":    true,
		"identity;q=1, *;q=0":     false,
		"gzip;level=1;q=0.8":      true,
		"deflate;q=0.5, *;q=0.25": true,
	} {
		rr := get(t, h, "/metrics", "Accept-Encoding", header)
		if got := rr.Header().Get("Content-Encoding") == "gzip"; got != gzipped {
			t.Errorf("Accept-Encoding %q: gzip %v, want %v", header, got, gzipped)
		}
		if rr.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("Accept-Encoding %q: Vary = %q", header, rr.Header().Get("Vary"))
		}
	}
}

func TestErrorCodeBaseline(t *testing.T) {
	// Common error codes start at 0 for every poller, so increase() sees their first occurrence.
	clk := func() time.Time { return t0 }
	pollers := []*poller.Poller{fixedPoller("a", "a_metric 1\n", clk), fixedPoller("b", "", clk)}
	for _, p := range pollers {
		p.RunOnce(t.Context())
	}
	body := get(t, Handler(opts(pollers, clk)), "/metrics").Body.String()
	for _, code := range osclient.BaselineErrorCodes {
		if want := `openstack_exporter_api_errors_total{poller="a",code="` + code + `"} 0`; !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.Contains(body, `openstack_exporter_api_errors_total{poller="b",code="503"} 1`) ||
		strings.Count(body, `openstack_exporter_api_errors_total{poller="b",code="503"}`) != 1 {
		t.Errorf("poller b's 503 count missing or duplicated:\n%s", body)
	}
	for _, code := range []string{"timeout", "decode", "500", "auth"} {
		if !slices.Contains(osclient.BaselineErrorCodes, code) {
			t.Errorf("%s not in the baseline", code)
		}
	}
}

func TestContentType(t *testing.T) {
	rr := get(t, Handler(opts(nil, time.Now)), "/metrics")
	if ct := rr.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestHealthz(t *testing.T) {
	rr := get(t, Handler(opts(nil, time.Now)), "/healthz")
	if rr.Code != 200 || rr.Body.String() != "ok\n" {
		t.Errorf("code %d body %q", rr.Code, rr.Body)
	}
}

func TestRuntimeHook(t *testing.T) {
	clk := func() time.Time { return t0 }
	a := fixedPoller("a", "a_metric 1\n", clk)
	a.RunOnce(t.Context())
	o := opts([]*poller.Poller{a}, clk)
	o.Runtime = func(p *expo.Page) { p.Gauge("go_goroutines", "Goroutines.").Add(7) }
	for _, target := range []string{"/metrics", "/metrics?module=a"} {
		body := get(t, Handler(o), target).Body.String()
		if !strings.HasSuffix(body, "# TYPE go_goroutines gauge\ngo_goroutines 7\n") {
			t.Errorf("%s: runtime metrics not at the end:\n%s", target, body)
		}
	}
}

func TestKnownModuleNotRunning(t *testing.T) {
	a := fixedPoller("a", "a_metric 1\n", time.Now)
	a.RunOnce(t.Context())
	o := opts([]*poller.Poller{a}, time.Now)
	o.Modules = []string{"a", "b"}
	rr := get(t, Handler(o), "/metrics?module=b")
	body := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(body, "openstack_exporter_build_info") || !strings.Contains(body, "openstack_exporter_auth_up 1") ||
		strings.Contains(body, "a_metric") || strings.Contains(body, `poller="a"`) {
		t.Errorf("known poller not running yet: code %d body\n%s", rr.Code, body)
	}
	rr = get(t, Handler(o), "/metrics?module=zzz")
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "a, b") {
		t.Errorf("unknown module: code %d body %q", rr.Code, rr.Body)
	}
}
