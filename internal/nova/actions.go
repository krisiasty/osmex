package nova

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/poller"
)

// actionWorkers bounds concurrent instance action fetches within one poll; the client's shared
// semaphore bounds them further across pollers.
const actionWorkers = 4

type instanceAction struct {
	Action string `json:"action"`
}

// changed reports whether s is new or its update markers differ from the previous cycle.
func changed(prev *serverState, s server) bool {
	return prev == nil || prev.updated != s.Updated || prev.launchedAt != s.LaunchedAt || prev.hostID != s.HostID
}

// fetchActions fetches the full action history of each server in ids, in order. It stops starting
// new fetches once half of the time until ctx's deadline has passed; servers not fetched stay pending.
// Each fetch started, successful or not, records the cycle in the server's tried.
func (p *serversPoller) fetchActions(ctx context.Context, ids []string) {
	cutoff := time.Time{}
	if deadline, ok := ctx.Deadline(); ok {
		now := time.Now()
		cutoff = now.Add(deadline.Sub(now) / 2)
	}
	pastCutoff := func() bool { return !cutoff.IsZero() && time.Now().After(cutoff) }
	queue := make(chan string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range actionWorkers {
		wg.Go(func() {
			for id := range queue {
				if pastCutoff() { // the send may have waited for a worker past the cutoff
					continue
				}
				mu.Lock()
				p.state[id].tried = p.cycle
				mu.Unlock()
				actions, err := osclient.ListAll[instanceAction](ctx, p.svc, "/servers/"+id+"/os-instance-actions", nil, "instanceActions")
				if err != nil {
					poller.ReportPartialFailure(ctx, err)
					continue
				}
				fetched := countActions(actions)
				mu.Lock()
				st := p.state[id]
				if st.counts == nil {
					st.counts = map[string]float64{}
				}
				for a, n := range fetched {
					st.counts[a] = max(st.counts[a], n)
				}
				st.pending = false
				mu.Unlock()
			}
		})
	}
	for _, id := range ids {
		if pastCutoff() {
			break
		}
		select {
		case queue <- id:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(queue)
	wg.Wait()
}

func countActions(actions []instanceAction) map[string]float64 {
	counts := map[string]float64{}
	for _, a := range actions {
		counts[a.Action]++
	}
	return counts
}

// pendingIDs returns the ids of servers whose actions must be fetched, least recently tried first and
// then by id. Servers skipped at the cutoff therefore come before those tried in the last cycle, and a
// server whose fetch fails moves behind the others instead of starving them.
func pendingIDs(state map[string]*serverState) []string {
	var ids []string
	for id, st := range state {
		if st.pending {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b string) int {
		return cmp.Or(cmp.Compare(state[a].tried, state[b].tried), strings.Compare(a, b))
	})
	return ids
}
