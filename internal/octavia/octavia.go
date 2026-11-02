// Package octavia polls the Load Balancer API: load balancers and their status trees.
package octavia

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/krisiasty/osmex/internal/expo"
	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/poller"
)

// Spec identifies the Load Balancer API.
var Spec = osclient.ServiceSpec{Types: []string{"load-balancer"}, Suffix: "/v2/lbaas"}

// statusWorkers bounds concurrent status tree fetches within one poll.
const statusWorkers = 4

// Unchanged status trees are refreshed as a safety net. Each poll re-fetches the oldest of those fetched at least
// refreshAfter polls ago, at most a refreshPolls-th of the unchanged trees, so that each is refreshed about every
// refreshPolls polls (every refreshAfter with fewer than refreshPolls of them) and the refreshes spread out instead
// of arriving together.
const (
	refreshPolls = 10
	refreshAfter = refreshPolls / 2
)

type loadBalancer struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Provider           string `json:"provider"`
	AZ                 string `json:"availability_zone"`
	OperatingStatus    string `json:"operating_status"`
	ProvisioningStatus string `json:"provisioning_status"`
	UpdatedAt          string `json:"updated_at"`
}

type statusTree struct {
	Listeners []listener `json:"listeners"`
}

type listener struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	OperatingStatus    string `json:"operating_status"`
	ProvisioningStatus string `json:"provisioning_status"`
	Pools              []pool `json:"pools"`
}

// listedListener is a listener as the listener list returns it.
type listedListener struct {
	ID                 string `json:"id"`
	OperatingStatus    string `json:"operating_status"`
	ProvisioningStatus string `json:"provisioning_status"`
	LoadBalancers      []struct {
		ID string `json:"id"`
	} `json:"loadbalancers"`
}

// listenerStates renders a load balancer's listeners and their statuses in a canonical form, so the listeners of a
// fetched tree can be compared with the listener list.
func listenerStates(states []string) string {
	slices.Sort(states)
	return strings.Join(states, ";")
}

func listenerState(id, operating, provisioning string) string {
	return id + " " + operating + " " + provisioning
}

func treeListeners(tree statusTree) string {
	states := make([]string, 0, len(tree.Listeners))
	for _, l := range tree.Listeners {
		states = append(states, listenerState(l.ID, l.OperatingStatus, l.ProvisioningStatus))
	}
	return listenerStates(states)
}

// listListeners returns the listener states of every load balancer, keyed by load balancer id, from one listener
// list. Octavia marks a listener missing from an amphora heartbeat ERROR without changing the load balancer's own
// status or updated_at, so only this list shows such a change without a status call.
func (p *octaviaPoller) listListeners(ctx context.Context) (map[string]string, error) {
	query := url.Values{"fields": {"id", "operating_status", "provisioning_status", "loadbalancers"}}
	listeners, err := osclient.ListAll[listedListener](ctx, p.svc, "/listeners", query, "listeners")
	if err != nil {
		return nil, err
	}
	byLB := map[string][]string{}
	for _, l := range listeners {
		for _, lb := range l.LoadBalancers {
			byLB[lb.ID] = append(byLB[lb.ID], listenerState(l.ID, l.OperatingStatus, l.ProvisioningStatus))
		}
	}
	out := make(map[string]string, len(byLB))
	for id, states := range byLB {
		out[id] = listenerStates(states)
	}
	return out, nil
}

type pool struct {
	ID              string   `json:"id"`
	OperatingStatus string   `json:"operating_status"`
	Members         []member `json:"members"`
}

type member struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	OperatingStatus string `json:"operating_status"`
	Address         string `json:"address"`
	ProtocolPort    int    `json:"protocol_port"`
}

type octaviaPoller struct {
	svc        *osclient.ServiceClient
	fixed      time.Duration
	thresholds [2]int
	interval   atomic.Int64
	now        func() time.Time
	trees      map[string]fetchedTree // last good status tree per load balancer
	tried      map[string]time.Time   // list time of the poll that last requested each status, successfully or not
}

type fetchedTree struct {
	tree      statusTree
	listed    listState // the load balancer's state in the list call of the poll that fetched the tree
	listeners string    // the tree's listeners and their statuses, in listenerStates form
	settled   bool      // every listener, pool and member is in a state that changes only together with the list entries
	at        time.Time // when that list call was made; the safety refresh counts from here
	seen      time.Time // when the tree was last known current: at, or a later list call that confirmed it
}

// listState is the part of a listed load balancer that change detection compares.
type listState struct {
	operating, provisioning, updatedAt string
}

