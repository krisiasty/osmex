package poller

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestSharedPublishGet(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	var ids Shared[[]string]
	src := New("src", every(time.Minute), func(ctx context.Context) ([]byte, error) {
		clk.advance(30 * time.Second) // a slow list call: the value is still dated by the poll's start
		ids.Publish(ctx, []string{"a", "b"})
		return nil, nil
	}, WithClock(clk.now))
	var got []string
	var ok bool
	dst := New("dst", every(time.Minute), func(ctx context.Context) ([]byte, error) {
		got, ok = ids.Get(ctx)
		return []byte("x 1\n"), nil
	}, WithClock(clk.now))

	src.RunOnce(t.Context()) // starts at t=1000, publishes at t=1030
	clk.advance(60 * time.Second)
	dst.RunOnce(t.Context()) // t=1090
	if !ok || len(got) != 2 {
		t.Fatalf("Get = %v, %v", got, ok)
	}
	// A shared value only shapes the consumer's output; it does not age the consumer's own data.
	if st := dst.Status(clk.now()); st.DataAge != 0 {
		t.Errorf("consumer data age = %v, want 0", st.DataAge)
	}
}

func TestSharedShorterConsumerInterval(t *testing.T) {
	// A consumer polling more often than the publisher must still serve what it just fetched.
	clk := &clock{t: time.Unix(1000, 0)}
	var ids Shared[int]
	src := New("src", every(10*time.Minute), func(ctx context.Context) ([]byte, error) {
		ids.Publish(ctx, 7)
		return nil, nil
	}, WithClock(clk.now))
	var ok bool
	dst := New("dst", every(time.Minute), func(ctx context.Context) ([]byte, error) {
		_, ok = ids.Get(ctx)
		return []byte("x 1\n"), nil
	}, WithClock(clk.now))
	src.RunOnce(t.Context())
	clk.advance(4 * time.Minute)
	dst.RunOnce(t.Context())
	if !ok {
		t.Error("value refused although it is within 3× the publisher's interval")
	}
	if dst.Body(clk.now()) == nil {
		t.Error("consumer's fresh body treated as stale because of the shared value's age")
	}
}

func TestSharedExpiresWithSourceWindow(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	var ids Shared[int]
	src := New("src", every(time.Minute), func(ctx context.Context) ([]byte, error) {
		ids.Publish(ctx, 7)
		return nil, nil
	}, WithClock(clk.now))
	var ok bool
	dst := New("dst", every(time.Hour), func(ctx context.Context) ([]byte, error) {
		_, ok = ids.Get(ctx)
		return nil, nil
	}, WithClock(clk.now))
	src.RunOnce(t.Context())

	clk.advance(3 * time.Minute) // exactly as old as the source still serves its own data
	if dst.RunOnce(t.Context()); !ok {
		t.Error("value refused within 3× the source's interval")
	}
	clk.advance(time.Second)
	if dst.RunOnce(t.Context()); ok {
		t.Error("value returned after the source would have stopped serving it")
	}
}

func TestSharedOutsidePoll(t *testing.T) {
	var ids Shared[int]
	ids.Publish(t.Context(), 1) // no-op outside a poll
	if _, ok := ids.Get(t.Context()); ok {
		t.Error("Get outside a poll returned a value")
	}
}

func TestWaitForFirstPoll(t *testing.T) {
	release := make(chan struct{})
	var srcDone atomic.Bool
	src := New("src", every(time.Hour), func(ctx context.Context) ([]byte, error) {
		<-release
		srcDone.Store(true)
		return nil, nil
	})
	var sawSrcDone atomic.Bool
	firstPoll := make(chan struct{})
	dst := New("dst", every(time.Hour), func(ctx context.Context) ([]byte, error) {
		sawSrcDone.Store(srcDone.Load())
		close(firstPoll)
		return nil, nil
	}, WaitForFirstPoll(time.Hour, src))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go src.Run(ctx)
	go dst.Run(ctx)
	select {
	case <-firstPoll:
		t.Fatal("consumer polled before the source's first poll finished")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-firstPoll:
	case <-time.After(2 * time.Second):
		t.Fatal("consumer never polled after the source finished")
	}
	if !sawSrcDone.Load() {
		t.Error("consumer's first poll ran before the source's first poll completed")
	}
}

func TestWaitForFirstPollStopsOnCancel(t *testing.T) {
	src := New("src", every(time.Hour), func(ctx context.Context) ([]byte, error) { return nil, nil })
	dst := New("dst", every(time.Hour), func(ctx context.Context) ([]byte, error) {
		t.Error("consumer polled although the source never ran")
		return nil, nil
	}, WaitForFirstPoll(time.Hour, src))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { dst.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel while waiting for the source")
	}
}

func TestWaitForFirstPollGivesUp(t *testing.T) {
	src := New("src", every(time.Hour), func(ctx context.Context) ([]byte, error) {
		<-ctx.Done() // a source whose first poll hangs until its deadline
		return nil, ctx.Err()
	})
	polled := make(chan struct{})
	dst := New("dst", every(time.Hour), func(ctx context.Context) ([]byte, error) {
		close(polled)
		return nil, nil
	}, WaitForFirstPoll(50*time.Millisecond, src))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go src.Run(ctx)
	go dst.Run(ctx)
	select {
	case <-polled:
	case <-time.After(2 * time.Second):
		t.Fatal("consumer kept waiting for a hanging source beyond its limit")
	}
}
