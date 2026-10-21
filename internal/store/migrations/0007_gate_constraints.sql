-- The paging gate records 'acked' for an incident the operator acknowledged,
-- which the status check from 0004 did not allow, so those audit rows were
-- rejected. It never records 'stand_down' or 'escalated_blocked', and the
-- silence kinds 'quiet_hours' and 'pager_cooldown' from 0003 are not
-- silences: the gate computes quiet hours and the cooldown itself. Narrow
-- both checks to what is used.
--
-- The status check was created unnamed in 0001, so it is found by its
-- definition; the kind check carries the default name.
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
    check (status in ('pending', 'sent', 'failed', 'silenced', 'acked', 'held', 'cooldown'));

alter table silences drop constraint silences_kind_check;
alter table silences add constraint silences_kind_check
    check (kind in ('maintenance_window', 'one_shot_mute', 'auto_deploy'));
