// Package server serves /metrics, /livez and /readyz.
package server

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/krisiasty/osmex/internal/expo"
	"github.com/krisiasty/osmex/internal/poller"
)

// Options configures the handler.
type Options struct {
	Pollers   func() []*poller.Poller // current poller modules; the set may grow once after -all resolution
	Modules   []string                // module names ?module= accepts for the enabled services, running or not
	AuthUp    func() bool             // nil when no service is enabled: auth_up is not emitted
	Version   string
	GoVersion string
	Now       func() time.Time // nil → time.Now
	Runtime   func(*expo.Page) // adds the process's own runtime metrics at scrape time; nil → none
	Health    *Health          // nil → NewHealth(Pollers, nil, Now)
}

// Handler returns the exporter's HTTP handler.
func Handler(o Options) http.Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Health == nil {
		o.Health = NewHealth(o.Pollers, nil, o.Now)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) { serveMetrics(w, r, o) })
	o.Health.register(mux)
	return mux
}

func serveMetrics(w http.ResponseWriter, r *http.Request, o Options) {
	now := o.Now()
	all := o.Pollers()
	selected := all
	if modules := r.URL.Query()["module"]; len(modules) > 0 {
		for _, m := range modules {
			if !slices.Contains(o.Modules, m) {
				http.Error(w, fmt.Sprintf("unknown module %q; valid modules: %s", m, strings.Join(o.Modules, ", ")), http.StatusBadRequest)
				return
			}
		}
		// A known module that has not started yet (-all before the first authentication) contributes nothing, so the
		// scrape still returns the global series such as auth_up.
		selected = slices.DeleteFunc(slices.Clone(all), func(p *poller.Poller) bool { return !slices.Contains(modules, p.Name()) })
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Vary", "Accept-Encoding")
	var out io.Writer = w
	if acceptsGzip(r.Header.Values("Accept-Encoding")) {
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		// Close flushes to the scraper; if that fails the scraper has gone and nobody is left to tell.
		defer func() { _ = zw.Close() }()
		out = zw
	}
	chunks := make([][]byte, 0, len(selected)+1)
	for _, p := range selected {
		chunks = append(chunks, p.Body(now))
	}
	chunks = append(chunks, renderSelf(o, selected, now))
	for _, c := range chunks {
		if _, err := out.Write(c); err != nil {
			return // the scraper went away
		}
	}
}

// acceptsGzip reports whether an Accept-Encoding header allows a gzip response (RFC 9110, section 12.5.3): gzip or
// its x-gzip alias with a q value above 0, or, if neither is listed, * with a q value above 0. A q value that does not
// parse, or is not positive, counts as 0.
func acceptsGzip(headers []string) bool {
	named, wildcard := -1.0, -1.0 // -1: not listed
	for _, h := range headers {
		for item := range strings.SplitSeq(h, ",") {
			coding, params, _ := strings.Cut(item, ";")
			q := 1.0
			for param := range strings.SplitSeq(params, ";") {
				name, value, ok := strings.Cut(param, "=")
				if ok && strings.EqualFold(strings.TrimSpace(name), "q") {
					var err error
					if q, err = strconv.ParseFloat(strings.TrimSpace(value), 64); err != nil || !(q > 0) {
						q = 0 // also negative and NaN
					}
				}
			}
			switch coding = strings.ToLower(strings.TrimSpace(coding)); coding {
			case "gzip", "x-gzip":
				named = max(named, q)
			case "*":
				wildcard = max(wildcard, q)
			}
		}
	}
	if named >= 0 {
		return named > 0
	}
	return wildcard > 0
}
