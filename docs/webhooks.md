# Webhooks

A webhook is a message Velox sends to a URL in your app when something
happens, such as an invoice paid, a payment failed or a credit balance
running low. Your app can then react: send a receipt, pause a service,
remind a customer to top up.

Think of a doorbell. You do not keep checking the door; it rings when
someone arrives. Webhooks ring your app when billing changes.

**Who it is for:** developers building the endpoint in their app that
receives these messages. No earlier Velox knowledge is needed.

## Receive your first event

This takes about five minutes. You run a small test receiver, register it
with Velox, make something happen, and check the message.

**Before you start:** Velox is running, and you have a test API key (see
[API keys](api-keys.md)). Open a terminal in the `velox` folder and set:

```bash
export VELOX=http://localhost:8080
export VELOX_KEY=vlx_secret_test_…   # your test key
```

1. **Start the test receiver.** It answers every message with `200` and
   writes each one to a file.

   ```bash
   python3 scripts/dev/webhook-sink.py /tmp/velox-webhooks.jsonl
   ```

   **You see:** `webhook sink on 127.0.0.1:9099 -> /tmp/velox-webhooks.jsonl (paths under /fail return 500)`

2. **Pick the address Velox will call.** It depends on how you run Velox.

   - **From source** (`make dev`): use `http://localhost:9099/ok`.
   - **In Docker** (the [quick start](quick-start.md) or a server): this
     setup refuses `localhost` and private network addresses, so Velox
     cannot reach the receiver directly. In a second terminal, start a
     tunnel, which gives your computer a public `https://` address. For
     example: `cloudflared tunnel --url http://localhost:9099`. Use the
     `https://` address it prints, plus `/ok`.

   ```bash
   export HOOK_URL=http://localhost:9099/ok   # or https://<tunnel address>/ok
   ```

   **You see:** nothing yet. The address is saved in `HOOK_URL`.

3. **Register the endpoint.** `"*"` asks for every event.

   ```bash
   jq -n --arg url "$HOOK_URL" '{url: $url, events: ["*"]}' |
     curl -s -X POST "$VELOX/v1/webhook-endpoints/endpoints" \
       -H "Authorization: Bearer $VELOX_KEY" \
       -H "Content-Type: application/json" \
       -d @-
   ```

   **You see:** JSON with `endpoint.id` (`vlx_whe_…`) and `secret`
   (`whsec_…`). The secret is shown only this once. Save it:
   `export WHSEC=whsec_…`.

4. **Make something happen.** Run the demo again. It creates a
   subscription and a month of usage, then bills it.
   - Docker quick start: `cd deploy/compose && ./quickstart.sh demo`
   - From source: `./scripts/demo.sh $VELOX_KEY`

   Then look at the file:

   ```bash
   tail -n 3 /tmp/velox-webhooks.jsonl
   ```

   **You see:** one JSON line per message, each with `"path": "/ok"`, a
   `"sig"` such as `t=…,v1=…`, and a `"body"` whose `event_type` is, for
   example, `subscription.created` or `invoice.finalized`.

5. **Check the signature.** This proves the message came from Velox.

   ```bash
   head -n 1 /tmp/velox-webhooks.jsonl | python3 -c '
   import hashlib, hmac, json, os, sys
   r = json.loads(sys.stdin.readline())
   p = dict(x.split("=", 1) for x in r["sig"].split(","))
   mac = hmac.new(os.environ["WHSEC"].encode(), (p["t"] + "." + r["body"]).encode(), hashlib.sha256)
   print(mac.hexdigest() == p["v1"])'
   ```

   **You see:** `True`

The dashboard shows the same messages: open **Webhooks**, then your
endpoint, to see each delivery and its result.

To test failures, register an address whose path starts with `/fail`, such
as `http://localhost:9099/fail`. The receiver answers `500`, so Velox
retries on the schedule below.

## Three rules for your receiver

1. **The same event can arrive more than once.** Store each event's `id`
   and skip one you have seen before.
2. **Events can arrive out of order.** Do not work out the current state
   from the order they arrive. Read the event's data, or fetch the
   invoice or subscription from the API.
