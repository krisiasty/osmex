package poller

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/osclient"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func every(d time.Duration) func() time.Duration { return func() time.Duration { return d } }

func TestRunOncePublishes(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	p := New("p", every(time.Minute), func(context.Context) ([]byte, error) { return []byte("x 1\n"), nil }, WithClock(clk.now))
	if p.Name() != "p" || p.Interval() != time.Minute {
		t.Fatalf("name %q interval %v", p.Name(), p.Interval())
	}
	p.RunOnce(t.Context())
	if got := string(p.Body(clk.now())); got != "x 1\n" {
		t.Errorf("body = %q", got)
	}
	st := p.Status(clk.now())
	if !st.Up || !st.HasData || !st.LastSuccess.Equal(time.Unix(1000, 0)) || st.DataAge != 0 {
		t.Errorf("status = %+v", st)
	}
	clk.advance(10 * time.Second)
	if st := p.Status(clk.now()); st.DataAge != 10*time.Second {
		t.Errorf("data age = %v", st.DataAge)
	}
}

func TestFailureKeepsSnapshot(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	fail := false
	p := New("p", every(time.Minute), func(context.Context) ([]byte, error) {
		if fail {
			return nil, errors.New("boom")
		}
		return []byte("x 1\n"), nil
	}, WithClock(clk.now))
	p.RunOnce(t.Context())
	fail = true
	clk.advance(time.Minute)
	p.RunOnce(t.Context())
	if got := string(p.Body(clk.now())); got != "x 1\n" {
		t.Errorf("body = %q", got)
	}
	st := p.Status(clk.now())
	if st.Up || !st.HasData || !st.LastSuccess.Equal(time.Unix(1000, 0)) {
		t.Errorf("status = %+v", st)
	}
}

func TestStaleSnapshotDropped(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	fail := false
	p := New("p", every(time.Minute), func(context.Context) ([]byte, error) {
		if fail {
			return nil, errors.New("boom")
		}
		return []byte("x 1\n"), nil
	}, WithClock(clk.now))
	p.RunOnce(t.Context())
	fail = true
	clk.advance(3 * time.Minute)
	p.RunOnce(t.Context())
	if p.Body(clk.now()) == nil {
		t.Error("body dropped at exactly 3× interval")
	}
	clk.advance(time.Second)
	if p.Body(clk.now()) != nil || p.Status(clk.now()).HasData {
		t.Error("stale body still served")
	}
}

func TestFailureBeforeSuccess(t *testing.T) {
	p := New("p", every(time.Minute), func(context.Context) ([]byte, error) { return nil, errors.New("boom") })
	p.RunOnce(t.Context())
	st := p.Status(time.Now())
	if p.Body(time.Now()) != nil || st.Up || st.HasData || !st.LastSuccess.IsZero() {
		t.Errorf("status = %+v", st)
	}
}

func TestStatusBeforeFirstPoll(t *testing.T) {
	p := New("p", every(time.Minute), func(context.Context) ([]byte, error) { return nil, nil })
	if st := p.Status(time.Now()); st.Up || st.HasData {
		t.Errorf("status = %+v", st)
	}
}

func TestDeadline(t *testing.T) {
	var deadline time.Time
	p := New("p", every(time.Minute), func(ctx context.Context) ([]byte, error) {
		deadline, _ = ctx.Deadline()
		return nil, nil
	})
	p.RunOnce(t.Context())
	if d := time.Until(deadline); d < 59*time.Second || d > time.Minute {
		t.Errorf("deadline in %v", d)
	}
}

func TestRecorderInContext(t *testing.T) {
	p := New("p", every(time.Minute), func(ctx context.Context) ([]byte, error) {
		osclient.RecordErrorCode(ctx, "500")
		osclient.RecordErrorCode(ctx, "500")
		osclient.RecordErrorCode(ctx, "timeout")
		return nil, nil
	})
	p.RunOnce(t.Context())
	errs := p.Status(time.Now()).Errors
	if errs["500"] != 2 || errs["timeout"] != 1 {
		t.Errorf("errors = %v", errs)
	}
}

func TestJitterBounds(t *testing.T) {
	if jitter(time.Minute, 0) != 54*time.Second || jitter(time.Minute, 1) != 66*time.Second || jitter(time.Minute, 0.5) != time.Minute {
		t.Error("jitter")
	}
	if firstOffset(time.Minute, 0) != 0 || firstOffset(time.Minute, 0.5) != 30*time.Second {
		t.Error("firstOffset")
	}
}

