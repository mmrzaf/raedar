# Storage

Raedar stores everything in its own PostgreSQL database. Most Docker + Traefik hosts already run PostgreSQL for their apps; Raedar uses a separate database (and preferably a separate role) on that server.

- PostgreSQL 14 or newer is required (`date_bin` is used for downsampling).
- One database per Raedar instance. While running, an instance holds a session-level advisory lock; a second instance pointed at the same database exits with an error.
- Raedar uses at most 4 connections.

## Tables

### Model

| Table | Content |
|---|---|
| `apps` | `id`, `name` (unique), `first_seen`, `last_seen`, `archived_at` |
| `slots` | `id`, `app_id`, `key` (unique per app), `service`, `replica`, `first_seen`, `last_seen` |
| `containers` | `docker_id` (primary key), `slot_id`, `name`, `image_ref`, `image_id`, `created_at`, `started_at`, `removed_at` |
| `routes` | `id`, `name` (unique, `name@provider`), `app_id`, `kind` (`http`, `tcp`), `rule`, `domains` (text[]), `tls`, `passthrough`, `origin` (`labels`, `accesslog`), `first_seen`, `last_seen` |
| `peers` | `name` (primary key), `last_heartbeat` |
| `source_state` | `source` (primary key), `position` (jsonb: inode and offset, or last log timestamp), `status`, `last_error`, `updated_at` |

### Measurements

| Table | Key | Columns |
|---|---|---|
| `route_minute` | `route_id`, `ts` | `requests`, `s1xx`, `s2xx`, `s3xx`, `s4xx`, `s5xx`, `no_backend`, `bytes`, `latency` (int[21]) |
| `route_hour` | `route_id`, `ts` | same as `route_minute` |
| `path_window` | `route_id`, `ts` (10-minute window), `method`, `pattern` | `requests`, `s4xx`, `s5xx`, `latency` (int[21]) |
| `slot_minute` | `slot_id`, `ts` | `cpu_millicores`, `mem_bytes`, `mem_limit_bytes`, `net_rx_bytes`, `net_tx_bytes`, `running` |
| `slot_hour` | `slot_id`, `ts` | `cpu_avg`, `cpu_max`, `mem_avg`, `mem_max`, `mem_limit_bytes`, `net_rx_bytes`, `net_tx_bytes` |
| `host_minute` | `ts` | `cpu_percent`, `load1`, `load5`, `load15`, `mem_total`, `mem_available`, `swap_total`, `swap_used` |
| `host_hour` | `ts` | `cpu_avg`, `cpu_max`, `load1_avg`, `load1_max`, `mem_available_min`, `mem_available_avg`, `swap_used_max` |
| `disk_minute` | `disk`, `ts` | `size_bytes`, `used_bytes`, `avail_bytes`, `inodes`, `inodes_used` |
| `disk_hour` | `disk`, `ts` | `size_bytes`, `used_bytes_max`, `avail_bytes_min`, `inodes`, `inodes_used_max` |
| `docker_disk` | `ts` | `images_bytes`, `images_reclaimable_bytes`, `build_cache_bytes`, `volumes_bytes`, `containers_bytes` |

`route_minute` holds a row only for minutes with at least one request. `ts` is always the start of the minute, hour or window, stored in UTC. Hourly rows start on local hour boundaries of `RAEDAR_TZ`, so in a zone such as `Asia/Tehran` (UTC+03:30) they start at :30 UTC and every hour belongs to exactly one local hour of the day.

### State and history

| Table | Content |
|---|---|
| `units` | Current state of the systemd units Raedar has seen; units not seen for 7 days are dropped: `name`, `active_state`, `sub_state`, `result`, `exit_status`, `since`, `last_seen` |
| `certs` | Latest check per domain: `domain`, `route_id`, `checked_at`, `status`, `not_after`, `issuer`, `subject`, `sans`, `error` |
| `baselines` | `route_id`, `hour_of_day` (0–23 in `RAEDAR_TZ`), `error_rate`, `p95_ms`, `days_used`, `computed_at` |
| `events` | `id`, `ts`, `kind`, `severity`, `app_id`, `slot_id`, `route_id`, `subject`, `title`, `data` (jsonb) |
| `incidents` | `id`, `rule`, `subject`, `app_id`, `slot_id`, `route_id`, `severity`, `opened_at`, `resolved_at`, `one_shot`, `summary`, `details` (jsonb) |
| `deliveries` | `id`, `incident_id`, `kind` (`opened`, `escalated`, `resolved`, `digest`, `test`), `channel`, `created_at`, `sent_at`, `attempts`, `status`, `error` |
| `schema_migrations` | applied migration versions |

