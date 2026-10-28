# Notifications

Raedar delivers incident messages and the daily digest through three kinds of channel: Telegram, SMS and webhooks. A channel is enabled by configuring it; there is nothing else to switch on.

| Channel | Enabled by | Typical use |
|---|---|---|
| `telegram` | `RAEDAR_TELEGRAM_TOKEN` and `RAEDAR_TELEGRAM_CHAT_IDS` | everything |
| `sms` | `RAEDAR_SMS_HTTP_URL` and `RAEDAR_SMS_TO` | critical incidents only |
| `webhook` | `RAEDAR_WEBHOOK_URL` | forwarding to other systems (chat tools, ntfy relays, scripts) |

## Routing

| Variable | Default |
|---|---|
| `RAEDAR_NOTIFY_WARNING` | `none` |
| `RAEDAR_NOTIFY_CRITICAL` | every configured channel |
| `RAEDAR_DIGEST_CHANNELS` | `telegram,webhook` (those that are configured) |

Warnings are recorded and included in the daily digest but do not page by default. Set `RAEDAR_NOTIFY_WARNING` to channels when you want warning pages too. `none` is also accepted explicitly to disable a route's pages.

A resolution goes to the same channels the incident's opening message went to. With the defaults, SMS is used only for critical incidents and never for the digest.

## Flapping

A subject that opens repeatedly can flood a channel even when its opening pages are in the pager cooldown. `RAEDAR_FLAP_WINDOW` (1 hour) and `RAEDAR_FLAP_MAX_PAGES` (1) limit how many **announced** episodes a (rule, subject) may produce in that window. A page that was never sent, because a deploy silence or quiet hours held it, does not consume the budget. Critical incidents bypass the budget, so a crash loop is never lost because an earlier warning used the last slot. Later warning opens are recorded as `dropped` deliveries and cancelled. Once the subject stays quiet for the window, the next page goes out normally. Set the window to `0` to disable flap suppression.

## Telegram

| Variable | Meaning |
|---|---|
| `RAEDAR_TELEGRAM_TOKEN` | Bot token from @BotFather. |
| `RAEDAR_TELEGRAM_CHAT_IDS` | Comma-separated chat IDs: a user, a group or a channel the bot can post to. |
| `RAEDAR_TELEGRAM_API_URL` | Bot API base URL. Default `https://api.telegram.org`. Set it to use a self-hosted Bot API server or a relay. |

Messages are sent with `sendMessage`, HTML formatting and link previews disabled. Each block starts with 🔴 (critical), 🟠 (warning) or ✅ (resolved).

## SMS

SMS messages are plain text, at most 160 characters, one line:

```
RAEDAR host-1 CRITICAL shop: no backend for 63% of requests since 14:32
RAEDAR host-1 RESOLVED shop: no backend (18 min)
```

Grouped incidents become `RAEDAR host-1 CRITICAL 3 problems: shop down, api down, +1`.

Raedar ships one SMS shape: a single generic HTTP request per recipient. Providers that take an API key in the URL path, a header, or a body field are all the same code.

| Variable | Meaning |
|---|---|
| `RAEDAR_SMS_TO` | Comma-separated recipient numbers. |
| `RAEDAR_SMS_FROM` | Sender line. Optional; some providers require it, others ignore it. Replaced into `{{from}}`. |
| `RAEDAR_SMS_HTTP_URL` | Request URL. May contain `{{to}}`, `{{message}}`, `{{from}}` and `{{api_key}}`; values inserted into the URL are always URL-encoded. |
| `RAEDAR_SMS_HTTP_METHOD` | `POST` (default) or `GET`. |
| `RAEDAR_SMS_HTTP_HEADERS` | Extra headers, one `Name: value` per line. Use this for `Authorization` when the provider expects a header instead of a path-style key. |
| `RAEDAR_SMS_HTTP_BODY` | Request body. Same `{{...}}` placeholders as the URL. |
| `RAEDAR_SMS_HTTP_ENCODING` | How placeholder values are escaped in the body: `json` (JSON string escaping, quotes are part of the template), `form` (URL encoding) or `raw`. Default `json`. It also sets `Content-Type` unless a header overrides it. |
| `RAEDAR_SMS_HTTP_API_KEY` | Substituted for `{{api_key}}` in the URL and body. Prefer `RAEDAR_SMS_HTTP_API_KEY_FILE`. |

Any 2xx response counts as delivered. Raedar retries 4xx/5xx with the same capped backoff as the other channels while the incident is open. The request URL is never logged, because the API key may be part of the URL.

Example for a provider expecting JSON:

