# Security

Raedar sees a lot of the host it monitors: container metadata, container logs, request paths, systemd state. This document describes what it can access, what it keeps, and what the operator must do to keep it safe.

## What Raedar can access

| Access | Through | Scope |
|---|---|---|
| Docker API | a socket proxy allowing `GET` only (recommended) | container metadata, stats, events and logs; no changes |
| Host filesystem | `/` mounted read-only | used for filesystem statistics and the hostname; as UID 65532, only world-readable files are readable |
| systemd | the system D-Bus socket | unit and timer state; changing units requires privileges Raedar does not have |
| Traefik | the access log, and TLS handshakes on its HTTPS port | read-only |
| PostgreSQL | its own database and role | full access to that database only |

The container runs with a read-only root filesystem, no Linux capabilities, `no-new-privileges` and an unprivileged user. Raedar writes nothing to disk.

If the Docker socket is mounted directly instead of through a proxy, Raedar's own code still only reads, but anyone who takes over the Raedar process controls Docker, and with it the host. Use the proxy.

## What Raedar keeps

Stored in PostgreSQL:

- apps, slots, containers (names, image references, timestamps), routes and their rules
- per-minute and per-hour measurements
- normalized request path patterns per route (`/orders/:n`), with counts and latency
- events, incidents, delivery records, certificate check results, systemd unit states

Never stored:

- client IP addresses, user agents, headers, query strings, request or response bodies
- container environment variables (present in Docker's inspect responses, discarded on receipt)
- container logs (read live from Docker only while someone is viewing them)

Path normalization replaces numbers, UUIDs, long hex strings and token-like segments, which removes most identifiers that appear in URLs. A path segment that carries personal data in another form (for example a username) is stored as is.

## Exposure

**The UI has no authentication.** It shows container logs, which can contain secrets, and the structure of everything running on the host. It must only be reachable through a Traefik router with an authentication middleware (basic auth, forward auth, or an IP allow-list for a trusted network). Never publish Raedar's port on the host.

The UI is read-only: apart from the heartbeat receiver, every endpoint is `GET`, and none of them changes anything on the host, in Docker or in Raedar's configuration.

**The heartbeat receiver** (`/heartbeat/<name>`) can be routed without the authentication middleware so that peers can reach it. It accepts only names listed in `RAEDAR_HEARTBEAT_PEERS`, requires the bearer token `RAEDAR_HEARTBEAT_RECEIVE_TOKEN` (compared in constant time), and can do nothing except record that a peer is alive. It is rate-limited per peer.

## Outbound connections

Raedar connects only to:

- the Docker API, PostgreSQL and Traefik, as configured
- the configured notification channels
- the configured heartbeat URL

It sends no telemetry and does not check for updates.

Messages leave the host through third-party services (Telegram, the SMS provider, the webhook target). They contain app names, domains, path patterns, exit codes and numbers, never logs.

## Secrets

- Every variable can be read from a file (`<NAME>_FILE`), which keeps secrets out of `docker inspect` output and Compose files.
- Secrets are never written to logs. The `/about` page shows configuration with secret values masked.
- Request URLs for the Kavenegar API are never logged, because the API key is part of the URL.
- Webhook requests can be signed with `RAEDAR_WEBHOOK_SECRET` so the receiver can verify they come from Raedar.

## Reporting a vulnerability

See [SECURITY.md](../SECURITY.md).
