-- Active availability checks: synthetic requests Raedar sends to Traefik for
-- routes it discovered. They are kept apart from the passive route_* tables,
-- which describe real user traffic from the access log.

-- The current verdict per route. state is what incidents act on, after the
-- check runner applied its hysteresis; last_result is the latest single check.
create table probe_state (
    route_id bigint primary key references routes (id) on delete cascade,
    state text not null check (state in ('healthy', 'unhealthy', 'unreachable', 'unknown')),
    since timestamptz not null,
    last_result text not null check (last_result in ('healthy', 'unhealthy', 'unreachable')),
    last_checked timestamptz not null,
    status_code integer,
    latency_ms integer,
    error text not null default '',
    target text not null default '',
    consecutive_failures integer not null default 0,
    consecutive_successes integer not null default 0
);

-- Outcomes per route and minute. Counts are additive, like route_minute.
create table probe_minute (
    route_id bigint not null references routes (id) on delete cascade,
    ts timestamptz not null,
    checks integer not null default 0,
    healthy integer not null default 0,
    unhealthy integer not null default 0,
    unreachable integer not null default 0,
    latency_ms_max integer,
    primary key (route_id, ts)
);

create table probe_hour (
    route_id bigint not null references routes (id) on delete cascade,
    ts timestamptz not null,
    checks integer not null default 0,
    healthy integer not null default 0,
    unhealthy integer not null default 0,
    unreachable integer not null default 0,
    latency_ms_max integer,
    primary key (route_id, ts)
);

create index probe_minute_ts_idx on probe_minute (ts);
create index probe_hour_ts_idx on probe_hour (ts);