func TestRunSkipsWhileRunning(t *testing.T) {
	var running, calls, overlaps atomic.Int32
	p := New("p", every(20*time.Millisecond), func(ctx context.Context) ([]byte, error) {
		if running.Add(1) > 1 {
			overlaps.Add(1)
		}
		calls.Add(1)
		time.Sleep(70 * time.Millisecond)
		running.Add(-1)
		return nil, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	p.Run(ctx)
	if n := calls.Load(); n < 2 || n > 5 {
		t.Errorf("calls = %d", n)
	}
	if overlaps.Load() != 0 {
		t.Error("polls overlapped")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	p := New("p", every(time.Hour), func(context.Context) ([]byte, error) { return nil, nil })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestReportedDataTimeBoundsStaleness(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	reuse := false
	p := New("p", every(time.Minute), func(ctx context.Context) ([]byte, error) {
		if reuse {
			ReportDataTime(ctx, time.Unix(1000, 0)) // part of this body was fetched at t=1000
		}
		return []byte("x 1\n"), nil
	}, WithClock(clk.now))
	p.RunOnce(t.Context())

	clk.advance(2 * time.Minute)
	reuse = true
	p.RunOnce(t.Context())
	clk.advance(10 * time.Second)
	st := p.Status(clk.now())
	if !st.LastSuccess.Equal(time.Unix(1120, 0)) {
		t.Errorf("last success = %v, want the poll time", st.LastSuccess)
	}
	if st.DataAge != 130*time.Second {
		t.Errorf("data age = %v, want 130s: the age of the oldest reused data", st.DataAge)
	}

	clk.t = time.Unix(1000+180, 0)
	if p.Body(clk.now()) == nil {
		t.Error("body dropped while the oldest data is exactly 3× the interval old")
	}
	clk.t = time.Unix(1000+181, 0)
	if p.Body(clk.now()) != nil || p.Status(clk.now()).HasData {
		t.Error("body still served with data older than 3× the interval")
	}
}

func TestFallbackServedAfterReusedDataExpires(t *testing.T) {
	clk := &clock{t: time.Unix(1120, 0)}
	p := New("p", every(time.Minute), func(ctx context.Context) ([]byte, error) {
		ReportDataTime(ctx, time.Unix(1000, 0)) // part of this body was fetched at t=1000
		ReportFallback(ctx, []byte("fresh 1\n"))
		return []byte("fresh 1\nreused 1\n"), nil
	}, WithClock(clk.now))
	p.RunOnce(t.Context())

	clk.t = time.Unix(1000+180, 0)
	if got := string(p.Body(clk.now())); got != "fresh 1\nreused 1\n" {
		t.Errorf("body %q while the reused data is exactly 3× the interval old, want the full body", got)
	}
	// Past that, the reused data is dropped but the rest of the poll is still fresh.
	clk.t = time.Unix(1000+181, 0)
	if got := string(p.Body(clk.now())); got != "fresh 1\n" {
		t.Errorf("body %q once the reused data is too old, want the fallback", got)
	}
	if st := p.Status(clk.now()); !st.HasData || st.DataAge != 61*time.Second {
		t.Errorf("status %+v, want data dated by the poll (61s old)", st)
	}
	// The fallback expires with the poll's own data.
	clk.t = time.Unix(1120+181, 0)
	if p.Body(clk.now()) != nil || p.Status(clk.now()).HasData {
		t.Error("fallback still served more than 3× the interval after the poll")
	}
}

func TestFallbackWithoutReusedDataIgnored(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	p := New("p", every(time.Minute), func(ctx context.Context) ([]byte, error) {
		ReportFallback(ctx, []byte("other 1\n"))
		return []byte("x 1\n"), nil
	}, WithClock(clk.now))
	p.RunOnce(t.Context())
	clk.advance(3 * time.Minute)
	if got := string(p.Body(clk.now())); got != "x 1\n" {
		t.Errorf("body %q, want the poll's body", got)
	}
}

func TestReportedDataTimeAfterPollStartIgnored(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	p := New("p", every(time.Minute), func(ctx context.Context) ([]byte, error) {
		ReportDataTime(ctx, time.Unix(1005, 0))
		return []byte("x 1\n"), nil
	}, WithClock(clk.now))
	p.RunOnce(t.Context())
	if st := p.Status(clk.now()); st.DataAge != 0 {
		t.Errorf("data age = %v, want 0", st.DataAge)
	}
}

func TestReportDataTimeWithoutPoller(t *testing.T) {
	ReportDataTime(t.Context(), time.Now()) // must not panic outside a poll
}

func TestPartialFailuresLoggedOncePerPoll(t *testing.T) {
	var logs strings.Builder
	p := New("p", every(time.Minute), func(ctx context.Context) ([]byte, error) {
		ReportPartialFailure(ctx, errors.New("GET /x/1: HTTP 500"))
		ReportPartialFailure(ctx, errors.New("GET /x/2: HTTP 500"))
		ReportPartialFailure(ctx, errors.New("GET /x/3: HTTP 500"))
		return []byte("x 1\n"), nil
	}, WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	p.RunOnce(t.Context())
	out := logs.String()
	if n := strings.Count(out, "level=WARN"); n != 1 {
		t.Fatalf("%d warnings, want 1:\n%s", n, out)
	}
	for _, want := range []string{`msg="poll partly failed"`, "poller=p", "failed_calls=3", `first_err="GET /x/1: HTTP 500"`} {
		if !strings.Contains(out, want) {
			t.Errorf("warning lacks %q:\n%s", want, out)
		}
	}
	if !p.Status(time.Now()).Up {
		t.Error("a poll with partial failures must still count as successful")
	}
}

func TestNoPartialFailuresNoWarning(t *testing.T) {
	var logs strings.Builder
	p := New("p", every(time.Minute), func(ctx context.Context) ([]byte, error) {
		return []byte("x 1\n"), nil
	}, WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	p.RunOnce(t.Context())
	ReportPartialFailure(t.Context(), errors.New("outside a poll")) // must not panic
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("unexpected warning:\n%s", logs.String())
	}
}

func TestShutdownDuringPollIsNotAFailure(t *testing.T) {
	for name, result := range map[string]error{"poll fails": context.Canceled, "poll publishes": nil} {
		t.Run(name, func(t *testing.T) {
			var logs strings.Builder
			ctx, shutdown := context.WithCancel(t.Context())
			first := true
			p := New("p", every(time.Minute), func(ctx context.Context) ([]byte, error) {
				if first {
					first = false
					return []byte("x 1\n"), nil
				}
				shutdown() // SIGTERM arrives while the poll runs
				osclient.RecordErrorCode(ctx, "other")
				ReportPartialFailure(ctx, ctx.Err())
				if result != nil {
					return nil, result
				}
				return []byte("x 2\n"), nil
			}, WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
			p.RunOnce(ctx)
			p.RunOnce(ctx)
			st := p.Status(time.Now())
			if strings.Contains(logs.String(), "level=WARN") || !st.Up || len(st.Errors) != 0 {
				t.Errorf("up %v, errors %v, logs:\n%s", st.Up, st.Errors, logs.String())
			}
		})
	}
}

func TestAttempts(t *testing.T) {
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	p := New("p", every(time.Minute), func(context.Context) ([]byte, error) { return nil, errors.New("boom") }, WithClock(clk.now))
	if !p.LastAttempt().IsZero() || p.Attempted() {
		t.Fatalf("before the first poll: LastAttempt %v, Attempted %v", p.LastAttempt(), p.Attempted())
	}
	p.RunOnce(t.Context())
	// A failed poll is an attempt all the same.
	if !p.LastAttempt().Equal(clk.now()) || !p.Attempted() {
		t.Errorf("after a failed poll: LastAttempt %v, Attempted %v", p.LastAttempt(), p.Attempted())
	}
	clk.advance(time.Minute)
	p.RunOnce(t.Context())
	if !p.LastAttempt().Equal(clk.now()) {
		t.Errorf("LastAttempt %v, want %v", p.LastAttempt(), clk.now())
	}
}

func TestRunCountsAsAttemptWhileWaiting(t *testing.T) {
	// A poller waiting for its sources has started, so liveness must not see it as never having run.
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	src := New("src", every(time.Minute), func(context.Context) ([]byte, error) { return nil, nil })
	p := New("p", every(time.Minute), func(context.Context) ([]byte, error) { return nil, nil }, WithClock(clk.now), WaitForFirstPoll(time.Hour, src))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for p.LastAttempt().IsZero() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if !p.LastAttempt().Equal(clk.now()) || p.Attempted() {
		t.Errorf("waiting: LastAttempt %v, Attempted %v", p.LastAttempt(), p.Attempted())
	}
}
