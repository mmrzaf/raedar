# Design decisions

The choices that shape Raedar, and the alternatives they were weighed against.

## One kind of server, done well

Raedar assumes Docker and Traefik and uses what they provide: Compose projects become apps, router labels become routes and domains, the access log becomes traffic data. A general-purpose tool has to be told what exists and how it relates; Raedar can read it. The cost is that Raedar is not useful outside this setup, which is accepted.

## Traefik access log instead of Traefik metrics

Traefik's Prometheus metrics give request counts and latency per router, but not per path. The access log gives the same numbers plus the path, the backend status and the exact completion time, and most Traefik setups already write it. With it, Raedar can say *which endpoint* fails or slows down, which is usually the first question. Reading the log costs some CPU per request, which is negligible at the traffic levels of a single server.

## PostgreSQL instead of SQLite or a time-series database

Hosts in this setup already run PostgreSQL, and operators already know how to back it up and inspect it. A Prometheus-style time-series database would be one more service to run and learn, and SQLite would put a database file inside a container that is otherwise stateless and read-only. Per-minute rows with hourly rollups keep PostgreSQL's size and write load small.

## Histograms, not percentiles, in storage

Storing p95 per minute makes it impossible to compute a correct p95 for an hour or a day, or for several routes together. Storing bucket counts makes every aggregation exact up to bucket resolution, and percentiles are computed when they are needed.

## Baselines from the same hour, not anomaly detection

"Usually" is defined as the median of the same hour over the last seven days. It is easy to explain in a message, needs no training, adapts within a week, and handles daily rhythms. It does not model weekly differences (quieter weekends) or holidays; fixed minimums on every rule keep that from turning into false alarms.

## Slots instead of containers

Deploys recreate containers with new IDs. Keying history by Compose service and replica keeps one continuous line per position, with deploys as markers on it, which is what makes before/after comparisons readable.

## One instance per host

A central server collecting from agents would bring a network protocol, authentication between hosts, and a single point of failure. One self-contained instance per host is simpler and keeps working when other hosts or the network between them fail. Hosts can still watch each other through heartbeats.

## Read-only

Raedar never needs write access to anything it watches. That keeps the Docker socket proxy at `GET` only, allows running without privileges, and means a mistake or compromise in Raedar cannot change the host. Restarting and redeploying belong to the tools that deploy.

## No built-in authentication

Every host in this setup already has Traefik and can put an authentication middleware in front of any service. Adding user accounts, sessions and password storage to Raedar would duplicate that and add the most security-sensitive code in the project. The cost is that a misconfigured router exposes the UI; the deployment guide makes the middleware part of the standard setup.

## Logs are viewed, not stored

Docker already keeps recent logs. Showing them live around a point on a chart covers the common need, "what was it saying at that moment", without Raedar becoming a log store with its own retention, indexing and privacy concerns.

## A small Docker client of its own

Raedar uses nine read-only endpoints. The official Docker client library is large and changes often; a few hundred lines over `net/http` are easier to audit and keep the binary small.

## Server-rendered pages without a front-end build

The UI is a handful of pages with charts. Go templates, plain JavaScript modules and uPlot are enough, keep the repository approachable for contributors, and avoid a Node.js toolchain in the build.

## Kavenegar first, generic HTTP for everything else

Kavenegar is common among Iranian operators and has a simple API, so it is supported directly. Every other SMS provider is reachable through the generic HTTP channel, which avoids maintaining a long list of provider integrations.
