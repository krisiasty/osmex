package octavia

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/ostest"
	"github.com/krisiasty/osmex/internal/poller"
)

var thresholds = [2]int{20, 100}

type fakeOctavia struct {
	cloud         *ostest.Cloud
	lbs           atomic.Value // []json.RawMessage
	statuses      map[string]json.RawMessage
	failStatus    atomic.Bool
	failListeners atomic.Bool
	statusDelay   atomic.Int64 // time.Duration

	mu       sync.Mutex
	calls    []string // load balancer ids whose status was requested, in order
	onStatus func()   // run on every status call, if set
}

// fetched returns the ids whose status was requested since the last call.
func (f *fakeOctavia) fetched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

func newFakeOctavia(t *testing.T) *fakeOctavia {
	b, err := os.ReadFile("testdata/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		LoadBalancers []json.RawMessage          `json:"loadbalancers"`
		Statuses      map[string]json.RawMessage `json:"statuses"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	f := &fakeOctavia{cloud: ostest.New(t, "load-balancer"), statuses: fx.Statuses}
	f.lbs.Store(fx.LoadBalancers)
	f.cloud.Handle("GET /load-balancer/v2/lbaas/loadbalancers", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"loadbalancers": f.lbs.Load()})
	})
	// The listener list reflects the listeners in the status trees, as both read the same database.
	f.cloud.Handle("GET /load-balancer/v2/lbaas/listeners", func(w http.ResponseWriter, r *http.Request) {
		if f.failListeners.Load() {
			http.Error(w, "boom", 500)
			return
		}
		listeners := []map[string]any{}
		for id, raw := range f.statuses {
			var st struct {
				Statuses struct {
					LoadBalancer struct {
						Listeners []map[string]any `json:"listeners"`
					} `json:"loadbalancer"`
				} `json:"statuses"`
			}
			if err := json.Unmarshal(raw, &st); err != nil {
				t.Error(err)
			}
			for _, l := range st.Statuses.LoadBalancer.Listeners {
				delete(l, "pools")
				l["loadbalancers"] = []map[string]string{{"id": id}}
				listeners = append(listeners, l)
			}
		}
		ostest.WriteJSON(w, 200, map[string]any{"listeners": listeners})
	})
	f.cloud.Handle("GET /load-balancer/v2/lbaas/loadbalancers/{id}/status", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		f.calls = append(f.calls, id)
		hook := f.onStatus
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		time.Sleep(time.Duration(f.statusDelay.Load()))
		st, ok := f.statuses[id]
		if f.failStatus.Load() || !ok {
			http.Error(w, "boom", 500)
			return
		}
		_, _ = w.Write(st)
	})
	return f
}

func TestAutoInterval(t *testing.T) {
	for n, want := range map[int]time.Duration{0: time.Minute, 20: time.Minute, 21: 2 * time.Minute, 100: 2 * time.Minute, 101: 5 * time.Minute} {
		if got := autoInterval(n, thresholds); got != want {
			t.Errorf("autoInterval(%d) = %v, want %v", n, got, want)
		}
	}
}

func TestRenderGolden(t *testing.T) {
	f := newFakeOctavia(t)
	p := New(f.cloud.Client(t).Service(Spec), 0, thresholds)
	p.RunOnce(t.Context())
	if p.Name() != "octavia" {
		t.Errorf("name %q", p.Name())
	}
	ostest.Golden(t, "testdata/octavia.golden", p.Body(time.Now()))
}

func TestStatusFailureReusesPrevious(t *testing.T) {
	f := newFakeOctavia(t)
	op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds)
	first, err := op.poll(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.failStatus.Store(true)
	second, err := op.poll(t.Context())
	if err != nil {
		t.Fatalf("status failures failed the poll: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("status trees not reused:\n%s", second)
	}
}

func TestStatusFailureNoPrevious(t *testing.T) {
	f := newFakeOctavia(t)
	f.failStatus.Store(true)
	body, err := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds).poll(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "openstack_lb_pool") || !strings.Contains(string(body), `openstack_lb_up{id="lb1"} 1`) {
		t.Errorf("body:\n%s", body)
	}
}

func TestIntervalAdapts(t *testing.T) {
	f := newFakeOctavia(t)
	var lbs []json.RawMessage
	for i := range 25 {
		lbs = append(lbs, json.RawMessage(fmt.Sprintf(`{"id":"x%d","operating_status":"ONLINE","provisioning_status":"ACTIVE"}`, i)))
	}
	f.lbs.Store(lbs)
	p := New(f.cloud.Client(t).Service(Spec), 0, thresholds)
	if p.Interval() != time.Minute {
		t.Errorf("initial interval %v", p.Interval())
	}
	p.RunOnce(t.Context())
	if p.Interval() != 2*time.Minute {
		t.Errorf("interval after 25 LBs = %v", p.Interval())
	}
}

func TestFixedInterval(t *testing.T) {
	f := newFakeOctavia(t)
	p := New(f.cloud.Client(t).Service(Spec), 90*time.Second, thresholds)
	p.RunOnce(t.Context())
	if p.Interval() != 90*time.Second {
		t.Errorf("interval %v", p.Interval())
	}
}

func TestEmptyProject(t *testing.T) {
	f := newFakeOctavia(t)
	f.lbs.Store([]json.RawMessage{})
	p := New(f.cloud.Client(t).Service(Spec), 0, thresholds)
	p.RunOnce(t.Context())
	if st := p.Status(time.Now()); !st.Up || len(p.Body(time.Now())) != 0 {
		t.Errorf("status %+v body %q", st, p.Body(time.Now()))
	}
}

func TestStatusReuseExpires(t *testing.T) {
	f := newFakeOctavia(t)
	f.settle(t, "lb1")
	now := time.Now()
	op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds)
	op.now = func() time.Time { return now }
	if _, err := op.poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.failStatus.Store(true)
	now = now.Add(3*time.Minute + time.Second) // interval is 60s with 3 LBs
	body, err := op.poll(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// lb1 is unchanged and ONLINE, so the list confirms its tree; lb2's needs fetching and is too old to reuse.
	if strings.Contains(string(body), `lb_id="lb2"`) || !strings.Contains(string(body), `lb_id="lb1"`) {
		t.Errorf("want lb1's confirmed tree and not lb2's expired one:\n%s", body)
	}
}

func TestStatusSeries(t *testing.T) {
	f := newFakeOctavia(t)
	body, err := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds).poll(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`openstack_lb_operating_status{id="lb2",operating_status="DEGRADED"} 1`,
		`openstack_lb_provisioning_status{id="lb2",provisioning_status="PENDING_UPDATE"} 1`,
		`openstack_lb_provisioning_status{id="lb3",provisioning_status="ERROR"} 1`,
		`openstack_lb_pool_operating_status{lb_id="lb2",pool_id="pl2",operating_status="DEGRADED"} 1`,
		`openstack_lb_member_operating_status{member_id="m2",pool_id="pl1",operating_status="ERROR"} 1`,
		`openstack_lb_member_operating_status{member_id="m3",pool_id="pl2",operating_status="NO_MONITOR"} 1`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestReusedTreesReportDataAge(t *testing.T) {
	f := newFakeOctavia(t)
	now := time.Unix(1_800_000_000, 0)
	clk := func() time.Time { return now }
	op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds)
	op.now = clk
	p := poller.New("octavia", func() time.Duration { return time.Minute }, op.poll, poller.WithClock(clk))
	p.RunOnce(t.Context())

	now = now.Add(2 * time.Minute)
	f.failStatus.Store(true)
	p.RunOnce(t.Context())
	if st := p.Status(now); st.DataAge != 2*time.Minute {
		t.Errorf("data age = %v, want 2m: the status trees were fetched in the first poll", st.DataAge)
	}
}

// generate replaces the listed load balancers with n generated ones, gen-0 … gen-(n-1), each with a one-pool status
// tree. Call it before polling: the handlers read the statuses map without locking.
func (f *fakeOctavia) generate(n int) {
	lbs := make([]json.RawMessage, n)
	for i := range n {
		id := fmt.Sprintf("gen-%d", i)
		lbs[i] = json.RawMessage(fmt.Sprintf(
			`{"id":%q,"provider":"amphora","operating_status":"ONLINE","provisioning_status":"ACTIVE","updated_at":"2026-10-31T21:07:03"}`, id))
		f.statuses[id] = json.RawMessage(fmt.Sprintf(
			`{"statuses":{"loadbalancer":{"id":%q,"listeners":[{"id":"lsn-%s","operating_status":"ONLINE","pools":[{"id":"pool-%s","operating_status":"ONLINE","members":[]}]}]}}}`, id, id, id))
	}
	f.lbs.Store(lbs)
}

func TestStatusOrder(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	online := loadBalancer{Provider: "amphora", OperatingStatus: "ONLINE", ProvisioningStatus: "ACTIVE", UpdatedAt: "t1"}
	lb := func(id string) loadBalancer { l := online; l.ID = id; return l }
	lbs := []loadBalancer{lb("a"), lb("b"), lb("c"), lb("d"), lb("e"), lb("f")}
	lbs[1].OperatingStatus = "DEGRADED"
	lbs[2].UpdatedAt = "t2"
	trees := map[string]fetchedTree{
		"b": {listed: stateOf(lbs[1]), settled: true, at: now.Add(-1 * time.Minute)},
		"c": {listed: stateOf(lb("c")), settled: true, at: now.Add(-2 * time.Minute)},
		"e": {listed: stateOf(lb("e")), settled: true, at: now.Add(-refreshPolls * time.Minute)},
		"f": {listed: stateOf(lb("f")), settled: true, at: now.Add(-time.Minute)},
	}
	tried := map[string]time.Time{}
	for id, ft := range trees {
		tried[id] = ft.at
	}
	order := func() []string {
		var got []string
		for _, l := range statusOrder(lbs, trees, tried, map[string]string{}, now, time.Minute) {
			got = append(got, l.ID)
		}
		return got
	}
	// Never requested first, in listing order (a, d). Then one rotation, least recently requested first, of the due
	// safety refresh (e) and the unconfirmed trees (c changed, b degraded): a slow API that never gets past the
	// unconfirmed load balancers must not starve the refreshes. f is confirmed and recent.
	if got := order(); !slices.Equal(got, []string{"a", "d", "e", "c", "b"}) {
		t.Errorf("statusOrder = %v, want [a d e c b]", got)
	}
	// a's request failed a minute ago and e's refresh 90 seconds ago: they join the rotation by when they were
	// requested, a before b on the older tree (none), instead of keeping their place at the front.
	tried["a"], tried["e"] = now.Add(-time.Minute), now.Add(-90*time.Second)
	if got := order(); !slices.Equal(got, []string{"d", "c", "e", "a", "b"}) {
		t.Errorf("after failures, statusOrder = %v, want [d c e a b]", got)
	}
}

func slowOctavia(t *testing.T, n int) (*fakeOctavia, *octaviaPoller) {
	f := newFakeOctavia(t)
	f.generate(n)
	f.statusDelay.Store(int64(150 * time.Millisecond))
	client := osclient.NewClient(osclient.ClientConfig{
		Auth: f.cloud.Auth(t), HTTP: f.cloud.Server.Client(), Region: "RegionOne", Interface: "public",
		MaxConcurrent: 1, RequestTimeout: 5 * time.Second,
	})
	return f, newOctaviaPoller(client.Service(Spec), time.Minute, thresholds)
}

func TestStatusCutoff(t *testing.T) {
	f, op := slowOctavia(t, 10)
	ctx, cancel := context.WithTimeout(t.Context(), 600*time.Millisecond)
	defer cancel()
	rec := &countingRecorder{}
	body, err := op.poll(osclient.WithRecorder(ctx, rec))
	if err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	// Without a cutoff, every status call still queued at the deadline fails and counts as a timeout. With it, only
	// calls already handed to a worker can run into the deadline.
	if n := rec.count("timeout"); n > statusWorkers {
		t.Errorf("%d status calls ran into the poll deadline, want at most %d", n, statusWorkers)
	}
	fetched := f.fetched()
	if len(fetched) >= 10 {
		t.Errorf("all %d status calls were made despite the deadline", len(fetched))
	}
	// A load balancer whose status was never requested has no pool series yet.
	for i := range 10 {
		id := fmt.Sprintf("gen-%d", i)
		if !slices.Contains(fetched, id) && strings.Contains(string(body), `pool_id="pool-`+id+`"`) {
			t.Errorf("%s has pool series although its status was never fetched", id)
		}
	}
}

func TestStatusCutoffCatchesUp(t *testing.T) {
	f, op := slowOctavia(t, 10)
	poll := func() []string {
		ctx, cancel := context.WithTimeout(t.Context(), 600*time.Millisecond)
		defer cancel()
		if _, err := op.poll(ctx); err != nil {
			t.Fatal(err)
		}
		return f.fetched()
	}
	poll()
	// Skipped means no status tree after the first poll: never requested, or requested but cut off by the deadline.
	var skipped []string
	for i := range 10 {
		if id := fmt.Sprintf("gen-%d", i); op.trees[id].at.IsZero() {
			skipped = append(skipped, id)
		}
	}
	if len(skipped) == 0 {
		t.Fatal("first poll was not cut off; the test needs a slower status call")
	}
	second := poll()
	// The skipped load balancers are queued first, so the second poll starts with them.
	for _, id := range second[:min(len(second), len(skipped))] {
		if !slices.Contains(skipped, id) {
			t.Errorf("second poll fetched %s before the load balancers without a tree (%v); order %v", id, skipped, second)
		}
	}
}

type countingRecorder struct {
	mu    sync.Mutex
	codes []string
}

func (r *countingRecorder) RecordError(code string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.codes = append(r.codes, code)
}

func (r *countingRecorder) count(code string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.codes {
		if c == code {
			n++
		}
	}
	return n
}

func TestStatusFailureLogged(t *testing.T) {
	f := newFakeOctavia(t)
	f.failStatus.Store(true)
	var logs strings.Builder
	op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds)
	p := poller.New("octavia", func() time.Duration { return time.Minute }, op.poll,
		poller.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	p.RunOnce(t.Context())
	out := logs.String()
	if !strings.Contains(out, `msg="poll partly failed"`) || !strings.Contains(out, "failed_calls=3") ||
		!strings.Contains(out, "/status: HTTP 500") {
		t.Errorf("failed status calls not logged:\n%s", out)
	}
}

// setLB changes fields of one listed load balancer, as a later list call would return it.
func (f *fakeOctavia) setLB(t *testing.T, id string, fields map[string]any) {
	t.Helper()
	var out []json.RawMessage
	for _, raw := range f.lbs.Load().([]json.RawMessage) {
		var lb map[string]any
		if err := json.Unmarshal(raw, &lb); err != nil {
			t.Fatal(err)
		}
		if lb["id"] == id {
			maps.Copy(lb, fields)
		}
		b, err := json.Marshal(lb)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	f.lbs.Store(out)
}

func sorted(ids []string) []string { slices.Sort(ids); return ids }

func TestUnchangedOnlineTreeReused(t *testing.T) {
	f := newFakeOctavia(t)
	f.settle(t, "lb1")
	now := time.Unix(1_800_000_000, 0)
	clk := func() time.Time { return now }
	op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds)
	op.now = clk
	p := poller.New("octavia", func() time.Duration { return time.Minute }, op.poll, poller.WithClock(clk))
	p.RunOnce(t.Context())
	f.fetched()

	now = now.Add(2 * time.Minute)
	p.RunOnce(t.Context())
	// lb1 is ACTIVE + ONLINE and unchanged. lb2 (DEGRADED, PENDING_UPDATE) and lb3 (ERROR) are fetched every poll:
	// member changes on a load balancer that is not ONLINE don't show in the list.
	if got := sorted(f.fetched()); !slices.Equal(got, []string{"lb2", "lb3"}) {
		t.Errorf("second poll fetched %v, want [lb2 lb3]", got)
	}
	if body := string(p.Body(now)); !strings.Contains(body, `openstack_lb_member_up{member_id="m1",pool_id="pl1"} 1`) {
		t.Errorf("reused tree of lb1 not rendered:\n%s", body)
	}
	if st := p.Status(now); st.DataAge != 0 {
		t.Errorf("data age = %v, want 0: the list call confirms lb1's tree", st.DataAge)
	}
}

func TestChangedTreeRefetched(t *testing.T) {
	for name, change := range map[string]map[string]any{
		"updated_at":          {"updated_at": "2026-10-31T21:08:49"},
		"operating_status":    {"operating_status": "DEGRADED"},
		"provisioning_status": {"provisioning_status": "PENDING_UPDATE"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeOctavia(t)
			f.settle(t, "lb1")
			op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds)
			if _, err := op.poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.fetched()
			f.setLB(t, "lb1", change)
			if _, err := op.poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := f.fetched(); !slices.Contains(got, "lb1") {
				t.Errorf("lb1 not re-fetched after its %s changed; fetched %v", name, got)
			}
		})
	}
}

func TestSafetyRefreshSpread(t *testing.T) {
	f := newFakeOctavia(t)
	f.generate(20)
	now := time.Unix(1_800_000_000, 0)
	op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), time.Minute, thresholds)
	op.now = func() time.Time { return now }
	poll := func() []string {
		if _, err := op.poll(t.Context()); err != nil {
			t.Fatal(err)
		}
		return f.fetched()
	}
	if n := len(poll()); n != 20 {
		t.Fatalf("first poll fetched %d trees, want 20", n)
	}
	for range refreshAfter - 1 {
		now = now.Add(time.Minute)
		if got := poll(); len(got) != 0 {
			t.Fatalf("unchanged trees re-fetched before %d polls: %v", refreshAfter, got)
		}
	}
	// From then on, a tenth per poll, oldest first: every tree once within refreshAfter+refreshPolls polls of
	// startup, then each again refreshPolls polls after its last refresh.
	var order []string
	seen := map[string]bool{}
	for range refreshPolls {
		now = now.Add(time.Minute)
		got := sorted(poll())
		if len(got) != 2 {
			t.Fatalf("refreshed %d trees in one poll, want 2: %v", len(got), got)
		}
		for _, id := range got {
			if seen[id] {
				t.Errorf("%s refreshed twice before every tree was refreshed once", id)
			}
			seen[id] = true
		}
		order = append(order, got...)
	}
	now = now.Add(time.Minute)
	if got := sorted(poll()); !slices.Equal(got, order[:2]) {
		t.Errorf("next refresh %v, want the two refreshed longest ago %v", got, order[:2])
	}
}

func TestReusedTreeDatedByLastConfirmation(t *testing.T) {
	f := newFakeOctavia(t)
	f.settle(t, "lb1")
	now := time.Unix(1_800_000_000, 0)
	clk := func() time.Time { return now }
	op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds)
	op.now = clk
	p := poller.New("octavia", func() time.Duration { return time.Minute }, op.poll, poller.WithClock(clk))
	p.RunOnce(t.Context())
	now = now.Add(2 * time.Minute)
	p.RunOnce(t.Context()) // confirms lb1's tree, re-fetches lb2 and lb3

	now = now.Add(time.Minute)
	f.setLB(t, "lb1", map[string]any{"updated_at": "2026-10-31T21:08:49"})
	f.failStatus.Store(true)
	p.RunOnce(t.Context())
	// Every tree is reused now. lb1's was fetched three minutes ago but confirmed current one minute ago.
	if st := p.Status(now); st.DataAge != time.Minute {
		t.Errorf("data age = %v, want 1m", st.DataAge)
	}
}

func TestConfirmationNeedsAmphoraAndUpdatedAt(t *testing.T) {
	// Only the amphora health manager is known to bump the load balancer's updated_at when a member changes, and a
	// list without updated_at can't show configuration changes at all.
	for name, change := range map[string]map[string]any{
		"other provider":     {"provider": "ovn"},
		"no updated_at":      {"updated_at": nil},
		"updated_at omitted": {"updated_at": ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeOctavia(t)
			f.settle(t, "lb1")
			f.setLB(t, "lb1", change)
			op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds)
			for range 2 {
				if _, err := op.poll(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			got := f.fetched()
			if n := len(slices.DeleteFunc(slices.Clone(got), func(id string) bool { return id != "lb1" })); n != 2 {
				t.Errorf("lb1 fetched in %d of 2 polls; fetched %v", n, got)
			}
		})
	}
}

func TestSafetyRefreshDueByListTime(t *testing.T) {
	// The safety limit counts from the list call of the poll that fetched the tree, not from the fetch itself, which
	// comes later in the poll, and allows for polls arriving early through jitter. Otherwise the refresh slips to the
	// poll after the one it is due in.
	f := newFakeOctavia(t)
	f.generate(1)
	start := time.Unix(1_800_000_000, 0)
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	f.mu.Lock()
	f.onStatus = func() { clock.Add(int64(30 * time.Second)) } // a status call finishes 30 s after the list call
	f.mu.Unlock()
	op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), time.Minute, thresholds)
	op.now = func() time.Time { return time.Unix(0, clock.Load()) }
	for i := range refreshPolls + 1 {
		clock.Store(start.Add(time.Duration(i) * 58 * time.Second).UnixNano()) // jitter brings polls up to 10% early
		if _, err := op.poll(t.Context()); err != nil {
			t.Fatal(err)
		}
		got := f.fetched()
		if want := i%refreshAfter == 0; (len(got) == 1) != want {
			t.Errorf("poll %d fetched %v; want a fetch every %d polls", i, got, refreshAfter)
		}
	}
}

// settle makes a load balancer's status tree consistent with an ONLINE amphora load balancer: the fixture has an
// ERROR member under an ONLINE pool, which such a load balancer would report as DEGRADED.
func (f *fakeOctavia) settle(t *testing.T, id string) {
	t.Helper()
	f.statuses[id] = bytes.ReplaceAll(f.statuses[id], []byte(`"operating_status": "ERROR"`), []byte(`"operating_status": "ONLINE"`))
}

func TestUnsettledTreeRefetched(t *testing.T) {
	// A new member is OFFLINE until the health manager's first report and then turns ONLINE without changing the load
	// balancer's status or updated_at, so a tree with listeners, members or pools in such states is never confirmed.
	for name, from := range map[string][2]string{
		"error member":      {`"operating_status": "ERROR"`, `"operating_status": "ERROR"`},
		"offline member":    {`"operating_status": "ERROR"`, `"operating_status": "OFFLINE"`},
		"offline pool":      {`"name": "web-pool", "operating_status": "ONLINE"`, `"name": "web-pool", "operating_status": "OFFLINE"`},
		"offline listener":  {`"name": "http", "operating_status": "ONLINE"`, `"name": "http", "operating_status": "OFFLINE"`},
		"error listener":    {`"name": "http", "operating_status": "ONLINE"`, `"name": "http", "operating_status": "ERROR"`},
		"degraded listener": {`"name": "alt", "operating_status": "ONLINE"`, `"name": "alt", "operating_status": "DEGRADED"`},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeOctavia(t)
			if name != "error member" && name != "offline member" {
				f.settle(t, "lb1")
			}
			f.statuses["lb1"] = bytes.ReplaceAll(f.statuses["lb1"], []byte(from[0]), []byte(from[1]))
			op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds)
			for range 2 {
				if _, err := op.poll(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			got := f.fetched()
			if n := len(slices.DeleteFunc(slices.Clone(got), func(id string) bool { return id != "lb1" })); n != 2 {
				t.Errorf("lb1 fetched in %d of 2 polls; fetched %v", n, got)
			}
		})
	}
}

func TestAmphoraAliasesConfirm(t *testing.T) {
	for _, provider := range []string{"amphora", "amphorav2", "octavia"} {
		t.Run(provider, func(t *testing.T) {
			f := newFakeOctavia(t)
			f.settle(t, "lb1")
			f.setLB(t, "lb1", map[string]any{"provider": provider})
			op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds)
			for range 2 {
				if _, err := op.poll(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			got := f.fetched()
			if n := len(slices.DeleteFunc(slices.Clone(got), func(id string) bool { return id != "lb1" })); n != 1 {
				t.Errorf("lb1 fetched in %d of 2 polls, want 1; fetched %v", n, got)
			}
		})
	}
}

func TestStatusWithoutTreeKeepsPrevious(t *testing.T) {
	// A 200 response without the load balancer object is a decode error, not an empty tree: it would otherwise
	// replace the cached tree, and pass as settled.
	for name, body := range map[string]string{
		"empty":             `{}`,
		"null statuses":     `{"statuses":null}`,
		"null loadbalancer": `{"statuses":{"loadbalancer":null}}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeOctavia(t)
			op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds)
			p := poller.New("octavia", func() time.Duration { return time.Minute }, op.poll)
			p.RunOnce(t.Context())
			f.statuses["lb2"] = json.RawMessage(body) // lb2 is not amphora, so it is fetched on every poll
			p.RunOnce(t.Context())
			st := p.Status(time.Now())
			if got := string(p.Body(time.Now())); !strings.Contains(got, `openstack_lb_pool_up{lb_id="lb2",pool_id="pl2"}`) {
				t.Errorf("lb2's previous tree was dropped:\n%s", got)
			}
			if !st.Up || st.Errors["decode"] != 1 {
				t.Errorf("up %v, errors %v; want up with one decode error", st.Up, st.Errors)
			}
		})
	}
}

