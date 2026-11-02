# OpenStack behaviour osmex relies on

osmex avoids most API calls by trusting what list responses say about change: Nova's `updated_at` for delta polling, and the Octavia load
balancer list for reusing status trees. This file records each behaviour osmex depends on, the evidence for it, and what in osmex would go wrong
if it changed.

Evidence is of two kinds:

- **Source:** read in the upstream code, `stable/2025.1` unless stated otherwise. Re-check these when the cloud moves to a newer release.
- **Cloud test:** observed on a real cloud, with the date. These were run in a test project, with the amphora Octavia provider; all test
  resources were deleted afterwards.

## Nova

### `changes-since` sees every change osmex exports

Source. osmex relies on it for delta polling of `nova-servers`.

- `instances.updated_at` has `onupdate=timeutils.utcnow()` (oslo.db `TimestampMixin`). Instance saves go through `_instance_update` and
  `update_on_match`, a Core `UPDATE`, which applies it.
- `changes-since` filters on `updated_at >= since` and nothing else (`nova/db/main/api.py`, `_get_query_nova_resource_by_changes_time`).
- Deleted servers are returned to project-scoped callers: `servers.py` sets `deleted=False` only when neither `changes-since` nor
  `changes-before` is given, with no admin check. Deletion uses oslo.db's query-level `soft_delete`, which passes `updated_at` explicitly so the
  deletion doesn't bump it.
- Soft-deleted servers: the default listing (`deleted=False`) also filters out `vm_state == SOFT_DELETED` in `instance_get_all_by_filters_sort`,
  but `changes-since` applies no deleted filter, so a delta returns them as `SOFT_DELETED`. osmex removes them like `DELETED` ones. A restored
  server is saved, so a later delta returns it again.

Every exported state bumps `updated_at`:

| State | Evidence |
| --- | --- |
| `status`, `vm_state`, `task_state` | Transitions are `instance.save()`; `status` is computed from the other two (`status_from_state`). |
| `power_state` | `_sync_instance_power_state` saves whenever the database value differs from the hypervisor's. |
| `locked` | `lock` and `unlock` save `instance.locked`. |
| `hostId`, `launched_at`, flavor | `host` and `launched_at` are columns of the instance row; resizes save the flavor together with state changes. Not every migration path was traced individually. |
| New servers | Build requests ignore `changes-since` (`BuildRequestList.get_by_filters`), so they are always returned. Instances buried in cell0 are saved right after `create()` (`set_vm_state_and_notify`). |
| Normal deletion | Every normal path saves `vm_state=DELETED` just before `soft_delete`: compute manager `_delete_instance`, API `_local_delete`, `_do_delete`, `_do_soft_delete` and the main `_delete` path. |

Two changes don't bump `updated_at`. The full listing every 10th poll bounds both, and the README documents them:

1. **A server deleted while it is still being scheduled.** With no host and a `vm_state` other than `ERROR` or `SHELVED_OFFLOADED`
   (`may_have_ports_or_volumes`), `compute/api.py` `_delete` calls `instance.destroy()` without saving the instance first.
2. **A new fault on a server already in `ERROR`.** `add_instance_fault_from_exc` only adds an `instance_faults` row. 68 compute manager methods
   record faults through `@wrap_instance_fault`, including read-only ones such as `get_vnc_console` and `get_console_output`.

Two more details, both harmless to osmex:

- An instance created in a cell has a NULL `updated_at` until the compute host first saves it, so it briefly matches no delta. It was already
  returned as a build request, and the merge keeps that record.
- Deleted rows stay visible to deltas until the operators archive them (`nova-manage db archive_deleted_rows`). The full listing catches a row
  archived before a delta saw it.

**If this changed:** a state change that no longer bumps `updated_at` would show only at the next full listing, up to 10 polls late.

### Down cells

Source. Nova returns minimal records for servers in an unreachable cell (microversion 2.69) only for a listing with no filters and no paging or
sorting parameters (`_is_cell_down_supported` in `nova/api/openstack/compute/servers.py`):

