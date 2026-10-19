# Deployment

Raedar runs as one more Compose project on the host it watches.

## What the host needs

| Requirement | Why |
|---|---|
| Docker with Compose | Raedar runs as a container and reads the Docker API. |
| Traefik v3 writing a JSON access log | Request, error and latency data. |
| PostgreSQL 14 or newer, reachable over a Docker network | History. A database on the host's existing PostgreSQL is enough. |
| A Docker socket proxy (recommended) | Gives Raedar read-only access to the Docker API. |
| An authentication middleware in Traefik | The UI has no login of its own. |

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

mount the directory into Raedar read-only and set `RAEDAR_ACCESSLOG_PATH`. The file must be readable by Raedar's user (UID 65532). If it is not, either make the log directory group-readable for a group Raedar is added to with `group_add`, or let Raedar read Traefik's output instead.

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

## 3. Database

On the existing PostgreSQL:

```sql
CREATE ROLE raedar LOGIN PASSWORD 'choose-a-password';
CREATE DATABASE raedar OWNER raedar;
```

Raedar creates its tables on first start.

## 4. Raedar

`compose.yml`:

```yaml
services:
  raedar:
    image: ghcr.io/mmrzaf/raedar:<version>
    container_name: raedar
    restart: unless-stopped
    read_only: true
    user: "65532:65532"
    security_opt: [no-new-privileges:true]
    cap_drop: [ALL]
    mem_limit: 128m
    cpus: 0.25
    environment:
      RAEDAR_DATABASE_URL_FILE: /run/secrets/database_url
      RAEDAR_DOCKER_HOST: tcp://socket-proxy:2375
      RAEDAR_PUBLIC_URL: https://raedar.example.com
      RAEDAR_TZ: Asia/Tehran
      RAEDAR_TELEGRAM_TOKEN_FILE: /run/secrets/telegram_token
      RAEDAR_TELEGRAM_CHAT_IDS: "123456789"
      RAEDAR_SMS_PROVIDER: kavenegar
      RAEDAR_KAVENEGAR_API_KEY_FILE: /run/secrets/kavenegar_api_key
      RAEDAR_SMS_TO: "+989120000000"
    volumes:
      - /:/host/root:ro
      - /run/dbus/system_bus_socket:/run/dbus/system_bus_socket:ro
      - ./secrets:/run/secrets:ro
      # with an access log file:
      # - /srv/logs/traefik:/traefik-logs:ro
    healthcheck:
      test: ["CMD", "/usr/local/bin/raedar", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
    labels:
      - traefik.enable=true
      - traefik.http.routers.raedar.rule=Host(`raedar.example.com`)
      - traefik.http.routers.raedar.entrypoints=websecure
      - traefik.http.routers.raedar.tls=true
      - traefik.http.routers.raedar.middlewares=admin-auth@file
      - traefik.http.services.raedar.loadbalancer.server.port=8080
    networks: [proxy, socket, data]

networks:
  proxy:
    external: true   # the network Traefik routes on
  socket:
    external: true   # the socket proxy's network
  data:
    external: true   # the network PostgreSQL is on
```

Notes:

- **Host root.** `/` is mounted read-only only to measure filesystem usage and read the hostname. Raedar runs as an unprivileged user, so it can read only world-readable files.
- **D-Bus.** Remove that line if you do not want systemd monitoring.
- **Networks.** Raedar must share a network with Traefik (for routing and TLS checks), with the socket proxy and with PostgreSQL. Use the names of your existing networks.
- **Memory.** Raedar sets `GOMEMLIMIT` to 80 % of the container's memory limit when it detects one.

## 5. Protect the UI

The UI has no login. The `middlewares` label above must point to an authentication middleware that exists in Traefik, for example basic auth from the file provider:

```yaml
http:
  middlewares:
    admin-auth:
      basicAuth:
        usersFile: /etc/traefik/users
```

An IP allow-list or a forward-auth service (Authelia, oauth2-proxy) works as well. Never publish Raedar's port directly with `ports:`.

## 6. First run

```sh
docker compose up -d
docker exec raedar raedar check
docker exec raedar raedar notify-test
```

`check` lists every source and channel with its status. Within two minutes the overview shows apps, containers and host data; request data appears as soon as traffic arrives. Baselines need three days of history; until then the fixed thresholds apply.

## Optional: receiving heartbeats from another host

To let another Raedar instance (say on `host-2`) report to this one, set on this host:

```yaml
      RAEDAR_HEARTBEAT_PEERS: host-2
      RAEDAR_HEARTBEAT_RECEIVE_TOKEN_FILE: /run/secrets/heartbeat_token
```

and add a second router that serves only `/heartbeat/` without the authentication middleware, since the token protects it:

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

Change the image tag and run `docker compose up -d`. Migrations run on start. Downgrading to a version older than the database schema is not supported.

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