func stateOf(lb loadBalancer) listState {
	return listState{operating: lb.OperatingStatus, provisioning: lb.ProvisioningStatus, updatedAt: lb.UpdatedAt}
}

// amphoraProviders name the amphora driver: amphorav2 and the octavia alias remain on load balancers created by
// older releases.
var amphoraProviders = []string{"amphora", "amphorav2", "octavia"}

// confirmedBy reports whether lb, as listed now, shows that the tree is still current. With the amphora provider,
// Octavia bumps updated_at on every configuration change and on every change of the load balancer's own operating
// status, but a member failing or recovering on a load balancer that is already DEGRADED changes nothing in the list.
// So only an ACTIVE + ONLINE amphora load balancer, unchanged since the fetch, confirms its tree, and only a settled
// one whose listeners, as listed now, still match the tree. Other providers are not known to roll member changes up
// the same way, and without updated_at configuration changes can't be seen, so their trees are fetched on every poll.
// listeners is the listener list (listListeners), or nil if it failed, which confirms nothing.
func (ft fetchedTree) confirmedBy(lb loadBalancer, listeners map[string]string) bool {
	return ft.settled && listeners != nil && listeners[lb.ID] == ft.listeners && slices.Contains(amphoraProviders, lb.Provider) &&
		lb.UpdatedAt != "" && lb.ProvisioningStatus == "ACTIVE" && lb.OperatingStatus == "ONLINE" && stateOf(lb) == ft.listed
}

// settled reports whether every listener, pool and member in the tree is in a state that, under an ONLINE load
// balancer, changes only through an API call or a health failure, both of which change the load balancer's list entry.
// A new listener or member is OFFLINE until the health manager's first report and then turns ONLINE without any such
// change; an OFFLINE (disabled), ERROR or DEGRADED listener, an OFFLINE pool, or an OFFLINE or ERROR member is fetched
// again on every poll for the same reason.
func settled(tree statusTree) bool {
	for _, l := range tree.Listeners {
		if !healthy(l.OperatingStatus) {
			return false
		}
		for _, pl := range l.Pools {
			if pl.OperatingStatus != "ONLINE" && pl.OperatingStatus != "NO_MONITOR" {
				return false
			}
			for _, m := range pl.Members {
				if !slices.Contains([]string{"ONLINE", "NO_MONITOR", "DRAINING"}, m.OperatingStatus) {
					return false
				}
			}
		}
	}
	return true
}

func newOctaviaPoller(svc *osclient.ServiceClient, fixed time.Duration, thresholds [2]int) *octaviaPoller {
	op := &octaviaPoller{svc: svc, fixed: fixed, thresholds: thresholds, now: time.Now, trees: map[string]fetchedTree{}, tried: map[string]time.Time{}}
	op.interval.Store(int64(time.Minute))
	if fixed > 0 {
		op.interval.Store(int64(fixed))
	}
	return op
}

// New returns the octavia poller. With fixed 0 the interval follows the load balancer count.
func New(svc *osclient.ServiceClient, fixed time.Duration, thresholds [2]int, opts ...poller.Option) *poller.Poller {
	op := newOctaviaPoller(svc, fixed, thresholds)
	return poller.New("octavia", func() time.Duration { return time.Duration(op.interval.Load()) }, op.poll, opts...)
}

func autoInterval(lbCount int, thresholds [2]int) time.Duration {
	switch {
	case lbCount <= thresholds[0]:
		return time.Minute
	case lbCount <= thresholds[1]:
		return 2 * time.Minute
	}
	return 5 * time.Minute
}

