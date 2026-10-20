-- Telegram inbound inbox tracking, ack metadata on incidents, and the wider
-- deliveries.status and .kind check constraints used by the new paging gate.

create table telegram_inbox (
    -- The update_id we have already processed. Rows are deduped against this
    -- table on every poll. Rows older than the dedup window (kept short, see
    -- retention) are deleted by the poller after a successful run.
    update_id bigint primary key,
    processed_at timestamptz not null default now()
);

-- Ack metadata on incidents. Both columns stay on the row after the
-- incident resolves, so a resolved message can still say
-- "acked by you 12 min before close".
alter table incidents add column acked_at timestamptz;
alter table incidents add column acked_by text;

-- Widen the deliveries.status check: in addition to 'pending', 'sent' and
-- 'failed', the paging gate writes 'silenced', 'cooldown', 'held', 'stand_down'
-- and 'escalated_blocked' so the deliveries page reflects every reason a page
-- did not happen. Drop the existing constraint if present so the migration is
-- idempotent against upgrades from versions where it was named differently.
do $$
declare name text;
begin
    for name in
        select conname from pg_constraint
        where conrelid = 'deliveries'::regclass
          and contype = 'c'
          and pg_get_constraintdef(oid) like 'CHECK ((status = ANY (ARRAY[%'
    loop
        execute 'alter table deliveries drop constraint ' || quote_ident(name);
    end loop;
end$$;
alter table deliveries add constraint deliveries_status_check
    check (status in ('pending', 'sent', 'failed', 'silenced', 'cooldown', 'held', 'stand_down', 'escalated_blocked'));