```
RAEDAR_SMS_TO=+989120000000
RAEDAR_SMS_FROM=10004346
RAEDAR_SMS_HTTP_URL=https://sms.example.com/v1/send
RAEDAR_SMS_HTTP_HEADERS=Authorization: Bearer xxxxxxxx
RAEDAR_SMS_HTTP_BODY={"to": "{{to}}", "from": "{{from}}", "text": "{{message}}"}
RAEDAR_SMS_HTTP_ENCODING=json
```

Example for a provider that takes an API key in the URL path (for example, the Iranian panel at `api.example.com/v1/<key>/sms/send.json`):

```
RAEDAR_SMS_TO=+989120000000
RAEDAR_SMS_FROM=10004346
RAEDAR_SMS_HTTP_URL=https://api.example.com/v1/{{api_key}}/sms/send.json?receptor={{to}}&message={{message}}&sender={{from}}
RAEDAR_SMS_HTTP_BODY=
RAEDAR_SMS_HTTP_ENCODING=raw
```

Set the key with `RAEDAR_SMS_HTTP_API_KEY` (or `RAEDAR_SMS_HTTP_API_KEY_FILE`). Raedar substitutes it for `{{api_key}}` in the URL and the body. It is never logged.

## Webhook

| Variable | Meaning |
|---|---|
| `RAEDAR_WEBHOOK_URL` | URL that receives a `POST` with a JSON body. |
| `RAEDAR_WEBHOOK_SECRET` | Optional. When set, each request carries `X-Raedar-Timestamp` (Unix seconds) and `X-Raedar-Signature: sha256=<hex>`, the HMAC-SHA256 of `<timestamp>.<body>` with the secret. |

Body:

```json
{
  "version": 1,
  "host": "host-1",
  "kind": "incident.opened",
  "sent_at": "2026-10-19T14:34:15Z",
  "url": "https://raedar.example.com/incidents/812",
  "incidents": [
    {
      "id": 812,
      "rule": "route.errors",
      "subject": "shop@docker",
      "app": "shop",
      "severity": "warning",
      "opened_at": "2026-10-19T14:32:00Z",
      "resolved_at": null,
      "reopened": false,
      "title": "5xx errors",
      "summary": "6.4% of requests fail (usual at this hour: 0.2%)",
      "context": [
        "4 min after deploy shop 2.3.0 → 2.4.0",
        "most errors: POST /api/checkout"
      ],
      "details": { "rate": 0.064, "baseline": 0.002, "requests": 1840, "errors": 118 }
    }
  ]
}
```

`kind` is `incident.opened`, `incident.resolved`, `incident.escalated`, `digest` or `test`. A digest carries `text` (the formatted digest) instead of `incidents`. `details` depends on the rule; the other fields are stable within `version` 1.

## Silences

A silence stops pages for a scope **until it expires**. The page is not
discarded: it stays pending in the database, and when the silence ends the
policy is evaluated again. If the incident is still open, the page goes out
then; if it resolved in the meantime, the opening page is cancelled. Removing
a silence early wakes the pages waiting on it immediately. Because the queue is
in PostgreSQL, a restart does not lose a held page.

| Kind | Created by | Scope |
|---|---|---|
| `maintenance_window` | the Silences page | a rule, a subject, or both; at least one is required, so a silence never covers everything |
| `one_shot_mute` | Telegram `mute` | one (rule, subject), or the one of the replied-to page |
| `auto_deploy` | Raedar itself | one app, for `RAEDAR_DEPLOY_SILENCE` after a deploy |

Precedence, most specific first: (rule, subject), rule, subject, app,
everything. When two silences match, the one ending later wins. A silence
created by an operator lasts at most 30 days.

**Auto-deploy silences never hide a critical incident or a resolution.** They
only cover the warning noise a deploy can produce, such as one container crash
or a route blip while the new image starts. A crash loop is critical and pages
immediately, even inside the deploy window. Manual maintenance windows and
Telegram mutes apply to every severity because the operator asked for them.

A resolution is best-effort: it is sent to the channels that announced the
opening, but if a maintenance window or mute covers the incident, or quiet
hours are open, when the incident resolves, the stale resolution is dropped
rather than delivered later. A critical resolution still goes out during quiet
hours unless `RAEDAR_QUIET_PAGE_CRITICAL=false`. The incident timeline and the
digest still show it.

Expired auto-deploy silences are removed after a day, and expired one-shot
mutes after the 30-day cap, so the Silences page stays readable.

### Durable pages

