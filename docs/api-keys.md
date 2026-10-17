# API keys

An API key is the password your server sends with every API call. It tells
Velox which account the call is for, and whether it works on test data or
live data.

**Who it is for:** developers who call the Velox API from their own code or
from a terminal.

## Get a key

You already have one if you ran the [quick start](quick-start.md): it
printed a test key and saved it in
`deploy/compose/quickstart-credentials.json`.

To make a new key in the dashboard:

1. Open **API keys** and click **Create API Key**.
   **You see:** a dialog with a name field, the key types **Secret** and
   **Publishable**, and an expiry choice.
2. Give the key a name, such as `Production server`, pick **Secret**, and
   click **Create Key**.
   **You see:** the full key, with "Save this key now. It won't be shown
   again." Copy it into your secret store.

The new key gets the mode the dashboard is in. In test mode you get a
`vlx_secret_test_…` key; switch to live mode first to get a
`vlx_secret_live_…` key.

You can also create a key with the API: `POST /v1/api-keys` with
`{"name": "…", "key_type": "secret"}`. The response's `raw_key` is the full
key, and it is shown only this once.

## Send it

Put the key in the `Authorization` header as a Bearer token:

```bash
export VELOX=http://localhost:8080
export VELOX_KEY=vlx_secret_test_…   # your key

curl -s "$VELOX/v1/whoami" -H "Authorization: Bearer $VELOX_KEY"
```

**You see:** JSON with your `tenant_id` (your account's ID), `"livemode":
false` for a test key, and `"key_type": "secret"`.

A call with no key gets `401` and the message `missing credentials — use
Authorization: Bearer vlx_secret_...`.

## Test mode and live mode

Each account has two separate sets of data. The mode in the key's name
picks the set.

| Mode | Key starts with | What happens |
|---|---|---|
| Test | `vlx_secret_test_` | Works on test data only. Test mode connects only to Stripe test keys (`sk_test_…`), so no real card is charged. |
| Live | `vlx_secret_live_` | Works on live data. Charges real cards once you connect Stripe's live keys. |

Build and try everything with a test key. Switch to a live key when you go
live. Customers, prices and invoices made in test mode do not appear in live
mode.

## Key types

| Type | Starts with | Use it for |
|---|---|---|
| Secret | `vlx_secret_` | All API calls from your server. It can read and change everything in one account, so keep it out of browsers and out of source control. |
| Publishable | `vlx_pub_` | Places a key can be seen, such as a web page. It only proves which account it belongs to: it cannot read or change any data. |
| Platform | `vlx_platform_` | Managing accounts across a whole Velox server. You cannot create one in the dashboard or with a secret key. |

## Rotate a key

Rotating a key gives you a new key and retires the old one. Do it when a key
may have leaked, or on a schedule.

1. Open **API keys** and click **Rotate** on the key.
   **You see:** a dialog that asks when the current key should stop
   working: **Now**, **In 1 hour**, **In 24 hours** or **In 7 days**.
2. Pick a time and confirm.
   **You see:** the new key, shown once. Copy it.
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

## How Velox keeps keys safe

Velox stores only a salted SHA-256 hash of each key, never the key itself.
That is why a key is shown only once, when it is created.

Related: to add a second account to one Velox server, see
[Working on Velox locally](self-host.md#add-a-second-account).
