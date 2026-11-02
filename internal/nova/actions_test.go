package nova

import (
	"context"
	"github.com/krisiasty/osmex/internal/poller"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/ostest"
)

type fakeNova struct {
	mu      sync.Mutex
	servers []map[string]any
	actions map[string][]string // server id → action names
	fail    map[string]bool
	calls   map[string]int
	delay   time.Duration
}

func newFakeNova(t *testing.T, ids ...string) (*fakeNova, *ostest.Cloud) {
	f := &fakeNova{actions: map[string][]string{}, fail: map[string]bool{}, calls: map[string]int{}}
	for _, id := range ids {
		f.servers = append(f.servers, map[string]any{"id": id, "status": "ACTIVE", "updated": "u1", "hostId": "h1", "OS-EXT-STS:power_state": 1})
		f.actions[id] = []string{"create"}
	}
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		ostest.WriteJSON(w, 200, map[string]any{"servers": f.servers})
	})
	cloud.Handle("GET /compute/v2.1/servers/{id}/os-instance-actions", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		f.calls[id]++
		fail, names, delay := f.fail[id], f.actions[id], f.delay
		f.mu.Unlock()
		time.Sleep(delay)
		if fail {
			http.Error(w, "boom", 500)
			return
		}
		var out []map[string]string
		for _, n := range names {
			out = append(out, map[string]string{"action": n})
		}
		ostest.WriteJSON(w, 200, map[string]any{"instanceActions": out})
	})
	return f, cloud
}

func (f *fakeNova) set(id, key string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.servers {
		if s["id"] == id {
			s[key] = v
		}
	}
}

func (f *fakeNova) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

func newServersPoller(svc *osclient.ServiceClient) *serversPoller {
	return &serversPoller{svc: svc, interval: time.Minute, now: time.Now, state: map[string]*serverState{}}
}

