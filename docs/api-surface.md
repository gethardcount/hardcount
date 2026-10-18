# The Velox API

The Velox API is how your code talks to Velox: you create customers, send
usage, and read invoices with plain HTTP requests and JSON. Everything the dashboard does
goes through the same API.

**Who it is for:** developers connecting their product to Velox. New to
Velox? Start with [Send usage and get your first invoice](first-invoice.md),
then come back here for the details.

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
`vlx_cus_…` and `"status": "active"`. The `external_id` is your own ID for
the customer, such as the user ID in your database. Run it again with the
same `external_id` and you get `409` with code `already_exists`.

## The full reference

This page covers the routes most integrations use. Every route, field and
status is in the OpenAPI file (a standard, machine-readable API
description):

- On a running server: `http://localhost:8080/openapi.yaml`. Open it in any
  OpenAPI viewer, or import it into Postman or Insomnia.
- In the repository: [`api/openapi.yaml`](../api/openapi.yaml).

There is no official SDK (client library) yet. Call the API with any HTTP
client, or generate a client from the OpenAPI file.

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
| `POST` | `/v1/meters/{id}/pricing-rules` | Add a pricing rule that prices events by their dimensions |
| `GET` | `/v1/plans` | List plans |

### Customers and subscriptions

A subscription puts a customer on a plan.

| Method | Path | What it does |
|---|---|---|
| `POST` | `/v1/customers` | Create a customer |
| `POST` | `/v1/subscriptions` | Create a subscription |
| `PATCH` | `/v1/subscriptions/{id}/items/{itemID}` | Change the plan or quantity. A change that takes effect now is prorated (charged or credited for the part of the period left). |
| `PUT` | `/v1/subscriptions/{id}/pause-collection` | Keep the billing cycle, but leave new invoices as drafts |
| `POST` | `/v1/subscriptions/{id}/extend-trial` | Move `trial_end_at` later |
| `POST` | `/v1/subscriptions/{id}/cancel` | Cancel now |
| `POST` | `/v1/subscriptions/{id}/retry-billing` | Clear a billing failure and bill now |
| `GET` | `/v1/subscriptions?billing=failing` | List subscriptions whose billing is failing |

Changing, pausing or canceling a subscription first bills any period that
has already ended, then applies the change. If that period cannot be billed,
the call returns `409` with code `unbilled_period`.

### Invoices and billing

| Method | Path | What it does |
|---|---|---|
| `GET` | `/v1/invoices` | List invoices |
| `GET` | `/v1/invoices/{id}/pdf` | Download an invoice as a PDF |
| `POST` | `/v1/billing/run` | Bill this account's subscriptions that are due now |
| `GET` | `/v1/billing/preview/{sub_id}` | Preview the next invoice without creating it |

A period is due `INVOICE_GRACE_PERIOD` after it ends. `/v1/billing/run`
skips subscriptions that are waiting after a failed attempt, and ones set
aside after too many failed attempts (`retry-billing` brings them back).

### Credits and refunds

| Method | Path | What it does |
|---|---|---|
| `POST` | `/v1/credit-notes` | Issue a credit note: lower what a customer owes, as a balance credit or a refund |
| `POST` | `/v1/credits/grant` | Give a customer prepaid credit |
| `GET` | `/v1/credits/balance/{customer_id}` | The settled balance: changes when an invoice, grant, adjustment or expiry is written |
| `GET` | `/v1/credits/balance/{customer_id}/live` | The balance you can spend now: settled, without expired grants, minus usage not yet invoiced. Poll it and cache it. |

To stop serving a customer when credit runs out, use the live balance. See
[Stop usage when credit runs out](stop-usage-when-credit-runs-out.md).

### Test clocks

A test clock is a pretend clock for test customers, so a month of billing
takes seconds.

| Method | Path | What it does |
|---|---|---|
| `POST` | `/v1/test-clocks` | Create a test clock |
| `POST` | `/v1/test-clocks/{id}/advance` | Move the clock forward, and bill what falls due |

### Payment retries, webhooks and the audit log

| Method | Path | What it does |
|---|---|---|
| `GET` | `/v1/dunning/runs` | List dunning runs (Velox retrying a failed payment and emailing the customer) |
| `POST` | `/v1/webhook-endpoints/endpoints` | Register a URL in your app that Velox calls when something happens. See [Webhooks](webhooks.md). |
| `GET` | `/v1/audit-log` | Search the audit log, a record of every change that cannot be edited |

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

**The `Idempotency-Key` header is something else.** It is separate from
the event key and does not replace it. It saves one HTTP response for 24
hours, so a retried call gets the same answer. The event key stops an event
from being counted twice, forever. On the usage routes, Velox does not save
a `5xx` response under the header's key, so a retry with the same key runs
again. On other routes, a `5xx` response is saved and replayed on purpose.

### Late usage

An event counts toward the billing period its `timestamp` falls in.

- Velox builds a period's invoice once `INVOICE_GRACE_PERIOD` (default 1
  hour) has passed after the period ends. An event that arrives within that
  time is on the invoice.
- After that, the invoice does not change. A later event for that period is
  stored, but it is not added to that invoice or to any later one, and
  Velox does not alert you.
- To bill it, make a one-off invoice: `POST /v1/invoices`, then
  `POST /v1/invoices/{id}/line-items`, then
  `POST /v1/invoices/{id}/finalize`.
- The same applies after a spend-threshold invoice (an invoice made early
  because spend crossed a limit). Usage stamped before it that arrives later
  is not on the period-end invoice either. Bill it the same way.
- An immediate cancel, `retry-billing`, a spend-threshold invoice and a
  test-clock advance build their invoice at once, with no grace period.

