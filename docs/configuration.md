# Configuration

Raedar is configured with environment variables and, per app, with container labels. There is no configuration file. Apart from the database URL, every setting has a default.

## Formats

| Kind | Format | Examples |
|---|---|---|
| Duration | number followed by `s`, `m`, `h` or `d` | `90s`, `5m`, `6h`, `14d` |
| Percentage | number with or without `%` | `85%`, `85`, `2.5%` |
| List | comma-separated, spaces around items ignored | `telegram,sms` |
| Time of day | `HH:MM` in `RAEDAR_TZ` | `09:00` |

**Secrets from files.** Every variable can also be given as `<NAME>_FILE` pointing to a file that contains the value, for example `RAEDAR_TELEGRAM_TOKEN_FILE=/run/secrets/telegram_token`. Trailing newlines are removed. Setting both forms of the same variable is an error.

Raedar validates the whole configuration at startup and exits with a list of every problem it found. `raedar check` runs the same validation and also tests connectivity.

## General

| Variable | Default | Meaning |
|---|---|---|
| `RAEDAR_DATABASE_URL` | **required** | PostgreSQL connection URL, for example `postgres://raedar:…@postgres:5432/raedar?sslmode=disable`. |
| `RAEDAR_LISTEN` | `:8080` | Address the web server listens on. |
| `RAEDAR_PUBLIC_URL` | — | Public URL of the UI. Used for links in messages; without it messages contain no links. |
| `RAEDAR_HOST_NAME` | hostname from `<RAEDAR_HOST_ROOT>/etc/hostname` | Name of the host in the UI and in messages. |
| `RAEDAR_TZ` | `UTC` | IANA time zone for display, baselines, heatmaps and the digest, for example `Asia/Tehran`. |
| `RAEDAR_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON on stdout. |

## Docker and Traefik

| Variable | Default | Meaning |
|---|---|---|
| `RAEDAR_DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker API address. With a socket proxy: `tcp://socket-proxy:2375`. |
| `RAEDAR_TRAEFIK_CONTAINER` | detected | Name of the Traefik container. See [discovery.md](discovery.md#traefik). |
| `RAEDAR_ACCESSLOG_PATH` | — | Path of the Traefik access log file inside the Raedar container. When empty, the access log is read from the Traefik container's output. |
| `RAEDAR_TLS_CHECK_ADDR` | detected | `host:port` where Traefik serves HTTPS, reachable from Raedar, for example `traefik:443`. Detected from the Traefik container's published port 443 and a network both containers share. |
| `RAEDAR_TLS_DOMAINS` | — | Extra domains to check, for routes defined outside Docker labels. |

## Host

| Variable | Default | Meaning |
|---|---|---|
| `RAEDAR_HOST_ROOT` | `/host/root` | Where the host's root filesystem is mounted, read-only. |
| `RAEDAR_DISKS` | `RAEDAR_HOST_ROOT` | Filesystems to watch: paths inside the container. Each is shown by its path relative to `RAEDAR_HOST_ROOT` (`/` for the root itself), or by an explicit name given as `name=path`. |
| `RAEDAR_SYSTEMD` | `auto` | `auto` reads systemd when `/run/dbus/system_bus_socket` exists; `on` makes a missing socket an error; `off` disables it. |
| `RAEDAR_SYSTEMD_IGNORE` | — | Unit name patterns to ignore, with `*` wildcards, for example `apt-daily*.service,man-db.service`. |
| `RAEDAR_SYSTEMD_CRITICAL` | — | Unit name patterns whose failure is critical, for example `*-backup.service`. |

## Rules

See [alerts.md](alerts.md) for what each rule does.

| Variable | Default | Used by |
|---|---|---|
| `RAEDAR_ERRORS_MIN_RATE` | `2%` | `route.errors` (per app: label `raedar.errors.min_rate`) |
| `RAEDAR_ERRORS_FACTOR` | `3` | `route.errors` |
| `RAEDAR_ERRORS_MIN_REQUESTS` | `20` | `route.errors`, baselines |
| `RAEDAR_SLOW_MIN_P95` | `300ms` | `route.slow` (per app: label `raedar.slow.min_p95`) |
| `RAEDAR_SLOW_FACTOR` | `2` | `route.slow` |
| `RAEDAR_SLOW_MIN_REQUESTS` | `50` | `route.slow` |
| `RAEDAR_MEMORY_LIMIT_WARN` | `90%` | `container.memory` |
| `RAEDAR_DISK_WARN` | `85%` | `host.disk`, `host.inodes` |
| `RAEDAR_DISK_CRIT` | `95%` | `host.disk`, `host.inodes` |
| `RAEDAR_DISK_FULL_WITHIN` | `3d` | `host.disk_forecast` |
| `RAEDAR_HOST_MEMORY_MIN_AVAILABLE` | `5%` | `host.memory` |
| `RAEDAR_CERT_WARN` | `14d` | `cert.expiry` |
| `RAEDAR_CERT_CRIT` | `3d` | `cert.expiry` |
| `RAEDAR_RESOLVE_AFTER` | `5m` | all persistent rules that do not define their own resolution |

## Notifications

See [notifications.md](notifications.md) for channel details.

| Variable | Default | Meaning |
|---|---|---|
| `RAEDAR_NOTIFY_WARNING` | `telegram,webhook` | Channels for warnings (unconfigured ones are skipped). |
| `RAEDAR_NOTIFY_CRITICAL` | all configured | Channels for critical incidents. |
| `RAEDAR_DIGEST_AT` | `09:00` | Time of the daily digest. Empty disables it. |
| `RAEDAR_DIGEST_CHANNELS` | `telegram,webhook` | Channels for the digest. |
| `RAEDAR_RATE_LIMIT` | `30` | Messages per hour per channel. |
| `RAEDAR_SMS_RATE_LIMIT` | `10` | Messages per hour for SMS. |
| `RAEDAR_OUTBOUND_PROXY` | — | `http://` or `socks5://` proxy for the outbound requests selected by `RAEDAR_OUTBOUND_PROXY_FOR`. |
| `RAEDAR_OUTBOUND_PROXY_FOR` | all | Which outbound requests use the proxy: `all`, or a comma-separated subset of `telegram`, `sms`, `webhook` and `heartbeat`. |
| `RAEDAR_TELEGRAM_TOKEN` | — | Bot token. |
| `RAEDAR_TELEGRAM_CHAT_IDS` | — | Chat IDs. |
| `RAEDAR_TELEGRAM_API_URL` | `https://api.telegram.org` | Bot API base URL. |
| `RAEDAR_SMS_PROVIDER` | — | `kavenegar` or `http`. |
| `RAEDAR_SMS_TO` | — | Recipient numbers. |
| `RAEDAR_KAVENEGAR_API_KEY` | — | Kavenegar API key. |
| `RAEDAR_KAVENEGAR_SENDER` | — | Kavenegar sender line. |
| `RAEDAR_SMS_HTTP_URL` | — | Generic SMS request URL. |
| `RAEDAR_SMS_HTTP_METHOD` | `POST` | Generic SMS request method. |
| `RAEDAR_SMS_HTTP_HEADERS` | — | Generic SMS headers, one `Name: value` per line. |
| `RAEDAR_SMS_HTTP_BODY` | — | Generic SMS body template. |
| `RAEDAR_SMS_HTTP_ENCODING` | `json` | `json`, `form` or `raw`. |
| `RAEDAR_WEBHOOK_URL` | — | Webhook URL. |
| `RAEDAR_WEBHOOK_SECRET` | — | Webhook signing secret. |

## Heartbeats

| Variable | Default | Meaning |
|---|---|---|
| `RAEDAR_HEARTBEAT_URL` | — | URL requested while Raedar is healthy. |
| `RAEDAR_HEARTBEAT_HEADER` | — | One header sent with it, `Name: value`. |
| `RAEDAR_HEARTBEAT_INTERVAL` | `1m` | How often. |
| `RAEDAR_HEARTBEAT_PEERS` | — | Names of peers whose heartbeats this instance expects. |
| `RAEDAR_HEARTBEAT_RECEIVE_TOKEN` | — | Bearer token peers must send. Required when `RAEDAR_HEARTBEAT_PEERS` is set. |
| `RAEDAR_HEARTBEAT_TIMEOUT` | `3m` | Silence after which a peer is reported. |

## Retention

| Variable | Default |
|---|---|
| `RAEDAR_RETENTION_MINUTE` | `14d` |
| `RAEDAR_RETENTION_PATHS` | `7d` |
| `RAEDAR_RETENTION_HOUR` | `400d` |
| `RAEDAR_RETENTION_EVENTS` | `400d` |

## Labels

Per-app behavior is set with container labels: `raedar.app`, `raedar.ignore`, `raedar.critical`, `raedar.alerts`, `raedar.latency.ignore`, `raedar.errors.min_rate`, `raedar.slow.min_p95` and `raedar.traefik`. They are described in [discovery.md](discovery.md#labels).

## Commands

The image's entry point is the `raedar` binary.

| Command | Purpose |
|---|---|
| `raedar serve` | Run the service. Default when no command is given. |
| `raedar check` | Validate configuration and test every source and channel connection (without sending messages), then print a report. |
| `raedar notify-test [--channel <name>]` | Send a test message. |
| `raedar healthcheck` | Exit 0 when `/healthz` answers `200`. Used as the container healthcheck, because the image contains no shell or curl. |
| `raedar version` | Print the version. |
