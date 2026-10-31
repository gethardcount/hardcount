# The Hardcount API

The Hardcount API is how your code talks to Hardcount: you create customers, send
usage, and read invoices with plain HTTP requests and JSON. Everything the dashboard does
goes through the same API.

New to Hardcount? Start with [Send usage and get your first invoice](first-invoice.md).

## Make your first call

Every call needs three things:

- **The address** of your Hardcount server. After the
  [quick start](quick-start.md) it is `http://localhost:8080`. All routes
  start with `/v1/`.
- **An API key** in the header `Authorization: Bearer <key>`. Use a secret
  key from your server. A gateway that only sends usage, such as LiteLLM,
  gets a usage-only key. See [API keys](api-keys.md#key-types).
- **`Content-Type: application/json`** on calls that send a body.

Send an `Idempotency-Key` header on every `POST`, `PUT` and `PATCH` you may
retry. It is a unique ID, such as a UUID: make it once, and send it again on
each retry, so a retry cannot do the work twice. Usage events carry their own
key instead. See [Idempotency](api/idempotency.md).

Create a test customer:

```bash
export HARDCOUNT=http://localhost:8080
export HARDCOUNT_KEY=hc_secret_test_…   # your test key

curl -s -X POST "$HARDCOUNT/v1/customers" \
  -H "Authorization: Bearer $HARDCOUNT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"external_id": "cust_123", "display_name": "Acme Inc"}'
```

**You see:** status `201` and the new customer as JSON, with an `id` such as
`hc_cus_…`. The `external_id` is your own ID for the customer.

For a worked sequence of calls, see [first invoice](first-invoice.md#steps).
The same two calls in curl, Node, Python and Go, with retries:
[Send usage and read a balance](api/two-calls.md).

## The full reference

This page covers the routes most integrations use. Every route, field and
status is in the OpenAPI file (a standard, machine-readable API
description):

- On a running install, the dashboard serves it:
  `http://localhost/openapi.yaml` with the Compose stack, or
  `http://localhost:5173/openapi.yaml` under `make dev`. The API itself, on
  `:8080`, does not. Open it in any OpenAPI viewer, or import it into
  Postman or Insomnia.
- In the repository: [`api/openapi.yaml`](../api/openapi.yaml).

There is no official SDK (client library) yet. Call the API with any HTTP
client, or generate a client from the OpenAPI file.

The rules every route shares have their own pages:

| Page | What it covers |
|---|---|
| [Authentication and keys](api/authentication.md) | The header, key types and modes, and the answers when a key is refused |
| [Errors](api/errors.md) | The error body, and every status and code with what to do |
| [Idempotency](api/idempotency.md) | Retrying a request without doing it twice |
| [Rate limits](api/rate-limits.md) | How many requests a minute, the headers, and the `429` |
| [Pagination](api/pagination.md) | `limit`, `offset` and `total` on list routes |
| [Timing and limits](api/timing-and-limits.md) | Every fixed time and size, such as when invoices are made |
| [Send usage and read a balance](api/two-calls.md) | Two calls in curl, Node, Python and Go |

The OpenAPI file's `info.version` is a date, not an API version you can pick:
see [Versioning](versioning.md#the-api-version).

## Lists

Every list route takes `limit` (1 to 100, default 50) and `offset`, and
answers `{"data": [...], "total": n}`. There is no cursor. The few lists that
stop counting or answer in one piece: [Pagination](api/pagination.md).

## Rate limits

By default, each API key may send 1,000 requests a minute, and 1,000 usage
requests a second. A request over the limit gets `429` and a `Retry-After`
header with the seconds to wait. Every limit and header:
[Rate limits](api/rate-limits.md).

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

One usage event:

```bash
curl -s -X POST "$HARDCOUNT/v1/usage-events" \
  -H "Authorization: Bearer $HARDCOUNT_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "event_name": "tokens",
    "external_customer_id": "acme-1",
    "quantity": 1000000,
    "idempotency_key": "acme-1-call-8812:input",
    "dimensions": {"model": "claude-sonnet-4.5", "token_type": "input"}
  }'
```

- `event_name` picks the meter that counts the event.
- `external_customer_id` is your own ID for the customer, its `external_id`.
- `idempotency_key` names this one event; see
  [the retry rules](#sending-usage-safely).
- `dimensions` are labels; the plan's prices pick events by them.
- `timestamp` is optional: an RFC 3339 time, such as
  `2026-10-31T12:00:00Z`. Without it, the event gets the customer's current
  time.

**You see:** status `201` and the stored event, with an `id` such as
`hc_evt_…`. The meter and the customer must exist first, or the event is
refused with `422`.

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
| `GET` | `/v1/customers/{id}` | Read a customer |
| `GET` | `/v1/customers?external_id=…` | Find a customer by your own ID |
| `POST` | `/v1/subscriptions` | Create a subscription. It starts now (or starts its trial now). Send `"status": "draft"` to create it without starting it. |
| `GET` | `/v1/subscriptions/{id}` | Read a subscription, with its items |
| `POST` | `/v1/subscriptions/{id}/activate` | Start a draft subscription now |
| `POST` | `/v1/subscriptions/{id}/items` | Add a plan to the subscription |
| `PATCH` | `/v1/subscriptions/{id}/items/{itemID}` | Change the plan or quantity. A change that takes effect now is prorated (charged or credited for the part of the period left). |
| `DELETE` | `/v1/subscriptions/{id}/items/{itemID}` | Remove a plan from the subscription |
| `PUT` | `/v1/subscriptions/{id}/pause-collection` | Keep the billing cycle, but leave new invoices as drafts |
| `DELETE` | `/v1/subscriptions/{id}/pause-collection` | Resume collection |
| `POST` | `/v1/subscriptions/{id}/extend-trial` | Move `trial_end_at` later |
| `POST` | `/v1/subscriptions/{id}/end-trial` | End the trial now |
| `POST` | `/v1/subscriptions/{id}/schedule-cancel` | Cancel later: send `{"at_period_end": true}`, or a `cancel_at` time |
| `DELETE` | `/v1/subscriptions/{id}/scheduled-cancel` | Remove a scheduled cancel |
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
| `GET` | `/v1/invoices/{id}` | Read an invoice (under `invoice`) and its `line_items` |
| `GET` | `/v1/invoices/{id}/pdf` | Download an invoice as a PDF |
| `POST` | `/v1/billing/run` | Bill this account's subscriptions that are due now |
| `POST` | `/v1/invoices/create_preview` | Preview the next invoice without creating it |

`/v1/billing/run` bills what the hourly run would: periods whose
`INVOICE_GRACE_PERIOD` has passed. It skips subscriptions on a test clock
(advancing the clock bills those), ones waiting after a failed attempt, and
ones set aside after too many failed attempts (`retry-billing` brings them
back). It answers 206 when some subscriptions could not be billed.

### Credits and refunds

| Method | Path | What it does |
|---|---|---|
| `POST` | `/v1/credit-notes` | Create a credit note that lowers what a customer owes, as a balance credit or a refund. It is a draft until you issue it (`POST /v1/credit-notes/{id}/issue`); send `auto_issue: true` to issue it at once |
| `POST` | `/v1/credits/grant` | Give a customer prepaid credit in one currency. It pays only invoices in that currency |
| `GET` | `/v1/credits/balance/{customer_id}` | The settled balance, one per currency: changes when an invoice, grant, adjustment or expiry is written |
| `GET` | `/v1/credits/balance/{customer_id}/live` | The balance you can spend now, per currency: settled, without expired grants, minus usage not yet invoiced. Poll it and cache it. |

To stop serving a customer when credit runs out, use the live balance. See
[Stop usage when credit runs out](stop-usage-when-credit-runs-out.md).

### Test clocks

A test clock is a pretend clock for test customers: you move it forward to
see a month of billing in seconds. See
[test clock](glossary.md#testing-with-simulated-time). Test clocks work only
with a test key.

| Method | Path | What it does |
|---|---|---|
| `POST` | `/v1/test-clocks` | Create a test clock, with a `name` and its starting `frozen_time` |
| `GET` | `/v1/test-clocks/{id}` | Read the clock, with its `status` |
| `POST` | `/v1/test-clocks/{id}/advance` | Move the clock forward to `frozen_time`, at most 1 year at a time, and bill what falls due |

1. Put a customer on the clock: send its ID as `test_clock_id` when you
   create the customer, with `POST /v1/customers`. Its subscriptions and
   usage then follow the clock's time.
2. Move the clock:

   ```bash
   curl -s -X POST "$HARDCOUNT/v1/test-clocks/$CLOCK_ID/advance" \
     -H "Authorization: Bearer $HARDCOUNT_KEY" \
     -H "Content-Type: application/json" \
     -d '{"frozen_time": "2026-12-01T00:00:00Z"}' | jq -r .status
   ```

   The call answers at once, while Hardcount bills in the background.
   **You see:**

   ```text
   advancing
   ```

3. Read the clock with `GET /v1/test-clocks/{id}` every second or two until
   its `status` is `ready`. Then the invoices are built. A `status` of
   `internal_failure` means the billing stopped;
   `POST /v1/test-clocks/{id}/retry-advance` tries again.

[First invoice](first-invoice.md#steps) walks through this end to end.

### Payment retries, webhooks and the audit log

| Method | Path | What it does |
|---|---|---|
| `GET` | `/v1/dunning/runs` | List dunning runs (Hardcount retrying a failed payment and emailing the customer) |
| `POST` | `/v1/webhook-endpoints/endpoints` | Register a URL in your app that Hardcount calls when something happens. See [Webhooks](webhooks.md). |
| `GET` | `/v1/audit-log` | Search the audit log: who changed what, and when. Hardcount cannot edit or delete entries. |

## Sending usage safely

A response that is not `2xx` means this request recorded nothing. Retry the
same request, with the same events.

Give every event its own `idempotency_key`. Pick it once, and send the same
key on every retry of that event. Hardcount stores one event per key, forever,
so a retry is never counted twice. An event without a key is refused with
`422`. The usage routes ignore the `Idempotency-Key` header. The full rules,
and what a repeated key answers:
[Idempotency](api/idempotency.md#usage-events-the-idempotency_key-field).

**Send `quantity` on every event.** It is a JSON number (`5`) or a string
holding one (`"5.5"`, for exact decimals), with at most 12 decimal places,
a size under 10^26, and at most 100 characters. A negative value is a
correction. An event without one, or with any other
value, is refused with `422` and `param: quantity`. The event's
`dimensions` pick its price; `properties`, the old name, is refused when
it has a key (`null` or `{}` is ignored).

### What Hardcount found when the event arrived

Hardcount stores every valid event. The answer that wrote it can carry a
`billing_status`: what Hardcount found when the event arrived. Hardcount checked
the event against the customer's subscriptions, the plans and prices in
force, and the invoices already built at that moment. A status that is
present was true at that moment:

| `billing_status` | What Hardcount found | What to do |
|---|---|---|
| `after_close` | The event's time is already invoiced (see Late usage below). | Bill it with a one-off invoice if it should be charged. A price added to a plan does not bill it. |
| `not_on_plan` | A billing period not invoiced yet holds the event's time, but its plan has no price for the meter. | To bill later usage, add a price for the meter to the plan, or move the customer to a plan that has one. Check the invoice before you bill this event by hand. |
| `outside_subscription` | No active or trialing subscription of the customer bills the event's time. For example, there is none, or the time is before one starts, in a free trial, or after one ends or its scheduled cancel. | Check the event's customer and timestamp. |
| `no_price` | The plan that bills the event's time prices the meter, but no price of it takes the event's `dimensions`. A price takes an event whose dimensions include all of its `dimension_match`, compared as strings: the number `1` is not `"1"`. | Add a price for those dimensions to bill later usage. Check the invoice before you bill this event by hand. |

No `billing_status` means Hardcount found no reason then. It is not a promise
that the event will be billed.

- Hardcount checks only the customer's current billing periods. Time after a
  period end whose invoice is not built yet is not checked, nor is time
  after a trial's end before Hardcount ends the trial.
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

- Hardcount builds a period's invoice on the first scheduled billing run
  after `INVOICE_GRACE_PERIOD` (default 1 hour) has passed since the period
  ended. The run starts every hour, so the invoice appears 1 to 2 hours after
  the period ends ([timing](api/timing-and-limits.md#when-invoices-are-made)).
  An event that arrives before the invoice is built is on it.
- After that, the invoice does not change. A later event for that period is
  stored, but it is not added to that invoice or to any later one. The
  answer says so: `billing_status: "after_close"` on the event's 201
  (single and backfill), and an `after_close` count on a batch or a
  LiteLLM delivery. A replay never carries it. What no `billing_status`
  means is [above](#what-hardcount-found-when-the-event-arrived).
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
- These build the ended period's invoice at once, with no grace period,
  whether they come through the API or the dashboard. Setting or clearing
  spend thresholds does not.
  - an immediate cancel;
  - an item added, changed or removed, or a pending item change canceled;
  - a pause or resume of collection;
  - a scheduled cancel, or its removal;
  - `retry-billing`;
  - a spend-threshold invoice;
  - a test-clock advance.
- Dunning's final pause or cancel waits for the grace. When retries run out
  inside it, the run waits until the grace ends, then pauses or cancels.

The metric `hardcount_usage_unbilled_total{reason, origin}` counts the events
answered with each `billing_status` (`reason="after_close"` for late usage).
Usage on a canceled subscription is answered `outside_subscription`, not
`after_close`, or `not_on_plan` when another subscription's open period
holds that time without pricing the meter. An event before a
subscription's current period is `after_close` whatever plan it is on now:
adding a price to a plan never bills it. `hardcount_usage_late_event_total`
counts events, not sent through the backfill route, whose `timestamp` is
more than 24 hours older than their arrival. It does not say whether an
event missed an invoice.

## Errors

An error answers with a status and a JSON body,
`{"error": {"type", "code", "message", "request_id", "param"}}`. Read `code`
in your code; `message` is for people. Every answer has a
`Hardcount-Request-Id` header: look it up in your Hardcount server's logs to
see what happened to the request. Every status and code, and what to do about
each: [Errors](api/errors.md).

## Routes you do not call yourself

The OpenAPI file lists these too, but your code does not need them:

- `GET /health`, `GET /health/ready` and `/metrics`, for load balancers
  and Prometheus.
- The data behind the hosted invoice page, the payment-update page and the
  public cost dashboard (`/v1/public/invoices/*`,
  `/v1/public/payment-updates/*`, `/v1/public/cost-dashboard/*`). Share the
  page link instead.
- The inbound webhooks that Stripe and Postmark call
  (`/v1/webhooks/stripe/{endpoint_id}`, `/v1/webhooks/postmark`).

**Next:** let your app react when an invoice is paid or a payment fails: [Webhooks](webhooks.md).
