package poller

import (
	"context"
	"sync/atomic"
	"time"
)

// Shared is a value one poller publishes for other pollers to read, such as the list of volume ids that zero
// summaries in another poller need. It is dated by the publishing poll's start time and readable only as long as
// the publishing poller would still serve its own data of that age. Reading it does not age the reader's own data:
// a shared value should only shape the reader's output (which rows exist), never stand in for data it fetches.
type Shared[T any] struct {
	v atomic.Pointer[shared[T]]
}

type shared[T any] struct {
	val    T
	at     time.Time     // start of the poll that fetched it
	maxAge time.Duration // how long the publishing poller serves its data
}

// Publish stores v as fetched by the current poll. Call it from the publishing poller's Func; outside a poll it is a
// no-op.
func (s *Shared[T]) Publish(ctx context.Context, v T) {
	st := pollFrom(ctx)
	if st == nil {
		return
	}
	s.v.Store(&shared[T]{val: v, at: st.at, maxAge: staleAfter * st.interval})
}

// Get returns the published value if it is still within the publishing poller's staleness limit. Outside a poll,
// or when nothing usable is published, it returns false.
func (s *Shared[T]) Get(ctx context.Context) (T, bool) {
	var zero T
	st := pollFrom(ctx)
	v := s.v.Load()
	if st == nil || v == nil || st.now().Sub(v.at) > v.maxAge {
		return zero, false
	}
	return v.val, true
}