```python
pag_sort = any(ps in req.GET.keys() for ps in PAGING_SORTING_PARAMS)   # ('sort_key', 'sort_dir', 'limit', 'marker')
filters = list(search_opts.keys()) not in ([u'all_tenants'], [])
if pag_sort or filters:
    cell_down_support = False
```

Otherwise `[api] list_records_by_skipping_down_cells` decides. With `True`, the default, servers in a down cell are left out of the listing. With
`False`, the listing fails with HTTP 500.

A minimal record has `id`, `status: UNKNOWN`, `tenant_id`, `created` and `links` (`_show_from_down_cell` in `views/servers.py`).

osmex therefore sends no `limit` on the server listing, and Nova pages at `[api] max_limit` (1000 by default). Pages after the first carry a
`marker` and lose down-cell records. `changes-since` is a filter too, so deltas never contain minimal records, and every poll is a full listing
while any server is `UNKNOWN`.

### Limits are complete

Source, on `master`. `nova/api/openstack/compute/limits.py` gets `QUOTAS.get_project_quotas(context, project_id, usages=True)`, and
`views/limits.py` maps cores, RAM and instances to fixed keys. A key is left out only when its resource is filtered for the microversion, which
never applies to these three, or when the quota engine returns `None`. Unlimited is `-1`, not `None`. Usage of one's own project has no separate
policy. osmex still omits a missing field instead of reporting 0, as a guard against something other than Nova answering.

## Cinder

- **Usage can be hidden by policy.** The `used_limits` extension checks its policy with `fatal=False` and silently drops the usage fields while
  still answering 200 (source). So `openstack_volume_quota_used` is omitted rather than reported as 0 when a field is missing. Cloud test,
  2026-10-31: a `reader` credential sees the usage fields.
- **A backup's `data_timestamp` is its recovery point.** Cloud test, 2026-10-31: a backup taken from a snapshot reports the snapshot's time, so
  `openstack_volume_last_backup_timestamp_seconds` uses `data_timestamp`, falling back to `created_at`.
- **A failed backup deletion leaves `error_deleting`**, as for snapshots, so both count as failed.

## Octavia

### What the load balancer list shows

Cloud test, 2026-10-31. Setup: two servers, one HTTP load balancer, and a pool with an HTTP health monitor. Members were failed by removing the
server's security group, so no Octavia API call was involved.

| Step | Change | Member 2 | Load balancer in the list | Load balancer `updated_at` |
| --- | --- | --- | --- | --- |
| 1 | baseline | ONLINE | ONLINE | 21:07:03 |
| 2 | member 2 fails health checks | ERROR | DEGRADED | moved |
| 3 | member 2 recovers | ONLINE | ONLINE | moved |
| 4 | pool description changed | ONLINE | ONLINE | moved |
| 5 | member 3 added on a dead port (always ERROR) | ONLINE | DEGRADED | 21:13:00 |
| 6 | member 2 fails as well | **ERROR** | DEGRADED | **21:13:00, unchanged** |
| 7 | member 2 recovers | **ONLINE** | DEGRADED | **21:13:00, unchanged** |
| 8 | member 3 removed | ONLINE | ONLINE | moved |
| 9 | member 2 weight 0 | DRAINING | ONLINE | moved |
| 10 | draining member 2 fails | ERROR | DEGRADED | moved |
| 11 | draining member 2 recovers | DRAINING | ONLINE | moved |
| 12 | member 2 disabled | OFFLINE | ONLINE | moved |
| 13 | health monitor deleted | NO_MONITOR | ONLINE | moved |

The pool's own status and `updated_at` followed the load balancer's in every step.

- Every configuration change bumped `updated_at`.
- A health change on an `ONLINE` load balancer flips it to `DEGRADED` and back, draining members included.
- **Once a load balancer is `DEGRADED`, more members can fail or recover without any change in the list** (steps 6 and 7). Octavia writes the
  load balancer row only when its own status changes.
- Not tested: renaming a member, and changing a listener.

**What osmex relies on:** a tree is reused only for an `amphora` load balancer listed `ACTIVE` + `ONLINE` with an unchanged `updated_at`. Any
other load balancer is fetched every poll.

### Change detection end to end

