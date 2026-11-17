# Authentication and keys

Every API request names its account with an API key. The key also picks the
mode, test or live, and what the request may do. This page is the reference:
the header, the key's format, the key types, the answers when a key is
refused, and the key routes. To make, rotate or revoke a key, see
[API keys](../api-keys.md).

## Send the key

Put the key in the `Authorization` header, after `Bearer` and one space:

```bash
curl -s "$HARDCOUNT/v1/whoami" -H "Authorization: Bearer $HARDCOUNT_KEY"
```

`GET /v1/whoami` needs no permission, so it is a good first call. It answers
with your `tenant_id` (your account's ID), `livemode`, `key_id` and
`key_type`, among other fields.

Outside your own computer, send keys only over HTTPS.

## Key format

A key is its **type prefix** followed by 64 lowercase hex characters. Any
other string is refused as an invalid key. The type prefix says the key's
type and mode:

| Type prefix | Type | Mode |
|---|---|---|
| `hc_secret_test_` | Secret | Test |
| `hc_secret_live_` | Secret | Live |
| `hc_ingest_test_` | Usage only | Test |
| `hc_ingest_live_` | Usage only | Live |

A key's **lookup prefix** is its type prefix and the first 12 hex
characters. The key list shows it, and the API returns it as `key_prefix`,
so you can tell keys apart without the whole key.

Hardcount stores only a SHA-256 hash of each key, never the key itself, so
the full key is shown only once, when it is made. Each key's hash has its own
random salt, except keys that the setup command (`hardcount-bootstrap` or
`POST /v1/bootstrap`) made in v0.9.3 or earlier, which have none. They still
work, and rotating one replaces it with a salted key.

**The mode is fixed by the key.** Each account has two separate sets of
data, test and live. A test key reads and writes only test data, and a live
key only live data. A key made in test mode is a test key.

The Stripe connection is per mode too: a key sees and changes only its own
mode's (`/v1/settings/stripe`), and the dashboard's Test/Live toggle
switches it.

## Key types

| Type | `key_type` | Can call |
|---|---|---|
| Secret | `secret` | Every route of one account, in the key's mode, except the few only a dashboard session can call (below) |
| Usage only | `ingest` | `POST /v1/usage-events`, `POST /v1/usage-events/batch`, `POST /v1/integrations/litellm/spend` and `GET /v1/whoami` |

A usage-only key cannot backfill past usage, release held usage, read usage
or read the credit balance. Do those with a secret key.

A dashboard session (the cookie the dashboard gets when someone signs in)
can do what a secret key can, in the mode the dashboard is in. That holds
for every member: roles are recorded but not enforced, so any member can
create a live secret key. Only a session can:

- manage team members (`/v1/members`);
- change the account settings (`PUT /v1/settings`), which test and live
  mode share. A key can read them;
- send the test email (`POST /v1/settings/email/test`), which goes to the
  signed-in member's own address.

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
  customers, invoices, refunds, credit, webhooks, its mode's Stripe keys
  and API keys. It can read the account settings. Rotate it right away,
  with **Now** ([Rotate a key](../api-keys.md#rotate-a-key)).
- **A usage-only key** can send usage for any customer in its mode. Usage
  can raise a bill, and a negative quantity can lower one. It can read only
  `GET /v1/whoami` and an event it sends again: a repeated `idempotency_key`
  answers with the first event. Rotate it too.

A leaked secret test key exposes test data and the account settings, which
both modes share. Test mode cannot charge a real card.

The account's Audit Log shows who created, rotated and revoked each key, and
when.

## When a key is refused

| Status | `code` | `message` | Why |
|---|---|---|---|
| `401` | `unauthorized` | `missing credentials — sign in at /login or send Authorization: Bearer hc_secret_...` | No key was sent in `Authorization: Bearer <key>`, and the request has no dashboard session, or one that has ended |
| `401` | `unauthorized` | `invalid or expired API key` | The key is wrong, revoked or past its `expires_at`. The message is the same in each case, so it does not tell anyone whether a key exists. |
| `403` | `forbidden` | `insufficient permissions: this key type does not have customer:read access` | The key's type cannot call this route; the message names the permission |
| `403` | `forbidden` | `this endpoint requires a dashboard session — sign in to the dashboard to manage team membership` | `/v1/members` takes only a dashboard session |
| `403` | `forbidden` | `this endpoint requires a dashboard session — sign in to the dashboard to change account settings` | `PUT /v1/settings` takes only a dashboard session |
| `403` | `forbidden` | `a test-mode request can manage only the test-mode Stripe connection: for live mode, use a live-mode key or switch the dashboard to live mode` | A test key, or the dashboard in test mode, named live mode on `/v1/settings/stripe`. A request in live mode that names test mode gets the same message with the modes swapped. |
| `403` | `forbidden` | `cross-site request blocked` | A browser sent a change from another site with the dashboard's cookie. A request with a Bearer key and no cookie is never refused this way. |
| `503` | `authentication_unavailable` | `authentication is temporarily unavailable — retry shortly` | Hardcount could not reach its database to check the key. It says nothing about your key. Retry the same request. |

All of them use `type` `authentication_error`, except the `503`, which is
`api_error`. Every error code is in [Errors](errors.md).

An open event stream (`GET /v1/webhook_events/stream`) checks its caller
again every 15 seconds, and closes with no final frame once that caller no
longer authenticates: its key is revoked or expires, or its dashboard
session ends. A stream opened with a session cookie and a key on the same
request is the session's, so it closes when the session ends, even though
the key still works. A reconnect with credentials that no longer work
answers `401`.

## Manage keys with the API

The key routes take a secret key or a dashboard session. Each works on the
keys of the caller's mode. [API keys](../api-keys.md) says what each one
takes and does.

| Method | Path | What it does |
|---|---|---|
| `POST` | `/v1/api-keys` | [Create a key](../api-keys.md#get-a-key) |
| `GET` | `/v1/api-keys` | List keys, newest first, 50 to a page ([pagination](pagination.md)) |
| `POST` | `/v1/api-keys/{id}/rotate` | [Rotate a key](../api-keys.md#rotate-a-key) |
| `DELETE` | `/v1/api-keys/{id}` | [Revoke a key](../api-keys.md#revoke-a-key) |

Each key's record has `id`, `name`, `key_type`, `status`, `livemode`,
`key_prefix`, `tenant_id`, `created_at`, and, when set, `expires_at`,
`revoked_at` and `last_used_at`. `last_used_at` is updated at most once a
minute per server, so it can lag a little.

### A key's status

`status` is `revoked` once the key is revoked. Otherwise it is `expired`
from the key's `expires_at` on, and `active` until then. Only an active key
works, and only an active key can be rotated. Expired and revoked keys stay
in the list.
