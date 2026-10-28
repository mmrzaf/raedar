# Changelog

All notable changes to Raedar are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [0.4.3] - 2026-10-28

### Added

- **Durable notification pages.** Every incident episode now has one page per
  channel and recipient in PostgreSQL. The planner re-creates a missing page
  after a crash, and the dispatcher retries it until it is announced,
  resolved, cancelled or dropped. Silence, quiet hours, cooldown, flap and
  rate limits only move `due_at`; they no longer discard the page.
- The incident page shows the notification state of every channel and
  recipient, including the reason a page is waiting and its next attempt. The
  Deliveries page shows the recipient, the suppression reason and the silence.
- Removing a silence wakes the pages waiting on it.

### Changed

- **Auto-deploy silences no longer hide critical incidents or resolutions.**
  They only cover warning noise during a deploy. A crash loop pages
  immediately even inside the deploy window.
- **Flap suppression counts only announced pages.** A page held by a silence
  or quiet hours no longer consumes the page budget. Critical incidents
  bypass the budget.
- **Resolutions are best-effort.** A resolution that would be held by a
  silence or quiet hours is dropped instead of arriving stale. Critical
  resolutions still pass quiet hours.
- Transport failures now retry with capped backoff for as long as the
  incident is open, instead of failing permanently after three attempts.
- A page withheld by the rate limit is rescheduled instead of summarized.
- Expired auto-deploy silences are removed after a day and expired one-shot
  mutes after the 30-day cap.

### Fixed

- A critical incident opened during an auto-deploy silence was suppressed at
  open and never retried after the silence expired. The durable page now
  sends when the silence ends, and criticals bypass the deploy silence
  entirely.
- A page suppressed by a silence, a restart, a failed send or a dropped
  in-memory retry could be lost forever because notifications were only
  driven by incident state changes. The planner is now level-triggered and
  guarantees every open incident episode has a durable page.
- Deleting a silence only removed the row; it did not release the incident.
  Pending pages are now woken in the same transaction.
- A pending opening message for an app with alerts disabled waits for alerts
  to come back on instead of being cancelled. Resolutions are still dropped.
- Acknowledging an incident now records an `acked` delivery for each page it
  cancels, so the Deliveries page shows why paging ended.
- A Telegram chat that refuses the bot (HTTP 400 or 403) cancels its page
  instead of being retried for as long as the incident is open. A rejected
  token or a server error is still retried.
- Certificate chain verification used the wall clock instead of the checker's
  clock, so a certificate that was expiring at the evaluated moment could be
  classified as `private` once its test window passed. It now verifies at the
  same `now` as the rest of the check.

### Upgrading

- Migration `0008` is applied on start and is forward-only. It adds
  `incidents.episode`, the `pages` table and the delivery reason, recipient,
  page and silence columns.
- On startup the planner seeds pages from existing incidents and sent
  deliveries. Already-announced incidents are not paged again. An open
  incident that has no sent page, such as one whose only page was silenced,
  is paged after the upgrade. Those messages are grouped per channel and
  recipient and paced by `RAEDAR_RATE_LIMIT` and `RAEDAR_SMS_RATE_LIMIT`.

## [0.4.2] - 2026-10-24

### Fixed

- A `private` certificate (a valid Cloudflare Origin CA certificate used by a
  proxied origin) is no longer shown as a critical badge on the Host and app
  pages. It is a valid origin certificate, not an invalid one.

## [0.4.1] - 2026-10-23

### Fixed

- A certificate incident now resolves when its domain is removed from the
  route that used to serve it, even if the route itself still exists. This
  covers renamed hosts and domains moved to another router.

## [0.4.0] - 2026-10-23

### Changed

- **Warnings go to the digest by default.** They are still recorded and listed
  in the daily digest, but only critical incidents page. Set
  `RAEDAR_NOTIFY_WARNING` to channels to page warnings again, or `none` to
  disable pages explicitly.
- Route messages now name the host, say what happened and include one action
  line. Reopened incidents say "(again)". The webhook `context` array includes
  the same action line.
- The catch-all file router (`fallback@*`) and `(unrouted)` traffic are not
  attached to an app and never page. Their requests are still recorded.
- `route.slow` does not page for long-lived traffic: a route with at least
  `RAEDAR_LONG_REQUEST_SHARE` of requests longer than `RAEDAR_LONG_REQUEST`
  is treated as streaming. The `raedar.latency.ignore` label remains the
  explicit per-path override.
- A flapping (rule, subject) may use `RAEDAR_FLAP_MAX_PAGES` opening pages per
  `RAEDAR_FLAP_WINDOW`; later opens and their resolutions are recorded but
  dropped.

### Added

- `RAEDAR_ROUTE_IGNORE` for route name patterns that should not be evaluated.
- `RAEDAR_LONG_REQUEST` and `RAEDAR_LONG_REQUEST_SHARE` for long-lived traffic.
- `RAEDAR_FLAP_WINDOW` and `RAEDAR_FLAP_MAX_PAGES` for flap suppression.
- Human-readable silence kinds and day/hour durations in the UI.