3. **Money events are never lost; a few others can be.** The events marked
   *(always sent)* in the [catalog](#event-catalog) are saved together with
   the change that caused them. If the change is saved, the event will be
   sent, even after a crash. Other events are queued just after the change
   is saved, and a crash at that moment can drop one.

## What a message looks like

Every message is an HTTP `POST` to your URL with
`Content-Type: application/json` and this body:

```json
{
  "id": "vlx_whevt_…",
  "event_type": "invoice.paid",
  "created_at": "2026-07-05T14:03:22Z",
  "data": { "invoice_id": "vlx_inv_…", "customer_id": "vlx_cus_…", "…": "…" }
}
```

- `id` is the event's ID. It stays the same on every retry and replay, so
  use it to skip repeats (rule 1).
- `data` holds the event's details. The [catalog](#event-catalog) says what
  each event carries.

Headers:

| Header | Value |
|---|---|
| `Velox-Signature` | `t=<unix-seconds>,v1=<hex hmac>`. It has two `v1=` entries while a secret is being rotated. |
| `Velox-Event-Type` | The event type, so you can route the message before you read the body |

## Verifying signatures

The signature is `HMAC-SHA256(secret, "<t>.<raw body>")`, hex-encoded.

- The key is the full secret string, `whsec_` prefix included, used as
  UTF-8 bytes. Do not strip the prefix or decode it.
- Check against the raw request bytes. Do not parse and re-serialize the
  JSON first.
- While a secret is being rotated, the header has two `v1=` entries, one
  for the old secret and one for the new. Accept the message if any `v1=`
  matches.

Node:

```js
const crypto = require("crypto");

function verifyVeloxSignature(rawBody, header, secret, toleranceSec = 300) {
  const t = header.match(/(?:^|,)t=(\d+)/)?.[1];
  // Collect every v1= entry: there are two during a secret rotation.
  const sigs = [...header.matchAll(/v1=([0-9a-f]+)/g)].map(m => m[1]);
  if (!t || sigs.length === 0) return false;
  if (Math.abs(Date.now() / 1000 - Number(t)) > toleranceSec) return false;
  const expected = crypto
    .createHmac("sha256", secret)
    .update(`${t}.${rawBody}`)
    .digest("hex");
  return sigs.some(s =>
    s.length === expected.length &&
    crypto.timingSafeEqual(Buffer.from(s), Buffer.from(expected)),
  );
}
```

Python:

```python
import hashlib, hmac, re, time

def verify_velox_signature(raw_body: bytes, header: str, secret: str, tolerance: int = 300) -> bool:
    m = re.search(r"(?:^|,)t=(\d+)", header)
    sigs = re.findall(r"v1=([0-9a-f]+)", header)
    if not m or not sigs:
        return False
    t = int(m.group(1))
    if abs(time.time() - t) > tolerance:
        return False
    expected = hmac.new(secret.encode(), f"{t}.".encode() + raw_body, hashlib.sha256).hexdigest()
    return any(hmac.compare_digest(s, expected) for s in sigs)
```

### Rotating the secret

The secret (`whsec_…`) is shown once when you create the endpoint, and once
when you rotate it.

1. Call `POST /v1/webhook-endpoints/endpoints/{id}/rotate-secret`.
   **You see:** the new secret. For the next 72 hours, Velox signs each
   message with both the old and the new secret.
2. Update your receiver to use the new secret within those 72 hours.
   **You see:** messages keep passing your check, because one of the two
   `v1=` entries matches.

Changing an endpoint with `PATCH` (its URL, events, or whether it is
active) never changes the secret.

## Managing endpoints

Manage endpoints in the dashboard under **Webhooks**, or with the API (see
the [API reference](api-surface.md#the-full-reference)):

| Method | Path | What it does |
|---|---|---|
| `POST` | `/v1/webhook-endpoints/endpoints` | Register an endpoint |
| `GET` | `/v1/webhook-endpoints/endpoints` | List endpoints |
| `PATCH` | `/v1/webhook-endpoints/endpoints/{id}` | Change the URL, events or active flag |
| `DELETE` | `/v1/webhook-endpoints/endpoints/{id}` | Remove an endpoint |

- `events` takes event names from the [catalog](#event-catalog). `"*"`
  means every event, and a prefix such as `"invoice.*"` means every event
  that starts with it.
- An event name Velox never sends is refused with `422`, so a typo cannot
  leave you waiting for messages that never come.
- The URL must start with `https://` and must not point to a private
  network address, IPv4 or IPv6, such as `10.0.0.1`, `169.254.169.254`,
  `fd00::1` or `fe80::1`. Velox checks the address when you save the URL
  and again each time it sends.
- On a server (`APP_ENV=production`), `localhost` is refused too. In local
  development, `localhost` is allowed, over `http://` as well.

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

That is six attempts over about 26.5 hours. After the last one, the
delivery is marked failed.

**If your endpoint was down for longer:** failed deliveries stay visible in
the dashboard, with each endpoint's success rate and each event's delivery
history. Send any event again from there, or with
`POST /v1/webhook_events/{id}/replay`. `GET /v1/webhook-endpoints/events`
lists the newest 50 events; it has no filters.

## Event catalog

Two words used below:

- **Dunning** is Velox retrying a failed payment on a schedule and emailing
  the customer, until the invoice is paid or written off.
- **Your account** is your business in Velox (a "tenant" in the API), as
  opposed to the customers you bill.

Events marked *(always sent)* are saved together with the change that
caused them, so a crash cannot drop them (rule 3).

### Invoices and payments

| Event | Sent when |
|---|---|
| `invoice.finalized` | An invoice is final and ready to pay |
| `invoice.paid` | An invoice is fully paid, by card, credit or an offline payment *(always sent)* |
| `invoice.payment_recorded` | Someone recorded a payment made outside Velox, such as a bank transfer or cheque |
| `invoice.marked_uncollectible` | An invoice was written off as bad debt |
| `invoice.voided` | An invoice was voided |
| `payment.succeeded` | A charge was collected. Carries the Stripe PaymentIntent ID. *(always sent)* |
| `payment.failed` | A charge the invoice was waiting on failed. Sent once per charge attempt (PaymentIntent). A decline on the hosted payment page does not send it, because the customer saw it there and can try again. *(always sent)* |
| `payment_method.attached` | A customer saved a payment method. Invoices waiting for one are charged on the next run. |
| `payment_method.updated` | A customer's payment method was set up or replaced. Carries the card brand and last 4 digits. |
| `payment.duplicate_charge` | A second charge succeeded on an invoice that was already paid. Refund it. |
| `payment.amount_mismatch` | A charge settled for a different amount than the invoice owed |
| `payment.received_on_voided_invoice` | Money arrived for a voided invoice. Refund it. |

### Subscriptions

| Event | Sent when |
|---|---|
| `subscription.created` | A subscription was created *(always sent)* |
| `subscription.activated` | A draft subscription was activated *(always sent)* |
| `subscription.canceled` | A subscription was canceled. `canceled_by` is `operator` or `schedule`; a cancel at trial end carries `reason: trial_end_cancel`. *(always sent)* |
| `subscription.trial_ended` | A trial ended. `triggered_by` is `operator` or `schedule`. *(always sent)* |
| `subscription.trial_extended` | A trial's end date moved later |
| `subscription.cancel_scheduled` / `.cancel_cleared` | A cancel at period end was scheduled, or the schedule was removed |
| `subscription.collection_paused` / `.collection_resumed` | Collection was paused (invoices stay drafts), or resumed |
| `subscription.item.added` / `.updated` / `.removed` | A plan or quantity on the subscription was added, changed or removed |
| `subscription.pending_change.scheduled` / `.applied` / `.canceled` | A change set for the next billing period was scheduled, applied or canceled |
| `subscription.threshold_crossed` | Usage crossed a billing threshold, so an invoice was made early |

### Customers, dunning and credit

| Event | Sent when |
|---|---|
| `customer.email_bounced` | An email to a customer bounced, and Velox stopped emailing that address |
| `customer.email_complained` | A customer marked an email from Velox as spam, and Velox stopped emailing that address |
| `dunning.started` | Payment retries started for an invoice *(always sent)* |
| `dunning.escalated` / `.resolved` | Payment retries ran out (the final action from your policy was taken), or ended early, for example because the invoice was paid or someone resolved it by hand |
| `credit.balance_low` | The settled credit balance fell below your account's threshold. Carries `threshold_cents`. *(always sent)* |
| `credit.balance_depleted` | The settled credit balance reached zero or less *(always sent)* |
| `credit.balance_recovered` | The settled credit balance went above zero again *(always sent)* |
| `credit.commit_retired` | A credit note took back prepaid credit. Carries `grant_id`, `credit_note_id`, `retired_cents`, `refunded_gross_cents` and `remaining_after_cents`. *(always sent)* |

### The credit events follow the settled balance

`credit.balance_low`, `credit.balance_depleted` and
`credit.balance_recovered` follow the settled balance: the credit a customer
holds after every invoice issued so far. They fire when an invoice, a grant,
an adjustment or an expiry moves that balance. They do not follow usage.

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
