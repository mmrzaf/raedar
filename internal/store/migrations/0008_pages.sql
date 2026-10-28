-- Notifications become durable obligations instead of one-shot reactions to
-- incident changes. For every incident episode, channel and recipient there is
-- exactly one page row. Silence, quiet hours, cooldown, flap, rate limiting,
-- transport failures and process restarts only move its due_at; they no longer
-- discard the page.
--
-- A new episode (an incident that resolves and reopens within the reopen
-- window) increments incidents.episode, so the old episode's pages stay as
-- history and the new one gets its own obligations.
alter table incidents add column episode bigint not null default 1;

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

-- The deliveries table stays the audit log. It now records which durable
-- obligation an attempt belongs to, which recipient it targeted, and why a
-- page was suppressed.
alter table deliveries add column page_id bigint references pages (id) on delete set null;
alter table deliveries add column recipient text not null default '';
alter table deliveries add column reason text;
alter table deliveries add column silence_id bigint references silences (id) on delete set null;
create index deliveries_page_idx on deliveries (page_id) where page_id is not null;

alter table deliveries drop constraint deliveries_status_check;
alter table deliveries add constraint deliveries_status_check
    check (status in ('pending', 'sent', 'failed', 'silenced', 'acked', 'held', 'cooldown', 'dropped'));
