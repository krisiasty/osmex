package nova

import (
	"context"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/krisiasty/osmex/internal/expo"
	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/poller"
)

const (
	mib = 1 << 20
	gib = 1 << 30
)

// serverState is what the nova-servers poller remembers about a server between cycles.
type serverState struct {
	updated, launchedAt, hostID string
	counts                      map[string]float64 // instance action counts
	pending                     bool               // instance actions still to be fetched
	tried                       uint64             // cycle of the last action fetch started, 0 if none
	confirmed                   time.Time          // list time of the last poll in which counts were known current
}

type serversPoller struct {
	svc       *osclient.ServiceClient
	interval  time.Duration
	now       func() time.Time
	state     map[string]*serverState
	cycle     uint64            // polls so far that listed the servers
	servers   map[string]server // last known record of every server; nil until a full listing, and after a failed poll
	listStart time.Time         // when the last successful listing started
	deltas    int               // delta listings since the last full one
}

// fullListingEvery is how often, in polls, nova-servers lists every server instead of only those changed since the
// previous listing. Nova bumps a server's updated_at on every change to its record, but not in two cases that a full
// listing catches: a server deleted while it is still being scheduled is destroyed without an update, and a new fault
// on a server already in ERROR is stored apart from the server record.
const fullListingEvery = 10

// NewServers returns the nova-servers poller.
func NewServers(svc *osclient.ServiceClient, interval time.Duration, opts ...poller.Option) *poller.Poller {
	sp := &serversPoller{svc: svc, interval: interval, now: time.Now, state: map[string]*serverState{}}
	return poller.New("nova-servers", func() time.Duration { return interval }, sp.poll, opts...)
}

// serversQuery sends no limit: Nova returns minimal records for servers in an unreachable cell only for a listing
// without paging or sorting parameters, and otherwise leaves those servers out. Without a limit Nova pages at its own
// maximum ([api] max_limit, 1000 by default), so only the pages after the first, which carry a marker, lose them.
var serversQuery = url.Values{"limit": {""}}

func (p *serversPoller) poll(ctx context.Context) ([]byte, error) {
	start := p.now()
	full := p.needsFullListing()
	query := serversQuery
	if !full {
		// From one interval before the previous listing started: changes committed while it ran, and clock differences
		// between osmex and Nova's database, fall inside the overlap. Merging a server twice is harmless.
		since := p.listStart.Add(-p.interval).UTC().Format(time.RFC3339)
		query = url.Values{"limit": {""}, "changes-since": {since}}
	}
	listed, err := osclient.ListAll[server](ctx, p.svc, "/servers/detail", query, "servers")
	if err != nil {
		p.servers = nil // a delta from an unknown point could miss changes, so list every server next time
		return nil, err
	}
	p.merge(listed, full)
	p.listStart = start
	servers := slices.SortedFunc(maps.Values(p.servers), func(a, b server) int { return strings.Compare(a.ID, b.ID) })
	listedAt := p.now()
	p.cycle++
	p.updateState(servers)
	p.fetchActions(ctx, pendingIDs(p.state))
	if reused := p.ageCounts(ctx, servers, listedAt); len(reused) > 0 {
		current := maps.Clone(p.state)
		for _, id := range reused {
			current[id] = &serverState{} // no counts
		}
		poller.ReportFallback(ctx, renderServers(servers, current))
	}
	return renderServers(servers, p.state), nil
}

// needsFullListing reports whether this poll must list every server: at startup and after a failed poll, every
// fullListingEvery polls, and while any server is known only as a down-cell record. A delta can't show a down cell
// coming back, because its servers' updated_at doesn't move, and Nova returns down-cell records only for a listing
// without filters.
func (p *serversPoller) needsFullListing() bool {
	if p.servers == nil || p.deltas+1 >= fullListingEvery {
		return true
	}
	for _, s := range p.servers {
		if s.downCell() {
			return true
		}
	}
	return false
}

// merge applies a listing to the known servers: a full listing replaces them, a delta replaces the servers it returns
// and removes those it returns as DELETED or SOFT_DELETED, which Nova's default listing leaves out. Servers a delta
// doesn't return are unchanged. A restored server is updated, so a later delta returns it again.
func (p *serversPoller) merge(listed []server, full bool) {
	if full {
		p.servers = make(map[string]server, len(listed))
		p.deltas = 0
	} else {
		p.deltas++
	}
	for _, s := range listed {
		if s.Status == "DELETED" || s.Status == "SOFT_DELETED" {
			delete(p.servers, s.ID)
			continue
		}
		p.servers[s.ID] = s
	}
}

// ageCounts dates the instance action counts rendered with servers and returns the servers whose counts are reused.
// Counts of a server that is not pending are current: fetched in this poll, or unchanged since. A pending server's
// counts are reused data: they date the snapshot from when they were last known current and are dropped three
// intervals after that. Between polls the poller then serves the fallback, which has no reused counts, so the rest
// of the snapshot stays.
func (p *serversPoller) ageCounts(ctx context.Context, servers []server, listedAt time.Time) (reused []string) {
	now, maxAge := p.now(), 3*p.interval
	for _, s := range servers {
		st := p.state[s.ID]
		switch {
		case st == nil || s.downCell(): // counts not rendered
		case !st.pending:
			st.confirmed = listedAt
		case st.counts == nil:
		case now.Sub(st.confirmed) > maxAge:
			st.counts = nil
		default:
			poller.ReportDataTime(ctx, st.confirmed)
			reused = append(reused, s.ID)
		}
	}
	return reused
}

