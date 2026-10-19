# Raedar on the Waiotech host

Raedar runs next to the other apps on `fastcreate-733261`, behind the existing
Traefik and on the shared PostgreSQL. A `v*` tag makes Gitman build
`raedar:<version>` on the host and recreate the stack. GitHub is only a mirror
and publishes the same version independently.

## Requirements on the host

- The `golang:1.27-alpine` image for the build stage (the Gitman pipeline waits
  for it); the runtime image needs no base.
- The `proxy`, `socket`, `data` and `zld_panel_egress` networks.
- Traefik with the `security-headers`, `arvancloud-only` and `admin-auth`
  middlewares, and the wildcard certificate that covers `*.waiotech.com`.
- The socket proxy (`tcp://socket-proxy:2375`); Docker disk usage stays empty
  because `/system/df` is not permitted.
- The Traefik JSON access log at `/srv/logs/traefik/access.log`.

## First deploy

```sh
cd /srv/apps/raedar
cp .env.example .env
chmod 600 .env
# Fill in APP_DB_PASSWORD (openssl rand -hex 32), the channels and any optional
# settings. Never commit .env.
docker exec -i -e PGPASSWORD="$(docker exec postgres printenv POSTGRES_PASSWORD)" postgres \
  psql -U postgres -v ON_ERROR_STOP=1 \
  -v db=raedar -v user=raedar -v password="$(grep '^APP_DB_PASSWORD=' .env | cut -d= -f2-)" \
  < db/init.sql
```

The Gitman pipeline builds the image, writes `docker-compose.yml`, `README.md`,
`.env.example` and `db/init.sql`, points `APP_IMAGE` at the pinned image and
recreates the stack. The first deploy needs the `.env` above to exist.

## Verify

```sh
docker compose ps
docker exec raedar raedar healthcheck
docker exec raedar raedar check
docker exec raedar raedar notify-test
curl -sk -H 'Host: raedar.waiotech.com' https://127.0.0.1/ -o /dev/null -w '%{http_code}\n'
```

From the host, a `403` from `arvancloud-only` (or a `401` from `admin-auth`)
proves Traefik routed the request; `404` means the router is missing and `503`
means the container is unhealthy. Open `https://raedar.waiotech.com` with the
admin credentials.

Within two minutes the overview shows apps, containers and host data. Request
data appears as traffic arrives. Baselines need three days.

## Updates and rollback

A release is a `v*` tag on a commit of `main` that passed CI, with `VERSION`
matching the tag. Push the tag to Gitman (it builds and deploys it); pushing it
to GitHub publishes the portable image and the release. Roll back by rerunning
an older successful run in Gitman, or by pinning an older `APP_IMAGE` and
running `docker compose up -d --no-build --pull never`.

Database migrations are forward-only. Back up the database before a release
that changes the schema (`~/dev/servers/waiotech/scripts/backup.sh`); an older
image cannot read a newer schema.

## Data

Raedar keeps everything in PostgreSQL; it has no local state. The container
logs go through the host's json-file rotation. The Traefik access log is
mounted read-only and is never copied.