## Histograms in SQL

Latency histograms are `int[]` with one count per bucket (see [data-sources.md](data-sources.md#latency-histogram)). Two helpers, created by the migrations, keep merging inside the database:

- `raedar_hist_add(a int[], b int[])` adds two histograms element by element. The minute flush uses it in its upsert: `latency = raedar_hist_add(route_minute.latency, excluded.latency)`.
- `raedar_hist_sum(int[])` is an aggregate that sums histograms over many rows, used by rollups and range queries.

Percentiles are computed in Go from the summed histogram.

## Rollups

Every local hour at :05, Raedar recomputes the hourly tables for the last 3 complete local hours from the minute tables and replaces those rows. Recomputing more than one hour absorbs late access log lines and restarts. At startup, hourly rows that do not start on a local hour boundary of the configured zone (after changing `RAEDAR_TZ`, or from before hours followed the zone) are rebuilt from the minute tables for as far back as those still exist; older rows stay as they are.

Rollups use:

| Hourly column | From minute data |
|---|---|
| counts, bytes, histograms | sum |
| `cpu_avg`, `mem_avg`, `load1_avg`, `mem_available_avg` | average |
| `cpu_max`, `mem_max`, `load1_max`, `swap_used_max`, `used_bytes_max`, `inodes_used_max` | maximum |
| `mem_available_min`, `avail_bytes_min` | minimum |

## Reading ranges

Charts ask for a time range and a number of points (about 600 by default). Raedar picks the source table and bins rows with `date_bin`:

| Range | Source | Bin size |
|---|---|---|
| up to 14 days, within minute retention | minute tables | range ÷ points, rounded up to a whole minute |
| longer, or older than minute retention | hourly tables | range ÷ points, rounded up to a whole hour |

Counts and histograms are summed per bin; gauges use the average, with the maximum available for CPU and memory.

## Baselines

After each rollup, Raedar computes, for every route and every hour of the day in `RAEDAR_TZ`:

- the median error rate (`s5xx / requests`) and the median p95 latency
- over the same hour on each of the previous 7 days
- using only hours with at least `RAEDAR_ERRORS_MIN_REQUESTS` requests, and skipping hours that overlap an incident on that route, so an outage does not become "normal"

A baseline needs at least 3 qualifying days. Otherwise it is stored as missing, and rules fall back to their fixed thresholds.

## Retention

| Data | Default | Variable |
|---|---|---|
| Minute tables | 14 days | `RAEDAR_RETENTION_MINUTE` |
| `path_window` | 7 days | `RAEDAR_RETENTION_PATHS` |
| Hourly tables and `docker_disk` | 400 days | `RAEDAR_RETENTION_HOUR` |
| Events, incidents, deliveries | 400 days | `RAEDAR_RETENTION_EVENTS` |

400 days keeps a full year of hourly data for heatmaps and trends, plus a margin for comparing with the same period last year. Cleanup runs once a day and deletes in batches to keep transactions short.

## Size

Expected size with default retention on a host with 20 routes, 20 slots and moderate traffic:

| Table | Approximate rows | Approximate size |
|---|---|---|
| `route_minute` (routes active 60 % of minutes) | 240,000 | 50 MB |
| `slot_minute` | 400,000 | 50 MB |
| `path_window` (10 active patterns per route per window) | 200,000 | 50 MB |
| hourly tables | 400,000 | 55 MB |
| host, disk, events, everything else | — | under 20 MB |
| **Total, including indexes** | | **about 200–250 MB** |

On hosts with little free disk, shorten `RAEDAR_RETENTION_MINUTE` and `RAEDAR_RETENTION_PATHS` first: they account for most of the size.

## Migrations

Migrations are plain SQL files embedded in the binary, numbered and applied in order at startup, each in its own transaction. Downgrades are not supported. Upgrading Raedar applies new migrations automatically.

## Backups

Raedar's database holds history only. Losing it loses charts and incident history, not configuration, so including it in database backups is optional.
