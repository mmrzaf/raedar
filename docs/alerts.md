# Alerts

Raedar alerts on a short list of conditions that mean something is wrong or about to be. Each condition is a **rule**. When a rule's condition holds for a **subject** (a route, a slot, a disk, a unit, a domain, a peer), Raedar opens an **incident** and sends one message. When the condition stops holding, it resolves the incident and sends one more.

## Incidents

| Property | Meaning |
|---|---|
| Rule | Which rule fired, for example `route.errors`. |
| Subject | What it fired for, for example `shop@docker`, `gitman / web #1`, `/` (a filesystem), `postgres-backup.service`, `shop.example.com`. |
| Severity | `warning` or `critical`. |
| Opened, resolved | When the condition started holding, and when it stopped. |
| Summary and details | The values that triggered it, the baseline, and context such as a recent deploy or the endpoint involved. |

Lifecycle:

- **Open.** A persistent rule opens an incident once its condition has held for the rule's *for* duration.
- **Resolve.** It resolves once the condition has been false for `RAEDAR_RESOLVE_AFTER` (5 minutes by default), unless the rule says otherwise. Waiting before resolving stops a value hovering around a threshold from producing a stream of messages.
- **Reopen.** If the same rule opens for the same subject within 30 minutes after resolving, the previous incident is reopened and the message says so ("again").
- **Change of severity.** A warning that becomes critical (for example disk usage crossing the second threshold) sends a new message for the same incident.
- **One-shot.** Some rules describe something that happened rather than a state: they open and resolve at once and send a single message.

