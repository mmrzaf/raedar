# Changelog

All notable changes to Raedar are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [0.2.0] - 2026-10-19

### Upgrading

- Migration `0002` widens the delivery kinds. It is applied on start and is
  forward-only; back up the database first.
- On the first start, hourly rows inside the minute-retention window that do not
  start on a local hour boundary of `RAEDAR_TZ` are rebuilt from the minute
  data. This matters in zones with a half-hour offset such as `Asia/Tehran`; it
  takes a few seconds and happens once. Hourly rows older than the minute
  retention keep their old alignment.
- The JSON API changed: `/api/series` takes repeated `series=kind/subject/metric`
  parameters (see [docs/ui.md](docs/ui.md#json-api)), `/api/patterns` takes
  `series`, series values can be `null`, and a few field names were corrected
  (`id`, `ts`, `cpu`). The interface ships in the same binary, so nothing else
  needs to change.

### Added

- A redesigned interface: an app shell with a status banner, cards, host tiles
  with usage meters, tighter typography, a first-class dark theme with a theme
  switch (system, light, dark), floating chart tooltips, a heatmap legend, a
  footer with the About page, and a phone layout. The overview refreshes itself
  every 30 seconds while it is visible.
- Latency is shown as `63 ms` or `1.5 s`, and a latency beyond the histogram's
  60 s bound as `>60 s` instead of a number; CPU is shown in cores.
- Charts use 24-hour clocks in `RAEDAR_TZ`, 0–100 % axes for percentages, round
  byte ticks, gaps for missing data and labeled empty states, plus a hover
  readout.
- A favicon.
- A `Content-Security-Policy` of `default-src 'self'`, and a test that fails on
  any external resource or inline handler in the interface.
- Hourly rollups, baselines, heatmaps and day profiles follow local hours, so
  zones with half-hour offsets are exact.
- Open incidents resolve on their own when the container, route, unit or peer
  they belong to is gone.
- Escalations are recorded as their own delivery kind and digests are recorded
  as deliveries.
- Systemd units and certificates that were not refreshed for 7 days are dropped.

### Changed

- "Last deploy" now shows the newest deploy or recreate of the app, however old,
  and deploys are found even when the old container was removed before the new
  one appeared or while Raedar was down.
- One-off containers (`docker compose run`) and containers with
  `raedar.ignore=true` are not tracked at all; lifecycle events are attributed by
  labels, restarts are recorded once, and events of short-lived containers that
  belong to no app are dropped.
- The heartbeat is sent while the database accepted writes recently (judged by
  the host sample, which is written every minute) and the Docker source works,
  as documented, instead of stopping on an idle server.
- Containers that ran to completion (exit code 0, no restart policy) no longer
  make an app show a warning.
- The Events page filter offers the kinds that exist in the range.
- Free space and the disk forecast both use the space available to writers.
- Notifications are sent after the rule engine releases its state, so a slow
  channel no longer delays the next evaluation.
- The overview reads all apps with a few queries instead of a few per app.
- Series are identified by typed references and a single metric registry, which
  replaces four lookup functions, a hand-written list of pattern metrics and the
  unit table in the browser. Unknown metrics are rejected with `400`.
- Severities, event kinds and delivery kinds are typed.
- The web package is split by concern, and the disk forecast, app status and
  pattern aggregation are covered by tests that need no database.
- The integration tests fail, not skip, without `RAEDAR_TEST_DATABASE_URL`, and
  share one set of helpers.

### Fixed

- The patterns page showed `:00`–`:23` and `1/1/70` on its axes, an empty trend,
  and zeros for hours without data.
- Charts of an app were empty after a restart until Docker had answered.
- The log drawer was always visible, followed the logs only until the end of its
  window, reconnected in a loop and fetched twice.
- A crash of a container created since the last resync raised no alert.
- Delivery bookkeeping failed for escalations.
- Pluralization ("1 days") and unreadable axis ticks.

### Removed

- Per-package copies of the clock, the source status recorder and the retry
  schedule, the Docker adapter in `cmd/raedar`, and unused code in the web
  package.

## [0.1.3] - 2026-10-19

### Fixed

- Patterns read recent data from the minute tables.

## [0.1.2] - 2026-10-19

### Added

- Swap series on the host charts.

### Fixed

- Duplicate TLS source dot.

## [0.1.1] - 2026-10-19

### Added

- Subject and metric pickers on the patterns page.

### Fixed

- The chart library is loaded and every slot is charted.
- Pages render completely and template errors fail loudly.

## [0.1.0] - 2026-10-19

- First deployment: access log, containers, host, systemd and TLS sources,
  rules and incidents, notifications, digest, heartbeats and the web interface.
