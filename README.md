# osmex

osmex is a project-scoped OpenStack exporter for Prometheus. It runs in a customer's monitoring stack, polls the public OpenStack APIs with a read-only application credential,
and exposes the state of that project's servers, volumes, ports, floating IPs, load balancers and secrets for availability dashboards and alerts.

- Standard library only: a single static binary with no third-party code.
- Background polling: each module refreshes a pre-rendered snapshot on its own interval. Scrapes never call an OpenStack API, so API load does not grow with the number of scrapers.
- Metrics built for availability math: labels identify an object and never change during its lifetime; state lives in values or in `_info` metrics.
- A failing API never reports resources as down: the last good data is served for up to three poll intervals, and `openstack_exporter_api_up` drops to 0.

Why osmex works the way it does is in [docs/decisions.md](docs/decisions.md), and the OpenStack behaviour it relies on, with the evidence and
measured API cost, is in [docs/openstack-behaviour.md](docs/openstack-behaviour.md).

## Build

Requires Go 1.27.3 or newer; earlier 1.27 releases have known vulnerabilities.

Build with [Task](https://taskfile.dev/) and [GoReleaser](https://goreleaser.com):

```sh
task build
```

The native binary is written to `build/<os>_<arch>/osmex`, with release flags and version information.
See [Development](#development) for installation, running, checks and releases.

## Configuration

A **service** is an OpenStack service enabled with a flag such as `-nova`. Each service contributes one or more **poller modules** (or **modules**):
independent collection jobs with their own interval, snapshot and health metrics.

| Service | Modules |
| --- | --- |
| Nova | `nova-servers`, `nova-limits` |
| Cinder | `cinder`, `cinder-snapshots`, `cinder-backups`, `cinder-limits` |
| Neutron | `neutron-ports`, `neutron-fip`, `neutron-limits` |
| Octavia | `octavia`, `octavia-limits` |
| Barbican | `barbican` |

The shared `keystone-credential` module runs whenever any service is enabled. Module names are used by `?module=` and by the existing `poller` metric label
and log field. For example, `-nova` enables the Nova service and its two modules; `?module=nova-servers` selects just the server module's metrics for a scrape.

### Environment

The variables match the openrc that Horizon generates for an application credential.

| Variable | Required | Description |
| --- | --- | --- |
| `OS_AUTH_URL` | yes | Keystone URL, with or without `/v3`; must be `https://` unless `-insecure` is set |
| `OS_APPLICATION_CREDENTIAL_ID` | yes | Application credential id |
| `OS_APPLICATION_CREDENTIAL_SECRET` | one of these | Inline application credential secret; never logged |
| `OS_APPLICATION_CREDENTIAL_SECRET_FILE` | one of these | Path to a plaintext secret file, such as a container secret; secret contents are never logged |
| `OS_REGION_NAME` | no | Region for catalog endpoints; required when a service has endpoints in several regions |
| `OS_INTERFACE` | no | `public` (default), `internal` or `admin` |
| `OS_CACERT` | no | PEM CA bundle; when set it is the only trusted CA |

The secret is required and comes from exactly one source: `OS_APPLICATION_CREDENTIAL_SECRET`, `OS_APPLICATION_CREDENTIAL_SECRET_FILE`, or the systemd
credential `application-credential-secret` in `$CREDENTIALS_DIRECTORY`, which the example unit loads with `LoadCredentialEncrypted=`. While that credential is
loaded, setting either variable is an error. A file is read at startup and loses trailing CR/LF characters; a missing, unreadable, empty or whitespace-only
file, and anything but a regular file of at most 4 KiB, is an error. See [credential setup](docs/credentials.md#supply-the-secret-through-systemd-creds).

At startup osmex logs the Keystone URL (without any user information or query), the region and the interface. After the first successful
authentication it logs the project and domain the credential belongs to. Credential secrets and tokens are never logged; credential ids
appear only in `-debug` request URLs (see [Debugging HTTP requests](#debugging-http-requests)).

### Flags

No service is enabled by default; without service flags only the exporter's own metrics are served.

| Flag | Default | Description |
| --- | --- | --- |
| `-nova`, `-cinder`, `-neutron`, `-octavia`, `-barbican` | off | Enable a service |
| `-all` | off | Enable every service with an endpoint for `OS_REGION_NAME` and `OS_INTERFACE` (resolved once, after the first successful authentication); other services are skipped and logged. A service with endpoints in several regions and no `OS_REGION_NAME` stays enabled and logs an error |
| `-no-nova`, `-no-cinder`, `-no-neutron`, `-no-octavia`, `-no-barbican` | off | Exclude a service. An exclusion wins over `-all` and over the service's own flag, so `-all -no-barbican` enables every available service except Barbican. A `-no-` flag for a service that is not enabled has no effect and logs a warning |
| `-listen-address` | `127.0.0.1:9180` | Address for `/metrics`, `/livez` and `/readyz` |
| `-probe-address` | | Also serve `/livez` and `/readyz`, and nothing else, on this address over plain HTTP without authentication; see [Securing the endpoint](#securing-the-endpoint) |
| `-listen-tls-cert-file`, `-listen-tls-key-file` | | Serve HTTPS with this PEM certificate and key; set both or neither |
| `-listen-tls-client-ca-file` | | Require a client certificate signed by this PEM bundle; needs the certificate and key |
| `-listen-auth-username`, `-listen-auth-password-file` | | Require basic authentication with this username and the password in this file; set both or neither |
| `-nova-servers-interval` | `60s` | |
| `-nova-limits-interval` | `5m` | |
| `-cinder-interval` | `60s` | Volumes; raise it if the Cinder API is slow |
| `-cinder-snapshots-interval` | `-cinder-interval` | Volume snapshots |
| `-cinder-backups-interval` | `-cinder-interval` | Volume backups |
| `-cinder-limits-interval` | `5m` | Block storage quota |
| `-neutron-ports-interval` | `60s` | |
| `-neutron-fip-interval` | `5m` | |
| `-neutron-limits-interval` | `5m` | Network quota |
| `-octavia-interval` | `0` (auto) | Auto: 60 s up to the first threshold, 120 s up to the second, 300 s above |
| `-octavia-auto-thresholds` | `20,100` | Load balancer counts for the auto interval |
| `-octavia-limits-interval` | `5m` | Load balancer quota |
| `-barbican-interval` | `1h` | |
| `-max-concurrent-requests` | `4` | Concurrent API requests across all modules |
| `-request-timeout` | `30s` | Timeout of a single HTTP request |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error` |
| `-log-format` | `text` | `text` or `json` |
| `-debug` | off | Log every HTTP exchange with OpenStack, see [Debugging HTTP requests](#debugging-http-requests); implies `-log-level debug` |
| `-insecure` | off | Allow unencrypted (`http://`) connections to Keystone and service endpoints. Certificates of `https` endpoints are still verified |
| `-version` | | Print the version and exit |

Modules of an explicitly enabled service missing from the catalog keep running with `openstack_exporter_api_up` 0 and errors counted under `code="catalog"`.

### Debugging HTTP requests

`-debug` logs every HTTP exchange with Keystone and the service APIs, redirects included, at debug level in the `-log-format` chosen:

```sh
osmex -nova -debug
```

Each exchange logs an `http request` line when it is sent and an `http response` line when its body has been read, or when it fails. They
share a `request_id` and identify the module in the `poller` field (token requests outside a poll have none), with the method, URL, status,
content type and length, bytes read, any read error, time to response headers and total duration, plus the `Location` of a redirect. Headers and bodies are
never logged, so tokens and the credential secret stay out of the log; URLs lose any user information and fragment, and query values whose names suggest a
token, signature, secret, password or credential are replaced by `REDACTED`. Paths are logged as they are, so the URLs show resource ids,
including the project id and, in the `keystone-credential` call, the user and application credential ids: these identify the credential but
cannot authenticate without its secret. Each API call adds two lines, so use `-debug` for
investigation rather than in normal operation.

## Endpoints

- `/metrics`: all metrics. `?module=<name>` (repeatable) limits the output to the named modules, for example `?module=nova-servers&module=nova-limits`.
  This filters the response; all enabled modules continue polling.
  The global series (`build_info`, `auth_up`, runtime metrics) are always included. Any module of an enabled service is accepted, including one that
  has not started yet, such as with `-all` before the first successful authentication or when the service is missing from the catalog, and so is
  `keystone-credential` when any service is enabled. Other names return 400.
- `/livez`: 200 while every module keeps polling; 503 when a module has not started a poll for 10 of its intervals. A failing OpenStack API or
  Keystone does not fail it: restarting would not help, and `api_up` and `auth_up` report those failures.
- `/readyz`: 503 until osmex has finished starting, then 200 until it exits. Starting is finished when every module has finished its first
  poll, successfully or not; with `-all` it waits for the modules to start after the first successful authentication, or finishes as soon as
  the first authentication fails. It never goes back to 503, so OpenStack trouble never takes the exporter out of service.

`/livez` and `/readyz` carry no OpenStack data and never require authentication.

### Securing the endpoint

osmex serves plain HTTP without authentication by default, on the loopback address. Its metrics name the project's servers, volumes, addresses and
load balancers, so when the scrape crosses a network you don't control, TLS is required: osmex's own, or a TLS-terminating proxy in front of osmex
listening only where that proxy can reach it. Authentication comes on top of TLS, never instead of it. Basic authentication over plain HTTP sends
the password, readable, with every scrape, however securely it is stored, so use it without TLS only on a network you trust, such as loopback behind
a proxy; osmex logs a warning when it is configured that way.

```sh
# HTTPS
osmex -nova -listen-address=:9180 -listen-tls-cert-file=/etc/osmex/tls.crt -listen-tls-key-file=/etc/osmex/tls.key

# HTTPS with mutual TLS: the scraper must present a certificate signed by ca.crt, and is refused during the handshake otherwise
osmex -nova -listen-address=:9180 -listen-tls-cert-file=/etc/osmex/tls.crt -listen-tls-key-file=/etc/osmex/tls.key \
  -listen-tls-client-ca-file=/etc/osmex/ca.crt

# HTTPS with basic authentication
osmex -nova -listen-address=:9180 -listen-tls-cert-file=/etc/osmex/tls.crt -listen-tls-key-file=/etc/osmex/tls.key \
  -listen-auth-username=prometheus -listen-auth-password-file=/etc/osmex/scrape-password
```

- Every file is read at startup, so a wrong path or an empty password file stops osmex before it serves. A private key or password file readable
  beyond its owner logs a warning; group-readable is fine when the group is one osmex runs in, as with a Kubernetes secret mounted under the pod's
  `fsGroup`.
- TLS 1.2 is the minimum. The certificate and key are checked for changes at most every 30 seconds when a connection arrives, and reloaded when
  they change, so a renewal by cert-manager or another tool needs no restart. A pair that fails to load, such as a half-written one, keeps the
  previous certificate in use.
- The password file loses trailing CR/LF characters. The first request at least 30 seconds after the last read re-reads the file before its
  credentials are checked, so a rotated Kubernetes secret is picked up without a restart: from that request on, the new password is accepted and
  the old one refused. Until then, the old password still works and the new one is refused. A file that can't be read keeps the previous password.
  Credentials are compared in constant time, and neither the password nor a failed attempt's credentials are logged.
- A client certificate is required by the listener, not by a path, so a Kubernetes probe without one is refused with mutual TLS. Serve the
  probes on a separate plain-HTTP listener with `-probe-address`; it serves only `/livez` and `/readyz` and returns 404 for anything else.
- Under systemd, the private key and password are stored encrypted with `systemd-creds` and loaded with `LoadCredentialEncrypted=`, whose copies are
  made at start, so a renewed key or rotated password reaches osmex only after `systemctl restart osmex`. See
  [the scrape password and TLS key](docs/credentials.md#the-scrape-password-and-tls-key-under-systemd) and the commented example in
  [deploy/systemd/osmex.service](deploy/systemd/osmex.service).

## Services and metrics

All metrics are gauges except those ending in `_total`. Sizes are in bytes and timestamps in Unix seconds.

Every state field osmex reads from an API has a state metric, and states that change often also have 0/1 metrics:

- **State metric:** named after the field, with the current state in a label of the same name, for example
  `openstack_lb_member_operating_status{…,operating_status="DRAINING"} 1`. One series exists per object and is replaced when the state changes,
  so use it for display, filtering and investigation. The value is 1, except `openstack_server_power_state`, whose value is Nova's numeric code.
  `openstack_server_task_state` exists only while a task runs, so idle servers add no series.
- **0/1 metrics** (`*_up`, `*_ok`, `*_transitional`): derived from the state, with label sets that never change. Use these for
  availability math and alerts.

States that stay put until the next API call, such as provisioning status and floating IP status, have only the state metric, because its label
doesn't change while the state lasts: alert on it directly, for example `openstack_lb_provisioning_status{provisioning_status="ERROR"} == 1`.
A floating IP's association is the `port_id` label of `openstack_floating_ip_info`.

### Nova

| Module | Interval | Calls |
| --- | --- | --- |
| nova-servers | 60 s | `GET /servers/detail` (microversion 2.96), every 10th poll in full and otherwise with `changes-since` (see below), then `GET /servers/{id}/os-instance-actions` for new or changed servers |
| nova-limits | 300 s | `GET /limits` |

| Metric | Labels | Value |
| --- | --- | --- |
| `openstack_server_info` | id, name, flavor, availability\_zone, host\_id, image\_id | 1 |
| `openstack_server_up` | id | 1 if status ACTIVE and power state RUNNING, else 0 |
| `openstack_server_status` | id, status | 1 for the current status |
| `openstack_server_vm_state` | id, vm\_state | 1 for the current VM state |
| `openstack_server_power_state` | id, power\_state | Nova power state code; `power_state` names it (0 `nostate`, 1 `running`, 3 `paused`, 4 `shutdown`, 6 `crashed`, 7 `suspended`) |
| `openstack_server_task_state` | id, task\_state | 1 while a task runs; absent when idle |
| `openstack_server_launched_timestamp_seconds` | id | `OS-SRV-USG:launched_at` |
| `openstack_server_updated_timestamp_seconds` | id | `updated` |
| `openstack_server_created_timestamp_seconds` | id | `created` |
| `openstack_server_locked` | id | 1 if locked |
| `openstack_server_fault_timestamp_seconds` | id, code | fault time; only in ERROR status |
| `openstack_server_actions_total` | id, action | instance actions by type (reboot, stop, live-migration, evacuate, …). Once a server's history is known, `evacuate`, `live-migration`, `migrate`, `resize`, `rebuild`, `reboot`, `stop` and `start` start at 0 so `increase()` counts their first occurrence; other actions appear when they first happen |
| `openstack_server_vcpus`, `openstack_server_memory_bytes`, `openstack_server_root_disk_bytes` | id | from the embedded flavor |
| `openstack_compute_quota_limit`, `openstack_compute_quota_used` | resource (`cores`, `ram_bytes`, `instances`) | quota; unlimited limits and fields Nova leaves out are omitted |

`host_id` is a per-project hash of the compute host; a change means the server moved.

Between full listings, nova-servers asks only for the servers changed since the previous listing (`changes-since`, from one interval before
it started, to cover clock differences with Nova's database), and keeps the last known record of every other server. Nova updates a server's
`updated` time on every change to its record, and returns deleted and soft-deleted servers in such a listing, as `DELETED` or `SOFT_DELETED`; both are removed, as a full listing
leaves them out too. A full listing
runs at startup, after a failed poll, and every 10th poll. It catches the two changes a delta can miss, so they show within 10 polls:

- a server deleted while it is still being scheduled, which Nova removes without updating it;
- a new fault on a server already in ERROR, which Nova stores apart from the server record, so `openstack_server_fault_timestamp_seconds` can
  lag.

Servers in an unreachable Nova cell report only `openstack_server_status{status="UNKNOWN"}`, and keep their instance action counts for when the
cell is back. Nova returns these records only for a listing without filters or paging parameters, so osmex sends no `limit` and Nova pages at its own
maximum (`[api] max_limit`, 1000 by default). In a project with more servers than that, servers on later pages are left out while their cell is down.
A delta can't see a cell go down, so its servers keep their last record until the next full listing; while any server is reported as
`UNKNOWN`, every poll is a full listing, so the cell's return shows at once.

### Cinder

Four independent modules, each making one call at microversion 3.71:

- `cinder`, every 60 s: `GET /volumes/detail`, for the volume metrics.
- `cinder-snapshots`, at the `cinder` interval unless set: `GET /snapshots/detail`, for the snapshot metrics.
- `cinder-backups`, at the `cinder` interval unless set: `GET /backups/detail`, for the backup metrics.
- `cinder-limits`, every 300 s: `GET /limits` for the project's block storage quota. A full quota makes new volumes, snapshots and backups fail.

Each fails on its own. A cloud without the backup service, for example, shows `openstack_exporter_api_up{poller="cinder-backups"} 0` while snapshot and
volume metrics stay complete. Their per-volume summaries cover every volume from the `cinder` module's latest list, with zeros for volumes without
copies, as long as that list is at most three `cinder` intervals old; otherwise only volumes that have copies appear. The volume list only decides
which zero rows exist, so it never ages the snapshot and backup data, and those zero rows reflect the volume list as of each snapshots or backups poll.
At startup the snapshots and backups modules wait up to 30 s for the first volume poll, so their first output already has the zero rows.

| Metric | Labels | Value |
| --- | --- | --- |
| `openstack_volume_info` | id, name, volume\_type, availability\_zone, bootable | 1 |
| `openstack_volume_status` | id, status | 1 for the current status |
| `openstack_volume_ok` | id | 0 in an `error*` state, else 1 |
| `openstack_volume_transitional` | id | 1 while creating, attaching, detaching, extending, backing-up, restoring, … |
| `openstack_volume_size_bytes` | id | volume size |
| `openstack_volume_attachment_info` | id, server\_id, device | 1 per attachment |
| `openstack_volume_snapshot_info` | id, name, volume\_id | 1 per snapshot, in any status |
| `openstack_volume_snapshot_status` | id, status | 1 for the snapshot's current status |
| `openstack_volume_backup_info` | id, name, volume\_id, incremental | 1 per backup, in any status; `incremental` is `true` or `false` |
| `openstack_volume_backup_status` | id, status | 1 for the backup's current status |
| `openstack_volume_snapshots`, `openstack_volume_snapshots_failed`, `openstack_volume_snapshots_size_bytes` | volume\_id | available snapshots, failed snapshots, total size |
| `openstack_volume_last_snapshot_timestamp_seconds` | volume\_id | newest available snapshot |
| `openstack_volume_backups`, `openstack_volume_backups_failed`, `openstack_volume_backups_size_bytes` | volume\_id | available backups, failed backups, total size (source volume size) |
| `openstack_volume_backup_in_progress` | volume\_id | 1 while a backup is being created |
| `openstack_volume_last_backup_timestamp_seconds` | volume\_id | point in time captured by the newest available backup (`data_timestamp`: for a backup taken from a snapshot, the snapshot's time) |
| `openstack_volume_last_full_backup_timestamp_seconds` | volume\_id | the same for the newest available full backup; restoring an incremental backup needs every backup back to this one |
| `openstack_volume_quota_limit` | resource (`volumes`, `snapshots`, `backups`, `volume_bytes`, `backup_bytes`) | quota limit; unlimited limits are omitted. `volume_bytes` covers volumes and snapshots together |
| `openstack_volume_quota_used` | resource | quota usage; omitted when Cinder's policy hides usage |

Snapshots and backups of deleted volumes keep their `volume_id`, so orphaned copies stay visible. The per-volume summaries (available, failed, in
progress) suit alerts; the per-copy `_info` and `_status` metrics show which copy is affected and join to the volume through `volume_id`.

### Neutron

| Module | Interval | Calls |
| --- | --- | --- |
| neutron-ports | 60 s | `GET /v2.0/ports` (server ports only: `device_owner` starting with `compute:`) |
| neutron-fip | 300 s | `GET /v2.0/floatingips` |
| neutron-limits | 300 s | `GET /v2.0/quotas/{project_id}/details` for the project's network quota. A full quota makes new ports, floating IPs or security groups fail |

| Metric | Labels | Value |
| --- | --- | --- |
| `openstack_port_info` | id, name, device\_id, network\_id | 1; `device_id` is the server id |
| `openstack_port_up` | id | 1 if ACTIVE |
| `openstack_port_status` | id, status | 1 for the current status (ACTIVE, DOWN, BUILD, ERROR) |
| `openstack_floating_ip_info` | id, floating\_ip\_address, port\_id | 1 |
| `openstack_floating_ip_status` | id, status | 1 for the current status (ACTIVE, DOWN, ERROR) |
| `openstack_network_quota_limit` | resource (`floating_ips`, `ports`, `security_groups`, `security_group_rules`, `routers`, `networks`, `subnets`) | quota limit; unlimited limits and resources Neutron leaves out are omitted |
| `openstack_network_quota_used` | resource | quota usage, without reservations held while resources are being created |

### Octavia

Module `octavia`: `GET /v2/lbaas/loadbalancers` and `GET /v2/lbaas/listeners`, then the status trees (`GET /v2/lbaas/loadbalancers/{id}/status`)
that may have changed. An `amphora` load balancer that is `ACTIVE` + `ONLINE` with the same `updated_at` as when its tree was fetched keeps that
tree: with the amphora provider, Octavia bumps `updated_at` on every configuration change and whenever the load balancer's own operating status
changes. The listener list must also show the same listeners with the same statuses as the tree, because Octavia marks a listener missing from an
amphora heartbeat `ERROR` without changing the load balancer; if the listener call fails, every tree is fetched. This applies only to a tree whose
listeners and pools are `ONLINE` or `NO_MONITOR` and whose members are `ONLINE`, `NO_MONITOR` or `DRAINING`: a new listener or member is
`OFFLINE` until its first health report and then turns `ONLINE` without any change in the lists. Every other load balancer
is fetched on every poll: one that is not `ONLINE`, because a member failing or recovering on a load balancer that is already `DEGRADED` doesn't
show in the list; one with an `OFFLINE`, `ERROR` or `DEGRADED` listener, or an `OFFLINE` or `ERROR` pool or member; one with another provider or
without `updated_at`, because its list entry isn't known to reflect member changes. As a safety net, each poll also re-fetches the oldest
unchanged trees that are at least 5 intervals old, at most a tenth of them, so each is refreshed every 5 to 10 intervals (up to about 15 for the
first round after startup).

Status calls stop being started once half the poll's time is used, so a slow API cannot run the poll into its deadline. Load balancers are fetched
least recently requested first, so whatever a poll leaves out comes earlier in the next one, and a load balancer whose status call fails moves behind
the others instead of starving them. A tree left out stays current if the list confirms it as above; any other tree is kept for at most three
intervals.

Module `octavia-limits`, every 300 s: `GET /v2/lbaas/quotas/{project_id}` for the project's load balancer quota. Octavia fills in its configured
defaults for quotas the project doesn't set, and those defaults are unlimited unless the cloud changes them. Octavia returns limits only; usage is
counted from the `octavia` module's series (see the example queries). It fails on its own, without affecting the load balancer metrics.

| Metric | Labels | Value |
| --- | --- | --- |
| `openstack_lb_info` | id, name, provider, availability\_zone | 1 |
| `openstack_lb_up` | id | 1 if operating status ONLINE or NO\_MONITOR |
| `openstack_lb_operating_status` | id, operating\_status | 1 for the current operating status |
| `openstack_lb_provisioning_status` | id, provisioning\_status | 1 for the current provisioning status |
| `openstack_lb_listener_info` | lb\_id, listener\_id, name | 1 |
| `openstack_lb_listener_up` | lb\_id, listener\_id | 1 if operating status ONLINE, NO\_MONITOR or DEGRADED: the amphora driver reports a listener DEGRADED when it has reached its connection limit, which still serves |
| `openstack_lb_listener_operating_status` | lb\_id, listener\_id, operating\_status | 1 for the current operating status (DEGRADED at the connection limit, OFFLINE when disabled, ERROR when failing) |
| `openstack_lb_listener_provisioning_status` | lb\_id, listener\_id, provisioning\_status | 1 for the current provisioning status. Octavia sets a listener PENDING\_\* only together with its load balancer, so `openstack_lb_provisioning_status` shows pending changes |
| `openstack_lb_pool_up` | lb\_id, pool\_id | 1 if ONLINE or NO\_MONITOR |
| `openstack_lb_pool_operating_status` | lb\_id, pool\_id, operating\_status | 1 for the current operating status |
| `openstack_lb_member_info` | member\_id, pool\_id, name, address, port | 1 |
| `openstack_lb_member_up` | member\_id, pool\_id | 1 if ONLINE or NO\_MONITOR |
| `openstack_lb_member_operating_status` | member\_id, pool\_id, operating\_status | 1 for the current operating status (DRAINING during maintenance, ERROR or OFFLINE when failing) |
| `openstack_lb_quota_limit` | resource (`load_balancers`, `listeners`, `pools`, `members`, `health_monitors`) | quota limit; unlimited limits are omitted |

### Barbican

Module `barbican`, every hour: `GET /v1/secrets`. Only secrets with an expiration produce series; payloads are never read.

| Metric | Labels | Value |
| --- | --- | --- |
| `openstack_secret_info` | id, name, secret\_type, algorithm | 1 |
| `openstack_secret_expiration_timestamp_seconds` | id | time after which the secret can no longer be retrieved |

### Exporter

When any service is enabled, module `keystone-credential` reads the exporter's own application credential every hour:
`GET /v3/users/{user_id}/application_credentials/{id}` on the catalog's `identity` endpoint. Application credentials are immutable, so the hourly
read only matters after a failure. It fails on its own, for example when access rules don't allow the call, without affecting the other service modules.

| Metric | Labels | Value |
| --- | --- | --- |
| `openstack_exporter_build_info` | version, goversion | 1 |
| `openstack_exporter_auth_up` | | 1 while a valid Keystone token is held; present when a service is enabled |
| `openstack_exporter_credential_expiration_timestamp_seconds` | | time after which the application credential can no longer authenticate; absent when it never expires |
| `openstack_exporter_api_up` | poller | 1 if the last poll succeeded |
| `openstack_exporter_last_success_timestamp_seconds` | poller | time of the last successful poll |
| `openstack_exporter_data_age_seconds` | poller | age of the oldest data being served, including load balancer status data and instance action counts reused after their calls failed; a module stops serving data older than three intervals, and drops reused data that reaches that age without dropping the rest of its last poll |
| `openstack_exporter_poll_duration_seconds` | poller | duration of the last poll |
| `openstack_exporter_api_errors_total` | poller, code | failed API requests by HTTP status, or `auth`, `catalog` (no endpoint for the configured region and interface, or endpoints in several regions with `OS_REGION_NAME` unset), `insecure` (a plain-http endpoint refused without `-insecure`), `redirect` (a redirect to another host, port or scheme, refused so the token and secret never leave the endpoint), `timeout`, `network`, `decode` (a malformed response, including pagination links that can't be followed and pagination that loops or exceeds 10,000 pages), `other` (anything else). Requests cancelled by shutdown are not counted. Every code except an HTTP status, and HTTP 401, 403, 404, 429, 500, 502, 503 and 504, starts at 0 for every module so `increase()` counts its first occurrence |

### Exporter process

The exporter's own Go runtime and process figures, read at scrape time under the conventional Prometheus Go client names. They are always
present, including when no service is enabled.

| Metric | Value |
| --- | --- |
| `process_start_time_seconds` | when the exporter started; uptime is `time() - process_start_time_seconds` |
| `go_goroutines` | goroutines |
| `go_threads` | OS threads created by the Go runtime |
| `go_sched_gomaxprocs_threads` | GOMAXPROCS |
| `go_logical_cpus` | logical CPUs usable by the process |
| `go_memstats_heap_alloc_bytes` | heap bytes allocated and still in use |
| `go_memstats_heap_inuse_bytes` | heap bytes in use, including span space reserved for live objects' size classes |
| `go_memstats_stack_inuse_bytes` | goroutine stack memory |
| `go_memstats_sys_bytes` | memory the Go runtime has obtained from the OS; it does not shrink when memory is returned |
| `go_memstats_heap_objects` | live heap objects |
| `process_cpu_seconds_total` | user and system CPU time; Linux only |
| `process_resident_memory_bytes` | resident memory as the OS counts it, which is what limits such as systemd's `MemoryMax` apply to; Linux only |

Peaks come from PromQL rather than extra series, for example `max_over_time(go_memstats_heap_alloc_bytes[1d])`.

## Example queries

Aggregations use `without` rather than `by`, so alerts keep the labels vmagent adds (job, instance, customer) and can be routed per stack.

```promql
# availability per server, 30 days
avg_over_time(openstack_server_up[30d]) * on(id) group_left(name) openstack_server_info

# guest stopped or crashed from inside, not via the API
openstack_server_power_state{power_state!="running"} and on(id) openstack_server_vm_state{vm_state="active"}

# evacuations in the last 30 days
increase(openstack_server_actions_total{action="evacuate"}[30d]) > 0

# server moved host in the last hour
count without (host_id) (count without (name, flavor, availability_zone, image_id) (count_over_time(openstack_server_info[1h]))) > 1

# task stuck (alert with for: 30m)
openstack_server_task_state == 1

# no available backup, or newest successful backup older than 25 hours (for volumes expected to have backups)
(time() - openstack_volume_last_backup_timestamp_seconds > 25 * 3600)
  or (openstack_volume_backups == 0)

# newest full backup older than 8 days (long incremental chain)
time() - openstack_volume_last_full_backup_timestamp_seconds > 8 * 86400

# block storage quota above 90%
openstack_volume_quota_used / openstack_volume_quota_limit > 0.9

# network quota above 90%
openstack_network_quota_used / openstack_network_quota_limit > 0.9

# volume stuck in a transitional state (alert with for: 30m)
openstack_volume_transitional == 1

# pool with members down
count without (member_id) (openstack_lb_member_up == 0) > 0

# listener down (alert with for: 5m)
openstack_lb_listener_up == 0

# listener at its connection limit
openstack_lb_listener_operating_status{operating_status="DEGRADED"} == 1

# load balancer and member quota above 90%, each stack against its own quota
count without (id, name, provider, availability_zone) (openstack_lb_info)
  / ignoring(resource) openstack_lb_quota_limit{resource="load_balancers"} > 0.9
count without (member_id, pool_id, name, address, port) (openstack_lb_member_info)
  / ignoring(resource) openstack_lb_quota_limit{resource="members"} > 0.9

# server running but its port down
(openstack_port_up == 0) * on(id) group_left(device_id) openstack_port_info
  and on(device_id) label_replace(openstack_server_up == 1, "device_id", "$1", "id", "(.*)")

# secret becomes unretrievable within 14 days
openstack_secret_expiration_timestamp_seconds - time() < 14 * 86400

# module failing (alert with for: 10m)
openstack_exporter_api_up == 0

# exporter cannot authenticate
openstack_exporter_auth_up == 0

# application credential expires within 14 days
openstack_exporter_credential_expiration_timestamp_seconds - time() < 14 * 86400

# Octavia API slow: polls reach the status-call cutoff at half the interval (here the default 60 s)
openstack_exporter_poll_duration_seconds{poller="octavia"} > 30

# Octavia API calls timing out
increase(openstack_exporter_api_errors_total{poller="octavia",code="timeout"}[15m]) > 0

# load balancer data getting old: polls failing, or status calls falling behind (here twice the default 60 s)
openstack_exporter_data_age_seconds{poller="octavia"} > 2 * 60
```

osmex runs one exporter per customer stack, so these queries also work as a probe of the Octavia API: aggregated across all
stacks, they show whether Octavia as a whole is struggling or only one project.

## Grafana

`openstack_server_power_state` carries the state twice: the `power_state` label names it, and the value is Nova's code. When the state
changes, the series ends and a new one starts, like `openstack_server_status`. Use `openstack_server_up` for availability and alerts.

To show power state over time in a State timeline panel:

| View | Query | Legend |
| --- | --- | --- |
| One server, one row per state | `openstack_server_power_state{id="$id"}` | `{{power_state}}` |
| One server, one continuous row | `max by (id) (openstack_server_power_state{id="$id"})` | the server name |
| All servers, one row each | `max by (id) (openstack_server_power_state) * on(id) group_left(name) openstack_server_info` | `{{name}}` |

`max by (id)` drops the label, which joins the pieces into one continuous series per server. Add these value mappings to the continuous-row
panels so each code shows its name:

| Value | Display text | Colour |
| --- | --- | --- |
| 0 | nostate | light grey |
| 1 | running | green |
| 3 | paused | yellow |
| 4 | shutdown | grey |
| 6 | crashed | red |
| 7 | suspended | purple |

## Installation with systemd

1. Create an application credential in the customer project: see [docs/credentials.md](docs/credentials.md).
2. Install the binary from a release archive (the Linux archives also contain the files in `deploy/systemd/`): `install -m 0755 osmex /usr/local/bin/osmex`.
3. Create `/etc/osmex/osmex.env` from [deploy/systemd/osmex.env.example](deploy/systemd/osmex.env.example), owned by root with mode 0600, with non-secret settings only.
4. Encrypt the application credential secret with `systemd-creds` on the target host: see [credential setup](docs/credentials.md#supply-the-secret-through-systemd-creds).
   The example unit loads it with `LoadCredentialEncrypted=` and fails to start without it.
5. Install [deploy/systemd/osmex.service](deploy/systemd/osmex.service) to `/etc/systemd/system/`, adjust the service flags in `ExecStart`, then run
   `systemctl daemon-reload && systemctl enable --now osmex`.
6. Point vmagent at `http://127.0.0.1:9180/metrics` with a 60 s scrape interval. To scrape over the network, see
   [Securing the endpoint](#securing-the-endpoint).

## Kubernetes deployment

Releases publish `ghcr.io/krisiasty/osmex` for Linux amd64 and arm64. A version tag such as `v1.2.0` selects both platforms;
Kubernetes pulls the matching architecture. Stable releases also update the minor tag (`v1.2`) and `latest`; prereleases only publish their full version tag.
The image uses Debian 13 distroless, pinned by digest, and runs as UID/GID `65532:65532`. It contains the binary and the base image, with no shell.

The [Kubernetes example](deploy/kubernetes/osmex.yaml) serves HTTPS metrics with basic authentication and mounts the OpenStack credential secret,
scrape password and TLS private key from Kubernetes Secrets as read-only files. Optional
[ExternalSecret mappings](deploy/kubernetes/external-secrets.yaml) synchronize them from an existing external secret store; the image and workload YAML contain no secret values.

See [docs/kubernetes.md](docs/kubernetes.md) for permissions, secret-store integration, scraping and rotation. Mounted scrape passwords and server TLS pairs reload
while osmex runs; rotating the OpenStack application credential requires a pod rollout. The systemd deployment continues to use
[systemd-creds](docs/credentials.md#supply-the-secret-through-systemd-creds).

## Development

### Tools

| Tool | Needed for | Required | Repository |
| --- | --- | --- | --- |
| Go 1.27.3 or newer | building and testing | yes | [go.dev/dl](https://go.dev/dl/) |
| Task | every `task` command | yes | [go-task/task](https://github.com/go-task/task) |
| GoReleaser v2 | `task build`, `task check` and `task release`; its built-in [ko](https://github.com/ko-build/ko) integration builds the container images, so ko needs no separate install | yes | [goreleaser/goreleaser](https://github.com/goreleaser/goreleaser) |
| golangci-lint v2 | `task lint` and `task check` | yes | [golangci/golangci-lint](https://github.com/golangci/golangci-lint) |
| gohawk | `task lint` and `task check` | yes | [kojah/gohawk](https://github.com/kojah/gohawk) |
| govulncheck | `task check` | yes | [golang/vuln](https://github.com/golang/vuln) |
| GitHub CLI | the token for `task release` when `GITHUB_TOKEN` is not set | no | [cli/cli](https://github.com/cli/cli) |
| crane | `task base-image` and `task release`, to check the pinned base image digest; also handy for inspecting published images | for releases | [google/go-containerregistry](https://github.com/google/go-containerregistry/tree/main/cmd/crane) |

Docker is not needed for any of them.

### Tasks

The [Taskfile](Taskfile.yml) provides shortcuts using [go-task](https://taskfile.dev/). Run `task` to list them:

```sh
task check             # Go modules, release configuration, lint, vulnerabilities, and race tests
task build             # build the native binary in build/ with release flags
task install           # build the native binary if needed, then install it
task run               # run the installed binary with -all (OS_* variables from the environment)
task run -- -nova      # run it with other flags instead of -all
task test              # run all tests
task test -- -race     # run tests with the race detector
task test -- -run TestName -count=1 # run matching tests without cached results
task lint              # run golangci-lint (including govet) and gohawk
task lint -- -v        # pass verbose output to golangci-lint, then run gohawk
task base-image        # check that the pinned container base image is still current
task release           # check and publish tagged GitHub archives and GHCR container images
```

`task check` runs `go mod tidy -diff`, `go mod verify`, `goreleaser check`, `task lint`, `govulncheck ./...`, and `task test` with `-race`
in sequence, stopping on failure. The test task is reused, so tests run once with the race detector. Lint runs golangci-lint (which includes govet) and gohawk.
The tidy check reports differences without modifying `go.mod`. Check requires GoReleaser, golangci-lint, gohawk, and govulncheck on `PATH`;
the vulnerability scan needs access to the Go vulnerability database.

`task build` uses GoReleaser to build only the current platform, with the release flags and version, commit, and build date. Native output goes in
`build/<os>_<arch>/osmex`; a generated local configuration puts GoReleaser's intermediate output in `build/dist/`, keeping release artifacts in `dist/` intact.
Compilation is skipped when the binary exists and its inputs are not newer; the inputs include Go source, module files, build configuration and Git refs.
Use `task --force build` to force a rebuild.

`task install` builds if necessary and installs the native binary as `/usr/local/bin/osmex`, using `sudo` if the directory is not writable.
The privilege explanation is part of sudo's password prompt, so it appears only when authentication is requested.
Override the install directory with `task install INSTALL_DIR=/path/to/bin`. `task run` executes the installed binary without building or installing;
use the same `INSTALL_DIR` override when running from a custom location. It defaults to `-all`, or forwards the flags after `--` instead.

`task release` requires HEAD to be exactly on a tag and the working tree to be clean, including untracked files. It checks that the tag is on `origin`
at the same commit and that a GitHub token is available. It then runs `task base-image` and `task check` before asking for confirmation and publishing
archives to GitHub and multi-platform images to GHCR. The token also needs permission to publish packages, as described below.
Run `task base-image` before tagging as well: a stale base image needs a commit that updates the pin, and the tag has to point to that commit.
Without a terminal the prompt cancels the release unless `task -y` is used. Release never installs a binary or falls back to a snapshot.

### Release builds

The [GoReleaser configuration](.goreleaser.yaml) is adapted from
[videocheck](https://github.com/krisiasty/videocheck/blob/main/.goreleaser.yaml).
It builds binaries for macOS on arm64 and Linux on amd64 and arm64, and embeds the version shown by `-version` and in `openstack_exporter_build_info`.
`-version` also prints the commit and the build date, for example `osmex v1.2.0 (commit 1a2b3c…, built 2026-10-24T18:00:00Z)`. Builds without these flags fall back to the
commit that Go records when building from a checkout, and show `unknown` for anything missing.
Releases include tar.gz archives with the README, the license, the credentials guide, and SHA-256 checksums.
Linux archives also contain the systemd unit and environment example from `deploy/systemd/`.
GoReleaser's built-in [Ko integration](https://goreleaser.com/customization/package/ko/) builds and publishes the Linux images directly to GHCR.
It inherits the existing `linux` build's main package, static-build environment and version flags. It rebuilds the binary using those settings;
the Go build cache avoids recompiling unchanged packages. Docker, Buildx and a separate `ko` executable are not required.
The base image is pinned by digest in `.goreleaser.yaml` and nothing updates it automatically. `task base-image` compares the pin with the digest its tag
points to now, using crane, and fails with the replacement value when they differ; `task release` runs it first.

Validate the configuration and build a local snapshot of the release archives without publishing:

```sh
goreleaser check
goreleaser release --snapshot --clean
```

Container image publishing is disabled in snapshot mode. Images are built and pushed during a normal tagged release through GoReleaser's Ko integration.
`task build` continues to build only the native binary.
Artifacts are written to `dist/`, which is ignored by Git. Snapshot and release commands with `--clean` replace that directory.

### Publishing container images from macOS

A tagged release uses `goreleaser release --clean`, or `task release`, with a GitHub token for `krisiasty/osmex`.
`task release` uses `GITHUB_TOKEN` when set and otherwise the GitHub CLI login (`gh auth token`). Ko also authenticates to GHCR with that token;
no separate registry login or Docker installation is required. Supply a token from your secret store or existing CLI login with both permission to
create releases and `write:packages` permission to publish the container package. A classic personal access token supports these permissions.
After the first publish, set the GHCR package's visibility to public to allow anonymous pulls.

The macOS Homebrew cask is generated in `dist/`; tap publishing remains disabled until its release credentials are configured.
Removing `skip_upload` from the cask in `.goreleaser.yaml` also requires a token with write access to `krisiasty/homebrew-tap`.
