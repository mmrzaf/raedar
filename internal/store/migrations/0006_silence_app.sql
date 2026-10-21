-- A silence may target an app rather than one (rule, subject). This is
-- what auto-deploy silences and app-level maintenance windows use: an
-- incident whose app_id matches is silenced regardless of its rule or
-- subject, so a restart after a deploy does not page.
--
-- Precedence stays: rule+subject > rule > subject > app > wildcard.
alter table silences add column app_id bigint references apps (id) on delete cascade;
create index silences_app_idx on silences (app_id);
