package nova

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/ostest"
)

var deltaT0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// deltaNova behaves like Nova's servers/detail for delta polling: changes-since filters on updated >= since and also
// returns deleted servers, as DELETED; a listing without it returns only live servers. Servers in a down cell come back
// as minimal records from an unfiltered listing, and are left out of a filtered one.
type deltaNova struct {
	mu       sync.Mutex
	servers  map[string]map[string]any
	updated  map[string]time.Time
	deleted  map[string]bool
	soft     map[string]bool // soft-deleted: left out of a full listing, SOFT_DELETED in a delta
	downCell map[string]bool
	queries  []url.Values
	fail     bool
}

func newDeltaNova(t *testing.T, ids ...string) (*deltaNova, *ostest.Cloud) {
	f := &deltaNova{servers: map[string]map[string]any{}, updated: map[string]time.Time{}, deleted: map[string]bool{}, soft: map[string]bool{},
		downCell: map[string]bool{}}
	for _, id := range ids {
		f.put(id, "ACTIVE", deltaT0.Add(-time.Hour))
	}
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		q := r.URL.Query()
		f.queries = append(f.queries, q)
		if f.fail {
			http.Error(w, "boom", 500)
			return
		}
		var since time.Time
		if s := q.Get("changes-since"); s != "" {
			var err error
			if since, err = time.Parse(time.RFC3339, s); err != nil {
				t.Errorf("changes-since %q: %v", s, err)
			}
		}
		_, paged := q["limit"]
		out := []map[string]any{}
		for id, s := range f.servers {
			switch {
			case f.downCell[id] && (!since.IsZero() || paged):
				continue
			case f.downCell[id]:
				out = append(out, map[string]any{"id": id, "status": "UNKNOWN", "tenant_id": "proj", "created": "2026-10-01T00:00:00Z"})
			case !since.IsZero() && f.updated[id].Before(since):
			case f.deleted[id] && since.IsZero():
			case f.deleted[id]:
				out = append(out, map[string]any{"id": id, "status": "DELETED", "updated": s["updated"]})
			case f.soft[id] && since.IsZero():
			case f.soft[id]:
				out = append(out, map[string]any{"id": id, "status": "SOFT_DELETED", "updated": s["updated"]})
			default:
				out = append(out, s)
			}
		}
		ostest.WriteJSON(w, 200, map[string]any{"servers": out})
	})
	cloud.Handle("GET /compute/v2.1/servers/{id}/os-instance-actions", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"instanceActions": []any{}})
	})
	return f, cloud
}

func (f *deltaNova) put(id, status string, updated time.Time) {
	power := 1
	if status != "ACTIVE" {
		power = 4
	}
	f.servers[id] = map[string]any{"id": id, "name": id, "status": status, "OS-EXT-STS:power_state": power, "updated": updated.Format(time.RFC3339)}
	f.updated[id] = updated
}

func (f *deltaNova) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// full reports, for each request so far, whether it was a full listing (no changes-since).
func (f *deltaNova) full() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []bool
	for _, q := range f.queries {
		out = append(out, q.Get("changes-since") == "")
	}
	return out
}

func deltaPoller(t *testing.T, cloud *ostest.Cloud, now *time.Time) *serversPoller {
	t.Helper()
	sp := newServersPoller(cloud.Client(t).Service(Spec))
	sp.now = func() time.Time { return *now }
	return sp
}

func TestDeltaQuery(t *testing.T) {
	// After a full listing, polls ask only for servers changed since the previous listing started, minus one interval
	// for clock differences between osmex and Nova's database.
	f, cloud := newDeltaNova(t, "s1")
	now := deltaT0
	sp := deltaPoller(t, cloud, &now)
	pollBody(t, sp)
	now = now.Add(time.Minute)
	pollBody(t, sp)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) != 2 || f.queries[0].Has("changes-since") || f.queries[1].Get("changes-since") != "2026-11-01T11:59:00Z" {
		t.Errorf("queries = %v, want a full listing, then changes-since=2026-11-01T11:59:00Z", f.queries)
	}
	for _, q := range f.queries {
		if q.Has("limit") {
			t.Errorf("query %v carries limit", q)
		}
	}
}