### Fixed

- `cert.invalid` resolves when the route that served the domain disappears,
  instead of staying open until the certificate row is pruned after seven days.
- The Silences and Deliveries tables scroll inside their card instead of
  widening the page. Long reasons and delivery errors wrap, the form is a
  responsive grid, delivery statuses are coloured, and the remove action is a
  text link.
- Message formatting no longer panics when a certificate or unit detail is
  missing.

### Upgrading

- The default `RAEDAR_NOTIFY_WARNING` changed from `telegram,webhook` to
  `none`. Set it explicitly if you want warning pages.
- No database migration is needed.

## [0.3.3] - 2026-10-21

### Fixed

- A failed unit that systemd unloads, as it does after `systemctl
  reset-failed`, no longer keeps its incident open forever: the unit is
  recorded as inactive once it leaves systemd's list.
- Deliveries left `pending` by a restart are marked failed at startup. Their
  retries lived in memory, so nothing ever finished them.

## [0.3.2] - 2026-10-21

### Fixed

- Telegram polling now goes through the outbound proxy exactly when Telegram
  messages do. On a host that reaches Telegram only through
  `RAEDAR_OUTBOUND_PROXY`, `getUpdates` timed out on every poll and
  `ack`/`mute` never worked.
- A failed poll no longer logs the request URL, which contains the bot token.
  Rotate the token if an earlier version logged it.

## [0.3.1] - 2026-10-21

### Fixed

- The audit row of an acked incident was rejected by the deliveries status
  check, so the Deliveries page never showed acked pages (the ack itself
  worked). Migration `0007` allows the `acked` status and narrows the status
  and silence kind checks to the values Raedar writes.

### Upgrading

- Migration `0007` is applied on start and is forward-only. Deploy 0.3.1
  instead of 0.3.0.

## [0.3.0] - 2026-10-21

### Added

- **Silences.** A silence stops pages for a scope until it expires while
  every incident change is still recorded. Maintenance windows come from the
  new Silences page, one-shot mutes from Telegram, and auto-deploy silences
  from Raedar itself; quiet hours are a recurring window. Precedence is
  (rule, subject) > rule > subject > app > everything.
- **Telegram inbound.** Raedar long-polls `getUpdates` while it runs, so the
  operator can reply `ack <id>`, `mute 2h` (on a paged message),
  `mute <rule> <subject> <duration>`, `unmute <id>`, `status` and `help`
  without opening a port or configuring a webhook. Processed updates are
  deduplicated in `telegram_inbox`.
- **Quiet hours.** `RAEDAR_QUIET_FROM/TO/TZ` hold warnings and deliver them
  as one message when the window closes. Critical incidents page through
  unless `RAEDAR_QUIET_PAGE_CRITICAL=false`.
- **Pager cooldown.** `RAEDAR_PAGER_COOLDOWN` (with per-rule overrides in
  `RAEDAR_PAGER_COOLDOWN_BY_RULE`) keeps the same (rule, subject) from
  paging repeatedly; the held page goes out once when the cooldown expires.
- **Stand-down.** `RAEDAR_STAND_DOWN_AFTER` sends one "all quiet for N h"
  message after that much silence, so an idle host still proves the
  pipeline works.
- **Auto-deploy silence.** With `RAEDAR_DEPLOY_SILENCE` set, each deploy
  silences the app for that long, deduplicated per image, so a restart does
  not page.
- **Subject names.** `RAEDAR_SUBJECT_NAMES` renames subjects in messages,
  the digest and the UI; a trailing `*` is a prefix pattern.
- **Route ignore.** `raedar.ignore=true` on a Traefik router keeps it out of
  the route rules while leaving it visible.
- **Silences and Deliveries pages.** The UI lists, creates and removes
  silences (maintenance windows) and shows every delivery with its status:
  `sent`, `failed`, `silenced`, `cooldown` or `held`.
- **Ack metadata.** `incidents.acked_at` and `acked_by` persist an ack, and
  the incident page shows who acknowledged it and when.
- **Patterns default.** The patterns page opens on the host (CPU and
  memory) instead of the first app.

### Changed

- **SMS is one generic HTTP shape.** The provider-specific path is gone; any
  provider is described with `RAEDAR_SMS_HTTP_URL` and
  `RAEDAR_SMS_HTTP_BODY`, with `{{to}}`, `{{message}}`, `{{from}}` and
  `{{api_key}}` placeholders (the key comes from
  `RAEDAR_SMS_HTTP_API_KEY`). `RAEDAR_SMS_PROVIDER` and the Kavenegar
  variables were removed.
- One delivery row is recorded per (incident × chat) instead of per
  incident, and a retry contacts only the chats that failed, so a dead chat
  can no longer duplicate pages to the working ones.
- `container.restarting` thresholds are configurable with
  `RAEDAR_CRASH_LOOP_WINDOW` and `RAEDAR_CRASH_LOOP_CRASHES`.

### Fixed

