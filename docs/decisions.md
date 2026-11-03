# Design decisions

Decisions that shape osmex, with the reason and the alternatives that were not chosen. The README describes what osmex does; this file explains
why. The OpenStack behaviour these decisions rest on is in [openstack-behaviour.md](openstack-behaviour.md).

## Metrics

### Zero means "checked, none"; absent means "unknown"

Per-volume snapshot and backup summaries are exported at 0 for volumes without copies, using the volume list. With zeros,
`openstack_volume_backups == 0` goes quiet when backup data is unavailable, which is correct. Without them, the same question needs
`openstack_volume_info unless on(id) …`, which matches every volume when the backups API fails and needs an `api_up` guard on every such query.

The same rule applies to counters. `increase()` needs two samples, so a counter that first appears at 1 hides its first event. Common instance
actions and error codes are therefore exported at 0, but a server's actions only once its history is known: before that, 0 would be a guess.

### No redundant metrics

A series is added only if it carries information no other series, or simple PromQL filter, already has; never for symmetry. Removed on that rule:

| Removed | Use instead |
| --- | --- |
| `openstack_lb_provisioning_ok`, `openstack_lb_listener_provisioning_ok` | `…_provisioning_status{provisioning_status="ERROR"}` |
| `openstack_lb_provisioning_pending`, `openstack_lb_listener_provisioning_pending` | `…_provisioning_status{provisioning_status=~"PENDING_.*"}` |
| `openstack_lb_pool_members`, `openstack_lb_pool_members_up` | `count without (member_id)` and `sum without (member_id)` over `openstack_lb_member_up` |
| `openstack_floating_ip_associated` | `openstack_floating_ip_info{port_id!=""}` |
| `openstack_floating_ip_up` | `openstack_floating_ip_status{status="ACTIVE"}` |

Kept, although derivable in theory:

- `openstack_server_status`: the only series for a server in an unreachable cell, and `vm_state` can't be derived from it.
- The `*_up` and `*_ok` metrics of states that change often, and `openstack_volume_transitional`: stable label sets for availability math, and
  Cinder's list of transitional states.
- The per-volume snapshot and backup summaries: zero rows, sizes and timestamps.
- `openstack_exporter_data_age_seconds`: differs from the poll time while reused data is served.

### 0/1 metrics only for states that change often

Provisioning status and floating IP status stay put until the next API call, so their state metric keeps one label value while the state lasts,
and `for:` alerts work on it directly. Nobody computes availability over them, so they have no 0/1 metric.

### State metrics are named after their field

`openstack_server_task_active` was renamed `openstack_server_task_state`, so no metric needs special handling in dashboards. It still exists only
while a task runs, which keeps one series per idle server out of every scrape.

### A `DEGRADED` listener counts as up

The amphora driver sets a listener `DEGRADED` when it reaches its connection limit, and it still serves. `openstack_lb_listener_up` stays about
whether the listener accepts connections, and saturation is alerted on through `openstack_lb_listener_operating_status`.

### Aggregations use `without`

Example queries aggregate with `without (…)` rather than `by (…)` or a bare `count`. One Prometheus may hold many customer stacks, and `without`
keeps the labels vmagent adds, so each stack is compared with its own data and quota.

### Quota details

- Octavia returns limits only, so there is no `openstack_lb_quota_used`: usage is counted from series osmex already exports.
- Neutron's `reserved` is not exported: reservations exist only while a resource is being created.
- Unlimited (`-1`) limits and fields the API leaves out are omitted, never reported as 0.

### A backup's recovery point is its `data_timestamp`

For a backup taken from a snapshot, the data is as old as the snapshot, which is what a recovery-point alert must compare against. The last full
backup is exported too, because restoring an incremental backup needs the whole chain back to it.

## Polling

### Nova lists in full every 10th poll

Between full listings, `nova-servers` asks only for servers changed since the previous listing. The full listing bounds the two changes a delta
can miss to about 10 minutes, and resyncs after a failed poll and while a cell is down. The overlap of one interval covers clock differences with
Nova's database; merging a server twice is harmless.

### Octavia reuses only what the list can confirm

Status trees are reused only for settled `amphora` load balancers listed `ACTIVE` + `ONLINE` with an unchanged `updated_at`, plus a safety
refresh every 5 to 10 intervals. Other providers are fetched every poll, because only amphora was tested.

Alternatives considered and rejected:

- **Confirmed trees age the snapshot:** no. A tree the list confirms is current by design, not reused data.
- **`updated_at` has one-second resolution:** missing a change would need two complete changes plus osmex's list call within one second.
- **Skip `OFFLINE` or `ERROR` load balancers:** unverified, so they are fetched like any other unsettled tree.

### The Octavia listener list is kept

Each poll also calls `GET /v2/lbaas/listeners` to catch a listener that turns `ERROR` without changing its load balancer. With default settings,
the amphora failover that follows makes the change visible within about one poll anyway. The check is kept because it costs one request per poll,
and the case it fully covers, failovers stopped by `failover_threshold`, is a mass amphora failure, when accurate monitoring matters most.

### Neutron ports are not filtered by owner

