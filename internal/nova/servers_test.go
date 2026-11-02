package nova

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/ostest"
)

func loadServers(t *testing.T) []server {
	t.Helper()
	b, err := os.ReadFile("testdata/servers.json")
	if err != nil {
		t.Fatal(err)
	}
	var page struct{ Servers []server }
	if err := json.Unmarshal(b, &page); err != nil {
		t.Fatal(err)
	}
	return page.Servers
}

func TestRenderServersGolden(t *testing.T) {
	ostest.Golden(t, "testdata/servers.golden", renderServers(loadServers(t), map[string]*serverState{}))
}

func TestImageID(t *testing.T) {
	cases := map[string]string{`""`: "", `null`: "", ``: "", `{"id":"img","links":[]}`: "img"}
	for in, want := range cases {
		if got := imageID(json.RawMessage(in)); got != want {
			t.Errorf("imageID(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestServersPollPaginates(t *testing.T) {
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("OpenStack-API-Version") != "compute 2.96" {
			t.Errorf("microversion header = %q", r.Header.Get("OpenStack-API-Version"))
		}
		if r.URL.Query().Get("marker") == "" {
			ostest.WriteJSON(w, 200, map[string]any{
				"servers":       []map[string]any{{"id": "s1", "name": "one", "status": "ACTIVE", "OS-EXT-STS:power_state": 1}},
				"servers_links": []map[string]string{{"rel": "next", "href": "http://nova/v2.1/servers/detail?marker=s1"}},
			})
			return
		}
		ostest.WriteJSON(w, 200, map[string]any{"servers": []map[string]any{{"id": "s2", "name": "two", "status": "SHUTOFF", "OS-EXT-STS:power_state": 4}}})
	})
	cloud.Handle("GET /compute/v2.1/servers/{id}/os-instance-actions", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"instanceActions": []any{}})
	})
	p := NewServers(cloud.Client(t).Service(Spec), time.Minute)
	p.RunOnce(t.Context())
	body := string(p.Body(time.Now()))
	if !strings.Contains(body, `openstack_server_up{id="s1"} 1`) || !strings.Contains(body, `openstack_server_up{id="s2"} 0`) {
		t.Errorf("body:\n%s", body)
	}
	if p.Name() != "nova-servers" {
		t.Errorf("name = %q", p.Name())
	}
}

func TestServersPollEmpty(t *testing.T) {
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"servers": []any{}})
	})
	p := NewServers(cloud.Client(t).Service(Spec), time.Minute)
	p.RunOnce(t.Context())
	if st := p.Status(time.Now()); !st.Up || !st.HasData || len(p.Body(time.Now())) != 0 {
		t.Errorf("status %+v body %q", st, p.Body(time.Now()))
	}
}

func TestServersPollFails(t *testing.T) {
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	})
	p := NewServers(cloud.Client(t).Service(Spec), time.Minute)
	p.RunOnce(t.Context())
	if st := p.Status(time.Now()); st.Up || st.Errors["500"] != 1 {
		t.Errorf("status %+v", st)
	}
}

func TestServerStateForgetsDeleted(t *testing.T) {
	sp := &serversPoller{state: map[string]*serverState{"gone": {}}}
	sp.updateState([]server{{ID: "a", Status: "ACTIVE"}, {ID: "x", Status: "UNKNOWN"}})
	if _, ok := sp.state["gone"]; ok {
		t.Error("state of a deleted server kept")
	}
	if _, ok := sp.state["a"]; !ok {
		t.Error("state of a listed server missing")
	}
	if _, ok := sp.state["x"]; ok {
		t.Error("state created for a down-cell record")
	}
}

func TestPowerStateName(t *testing.T) {
	for code, want := range map[int]string{0: "nostate", 1: "running", 3: "paused", 4: "shutdown", 6: "crashed", 7: "suspended", 9: "9"} {
		if got := powerStateName(code); got != want {
			t.Errorf("powerStateName(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestPowerStateSeries(t *testing.T) {
	body := string(renderServers(loadServers(t), map[string]*serverState{}))
	for _, want := range []string{
		`openstack_server_power_state{id="a1",power_state="running"} 1`,
		`openstack_server_power_state{id="b2",power_state="shutdown"} 4`,
		`openstack_server_power_state{id="c3",power_state="nostate"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestServersPollDownCell(t *testing.T) {
	// Nova returns minimal records for servers in an unreachable cell only for a listing without filters or paging
	// parameters, and otherwise leaves them out (list_records_by_skipping_down_cells, the default); changes-since is a
	// filter. The poller must report such a server as UNKNOWN at the next full listing, not drop it, and keep its
	// action counts for when the cell is back.
	cloud := ostest.New(t, "compute")
	var cellDown atomic.Bool
	cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		filtered := q.Has("limit") || q.Has("marker") || q.Has("changes-since")
		s2 := map[string]any{"id": "s2", "name": "two", "status": "ACTIVE", "OS-EXT-STS:power_state": 1, "updated": "2026-11-01T10:00:00Z"}
		if cellDown.Load() {
			s2 = map[string]any{"id": "s2", "status": "UNKNOWN", "tenant_id": "proj", "created": "2026-10-01T10:00:00Z"}
			if filtered {
				s2 = nil
			}
		}
		servers := []map[string]any{{"id": "s1", "name": "one", "status": "ACTIVE", "OS-EXT-STS:power_state": 1, "updated": "2026-11-01T10:00:00Z"}}
		if s2 != nil {
			servers = append(servers, s2)
		}
		ostest.WriteJSON(w, 200, map[string]any{"servers": servers})
	})
	var actionCalls atomic.Int32
	cloud.Handle("GET /compute/v2.1/servers/{id}/os-instance-actions", func(w http.ResponseWriter, r *http.Request) {
		actionCalls.Add(1)
		ostest.WriteJSON(w, 200, map[string]any{"instanceActions": []map[string]string{{"action": "reboot"}}})
	})
	p := NewServers(cloud.Client(t).Service(Spec), time.Minute)
	p.RunOnce(t.Context())
	calls := actionCalls.Load()

	// Delta polls can't see the cell go down: s2 keeps its last record until the next full listing.
	cellDown.Store(true)
	for range fullListingEvery - 1 {
		p.RunOnce(t.Context())
		if body := string(p.Body(time.Now())); !strings.Contains(body, `openstack_server_status{id="s2",status="ACTIVE"} 1`) {
			t.Fatalf("server dropped during delta polls:\n%s", body)
		}
	}
	p.RunOnce(t.Context()) // full listing
	body := string(p.Body(time.Now()))
	if !strings.Contains(body, `openstack_server_status{id="s2",status="UNKNOWN"} 1`) || strings.Contains(body, `openstack_server_up{id="s2"}`) {
		t.Errorf("down-cell server not reported as UNKNOWN only:\n%s", body)
	}

	// While s2 is a down-cell record every poll is a full listing, so its return shows at once.
	cellDown.Store(false)
	p.RunOnce(t.Context())
	body = string(p.Body(time.Now()))
	if !strings.Contains(body, `openstack_server_actions_total{id="s2",action="reboot"} 1`) || actionCalls.Load() != calls {
		t.Errorf("action counts lost across the cell outage (%d action calls after the first poll's %d):\n%s", actionCalls.Load(), calls, body)
	}
}
