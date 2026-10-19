-- The model of the host: apps, slots, containers and routes.
create table apps (
    id bigint generated always as identity primary key,
    name text not null unique,
    first_seen timestamptz not null,
    last_seen timestamptz not null,
    archived_at timestamptz
);

create table slots (
    id bigint generated always as identity primary key,
    app_id bigint not null references apps (id) on delete cascade,
    key text not null,
    service text not null,
    replica integer not null,
    first_seen timestamptz not null,
    last_seen timestamptz not null,
    unique (app_id, key)
);

create table containers (
    docker_id text primary key,
    slot_id bigint not null references slots (id) on delete cascade,
    name text not null,
    image_ref text not null,
    image_id text not null,
    created_at timestamptz,
    started_at timestamptz,
    removed_at timestamptz
);

create index containers_slot_idx on containers (slot_id);

create table routes (
    id bigint generated always as identity primary key,
    name text not null unique,
    app_id bigint references apps (id) on delete set null,
    kind text not null check (kind in ('http', 'tcp')),
    rule text not null default '',
    domains text[] not null default '{}',
    tls boolean not null default false,
    passthrough boolean not null default false,
    origin text not null check (origin in ('labels', 'accesslog')),
    first_seen timestamptz not null,
    last_seen timestamptz not null
);

create index routes_app_idx on routes (app_id);

create table peers (
    name text primary key,
    last_heartbeat timestamptz not null
);

create table source_state (
    source text primary key,
    position jsonb,
    status text not null,
    last_error text,
    updated_at timestamptz not null
);

-- Measurements. route_minute holds a row only for minutes with at least one
-- request; ts is always the start of the minute, hour or window, in UTC.
create table route_minute (
    route_id bigint not null references routes (id) on delete cascade,
    ts timestamptz not null,
    requests bigint not null default 0,
    s1xx bigint not null default 0,
    s2xx bigint not null default 0,
    s3xx bigint not null default 0,
    s4xx bigint not null default 0,
    s5xx bigint not null default 0,
    no_backend bigint not null default 0,
    bytes bigint not null default 0,
    latency integer[] not null default array_fill(0, array[21], array[1]),
    primary key (route_id, ts)
);

create index route_minute_ts_idx on route_minute (ts);

create table route_hour (
    route_id bigint not null references routes (id) on delete cascade,
    ts timestamptz not null,
    requests bigint not null default 0,
    s1xx bigint not null default 0,
    s2xx bigint not null default 0,
    s3xx bigint not null default 0,
    s4xx bigint not null default 0,
    s5xx bigint not null default 0,
    no_backend bigint not null default 0,
    bytes bigint not null default 0,
    latency integer[] not null default array_fill(0, array[21], array[1]),
    primary key (route_id, ts)
);

create index route_hour_ts_idx on route_hour (ts);

create table path_window (
    route_id bigint not null references routes (id) on delete cascade,
    ts timestamptz not null,
    method text not null,
    pattern text not null,
    requests bigint not null default 0,
    s4xx bigint not null default 0,
    s5xx bigint not null default 0,
    latency integer[] not null default array_fill(0, array[21], array[1]),
    primary key (route_id, ts, method, pattern)
);

create index path_window_ts_idx on path_window (ts);

create table slot_minute (
    slot_id bigint not null references slots (id) on delete cascade,
    ts timestamptz not null,
    cpu_millicores integer not null default 0,
    mem_bytes bigint not null default 0,
    mem_limit_bytes bigint,
    net_rx_bytes bigint not null default 0,
    net_tx_bytes bigint not null default 0,
    running boolean not null default false,
    primary key (slot_id, ts)
);

create index slot_minute_ts_idx on slot_minute (ts);

create table slot_hour (
    slot_id bigint not null references slots (id) on delete cascade,
    ts timestamptz not null,
    cpu_avg double precision not null default 0,
    cpu_max integer not null default 0,
    mem_avg double precision not null default 0,
    mem_max bigint not null default 0,
    mem_limit_bytes bigint,
    net_rx_bytes bigint not null default 0,
    net_tx_bytes bigint not null default 0,
    primary key (slot_id, ts)
);

create index slot_hour_ts_idx on slot_hour (ts);