Filtering `GET /v2.0/ports` by `device_owner=compute:<az>` would return only server ports and roughly halve the slowest call (see [measured API
cost](openstack-behaviour.md#measured-api-cost)). Neutron matches only exact values, so osmex would have to know every availability zone: from an
extra call with its own permission, from the `nova-servers` results, which couples two pollers, or from a flag. In every case a missed zone makes
server ports silently disappear, the kind of loss the design otherwise avoids. `-neutron-ports-interval` already lets an operator poll less
often.

### Cinder runs as three pollers

Volumes, snapshots and backups are independent data, so each has its own poller and fails, ages out and reports `api_up` on its own. A cloud
without the backup service shows only `cinder-backups` down. The copy pollers read the volume list only to decide which zero rows exist, so it
never ages their data.

### Cinder polls every 60 seconds

At 300 s, a new snapshot or backup could take about 6.5 minutes to appear. 60 s costs 3 list calls per project per minute instead of 0.6, about 4
more requests a second against Cinder across 100 projects.

### The credential expiry is polled hourly, outside any service

Application credentials are immutable, so the hourly read matters only after a failure, and there is no interval flag. The poller runs whenever
any service is enabled and belongs to none, so `-no-<service>` doesn't affect it and its failure affects no other poller.

## Configuration and security

### Unencrypted connections need `-insecure`

`OS_AUTH_URL` and every catalog endpoint must be `https://`, so the credential secret and tokens never cross the network in plain text. Unlike
openstackclient's `--insecure`, osmex's flag allows plain http and still verifies certificates.

### Redirects stay on the same scheme, host and port

Go's HTTP client keeps `X-Auth-Token` on a redirect to another host, and re-sends the token request body, with the secret, on a 307 or 308. osmex
refuses every other redirect before any request reaches the new location.

### An ambiguous region keeps the service enabled

With `-all`, a service without an endpoint for the configured region and interface is skipped. One with endpoints in several regions and no
`OS_REGION_NAME` stays enabled and fails with `code="catalog"`: that is a configuration mistake to surface through `api_up`, not an unavailable
service.

### `?module=` knows every module osmex could run

A known module that isn't running yet, before `-all` resolves or when its service is missing from the catalog, returns 200 with the global
series, so a split scrape job still sees `openstack_exporter_auth_up`. A module that can never run in the configuration, because of explicit
service flags or an exclusion, returns 400.

### Exclusions are `-no-<service>` flags

`-barbican=false` is accepted by Go's `flag` package, but can't be told apart from the default and couldn't override `-all`. An exclusion always
wins, and one that excludes nothing logs a warning instead of failing, because osmex exits only on invalid input.

### The secret comes from exactly one source

The secret is read from the systemd credential `application-credential-secret`, `OS_APPLICATION_CREDENTIAL_SECRET_FILE` or
`OS_APPLICATION_CREDENTIAL_SECRET`, and setting more than one is an error. osmex reads the systemd credential from `$CREDENTIALS_DIRECTORY`
itself instead of taking its path from an `Environment=` line, because systemd lets `EnvironmentFile=` override `Environment=`, and a variable
left in the environment file would silently replace the encrypted secret.

### The probes never require authentication

`/livez` and `/readyz` are exempt from basic authentication, matched by exact path. A Kubernetes probe sends no credentials, so requiring them
would turn a health check into a restart loop, and the probes report only whether osmex itself works. Mutual TLS can't exempt a path, because
the handshake refuses a client without a certificate before any path is known, so `-probe-address` serves the probes on a second, plain-HTTP
listener that serves nothing else.

### Requests re-read the password, at most every 30 seconds

A mounted Kubernetes secret is updated in place, so the password is re-read while osmex runs rather than only at startup. The re-read happens before
a request's credentials are checked, whether or not they match the password in memory: re-reading only after a rejection would let a client with the
old password keep access for as long as nobody sent different credentials. It is limited to once every 30 seconds across all requests: anyone who
can reach the port decides when a request arrives, and an unconditional re-read would make every request a file read. The certificate is checked for
changes on the same interval, when a connection arrives, rather than on a timer.

### Readiness is a startup latch

`/readyz` turns 200 once every module has finished its first poll, successfully or not, and never turns back. A not-ready exporter is taken out
of a Kubernetes Service exactly when its `api_up` and `auth_up` would explain an OpenStack outage, so readiness doesn't follow OpenStack health.
For the same reason, with `-all` it doesn't wait for a Keystone that keeps failing: the first failed authentication finishes startup. `/livez`
fails only when a module stops starting polls for 10 of its intervals, because a restart can fix a stalled poll loop and nothing else.

### Credential ids appear in `-debug` URLs

`-debug` logs request paths as they are, including the user and application credential ids in the `keystone-credential` call. Whoever runs
`-debug` already holds the credential, ids can't authenticate without the secret, and they help troubleshooting, so the README states this rather
than osmex redacting paths. Query values whose names suggest a secret are redacted, but `key` is not on that list, so OpenStack's `sort_key`
stays readable.

### Ids in request paths are not escaped

Server and load balancer ids put into paths are UUIDs generated by Nova and Octavia. A broken or malicious API could at most steer a request to
another path on the same endpoint, which already receives the project token on every request.
