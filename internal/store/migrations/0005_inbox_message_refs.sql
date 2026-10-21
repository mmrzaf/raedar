-- Pair each outgoing Telegram sendMessage with the message_id Telegram
-- returned. The inbox goroutine uses this to look up the (rule, subject)
-- of a message the operator replied to with `mute 2h` etc.
--
-- One Telegram message_id is reused across the (incident × chat) pairs of
-- a single delivery: a group message communicates every incident at the same
-- chat through the same message_id, so a unique constraint on
-- telegram_message_id is wrong. We keep a non-unique index for lookups by
-- reply-to.
alter table deliveries add column telegram_message_id bigint;
create index deliveries_telegram_message_id_idx
    on deliveries (telegram_message_id)
    where telegram_message_id is not null;
