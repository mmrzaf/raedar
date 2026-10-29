# Overview

Raedar is a monitoring and alerting service for a server whose workloads all run in Docker and are published through Traefik. It runs as a single container on that server, keeps its history in PostgreSQL, and never changes anything it watches.

## What it is for

Raedar is built around two moments.

1. **Something goes wrong.** You get one message that says what broke, how far it is from normal, since when, and the most useful lead: a deploy a few minutes earlier, or the endpoint producing the errors. When it recovers, you get one more message.
2. **You open it.** Within seconds you know whether everything is fine. Within a minute you can see why something happened, or when it tends to happen: the same latency bump every evening, a disk that grows 300 MB a day, a job that fails every night at 02:15.

## Principles

- **Fit one setup well.** Docker and Traefik are assumed, not abstracted away. Raedar uses what that setup already provides (Compose labels, Traefik labels, the Traefik access log) instead of asking you to describe your server.
- **Discover, don't configure.** Apps, containers, routes and domains appear on their own. Configuration exists for credentials, channels and thresholds, never for listing what to monitor.
- **Compare with normal.** Error rates and latency are judged against the same route's own history, so no app needs tuning.
- **Few, meaningful alerts.** One message when a problem starts, one when it ends. Problems that start together arrive together.
- **Read-only.** Raedar observes. There is nothing in it that restarts, stops, deletes or edits.
- **Small.** One process in one container, designed to stay under 64 MB of memory on a typical small server.

## Concepts

| Term | Meaning |
|---|---|
| Host | The server Raedar runs on. One Raedar instance watches one host. |
| App | A group of containers, normally one Docker Compose project. Everything in the UI is organized by app. |
| Slot | One position inside an app: a Compose service and its replica number. A slot's history continues across container recreations and deploys. |
| Container | A Docker container currently or previously filling a slot. |
| Route | A Traefik router, identified as `name@provider` (for example `shop@docker`), attributed to an app. |
| Domain | A hostname taken from a route rule (`Host`, `HostSNI`). Certificates are checked per domain. |
| Deploy | A slot whose container is recreated with a different image. |
| Event | Something that happened at one point in time: a deploy, a crash, a health change, a failed systemd unit, an incident starting or ending. |
| Incident | A rule condition that holds for a subject (a route, slot, disk, unit, certificate or peer) over time. It has a start and, eventually, an end. |
| Baseline | What is normal for a route at a given hour of the day, learned from the previous seven days. |
| Source | One place Raedar reads data from: the access log, Docker, the host, systemd, TLS checks. |

## An example

At 21:04 the error rate on `git.example.com` climbs from its usual 0.1 % to 8 %. Two minutes later a Telegram message arrives:

```
🔴 host-1 · gitman — 5xx errors
8.2% of requests fail (usual at this hour: 0.1%)
since 21:04 · 6 min after deploy gitman 1.4.1 → 1.4.2
most errors: POST /api/repos/:n/push
https://raedar.example.com/apps/gitman?from=…&to=…
```

The link opens the gitman page on that time window. The error line jumps right after the deploy marker. Clicking the spike shows the gitman container's logs from that minute. After a rollback the rate falls back, and at 21:25 a second message says the incident is resolved after 21 minutes.

The next morning's digest mentions that `/` is at 81 % and growing about 0.4 GB a day, full in roughly 12 days. On the Patterns page, the latency heatmap for the same app shows a clear band every night between 02:00 and 03:00. Adding the Postgres container's CPU to the gitman charts shows that it lines up with the nightly backup.

## Out of scope

- **Several hosts in one UI.** Each host runs its own instance. Instances can watch each other through heartbeats.
- **Other platforms.** Kubernetes, Swarm, reverse proxies other than Traefik, and metrics exported from inside applications.
- **Actions.** Restarting, stopping, pruning, editing.
- **Log storage and search.** Raedar shows container logs live from Docker and keeps none.
- **Custom dashboards, query languages and plugins.**
- **Built-in authentication.** The UI is served behind a Traefik authentication middleware.
- **Uptime checks from the internet.** Use the outgoing heartbeat together with an external service or a second Raedar instance.
