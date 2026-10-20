-- A silence prevents incident pages for one subject for a window of time.
-- The pager consults silences before paging, so an actively silenced
-- (rule, subject) sees deliveries rows with status='silenced' but no Telegram
-- message.
--
-- rule and subject may be NULL to match any; most-specific-match wins.
--
-- The kind column distinguishes the kinds of silences Raedar owns:
--   maintenance_window  explicit window created from the UI
--   one_shot_mute       a Telegram "mute 2h" reply or a CLI/api mute
--   quiet_hours         a recurring window derived from QUIET_FROM/QUIET_TO
--   pager_cooldown      written automatically after a successful page so the
--                       same (rule, subject) does not page again too soon
--   auto_deploy         written automatically around a deploy of an app
create table silences (
    id bigint generated always as identity primary key,
    kind text not null check (kind in (
        'maintenance_window', 'one_shot_mute', 'quiet_hours',
        'pager_cooldown', 'auto_deploy'
    )),
    rule text,
    subject text,
    reason text not null,
    starts_at timestamptz not null,
    ends_at timestamptz not null,
    created_at timestamptz not null default now(),
    created_by text,
    dedup_key text,
    comment text
);

create index silences_active_idx on silences (starts_at, ends_at);
create index silences_rule_subject_idx on silences (rule, subject);

-- A (kind, dedup_key) pair is unique. NULLs are distinct; the UI bumps the
-- dedup_key when it wants to add a second silence of the same kind.
create unique index silences_dedup_idx on silences (kind, dedup_key)
    where dedup_key is not null;
