package server

import (
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/krisiasty/osmex/internal/poller"
)

// ProbePaths are the endpoints that answer whether osmex itself is working. They carry no OpenStack data, so they are
// served without authentication.
var ProbePaths = []string{"/livez", "/readyz"}

// liveIntervals is how many of its intervals a poller may go without starting a poll before liveness fails. Liveness
// has one remedy, a restart, and that helps only a poll loop that has stopped; a failing OpenStack API is reported by
// api_up and must not restart a working exporter.
const liveIntervals = 10

// Health answers the probes from the pollers' state.
type Health struct {
	pollers func() []*poller.Poller
	settled func() bool
	now     func() time.Time
	ready   atomic.Bool
}

// NewHealth returns a Health over pollers. settled reports whether pollers returns the final set, or osmex has stopped
// waiting for it; nil means always.
func NewHealth(pollers func() []*poller.Poller, settled func() bool, now func() time.Time) *Health {
	if settled == nil {
		settled = func() bool { return true }
	}
	if now == nil {
		now = time.Now
	}
	return &Health{pollers: pollers, settled: settled, now: now}
}

// Live reports whether every poller has started a poll within liveIntervals of its current interval.
func (h *Health) Live() bool {
	now := h.now()
	for _, p := range h.pollers() {
		if last := p.LastAttempt(); !last.IsZero() && now.Sub(last) > liveIntervals*p.Interval() {
			return false
		}
	}
	return true
}

// Ready reports whether osmex has finished starting: the set of pollers is settled and each has finished its first
// poll, successfully or not. It is a latch: once ready, osmex stays ready, because a not-ready exporter is taken out of
// service exactly when its metrics would explain an OpenStack outage.
func (h *Health) Ready() bool {
	if h.ready.Load() {
		return true
	}
	if !h.settled() {
		return false
	}
	for _, p := range h.pollers() {
		if !p.Attempted() {
			return false
		}
	}
	h.ready.Store(true)
	return true
}

func (h *Health) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		if !h.Live() {
			http.Error(w, "poll loop stalled", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !h.Ready() {
			http.Error(w, "waiting for the first poll of every module", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
}

// ProbeHandler serves only the probes, for a listener of their own.
func ProbeHandler(h *Health) http.Handler {
	mux := http.NewServeMux()
	h.register(mux)
	return mux
}
