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
   **You see:** a dialog with a name field, two key types, **Secret**
   (picked) and **Usage only**, and an expiry choice.
2. Give the key a name, such as `Production server`, pick its type (see
   [Key types](#key-types)), and click **Create key**.
   **You see:** the full key, with "Copy it now. Hardcount shows it only
   this once.", and a `curl` line that tries it. Copy the key into your
   secret store, then click **I've copied it**: the dialog closes no other
   way.

The new key gets the mode the dashboard is in. In test mode you get a
`hc_secret_test_…` key (`hc_ingest_test_…` for a usage-only key); switch to
live mode first to get a `hc_secret_live_…` key.

You can also create a key with the API: `POST /v1/api-keys` with
`{"name": "…", "key_type": "secret"}`, or `"key_type": "ingest"` for a
usage-only key. Add `"expires_at": "2027-01-01T00:00:00Z"` for a key that
stops working at that time. The response's `raw_key` is the full key, and it
is shown only this once. `GET /v1/api-keys` lists the keys of your mode,
without the keys themselves. Every field and answer:
[Authentication and keys](api/authentication.md#manage-keys-with-the-api).

## Send it

The **Quick start** box at the top of **API keys** shows the address to call.
Put the key in the `Authorization` header as a Bearer token:

```bash
export HARDCOUNT=http://localhost:8080
export HARDCOUNT_KEY=hc_secret_test_…   # your key

curl -s "$HARDCOUNT/v1/whoami" -H "Authorization: Bearer $HARDCOUNT_KEY"
```

**You see:** JSON with your `tenant_id` (your account's ID), `"livemode":
false` for a test key, and `"key_type": "secret"` (`"ingest"` for a
usage-only key).

A call with no key gets `401` and the message `missing credentials — sign
in at /login or send Authorization: Bearer hc_secret_...`. A wrong, revoked
or expired key gets `401` and `invalid or expired API key`. A `503` with code
`authentication_unavailable` means Hardcount could not check the key just
then; your key is fine, so retry. More:
[When a key is refused](api/authentication.md#when-a-key-is-refused).

## Test mode and live mode

Each account has two separate sets of data. The mode in the key's name
picks the set.

| Mode | Key starts with | What happens |
|---|---|---|
| Test | `hc_secret_test_` | Works on test data only. Test mode connects only to Stripe test keys (`sk_test_…`), so no real card is charged. |
| Live | `hc_secret_live_` | Works on live data. Charges real cards once you connect Stripe's live keys. |

Build and try everything with a test key. Switch to a live key when you go
live. Customers, prices and invoices made in test mode do not appear in live
mode.

## Key types

| Type | Starts with | Use it for |
|---|---|---|
| Secret | `hc_secret_` | All API calls from your server. It can read and change everything in one account, so keep it out of browsers and out of source control. |
| Usage only (`ingest` in the API) | `hc_ingest_` | Sending usage from LiteLLM or another gateway. It can call only `POST /v1/usage-events`, `POST /v1/usage-events/batch` and `POST /v1/integrations/litellm/spend`, plus `GET /v1/whoami`. |

A gateway keeps its key in a config file, often on another machine. Give
it a usage-only key: if that key leaks, it can add usage but cannot refund,
void, read your customers or make keys. Every other call with it gets
`403` and a message such as `insufficient permissions: this key type does
not have customer:read access`. It cannot backfill past usage or release
held usage either: do those with a secret key or in the dashboard.

A dashboard session can do everything a secret key can.

## Rotate a key

Rotating a key gives you a new key and retires the old one. Do it when a key
may have leaked, or on a schedule.

1. Open **API keys** and click **Rotate** on the key.
   **You see:** a dialog that asks when the current key should stop
   working: **Now**, **In 1 hour**, **In 24 hours** or **In 7 days**.
2. Pick a time and confirm.
   **You see:** the new key, shown once, in a dialog titled **Key rotated**.
   Copy it, then click **I've copied it**.
3. Put the new key in your servers' settings and redeploy before the time
   you picked.
   **You see:** your calls keep working. After that time, the old key gets
   `401`.

Pick **Now** only if you can switch every server at once, or if the key has
leaked. The new key keeps the old key's name, type and mode.

With the API: `POST /v1/api-keys/{id}/rotate`, with an optional
`{"expires_in_seconds": 86400}`. The default, `0`, stops the old key at
once. The longest wait is 7 days (`604800`).

To turn a key off without a replacement, click **Revoke**, or call
`DELETE /v1/api-keys/{id}`. It stops working at once.

## How Hardcount keeps keys safe

Hardcount stores only a salted SHA-256 hash of each key, never the key itself.
That is why a key is shown only once, when it is created.

**Next:** [make your first API call](api-surface.md#make-your-first-call).
The whole key reference: [Authentication and keys](api/authentication.md).
