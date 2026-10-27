# The Velox API

The Velox API is how your code talks to Velox: you create customers, send
usage, and read invoices with plain HTTP requests and JSON. Everything the dashboard does
goes through the same API.

New to Velox? Start with [Send usage and get your first invoice](first-invoice.md).

## Make your first call

Every call needs three things:

- **The address** of your Velox server. After the
  [quick start](quick-start.md) it is `http://localhost:8080`. All routes
  start with `/v1/`.
- **An API key** in the header `Authorization: Bearer <key>`. Use a secret
  key from your server. See [API keys](api-keys.md).
- **`Content-Type: application/json`** on calls that send a body.

Create a test customer:

```bash
export VELOX=http://localhost:8080
export VELOX_KEY=vlx_secret_test_…   # your test key

curl -s -X POST "$VELOX/v1/customers" \
  -H "Authorization: Bearer $VELOX_KEY" \
  -H "Content-Type: application/json" \
  -d '{"external_id": "cust_123", "display_name": "Acme Inc"}'
```

**You see:** status `201` and the new customer as JSON, with an `id` such as
`vlx_cus_…`. The `external_id` is your own ID for the customer.

For a worked sequence of calls, see [first invoice](first-invoice.md#steps).

## The full reference

This page covers the routes most integrations use. Every route, field and
status is in the OpenAPI file (a standard, machine-readable API
description):

- On a running server: `http://localhost:8080/openapi.yaml`. Open it in any
  OpenAPI viewer, or import it into Postman or Insomnia.
- In the repository: [`api/openapi.yaml`](../api/openapi.yaml).

There is no official SDK (client library) yet. Call the API with any HTTP
client, or generate a client from the OpenAPI file.

## Lists

Every route that lists things pages the same way:

- Send `limit` (how many rows, 1 to 100, default 50) and `offset` (how
  many rows to skip, default 0).
- You get `{"data": [...], "total": n}`. `total` counts every row that
  matches your filters, not only this page.
- A `limit` outside 1 to 100, a negative `offset`, or a value that is not a
  whole number is a 422 with `param` naming it. It is never rounded to a
  valid value.

To read everything, add 100 to `offset` until `offset` reaches `total`:

```bash
curl -s "$VELOX/v1/plans?limit=100&offset=100" -H "Authorization: Bearer $VELOX_KEY"
```

One list counts differently: the audit log's `total` stops at 10,001, so a
value above 10,000 means "more than 10,000".

## Core routes

### Usage

| Method | Path | What it does |
|---|---|---|
| `POST` | `/v1/usage-events` | Send one usage event, with dimensions (labels) and a decimal value |
| `POST` | `/v1/usage-events/batch` | Send up to 1000 events in one call |
| `POST` | `/v1/usage-events/backfill` | Send one past event (`timestamp` required) |
| `GET` | `/v1/usage-events/held` | LiteLLM usage held because its customer or meter did not exist yet |
| `POST` | `/v1/usage-events/held/release` | Record held usage for a customer, once per idempotency key |
| `POST` | `/v1/usage-events/held/discard` | Mark held usage as never to be billed |
| `GET` | `/v1/customers/{id}/usage` | A customer's usage for the period, grouped by dimension |

Before you send usage, read [the retry rules](#sending-usage-safely).

### Pricing

| Method | Path | What it does |
|---|---|---|
| `POST` | `/v1/recipes/{key}/instantiate` | Create a ready-made pricing setup (meter, prices and plan) in one call |
| `POST` | `/v1/meters` | Create a meter (a counter for one kind of usage) |
| `POST` | `/v1/plans` | Create a plan, with its usage prices |
| `POST` | `/v1/plans/{id}/prices` | Add a price to a plan: a meter, the labels its events must carry, and a rate |
| `PATCH` | `/v1/plans/{id}/prices/{price_id}` | Change a price's rate |
| `GET` | `/v1/plans` | List plans, with their prices |

**A price belongs to a plan.** Two plans can price one meter differently.
When an event fits more than one price of the plan, the price with the most
labels takes it. On a plan no subscription has used, a price change applies
at once; on a used plan, from each subscriber's next billing period, and
from the join for a subscriber who first joins the plan in a period after
it.

**A price has one `mode`:**

- `per_unit` bills quantity × `unit_amount_cents`.
- `graduated` bills by `tiers`, each `{up_to, unit_amount_cents}`. A tier
  prices the units above the tier before it, up to its `up_to`. The last
  tier has `up_to: null` (no upper limit), and only the last.
- `package` bills ceil(quantity ÷ `package_size`) × `package_amount_cents`.
  A part package bills as a whole one.

A price takes only its mode's fields. A field the mode does not use must be
0 or left out, or the request is refused with 422.

**Units.** A field named `*_cents` is in the currency's smallest unit, such
as cents for USD. A price you send is a decimal string with at most 12
decimal places: `"0.0003"` is $3.00 per million units. An invoice line
rounds to whole cents once. Provider costs follow the same rule:
`unit_cost_cents` is US cents per token, and usage events and the margin
report give cost as `provider_cost_cents` and `cost_cents`.

### Customers and subscriptions

A subscription puts a customer on a plan.

| Method | Path | What it does |
|---|---|---|
| `POST` | `/v1/customers` | Create a customer |
| `POST` | `/v1/subscriptions` | Create a subscription. It starts now (or starts its trial now). Send `"status": "draft"` to create it without starting it. |
| `POST` | `/v1/subscriptions/{id}/activate` | Start a draft subscription now |
| `PATCH` | `/v1/subscriptions/{id}/items/{itemID}` | Change the plan or quantity. A change that takes effect now is prorated (charged or credited for the part of the period left). |
| `PUT` | `/v1/subscriptions/{id}/pause-collection` | Keep the billing cycle, but leave new invoices as drafts |
| `POST` | `/v1/subscriptions/{id}/extend-trial` | Move `trial_end_at` later |
| `POST` | `/v1/subscriptions/{id}/cancel` | Cancel now |
| `POST` | `/v1/subscriptions/{id}/retry-billing` | Clear a billing failure and bill now |
| `GET` | `/v1/subscriptions?billing=failing` | List subscriptions whose billing is failing |
| `GET` | `/v1/subscriptions?collection=paused` | List active and trialing subscriptions whose collection is paused, by you or by dunning |

Changing, pausing or canceling a subscription first bills any period that
has already ended, then applies the change. If that period cannot be billed,
the call returns `409` with code `unbilled_period`.

### Invoices and billing

| Method | Path | What it does |
|---|---|---|
| `GET` | `/v1/invoices` | List invoices |
| `GET` | `/v1/invoices/{id}/pdf` | Download an invoice as a PDF |
| `POST` | `/v1/billing/run` | Bill this account's subscriptions that are due now |
| `POST` | `/v1/invoices/create_preview` | Preview the next invoice without creating it |

`/v1/billing/run` skips subscriptions that are waiting after a failed
attempt, and ones set aside after too many failed attempts (`retry-billing`
brings them back).

### Credits and refunds

| Method | Path | What it does |
|---|---|---|
| `POST` | `/v1/credit-notes` | Issue a credit note: lower what a customer owes, as a balance credit or a refund |
| `POST` | `/v1/credits/grant` | Give a customer prepaid credit in one currency. It pays only invoices in that currency |
| `GET` | `/v1/credits/balance/{customer_id}` | The settled balance, one per currency: changes when an invoice, grant, adjustment or expiry is written |
| `GET` | `/v1/credits/balance/{customer_id}/live` | The balance you can spend now, per currency: settled, without expired grants, minus usage not yet invoiced. Poll it and cache it. |

To stop serving a customer when credit runs out, use the live balance. See
[Stop usage when credit runs out](stop-usage-when-credit-runs-out.md).

### Test clocks

See [test clock](glossary.md#testing-with-simulated-time).

| Method | Path | What it does |
|---|---|---|
| `POST` | `/v1/test-clocks` | Create a test clock |
| `POST` | `/v1/test-clocks/{id}/advance` | Move the clock forward, and bill what falls due |

### Payment retries, webhooks and the audit log

| Method | Path | What it does |
|---|---|---|
| `GET` | `/v1/dunning/runs` | List dunning runs (Velox retrying a failed payment and emailing the customer) |
| `POST` | `/v1/webhook-endpoints/endpoints` | Register a URL in your app that Velox calls when something happens. See [Webhooks](webhooks.md). |
| `GET` | `/v1/audit-log` | Search the audit log: who changed what, and when. Entries cannot be edited. |

## Sending usage safely

A response that is not `2xx` means nothing was recorded. Retry the same
request.

1. **Give every event an `idempotency_key`:** a unique ID for that one
   event. An event without one, or with an empty one, is refused with `422`
   and `param: idempotency_key`. In a batch, `errors[]` names each such
   event and nothing is stored. Without a key, Velox cannot tell a retry
   from new usage.
2. **Pick the key once, and reuse it on every retry.** A new key on each
   attempt makes each attempt a new event, so the usage is counted again.
3. **Keep it to 255 characters or fewer.** A longer key is refused with
   `422`.
4. **Never reuse a key for a different event.** Keys are unique per account
   and mode, forever. When a key was already used, Velox keeps the first
   event and drops the new one without comparing them. A single event gets
   `200` with the first event and `Idempotent-Replayed: true`; a batch
   counts it in `deduplicated`. So do not build keys from a batch index or a
   timestamp that can repeat, or that usage is lost.

Because a batch is stored all-or-nothing and each key is stored once,
resending events with the same keys never counts them twice. See
*idempotency key* in the [glossary](glossary.md#pricing-and-usage).

**The usage routes ignore the `Idempotency-Key` header.** The event key is
their only dedup key. A retried call runs again, and each event already
stored is answered as a replay (`200`, or counted in `deduplicated`).

**Send `quantity` on every event.** It is a JSON number (`5`) or a string
holding one (`"5.5"`, for exact decimals), with at most 12 decimal places,
a size under 10^26, and at most 100 characters. A negative value is a
correction. An event without one, or with any other
value, is refused with `422` and `param: quantity`. The event's
`dimensions` pick its price; `properties`, the old name, is refused when
it has a key (`null` or `{}` is ignored).

### What Velox found when the event arrived

Velox stores every valid event. The answer that wrote it can carry a
`billing_status`: what Velox found when the event arrived. Velox checked
the event against the customer's subscriptions, the plans and prices in
force, and the invoices already built at that moment. A status that is
present was true at that moment:

| `billing_status` | What Velox found | What to do |
|---|---|---|
| `after_close` | The event's time is already invoiced (see Late usage below). | Bill it with a one-off invoice if it should be charged. A price added to a plan does not bill it. |
| `not_on_plan` | A billing period not invoiced yet holds the event's time, but its plan has no price for the meter. | To bill later usage, add a price for the meter to the plan, or move the customer to a plan that has one. Check the invoice before you bill this event by hand. |
| `outside_subscription` | No active or trialing subscription of the customer bills the event's time. For example, there is none, or the time is before one starts, in a free trial, or after one ends or its scheduled cancel. | Check the event's customer and timestamp. |
| `no_price` | The plan that bills the event's time prices the meter, but no price of it takes the event's `dimensions`. A price takes an event whose dimensions include all of its `dimension_match`, compared as strings: the number `1` is not `"1"`. | Add a price for those dimensions to bill later usage. Check the invoice before you bill this event by hand. |

No `billing_status` means Velox found no reason then. It is not a promise
that the event will be billed.

- Velox checks only the customer's current billing periods. Time after a
  period end whose invoice is not built yet is not checked, nor is time
  after a trial's end before Velox ends the trial.
- A later change can change the outcome either way: a subscription
  created, activated or changed, a trial extended, a scheduled cancel
  undone, or a price added.
- The invoice is the final answer.

A present status can be wrong in two cases: a price change whose save
began before a billing period started and finished after the request read
the prices, and a subscription change saved while the request is in
flight. A LiteLLM delivery reads the subscriptions once, so the second
case covers the whole delivery.

Where it is sent:

- The single and backfill 201 carry it. `POST /v1/usage-events/batch`
  returns `results[i]` for each event, in the order sent: `status`
  (`ingested` or `deduplicated`) and `billing_status`, plus a count of
  each status.
- The LiteLLM answer counts each status, and the held-usage release counts
  `no_price` and `not_on_plan`. Their counts follow the same rule.
- A replay and a read never carry `billing_status`.

### Late usage

An event counts toward the billing period its `timestamp` falls in.

- Velox builds a period's invoice once `INVOICE_GRACE_PERIOD` (default 1
  hour) has passed after the period ends. An event that arrives within that
  time is on the invoice.
- After that, the invoice does not change. A later event for that period is
  stored, but it is not added to that invoice or to any later one. The
  answer says so: `billing_status: "after_close"` on the event's 201
  (single and backfill), and an `after_close` count on a batch or a
  LiteLLM delivery. A replay never carries it. What no `billing_status`
  means is [above](#what-velox-found-when-the-event-arrived).
- To bill it, make a one-off invoice: `POST /v1/invoices`, then
  `POST /v1/invoices/{id}/line-items`, then
  `POST /v1/invoices/{id}/finalize`.
- The same applies after a spend-threshold invoice (an invoice made early
  because spend crossed a limit). Usage of a sum or count meter stamped
  before it that arrives later is not on the period-end invoice either.
  If the spend-threshold invoice is voided before the period-end invoice
  is built, the period-end invoice bills all the period's usage, late
  events included. Writing it off does not do that: a written-off
  spend-threshold invoice keeps its usage billed. So the answer flags such
  an event only when the spend-threshold invoice is already paid: a paid
  invoice cannot be voided. Bill a flagged event the same way.
- These build the ended period's invoice at once, with no grace period:
  - an immediate cancel;
  - any other change to a subscription, through the API or the dashboard
    (an item change, a pause or resume of collection, a scheduled cancel or
    its removal);
  - `retry-billing`;
  - a spend-threshold invoice;
  - a test-clock advance.
- Dunning's final pause or cancel waits for the grace. When retries run out
  inside it, the run waits until the grace ends, then pauses or cancels.

The metric `velox_usage_unbilled_total{reason, origin}` counts the events
answered with each `billing_status` (`reason="after_close"` for late usage).
Usage on a canceled subscription is answered `outside_subscription`, not
`after_close`, or `not_on_plan` when another subscription's open period
holds that time without pricing the meter. An event before a
subscription's current period is `after_close` whatever plan it is on now:
adding a price to a plan never bills it. `velox_usage_late_event_total`
counts events, not sent through the backfill route, whose `timestamp` is
more than 24 hours older than their arrival. It does not say whether an
event missed an invoice.

## Errors

Errors return the same JSON body, including an unknown path and a method
the path does not take. Every response that reaches Velox also has a
`Velox-Request-Id` header. Send that ID to support when you report a
problem.

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "validation_error",
    "message": "external_id is required",
    "request_id": "req_<id>",
    "param": "external_id"
  }
}
```

`type`, `code`, `message` and `request_id` are always there. When `param`
is there, it names the field that failed. Read `code` in your code;
`message` is meant for people. Some endpoints put a more specific code in
`code` (for example `clock_advancing`, `not_payable`, `cooldown_active`) in
place of the default code shown below.

| Status | type | When |
|---|---|---|
| 400 | `invalid_request_error` | The request is malformed: the body is not valid JSON, or a required path or query parameter is missing or invalid (`invalid_request`). |
| 401 | `authentication_error` | The API key, session or public token is missing, invalid or expired. |
| 403 | `authentication_error` | You are signed in but not allowed to do this, or the request was blocked as cross-site. |
| 404 | `invalid_request_error` | The resource does not exist (`not_found`, or a specific code such as `invoice_not_found`), or the path does not exist (`route_not_found`). |
| 405 | `invalid_request_error` | The path exists but does not accept this method (`method_not_allowed`). The `Allow` header lists the methods it does accept. |
| 409 | `invalid_request_error` | The value already exists (`already_exists`), or the resource is in the wrong state for this action (`invalid_state` or a more specific code). |
| 413 | `invalid_request_error` | The usage batch (`batch_too_large`) or the Stripe webhook body (`payload_too_large`) is too large. |
| 422 | `invalid_request_error` | A field or business rule failed (`validation_error`, or a specific code). |
| 429 | `rate_limit_error` | Too many requests (`rate_limited`, or `cooldown_active` for a repeated setup-link email). Wait for the number of seconds in `Retry-After`. |
| 500 | `api_error` | Unexpected server error (`internal_error`). The details are logged on the server, not returned. A few operations return a specific code with instructions instead, for example `proration_failed`. |
| 502 | `api_error` | An upstream call failed. See the 502 codes below. |
| 503 | `api_error` | A dependency is down for a short time (`authentication_unavailable`, `ingest_unavailable`, `idempotency_unavailable`), so you can retry. `stripe_unavailable` and `email_unavailable` mean the feature is not set up on this server, and retrying will not help. |
| 504 | `api_error` | The request ran past its time limit before the server wrote any answer (`timeout`). |

The 502 codes:

- `provider_error`, `stripe_error`: the payment provider rejected the
  request.
- `payment_outcome_unknown`: the payment provider never answered. The
  charge may have gone through, so check before you retry.
- `email_enqueue_failed`: an email could not be queued.

A request body may hold only the fields its operation lists in the API
reference. Any other field is refused with 422, and `param` names it: a
misspelled field, a field the operation does not take, a field the
server sets, such as `id`, `tenant_id` or `created_at`, or a name in
another case, such as `Email` for `email`. Nothing is changed. A usage
batch names each such event in `errors[]` instead. An operation with no
request body, such as `POST /v1/subscriptions/{id}/cancel`, lists no
fields: send no body, `{}` or `null`. A body that is not one JSON value is refused with 400. The Stripe
and Postmark webhooks and the LiteLLM callback are the exceptions: they
take their sender's body as it comes.

Request bodies are limited to 1 MB, and Stripe webhook bodies to 64 KB. Only
the two routes in the 413 row return 413. Other routes do not promise a
specific status for a body that is too large.

<details>
<summary><strong>Less common error shapes</strong></summary>

- **Idempotency-Key errors** have `type` `invalid_request_error` and one of these codes:
  - `idempotency_error` (422): the key was used before with a different method, path or body. This is checked only after the first request has finished. While the first request is still running, a mismatched request gets `conflict_idempotency`.
  - `conflict_idempotency` (409): another request with this key is still running, or did not finish. Retry later.
  - `conflict_idempotency_unresolved` (409): an earlier request with this key started more than 5 minutes ago and never recorded an outcome. Check whether the resource exists before you retry with a new key.
  - `invalid_request` (400): the body could not be read. This includes any body over 1 MB.
- **A replayed key** (POST, PUT or PATCH under `/v1` with the same key, method, path and body) returns the first response's status and body, plus `Idempotent-Replayed: true`. The body's `request_id` is the first request's; the `Velox-Request-Id` header is the new request's. The response's other headers are not sent again. A first response of 409 or 422 is not stored: the key is freed, and a retry with the same key runs the request again. The usage routes (`/v1/usage-events…`, `/v1/integrations/litellm/spend`) ignore the header. If a database error stops the server from freeing the key or storing the first response, the key stays reserved: a retry gets `conflict_idempotency`, then `conflict_idempotency_unresolved` once 5 minutes have passed. The request does not run twice.
- **A plain-text error** comes only from the HTTP server itself, before Velox sees the request. A request too malformed to parse, such as bad syntax or headers that are too large, gets 400 or 431 with no `Velox-Request-Id` header.
- **Other JSON shapes:**
  - `GET /health/ready` returns 503 with `{"status", "checks"}`.
  - A rejected usage batch returns 422 with code `batch_rejected`. Next to `error`, the body also has `errors` (one entry per failing event), `ingested: 0` and `total`.
  - The Postmark webhook accepts a payload that is too large or is not valid JSON with 200 and `{"status": "skipped", "reason": "<reason>"}`.

</details>

## Routes you do not call yourself

The OpenAPI file lists these too, but your code does not need them:

- `GET /health/ready` and `/metrics`, for load balancers and Prometheus.
- The data behind the hosted invoice and payment-update pages
  (`/v1/public/invoices/*`, `/v1/public/payment-updates/*`). Share the page
  link instead.
- The inbound webhooks that Stripe and Postmark call
  (`/v1/webhooks/stripe/{endpoint_id}`, `/v1/webhooks/postmark`).

**Next:** let your app react when an invoice is paid or a payment fails: [Webhooks](webhooks.md).
