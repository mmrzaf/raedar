# Raedar

Monitoring and alerting for a server that runs everything in Docker behind Traefik.

Raedar runs as one container next to your apps. It finds your apps from Docker Compose, your routes and domains from Traefik labels, reads the Traefik access log, and watches containers, the host, systemd jobs and TLS certificates. It tells you when something goes wrong and shows you how your server behaves over time.

## What it does

- **Alerts that say something useful.** Error rate up, responses slower than usual for this hour, a route with no backend, a container crashing or restart-looping, a disk filling up, a failed backup job, a certificate about to expire. One message when a problem starts, one when it ends, with context: the deploy that preceded it, the endpoint behind it.
- **Telegram, SMS and webhooks.** SMS through Kavenegar or any provider with an HTTP API. Critical incidents can go to SMS while everything else goes to Telegram.
- **Nothing to configure per app.** New apps, routes and domains appear on their own. Error and latency alerts compare each route with its own normal behavior.
- **History you can read.** Requests, errors, latency, CPU and memory per app on one time axis, with deploys and crashes marked on it. Click any moment to see the container logs from then.
- **Patterns.** Week heatmaps, daily profiles and long-term trends show when things tend to happen.
- **Read-only and small.** One process, read-only access to Docker through a socket proxy, history in your existing PostgreSQL.

## Requirements

- Docker with Compose
- Traefik v3 with a JSON access log
- PostgreSQL 14 or newer
- A Docker socket proxy (recommended)

## Documentation

Start with the [overview](docs/overview.md), or see [all documents](docs/README.md). Installation is described in [deployment](docs/deployment.md).

## Status

Raedar is in early development. The design is described in [docs](docs/README.md), and the [changelog](CHANGELOG.md) lists what changed in each release.

## License

[MIT](LICENSE)