func TestStatusWithoutListenersAccepted(t *testing.T) {
	f := newFakeOctavia(t)
	op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds)
	if _, err := op.poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.statuses["lb2"] = json.RawMessage(`{"statuses":{"loadbalancer":{"id":"lb2","listeners":[]}}}`)
	body, err := op.poll(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `lb_id="lb2"`) {
		t.Errorf("lb2 has no listeners but its old pools are still rendered:\n%s", body)
	}
}

func TestFailingStatusCallsDoNotStarve(t *testing.T) {
	// Four status calls that fail more slowly than the cutoff take every worker for the whole dispatch window. If
	// failures kept their place at the front of the queue, ok, listed after them, would never be requested.
	f := newFakeOctavia(t)
	var lbs []json.RawMessage
	for i := range statusWorkers {
		lbs = append(lbs, json.RawMessage(fmt.Sprintf(`{"id":"fail-%d","provider":"ovn","operating_status":"ONLINE","provisioning_status":"ACTIVE"}`, i)))
	}
	lbs = append(lbs, json.RawMessage(`{"id":"ok","provider":"ovn","operating_status":"ONLINE","provisioning_status":"ACTIVE"}`))
	f.lbs.Store(lbs)
	f.statuses = map[string]json.RawMessage{ // no status for fail-*: those calls fail
		"ok": json.RawMessage(`{"statuses":{"loadbalancer":{"id":"ok","listeners":[{"pools":[{"id":"pool-ok","operating_status":"ONLINE","members":[]}]}]}}}`),
	}
	f.statusDelay.Store(int64(400 * time.Millisecond))
	client := osclient.NewClient(osclient.ClientConfig{
		Auth: f.cloud.Auth(t), HTTP: f.cloud.Server.Client(), Region: "RegionOne", Interface: "public",
		MaxConcurrent: statusWorkers, RequestTimeout: 5 * time.Second,
	})
	op := newOctaviaPoller(client.Service(Spec), time.Minute, thresholds)
	var body []byte
	for range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), 600*time.Millisecond) // cutoff at 300ms
		var err error
		body, err = op.poll(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(string(body), `pool_id="pool-ok"`) {
		t.Errorf("ok was starved by the failing load balancers; requests %v", f.fetched())
	}
}

