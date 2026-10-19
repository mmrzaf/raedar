# Data sources

This document describes exactly what Raedar reads from each source and how it turns it into measurements.

## Traefik access log

The access log is the source of request rate, status codes and latency. It is preferred over Traefik's metrics endpoint because each line carries the request path, which lets Raedar say *which endpoint* is failing or slow.

### Requirements

Traefik must write its access log in JSON:

```yaml
accessLog:
  format: json
  bufferingSize: 0   # or a small number; see "Late lines" below
```

Raedar needs these fields, which Traefik includes by default: `time`, `StartUTC`, `Duration`, `RouterName`, `ServiceName`, `RequestHost`, `RequestMethod`, `RequestPath`, `DownstreamStatus`, `OriginStatus`, `DownstreamContentSize`. Dropping headers (`fields.headers.defaultMode: drop`) is fine.

### Reading modes

| Mode | When | How |
|---|---|---|
| File | `RAEDAR_ACCESSLOG_PATH` is set | Raedar follows the file like `tail -F`. It detects rotation by inode change (rename-and-create) and by size shrinking (copy-truncate), and finishes reading the old file before switching. The inode and offset are saved with every flush, and reading resumes from them after a restart. If the saved inode no longer exists, reading starts at the end of the current file. |
| Container output | `RAEDAR_ACCESSLOG_PATH` is not set | Raedar follows the Traefik container's output through the Docker logs API with timestamps. It keeps lines that parse as access log entries (they have `DownstreamStatus`) and skips Traefik's own log lines. The timestamp of the last processed line is saved with every flush, and reading resumes after it. |

### From line to measurement

For every line:

1. **Time.** The minute is taken from `time` (when Traefik wrote the line, which is when the request completed). If it is missing, `StartUTC + Duration` is used.
2. **Route.** `RouterName` (for example `shop@docker`). An empty value means no router matched; the line goes to `(unrouted)`.
3. **Status class.** `DownstreamStatus` as 1xx, 2xx, 3xx, 4xx or 5xx.
4. **No backend.** The request counts as `no_backend` when `DownstreamStatus` is 502, 503 or 504 and `OriginStatus` is 0, meaning Traefik answered because it could not reach any backend.
5. **Latency.** `Duration` in milliseconds, added to the latency histogram. It is skipped for status 101 (protocol upgrades such as WebSockets) and for paths under the app's `raedar.latency.ignore` prefixes.
6. **Bytes.** `DownstreamContentSize` is added to bytes sent.
7. **Path.** `RequestMethod` and the normalized `RequestPath` (below) feed the path statistics.

Client IP addresses, user agents, query strings and headers are never stored.

### Path normalization

Paths are reduced to patterns so that `/orders/123` and `/orders/456` count as one endpoint:

1. The query string and fragment are removed.
2. The path is split into segments. Each segment is replaced when it looks like an identifier:

   | Segment | Becomes |
   |---|---|
   | only digits | `:n` |
   | a UUID | `:uuid` |
   | 16 or more hex characters | `:hex` |
   | 24 or more characters mixing letters and digits (tokens, hashes, base64) | `:token` |

3. A last segment with a static file extension (`js`, `mjs`, `css`, `map`, `png`, `jpg`, `jpeg`, `gif`, `webp`, `avif`, `svg`, `ico`, `woff`, `woff2`, `ttf`) becomes `*.<ext>`, so hashed asset names collapse.
4. Paths deeper than 6 segments keep the first 6 and end in `/…`.
5. Requests answered with 404 are recorded under the single pattern `(not found)`, so scanners probing random paths do not flood the statistics.
6. Each route keeps at most 300 distinct patterns per 10-minute window. Further patterns are counted under `(other)`.

### Latency histogram

Latency is recorded as counts per bucket, not as raw values, so histograms from different minutes, routes and paths can be added together and any percentile can be computed for any time range.

Bucket upper bounds in milliseconds:

```
5, 10, 25, 50, 75, 100, 150, 200, 300, 400, 500, 750,
1000, 1500, 2000, 3000, 5000, 10000, 30000, 60000, +Inf
```

A percentile is computed by finding the bucket that contains it and interpolating linearly between that bucket's bounds. Values in the last bucket are shown as "over 60 s". The relative error is small enough for alerting and charts, which is all the histogram is used for.

### Late lines

Traefik can buffer access log lines (`bufferingSize`) and write them in batches. Raedar keeps each minute's bucket open for 5 minutes and adds late lines to the minute they belong to. Lines more than 5 minutes late are added to the oldest open minute and counted in the source's `late_lines` statistic. For timely alerts, set `bufferingSize` to 0 or a small value.