func (p *octaviaPoller) poll(ctx context.Context) ([]byte, error) {
	lbs, err := osclient.ListAll[loadBalancer](ctx, p.svc, "/loadbalancers", nil, "loadbalancers")
	if err != nil {
		return nil, err
	}
	listedAt := p.now()
	// Without the listener list no tree can be confirmed, so every load balancer's status is fetched as if osmex had no cache.
	listeners, err := p.listListeners(ctx)
	if err != nil {
		poller.ReportPartialFailure(ctx, err)
	}
	if p.fixed == 0 {
		p.interval.Store(int64(autoInterval(len(lbs), p.thresholds)))
	}
	interval := time.Duration(p.interval.Load())

	listed := make(map[string]bool, len(lbs))
	for _, lb := range lbs {
		listed[lb.ID] = true
	}
	for id := range p.trees {
		if !listed[id] {
			delete(p.trees, id)
		}
	}
	for id := range p.tried {
		if !listed[id] {
			delete(p.tried, id)
		}
	}

	// Status calls stop being started once half the remaining poll time has passed, like Nova's instance actions.
	// Load balancers left out keep their previous tree: current if the list confirms it, otherwise reused for at most
	// three intervals (below).
	var cutoff time.Time
	if deadline, ok := ctx.Deadline(); ok {
		now := time.Now()
		cutoff = now.Add(deadline.Sub(now) / 2)
	}
	pastCutoff := func() bool { return !cutoff.IsZero() && time.Now().After(cutoff) }

	queue := make(chan loadBalancer)
	var mu sync.Mutex
	fresh := map[string]bool{} // fetched in this poll
	var wg sync.WaitGroup
	for range statusWorkers {
		wg.Go(func() {
			for lb := range queue {
				if pastCutoff() {
					continue
				}
				mu.Lock()
				p.tried[lb.ID] = listedAt
				mu.Unlock()
				var out struct {
					Statuses struct {
						LoadBalancer *statusTree `json:"loadbalancer"`
					} `json:"statuses"`
				}
				err := p.svc.Get(ctx, "/loadbalancers/"+lb.ID+"/status", nil, &out)
				if err == nil && out.Statuses.LoadBalancer == nil {
					// Without this, a body of {} would replace the tree with an empty one that passes as settled.
					err = &osclient.DecodeError{Err: errors.New("response has no load balancer status")}
					osclient.RecordErrorCode(ctx, osclient.ErrorCode(err))
				}
				if err != nil {
					poller.ReportPartialFailure(ctx, err) // keep the previous tree
					continue
				}
				mu.Lock()
				tree := *out.Statuses.LoadBalancer
				p.trees[lb.ID] = fetchedTree{tree: tree, listed: stateOf(lb), listeners: treeListeners(tree), settled: settled(tree), at: listedAt, seen: listedAt}
				fresh[lb.ID] = true
				mu.Unlock()
			}
		})
	}
	mu.Lock()
	order := statusOrder(lbs, p.trees, p.tried, listeners, listedAt, interval)
	mu.Unlock()
dispatch:
	for _, lb := range order {
		if pastCutoff() {
			break
		}
		select {
		case queue <- lb:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(queue)
	wg.Wait()

	// A tree is current if this poll fetched it or this poll's list confirms it. Any other tree is reused data: it
	// dates the snapshot from when it was last known current, and is dropped three intervals after that. Between polls
	// the poller then serves the fallback, which has only the current trees, so the rest of the snapshot stays.
	now, maxAge := p.now(), 3*interval
	served := make(map[string]statusTree, len(p.trees))
	current := make(map[string]statusTree, len(p.trees))
	for _, lb := range lbs {
		ft, ok := p.trees[lb.ID]
		switch {
		case !ok:
			continue
		case fresh[lb.ID]:
		case ft.confirmedBy(lb, listeners):
			ft.seen = listedAt
			p.trees[lb.ID] = ft
		case now.Sub(ft.seen) > maxAge:
			delete(p.trees, lb.ID)
			continue
		default:
			poller.ReportDataTime(ctx, ft.seen)
			served[lb.ID] = ft.tree
			continue
		}
		served[lb.ID] = ft.tree
		current[lb.ID] = ft.tree
	}
	if len(current) < len(served) {
		poller.ReportFallback(ctx, render(lbs, current))
	}
	return render(lbs, served), nil
}

func healthy(status string) bool { return status == "ONLINE" || status == "NO_MONITOR" }

// listenerServing reports whether a listener accepts connections. The amphora driver reports a listener DEGRADED when
// HAProxy has reached its connection limit (FULL): it still serves, so saturation shows in the operating status only.
func listenerServing(status string) bool { return healthy(status) || status == "DEGRADED" }

func render(lbs []loadBalancer, trees map[string]statusTree) []byte {
	var page expo.Page
	info := page.Gauge("openstack_lb_info", "Load balancer information; the value is always 1.")
	up := page.Gauge("openstack_lb_up", "1 if the load balancer operating status is ONLINE or NO_MONITOR, else 0.")
	opStatus := page.Gauge("openstack_lb_operating_status", "Current load balancer operating status; the value is always 1.")
	provStatus := page.Gauge("openstack_lb_provisioning_status", "Current load balancer provisioning status; the value is always 1.")
	listenerInfo := page.Gauge("openstack_lb_listener_info", "Listener information; the value is always 1.")
	listenerUp := page.Gauge("openstack_lb_listener_up",
		"1 if the listener operating status is ONLINE, NO_MONITOR or DEGRADED (connection limit reached), else 0.")
	listenerStatus := page.Gauge("openstack_lb_listener_operating_status", "Current listener operating status; the value is always 1.")
	listenerProvStatus := page.Gauge("openstack_lb_listener_provisioning_status", "Current listener provisioning status; the value is always 1.")
	poolUp := page.Gauge("openstack_lb_pool_up", "1 if the pool operating status is ONLINE or NO_MONITOR, else 0.")
	poolStatus := page.Gauge("openstack_lb_pool_operating_status", "Current pool operating status; the value is always 1.")
	memberInfo := page.Gauge("openstack_lb_member_info", "Pool member information; the value is always 1.")
	memberUp := page.Gauge("openstack_lb_member_up", "1 if the member operating status is ONLINE or NO_MONITOR, else 0.")
	memberStatus := page.Gauge("openstack_lb_member_operating_status", "Current pool member operating status; the value is always 1.")

	for _, lb := range lbs {
		info.Add(1, "id", lb.ID, "name", lb.Name, "provider", lb.Provider, "availability_zone", lb.AZ)
		up.Add(expo.Bool(healthy(lb.OperatingStatus)), "id", lb.ID)
		opStatus.Add(1, "id", lb.ID, "operating_status", lb.OperatingStatus)
		provStatus.Add(1, "id", lb.ID, "provisioning_status", lb.ProvisioningStatus)

		tree, ok := trees[lb.ID]
		if !ok {
			continue
		}
		seen := map[string]bool{} // a pool shared by several listeners appears once per listener
		for _, l := range tree.Listeners {
			listenerInfo.Add(1, "lb_id", lb.ID, "listener_id", l.ID, "name", l.Name)
			listenerUp.Add(expo.Bool(listenerServing(l.OperatingStatus)), "lb_id", lb.ID, "listener_id", l.ID)
			listenerStatus.Add(1, "lb_id", lb.ID, "listener_id", l.ID, "operating_status", l.OperatingStatus)
			listenerProvStatus.Add(1, "lb_id", lb.ID, "listener_id", l.ID, "provisioning_status", l.ProvisioningStatus)
			for _, pl := range l.Pools {
				if seen[pl.ID] {
					continue
				}
				seen[pl.ID] = true
				poolUp.Add(expo.Bool(healthy(pl.OperatingStatus)), "lb_id", lb.ID, "pool_id", pl.ID)
				poolStatus.Add(1, "lb_id", lb.ID, "pool_id", pl.ID, "operating_status", pl.OperatingStatus)
				for _, m := range pl.Members {
					memberInfo.Add(1, "member_id", m.ID, "pool_id", pl.ID, "name", m.Name, "address", m.Address, "port", strconv.Itoa(m.ProtocolPort))
					memberUp.Add(expo.Bool(healthy(m.OperatingStatus)), "member_id", m.ID, "pool_id", pl.ID)
					memberStatus.Add(1, "member_id", m.ID, "pool_id", pl.ID, "operating_status", m.OperatingStatus)
				}
			}
		}
	}
	return page.Bytes()
}

// statusOrder returns the load balancers whose status trees this poll should fetch, in order: those without a tree,
// the trees the list does not confirm and the safety refreshes that are due, least recently requested first, ties
// oldest tree first, then in listing order. Those never requested therefore come first, and a poll that is cut off
// catches up on what it skipped the next time. A request that fails moves its load balancer behind the others, so
// slow failing calls can't starve the rest, and many unconfirmed load balancers on a slow API can't keep the
// refreshes from ever running. Refreshes follow the refreshPolls and refreshAfter constants.
func statusOrder(lbs []loadBalancer, trees map[string]fetchedTree, tried map[string]time.Time, listeners map[string]string, now time.Time,
	interval time.Duration) []loadBalancer {
	var missing, rotation, due []loadBalancer
	confirmed := 0
	for _, lb := range lbs {
		ft, ok := trees[lb.ID]
		switch {
		case !ok:
			missing = append(missing, lb)
		case !ft.confirmedBy(lb, listeners):
			rotation = append(rotation, lb)
		default:
			confirmed++
			// Half an interval of slack: jitter makes refreshAfter polls take between 0.9 and 1.1 times as many
			// intervals, so with refreshAfter = 5 they take at least 4.5.
			if now.Sub(ft.at) >= refreshAfter*interval-interval/2 {
				due = append(due, lb)
			}
		}
	}
	order := func(a, b loadBalancer) int {
		if c := tried[a.ID].Compare(tried[b.ID]); c != 0 {
			return c
		}
		return trees[a.ID].at.Compare(trees[b.ID].at)
	}
	slices.SortStableFunc(due, order)
	budget := (confirmed + refreshPolls - 1) / refreshPolls
	queue := slices.Concat(missing, rotation, due[:min(len(due), budget)])
	slices.SortStableFunc(queue, order)
	return queue
}
