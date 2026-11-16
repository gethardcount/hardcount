# API keys

An API key is the password your server sends with every API call. It tells
Hardcount which account the call is for, and whether it works on test data or
live data.

## Get a key

You already have one if you ran the [quick start](quick-start.md): it
printed a test key and saved it in
`deploy/compose/quickstart-credentials.json`.

To make a new key in the dashboard:

1. Open **API keys** and click **Create API key**.
   **You see:** a dialog that says which mode the key will work in, with a
   name field, two key types, **Secret** (picked) and **Usage only**, and an
   expiration choice.
2. Give the key a name, such as `Production server`, pick its type (see
   [Key types](#key-types)), and click **Create key**.
   **You see:** the full key, with "Copy it now. Hardcount shows it only
   this once.", and a `curl` line that tries it. Copy the key into your
   secret store, then click **I've copied it**: the dialog closes no other
   way.

The new key gets the mode the dashboard is in. In test mode you get a
`hc_secret_test_…` key (`hc_ingest_test_…` for a usage-only key); switch to
live mode first to get a `hc_secret_live_…` key.

With the API, send `name` (1 to 100 characters), `key_type` (`secret`, the
default, or `ingest`) and an optional `expires_at`: a time in the future,
in RFC 3339 form, when the key stops working. [Send it](#send-it) sets the
two variables.

```bash
curl -s -X POST "$HARDCOUNT/v1/api-keys" \
  -H "Authorization: Bearer $HARDCOUNT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name": "LiteLLM proxy", "key_type": "ingest", "expires_at": "2027-01-01T00:00:00Z"}'
```

**You see:** `201` with `key`, the key's
[record](api/authentication.md#manage-keys-with-the-api), and `raw_key`, the
full key, shown only this once. An `expires_at` that is not in the future
gets `422`. The route ignores `Idempotency-Key`, so a retried create makes a
second key; if you lose the answer, see
[Routes that ignore the header](api/idempotency.md#routes-that-ignore-the-header).

`GET /v1/api-keys` lists the keys of your mode, without the keys themselves.

## Send it

The **Quick start** box at the top of **API keys** shows the address to call.
Put the key in the `Authorization` header as a Bearer token. In the
commands below, put your own key in place of `hc_secret_test_…`:

```bash
export HARDCOUNT=http://localhost:8080
export HARDCOUNT_KEY=hc_secret_test_…

curl -s "$HARDCOUNT/v1/whoami" -H "Authorization: Bearer $HARDCOUNT_KEY"
```

**You see:** JSON with your `tenant_id` (your account's ID), `"livemode":
false` for a test key, and `"key_type": "secret"` (`"ingest"` for a
usage-only key).

If the call is refused, see
[When a key is refused](api/authentication.md#when-a-key-is-refused).

## Test mode and live mode

Each account has two separate sets of data, and the mode in the key's name
picks the set. Build and try everything with a test key, and switch to a
live key when you go live: customers, prices and invoices made in test mode
do not appear in live mode.

| Mode | Type prefix | What happens |
|---|---|---|
| Test | `hc_secret_test_` | Works on test data only. Test mode connects only to Stripe test keys (`sk_test_…`), so no real card is charged. |
| Live | `hc_secret_live_` | Works on live data. Charges real cards once you connect Stripe's live keys. |

A key starts with its type and mode, and Hardcount stores only a hash of
it, so the full key is shown only once
([Key format](api/authentication.md#key-format)).

## Key types

There are two: a **secret** key (`hc_secret_…`) for your own server, and a
**usage-only** key (`hc_ingest_…`, `ingest` in the API) for LiteLLM or
another gateway that only sends usage. What each one can call, and which
one goes where: [Key types](api/authentication.md#key-types). Keep a secret
key out of browsers and out of source control.

## Rotate a key

Rotating a key gives you a new key and retires the old one. Do it when a key
may have leaked, or on a schedule.

1. Open **API keys**, open the key's menu (**…**) and click **Rotate**.
   **You see:** a dialog that asks when the old key stops working: **Now**,
   **In 1 hour**, **In 24 hours** (picked) or **In 7 days**.
2. Pick a time and click **Rotate key**.
   **You see:** the new key, shown once, in a dialog titled **API key
   rotated**, which says when the old key stops working. Copy the key, then
   click **I've copied it**.
3. Put the new key in your servers' settings and redeploy before the time
   you picked.
   **You see:** your calls keep working. After that time, the old key gets
   `401`.

Pick **Now** only if you can switch every server at once, or if the key has
leaked: the dialog warns that anything still using the old key fails until
it has the new one. The new key keeps the old key's name, type, mode and
`expires_at`, so rotating does not extend a key that is about to expire:
[create a new key](#get-a-key) for that. The old key never works past its
own `expires_at`, whatever time you pick.

With the API: `POST /v1/api-keys/{id}/rotate`, with an optional
`{"expires_in_seconds": 86400}`. The API's default, `0`, revokes the old
key; the dashboard's dialog picks 24 hours instead. The longest wait is 7
days (`604800`). The answer has `old_key`, `new_key` and `raw_key`, and like
create, the route ignores `Idempotency-Key`. A revoked or expired key cannot
be rotated (`409`): create a new key instead.

## Revoke a key

Revoking turns a key off for good, with no replacement. The key is refused
from the next request; a request already running finishes.

1. Open **API keys**, open the key's menu (**…**) and click **Revoke**.
   **You see:** a dialog that names the key. For a live key it says "Type
   REVOKE to confirm".
2. Type `REVOKE` if asked, and click **Revoke key**.
   **You see:** "API key revoked", and the key leaves the table. A link
   under the table, such as **Show 1 revoked key**, shows the
   [expired and revoked](api/authentication.md#a-keys-status) keys.

With the API: `DELETE /v1/api-keys/{id}`. Revoking a key a second time
answers `404`. An open event stream closes within 15 seconds of the revoke
([When a key is refused](api/authentication.md#when-a-key-is-refused)).

**Next:** [make your first API call](api-surface.md#make-your-first-call).
The whole key reference: [Authentication and keys](api/authentication.md).
