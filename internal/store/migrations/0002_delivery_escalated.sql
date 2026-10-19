-- An escalation is announced with its own kind of delivery.
alter table deliveries drop constraint deliveries_kind_check;
alter table deliveries add constraint deliveries_kind_check
    check (kind in ('opened', 'escalated', 'resolved', 'digest', 'test'));
