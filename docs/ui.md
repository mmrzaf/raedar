# User interface

The UI answers "is everything fine?" in seconds and "what happened, and when does it happen?" in a minute. It is read-only, works on a phone (alert links are usually opened from one), and follows the system's light or dark mode.

Pages are rendered on the server. Charts and live logs use a small amount of plain JavaScript and [uPlot](https://github.com/leeoniya/uPlot); there is no front-end build step.

## Layout

Every page has a top bar with the host name, the current time in `RAEDAR_TZ`, the navigation (Overview, Host, Patterns, Events) and one dot per source (access log, Docker, host, systemd, TLS, database) that turns amber or red when the source is degraded or unavailable. Hovering or tapping a dot shows its last error.

## Overview `/`

The page refreshes itself every 30 seconds.

1. **Open incidents.** Shown only when there are any, most severe first: rule, subject, value, how long it has been open. Each links to its incident page.
2. **Host strip.** CPU, load, memory, swap, each filesystem (usage, free space, and "full in N days" when growing), Docker disk usage with the reclaimable part.
3. **Apps.** One row per app, apps with problems first, then by traffic:

   | Column | Content |
   |---|---|
   | Status | Worst of: open incidents, stopped or unhealthy containers. |
   | App | Name, and its domains underneath. |
   | Traffic | Requests per minute, with a 24-hour sparkline. |
   | Errors | 5xx rate over the last hour, marked when above its baseline. |
   | p95 | p95 latency over the last hour, with an arrow when it differs clearly from its baseline. |
   | CPU, memory | Sum over the app's containers; memory against the sum of limits when all have one. |
   | Containers | Running / total, with restarts in the last 24 hours. |
   | Last deploy | Relative time ("3 h ago"). |

4. **Certificates.** Only domains that need attention (expiring within 30 days, `default`, `mismatch`, `error`), or "All certificates fine".
5. **Host jobs.** systemd units in the `failed` state, or "No failed units".
6. **Recent events.** The last 20 events.

## App `/apps/{app}`

The main page for understanding what happened.

**Header.** App name, domains (each with certificate status), status, and the containers table: slot, container name, image, state, health, uptime, restarts, CPU and memory now.

**Time range.** Presets (1 h, 6 h, 24 h, 7 d, 14 d, 90 d), or drag across any chart to zoom. The range is part of the URL, so a link from an alert opens exactly the window around the incident. Relative ranges ("last 6 h") update live.

**Charts.** Stacked on one shared time axis, with a shared cursor: hovering one chart shows the values of all charts at that moment.

| Chart | Series |
|---|---|
| Requests | Requests per minute, stacked by status class. |
| Error rate | 5xx rate, with the baseline as a dotted line. |
| Latency | p50, p95 and p99, with the baseline p95 as a dotted line. |
| CPU | Per slot. |
| Memory | Per slot, with each slot's limit as a line. |
| Network | Received and sent bytes per slot. |

When the app has several routes, a selector switches the request charts between all routes combined and a single route.

**Markers.** Vertical lines for deploys (labeled with the new image tag), restarts and crashes; shaded spans for incidents. Hovering a marker shows what it is.

**Compare.** "Add series" puts another series on the same time axis as an extra chart: any host metric, any other slot's CPU or memory, any other route's traffic, errors or latency. This is how a latency spike in one app is lined up with database CPU or the host's disk activity. The added series are kept in the URL. A toggle also shows events from all apps, not only this one.

**Paths.** For the selected range (within path retention, 7 days by default): method, path pattern, requests, 4xx and 5xx rates, p50 and p95. Sortable, sorted by 5xx count when the error rate is elevated and by p95 otherwise.

**Logs.** Clicking a point on any chart opens a drawer with the logs of the app's containers from 2 minutes before to 2 minutes after that moment, read live from Docker. A selector picks the container (all of the app's containers by default), a text box filters lines, and a toggle follows the logs live. Logs are only available as long as Docker still has them; with the common `json-file` settings (3 files of 10 MB) that is often hours to days. Raedar never stores logs.

**Events.** Events of the app in the selected range.

## Host `/host`

The same chart layout and time range controls as the app page, for the host:

- CPU and load average
- memory available and swap used
- one chart per filesystem: used space with the warning and critical thresholds, and the forecast line when growing
- Docker disk usage: images, build cache, volumes, container layers

Below the charts:

- **systemd.** Failed units, then all timers with their last run, last result and next run.
- **Certificates.** Every checked domain: status, issuer, expiry date, days left, last check time.

## Patterns `/patterns`

For seeing when things tend to happen. Pick a subject (an app, one of its routes, a slot, or the host) and a metric (requests, error rate, p95, CPU, memory, disk used, load), and a period (4 weeks by default, up to 52).

- **Week heatmap.** Rows are weekdays, columns are hours of the day in `RAEDAR_TZ`, the color is the metric's average for that hour across the period. A recurring evening peak or a nightly slowdown shows up as a band.
- **Day profile.** The average 24-hour curve of the last 7 days overlaid on the average of the period before it, to see whether the daily shape is changing.
- **Trend.** One value per day across the whole period (daily total for counts, daily average for gauges, daily maximum for disk), to see slow growth.

## Events `/events`

Every event and incident, newest first, grouped by day. Filters for app, kind (deploy, crash, health, unit, certificate, incident) and severity, and a time range. The page for answering "what happened overnight?".

## Incident `/incidents/{id}`

- rule, subject, severity, opened and resolved times, duration
- the values that triggered it and the baseline
- context: deploys before it, top path, exit codes
- the relevant chart for a window around the incident, with a link to the app or host page for the same window
- deliveries: each channel, when it was sent, attempts, errors

## About `/about`

Raedar version, uptime, effective configuration with secrets masked, source details (position in the access log, Docker API version, last TLS check run) and database size per table.

## JSON API

The pages load chart data from a small JSON API. It is meant for the UI; it is documented here for transparency and may change between minor versions.

| Endpoint | Returns |
|---|---|
| `GET /api/overview` | Everything on the overview page. |
| `GET /api/series?keys=…&from=…&to=…&points=…` | Time series for one or more series keys (for example `route:shop@docker:p95`, `slot:shop/web#1:cpu`, `host:load1`, `disk:/:used`). |
| `GET /api/baseline?route=…&from=…&to=…` | Baseline values for the range. |
| `GET /api/paths?app=…&route=…&from=…&to=…` | Path statistics. |
| `GET /api/events?app=…&kind=…&from=…&to=…` | Events and incidents. |
| `GET /api/patterns?key=…&weeks=…` | Heatmap, day profile and trend. |
| `GET /api/logs?container=…&since=…&until=…&follow=…` | Container logs, streamed as server-sent events when `follow=true`. |
| `GET /healthz` | `200` when the process is running and the database is reachable. Used by the container healthcheck. |

Apart from the heartbeat receiver, every endpoint is `GET`. Raedar has no endpoint that changes anything on the host.