func TestDeltaMerge(t *testing.T) {
	f, cloud := newDeltaNova(t, "s1", "s2", "s3")
	now := deltaT0
	sp := deltaPoller(t, cloud, &now)
	pollBody(t, sp)

	now = now.Add(time.Minute)
	f.set(func() {
		f.put("s2", "SHUTOFF", now)
		f.deleted["s3"], f.updated["s3"] = true, now
		f.put("s4", "ACTIVE", now)
	})
	body := pollBody(t, sp)
	for _, want := range []string{
		`openstack_server_status{id="s1",status="ACTIVE"} 1`, // unchanged, kept from the previous listing
		`openstack_server_status{id="s2",status="SHUTOFF"} 1`,
		`openstack_server_status{id="s4",status="ACTIVE"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	if strings.Contains(body, `id="s3"`) || sp.state["s3"] != nil {
		t.Errorf("deleted server s3 still reported or remembered:\n%s", body)
	}
	if got := f.full(); len(got) != 2 || got[1] {
		t.Errorf("second poll was not a delta: %v", got)
	}
}

func TestFullListingCadence(t *testing.T) {
	f, cloud := newDeltaNova(t, "s1")
	now := deltaT0
	sp := deltaPoller(t, cloud, &now)
	for range 2*fullListingEvery + 1 {
		pollBody(t, sp)
		now = now.Add(time.Minute)
	}
	for i, full := range f.full() {
		if want := i%fullListingEvery == 0; full != want {
			t.Errorf("poll %d full listing %v, want %v", i+1, full, want)
		}
	}
}

func TestFailedPollForcesFullListing(t *testing.T) {
	f, cloud := newDeltaNova(t, "s1")
	now := deltaT0
	sp := deltaPoller(t, cloud, &now)
	pollBody(t, sp)
	f.set(func() { f.fail = true })
	if _, err := sp.poll(t.Context()); err == nil {
		t.Fatal("poll succeeded against a failing API")
	}
	f.set(func() { f.fail = false })
	pollBody(t, sp)
	if got := f.full(); len(got) != 3 || !got[2] {
		t.Errorf("poll after a failure was not a full listing: %v", got)
	}
}

func TestDownCellKeepsFullListings(t *testing.T) {
	// A delta can't show a down cell coming back, because its servers' updated_at doesn't move, so while any server is
	// known only as a down-cell record every poll is a full listing.
	f, cloud := newDeltaNova(t, "s1", "s2")
	f.downCell["s2"] = true
	now := deltaT0
	sp := deltaPoller(t, cloud, &now)
	pollBody(t, sp)
	now = now.Add(time.Minute)
	body := pollBody(t, sp)
	if !strings.Contains(body, `openstack_server_status{id="s2",status="UNKNOWN"} 1`) {
		t.Errorf("down-cell server not UNKNOWN:\n%s", body)
	}

	f.set(func() { delete(f.downCell, "s2") })
	now = now.Add(time.Minute)
	body = pollBody(t, sp)
	if !strings.Contains(body, `openstack_server_status{id="s2",status="ACTIVE"} 1`) {
		t.Errorf("server not back after its cell recovered:\n%s", body)
	}
	now = now.Add(time.Minute)
	pollBody(t, sp)
	if got := f.full(); len(got) != 4 || !got[0] || !got[1] || !got[2] || got[3] {
		t.Errorf("full listings %v, want full while s2 is a down-cell record, then a delta", got)
	}
}

func TestDeltaRemovesSoftDeleted(t *testing.T) {
	// Nova's default listing leaves soft-deleted servers out, but changes-since returns them as SOFT_DELETED. Keeping
	// them would report them down until the next full listing. A restore updates the server, so a delta brings it back.
	f, cloud := newDeltaNova(t, "s1", "s2")
	now := deltaT0
	sp := deltaPoller(t, cloud, &now)
	pollBody(t, sp)

	now = now.Add(time.Minute)
	f.set(func() { f.soft["s2"], f.updated["s2"] = true, now })
	body := pollBody(t, sp)
	if strings.Contains(body, `id="s2"`) || sp.state["s2"] != nil {
		t.Errorf("soft-deleted server s2 still reported or remembered:\n%s", body)
	}

	now = now.Add(time.Minute)
	f.set(func() {
		delete(f.soft, "s2")
		f.put("s2", "ACTIVE", now)
	})
	if body := pollBody(t, sp); !strings.Contains(body, `openstack_server_status{id="s2",status="ACTIVE"} 1`) {
		t.Errorf("restored server s2 not back:\n%s", body)
	}
	if got := f.full(); len(got) != 3 || got[1] || got[2] {
		t.Errorf("polls after the first were not deltas: %v", got)
	}
}