- A page withheld by the rate limit no longer leaves pending delivery rows
  behind.
- A held or cooled-down page is checked by the gate again before it is
  sent. An incident acked, silenced or resolved while waiting no longer
  produces a catch-up message, and a cooldown that ends inside quiet hours
  no longer pages during them.
- An ack now belongs to one episode: an incident that resolves and reopens
  pages again instead of staying silent.
- The pager cooldown only delays opening pages. Escalations and resolutions
  go out at once, and a resolution is sent only to channels that actually
  announced the incident.
- SMS to several numbers is attempted per number. One failing number no
  longer stops the others, and a retry no longer texts the numbers that
  already got the message.
- Telegram replies are valid HTML, so usage and error answers reach the
  operator instead of being rejected by Telegram.
- `mute` by reply resolves the page by chat and message id and refuses a
  message that announced several incidents, instead of muting an arbitrary
  one. Message ids are only unique per chat.
- `ack` reports an already acked or resolved incident correctly, and `mute`
  is capped at 30 days like the Silences page.
- `RAEDAR_QUIET_TZ` defaults to `RAEDAR_TZ` as documented (it was UTC), and
  `RAEDAR_PAGER_COOLDOWN`, `RAEDAR_STAND_DOWN_AFTER`, `RAEDAR_DEPLOY_SILENCE`
  and the per-rule cooldowns accept `0` to turn them off.
- The daily digest no longer counts as a page for the stand-down notice.
- The notification rate limiter is safe for concurrent use, and a failing
  held-summary send backs off instead of retrying every second.
- SMS placeholders are substituted in one pass, so a message that contains
  `{{api_key}}` cannot pull the key into the request.
- The Silences page refuses a silence without a rule or a subject.
- A deploy silence re-armed for the same image now extends instead of
  keeping its expired end time.

### Upgrading

- Migrations `0005` and `0006` are applied on start and are forward-only.
  `0005` adds `deliveries.telegram_chat_id` and `telegram_message_id`; `0006`
  adds `silences.app_id`.
- Removed variables: `RAEDAR_SMS_PROVIDER`, `RAEDAR_KAVENEGAR_API_KEY`,
  `RAEDAR_KAVENEGAR_SENDER`. Configure SMS through the generic HTTP shape.
- `RAEDAR_SMS_FROM` is new and optional.

## [0.2.2] - 2026-10-20

### Added

- A `silences` table backs every kind of "do not page this now" —
  maintenance windows, one-shot mutes, quiet hours, pager cooldowns and
  auto-deploy silences. Rule and subject are nullable so wildcards work;
  the partial `(kind, dedup_key)` unique index makes idempotent inserts
  possible (for example, a deploy that re-arms the same silence).
- `incidents.acked_at` and `incidents.acked_by` columns so an operator's
  ack is durable. Every query that reads an incident now exposes them.
- A paging `Gate` in `internal/notify` that every delivery must pass
  through. Today `Evaluate` returns `GateSilenced` when an active
  silence covers the (rule, subject) and `GateAcked` when the incident
  has been acked. The `held` and `cooldown` statuses are wired into the
  deliveries status check but unused until 0.3.0 turns them on.
- `telegram_inbox` table for inbound command dedup (later phase).
- New env vars registered and validated, but with their default behaviour
  unchanged: `RAEDAR_QUIET_FROM`, `RAEDAR_QUIET_TO`, `RAEDAR_QUIET_TZ`,
  `RAEDAR_QUIET_PAGE_CRITICAL`, `RAEDAR_PAGER_COOLDOWN`,
  `RAEDAR_STAND_DOWN_AFTER`, `RAEDAR_DEPLOY_SILENCE`.

### Fixed

- `Docker disk usage is unavailable` no longer logs at WARN every 15
  minutes. The 403 from a read-only socket-proxy is the well-known
  expected answer for `/system/df`; Raedar detects it and demotes the
  log line to Debug. `raedar check` still surfaces it in the
  docker-source line so the operator sees "Docker disk usage stays
  empty" by design.

### Upgrading

- Existing deployments need no changes. Migrating from 0.2.1 brings two
  forward-only migrations (0003_silences, 0004_inbox_ack). The
  deliveries.status check widens; the rows already in production are
  unaffected.

## [0.2.1] - 2026-10-20

### Fixed

- The container runs as `nobody` (uid 65534) instead of uid 65532. The system
  D-Bus only accepts connections from users that exist in the host's user
  database, and no host has 65532, so systemd monitoring could never connect.
  With `RAEDAR_SYSTEMD=on` that stopped Raedar from starting.
- The production compose file mounts the system D-Bus socket, as
  `docs/deployment.md` always described, and `deploy/.env.example` sets
  `RAEDAR_SYSTEMD=auto`, which monitors systemd when the bus answers and carries
  on without it otherwise.

### Upgrading

- A deployment that mounts the D-Bus socket and sets `user: "65532:65532"` must
  change the user to `"65534:65534"`. Access log files must be readable by that
  user, which holds for world-readable files.

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
