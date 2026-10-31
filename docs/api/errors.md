# Errors

When a request fails, Hardcount answers with an HTTP status and a JSON body
that names the problem with a `code`. This page shows the body, what each
status means, and every code the API sends, with what to do about it.

## The error body

Every error has this shape, including an unknown path and a method the path
does not take:

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

| Field | Always there | What it is |
|---|---|---|
| `type` | Yes | The kind of error; see [Statuses](#statuses) |
| `code` | Yes | A stable name for the problem. Branch on it in your code. |
| `message` | Yes | A sentence for people. Do not parse it: the words can change. |
| `request_id` | Yes | The request's ID; see [Find a request in your logs](#find-a-request-in-your-logs) |
| `param` | No | The field or query parameter that failed, when one did |

A few answers add fields beside `error`; see
[Answers in another shape](#answers-in-another-shape).

## Statuses

| Status | `type` | Meaning | Retry? |
|---|---|---|---|
| `400` | `invalid_request_error` | The request cannot be read: the body is not JSON, or a field has the wrong JSON type | No. Fix the request. |
| `401` | `authentication_error` | No key, or a wrong, revoked or expired one | No. Fix the key. |
| `403` | `authentication_error` | The key or session may not do this | No |
| `404` | `invalid_request_error` | The resource or the path does not exist | No |
| `405` | `invalid_request_error` | The path does not take this method. The `Allow` header lists the ones it takes. | No |
| `409` | `invalid_request_error` | It conflicts with what exists: a duplicate, or the resource is in the wrong state for this action | Sometimes; see the code |
| `413` | `invalid_request_error` | The body is too large | No. Send less. |
| `422` | `invalid_request_error` | A field or a business rule failed | No. Fix the request. |
| `429` | `rate_limit_error` | Too many requests | Yes, after `Retry-After` seconds |
| `500` | `api_error` | Something went wrong on the server | See [Retry after an error](#retry-after-an-error) |
| `502` | `api_error` | Stripe or the email server refused or did not answer | See the code |
| `503` | `api_error` | A part of Hardcount was not available for a moment | Yes |
| `504` | `api_error` | The request ran past its time limit before any answer was written | See [Retry after an error](#retry-after-an-error) |

## Every code

The status sets the default code. A more specific code replaces it where
the API has one.

### 400 Bad Request

| `code` | When | What to do |
|---|---|---|
| `invalid_request` | The body is not one JSON value, or a field has the wrong JSON type (`invalid JSON body`). The body is over 1 MB. A usage batch has no events or more than 1,000. A filter that takes a fixed set of values got another, such as `status` on the subscription list. | Fix the request |

### 401 Unauthorized

| `code` | When | What to do |
|---|---|---|
| `unauthorized` | No key or session, or a wrong, revoked or expired one. Also a wrong sign-in, and a bad public cost-dashboard link. | Fix the key. The messages: [When a key is refused](authentication.md#when-a-key-is-refused). |
| `invalid_token` | A payment-update link that is unknown, expired or already used | Ask for a new link |

### 403 Forbidden

| `code` | When | What to do |
|---|---|---|
| `forbidden` | The key's type may not call this route. A route that takes only a dashboard session. A browser request from another site. A test clock used with a live key. | Use the right key; see [Authentication](authentication.md) |

### 404 Not Found

| `code` | When | What to do |
|---|---|---|
| `not_found` | The resource, or one the body names, does not exist in this account and mode. A bad hosted invoice link gets it too. | Check the ID and the key's mode |
| `route_not_found` | No route has this path | Check the URL |
| `invoice_not_found` | The invoice behind a payment-update link was removed | Ask for a new link |

### 405 Method Not Allowed

| `code` | When | What to do |
|---|---|---|
| `method_not_allowed` | The path does not take this method | Use a method from the `Allow` header |

### 409 Conflict

Most of these say the resource is in the wrong state now. Read it again, then
decide.

**Duplicates and state**

| `code` | When | What to do |
|---|---|---|
| `already_exists` | A value that must be unique is taken, such as a customer's `external_id`. `param` names it. | Use another value, or read the one that exists |
| `invalid_state` | The resource is in the wrong state for this action, such as advancing a test clock that is still advancing | Read it, wait or change it, then try again |
| `already_bootstrapped` | `POST /v1/bootstrap` after the first account exists | Do not retry |

**Idempotency** ([more](idempotency.md))

| `code` | When | What to do |
|---|---|---|
| `conflict_idempotency` | A request with this `Idempotency-Key` is still running or did not finish | Wait a few seconds, then retry with the same key |
| `conflict_idempotency_unresolved` | A request with this key started more than 5 minutes ago and never recorded an answer | Check whether the change happened; retry with a new key only if not |

**Pricing**

| `code` | When | What to do |
|---|---|---|
| `price_exists` | The plan already prices this meter with the same `dimension_match` | Change that price instead |
| `price_conflict` | Another price on the plan and meter would take the same events with the same number of labels | Make one price more specific |
| `meter_overlap` | A subscriber of the plan already has this meter on another subscription | Price the meter on a new plan |
| `meter_has_usage` | You tried to change the aggregation of a meter that has events | Make a new meter |

**Subscriptions**

| `code` | When | What to do |
|---|---|---|
| `unbilled_period` | A billing period has ended and could not be invoiced, so the change waits | Fix the billing failure (`POST /v1/subscriptions/{id}/retry-billing`), then try again |
| `unpaid_invoice_blocks_change` | The current period's invoice is unpaid | Collect, void or write off that invoice first |
| `funding_invoice_tax_pending` | The tax of an invoice that buys prepaid credit is not worked out yet | Try again later |
| `item_changed` | The subscription's items changed while the request ran | Read it again, then retry |
| `subscription_period_moved` | The subscription moved to a new billing period while the request ran, or a spend-threshold invoice already billed past now | Read it again, then retry, or schedule the change for the next period |
| `billing_not_failing` | `retry-billing` on a subscription whose billing is not failing | Nothing to do |

**Invoices and payments**

| `code` | When | What to do |
|---|---|---|
| `payment_in_flight` | A card charge for the invoice may be in progress, so a void, write-off, offline payment, credit note, collect or email would race it | Wait a few minutes, then retry |
| `payment_unidentifiable` | Stripe's answer for a charge was unclear. Hardcount checks with Stripe within a day. Only a write-off is allowed until then. | Wait, or mark the invoice uncollectible |
| `payment_in_progress` | A payment for this invoice is already in progress on the hosted page | Wait |
| `not_payable` | The invoice cannot be paid now, for example it is paid or voided | Do not retry |
| `amount_below_minimum` | The amount is below Stripe's smallest card charge | Void it, record an offline payment, or bill it later |
| `card_refused` | The bank refused the saved card for good | Ask the customer for a new card |
| `invoice_not_collectible` | Resend a payment link on an invoice that is not unpaid or written off | Do not retry |
| `invoice_not_sendable` | Email a draft or voided invoice | Finalize it first, or do not send it |
| `no_email_on_file` | Resend a payment link to a customer with no email | Add an email to the customer |
| `invoice_changed`, `invoice_lines_changed` | The invoice changed while the request ran | Read it again, then retry |
| `credit_note_exceeds_invoice`, `invoice_not_open_receivable` | A credit note no longer fits the invoice, which changed while the request ran | Read the invoice, then retry with the right amount |
| `customer_currency_conflict` | A one-off invoice's currency is not the customer's billing currency | Use the customer's currency |
| `tax_calculation_expired` | Stripe Tax's calculation for the invoice is too old to finalize | Retry the tax, then finalize |
| `stripe_refund_unresolved` | A refund made in Stripe for this invoice is not reviewed yet, or is still in progress | Record or dismiss it on the invoice, then retry |
| `credit_block_changed` | The customer's credit changed while it was being returned | Retry |

**Setup on this server**

| `code` | When | What to do |
|---|---|---|
| `email_not_configured` | An action that sends email, on a server with no email set up (`SMTP_HOST`). Nothing was sent. | The operator sets up email: [Email setup](../ops/email-setup.md) |
| `stripe_not_connected` | An action that needs Stripe, in a mode with no Stripe keys | Connect Stripe under **Settings** |
| `online_payment_unavailable` | The hosted invoice or payment-update page cannot take a card: the seller has not connected Stripe for this mode | The customer pays another way; do not retry |

**Rare: a record that must be unique already exists**

These guard against a record being written twice, mostly by Hardcount's own
billing run. A request rarely gets one. Do not retry; read the invoice or
the credit ledger, and report it if the state looks wrong:
`invoice_number_taken`, `invoice_billing_period_taken`,
`invoice_threshold_cycle_taken`, `invoice_public_token_collision`,
`invoice_unique_violation`, `credit_note_source_taken`,
`credit_reversal_source_taken`, `credit_proration_source_taken`,
`credit_commit_fund_taken`, `credit_unique_violation` and
`credit_source_amount_mismatch`.

### 413 Payload Too Large

| `code` | When | What to do |
|---|---|---|
| `batch_too_large` | A `POST /v1/usage-events/batch` body is over 1 MB | Send smaller batches |
| `payload_too_large` | A Stripe webhook body is over 64 KB | Nothing; Stripe's events are smaller |

### 422 Unprocessable Entity

| `code` | When | What to do |
|---|---|---|
| `validation_error` | A field failed a rule, such as a missing `external_id`, a bad `limit`, or a field the operation does not take. `param` names it. | Fix the field |
| `batch_rejected` | At least one event of a usage batch failed. Nothing was stored. The body lists each failure in `errors`. | Fix the events named, then send the whole batch again |
| `idempotency_error` | An `Idempotency-Key` was used before with a different method, path or body | Use a new key for a new request |
| `customer_has_no_subscription` | An invoice preview or a usage summary for a customer with no active subscription | Send `subscription_id`, or a period with `from` and `to` |
| `subscription_has_no_cycle` | An invoice preview for a subscription with no current billing period | Send a period |
| `missing_email` | Email a payment-method link to a customer with no email | Add an email |
| `note_too_long` | That email's note is over 2,000 characters | Shorten it |
| `invalid_state` | Rotate the public link of a draft invoice | Finalize the invoice first |
| `invalid_or_expired` | A password-reset link that is unknown, expired or used | Ask for a new link |

### 429 Too Many Requests

Each sends a `Retry-After` header with the seconds to wait.

| `code` | When | What to do |
|---|---|---|
| `rate_limited` | Over a [rate limit](rate-limits.md) | Wait `Retry-After` seconds, then retry |
| `login_throttled` | Too many failed sign-ins for an email | Wait, then sign in again |
| `cooldown_active` | A payment-method link was emailed to this customer less than a minute ago. Counted only on a server with Redis. | Wait |

### 500 Internal Server Error

| `code` | When | What to do |
|---|---|---|
| `internal_error` | An unexpected fault. The details are in the server's log, not in the answer. | See [Retry after an error](#retry-after-an-error). Look up the `request_id` in the logs. |

### 502 Bad Gateway

| `code` | When | What to do |
|---|---|---|
| `provider_error` | Stripe refused the request, such as a declined card. The message says why. | Read the message; do not retry blindly |
| `payment_outcome_unknown` | Stripe never answered. The charge or refund may have gone through. | Check the invoice before you retry |
| `stripe_key_unusable` | Stripe refused the account's key, or no key is connected for this mode. Nothing was charged. | Fix the Stripe key under **Settings** |
| `stripe_error` | The hosted page could not start a Stripe Checkout session | Try again later |
| `smtp_error` | The email server refused the test email. The message says what it answered. | Fix the email settings |
| `email_enqueue_failed` | An email could not be queued | Retry |

### 503 Service Unavailable

| `code` | When | What to do |
|---|---|---|
| `authentication_unavailable` | Hardcount could not check the key or session | Retry the same request |
| `idempotency_unavailable` | Hardcount could not record the `Idempotency-Key`, so it ran nothing | Retry with the same key after `Retry-After` (1 second) |
| `ingest_unavailable` | The usage store did not answer. Nothing was recorded. | Send the same events again |
| `currency_unavailable` | `POST /v1/invoices` could not read the customer's currency | Retry |

### 504 Gateway Timeout

| `code` | When | What to do |
|---|---|---|
| `timeout` | The request ran past its 30-second time limit before any answer was written. A request that runs too long can also end with `500` `internal_error`. | See [Retry after an error](#retry-after-an-error) |

## Retry after an error

- **`4xx`:** do not retry the same request, except after `409` codes that
  say so and after `429`. Fix the request first.
- **`429`:** wait `Retry-After` seconds, then send the same request.
- **`503`:** retry. Each `503` above runs nothing, so the same request and
  the same keys are safe.
- **`500`, `502` and `504` on a usage route:** send the same events with
  the same keys. A key is never counted twice.
- **`500`, `502` and `504` on another write sent with an `Idempotency-Key`:**
  the answer is stored, so a retry with the same key gets it again. Read the
  resource to see whether the change happened, and retry with a new key only
  if it did not. See [Idempotency](idempotency.md#what-a-retry-with-the-same-key-gets).

## Find a request in your logs

Every answer has a `Hardcount-Request-Id` header, such as
`req_d3l2a8c5d6q3f0bnsp0g`. An error has the same value in `request_id`.
Hardcount makes the ID itself; a request cannot set it.

The server writes one JSON log line per request, with the ID in the
`request_id` field. Every other line the request writes, such as the
details of a `500`, carries it too. To find them on a
[quick start](../quick-start.md) install:

```bash
cd deploy/compose
docker compose logs hardcount-api | grep 'req_d3l2a8c5d6q3f0bnsp0g'
```

Under `make dev`, search the terminal output of the API.

## Request bodies

A request body may hold only the fields its operation lists in the
[API reference](../api-surface.md#the-full-reference).

- Any other field is refused with `422` and `param` naming it. That includes
  a misspelled field, a field the operation does not take, a field the
  server sets (such as `id`, `tenant_id` or `created_at`), and a name in
  another case (such as `Email` for `email`). Nothing is changed.
- A usage batch names each such event in `errors` instead.
- An operation with no request body, such as
  `POST /v1/subscriptions/{id}/cancel`, takes no body, `{}` or `null`.
- A body that is not one JSON value is refused with `400`.
- The Stripe and Postmark webhooks and the LiteLLM callback are the
  exceptions: they take their sender's body as it comes.

Bodies are limited to 1 MB, and Stripe webhook bodies to 64 KB. Only the
usage batch and the Stripe webhook answer `413`. Most other routes answer a
larger body with `400` `invalid_request`.

## Answers in another shape

- **A rejected usage batch** (`422` `batch_rejected`) has, beside `error`,
  `errors` (one line per failing event), `ingested: 0` and `total`.
- **`POST /v1/billing/run`** answers `206` when some subscriptions could not
  be billed, with `invoices_generated` and `errors`.
- **A replayed `Idempotency-Key`** returns the first answer's body byte for
  byte.
- **A CSV export** that fails partway ends with a row that starts
  `EXPORT_INCOMPLETE`. Treat that file as incomplete.
- **`GET /health/ready`** answers `503` with `{"status", "checks"}`.
- **The Postmark webhook** accepts a body that is too large or not JSON
  with `200` and `{"status": "skipped", "reason": "<reason>"}`.
- **Plain text, not JSON**, comes from two places:
  - The HTTP server itself, before Hardcount sees the request. A request it
    cannot parse gets `400`; headers over 8 KB get `431`; an unknown
    transfer encoding gets `501`; an unsupported HTTP version gets `505`.
    These answers have no `Hardcount-Request-Id`.
  - `/metrics`, the Prometheus route, answers a plain-text `500` if it
    cannot read the metrics.