create table host_minute (
    ts timestamptz primary key,
    cpu_percent double precision not null,
    load1 double precision not null,
    load5 double precision not null,
    load15 double precision not null,
    mem_total bigint not null,
    mem_available bigint not null,
    swap_total bigint not null,
    swap_used bigint not null
);

create table host_hour (
    ts timestamptz primary key,
    cpu_avg double precision not null,
    cpu_max double precision not null,
    load1_avg double precision not null,
    load1_max double precision not null,
    mem_available_min bigint not null,
    mem_available_avg double precision not null,
    swap_used_max bigint not null
);

create table disk_minute (
    disk text not null,
    ts timestamptz not null,
    size_bytes bigint not null,
    used_bytes bigint not null,
    avail_bytes bigint not null,
    inodes bigint not null,
    inodes_used bigint not null,
    primary key (disk, ts)
);

create index disk_minute_ts_idx on disk_minute (ts);

create table disk_hour (
    disk text not null,
    ts timestamptz not null,
    size_bytes bigint not null,
    used_bytes_max bigint not null,
    avail_bytes_min bigint not null,
    inodes bigint not null,
    inodes_used_max bigint not null,
    primary key (disk, ts)
);

create index disk_hour_ts_idx on disk_hour (ts);

create table docker_disk (
    ts timestamptz primary key,
    images_bytes bigint not null,
    images_reclaimable_bytes bigint not null,
    build_cache_bytes bigint not null,
    volumes_bytes bigint not null,
    containers_bytes bigint not null
);

-- State and history.
create table units (
    name text primary key,
    active_state text not null,
    sub_state text not null,
    result text not null,
    exit_status integer,
    since timestamptz,
    last_seen timestamptz not null
);

create table certs (
    domain text primary key,
    route_id bigint references routes (id) on delete set null,
    checked_at timestamptz not null,
    status text not null,
    not_after timestamptz,
    issuer text not null default '',
    subject text not null default '',
    sans text[] not null default '{}',
    error text
);

create table baselines (
    route_id bigint not null references routes (id) on delete cascade,
    hour_of_day integer not null check (hour_of_day between 0 and 23),
    error_rate double precision,
    p95_ms double precision,
    days_used integer not null default 0,
    computed_at timestamptz not null,
    primary key (route_id, hour_of_day)
);

create table events (
    id bigint generated always as identity primary key,
    ts timestamptz not null,
    kind text not null,
    severity text not null,
    app_id bigint references apps (id) on delete set null,
    slot_id bigint references slots (id) on delete set null,
    route_id bigint references routes (id) on delete set null,
    subject text not null,
    title text not null,
    data jsonb not null default '{}'
);

create index events_ts_idx on events (ts);
create index events_app_idx on events (app_id, ts);
create index events_slot_idx on events (slot_id, ts);
create index events_route_idx on events (route_id, ts);

create table incidents (
    id bigint generated always as identity primary key,
    rule text not null,
    subject text not null,
    app_id bigint references apps (id) on delete set null,
    slot_id bigint references slots (id) on delete set null,
    route_id bigint references routes (id) on delete set null,
    severity text not null check (severity in ('warning', 'critical')),
    opened_at timestamptz not null,
    resolved_at timestamptz,
    one_shot boolean not null default false,
    summary text not null,
    details jsonb not null default '{}'
);

create unique index incidents_open_idx on incidents (rule, subject) where resolved_at is null;
create index incidents_route_idx on incidents (route_id, opened_at);
create index incidents_slot_idx on incidents (slot_id, opened_at);

create table deliveries (
    id bigint generated always as identity primary key,
    incident_id bigint references incidents (id) on delete cascade,
    kind text not null check (kind in ('opened', 'resolved', 'digest', 'test')),
    channel text not null,
    created_at timestamptz not null,
    sent_at timestamptz,
    attempts integer not null default 0,
    status text not null,
    error text
);

create index deliveries_incident_idx on deliveries (incident_id);
create index deliveries_created_idx on deliveries (created_at);

-- Histogram helpers.
create function raedar_hist_add(a int[], b int[]) returns int[]
language sql immutable strict parallel safe as $$
    select coalesce(
        (select array_agg(a[i] + b[i] order by i)
           from generate_subscripts(a, 1) as sub(i)),
        '{}'::int[])
$$;

create aggregate raedar_hist_sum(int[]) (
    sfunc = raedar_hist_add,
    stype = int[],
    initcond = '{0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0}'
);
