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
| `invalid_request` | The body is not one JSON value, or a field has the wrong JSON type (`invalid JSON body`). The body is over 1 MB. A usage batch has no events or more than 1,000. | Fix the request |

### 401 Unauthorized

| `code` | When | What to do |
|---|---|---|
| `unauthorized` | No key or session, or a wrong, revoked or expired one. Also a wrong sign-in, and a bad public cost-dashboard link. | Fix the key. The messages: [When a key is refused](authentication.md#when-a-key-is-refused). |
| `invalid_token` | A payment-update link that is unknown, expired or already used, or whose invoice was removed | Ask for a new link |

### 403 Forbidden

| `code` | When | What to do |
|---|---|---|
| `forbidden` | The key's type may not call this route. A route that takes only a dashboard session. A browser request from another site. A test clock used with a live key. The other mode's Stripe connection. | Use the right key; see [Authentication](authentication.md) |

### 404 Not Found

| `code` | When | What to do |
|---|---|---|
| `not_found` | The resource, or one the body names, does not exist in this account and mode. A bad hosted invoice link gets it too. | Check the ID and the key's mode |
| `route_not_found` | No route has this path | Check the URL |
| `invoice_not_found` | The invoice behind a payment-update link was removed while the request ran | Ask for a new link |
| `customer_not_found` | The customer behind a payment-update link no longer exists | Ask for a new link |

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
| `concurrent_change` | Postgres stopped the request because of another change to the same records: it broke a deadlock, or it ended the request's wait for a lock. Only the routes listed below answer it. | Send the same request again. With an `Idempotency-Key`, the same key works. Some steps stay done (below). |
| `already_bootstrapped` | `POST /v1/bootstrap` after the first account exists | Do not retry |

These routes answer `concurrent_change`:

- Subscriptions: create, activate, cancel, and add, change or remove an item.
- Customers: update, and write the billing profile.
- Invoices: record payment, and collect when it settles the invoice without
  a card charge.
- Credit notes: issue, and the issue step of create with `auto_issue`. The
  create step itself answers `500`.

Postgres ends a wait for a lock only when a `lock_timeout` is set, for
example on the database user Hardcount runs as. On any other route or step,
a deadlock or an ended lock wait answers `500` `internal_error`, which an
`Idempotency-Key` stores ([Idempotency](idempotency.md)).

After `concurrent_change`, a step that the request saved on its own before
Postgres stopped it stays done. The routes not listed here save nothing
first:

- **Cancel, and adding, changing or removing an item:** the invoice for a
  billing period that had already ended. It may already be charged. The
  request sent again does not bill that period twice.
- **Collect:** the customer's credit that the request applied to the invoice
  before the settle that Postgres stopped. Sent again, Collect finds nothing
  left due, so it applies no credit and marks the invoice paid.
- **Create with `auto_issue`:** the note it made, which is voided. The
  request sent again makes and issues a new note.

**Idempotency** ([how idempotency keys work](idempotency.md))

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
| `unpaid_invoice_blocks_change` | A change that charges more (an upgrade, an added item, more seats) while the current period's invoice is unpaid. A written-off invoice counts as unpaid. | Collect or void that invoice first; writing it off does not lift this |
| `funding_invoice_tax_pending` | A change that credits unused time (a cancel, a downgrade, a removed item), while the invoice that billed that time is a draft whose tax is not worked out yet | Try again later |
| `item_changed` | The subscription's items changed while the request ran | Read it again, then retry |
| `subscription_period_moved` | The subscription moved to a new billing period while the request ran, or a spend-threshold invoice has already billed this subscription past the current time | Read it again, then retry, or schedule the change for the next period |
| `billing_not_failing` | `retry-billing` on a subscription whose billing is not failing | Nothing to do |

**Invoices and payments**

| `code` | When | What to do |
|---|---|---|
| `payment_in_flight` | A card charge for the invoice is running, or its result is not known yet. Every charge takes a second or so. A void, write-off, offline payment, credit note, collection or email would run at the same time as that charge. | Wait a few minutes, then retry. After an unclear answer from Stripe, Hardcount can take about an hour to settle the charge. |
| `payment_unidentifiable` | An earlier Hardcount version parked this invoice: Stripe's answer for its charge was unclear and named no payment. Only a write-off is allowed. Invoices this version charges never return it. | Mark the invoice uncollectible, or ask the person who runs your server to check it ([runbook](../ops/runbook/unclear-charge.md#parked-no-paymentintent)) |
| `payment_in_progress` | A payment for this invoice is already in progress on the hosted page | Wait |
| `not_payable` | The invoice cannot be paid now, for example it is paid or voided | Do not retry |
| `amount_below_minimum` | The amount is below Stripe's smallest card charge | Void it, record an offline payment, or bill it later |
| `card_refused` | The bank refused the saved card for good | Ask the customer for a new card |
| `invoice_not_collectible` | You asked to resend a payment link for an invoice that is not unpaid or written off | Do not retry |
| `invoice_not_sendable` | You asked to email a draft or voided invoice | Finalize it first, or do not send it |
| `no_email_on_file` | You asked to resend a payment link to a customer with no email | Add an email to the customer |
| `invoice_changed`, `invoice_lines_changed` | The invoice changed while the request ran | Read it again, then retry |
| `invoice_not_manual` | You tried to add a line to a draft invoice that billing made for a subscription. That invoice keeps the lines and tax that billing gave it. | Bill the charge on its own one-off invoice |
| `credit_note_exceeds_invoice`, `invoice_not_open_receivable` | A credit note no longer fits the invoice, which changed while the request ran | Read the invoice, then retry with the right amount |
| `customer_currency_conflict` | A one-off invoice's currency is not the customer's billing currency | Use the customer's currency |
| `tax_calculation_expired` | Stripe Tax's calculation for the invoice is too old to finalize | Retry the tax, then finalize |
| `stripe_refund_unresolved` | Retrying a credit note's refund while a refund made in Stripe for this invoice is not recorded yet, or is still in progress at Stripe | If it is not recorded: record it, attach it or dismiss it on the invoice. If it is in progress: wait for Stripe to settle it. Then retry. |
| `credit_block_changed` | The customer's credit changed while it was being returned | Retry |

**Setup on this server**

| `code` | When | What to do |
|---|---|---|
| `email_not_configured` | An action that sends email, on a server with no email set up (`SMTP_HOST`). Nothing was sent. | The person who runs your server sets up email: [Email setup](../ops/email-setup.md) |
| `stripe_not_connected` | An action that needs Stripe, in a mode with no Stripe keys | Connect Stripe under **Settings** |
| `return_address_not_set` | A setup link (`send-setup-email`, or `setup-session` with no `return_url`) on a server with no `CUSTOMER_PORTAL_URL`, so Stripe would have no page to send the customer back to. Nothing was created at Stripe. | The person who runs your server sets `CUSTOMER_PORTAL_URL`: [Settings](../settings.md) |
| `online_payment_unavailable` | The hosted invoice or payment-update page cannot take a card: the seller has not connected Stripe for this mode, or the server has no address for Stripe to send the customer back to (`HOSTED_INVOICE_BASE_URL`, `PAYMENT_UPDATE_RETURN_URL`) | The customer pays another way. A payment-update link is not used up, so it works once the seller fixes the setup |

**Rare: a record that must be unique already exists**

These guard against a record being written twice, mostly by Hardcount's own
billing run. A request rarely gets one. Do not retry; read the invoice or
the credit ledger, and report it if the state looks wrong:
`invoice_number_taken`, `invoice_billing_period_taken`,
`invoice_threshold_cycle_taken`, `invoice_proration_source_taken`,
`invoice_public_token_collision`, `invoice_unique_violation`,
`credit_note_source_taken`,
`credit_reversal_source_taken`, `credit_proration_source_taken`,
`credit_commit_fund_taken`, `credit_unique_violation` and
`credit_source_amount_mismatch`.

### 413 Payload Too Large

| `code` | When | What to do |
|---|---|---|
| `batch_too_large` | A `POST /v1/usage-events/batch` body is over 1 MB, or a `POST /v1/integrations/litellm/spend` body is over 32 MB. Nothing was saved. | Send smaller batches |
| `payload_too_large` | A Stripe webhook body is over 64 KB | Nothing; Stripe's events are smaller |

### 422 Unprocessable Entity

| `code` | When | What to do |
|---|---|---|
| `validation_error` | A field failed a rule, such as a missing `external_id`, a bad `limit`, a list `sort` or `dir` the list does not take, or a field the operation does not take. A filter that takes a fixed set of values got another, such as `status` on the subscription list. `param` names it. | Fix the field |
| `batch_rejected` | At least one event of a usage batch failed. Nothing was stored. The body lists each failure in `errors`. | Fix the events named, then send the whole batch again |
| `idempotency_error` | An `Idempotency-Key` was used before with a different method, path or body | Use a new key for a new request |
| `customer_has_no_subscription` | An invoice preview or a usage summary for a customer with no active subscription | Send `subscription_id`, or a period with `from` and `to` |
| `subscription_has_no_cycle` | An invoice preview for a subscription with no current billing period | Send a period |
| `missing_email` | You asked to email a payment-method link to a customer with no email | Add an email |
| `note_too_long` | That email's note is over 2,000 bytes | Shorten it |
| `invalid_state` | You asked to rotate the public link of a draft invoice | Finalize the invoice first |
| `invalid_or_expired` | The dashboard's check of a password-reset link (`GET /v1/auth/password-reset/check`) found it unknown, expired or used. Setting the password, or accepting an invite, with a bad token answers `validation_error` with `param: token` instead. | Ask for a new link |

### 429 Too Many Requests

Each sends a `Retry-After` header with the seconds to wait.

| `code` | When | What to do |
|---|---|---|
| `rate_limited` | Over a [rate limit](rate-limits.md) | Wait `Retry-After` seconds, then retry |
| `login_throttled` | Too many failed sign-ins for an email | Wait, then sign in again |
| `cooldown_active` | Someone asked to email this customer a payment-method link less than a minute ago, even if that request then failed. Counted only on a server with Redis. | Wait a minute, then send it with a new `Idempotency-Key`: this answer is stored like any other |

### 500 Internal Server Error

| `code` | When | What to do |
|---|---|---|
| `internal_error` | An unexpected fault. The details are in the server's log, not in the answer. | See [Retry after an error](#retry-after-an-error). Look up the `request_id` in the logs. |

### 502 Bad Gateway

| `code` | When | What to do |
|---|---|---|
| `provider_error` | Stripe refused the request, such as a declined card. The message says why. | Read the message; do not retry blindly |
| `payment_outcome_unknown` | Stripe never answered. The charge or refund may have gone through. | Check the invoice before you retry |
| `stripe_key_unusable` | Stripe refused the account's key, or no key is connected for this mode. Nothing was charged. If Stripe refused the key on the confirm, the invoice shows `unknown` until the first scheduled run after the key works again. | Fix the Stripe key under **Settings** |
| `stripe_error` | The hosted page could not start a Stripe Checkout session | Try again later |
| `smtp_error` | The email server refused the test email. The message says what it answered. | Fix the email settings |
| `email_enqueue_failed` | An email could not be queued | Retry |

### 503 Service Unavailable

| `code` | When | What to do |
|---|---|---|
| `authentication_unavailable` | Hardcount could not check the key or session | Retry the same request |
| `idempotency_unavailable` | Hardcount could not record the `Idempotency-Key`, so it ran nothing | Retry with the same key after `Retry-After` (1 second) |
| `ingest_unavailable` | The usage store did not answer. Nothing was recorded. | Send the same events again |
| `currency_unavailable` | `POST /v1/invoices` could not read the customer's currency. No invoice was made. | Retry the same request, with the same `Idempotency-Key` if you sent one |
| `proration_unavailable` | Adding an item, or an immediate quantity or plan change that costs more, could not check whether the current period's invoice is paid. The change was not made. | Retry the same request after `Retry-After` (1 second), with the same `Idempotency-Key` if you sent one |
| `catchup_queue_full` | A test-clock advance or retry-advance found too many catch-ups waiting. A catch-up is the billing Hardcount runs for the time a test clock skipped. This clock's catch-up did not start, and the clock is set to `internal_failure`. | Once the running catch-ups finish, send `POST /v1/test-clocks/{id}/retry-advance`. The same advance again answers `409`. |

### 504 Gateway Timeout

| `code` | When | What to do |
|---|---|---|
| `timeout` | The request ran past its 30-second time limit before any answer was written. A request that runs too long can also end with `500` `internal_error`. | See [Retry after an error](#retry-after-an-error) |

## Retry after an error

- **`4xx`:** do not send the same request again, except after `409` codes
  that say so and after `429`. Fix the request first. If it carried an
  `Idempotency-Key`, send the fixed request with a new key: the answer was
  stored, and the old key with a changed body answers `422`
  `idempotency_error`.
- **`429` `rate_limited` and `login_throttled`:** wait `Retry-After`
  seconds, then send the same request, with the same key. Nothing ran and
  nothing was stored. `cooldown_active` is stored: see its row above.
- **`503` `authentication_unavailable` and `idempotency_unavailable`:** send
  the same request with the same key. Nothing ran.
- **`503` `ingest_unavailable`:** send the same events with the same keys.
  Nothing was recorded.
- **`503` `currency_unavailable` and `proration_unavailable`:** the change
  was not made, and a `503` is never stored. Send the same request with the
  same key.
- **`500`, `502` and `504` on a usage route:** send the same events with
  the same keys. A key is never counted twice.
- **`500`, `502` and `504` on another write sent with an `Idempotency-Key`:**
  the answer is stored, so a retry with the same key gets it again. Read the
  resource to see whether the change happened, and retry with a new key only
  if it did not. A `500` from a crash inside Hardcount is the exception: it
  is not stored, and a retry with the same key gets `409`
  `conflict_idempotency`, then `conflict_idempotency_unresolved` after 5
  minutes. Treat that the same way. See
  [Idempotency](idempotency.md#what-a-retry-with-the-same-key-gets).

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

Bodies are limited to 1 MB, LiteLLM callback bodies to 32 MB, and Stripe
webhook bodies to 64 KB. Only the usage batch, the LiteLLM callback and the
Stripe webhook answer `413`. Most other routes answer a larger body with `400`
`invalid_request`.

## Answers in another shape

- **A rejected usage batch** (`422` `batch_rejected`) has, beside `error`,
  `errors` (one line per failing event), `ingested: 0` and `total`.
- **`POST /v1/billing/run`** answers `206` when some subscriptions could not
  be billed, with `invoices_generated` and `errors`.
- **A replayed `Idempotency-Key`** returns the first answer's body byte for
  byte.
- **A CSV export** that fails partway ends with a row that starts
  `EXPORT_INCOMPLETE`, or stops with no such row when the connection is
  cut. Treat a file as complete only if your client got the whole response
  without an error.
- **`GET /health/ready`** answers `503` with `{"status", "checks"}`.
- **The Postmark webhook** accepts a body that is too large or not JSON
  with `200` and `{"status": "skipped", "reason": "<reason>"}`.
- **Plain text, not JSON**, comes from two places:
  - The HTTP server itself, before Hardcount sees the request. A request it
    cannot parse gets `400`; headers over about 12 KB get `431`; an unknown
    transfer encoding gets `501`; an unsupported HTTP version gets `505`.
    These answers have no `Hardcount-Request-Id`.
  - `/metrics`, the Prometheus route, answers a plain-text `500` if it
    cannot read the metrics.
