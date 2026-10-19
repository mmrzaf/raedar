-- Idempotent role and database creation for Raedar.
--
-- Run it through the shared Postgres container after filling .env:
--
--   cd /srv/apps/raedar
--   set -a; . ./.env; set +a
--   docker exec -i -e PGPASSWORD="$(docker exec postgres printenv POSTGRES_PASSWORD)" postgres \
--     psql -U postgres -v ON_ERROR_STOP=1 \
--     -v db="$APP_DB_NAME" -v user="$APP_DB_USER" -v password="$APP_DB_PASSWORD" \
--     < db/init.sql
--
-- Rerunning it resets the role's password to the value in .env.
\set ON_ERROR_STOP on

SELECT format('CREATE ROLE %I LOGIN', :'user')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'user')\gexec

ALTER ROLE :"user" LOGIN PASSWORD :'password';

SELECT format('CREATE DATABASE %I OWNER %I', :'db', :'user')
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = :'db')\gexec
