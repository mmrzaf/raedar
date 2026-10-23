# Discovery

Raedar builds its model of the server from the Docker API. Nothing is listed in configuration. Labels adjust the result where the defaults are not enough.

## Apps

Each container belongs to exactly one app. The app name is the first match of:

1. the `raedar.app` label on the container
2. the `com.docker.compose.project` label
3. the container name, for standalone containers

Two kinds of containers are not tracked at all: containers with `raedar.ignore=true`, and one-off containers started by `docker compose run` (`com.docker.compose.oneoff=True`), such as migrations and shells. They have no app, slot, samples, routes, events or alerts. An app whose containers are all untracked does not exist. An app that has had no containers for 7 days is archived: it disappears from the overview and its history stays until retention removes it. If it gets containers again, it reappears with its history.

## Slots

A slot is a stable position inside an app, so that history does not break when containers are recreated. Its key is:

- `com.docker.compose.service` plus `com.docker.compose.container-number` for Compose containers (for example `api #1`)
- the container name for standalone containers

Charts, metrics and incidents for containers are keyed by slot, not by Docker container ID. The container that fills a slot is recorded with its ID, name, image reference, image ID, creation time and start time.

## Deploys

When a new container appears in a slot, Raedar compares its image ID with the image ID of the previous container Raedar recorded in that slot. The previous container is read from the database, so a deploy is still found when the old container was removed before the new one was created (`docker rm -f` followed by `docker run`) or while Raedar was down:

- **Different image** → a `deploy` event, recording the old and new image references (`Config.Image`) and short image IDs.
- **Same image** → a `recreated` event.

Deploys of several slots in the same app within 2 minutes are merged into one deploy event that lists the services involved. Deploy events appear as markers on every chart of the app and are mentioned in alert messages for incidents that start within 30 minutes after them.

## Container lifecycle events

Raedar follows the Docker event stream for containers (`create`, `start`, `kill`, `die`, `stop`, `destroy`, `oom`, `health_status`, `restart`, `rename`). From it, it derives the events below for tracked containers. A container is placed in its app and slot by its labels, so a container that was created after the last resync is attributed correctly. Events of containers outside the model, such as short-lived containers of build or deploy tools, are dropped.

| Event | Meaning |
|---|---|
| `crashed` | The container exited on its own with a non-zero exit code. Includes the exit code and whether it was killed for running out of memory. |
| `stopped` | The container was stopped on purpose (`kill`/`stop` before `die`). |
| `started` | The container started, either the first time or after a restart. A restart is recorded once. |
| `health` | The health status changed (`starting`, `healthy`, `unhealthy`). |
| `removed` | The container was destroyed. |

A `die` counts as intentional when Docker emitted a `kill` or `stop` event for the same container within 30 seconds before it, or a `stop` right after it. That is what `docker stop`, `docker kill`, `docker compose down` and redeploys produce. Anything else is a crash.

## Traefik

Raedar finds the Traefik container by the first match of:

1. `RAEDAR_TRAEFIK_CONTAINER` (container name)
2. a container with the label `raedar.traefik=true`
3. a running container whose image repository ends in `traefik` (`traefik`, `library/traefik`, `docker.io/traefik`). If several match, the one publishing port 443 is used.

The Traefik container is used for two things: reading its output when the access log goes to stdout, and finding the address for TLS checks (the container-side port published as host port 443, reached over a network Raedar shares with Traefik).

## Routes

### From labels

For every container that is not labeled `traefik.enable=false`, Raedar reads Traefik router labels:

| Label | Used for |
|---|---|
| `traefik.http.routers.<name>.rule` | HTTP route and its domains |
| `traefik.tcp.routers.<name>.rule` | TCP route and its domains |
| `traefik.<http\|tcp>.routers.<name>.entrypoints` | display |
| `traefik.<http\|tcp>.routers.<name>.tls` and `.tls.certresolver` | whether domains get TLS checks |
| `traefik.tcp.routers.<name>.tls.passthrough` | passthrough routes are not TLS-checked |
| `traefik.<http\|tcp>.routers.<name>.service` | display |

The route's ID is `<name>@docker`, matching the `RouterName` field in the access log, and the route belongs to the container's app. Compose has already substituted environment variables into label values, so rules read from the Docker API contain real hostnames even when the Compose file uses variables.

### From the access log

A router name seen in the access log that is not known from labels is added when first seen:

- If it ends in `@docker` and its name equals Traefik's default router name for a container (`<service>-<project>`), it belongs to that container's app.
- Otherwise (`@file`, `@internal`, and anything else) it belongs to the app that runs Traefik.

Requests that match no router at all are recorded under the synthetic route `(unrouted)`, which also belongs to the Traefik app.

Routes from the access log have no known rule, so their domains are unknown and they get no TLS checks. Domains served by file-provider routers can be added to TLS checks with `RAEDAR_TLS_DOMAINS`.

### Rule parsing

Domains are taken from the arguments of `Host(...)` and `HostSNI(...)` matchers anywhere in a rule, in both the Traefik v3 form (`` Host(`a.example.com`) || Host(`b.example.com`) ``) and the v2 form (`` Host(`a.example.com`, `b.example.com`) ``). `HostRegexp`, `HostSNIRegexp` and `` HostSNI(`*`) `` contribute no domains. Other matchers (`PathPrefix`, `Method`, …) are kept in the rule text for display only.

### Route kinds

| Kind | Request metrics | TLS check |
|---|---|---|
| HTTP router with TLS | yes | yes |
| HTTP router without TLS | yes | no |
| TCP router with TLS terminated by Traefik | no (the access log covers HTTP only) | yes |
| TCP router with TLS passthrough | no | no (the certificate belongs to the backend) |

## Labels

All labels are optional.

| Label | Set on | Effect |
|---|---|---|
| `raedar.app=<name>` | container | Put the container in this app instead of its Compose project. |
| `raedar.ignore=true` | container | Do not monitor this container: no slot, samples, events, routes or alerts. An app whose containers all carry it does not appear. |
| `raedar.critical=true` | any container of an app | The app's availability incidents are critical. See [alerts.md](alerts.md#severity). |
| `raedar.alerts=false` | any container of an app | Record everything for the app but send no alerts for it. Incidents still appear in the UI. |
| `raedar.latency.ignore=/ws,/events` | any container of an app | Path prefixes excluded from latency for the app's routes, for WebSockets, server-sent events and long downloads. |
| `raedar.errors.min_rate=5%` | any container of an app | Overrides `RAEDAR_ERRORS_MIN_RATE` for the app's routes. |
| `raedar.slow.min_p95=800ms` | any container of an app | Overrides `RAEDAR_SLOW_MIN_P95` for the app's routes. |
| `raedar.traefik=true` | the Traefik container | Marks Traefik when it cannot be detected by image name. |

App-level labels are read from all containers of the app. If two containers disagree, the most recently created container wins.

The catch-all file router (`fallback@*`) and the synthetic `(unrouted)` route are not attached to an app, even though they are defined by Traefik or seen in its access log. Their traffic is still recorded, but it does not count toward the Traefik app's request or error numbers and never pages.