## Docker

Raedar talks to the Docker Engine API directly over HTTP, through a socket proxy or the socket itself (`RAEDAR_DOCKER_HOST`). It uses only `GET` requests:

| Endpoint | Used for | Socket proxy permission |
|---|---|---|
| `/_ping`, `/version`, `/info` | connectivity, API version, host CPU count and memory | `PING`, `VERSION`, `INFO` |
| `/containers/json?all=1` | listing containers | `CONTAINERS` |
| `/containers/{id}/json` | labels, image, state, health, restart policy, networks, ports | `CONTAINERS` |
| `/containers/{id}/stats?stream=false&one-shot=true` | CPU, memory, network | `CONTAINERS` |
| `/containers/{id}/logs` | Traefik output in container-output mode; live logs in the UI | `CONTAINERS` |
| `/events` | container lifecycle | `EVENTS` |
| `/system/df` | Docker disk usage (optional) | `SYSTEM` |

Container environment variables are part of the inspect response. Raedar discards them immediately; they are never stored, logged or shown.

### Container stats

Every 60 seconds, for each running container:

| Measurement | Computed as |
|---|---|
| CPU | difference of `cpu_stats.cpu_usage.total_usage` between two samples, divided by elapsed time, in millicores |
| Memory | `memory_stats.usage` minus `inactive_file` (cgroup v2) or `cache` (cgroup v1): the working set, as `docker stats` shows it |
| Memory limit | `memory_stats.limit`; treated as "no limit" when it equals the host's total memory |
| Network | difference of received and sent bytes summed over all interfaces |

Samples are stored per slot, so a container recreated by a deploy continues the same line on the chart.

### Docker disk usage

Every 15 minutes, if `/system/df` is permitted: total and reclaimable size of images (images used by no container count as reclaimable), the build cache, volumes and container writable layers.

## Host

The host collector reads:

| Data | Source |
|---|---|
| CPU usage | `/proc/stat`, difference between two samples |
| Load average (1, 5, 15 min) | `/proc/loadavg` |
| Memory total and available, swap total and used | `/proc/meminfo` |
| Uptime | `/proc/uptime` |
| Hostname, OS | `<host root>/etc/hostname`, `<host root>/etc/os-release` |
| Filesystem size, used, available, inodes | `statfs` on each path in `RAEDAR_DISKS` |

Inside a container, `/proc/stat`, `/proc/loadavg` and `/proc/meminfo` report host-wide values, so no extra mount is needed for them. Filesystem statistics need the host filesystem mounted into the container; the default deployment mounts `/` read-only at `/host/root`. Disk usage percent is `used / (used + available)`, the same as `df`, so it reaches 100 % when ordinary users can no longer write.

## systemd

When the system D-Bus socket is mounted, Raedar reads systemd state through `org.freedesktop.systemd1` every 60 seconds:

- `ListUnits` for every unit's load, active and sub state
- for units in the `failed` state: `Result`, `ExecMainStatus` and the time the unit entered that state
- for timers: last trigger time, next elapse time, and the unit they activate

Reading unit state over D-Bus needs no privileges, so Raedar runs as an unprivileged user and cannot start, stop or change units. Units matching `RAEDAR_SYSTEMD_IGNORE` are skipped.

A unit entering or leaving the `failed` state is recorded as an event. Timers are shown on the host page with their last result.

## TLS

For every domain of every TLS route (and every domain in `RAEDAR_TLS_DOMAINS`), Raedar opens a TLS connection to Traefik with that domain as SNI and reads the certificate Traefik serves. It runs every 6 hours per domain, spread over time, and right after a new domain appears.

| Status | Meaning |
|---|---|
| `ok` | Valid for the domain, trusted by system roots, more than `RAEDAR_CERT_WARN` left. |
| `expiring` | Valid, but expires within `RAEDAR_CERT_WARN`. |
| `expired` | Past its expiry date. |
| `default` | Traefik served its built-in default certificate (`TRAEFIK DEFAULT CERT`), meaning no certificate matched this domain. |
| `mismatch` | The certificate does not cover the domain. |
| `private` | The chain does not verify against public roots, for example a Cloudflare Origin CA certificate. Shown, not alerted, because such certificates are used on purpose. |
| `error` | The connection or handshake failed. |

The connection goes to Traefik directly (`RAEDAR_TLS_CHECK_ADDR`, detected automatically by default), so the check sees what the origin serves even when a CDN sits in front of it.