func TestExpiredReusedTreeKeepsFreshData(t *testing.T) {
	// Reused trees date the snapshot, but once they are too old only they may go, not the load balancers listed
	// seconds ago. A scrape between polls must still see them.
	f := newFakeOctavia(t)
	f.settle(t, "lb1")
	now := time.Unix(1_800_000_000, 0)
	start := now
	clk := func() time.Time { return now }
	op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds)
	op.now = clk
	p := poller.New("octavia", func() time.Duration { return time.Minute }, op.poll, poller.WithClock(clk))
	p.RunOnce(t.Context())
	delete(f.statuses, "lb2") // lb2 is not amphora, so it is fetched every poll; its calls now fail
	for _, s := range []int{60, 120, 174} {
		now = start.Add(time.Duration(s) * time.Second)
		p.RunOnce(t.Context())
	}
	now = start.Add(181 * time.Second)
	body := string(p.Body(now))
	for _, want := range []string{`openstack_lb_up{id="lb2"} 0`, `openstack_lb_pool_up{lb_id="lb1",pool_id="pl1"} 1`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q 7s after a successful list:\n%s", want, body)
		}
	}
	if strings.Contains(body, `lb_id="lb2"`) {
		t.Errorf("lb2's tree served more than three intervals after it was fetched:\n%s", body)
	}
	if st := p.Status(now); st.DataAge != 7*time.Second {
		t.Errorf("data age %v, want 7s", st.DataAge)
	}
}

