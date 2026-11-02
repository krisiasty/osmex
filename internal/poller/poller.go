// Package poller runs background polls and serves their latest rendered snapshot.
package poller

import (
	"context"
	"log/slog"
	"maps"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/krisiasty/osmex/internal/osclient"
)

// Func performs one poll and returns the rendered metrics.
type Func func(ctx context.Context) ([]byte, error)

// Option configures a Poller.
type Option func(*Poller)

// WithClock sets the clock used for snapshot times and staleness.
func WithClock(now func() time.Time) Option { return func(p *Poller) { p.now = now } }

// WaitForFirstPoll delays this poller's first poll until each source poller's first poll has finished, successfully
// or not, so data it shares through Shared is available from the start. It waits at most limit in total, so a
// hanging source delays this poller only briefly.
func WaitForFirstPoll(limit time.Duration, sources ...*Poller) Option {
	return func(p *Poller) {
		p.waitFor = append(p.waitFor, sources...)
		p.waitMax = limit
	}
}

// WithLogger sets the logger for poll failures.
func WithLogger(l *slog.Logger) Option { return func(p *Poller) { p.log = l } }

// Poller runs a Func on an interval and keeps the last good snapshot. Snapshots are immutable
// once published, so readers take no locks.
type Poller struct {
	name     string
	interval func() time.Duration
	fn       Func
	now      func() time.Time
	log      *slog.Logger

	waitFor   []*Poller
	waitMax   time.Duration
	firstDone chan struct{} // closed when the first poll has finished
	firstOnce sync.Once

	good     atomic.Pointer[snapshot]
	up       atomic.Bool
	duration atomic.Int64

	mu     sync.Mutex
	errors map[string]uint64
}

type snapshot struct {
	body     []byte
	at       time.Time // when the poll ran
	dataAt   time.Time // when its oldest data was fetched: at, or earlier when the poll reused older data
	fallback *snapshot // the body without the reused data, served once dataAt is too old; nil if none
}

type pollKey struct{}

// pollState is what a poll function can reach through its context: when the poll started, the poller's interval
// and clock, the oldest data time and the fallback body reported so far.
type pollState struct {
	at       time.Time
	interval time.Duration
	now      func() time.Time

	mu       sync.Mutex
	oldest   time.Time
	fallback []byte // nil if none was reported
	failures int    // secondary calls that failed without failing the poll
	firstErr error  // the first of them
}

func pollFrom(ctx context.Context) *pollState {
	st, _ := ctx.Value(pollKey{}).(*pollState)
	return st
}

// ReportDataTime records that the body the current poll is building contains data fetched at t, for example a
// secondary list reused after its call failed. The snapshot's data age and its staleness limit then count from the
// oldest reported time, so reused data is never served for more than three intervals: past that, the poller serves
// the fallback body (see ReportFallback) or nothing. It is a no-op outside a poll.
func ReportDataTime(ctx context.Context, t time.Time) {
	st := pollFrom(ctx)
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.oldest.IsZero() || t.Before(st.oldest) {
		st.oldest = t
	}
}

// ReportFallback records the body to serve once the reused data reported through ReportDataTime is too old: the
// current poll's body without that data. Without it, all of the poll's data would go with the reused data, although
// the rest is still fresh. The fallback is dated by the poll itself; a poll that reports no data time doesn't need
// one, and its fallback is ignored. It is a no-op outside a poll.
func ReportFallback(ctx context.Context, body []byte) {
	st := pollFrom(ctx)
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.fallback = body
	if st.fallback == nil {
		st.fallback = []byte{}
	}
}

// ReportPartialFailure records a secondary call that failed without failing the poll, such as one load balancer's
// status. After a poll that still publishes, the poller logs one warning with the number of such failures and the
// first error, so a failing API is visible in the log without a line per call. It is a no-op outside a poll.
func ReportPartialFailure(ctx context.Context, err error) {
	st := pollFrom(ctx)
	if st == nil || err == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.failures == 0 {
		st.firstErr = err
	}
	st.failures++
}

// Status describes a poller for self-metrics.
type Status struct {
	Up          bool
	LastSuccess time.Time     // zero until the first success
	HasData     bool          // a snapshot is being served
	DataAge     time.Duration // age of the oldest data in the served snapshot
	Duration    time.Duration // duration of the last poll
	Errors      map[string]uint64
}

// staleAfter is how many intervals a snapshot is served after polls start failing.
const staleAfter = 3

