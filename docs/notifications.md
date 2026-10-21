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
| `RAEDAR_NOTIFY_WARNING` | `telegram,webhook` (those that are configured) |
| `RAEDAR_NOTIFY_CRITICAL` | every configured channel |
| `RAEDAR_DIGEST_CHANNELS` | `telegram,webhook` (those that are configured) |

A resolution goes to the same channels the incident's opening message went to. With the defaults, SMS is used only for critical incidents and never for the digest.

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

Any 2xx response counts as delivered. Raedar retries 4xx/5xx with the same retry schedule as the other channels (10 s, 1 m, 5 m). The request URL is never logged, because the API key may be part of the URL.

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

Read the API key from `RAEDAR_SMS_HTTP_BODY_FILE` (or any sibling secret file) into the `{{api_key}}` placeholder at deployment time; Raedar does not yet substitute it at runtime. For now, leave the URL containing `{{api_key}}` and inject the value via your secret manager (e.g. `envsubst` in compose, or a small startup script that rewrites the env file).

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

## Delivery

- **Retries.** A failed delivery is retried after 10 seconds, 1 minute and 5 minutes. After that it is marked failed. Every attempt is recorded in `deliveries` and visible on the incident page.
- **Rate limits.** Per channel, at most `RAEDAR_RATE_LIMIT` messages per hour (30 by default) and at most `RAEDAR_SMS_RATE_LIMIT` per hour for SMS (10 by default). Messages beyond the limit are held and sent as a single "N more incidents, see Raedar" message when the limit allows.
- **Timeouts.** Each request has a 15-second timeout.
- **Proxy.** `RAEDAR_OUTBOUND_PROXY` (an `http://` or `socks5://` URL) sends the outbound requests selected by `RAEDAR_OUTBOUND_PROXY_FOR` through a proxy. The setting names any of `telegram`, `sms`, `webhook` and `heartbeat`; `all` (the default) sends every request through it. A server that can reach its SMS provider directly but not Telegram sets `RAEDAR_OUTBOUND_PROXY_FOR=telegram,webhook,heartbeat`. The proxy does not affect the Docker API or PostgreSQL. This is for servers whose direct outbound access is blocked or filtered. The standard `HTTPS_PROXY` variable is ignored on purpose, so it cannot accidentally route Docker API calls through a proxy.

## Testing

`raedar notify-test` sends a test message to every configured channel, or to one with `--channel telegram`, and prints the result of each delivery. It uses the same configuration as the running service, so it is normally run as `docker exec raedar raedar notify-test`.
