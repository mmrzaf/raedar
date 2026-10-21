-- Pair each outgoing Telegram sendMessage with the chat and message_id
-- Telegram returned. The inbox goroutine uses the pair to look up the
-- incident of a message the operator replied to with `mute 2h` etc.
--
-- Telegram message ids are only unique within one chat, so the chat id is
-- part of the key. One message may announce several incidents, so the pair is
-- not unique either: every (incident, chat) delivery of a grouped message
-- carries the same ids. The index serves the reply-to lookup.
alter table deliveries add column telegram_chat_id bigint;
alter table deliveries add column telegram_message_id bigint;
create index deliveries_telegram_message_idx
    on deliveries (telegram_chat_id, telegram_message_id)
    where telegram_message_id is not null;
