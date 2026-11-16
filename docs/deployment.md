# Deployment

Raedar runs as one more Compose project on the host it watches.

## What the host needs

| Requirement | Why |
|---|---|
| Docker with Compose | Raedar runs as a container and reads the Docker API. |
| Traefik v3 writing a JSON access log | Request, error and latency data. |
| PostgreSQL 14 or newer | History. The stack in `deploy/` bundles one; an existing server works too. |
| A Docker socket proxy (recommended) | Gives Raedar read-only access to the Docker API. |
| Access control in front of the UI | The UI has no login of its own; `deploy/` ships basic auth. |

Optional: the system D-Bus socket for systemd units, and an outbound proxy if the host cannot reach Telegram or the SMS provider directly.

## 1. Traefik access log

In Traefik's static configuration:

```yaml
accessLog:
  format: json
  bufferingSize: 0
```

Without `filePath`, Traefik writes the access log to its standard output and Raedar reads it through the Docker API; nothing else is needed.

With a file:

```yaml
accessLog:
  filePath: /var/log/traefik/access.log
  format: json
  bufferingSize: 0
```

mount the directory into Raedar read-only and set `RAEDAR_ACCESSLOG_PATH`. The file must be readable by Raedar's user (UID 65534, `nobody`). If it is not, either make the log directory group-readable for a group Raedar is added to with `group_add`, or let Raedar read Traefik's output instead.

## 2. Docker socket proxy

Raedar needs only `GET` access. If the host already runs a socket proxy for Traefik, Raedar can share it when it allows the permissions below; otherwise run one for Raedar:

```yaml
services:
  socket-proxy:
    image: tecnativa/docker-socket-proxy:v0.4.0
    restart: unless-stopped
    environment:
      CONTAINERS: 1
      EVENTS: 1
      INFO: 1
      PING: 1
      VERSION: 1
      SYSTEM: 1      # optional: Docker disk usage
      POST: 0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    networks: [socket]

networks:
  socket:
    name: socket
    internal: true
```

`CONTAINERS` includes container logs, which the UI shows. Mounting the Docker socket directly into Raedar also works, but gives it full control of Docker; the proxy is strongly recommended.

## 3. Raedar, PostgreSQL and the socket proxy

[`deploy/compose.yaml`](../deploy/compose.yaml) is a complete, portable stack: Raedar, its own PostgreSQL, a read-only socket proxy, and an HTTP basic-auth middleware on Raedar's router. Hostname, Traefik network, entrypoint, certificate resolver and extra middlewares come from `deploy/.env`; application settings come from the optional `deploy/raedar.env`. [`deploy/README.md`](../deploy/README.md) is the installation guide, including the mount security table, upgrades, backup and restore, and using an existing PostgreSQL.

Optional host access (disk and inode usage, systemd) is a separate file, [`deploy/compose.host.yaml`](../deploy/compose.host.yaml); leave it out and those sources are reported as unavailable.

Notes:

- **Host root.** `/` is mounted read-only only to measure filesystem usage and read the hostname. Raedar runs as an unprivileged user, so it can read only world-readable files.
- **D-Bus.** Keep `user: "65534:65534"` (`nobody`) when you use it: the system bus only accepts connections from users that exist in the host's user database. With `RAEDAR_SYSTEMD=on` an unusable bus stops Raedar from starting; with `auto` it only costs the systemd view.
- **Networks.** Raedar must share a network with Traefik (routing, TLS checks and active checks) and with the socket proxy.
- **Memory.** Raedar sets `GOMEMLIMIT` to 80 % of the container's memory limit when it detects one.

## 4. Protect the UI

The UI has no login. The stack in `deploy/` always attaches a basic-auth middleware; extend it with `RAEDAR_EXTRA_MIDDLEWARES` (IP allow-list, forward-auth such as Authelia or oauth2-proxy). Never publish Raedar's port directly with `ports:`.

## 5. First run

```sh
docker compose up -d
docker compose exec raedar raedar check
docker compose exec raedar raedar notify-test
```

`check` lists every source and channel with its status. Within two minutes the overview shows apps, containers and host data; request data appears as soon as traffic arrives. Baselines need three days of history; until then the fixed thresholds apply.

## Optional: receiving heartbeats from another host

To let another Raedar instance (say on `host-2`) report to this one, set on this host:

```yaml
      RAEDAR_HEARTBEAT_PEERS: host-2
      RAEDAR_HEARTBEAT_RECEIVE_TOKEN_FILE: /run/secrets/heartbeat_token
```

and add a second router (for example in a `compose.override.yaml`) that serves only `/heartbeat/` without the authentication middleware, since the token protects it:

```yaml
      - traefik.http.routers.raedar-heartbeat.rule=Host(`raedar.example.com`) && PathPrefix(`/heartbeat/`)
      - traefik.http.routers.raedar-heartbeat.entrypoints=websecure
      - traefik.http.routers.raedar-heartbeat.tls=true
      - traefik.http.routers.raedar-heartbeat.service=raedar
```

On `host-2`:

```yaml
      RAEDAR_HEARTBEAT_URL: https://raedar.example.com/heartbeat/host-2
      RAEDAR_HEARTBEAT_HEADER_FILE: /run/secrets/heartbeat_header   # contains: Authorization: Bearer <token>
```

Do the same in the other direction so both hosts watch each other.

## Optional: restricted outbound access

If the host cannot reach Telegram or the SMS provider directly, set `RAEDAR_OUTBOUND_PROXY` to a proxy reachable **from inside the Raedar container**. A proxy that listens only on the host's `127.0.0.1` is not reachable from a container. Either make it also listen on the gateway address of one of Raedar's networks (and restrict that port with the firewall), or run the proxy as a container on a network Raedar shares.

Only some requests may need the proxy. `RAEDAR_OUTBOUND_PROXY_FOR` lists the ones that use it: `telegram`, `sms`, `webhook` and `heartbeat` (`all` by default). For example, a host that reaches its SMS provider directly but not Telegram sets:

```yaml
      RAEDAR_OUTBOUND_PROXY: http://172.27.0.1:1080
      RAEDAR_OUTBOUND_PROXY_FOR: telegram,webhook,heartbeat
```

`raedar check` lists each configured channel and whether it goes through the proxy.

## Optional: hosts with resource slices

On hosts where containers run under a shared systemd slice (`cgroup_parent`), add Raedar to it like any other app:

```yaml
    cgroup_parent: workloads.slice
```

## Upgrading

Pull the new image (or change `RAEDAR_IMAGE`) and run `docker compose up -d`. Back up the database first (see `deploy/README.md`). Migrations run on start. Downgrading to a version older than the database schema is not supported.

## Removing

`docker compose down`, then drop the `raedar` database and role. Raedar leaves nothing else on the host.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| No apps on the overview | Docker API not reachable: check `RAEDAR_DOCKER_HOST` and that Raedar shares a network with the socket proxy. |
| Apps but no traffic | The access log is not JSON, or Raedar reads the wrong source. `raedar check` shows which mode is used and the last line read. |
| Traffic only under router names, not apps | Routers are defined in Traefik's file provider; they are attributed to the Traefik app. |
| Request data arrives minutes late | Traefik's `bufferingSize` is large; set it to 0. |
| No certificates | `RAEDAR_TLS_CHECK_ADDR` could not be detected; set it to Traefik's HTTPS address on a shared network. |
| No disk data | `/` is not mounted at `RAEDAR_HOST_ROOT`. |
| No systemd units | The D-Bus socket is not mounted, or `RAEDAR_SYSTEMD=off`. |
| Telegram test fails with a timeout | Outbound access is blocked; see "restricted outbound access". |
