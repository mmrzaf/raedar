# Deploying Raedar

A portable Docker Compose stack: Raedar, its own PostgreSQL, and a read-only Docker socket proxy. It joins your existing Traefik; nothing about your host layout is assumed.

## Prerequisites

- Docker with Compose v2, and Traefik v3 running as a container on a Docker network (default name `traefik`, override with `TRAEFIK_NETWORK`).
- Traefik JSON access logs on stdout (`accessLog.format: json`, no `filePath`), or a log file you mount (see below).

## Install

```sh
cp .env.example .env && chmod 600 .env
# set RAEDAR_DOMAIN, RAEDAR_DB_PASSWORD and RAEDAR_BASIC_AUTH in .env
# optional app settings (notifications, timezone, probes): cp raedar.env.example raedar.env
docker compose up -d
docker compose exec raedar raedar check
```

Add `-f compose.yaml -f compose.host.yaml` to every `docker compose` command to also read host disk usage and systemd (read-only mounts of `/` and the D-Bus socket). Skip it for the smallest install; those sources then show as unavailable in the UI instead of empty.

## Access control

Raedar has no login. `compose.yaml` attaches an HTTP basic-auth middleware (`raedar-auth`) to the router so a fresh install is never open. Replace or extend it with `RAEDAR_EXTRA_MIDDLEWARES` (for example an IP allow-list or forward-auth middleware). Do not publish port 8080.

## Security of the mounts

| Mount | Why | Risk | Disable |
|---|---|---|---|
| `socket-proxy` (GET only: containers, events, info, version, system) | discovery, stats, lifecycle, logs | container logs and environment-free inspect data are readable; no writes | `SOCKET_PROXY_SYSTEM=0` drops disk usage |
| `/` read-only (`compose.host.yaml`) | disk/inode usage, hostname | world-readable host files | leave the file out |
| D-Bus socket (`compose.host.yaml`) | systemd units/timers | read of unit state | `RAEDAR_SYSTEMD=off` |

Container `inspect` output can contain environment variables; Raedar stores only identity, image, state, health and network facts, never environment values.

## Access log from a file

```yaml
# compose.override.yaml
services:
  raedar:
    volumes: ["/var/log/traefik:/logs:ro"]
    environment: {RAEDAR_ACCESSLOG_PATH: /logs/access.log}
```

The file must be readable by uid 65534.

## Upgrade, backup, restore

```sh
docker compose pull && docker compose up -d        # migrations run on start
docker compose exec -T postgres pg_dump -U raedar raedar | gzip > raedar-$(date +%F).sql.gz
gunzip -c raedar-DATE.sql.gz | docker compose exec -T postgres psql -U raedar -d raedar   # into an empty database
```

Migrations are forward-only; back up before a release that changes the schema, and do not run an older image against a newer schema.

## Using an existing PostgreSQL

Remove the `postgres` service, drop its `depends_on` entry and set `RAEDAR_DATABASE_URL` yourself (use `RAEDAR_DATABASE_URL_FILE` to keep the password in a secret file). PostgreSQL 14 or newer is required.
