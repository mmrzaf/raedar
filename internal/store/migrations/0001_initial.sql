-- The complete schema of a Raedar database. This is the only baseline;
-- later releases add their own numbered files.

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
    details jsonb not null default '{}',
    acked_at timestamptz,
    acked_by text,
    episode bigint not null default 1
);

create unique index incidents_open_idx on incidents (rule, subject) where resolved_at is null;
create index incidents_route_idx on incidents (route_id, opened_at);
create index incidents_slot_idx on incidents (slot_id, opened_at);

-- A silence prevents incident pages for one subject for a window of time.
-- The pager consults silences before paging, so an actively silenced
-- (rule, subject) sees deliveries rows with status='silenced' but no Telegram
-- message.
--
-- rule, subject and app_id may be NULL to match any; most-specific-match wins:
-- rule+subject > rule > subject > app > wildcard.
--
-- The kind column distinguishes the kinds of silences Raedar owns:
--   maintenance_window  explicit window created from the UI
--   one_shot_mute       a Telegram "mute 2h" reply or a CLI/api mute
--   auto_deploy         written automatically around a deploy of an app
create table silences (
    id bigint generated always as identity primary key,
    kind text not null check (kind in (
        'maintenance_window', 'one_shot_mute', 'auto_deploy'
    )),
    rule text,
    subject text,
    reason text not null,
    starts_at timestamptz not null,
    ends_at timestamptz not null,
    created_at timestamptz not null default now(),
    created_by text,
    dedup_key text,
    comment text,
    app_id bigint references apps (id) on delete cascade
);

create index silences_active_idx on silences (starts_at, ends_at);
create index silences_rule_subject_idx on silences (rule, subject);
create index silences_app_idx on silences (app_id);

-- A (kind, dedup_key) pair is unique. NULLs are distinct; the UI bumps the
-- dedup_key when it wants to add a second silence of the same kind.
create unique index silences_dedup_idx on silences (kind, dedup_key)
    where dedup_key is not null;

-- Telegram inbound inbox tracking.
create table telegram_inbox (
    -- The update_id we have already processed. Rows are deduped against this
    -- table on every poll. Rows older than the dedup window (kept short, see
    -- retention) are deleted by the poller after a successful run.
    update_id bigint primary key,
    processed_at timestamptz not null default now()
);

-- Notifications become durable obligations instead of one-shot reactions to
-- incident changes. For every incident episode, channel and recipient there is
-- exactly one page row. Silence, quiet hours, cooldown, flap, rate limiting,
-- transport failures and process restarts only move its due_at; they no longer
-- discard the page.
--
-- A new episode (an incident that resolves and reopens within the reopen
-- window) increments incidents.episode, so the old episode's pages stay as
-- history and the new one gets its own obligations.
create table pages (
    id bigint generated always as identity primary key,
    incident_id bigint not null references incidents (id) on delete cascade,
    episode bigint not null,
    channel text not null,
    recipient text not null default '',
    status text not null default 'pending'
        check (status in ('pending', 'announced', 'resolving', 'resolved', 'cancelled')),
    kind text not null default 'opened'
        check (kind in ('opened', 'escalated', 'resolved')),
    severity text not null,
    due_at timestamptz not null default now(),
    reason text not null default '',
    silence_id bigint references silences (id) on delete set null,
    attempts integer not null default 0,
    last_error text,
    announced_at timestamptz,
    resolved_at timestamptz,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now(),
    unique (incident_id, episode, channel, recipient)
);

create index pages_due_idx on pages (due_at)
    where status in ('pending', 'resolving');
create index pages_incident_idx on pages (incident_id, episode);

-- The deliveries table is the audit log. It records which durable obligation
-- an attempt belongs to, which recipient it targeted, and why a page was
-- suppressed.
create table deliveries (
    id bigint generated always as identity primary key,
    incident_id bigint references incidents (id) on delete cascade,
    kind text not null check (kind in ('opened', 'escalated', 'resolved', 'digest', 'test')),
    channel text not null,
    created_at timestamptz not null,
    sent_at timestamptz,
    attempts integer not null default 0,
    status text not null
        check (status in ('pending', 'sent', 'failed', 'silenced', 'acked', 'held', 'cooldown', 'dropped')),
    error text,
    telegram_chat_id bigint,
    telegram_message_id bigint,
    page_id bigint references pages (id) on delete set null,
    recipient text not null default '',
    reason text,
    silence_id bigint references silences (id) on delete set null
);

create index deliveries_incident_idx on deliveries (incident_id);
create index deliveries_created_idx on deliveries (created_at);

-- The inbox goroutine uses the (chat, message) pair to look up the incident of
-- a message the operator replied to with `mute 2h` etc. Telegram message ids
-- are only unique within one chat. One message may announce several incidents,
-- so the pair is not unique either.
create index deliveries_telegram_message_idx
    on deliveries (telegram_chat_id, telegram_message_id)
    where telegram_message_id is not null;
create index deliveries_page_idx on deliveries (page_id) where page_id is not null;

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
