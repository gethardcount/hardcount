# Webhooks

A webhook is a message Hardcount sends to a URL in your app when something
happens, such as an invoice paid, a payment failed or a credit balance
running low. Your app can then react: send a receipt, pause a service,
remind a customer to top up.

This page is the reference: the message, the signature, endpoints, modes and
retries.

- To build a receiver step by step, with signature checks in Node, Python
  and Go: [Receive webhooks](guides/receive-webhooks.md).
- Every event, when it is sent, and an example of each:
  [Webhook events](api/webhook-events.md).

## Three rules for your receiver

1. **The same event can arrive more than once.** Once you have handled an
   event, store its `id`, and skip any event whose `id` you have stored.
   Store it after handling, not on arrival: otherwise a failure in between
   makes you skip the retry and any replay, and the event is never handled.
2. **Events can arrive out of order.** Do not work out the current state
   from the order they arrive. Read the event's data, or fetch the
   invoice or subscription from the API.
3. **Events marked *always sent* survive a crash; others may not.** Those
   events in the [catalog](api/webhook-events.md) are saved together with
   the change that caused them, so a crash cannot drop them. Other events,
   including `invoice.finalized`, are queued just after the change is
   saved, and a crash or a database error at that moment can drop one.
   Any event can still go undelivered:
   - To an endpoint that keeps failing, a delivery is marked failed after 6
     attempts over about 26 hours 36 minutes ([Retries](#retries)). Send it
     again from the dashboard or the API ([Send a delivery again](#send-a-delivery-again)).
   - The queue gives up on an event it could not write after 15 tries over
     about 52 hours. That event was never written, so nothing can resend
     it; the person who runs your server can queue it again:
     [Webhook outbox backed up](ops/runbook/webhook-outbox-backed-up.md#send-failed-rows-again).

## What a message looks like

Every message is an HTTP `POST` to your URL with
`Content-Type: application/json` and this body:

```json
{
  "id": "hc_whevt_…",
  "event_type": "invoice.paid",
  "created_at": "2026-07-05T14:03:22Z",
  "data": {
    "invoice": { "id": "hc_inv_…", "customer_id": "hc_cus_…", "status": "paid", "amount_paid_cents": 4900, "…": "…" }
  }
}
```

- `id` is the event's ID and `created_at` is when Hardcount created the event.
  Both stay the same on every retry and replay, so use `id` to skip repeats
  (rule 1).
- `data` holds the resource the event is about, under the resource's name:
  `data.invoice`, `data.subscription` and so on. It is the same object the
  API returns for that resource, as it was when Hardcount created the event.
  Every event of a family carries the same resource. To act on the newest
  state, read the resource from the API.
- A few events also carry facts the resource does not hold, such as who
  canceled a subscription. They sit beside the resource in `data`. The
  [catalog](api/webhook-events.md) lists them for each event.
- The message has no field that says whether it is a test or a live event.
  Each endpoint belongs to one mode; see
  [Test mode and live mode](#test-mode-and-live-mode).
- This shape is new in v0.9.0. v0.9.0 installs fresh, so no event in it
  has an older shape ([old key → new key](../CHANGELOG.md)).

Headers:

| Header | Value |
|---|---|
| `Hardcount-Signature` | `t=<unix-seconds>,v1=<hex hmac>` |
| `Hardcount-Event-Type` | The event type, so you can route the message before you read the body |

## Verifying signatures

The signature is `HMAC-SHA256(secret, "<t>.<raw body>")`, hex-encoded.

- The key is the full secret string, `whsec_` prefix included, used as
  UTF-8 bytes. Do not strip the prefix or decode it.
- Check against the raw request bytes. Do not parse and re-serialize the
  JSON first.
- `t` is when this attempt was signed, so it is new on every retry.
  Refuse a `t` far from your own clock, such as more than 5 minutes.
- While a secret is being rotated, the header has two `v1=` entries, one
  for the old secret and one for the new. Accept the message if any `v1=`
  matches.

Code that checks it, in Node, Python and Go:
[Check the signature in your code](guides/receive-webhooks.md#check-the-signature-in-your-code).

### Rotating the secret

The secret (`whsec_…`) is shown once when you create the endpoint, and once
when you rotate it with
`POST /v1/webhook-endpoints/endpoints/{id}/rotate-secret`.

- For 72 hours after a rotation, Hardcount signs each message with both the
  old and the new secret. The answer's `secondary_valid_until` says when
  the old one stops.
- Rotating again within those 72 hours drops the older of the two secrets
  at once.
- Changing an endpoint with `PATCH` (its URL, events, or whether it is
  active) never changes the secret.

Steps: [Rotate the secret](guides/receive-webhooks.md#rotate-the-secret).

## Test mode and live mode

**An endpoint belongs to the mode of the key that registered it.** An
endpoint registered with a test key gets only test events. One registered
with a live key gets only live events.

To receive both, register one endpoint with each key. Each has its own
secret. Give each its own URL, so your receiver knows the mode and the
secret to check before it reads the body.

## Managing endpoints

Manage endpoints in the dashboard under **Webhooks**, or with the API (see
the [API reference](api-surface.md#the-full-reference)). Each route works on
the endpoints of the caller's mode.

| Method | Path | What it does |
|---|---|---|
| `POST` | `/v1/webhook-endpoints/endpoints` | Register an endpoint: `url`, `events`, and an optional `description`. The answer has `endpoint` and `secret`. |
| `GET` | `/v1/webhook-endpoints/endpoints` | List endpoints |
| `GET` | `/v1/webhook-endpoints/endpoints/{id}` | Read one endpoint |
| `PATCH` | `/v1/webhook-endpoints/endpoints/{id}` | Change `url`, `events`, `description` or `active` |
| `DELETE` | `/v1/webhook-endpoints/endpoints/{id}` | Remove an endpoint |
| `POST` | `/v1/webhook-endpoints/endpoints/{id}/rotate-secret` | Make a new secret ([Rotating the secret](#rotating-the-secret)) |
| `GET` | `/v1/webhook-endpoints/endpoints/{id}/deliveries` | The endpoint's deliveries, newest first: `status` (`pending`, `succeeded` or `failed`), `status_code`, `attempt_count`, `next_retry_at`, `error` and the start of your endpoint's `response_body` |
| `POST` | `/v1/webhook-endpoints/endpoints/{id}/deliveries/{deliveryID}/replay` | Send one delivery's event again, to this endpoint only |
| `GET` | `/v1/webhook-endpoints/endpoints/stats` | Each endpoint's `total_deliveries`, `succeeded`, `failed` and `success_rate` |
| `GET` | `/v1/webhook-endpoints/events` | Events, newest first, 50 to a page by default (`limit` up to 100, `offset`, and the `total`). It has no filters. |
| `GET` | `/v1/webhook_events/{id}/deliveries` | Every delivery of one event, to every endpoint |
| `POST` | `/v1/webhook_events/{id}/replay` | Send one event again, to every endpoint that matches it now |

- `events` takes event names from the [catalog](api/webhook-events.md).
  `"*"` means every event, and a prefix such as `"invoice.*"` means every
  event that starts with it.
- An event name Hardcount never sends is refused with `422`, so a typo cannot
  leave you waiting for messages that never come.
- **`active: false` pauses an endpoint.** It gets no new events. A retry
  that falls due while it is paused is marked failed, with the error
  `endpoint disabled`. Resending to a paused endpoint is refused with `409`.
  Set `active: true` to resume it.
- The URL must start with `https://` and must not point to a private
  network address, IPv4 or IPv6, such as `10.0.0.1`, `169.254.169.254`,
  `fd00::1` or `fe80::1`. Hardcount checks the address when you save the URL
  and again each time it sends.
- With `APP_ENV=production`, `localhost` and loopback addresses
  (`127.0.0.1`, `::1`) are refused too. With any other `APP_ENV`, staging
  included, they are allowed, over `http://` as well.

## Retries

A message counts as delivered when your endpoint answers with any `2xx`
status within 10 seconds. Anything else is retried on this schedule:

| Attempt | Wait after the previous failure |
|---|---|
| 1 | none |
| 2 | 1 minute |
| 3 | 5 minutes |
| 4 | 30 minutes |
| 5 | 2 hours |
| 6 | 24 hours |

Each wait also gets up to 30 seconds added at random. That is six attempts
over about 26 hours 36 minutes. After the last one, the delivery is marked
failed.

Hardcount does not follow redirects, so a `3xx` answer is retried like any
other failure: register the URL that answers with the `2xx` itself.

### Send a delivery again

Failed deliveries stay visible in the dashboard, with each endpoint's
success rate and each event's delivery history. Once your endpoint works
again, send what it missed:

- **To one endpoint:** **Resend** on the delivery in the dashboard, or
  `POST /v1/webhook-endpoints/endpoints/{id}/deliveries/{deliveryID}/replay`.
  Only that endpoint gets it.
- **To every endpoint:** `POST /v1/webhook_events/{id}/replay`. Every active
  endpoint that matches the event now gets it, including ones that already
  had it.

A resend carries the event's original `id` and `created_at`, so a receiver
that skips handled ids handles it once. Each resend is a new delivery, with
its own attempts and retries. A script that resends every failed delivery
of an endpoint:
[Resend what your endpoint missed](guides/receive-webhooks.md#resend-what-your-endpoint-missed).

## Event catalog

Every event Hardcount sends, when it sends it, what its `data` carries, and
an example message: [Webhook events](api/webhook-events.md).

Two things change how you read the amounts in those events.

### `invoice.finalized` shows the amount before credit

Hardcount uses the customer's credit when it collects the invoice. That is
after it sends `invoice.finalized`, usually within seconds. Then it charges
the card for what is left. So in this event's invoice:

- `amount_due_cents` is the amount before credit.
- `credits_applied_cents` is 0: credit is used after this event.

Credit is used again each time Hardcount tries to collect an unpaid invoice,
such as a payment retry. So credit the customer gets later can pay an older
invoice.

No invoice event is sent when credit pays only part of an invoice. When
credit pays all of it, `invoice.paid` is sent. To know what is still owed,
read the invoice with `GET /v1/invoices/{id}`: its `amount_due_cents` and
`credits_applied_cents` are current.

### Credit: `data.credit_balance`

The credit events carry the customer's credit balance in one currency. The
list, with examples:
[Credit events](api/webhook-events.md#credit-datacredit_balance).

### The credit events follow the settled balance

`credit.balance_low`, `credit.balance_depleted` and
`credit.balance_recovered` follow the settled balance: the credit a customer
holds after every invoice issued so far. They fire when an invoice, a grant,
an adjustment or an expiry moves that balance. They do not follow usage.
Credit a void or credit note hands back moves the balance once: if part of
it expires at once, because its grant was past its expiry, the events
follow the net change.

Each currency has its own balance, and each event is about one of them: a
customer whose EUR credit runs out gets `credit.balance_depleted` with
`credit_balance.currency` `EUR` even while it holds USD credit. Credit pays
only invoices in its own currency. The low threshold is one number applied
in each currency's own units: 1000 means $10.00 for USD credit and €10.00
for EUR credit.

Usage draws on credit only when it is invoiced. A customer billed at the end
of the period can use up all their credit in the first week, and
`credit.balance_depleted` fires weeks later, when the invoice is issued.

So use these events to remind a customer to top up, and to keep your own
records in step. Do not use them to decide whether to keep serving a
customer. For that, poll `GET /v1/credits/balance/{customer_id}/live`, which
subtracts the usage not yet invoiced. See
[Stop usage when credit runs out](stop-usage-when-credit-runs-out.md).

A typical receiver for prepaid customers subscribes to `credit.balance_low`
(remind the customer to top up), `credit.balance_recovered` (a top-up
landed), and `subscription.canceled` (turn off their service).

**Next:** gate usage on the live credit balance: [Stop usage when credit runs out](stop-usage-when-credit-runs-out.md).