Every incident episode has one page per channel and recipient. The page moves
through `pending`, `announced`, `resolving` and `resolved`, or ends as
`cancelled` when it was acked, the app disabled alerts, the episode resolved
before it was announced, or flap policy deliberately dropped it. Silence,
quiet hours, pager cooldown, rate limiting and transport failures only move the
`due_at`; they never delete the obligation. The incident page shows the current
state and the reason a page is waiting. The Deliveries page keeps the audit row
for every suppression, attempt and send.

### Quiet hours

`RAEDAR_QUIET_FROM` and `RAEDAR_QUIET_TO` define a recurring window in
`RAEDAR_QUIET_TZ` (default `RAEDAR_TZ`). Warnings inside the window are held
and delivered as one message when the window closes. Critical incidents page
immediately unless `RAEDAR_QUIET_PAGE_CRITICAL=false`.

A held page is checked again when it is released: if the incident was acked,
silenced or resolved in the meantime, or the window is still open, it is not
sent. An incident that resolved before its page went out gets no resolution
message either.

### Pager cooldown

`RAEDAR_PAGER_COOLDOWN` keeps the same (rule, subject) from opening a page
more than once in the window; `RAEDAR_PAGER_COOLDOWN_BY_RULE` overrides it per
rule, and `0` turns it off. A page inside the cooldown is held and delivered
once when it expires, so a flapping route produces one catch-up message
instead of a burst. Escalations and resolutions are never delayed by the
cooldown.

Held pages live in PostgreSQL. If Raedar restarts while one waits, the
dispatcher resumes it; the `held` and `cooldown` rows on the Deliveries page
remain as the audit record.

### Stand-down

With `RAEDAR_STAND_DOWN_AFTER` set, Raedar sends one "all quiet for N h"
message after that much time with no incident page and nothing open. The
digest does not count as a page. It proves the pipeline works without waiting
for the digest.

## Telegram commands

The bot accepts commands from any chat in `RAEDAR_TELEGRAM_CHAT_IDS` while
Raedar runs. It long-polls `getUpdates`; no webhook or open port is needed.

| Command | Effect |
|---|---|
| `help` | show the command list |
| `status` | open incidents and active silences |
| `ack <id>` | acknowledge an open incident; all its later messages stop. A recurrence after it resolves pages again |
| `mute <rule> <subject> <duration>` | silence one (rule, subject) |
| `mute <rule> <subject> until <RFC3339>` | silence until a timestamp |
| reply `mute 2h` | silence the (rule, subject) of the page you replied to; a message that announced several incidents needs the full form |
| `unmute <silence-id>` | remove a silence |

Durations use the configuration syntax: `30s`, `5m`, `2h`, `14d`, and a mute lasts at most 30 days. Only the chats in `RAEDAR_TELEGRAM_CHAT_IDS` are obeyed, and every member of such a chat can run these commands.

## Delivery

- **Retries.** A failed delivery is retried after 10 seconds, 1 minute, 5 minutes, 15 minutes, 30 minutes and then hourly for as long as the incident is open. A temporary Telegram or SMS outage cannot lose a page. Only the recipient that failed is retried: another chat or number that already got the message is not contacted again. A Telegram chat that refuses the bot with HTTP 400 or 403, such as a chat the bot was removed from, is not retried: its page is cancelled and the reason is shown on the incident page. Other errors, including a rejected token, stay retryable so the page survives until the operator fixes the configuration. Every attempt is recorded in `deliveries` and visible on the incident page. Delivery is at-least-once: a crash after the provider accepted a message but before Raedar recorded it can repeat that message.
- **Rate limits.** Per channel, at most `RAEDAR_RATE_LIMIT` messages per hour (30 by default) and at most `RAEDAR_SMS_RATE_LIMIT` per hour for SMS (10 by default). A page beyond the limit stays pending and goes out when the window allows it.
- **Timeouts.** Each request has a 15-second timeout.
- **Proxy.** `RAEDAR_OUTBOUND_PROXY` (an `http://` or `socks5://` URL) sends the outbound requests selected by `RAEDAR_OUTBOUND_PROXY_FOR` through a proxy. The setting names any of `telegram`, `sms`, `webhook` and `heartbeat`; `all` (the default) sends every request through it. A server that can reach its SMS provider directly but not Telegram sets `RAEDAR_OUTBOUND_PROXY_FOR=telegram,webhook,heartbeat`. The proxy does not affect the Docker API or PostgreSQL. This is for servers whose direct outbound access is blocked or filtered. The standard `HTTPS_PROXY` variable is ignored on purpose, so it cannot accidentally route Docker API calls through a proxy.

## Testing

`raedar notify-test` sends a test message to every configured channel, or to one with `--channel telegram`, and prints the result of each delivery. It uses the same configuration as the running service, so it is normally run as `docker exec raedar raedar notify-test`.