func TestListenerErrorRefetched(t *testing.T) {
	// Octavia marks a listener missing from an amphora heartbeat ERROR without changing the load balancer's status or
	// updated_at, so the load balancer list alone can't show it. The listener list does.
	f := newFakeOctavia(t)
	f.settle(t, "lb1")
	now := time.Unix(1_800_000_000, 0)
	clk := func() time.Time { return now }
	op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds)
	op.now = clk
	p := poller.New("octavia", func() time.Duration { return time.Minute }, op.poll, poller.WithClock(clk))
	p.RunOnce(t.Context())
	f.fetched()

	f.statuses["lb1"] = bytes.Replace(f.statuses["lb1"], []byte(`"name": "http", "operating_status": "ONLINE"`),
		[]byte(`"name": "http", "operating_status": "ERROR"`), 1)
	now = now.Add(time.Minute)
	p.RunOnce(t.Context())
	if got := sorted(f.fetched()); !slices.Contains(got, "lb1") {
		t.Errorf("second poll fetched %v; lb1's listener changed to ERROR", got)
	}
	if body := string(p.Body(now)); !strings.Contains(body, `openstack_lb_listener_up{lb_id="lb1",listener_id="l1"} 0`) {
		t.Errorf("listener ERROR not reported:\n%s", body)
	}
}

func TestListenerListFailureConfirmsNothing(t *testing.T) {
	// Without the listener list no tree can be confirmed: every load balancer is fetched, and the poll still succeeds.
	f := newFakeOctavia(t)
	f.settle(t, "lb1")
	op := newOctaviaPoller(f.cloud.Client(t).Service(Spec), 0, thresholds)
	p := poller.New("octavia", func() time.Duration { return time.Minute }, op.poll)
	p.RunOnce(t.Context())
	f.fetched()

	f.failListeners.Store(true)
	p.RunOnce(t.Context())
	if got := sorted(f.fetched()); !slices.Equal(got, []string{"lb1", "lb2", "lb3"}) {
		t.Errorf("fetched %v without the listener list, want [lb1 lb2 lb3]", got)
	}
	if st := p.Status(time.Now()); !st.Up || st.Errors["500"] == 0 {
		t.Errorf("status %+v: want a successful poll with the failed listener call counted", st)
	}
}
