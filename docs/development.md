# Development

## Stack

| Part | Choice |
|---|---|
| Language | Go, latest stable release |
| Database driver | `github.com/jackc/pgx/v5` |
| D-Bus | `github.com/godbus/dbus/v5` |
| Docker API | a small client in the repository, using `net/http` for the handful of `GET` endpoints Raedar needs (see [data-sources.md](data-sources.md#docker)) |
| Web | `net/http`, `html/template`, plain JavaScript modules, CSS |
| Charts | uPlot, vendored |
| Container image | static binary on a `scratch` base, running as uid 65532 |

Dependencies are kept to this list. Anything else needs a strong reason.

## Code layout

```
cmd/raedar/              entry point and commands (serve, check, notify-test, healthcheck, version)
internal/config/         environment parsing, _FILE secrets, validation
internal/docker/         minimal Docker Engine API client
internal/discovery/      apps, slots, containers, routes, deploys, Traefik rule parsing
internal/source/
  accesslog/             file and container-output readers, line parsing
  containers/            container stats
  host/                  /proc and statfs
  systemd/               D-Bus reader
  tlscheck/              certificate checks
internal/aggregate/      minute buckets, latency histograms, path normalization
internal/store/          PostgreSQL access, embedded migrations, rollups, baselines, retention
internal/rules/          rule evaluation and incident lifecycle
internal/notify/         channels (telegram, kavenegar, httpsms, webhook), grouping, retries, rate limits
internal/digest/         daily digest
internal/heartbeat/      sender and receiver
internal/web/            handlers, JSON API, templates, static files
docs/                    documentation
```

Packages under `internal/source/` share one small interface: start, stop, status. Each source owns its goroutines and reports its health; nothing outside it knows how it reads its data.

## Running locally

Raedar runs without Docker during development:

```sh
export RAEDAR_DATABASE_URL=postgres://raedar:raedar@localhost:5432/raedar?sslmode=disable
export RAEDAR_DOCKER_HOST=unix:///var/run/docker.sock
export RAEDAR_HOST_ROOT=/
export RAEDAR_ACCESSLOG_PATH=./testdata/accesslog/sample.log
go run ./cmd/raedar serve
```

`testdata/accesslog/` contains recorded access logs (with made-up hosts and paths) that can be replayed to produce traffic, errors and latency spikes.

The UI's static files are served from disk instead of the embedded copy when `RAEDAR_DEV_STATIC=./internal/web/static` is set, so changes show up on reload.

## Tests

| Kind | Command | Needs |
|---|---|---|
| Unit | `go test ./...` | nothing |
| Integration | `go test -tags integration ./...` | `RAEDAR_TEST_DATABASE_URL` pointing to a disposable PostgreSQL database |

Areas that must be covered by unit tests: Traefik rule parsing, path normalization, histogram merging and percentiles, baseline computation, every rule's open/resolve logic (driven by a fake clock), intentional-stop detection from event sequences, message formatting and SMS length limits.

## Checks

`gofmt`, `go vet` and `staticcheck` must pass. CI runs them together with the unit and integration tests on every pull request.

## Releases

- Versions follow semantic versioning and are tagged `vX.Y.Z`.
- A tag builds the image for `linux/amd64` and `linux/arm64` and pushes it to `ghcr.io/mmrzaf/raedar` as `X.Y.Z`, `X.Y` and `latest`.
- Each release lists changes to configuration variables, labels and the webhook payload explicitly.
- Database migrations are forward-only; a release never edits a migration that has been released.