Cloud test, 2026-10-31, of change detection, run with `-octavia -octavia-interval 60s` and a temporary log line listing the trees fetched per
poll. Two other load balancers in the project were left untouched.

| Step (UTC) | What osmex fetched and reported | Result |
| --- | --- | --- |
| member 2 re-added disabled (22:45:18) | fetched at 22:45:51, 22:46:50 and 22:47:47 while member 2 was `OFFLINE`, although the load balancer stayed `ONLINE` and unchanged | unsettled tree fetched every poll |
| member 2 enabled (22:47:59) | 22:48:53 member `ONLINE`; 22:49:48 nothing fetched | as intended |
| member 2 failing (22:50:15) | 22:50:46 load balancer `DEGRADED`, member `ERROR`; fetched every poll while `DEGRADED` | as intended |
| member 2 restored (22:52:28) | 22:52:49 `ONLINE`; 22:53:50 nothing fetched | as intended |
| member 3 added on a dead port (22:54:41) | 22:54:48 member 3 `OFFLINE` under an `ONLINE` load balancer; 22:55:49 `ERROR`, load balancer `DEGRADED` | as intended |
| member 2 failing while `DEGRADED` (22:57:01) | 22:57:49 member 2 `ERROR`; load balancer stays `DEGRADED`, its list entry unchanged | as intended |
| member 2 restored while `DEGRADED` (22:59:14) | 22:59:46 member 2 `ONLINE` | as intended |
| member 3 removed (23:01:27) | 23:01:48 back to `ONLINE`; from 23:02:43 nothing fetched | as intended |

- Every change appeared in the first poll after it, 7 to 48 s later.
- With nothing changing, polls made no status calls. Before, every poll made one per load balancer.
- The unchanged load balancers were safety-refreshed one per poll, each about every 5 polls, as the schedule intends with fewer than ten of them.
- A new member is `OFFLINE` under an `ONLINE` load balancer until its first health report, then turns `ONLINE` without any change in the list. So
  only settled trees are reused: pools `ONLINE` or `NO_MONITOR`, members `ONLINE`, `NO_MONITOR` or `DRAINING`.

### Listener status and amphora heartbeats

Source: `octavia/amphorae/drivers/health/heartbeat_udp.py`, which is the same on `master` apart from minor fixes.

- **A missing listener is hidden from the list, briefly.** If an enabled listener on an `ACTIVE` load balancer is missing from a heartbeat, the
  listener is set to `ERROR`, and its unreported pools to `OFFLINE`. The load balancer's status and `updated_at` don't change.
- **Failover then makes it visible.** The heartbeat reports fewer listeners than expected, so the amphora's health timestamp isn't refreshed.
  After `heartbeat_timeout` (60 s by default) the health manager fails the amphora over and sets the load balancer to `PENDING_UPDATE`, which the
  list shows. The change stays hidden only if failover doesn't run: when `failover_threshold` (unset by default) is reached, the amphorae stay in
  `FAILOVER_STOPPED`.
- **A full listener is `DEGRADED`.** A listener that HAProxy reports `FULL`, at its connection limit, becomes `DEGRADED`, and turns an `ONLINE`
  load balancer `DEGRADED`. The listener still serves, so `openstack_lb_listener_up` counts `DEGRADED` as up.
- **An unrecognised listener status** only logs a warning and changes nothing.
- Pools need no list of their own: a pool goes unreported only together with a missing listener.

**What osmex relies on:** each poll also compares the listener list (`GET /v2/lbaas/listeners`) with the cached trees. That closes the window
before failover, and the whole gap when failovers are stopped.

### Provisioning status

Source: `test_and_set_lb_and_listeners_prov_status` sets a listener's provisioning status together with its load balancer's.

### Quotas

Source.

- `_get_db_quotas` returns the configured defaults and fills in defaults for any unset field, so the API never returns `null`, and no
  `/quotas/defaults` call is needed.
- Every `default_*_quota` option defaults to unlimited. On a cloud that doesn't configure Octavia quotas, `octavia-limits` exports no series.
- Field names: `load_balancer`, `listener`, `member`, `pool`, `health_monitor`, `l7policy`, `l7rule`. Older responses may use `loadbalancer` and
  `healthmonitor`, which osmex reads as fallbacks.