func pollBody(t *testing.T, sp *serversPoller) string {
	t.Helper()
	body, err := sp.poll(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestFirstCycleFetchesAll(t *testing.T) {
	f, cloud := newFakeNova(t, "s1", "s2", "s3")
	f.actions["s2"] = []string{"create", "reboot", "reboot"}
	body := pollBody(t, newServersPoller(cloud.Client(t).Service(Spec)))
	if f.totalCalls() != 3 {
		t.Errorf("calls = %v", f.calls)
	}
	for _, want := range []string{
		`openstack_server_actions_total{id="s1",action="create"} 1`,
		`openstack_server_actions_total{id="s2",action="reboot"} 2`,
		"# TYPE openstack_server_actions_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
}

func TestUnchangedNotFetched(t *testing.T) {
	f, cloud := newFakeNova(t, "s1", "s2")
	sp := newServersPoller(cloud.Client(t).Service(Spec))
	pollBody(t, sp)
	body := pollBody(t, sp)
	if f.totalCalls() != 2 {
		t.Errorf("calls = %v", f.calls)
	}
	if !strings.Contains(body, `openstack_server_actions_total{id="s2",action="create"} 1`) {
		t.Errorf("counts lost on unchanged cycle:\n%s", body)
	}
}

func TestChangedFetched(t *testing.T) {
	f, cloud := newFakeNova(t, "s1", "s2")
	sp := newServersPoller(cloud.Client(t).Service(Spec))
	pollBody(t, sp)
	f.set("s2", "updated", "u2")
	f.actions["s2"] = []string{"create", "stop"}
	body := pollBody(t, sp)
	if f.calls["s1"] != 1 || f.calls["s2"] != 2 {
		t.Errorf("calls = %v", f.calls)
	}
	if !strings.Contains(body, `openstack_server_actions_total{id="s2",action="stop"} 1`) {
		t.Errorf("new action missing:\n%s", body)
	}
}

func TestHostIDChangeFetched(t *testing.T) {
	f, cloud := newFakeNova(t, "s1")
	sp := newServersPoller(cloud.Client(t).Service(Spec))
	pollBody(t, sp)
	f.set("s1", "hostId", "h2")
	pollBody(t, sp)
	f.set("s1", "OS-SRV-USG:launched_at", "2026-10-07T15:03:39.000000")
	pollBody(t, sp)
	if f.calls["s1"] != 3 {
		t.Errorf("calls = %v", f.calls)
	}
}

func TestCountsNeverDecrease(t *testing.T) {
	f, cloud := newFakeNova(t, "s1")
	f.actions["s1"] = []string{"create", "reboot", "reboot", "reboot"}
	sp := newServersPoller(cloud.Client(t).Service(Spec))
	pollBody(t, sp)
	f.set("s1", "updated", "u2")
	f.actions["s1"] = []string{"reboot"}
	body := pollBody(t, sp)
	if !strings.Contains(body, `openstack_server_actions_total{id="s1",action="reboot"} 3`) ||
		!strings.Contains(body, `openstack_server_actions_total{id="s1",action="create"} 1`) {
		t.Errorf("counter decreased:\n%s", body)
	}
}

func TestFetchFailureRetried(t *testing.T) {
	f, cloud := newFakeNova(t, "s1", "s2")
	f.fail["s2"] = true
	sp := newServersPoller(cloud.Client(t).Service(Spec))
	body := pollBody(t, sp)
	if strings.Contains(body, `id="s2",action=`) {
		t.Errorf("counts for failed fetch:\n%s", body)
	}
	f.fail["s2"] = false
	body = pollBody(t, sp)
	if f.calls["s1"] != 1 || f.calls["s2"] != 2 {
		t.Errorf("calls = %v", f.calls)
	}
	if !strings.Contains(body, `openstack_server_actions_total{id="s2",action="create"} 1`) {
		t.Errorf("retried fetch not rendered:\n%s", body)
	}
}

func TestFetchCutoff(t *testing.T) {
	f, cloud := newFakeNova(t, "s1", "s2", "s3", "s4", "s5")
	f.delay = 150 * time.Millisecond
	client := osclient.NewClient(osclient.ClientConfig{
		Auth: cloud.Auth(t), HTTP: cloud.Server.Client(), Region: "RegionOne", Interface: "public",
		MaxConcurrent: 1, RequestTimeout: 5 * time.Second,
	})
	sp := newServersPoller(client.Service(Spec))
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	if _, err := sp.poll(ctx); err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	if n := f.totalCalls(); n >= 5 {
		t.Errorf("all %d fetches ran despite the cutoff", n)
	}
	pending := 0
	for _, st := range sp.state {
		if st.pending {
			pending++
		}
	}
	if pending == 0 {
		t.Error("no server left pending")
	}
	f.delay = 0
	pollBody(t, sp)
	for _, st := range sp.state {
		if st.pending {
			t.Error("pending servers not fetched in the next cycle")
		}
	}
}

func TestActionFailureLogged(t *testing.T) {
	f, cloud := newFakeNova(t, "s1", "s2")
	f.fail["s2"] = true
	var logs strings.Builder
	sp := newServersPoller(cloud.Client(t).Service(Spec))
	p := poller.New("nova-servers", func() time.Duration { return time.Minute }, sp.poll,
		poller.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	p.RunOnce(t.Context())
	out := logs.String()
	if !strings.Contains(out, `msg="poll partly failed"`) || !strings.Contains(out, "failed_calls=1") ||
		!strings.Contains(out, "/servers/s2/os-instance-actions") {
		t.Errorf("failed instance action call not logged:\n%s", out)
	}
}

// clockedServers returns a nova-servers poller whose clock is *now, for checking data age.
func clockedServers(t *testing.T, cloud *ostest.Cloud, now *time.Time) *poller.Poller {
	sp := newServersPoller(cloud.Client(t).Service(Spec))
	clk := func() time.Time { return *now }
	sp.now = clk
	return poller.New("nova-servers", func() time.Duration { return sp.interval }, sp.poll, poller.WithClock(clk))
}

func TestUnconfirmedCountsAgeAndExpire(t *testing.T) {
	f, cloud := newFakeNova(t, "s1", "s2")
	now := time.Unix(1_800_000_000, 0)
	p := clockedServers(t, cloud, &now)
	p.RunOnce(t.Context())

	// s2 changes and its action history can't be fetched: its counts date from the first poll.
	f.set("s2", "updated", "u2")
	f.fail["s2"] = true
	for i := 1; i <= 3; i++ {
		now = now.Add(time.Minute)
		p.RunOnce(t.Context())
		if st := p.Status(now); !st.Up || st.DataAge != time.Duration(i)*time.Minute {
			t.Fatalf("poll %d: up %v, data age %v, want %v", i, st.Up, st.DataAge, time.Duration(i)*time.Minute)
		}
		if body := string(p.Body(now)); !strings.Contains(body, `openstack_server_actions_total{id="s2",action="create"} 1`) {
			t.Fatalf("poll %d: s2's counts dropped before three intervals:\n%s", i, body)
		}
	}
	// Past three intervals they are dropped, and the snapshot is current again.
	now = now.Add(time.Minute)
	p.RunOnce(t.Context())
	body := string(p.Body(now))
	if strings.Contains(body, `id="s2",action=`) || !strings.Contains(body, `openstack_server_actions_total{id="s1",action="create"} 1`) {
		t.Errorf("want s2's counts dropped and s1's kept:\n%s", body)
	}
	if st := p.Status(now); st.DataAge != 0 {
		t.Errorf("data age %v after dropping the old counts, want 0", st.DataAge)
	}
	// They come back with the next successful fetch.
	f.fail["s2"] = false
	now = now.Add(time.Minute)
	p.RunOnce(t.Context())
	if body := string(p.Body(now)); !strings.Contains(body, `openstack_server_actions_total{id="s2",action="create"} 1`) {
		t.Errorf("s2's counts not restored:\n%s", body)
	}
}

func TestConfirmedCountsStayCurrent(t *testing.T) {
	_, cloud := newFakeNova(t, "s1", "s2")
	now := time.Unix(1_800_000_000, 0)
	p := clockedServers(t, cloud, &now)
	for range 5 {
		p.RunOnce(t.Context())
		now = now.Add(time.Minute)
	}
	if st := p.Status(now.Add(-time.Minute)); st.DataAge != 0 {
		t.Errorf("unchanged servers aged the snapshot: data age %v", st.DataAge)
	}
	if body := string(p.Body(now)); !strings.Contains(body, `openstack_server_actions_total{id="s2",action="create"} 1`) {
		t.Errorf("unchanged server's counts dropped:\n%s", body)
	}
}

func TestFailingActionCallsDoNotStarve(t *testing.T) {
	// Four action calls that fail more slowly than the cutoff take every worker for the whole dispatch window. If
	// failures kept their place at the front of the queue, z, sorting after them, would never be requested.
	f, cloud := newFakeNova(t, "a1", "a2", "a3", "a4", "z")
	for _, id := range []string{"a1", "a2", "a3", "a4"} {
		f.fail[id] = true
	}
	f.delay = 400 * time.Millisecond
	client := osclient.NewClient(osclient.ClientConfig{
		Auth: cloud.Auth(t), HTTP: cloud.Server.Client(), Region: "RegionOne", Interface: "public",
		MaxConcurrent: actionWorkers, RequestTimeout: 5 * time.Second,
	})
	sp := newServersPoller(client.Service(Spec))
	var body []byte
	for range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), 600*time.Millisecond) // cutoff at 300ms
		var err error
		body, err = sp.poll(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(string(body), `openstack_server_actions_total{id="z",action="create"} 1`) {
		t.Errorf("z was starved by the failing servers; calls %v", f.calls)
	}
}

func TestExpiredCountsKeepFreshData(t *testing.T) {
	// Reused counts date the snapshot, but once they are too old only they may go, not the servers listed seconds
	// ago. A scrape between polls must still see them.
	f, cloud := newFakeNova(t, "s1", "s2")
	now := time.Unix(1_800_000_000, 0)
	start := now
	p := clockedServers(t, cloud, &now)
	p.RunOnce(t.Context())
	f.set("s2", "updated", "u2")
	f.fail["s2"] = true
	for _, s := range []int{60, 120, 174} {
		now = start.Add(time.Duration(s) * time.Second)
		p.RunOnce(t.Context())
	}
	now = start.Add(181 * time.Second)
	body := string(p.Body(now))
	for _, want := range []string{`openstack_server_up{id="s2"} 1`, `openstack_server_actions_total{id="s1",action="create"} 1`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q 7s after a successful list:\n%s", want, body)
		}
	}
	if strings.Contains(body, `id="s2",action=`) {
		t.Errorf("s2's counts served more than three intervals after they were confirmed:\n%s", body)
	}
	if st := p.Status(now); st.DataAge != 7*time.Second {
		t.Errorf("data age %v, want 7s", st.DataAge)
	}
}

func TestActionBaseline(t *testing.T) {
	// Actions worth alerting on start at 0 once a server's history is known, so increase() sees their first
	// occurrence. A server whose history isn't known yet gets no action series at all: 0 would be a guess.
	f, cloud := newFakeNova(t, "s1", "s2")
	f.actions["s1"] = []string{"create", "reboot", "reboot"}
	f.fail["s2"] = true
	body := pollBody(t, newServersPoller(cloud.Client(t).Service(Spec)))
	for _, want := range []string{
		`openstack_server_actions_total{id="s1",action="create"} 1`,
		`openstack_server_actions_total{id="s1",action="reboot"} 2`,
		`openstack_server_actions_total{id="s1",action="evacuate"} 0`,
		`openstack_server_actions_total{id="s1",action="live-migration"} 0`,
		`openstack_server_actions_total{id="s1",action="migrate"} 0`,
		`openstack_server_actions_total{id="s1",action="resize"} 0`,
		`openstack_server_actions_total{id="s1",action="rebuild"} 0`,
		`openstack_server_actions_total{id="s1",action="stop"} 0`,
		`openstack_server_actions_total{id="s1",action="start"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	if n := strings.Count(body, `openstack_server_actions_total{id="s1",action="reboot"}`); n != 1 {
		t.Errorf("reboot rendered %d times for s1", n)
	}
	if strings.Contains(body, `openstack_server_actions_total{id="s2",`) {
		t.Errorf("action series for s2, whose history was never fetched:\n%s", body)
	}
}
