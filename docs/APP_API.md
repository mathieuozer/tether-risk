# App and API contract

The bot binary serves two HTTP surfaces next to Telegram polling
(docs/DECISIONS.md D27). Both enforce the same plans, limits and history as
the chat bot, through one gate.

- **Mini App** at `/app/`: a web interface opened inside Telegram.
- **Public API** at `/api/v1/`: for business-plan customers, with API keys.

Both listen on `APP_ADDR` (default `127.0.0.1:8098`), next to `GET /healthz`, and are exposed over
HTTPS by a tunnel; see the go-live checklist. The internal screening API on
`127.0.0.1:8099` is never exposed.

Errors from either surface have one shape:

```json
{"error": "limit_reached", "message": "You have used all 20 screens for today."}
```

| `error` | HTTP | Meaning |
|---|---|---|
| `unauthorized` | 401 | Missing or invalid Telegram initData or API key |
| `bad_address` | 400 | Not a recognised address for any enabled chain |
| `chain_unavailable` | 400 | Recognised chain, not enabled yet |
| `no_plan` | 402 | No active plan or trial |
| `feature_locked` | 403 | The plan does not include this (details, pdf, batch, api, watches) |
| `limit_reached` | 429 | Daily screens, watches or batch size used up |
| `busy` | 409 | A screen or batch for this user is already running |
| `rate_limited` | 429 | Public API only: more than 5 requests a second on one key |
| `bad_request` | 400 | The body is not the JSON the endpoint expects |
| `not_found` | 404 | No such watch or key |
| `screen_failed` | 502 | The screening service failed; nothing was counted |

## Mini App: `/app/api/*`

Every request carries `Authorization: tma <initData>`, the raw
`Telegram.WebApp.initData` string. The server verifies its HMAC with the bot
token and rejects data older than 24 hours.

### `GET /app/api/me`

```json
{
  "user": {"id": 42, "first_name": "Ada", "username": "ada", "lang": "tr", "admin": false},
  "access": {
    "plan": {"id": "pro", "name": "Pro"},
    "until": "2026-10-22T12:00:00Z",
    "source": "stars",
    "renews": true
  },
  "limits": {"daily_screens": 200, "details": true, "pdf": true, "watches": 25, "batch": 100, "api": false},
  "usage": {"screens_today": 3, "watches": 2, "resets_at": "2026-09-23T00:00:00Z"},
  "plans": [
    {"id": "basic", "name": "Basic", "daily_screens": 20, "details": false, "pdf": false,
     "watches": 3, "batch": 0, "api": false, "price_stars": 800, "price_usdt": "10.00"}
  ],
  "usdt_enabled": true,
  "chains": [{"id": "tron", "name": "Tron", "enabled": true}, {"id": "ethereum", "name": "Ethereum", "enabled": false}],
  "support": "@support"
}
```

`access` is `null` with no active plan. `limits` is the active plan's, or all
zero with none. Admins get `"admin": true` and unlimited limits (`-1`).

### `POST /app/api/screen`

Request `{"address": "T...", "chain": "tron"}`; `chain` is optional and
detected from the address when absent. Uses one daily screen.

```json
{"result": { ...the screening API response, unchanged... }, "usage": {"screens_today": 4, "daily_screens": 200}}
```

`result` has the shape of `POST /v1/screen` on the internal API: `address`,
`chain`, `score`, `band`, `coverage`, `low_confidence`, `sanctions_override`,
`own_label`, `activity`, `depth`, and `inbound`/`outbound`, each with
`categories[] {category, pct}`, `unattributed_pct`, `connections[] {address,
entity, category, pct, min_hops, profile?}`, `unattributed_reasons[] {reason,
pct}`, `top_paths[] {explanation}` and `traversal {fanout_capped,
hop_limit_reached}`.

It also carries `flags[] {code, in_usd?, out_usd?, volume_usd?, days?,
age_days?}` (behaviour notes, never scored) and `verdict {level,
confidence, reasons[] {code, category?, pct?, flag?}}`, where `level` is
`clear`, `caution` or `high_risk` (docs/DECISIONS.md D29). The screen
response adds `follow_up: true` when the bot will send the final result to
the chat after further tracing.

