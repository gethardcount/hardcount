# Authentication and keys

Every API request names its account with an API key. The key also picks the
mode, test or live, and what the request may do. This page is the reference:
the header, the key types, the answers when a key is refused, and the key
routes. To make or rotate a key in the dashboard, see
[API keys](../api-keys.md).

## Send the key

Put the key in the `Authorization` header, after `Bearer` and one space:

```bash
curl -s "$HARDCOUNT/v1/whoami" -H "Authorization: Bearer $HARDCOUNT_KEY"
```

`GET /v1/whoami` needs no permission, so it is a good first call. It answers
with your `tenant_id` (your account's ID), `livemode`, `key_id` and
`key_type`.

Outside your own computer, send keys only over HTTPS.

## Key format

A key is its prefix followed by 64 hex characters. The prefix says the key's
type and mode:

| Prefix | Type | Mode |
|---|---|---|
| `hc_secret_test_` | Secret | Test |
| `hc_secret_live_` | Secret | Live |
| `hc_ingest_test_` | Usage only | Test |
| `hc_ingest_live_` | Usage only | Live |

**The mode is fixed by the key.** Each account has two separate sets of
data, test and live. A test key reads and writes only test data, and a live
key only live data. A key made in test mode is a test key.

## Key types

| Type | `key_type` | Can call |
|---|---|---|
| Secret | `secret` | Every route of one account, in the key's mode |
| Usage only | `ingest` | `POST /v1/usage-events`, `POST /v1/usage-events/batch`, `POST /v1/integrations/litellm/spend` and `GET /v1/whoami` |

A usage-only key cannot backfill past usage, release held usage, read usage
or read the credit balance. Do those with a secret key.

A dashboard session (the cookie the dashboard gets when someone signs in)
can do what a secret key can, in the mode the dashboard is in. Only a
session can manage team members (`/v1/members`).

### Which key goes where

| Where the key lives | Give it |
|---|---|
| Your backend server | A secret key |
| LiteLLM or another gateway that only reports usage | A usage-only key |
| A browser, a mobile app or any code your customers can read | No key. Call Hardcount from your server instead. |

Give each server or gateway its own key. You can then rotate or revoke one
without touching the others, and each has its own
[rate limit](rate-limits.md).

### If a key leaks

- **A secret key** can read and change everything in its account and mode:
  customers, invoices, refunds, credit, webhooks, settings (the Stripe keys
  too) and API keys. Rotate it with **Now** at once (see
  [Rotate a key](../api-keys.md#rotate-a-key)).
- **A usage-only key** can send usage for any customer in its mode. Usage
  can raise a bill, and a negative quantity can lower one. It cannot read
  anything. Rotate it too.

A test key leaks only test data, and test mode cannot charge a real card.

## When a key is refused

| Status | `code` | `message` | Why |
|---|---|---|---|
| `401` | `unauthorized` | `missing credentials — sign in at /login or send Authorization: Bearer hc_secret_...` | No key was sent, or the header is not `Bearer <key>` |
| `401` | `unauthorized` | `invalid or expired API key` | The key is wrong, revoked or past its `expires_at`. The message is the same in each case, so it does not tell anyone whether a key exists. |
| `403` | `forbidden` | `insufficient permissions: this key type does not have customer:read access` | The key's type cannot call this route; the message names the permission |
| `403` | `forbidden` | `this endpoint requires a dashboard session — sign in to the dashboard to manage team membership` | `/v1/members` takes only a dashboard session |
| `403` | `forbidden` | `cross-site request blocked` | A browser sent a change from another site with the dashboard's cookie. A request with a Bearer key and no cookie is never refused this way. |
| `503` | `authentication_unavailable` | `authentication is temporarily unavailable — retry shortly` | Hardcount could not reach its database to check the key. It says nothing about your key. Retry the same request. |

All of them use `type` `authentication_error`, except the `503`, which is
`api_error`. Every error code is in [Errors](errors.md).

## Manage keys with the API

The key routes take a secret key or a dashboard session. Each works on the
keys of the caller's mode.

| Method | Path | What it does |
|---|---|---|
| `POST` | `/v1/api-keys` | Create a key |
| `GET` | `/v1/api-keys` | List keys, newest first, 50 to a page ([pagination](pagination.md)) |
| `POST` | `/v1/api-keys/{id}/rotate` | Make a new key and retire the old one |
| `DELETE` | `/v1/api-keys/{id}` | Revoke a key at once |

**Create.** Send `name` (required), `key_type` (`secret`, the default, or
`ingest`) and an optional `expires_at`, a time in the future in RFC 3339
form:

```bash
curl -s -X POST "$HARDCOUNT/v1/api-keys" \
  -H "Authorization: Bearer $HARDCOUNT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name": "LiteLLM proxy", "key_type": "ingest", "expires_at": "2027-01-01T00:00:00Z"}'
```

The `201` answer has `key` (the key's record) and `raw_key` (the full key).
`raw_key` is shown only this once: Hardcount stores only a salted SHA-256
hash. An `expires_at` that is not in the future is refused with `422`.

**List.** Each key's record has `id`, `name`, `key_type`, `livemode`,
`key_prefix` (the prefix and the first 12 hex characters), `created_at`,
and, when set, `expires_at`, `revoked_at` and `last_used_at`. Revoked keys
stay in the list. `last_used_at` is updated at most once a minute.

**Rotate.** The new key keeps the old key's name, type, mode and
`expires_at`. Send `expires_in_seconds` to keep the old key working for a
while: `0`, the default, stops it at once, and the most is `604800` (7
days). The answer has `old_key`, `new_key` and `raw_key`. A revoked key
cannot be rotated: that is a `409`.

**Revoke.** The key stops working on the next request. Revoking it a second
time answers `404`.