// New returns a poller. interval is consulted before every poll, so it may change over time.
func New(name string, interval func() time.Duration, fn Func, opts ...Option) *Poller {
	p := &Poller{name: name, interval: interval, fn: fn, now: time.Now, log: slog.Default(), errors: map[string]uint64{},
		firstDone: make(chan struct{})}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Name returns the poller name.
func (p *Poller) Name() string { return p.name }

// Interval returns the current poll interval.
func (p *Poller) Interval() time.Duration { return p.interval() }

// Run polls immediately, then after a random offset of up to one interval, then every interval
// with ±10% jitter. Ticks that pass while a poll runs are skipped. Run returns when ctx is done.
func (p *Poller) Run(ctx context.Context) {
	if len(p.waitFor) > 0 {
		limit := time.NewTimer(p.waitMax)
	wait:
		for _, src := range p.waitFor {
			select {
			case <-src.firstDone:
			case <-limit.C:
				break wait
			case <-ctx.Done():
				limit.Stop()
				return
			}
		}
		limit.Stop()
	}
	p.RunOnce(ctx)
	next := time.Now().Add(firstOffset(p.Interval(), randFraction()))
	for {
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		p.RunOnce(ctx)
		for !next.After(time.Now()) {
			next = next.Add(jitter(p.Interval(), randFraction()))
		}
	}
}

// RunOnce performs one poll with a deadline of one interval and publishes the result.
// On failure the previous snapshot is kept. A poll interrupted by ctx being cancelled (shutdown) changes nothing.
func (p *Poller) RunOnce(ctx context.Context) {
	parent := ctx
	ctx, cancel := context.WithTimeout(osclient.WithRecorder(ctx, p), p.Interval())
	defer cancel()
	defer p.firstOnce.Do(func() { close(p.firstDone) })
	at := p.now()
	reported := &pollState{at: at, interval: p.Interval(), now: p.now}
	ctx = context.WithValue(ctx, pollKey{}, reported)
	body, err := p.fn(ctx)
	if parent.Err() != nil {
		// Shutdown interrupted the poll: its result says nothing about the API, so keep the last state and stay quiet.
		p.log.Debug("poll interrupted by shutdown", "poller", p.name)
		return
	}
	elapsed := p.now().Sub(at)
	p.duration.Store(int64(elapsed))
	if err != nil {
		p.up.Store(false)
		p.log.Warn("poll failed", "poller", p.name, "err", err, "duration", elapsed)
		return
	}
	snap := &snapshot{body: body, at: at, dataAt: at}
	if !reported.oldest.IsZero() && reported.oldest.Before(at) {
		snap.dataAt = reported.oldest
		if reported.fallback != nil {
			snap.fallback = &snapshot{body: reported.fallback, at: at, dataAt: at}
		}
	}
	p.good.Store(snap)
	p.up.Store(true)
	if reported.failures > 0 {
		p.log.Warn("poll partly failed", "poller", p.name, "failed_calls", reported.failures, "first_err", reported.firstErr)
	}
	p.log.Debug("poll succeeded", "poller", p.name, "duration", elapsed, "bytes", len(body))
}

// Body returns the latest good snapshot, or its fallback once its oldest data is older than three intervals, or nil
// if there is none or the fallback is too old as well.
func (p *Poller) Body(now time.Time) []byte {
	if s := p.served(now); s != nil {
		return s.body
	}
	return nil
}

// Status returns the poller state for self-metrics.
func (p *Poller) Status(now time.Time) Status {
	st := Status{Up: p.up.Load(), Duration: time.Duration(p.duration.Load())}
	if g := p.good.Load(); g != nil {
		st.LastSuccess = g.at
	}
	if s := p.served(now); s != nil {
		st.HasData = true
		st.DataAge = now.Sub(s.dataAt)
	}
	p.mu.Lock()
	st.Errors = maps.Clone(p.errors)
	p.mu.Unlock()
	return st
}

// RecordError counts a failed API request; it implements osclient.ErrorRecorder.
func (p *Poller) RecordError(code string) {
	p.mu.Lock()
	p.errors[code]++
	p.mu.Unlock()
}

func (p *Poller) served(now time.Time) *snapshot {
	s := p.good.Load()
	for s != nil && now.Sub(s.dataAt) > staleAfter*p.Interval() {
		s = s.fallback
	}
	return s
}

// randFraction returns a value in [0, 1) for spreading polls over time.
func randFraction() float64 {
	return rand.Float64() //nolint:gosec // G404: scheduling jitter needs no cryptographic randomness
}

func jitter(d time.Duration, r float64) time.Duration {
	return time.Duration(float64(d) * (0.9 + 0.2*r))
}

func firstOffset(d time.Duration, r float64) time.Duration {
	return time.Duration(float64(d) * r)
}