- `load-balancer:read-quota` allows `observer_and_owner`, so `load-balancer_observer` is enough.

## Keystone

Cloud test, 2026-11-01, with a restricted credential (`"unrestricted": false`) holding custom read roles and no access rules.

- `GET /v3/users/{user_id}/application_credentials/{id}` succeeds with a token issued for the credential itself, through Keystone's owner rule
  for `identity:get_application_credential`.
- `expires_at` is zone-less UTC with microseconds, and `null` when the credential never expires:

  ```json
  {
    "application_credential": {
      "id": "<credential id>",
      "name": "osmex",
      "user_id": "<user id>",
      "project_id": "<project id>",
      "expires_at": "2026-12-31T00:00:00.000000",
      "unrestricted": false,
      "roles": [{ "id": "<role id>", "name": "<role name>" }],
      "links": { "self": "https://<keystone>/v3/application_credentials/<credential id>" }
    }
  }
  ```

- The exported expiry matched: 2026-12-31T00:00:00Z, `1.7986752e+09`.

Not tested: whether Keystone enforces access rules on its own API, which needs a credential created with access rules.

## Neutron

- **Port listing cost grows with the number of ports returned.** Measured 2026-11-01 with `openstack port list` in a project with 61 ports: all
  ports took 2.03 s, and a `--device-owner` filter matching none took 1.36 s, so about 11 ms per port. osmex's own call took about 17 ms per port
  (see below). These are single runs, so treat the per-port figures as approximate. Neutron checks policy per port and per field, so a custom
  role may make this slower; comparing with a plain `reader` token would show it.
- **On OVN, a floating IP's status reflects configuration, not traffic**, so osmex exports no 0/1 metric for it.

## Measured API cost

Measured on 2026-11-01 in a project with about 25 servers, 61 ports and 2 load balancers, with `-debug` and every module except Barbican. The
figures are `time_to_headers` from the `http response` lines, over 16 to 17 polls at the default intervals. The first call to each host is left
out: it includes the TLS handshake and token validation, and took 140 to 620 ms, against 50 to 170 ms for later calls.

Before and after delta polling, `GET /servers/detail`:

| | Median | Max | Response |
| --- | --- | --- | --- |
| v0.3.0, full listing every poll | 1.30 s | 1.44 s | 44.6 KB every poll |
| v0.4.0, full listing every 10th poll | 168 ms | 1.38 s | 44.6 KB on full listings (1.38 s and 1.18 s), 15 bytes on deltas (71 to 199 ms) |

- Nova time per poll fell from 1.30 s to about 0.28 s on average, and the data transferred by about 88%.
- An empty delta still takes about 150 ms, Nova's fixed cost per request.
- All 25 instance action histories were fetched in the first poll and none after it.
- Octavia made 7 status calls over 17 polls for its 2 load balancers, instead of 34.

The slowest call is now `GET /v2.0/ports` (`neutron-ports`), with a median of 967 ms for 61 ports, of which 29 belong to servers. Filtering by
`device_owner` would roughly halve it, but was not adopted (see [decisions](decisions.md#neutron-ports-are-not-filtered-by-owner)). Every other
call has a median under 200 ms:

| Call | Median |
| --- | --- |
| `GET /v2.0/floatingips` | 171 ms |
| `GET /v3/volumes/detail` | 170 ms |
| `GET /v2.0/quotas/{id}/details` | 154 ms |
| `GET /v2/lbaas/loadbalancers/{id}/status` | 147 ms |
| `GET /v2/lbaas/loadbalancers` | 102 ms |
| `GET /v3/limits` (Cinder) | 100 ms |
| `GET /v2.1/servers/{id}/os-instance-actions` | 98 ms |
| `GET /v2.1/limits` (Nova) | 98 ms |
| `GET /v2/lbaas/listeners` | 89 ms |
| `GET /v3/users/{id}/application_credentials/{id}` | 82 ms |
| `GET /v3/backups/detail` | 57 ms |
| `GET /v3/snapshots/detail` | 56 ms |
| `GET /v2/lbaas/quotas/{id}` | 43 ms |