Incidents for apps labeled `raedar.alerts=false` are recorded and shown, but not sent. A Traefik router labeled `raedar.ignore=true` is left out of the route rules entirely; it still appears on its app page. Acked incidents, and incidents covered by a [silence](notifications.md#silences), are recorded but not sent.

## Baselines

Error rate and latency rules compare the current value with the route's **baseline**: the median of the same hour of the day over the previous 7 days, ignoring hours with little traffic or an incident on that route (see [storage.md](storage.md#baselines)). A rule fires only when the value is above **both** a fixed minimum and a multiple of the baseline:

- the fixed minimum keeps tiny, harmless changes quiet ("0.1 % became 0.3 %")
- the multiple keeps routes that are always somewhat noisy or slow from alerting all day

When a route has no baseline yet (new route, fewer than 3 days of history, too little traffic), only the fixed minimum applies.

## Rules

Evaluation runs every 60 seconds over complete minutes.

### Routes

| Rule | Fires when | For | Severity |
|---|---|---|---|
| `route.errors` | Over the last 5 minutes: at least `RAEDAR_ERRORS_MIN_REQUESTS` (20) requests and at least 5 responses with a 5xx status, and the 5xx rate is at least `RAEDAR_ERRORS_MIN_RATE` (2 %) and at least `RAEDAR_ERRORS_FACTOR` (3) × the baseline. | 2 min | warning |
| `route.slow` | Over the last 10 minutes: at least `RAEDAR_SLOW_MIN_REQUESTS` (50) requests counted for latency, and p95 latency is at least `RAEDAR_SLOW_MIN_P95` (300 ms) and at least `RAEDAR_SLOW_FACTOR` (2) × the baseline p95. | 5 min | warning |
| `route.down` | Over the last 2 minutes: at least 3 requests, and at least half of them answered by Traefik with 502/503/504 because no backend responded. | — | critical |

While `route.down` is open for a route, `route.errors` and `route.slow` stay quiet for it. A route with no traffic cannot be judged from the access log; the container rules below cover that case.

### Containers

| Rule | Fires when | For | Severity |
|---|---|---|---|
| `container.crashed` | A container exits on its own with a non-zero exit code. One-shot. The message includes the exit code and whether the kernel killed it for running out of memory. | — | warning |
| `container.restarting` | A slot's container crashed and was restarted at least `RAEDAR_CRASH_LOOP_CRASHES` (3) times in `RAEDAR_CRASH_LOOP_WINDOW` (10 min). Resolves after 10 minutes without a crash. | — | critical |
| `container.stopped` | A slot's container crashed and is still not running 1 minute later (no restart policy, or the policy gave up). Resolves when the slot runs again. | 1 min | warning |
| `container.unhealthy` | Docker reports the container as `unhealthy`. | 2 min | warning |
| `container.memory` | The container's memory working set is at least `RAEDAR_MEMORY_LIMIT_WARN` (90 %) of its memory limit. Only containers with a limit. | 10 min | warning |

While `container.restarting` is open for a slot, `container.crashed` stays quiet for it. Intentional stops (`docker stop`, `docker compose down`, redeploys) never alert. See [discovery.md](discovery.md#container-lifecycle-events).

### Host

| Rule | Fires when | For | Severity |
|---|---|---|---|
| `host.disk` | A filesystem in `RAEDAR_DISKS` is at least `RAEDAR_DISK_WARN` (85 %) full; critical from `RAEDAR_DISK_CRIT` (95 %). Resolves when usage drops 2 points below the warning threshold. | 2 samples | warning / critical |
| `host.disk_forecast` | The filesystem is at least 50 % full, has grown over the last 24 hours, and a straight-line projection of that growth reaches 100 % within `RAEDAR_DISK_FULL_WITHIN` (3 days). Needs at least 12 hours of data. Resolves when the projection moves beyond 1.5 × that period or growth stops. Quiet while `host.disk` is critical for the same filesystem. | — | warning |
| `host.inodes` | Inode usage of a filesystem crosses the same thresholds as `host.disk`. | 2 samples | warning / critical |
| `host.memory` | Available memory is below `RAEDAR_HOST_MEMORY_MIN_AVAILABLE` (5 %) of total. | 5 min | warning |
| `host.unit_failed` | A systemd unit is in the `failed` state. Critical for units matching `RAEDAR_SYSTEMD_CRITICAL`. Resolves when the unit leaves the `failed` state, for example when a timer's next run succeeds. | — | warning |

### Certificates

| Rule | Fires when | Severity |
|---|---|---|
| `cert.expiry` | The certificate served for a domain expires within `RAEDAR_CERT_WARN` (14 days); critical within `RAEDAR_CERT_CRIT` (3 days) or when expired. | warning / critical |
| `cert.invalid` | Traefik serves its default certificate for the domain, or a certificate that does not cover it. | warning |

Certificate incidents resolve when the next check is fine. Checks run every 6 hours, so a fixed certificate can take up to that long to resolve; the TLS page shows the time of the last check.

### Raedar itself

| Rule | Fires when | Severity |
|---|---|---|
| `peer.silent` | A peer listed in `RAEDAR_HEARTBEAT_PEERS` has sent no heartbeat for `RAEDAR_HEARTBEAT_TIMEOUT` (3 minutes). Resolves on the next heartbeat. | critical |
| `raedar.source` | A source that was working has failed for 5 minutes (unreadable access log, unreachable Docker API, D-Bus errors). | warning |
| `raedar.database` | Raedar cannot write to PostgreSQL for 2 minutes. Sent from memory, without being stored first. | critical |

## Severity

Every rule has a default severity, shown above. The `raedar.critical=true` label on an app raises the availability rules to critical:

| Rule | Default | App labeled `raedar.critical=true` |
|---|---|---|
| `route.errors` | warning | critical |
| `route.down` | critical | critical |
| `container.crashed` | warning | critical |
| `container.stopped` | warning | critical |
| `container.unhealthy` | warning | critical |
| `container.restarting` | critical | critical |
| `route.slow`, `container.memory` | warning | warning |

Severity decides which channels receive the message: `RAEDAR_NOTIFY_WARNING` and `RAEDAR_NOTIFY_CRITICAL` (see [notifications.md](notifications.md#routing)).

## Messages

Each message answers what, how bad, since when, and where to look first.

```
🔴 host-1 · shop — 5xx errors
6.4% of requests fail (usual at this hour: 0.2%)
since 14:32 · 4 min after deploy shop 2.3.0 → 2.4.0
most errors: POST /api/checkout
https://raedar.example.com/incidents/812
```

```
✅ host-1 · shop — 5xx errors resolved after 18 min
```

Context added when available:

| Context | Added to |
|---|---|
| A deploy of the same app within 30 minutes before the incident opened | all app incidents |
| The path pattern with the most 5xx responses in the window | `route.errors` |
| The path pattern with the highest p95 among patterns with at least 10 requests | `route.slow` |
| Exit code and out-of-memory kill | `container.crashed`, `container.restarting`, `container.stopped` |
| Free space and growth over the last 24 hours | `host.disk`, `host.disk_forecast` |
| Unit result and exit status | `host.unit_failed` |
| Issuer and expiry date | certificate rules |

Container logs are never included in messages, because messages leave the server through third-party services and logs can contain secrets.

### Grouping

Incidents that open in the same evaluation are sent as one message per channel, with one block per incident. If more than 5 open at once, the message lists the 5 most severe and says how many more there are. Resolutions are grouped the same way. When a whole server has a bad minute, you get one message, not twenty.

## Daily digest

Once a day at `RAEDAR_DIGEST_AT` (09:00 in `RAEDAR_TZ` by default), Raedar sends a summary of the previous 24 hours to `RAEDAR_DIGEST_CHANNELS`:

- a headline: "All quiet", or the number of incidents and how many are still open
- open incidents
- incidents of the last 24 hours, grouped by rule, with the longest one
- deploys, by app
- apps: the five busiest, plus any app whose error rate exceeded 1 % or whose p95 rose more than 50 % compared with the day before, each with requests, error rate and p95
- each filesystem: usage, change over 24 hours and, if growing, the projected days until full
- Docker disk usage that could be reclaimed
- certificates expiring within 30 days
- systemd units in the `failed` state

The digest is sent even when nothing happened. A daily "All quiet" message is also proof that alerting still works.

## Heartbeats

**Outgoing.** When `RAEDAR_HEARTBEAT_URL` is set, Raedar sends a `GET` request to it every `RAEDAR_HEARTBEAT_INTERVAL` (60 seconds) while it is healthy: the database holds a host sample written within the last 3 minutes (the host collector writes one every minute, whatever the traffic) and the Docker source is working. `RAEDAR_HEARTBEAT_HEADER` adds one header, typically `Authorization: Bearer …`. Pointing it at an external check service (healthchecks.io, an Uptime Kuma push monitor) or at another Raedar instance means that a server that dies completely, together with its Raedar, still gets reported.

**Incoming.** When `RAEDAR_HEARTBEAT_PEERS` lists peer names, Raedar accepts heartbeats at `GET` or `POST /heartbeat/<name>` with `Authorization: Bearer <RAEDAR_HEARTBEAT_RECEIVE_TOKEN>`. Names not in the list are rejected. A listed peer that stays silent for `RAEDAR_HEARTBEAT_TIMEOUT`, counted from its last heartbeat or from Raedar's start, opens `peer.silent`. Two hosts can watch each other this way, each sending its heartbeat to the other.