The metric `velox_usage_late_event_total` counts events, not sent through
the backfill route, whose `timestamp` is more than 24 hours older than their
arrival. It does not say whether an event missed an invoice.

## Errors

Most errors return the same JSON body. Every response that reaches Velox
also has a `Velox-Request-Id` header. Send that ID to support when you
report a problem.

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
| 404 | `invalid_request_error` | The resource does not exist (`not_found`, or a specific code such as `invoice_not_found`). |
| 405 | (empty body) | The path exists but does not accept this method. The `Allow` header lists the methods it does accept. |
| 409 | `invalid_request_error` | The value already exists (`already_exists`), or the resource is in the wrong state for this action (`invalid_state` or a more specific code). |
| 413 | `invalid_request_error` | The usage batch (`batch_too_large`, only when the request has no `Idempotency-Key`) or the Stripe webhook body (`payload_too_large`) is too large. |
| 422 | `invalid_request_error` | A field or business rule failed (`validation_error`, or a specific code). |
| 429 | `rate_limit_error` | Too many requests (`rate_limited`, or `cooldown_active` for a repeated setup-link email). Wait for the number of seconds in `Retry-After`. |
| 500 | `api_error` | Unexpected server error (`internal_error`). The details are logged on the server, not returned. A few operations return a specific code with instructions instead, for example `proration_failed`. |
| 502 | `api_error` | An upstream call failed. See the 502 codes below. |
| 503 | `api_error` | A dependency is down for a short time (`authentication_unavailable`, `ingest_unavailable`), so you can retry. `stripe_unavailable` and `email_unavailable` mean the feature is not set up on this server, and retrying will not help. |

The 502 codes:

- `provider_error`, `stripe_error`: the payment provider rejected the
  request.
- `payment_outcome_unknown`: the payment provider never answered. The
  charge may have gone through, so check before you retry.
- `email_enqueue_failed`: an email could not be queued.

Request bodies are limited to 1 MB, and Stripe webhook bodies to 64 KB. Only
the two routes in the 413 row return 413. Other routes do not promise a
specific status for a body that is too large. An oversized usage batch sent
with an `Idempotency-Key` header gets `400` with `type` `bad_request` in the
smaller body described below, not 413.

<details>
<summary><strong>Less common error shapes</strong></summary>

- **Idempotency-Key errors** return a smaller body, `{"error": {"type", "message"}}`. It has no `code` and no `request_id` in the body; the header still has the ID. Here `type` is the reason:
  - `idempotency_error` (422): the key was used before with a different method, path or body. This is checked only after the first request has finished. While the first request is still running, a mismatched request gets `conflict_idempotency`.
  - `conflict_idempotency` (409): another request with this key is still running, or did not finish. Retry later.
  - `conflict_idempotency_unresolved` (409): an earlier request with this key started more than 5 minutes ago and never recorded an outcome. Check whether the resource exists before you retry with a new key.
  - `bad_request` (400): the body could not be read. This includes any body over 1 MB, so an oversized usage batch sent with an Idempotency-Key gets this 400 instead of the 413 `batch_too_large`.
- **A replayed key** (POST, PUT or PATCH under `/v1` with the same key, method, path and body) returns the first response's status and body, plus `Idempotent-Replayed: true`. The body's `request_id` is the first request's; the `Velox-Request-Id` header is the new request's. The response's other headers are not sent again. A first response of 409 or 422 is not stored, and neither is a 5xx on the usage-ingest routes (`/v1/usage-events`, `/v1/integrations/litellm`). In those cases the key is freed, and a retry with the same key runs the request again. If a database error stops the server from freeing the key or storing the first response, the key stays reserved: a retry gets `conflict_idempotency`, then `conflict_idempotency_unresolved` once 5 minutes have passed. The request does not run twice.
- **Plain-text or empty bodies (not JSON)** come from these cases:
  - an unknown path (`404 page not found`);
  - a method the path does not accept (405, empty body);
  - a server panic (500, empty body);
  - a request that hits its time limit before the server has written any response (504, empty body);
  - `GET /v1/webhook_events/stream` when it returns 500;
  - `/metrics` when it returns 401;
  - a request too malformed for the server to parse, such as bad syntax or headers that are too large (400 or 431 from the HTTP server itself, with no `Velox-Request-Id` header).
- **Other JSON shapes:**
  - `GET /health/ready` returns 503 with `{"status", "checks"}`.
  - `GET /v1/usage-summary/{customer_id}` returns 500 with `{"error": "internal_error"}`.
  - A rejected usage batch returns 422 with code `batch_rejected`. Its `error` object has no `request_id`, and the body also has `errors` (one entry per failing event), `ingested: 0` and `total`.
  - `GET /v1/auth/password-reset/check` returns 422 with `{"valid": false, "reason": "invalid_or_expired"}`.
  - The Postmark webhook returns `{"error": "<reason>"}` with 401 or 500. A payload that is too large or is not valid JSON is accepted with 200 and `{"status": "skipped", "reason": "<reason>"}`.

</details>

## Routes you do not call yourself

The OpenAPI file lists these too, but your code does not need them:

- `GET /health/ready` and `/metrics`, for load balancers and Prometheus.
- The data behind the hosted invoice and payment-update pages
  (`/v1/public/invoices/*`, `/v1/public/payment-updates/*`). Share the page
  link instead.
- The inbound webhooks that Stripe and Postmark call
  (`/v1/webhooks/stripe/{endpoint_id}`, `/v1/webhooks/postmark`).
- Older routes the dashboard no longer uses: `/v1/checkout/setup`,
  `/v1/checkout/status/{customerID}`, and
  `/v1/webhook-endpoints/events/{id}/deliveries` and `/replay` (use
  `/v1/webhook_events/{id}/...`).