### `POST /app/api/presend`

Checks a payment before it is sent (docs/DECISIONS.md D34). Request
`{"to": "T...", "from": "T...", "chain": "tron"}`; `from`, the paying wallet,
and `chain` are optional. Uses one daily screen.

```json
{"presend": {"to": "T...", "from": "T...", "decision": "do_not_send",
  "lookalike_of": "T...", "lookalike_usd": 150000, "paid_before_usd": 0,
  "first_payment": true, "sender_known": true},
 "result": { ...the recipient's screen, as for screen... },
 "usage": {"screens_today": 5, "daily_screens": 200}}
```

`decision` is `do_not_send` when the recipient is risky or imitates a real
counterparty of `from` (`lookalike_of`, an address-poisoning copy), and
`send` otherwise. `sender_known` is false when `from` was not given or its
history could not be read; the look-alike and first-payment checks did not
run then. An invalid `from` is refused with `bad_address`.

### `POST /app/api/report`

Request as for screen. Needs the plan's `pdf`. The PDF is sent to the user's
Telegram chat by the bot, since a Mini App cannot reliably save files.
Response `{"sent": true}`.

### `GET /app/api/history?limit=50`

```json
{"items": [{"chain": "tron", "address": "T...", "band": "low", "score": 15.0, "coverage": 0.999,
            "channel": "app", "created_at": "2026-09-22T10:00:00Z"}]}
```

Newest first. `band`, `score` and `coverage` are null for PDF-only screens.

### Watches

- `GET /app/api/watches` returns `{"items": [watch], "limit": 25}`.
- `POST /app/api/watches` takes `{"address", "chain"?, "label"?}` and returns the watch.
- `DELETE /app/api/watches/{id}` returns `{"removed": true}`.

```json
{"id": 7, "chain": "tron", "address": "T...", "label": "Supplier A",
 "created_at": "...", "checked_at": "...",
 "last": {"band": "low", "score": 15.0, "coverage": 0.99, "risk_categories": ["gambling"], "listed": false}}
```

`last` is null until the first check, which happens within minutes of adding.

### Payments

- `POST /app/api/invoice/stars {"plan": "pro"}` returns `{"link": "https://t.me/$..."}`, which the app
  opens with `Telegram.WebApp.openInvoice(link, callback)`. Access is granted
  by the bot when Telegram confirms payment; the app refreshes `/me` after
  the `paid` callback status.
- `POST /app/api/invoice/usdt {"plan": "pro"}` returns
  `{"amount": "49.37", "address": "T...", "network": "TRON (TRC-20)", "expires_at": "..."}`.

### Batch

`POST /app/api/batch {"addresses": ["T...", "0x..."]}` returns
`{"accepted": 42}`. Runs in the background; results arrive in the Telegram
chat as a CSV file. Needs the plan's `batch`. Each address uses one daily
screen.

### API keys (business plan)

- `GET /app/api/keys` returns `{"items": [{"id", "prefix", "name", "created_at", "last_used_at"}]}`.
- `POST /app/api/keys {"name"}` returns `{"id", "key", "prefix"}`; `key` is shown only this once.
- `DELETE /app/api/keys/{id}` returns `{"revoked": true}`.

### Language

`POST /app/api/lang {"lang": "tr"}`, one of `en`, `tr` and `ru`. It sets the language for the bot too.

## Public API: `/api/v1/*`

`Authorization: Bearer trk_...`. Needs a plan with `api`. Same limits as the
customer's plan; requests beyond 5 a second per key get `429`.

| Method | Path | Body | Returns |
|---|---|---|---|
| POST | `/api/v1/screen` | `{"address", "chain"?}` | the screening result |
| POST | `/api/v1/report` | `{"address", "chain"?}` | `application/pdf` |
| GET | `/api/v1/usage` | | `{"plan", "until", "screens_today", "daily_screens"}` |
| GET | `/api/v1/watches` | | `{"items": [watch], "limit"}` |
| POST | `/api/v1/watches` | `{"address", "chain"?, "label"?}` | the watch |
| DELETE | `/api/v1/watches/{id}` | | `{"removed": true}` |

Alerts for watches are delivered to the key owner's Telegram chat.
