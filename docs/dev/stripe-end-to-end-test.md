# Stripe end-to-end test runbook

A hand check that Velox's Stripe integration works against a real Stripe
test account. You connect the account, save a card, charge it, receive
Stripe's webhooks, retry a declined card, refund, and disconnect.

Run it:

- before each [design-partner](../glossary.md) cutover, which brings an early
  pilot customer live on Velox;
- after any change touching `internal/payment/`, `internal/tenantstripe/` or
  `internal/dunning/`.

**Audience:** Velox maintainer, design-partner technical contact during
sandbox cutover.

**Never use live keys for this runbook.**

## Before you start

- **A Stripe test-mode account with keys minted.** At least a Restricted Key
  (a Stripe key limited to chosen permissions), or a Standard secret key and
  publishable key.
  - The secret key authenticates server-side API calls.
  - The publishable key is its browser-safe counterpart.
  - Once entered, the keys never leave the dashboard's connection form.
    Velox encrypts them at rest with the same AES-256-GCM key used for
    customer email.
- **The Stripe CLI** (`brew install stripe/stripe-cli/stripe`), and
  `$STRIPE_SK` set to your Stripe test secret key. Step 1b uses them.
- **`jq`**, to read the JSON responses.
- **The operator dashboard on `http://localhost:5173`**, for step 2b. Step 0
  starts only the API; start the dashboard as in the README
  [Quick start](../../README.md#quick-start).

To test a published release instead, start the Compose stack from
[`deploy/compose`](../../deploy/compose/README.md) with `VELOX_IMAGE` and
`VELOX_DASHBOARD_IMAGE` set to the release tag. Use its address in place of
`http://localhost:8080` and `http://localhost:5173`.

A secret API key (`Authorization: Bearer vlx_secret_test_…`) works in place
of the cookie in every call below.

---

## Step 0 — Stand up Velox locally

1. Start the services, bootstrap on first boot (it creates the owner and the
   tenant), and start the API:

   ```bash
   docker compose up -d postgres redis mailpit
   DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" \
     RUN_MIGRATIONS_ON_BOOT=true \
     go run ./cmd/velox-bootstrap   # only on first boot — creates owner + tenant
   DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" \
     RUN_MIGRATIONS_ON_BOOT=true \
     go run ./cmd/velox &
   ```

   Then run `curl -sf http://localhost:8080/health`.
   **You see:** `{"status":"ok"}`.

2. Log in as the owner the bootstrap step printed, and keep the cookie for
   the calls below:

   ```bash
   curl -s -X POST http://localhost:8080/v1/auth/login \
     -H 'Content-Type: application/json' \
     -c /tmp/velox-cookies.txt \
     -d '{"email":"<OWNER_EMAIL>","password":"<OWNER_PASSWORD>"}'
   ```

   **You see:** `/tmp/velox-cookies.txt` holds a `velox_session` cookie.

---

## Step 1 — Connect the Stripe test account

### 1a. Connect

The connection form encrypts secret keys at rest (AES-256-GCM, same key as
customer email). Do not paste keys into shell history or commit them. Write
them to a `umask 077` tmp file and delete it afterwards:

```bash
umask 077 && cat > /tmp/stripe-connect.json <<'EOF'
{
  "livemode": false,
  "secret_key": "sk_test_...",
  "publishable_key": "pk_test_..."
}
EOF
curl -s -X POST http://localhost:8080/v1/settings/stripe \
  -b /tmp/velox-cookies.txt \
  -H 'Content-Type: application/json' \
  -d @/tmp/stripe-connect.json
rm -f /tmp/stripe-connect.json
```

**You see (200):**

```json
{
  "id": "vlx_spc_...",
  "tenant_id": "vlx_ten_...",
  "livemode": false,
  "stripe_account_id": "acct_...",
  "stripe_account_name": "<your account display name>",
  "secret_key_prefix": "sk_test_...",
  "secret_key_last4": "...",
  "verified_at": "2026-04-27T..."
}
```

**What this proves:** Velox called `GET /v1/account` with the supplied secret
key, got an answer, and stored the encrypted keys. A `verified_at` timestamp
means the key shape and scope are valid and Stripe acknowledged the request.

If the response carries `last_verified_error`, the key is stored but Stripe
rejected the verify call. Common causes:

- a live key in test mode;
- a restricted key without `read_only` scope on Account.

> **Code note:** the verify call is `internal/tenantstripe/service.go` `Connect()` → `sc.V1Accounts.Retrieve`.

### 1b. Forward Stripe events to Velox

Do this before you save a card in step 2b. A saved card reaches Velox through
the `setup_intent.succeeded` webhook. Without the webhook secret in place,
Stripe's page says the card is saved and the customer still has no payment
method in Velox.

Velox verifies Stripe's webhook signatures with a **per-tenant** secret.
There is no operator-level `STRIPE_WEBHOOK_SECRET` env var and no
platform-level webhook URL. Each tenant's endpoint is
`/v1/webhooks/stripe/{endpoint_id}`, where `<endpoint_id>` is the `id`
from step 1a (`vlx_spc_…`, the tenant's `stripe_provider_credentials.id`).

1. Forward events to your local API, without exposing port 8080 to the
   public internet:

   ```bash
   stripe listen --api-key $STRIPE_SK \
     --forward-to http://localhost:8080/v1/webhooks/stripe/<endpoint_id>
   ```

   **You see:** the CLI prints a temporary webhook secret (`whsec_…`).
2. Paste the secret into the Velox dashboard under **Settings → Payments**,
   step 2 "Deliver Stripe events to Velox" (or send it with
   `PATCH /v1/settings/stripe/test/webhook`).
   **You see:** all later test-mode events flow through the CLI tunnel.

---

## Step 2 — Create a customer with a saved payment method

A [PaymentIntent](../glossary.md) is Stripe's object tracking the collection
of one payment. PaymentIntent flows require a saved payment method. The
Velox dashboard saves one through Stripe's Setup Intent flow, which saves a
card for later charges without charging it now.

Customer creation (2a) needs no browser. Attaching the card (2b) goes through
the dashboard and Stripe's hosted page.

### 2a. Create the Velox customer

```bash
curl -s -X POST http://localhost:8080/v1/customers \
  -b /tmp/velox-cookies.txt \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "Stripe E2E Smoke",
    "email": "e2e-smoke@velox.test",
    "external_id": "stripe-e2e-smoke-1"
  }'
```

**You see:** the new customer, with its `vlx_cus_...` ID. Keep the ID for
the next steps.

### 2b. Attach a Stripe test card through the dashboard

1. Open `http://localhost:5173/customers/<vlx_cus_...>`. In the **Payment
   methods** panel, click **Add payment method**, then **Copy link**.
2. Open the link in a browser.
   **You see:** Stripe's hosted Checkout page in setup mode.
3. Enter Stripe's standard test card, and save it. Choose "Pay without Link"
   if offered, and give a ZIP for a US address, or Save does nothing.
   - Card: `4242 4242 4242 4242`
   - Expiry: any future date
   - CVC: any 3 digits
   - Postal code: any 5 digits
4. Reload the customer page.
   **You see:** the **Payment methods** panel lists **Visa ····4242** with
   its expiry date.

Velox stores the resulting `pm_*` payment method ID as a row in the
`payment_methods` table for that customer. That table is the canonical store
for a customer's multiple payment methods.

> **Code note:** **Copy link** calls
> `POST /v1/customers/{id}/payment-methods/setup-session`, and
> `paymentmethods.Service.CreateSetupSession`
> (`internal/paymentmethods/service.go`) creates the Checkout session. The
> old `customer_payment_setups.stripe_payment_method_id` column was dropped
> in migration 0097.

### 2c. Failed-card variant (run after the happy path)

1. Repeat 2a with a new `external_id` and email, for example
   `stripe-e2e-smoke-2` and `e2e-decline@velox.test`. A customer's
   `external_id` is unique per account and mode.
2. Repeat 2b for that customer with `4000 0000 0000 0341`.
   **You see:** the card is listed, like `4242` was.

On this Stripe test card the attach succeeds but charges decline. Do not use
`4000 0000 0000 0002`: it declines at SetupIntent time too, so it never
produces this state. The decline in step 6 starts the
[dunning](../glossary.md) flow: automated retry-and-notify handling of a
failed payment.

---

## Step 3 — Subscribe the customer to a flat plan

Bootstrap seeds no plans, so create one first. Use the dashboard's Pricing
page, or instantiate a [recipe](../glossary.md): a pre-built pricing template
shipped with Velox.

1. Create the plan at **2900 cents/month**, because the amounts in steps 4
   and 7 assume it. Its base fee bills **in advance**
   (`base_bill_timing: in_advance`), so step 4 gets an invoice the moment the
   subscription starts. The default, in arrears, bills at the end of the
   period.

   ```bash
   curl -s -X POST http://localhost:8080/v1/plans \
     -b /tmp/velox-cookies.txt \
     -H 'Content-Type: application/json' \
     -d '{"code":"flat-29","name":"Flat 29","currency":"USD","billing_interval":"monthly",
          "base_amount_cents":2900,"base_bill_timing":"in_advance"}'
   ```

2. Subscribe the customer from 2a to it:

   ```bash
   PLAN_ID=$(curl -s http://localhost:8080/v1/plans -b /tmp/velox-cookies.txt \
     | jq -r '.data[0].id')

   curl -s -X POST http://localhost:8080/v1/subscriptions \
     -b /tmp/velox-cookies.txt \
     -H 'Content-Type: application/json' \
     -d "{
       \"code\": \"stripe-e2e-smoke\",
       \"display_name\": \"Stripe E2E Smoke\",
       \"customer_id\": \"<vlx_cus_...>\",
       \"items\": [{\"plan_id\": \"$PLAN_ID\", \"quantity\": 1}],
       \"billing_time\": \"anniversary\",
       \"start_now\": true
     }"
   ```

   **You see:** a `vlx_sub_...` with `status=active`, and current period
   bounds set to monthly (anchor: now).

---

## Step 4 — Check the immediate invoice and PaymentIntent

The flat-fee charge for the first cycle fires the moment the subscription
activates.

```bash
curl -s "http://localhost:8080/v1/invoices?customer_id=<vlx_cus_...>" \
  -b /tmp/velox-cookies.txt | jq '.data[0]'
```

**You see:** an invoice with `total_amount_cents=2900` and
`stripe_payment_intent_id` populated. With the `4242` card, the PaymentIntent
auto-confirms, and within about 5 seconds the invoice is `status=paid`,
`payment_status=succeeded` and `amount_due_cents=0`. The PaymentIntent ID
also shows on the Stripe dashboard under **Test data → Payments**.

---

## Step 5 — Check webhook delivery from Stripe to Velox

Verified events are stored in `stripe_webhook_events`, keyed on
`(tenant_id, livemode, stripe_event_id)` for idempotency. The key is a UNIQUE
constraint, so a replayed event cannot be stored twice. There is no
`processed` column. The secret is stored encrypted, in
`stripe_provider_credentials.webhook_secret_encrypted`.

> **Code note:** the per-request lookup is `LookupEndpoint` in
> `internal/payment/handler.go`.

**You see:** every Stripe event for steps 2–4 (`payment_method.attached`,
`payment_intent.created`, `payment_intent.succeeded`, `charge.succeeded`)
answered 200 in the `stripe listen` window, and stored as a row in
`stripe_webhook_events` (one per `stripe_event_id` per mode).

The dashboard's `/webhook_events` page does not show this inbound
Stripe-ingestion table. It shows **outbound** webhook deliveries: events
Velox sends to your configured endpoints.

---

## Step 6 — Failed payment and dunning

1. Repeat step 3, item 2, for the customer from step 2c (declined card). Use
   a new subscription `code`, such as `stripe-e2e-decline`: a code is unique
   per account and mode.
   **You see:** the first invoice's PaymentIntent fails.
2. List the dunning runs for that invoice:

   ```bash
   curl -s "http://localhost:8080/v1/dunning/runs?invoice_id=<vlx_inv_...>" \
     -b /tmp/velox-cookies.txt | jq
   ```

   **You see:** a [dunning run](../glossary.md), on the "Default retries"
   policy that bootstrap creates in each mode (ADR-123). Its first retry is
   due three days after the failure.

Retries follow the default policy's grace period and `retry_schedule`: day 3,
7 and 14 for "Default retries". Each retry is a fresh PaymentIntent: on the
Stripe dashboard's Payments page, the PI (PaymentIntent) ID changes per
attempt.

An account created before ADR-123 has no policy. The failed payment is then
skipped with a "dunning not configured" WARN, and the runs list stays empty.
Create a policy first (dashboard → Dunning policies, or
`POST /v1/dunning/policies`).

To recover the customer, swap the card via step 2b with `4242`, then wait for
the next scheduled dunning retry. Once the payment succeeds, the run resolves
automatically.

`POST /v1/dunning/runs/{id}/resolve` is the operator action to close a run
manually. There is no per-attempt force-retry endpoint.

---

## Step 7 — Refund via credit note

Issue a [credit note](../glossary.md) against the paid invoice from step 4. A
credit note is the document that records money credited or refunded against
an invoice.

```bash
curl -s -X POST http://localhost:8080/v1/credit-notes \
  -b /tmp/velox-cookies.txt \
  -H 'Content-Type: application/json' \
  -d '{
    "invoice_id": "<vlx_inv_...>",
    "reason": "duplicate",
    "lines": [
      {"description": "Refund - full invoice amount", "quantity": 1, "unit_amount_cents": 2900}
    ],
    "refund_amount_cents": 2900,
    "auto_issue": true
  }'
```

**You see:**

- The credit note, with `status=issued`.
- Because `refund_amount_cents` is set, Velox calls `refunds.Create` against
  Stripe, and the refund lands on the same charge.
- A `charge.refunded` webhook flows back. The credit note's `refund_status`
  flips to `succeeded`, and `stripe_refund_id` is populated.

A credit note has three allocation fields: `refund_amount_cents`,
`credit_amount_cents` and `out_of_band_amount_cents`. If you leave all three
at zero on a paid invoice, Velox defaults to `credit_amount = total`. That is
a customer-balance credit with **no** Stripe refund.

---

## Step 8 — Disconnect

```bash
curl -s -X DELETE http://localhost:8080/v1/settings/stripe/test \
  -b /tmp/velox-cookies.txt
```

**You see:** 204. The credentials row is deleted, and the encrypted blobs are
purged. Asking for a card-setup session afterwards answers 409 with code
`stripe_not_connected`.

---

## What to capture in your test report

A passing run reports:

- Stripe account name + ID returned from step 1 verify (proof the key works
  and is scoped to the intended account)
- Webhook event count from step 5 (≥4 events expected for the happy path)
- Final invoice status from step 4 (`paid` for happy, and `paid` after
  recovery in the dunning path)
- Refund webhook landed within 30 seconds (step 7)

Anything that does not match a **You see:** line above is a bug. File it
under the Velox repo with the request ID Velox returns in the error envelope:
`{"error": {"type", "code", "message", "request_id"}}`. The envelope is
defined in `internal/api/respond/respond.go`. The request ID is also echoed
in the `Velox-Request-Id` response header.

---

## What this runbook does NOT cover

- **Stripe Connect / Express accounts:** Velox is a self-host product, not a
  marketplace. Connect onboarding is out of scope.
- **Live mode:** explicitly forbidden in this runbook. Live testing belongs in
  the design-partner cutover playbook, maintained in the internal `velox-ops`
  repo.
- **Tax:** Stripe Tax integration is exercised separately. See
  [Tax calculation](../ops/tax-calculation.md).
- **Payouts / balance:** Velox does not surface Stripe payouts. The operator's
  Stripe dashboard is canonical for that.

---

## Last verified

| Date | Velox SHA | Stripe API version | Verified by | Result |
|---|---|---|---|---|
| 2026-04-27 | `f1b2301` | `2025-08-27.basil` (default) | maintainer | Step 1 (connect + verify) ✅; steps 2–8 documented as runbook, full flow validated during sandbox cutover for first design partner |
| 2026-10-13 | `v0.6.1` (published images, Compose) | `2026-03-25.dahlia` (Stripe CLI) | maintainer | Steps 1 to 5, 7 and 8 passed: account verified; cards `4242` and `0341` attached through Checkout; a 2,900 cent charge succeeded and a decline opened a dunning run, both matching Stripe's own PaymentIntent records; full refund through a credit note matched Stripe's refund; 23 webhook events, all answered 200. Not run: the dunning retry and recovery in step 6 (the first retry is three days out). Found and fixed in the same change: card setup after disconnect answered 500. |

Re-run and append a row after every change touching `internal/payment/`,
`internal/tenantstripe/`, or `internal/dunning/`.