// updateState records each server's change markers, marks new or changed servers as pending
// instance action fetches, and forgets servers no longer listed. Down-cell records leave the
// state untouched.
func (p *serversPoller) updateState(servers []server) {
	seen := make(map[string]bool, len(servers))
	for _, s := range servers {
		seen[s.ID] = true
		if s.downCell() {
			continue
		}
		st := p.state[s.ID]
		if changed(st, s) {
			if st == nil {
				st = &serverState{}
				p.state[s.ID] = st
			}
			st.pending = true
		}
		st.updated, st.launchedAt, st.hostID = s.Updated, s.LaunchedAt, s.HostID
	}
	for id := range p.state {
		if !seen[id] {
			delete(p.state, id)
		}
	}
}

func renderServers(servers []server, state map[string]*serverState) []byte {
	var page expo.Page
	info := page.Gauge("openstack_server_info", "Server information; the value is always 1.")
	up := page.Gauge("openstack_server_up", "1 if the server is ACTIVE and its power state is RUNNING, else 0.")
	status := page.Gauge("openstack_server_status", "Current server status; the value is always 1.")
	vmState := page.Gauge("openstack_server_vm_state", "Current server VM state; the value is always 1.")
	powerState := page.Gauge("openstack_server_power_state",
		"Server power state; the power_state label names it and the value is its Nova code "+
			"(0 nostate, 1 running, 3 paused, 4 shutdown, 6 crashed, 7 suspended). Use max by (id) for one series per server.")
	taskState := page.Gauge("openstack_server_task_state", "1 while a task runs on the server; absent when idle.")
	launched := page.Gauge("openstack_server_launched_timestamp_seconds", "Time the server was last launched (OS-SRV-USG:launched_at).")
	updated := page.Gauge("openstack_server_updated_timestamp_seconds", "Time the server record was last updated.")
	created := page.Gauge("openstack_server_created_timestamp_seconds", "Time the server was created.")
	locked := page.Gauge("openstack_server_locked", "1 if the server is locked, else 0.")
	faultTime := page.Gauge("openstack_server_fault_timestamp_seconds", "Time of the server fault; present only in ERROR status.")
	actions := page.Counter("openstack_server_actions_total", "Instance actions recorded for the server, by action.")
	vcpus := page.Gauge("openstack_server_vcpus", "vCPUs of the server flavor.")
	memory := page.Gauge("openstack_server_memory_bytes", "Memory of the server flavor in bytes.")
	rootDisk := page.Gauge("openstack_server_root_disk_bytes", "Root disk of the server flavor in bytes; 0 for volume-backed flavors.")

	for _, s := range servers {
		id := s.ID
		status.Add(1, "id", id, "status", s.Status)
		if s.downCell() {
			continue
		}
		info.Add(1, "id", id, "name", s.Name, "flavor", s.Flavor.OriginalName, "availability_zone", s.AZ,
			"host_id", s.HostID, "image_id", imageID(s.Image))
		running := s.PowerState != nil && *s.PowerState == 1
		up.Add(expo.Bool(s.Status == "ACTIVE" && running), "id", id)
		if s.VMState != "" {
			vmState.Add(1, "id", id, "vm_state", s.VMState)
		}
		if s.PowerState != nil {
			powerState.Add(float64(*s.PowerState), "id", id, "power_state", powerStateName(*s.PowerState))
		}
		if s.TaskState != "" {
			taskState.Add(1, "id", id, "task_state", s.TaskState)
		}
		addTime(launched, s.LaunchedAt, "id", id)
		addTime(updated, s.Updated, "id", id)
		addTime(created, s.Created, "id", id)
		locked.Add(expo.Bool(s.Locked), "id", id)
		if s.Status == "ERROR" && s.Fault != nil {
			addTime(faultTime, s.Fault.Created, "id", id, "code", strconv.Itoa(s.Fault.Code))
		}
		if st := state[id]; st != nil && st.counts != nil {
			for action, n := range st.counts {
				actions.Add(n, "id", id, "action", action)
			}
			for _, action := range baselineActions {
				if _, ok := st.counts[action]; !ok {
					actions.Add(0, "id", id, "action", action)
				}
			}
		}
		vcpus.Add(float64(s.Flavor.VCPUs), "id", id)
		memory.Add(float64(s.Flavor.RAM)*mib, "id", id)
		rootDisk.Add(float64(s.Flavor.Disk)*gib, "id", id)
	}
	return page.Bytes()
}

// baselineActions are the instance actions exported at 0 once a server's history is known, so that increase() counts
// their first occurrence; other actions appear when they first happen.
var baselineActions = []string{"evacuate", "live-migration", "migrate", "resize", "rebuild", "reboot", "stop", "start"}

// powerStateNames are Nova's names for OS-EXT-STS:power_state codes.
var powerStateNames = map[int]string{0: "nostate", 1: "running", 3: "paused", 4: "shutdown", 6: "crashed", 7: "suspended"}

// powerStateName returns the name of a power state code, or the code itself if Nova adds one osmex does not know.
func powerStateName(code int) string {
	if name, ok := powerStateNames[code]; ok {
		return name
	}
	return strconv.Itoa(code)
}

// addTime adds a timestamp sample in Unix seconds; empty or unparsable timestamps are skipped.
func addTime(f *expo.Family, ts string, labels ...string) {
	t, err := osclient.ParseTime(ts)
	if err != nil || t.IsZero() {
		return
	}
	f.Add(float64(t.UnixNano())/1e9, labels...)
}
