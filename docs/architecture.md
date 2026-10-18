# Architecture

Velox is one Go binary on one PostgreSQL database. The code has one package
per billing domain (customers, pricing, usage, invoices, payments and so on),
and each package owns its store, service and HTTP handler.

This page is for contributors. It shows how a usage event becomes a paid
invoice, which package does each part, and what runs in the background. The
same flow without code is in [How Velox works](how-velox-works.md). The
decisions behind this layout are in the [ADRs](adr/).

## The life of a usage event

Think of a taxi meter: it counts the ride, the fare card sets the price, and
at the end the driver gives you a receipt and charges your card. In Velox,
`usage` counts, `pricing` holds the prices, `billing` works out the bill,
`invoice` is the receipt and `payment` charges the card:

```
your app → usage → billing → invoice → payment → paid
                                          │
                                     card fails
                                          ▼
                                       dunning
```

1. **Your app sends a usage event.** `POST /v1/usage-events` (or `/batch`)
   names the customer by `external_customer_id` and the meter by
   `event_name`. A meter is a counter for one kind of usage, such as tokens.
   The `usage` package stores the event and answers `201`.
   **You see:** the event on the **Usage** page.
2. **The billing period ends.** The billing scheduler runs every hour (every
   5 minutes when `APP_ENV=local`). It closes a period on the first run after
   the period end plus the invoice grace period (`INVOICE_GRACE_PERIOD`,
   default 1 hour), so usage that arrives a little late still counts.
3. **Billing prices the usage.** The `billing` engine asks `usage` for the
   period's totals per meter and asks `pricing` for the rating rules (the
   price for each meter). It adds the plan's flat fees and builds the
   invoice through `invoice`.
   **You see:** a new invoice on the **Invoices** page and on the customer.
4. **The invoice is finalized.** Tax is worked out by the tenant's tax
   provider (see [Tax providers](#tax-providers)). If tax cannot be worked
   out yet, the invoice waits as a draft and a later run retries. Once
   finalized, the invoice's amounts are fixed.
5. **Velox charges the card.** Credits the customer holds are applied
   first. For the rest, `payment` creates a Stripe PaymentIntent on the
   customer's saved card. Stripe's answer, or its webhook, tells Velox the
   result, and a paid invoice moves to `paid`.
   **You see:** the invoice marked **Paid**, and the payment in your Stripe
   dashboard.
6. **A failed payment goes to dunning.** Dunning means retrying a failed
   card payment on a schedule and emailing the customer. The `dunning`
   package follows the tenant's dunning policy. If every retry fails, it
   takes the policy's final action, for example cancel the subscription or
   mark the invoice uncollectible.
   **You see:** the invoice on the **Dunning** page, with each retry.

Along the way, each step queues webhook events (such as an invoice being
finalized or paid) and emails for the background senders below.

### How an invoice ends

An invoice ends in one of three states:

- **paid**: the money arrived: by card, from credits, or recorded by hand.
- **voided**: the invoice was cancelled before it was paid. A voided invoice
  never changes again.
- **uncollectible**: the invoice was written off, by an operator or by a
  dunning policy. It stays in your reports, and it can still move to `paid`
  if the customer pays later.

A paid invoice cannot be voided or written off; issue a credit note instead.

## Design rules

- **Per-domain packages.** Each domain owns its store, service, and handler. No peer domain touches another's internals. The imports that legitimately cross domains are listed edge by edge in an allowlist, enforced by an [architecture test](../internal/arch/boundaries_test.go).
- **Row-Level Security (RLS).** Every tenant-scoped query runs inside a transaction where Postgres enforces tenant isolation. A tenant is one account's data in the shared database. Integration tests prove it.
- **PaymentIntent-only Stripe.** No Stripe Billing or Stripe Invoices. Velox owns invoices end to end; Stripe executes the card charge.
- **Billing engine as coordinator.** `billing` is the one package that works across domains. It calls usage, pricing, invoice, payment and the others only through small interfaces it defines itself (`UsageAggregator`, `PricingReader`, `InvoiceWriter`, `InvoiceCharger` and so on). The other domains do not know about each other, and the engine can be tested with fakes.
- **Append-only records for money.** Credits, the audit log, and the outbound webhook outbox are written as append-only events.

## Package layout

```
cmd/velox/                  — single Go binary
cmd/velox-doctor/           — read-only money-invariant sweep (see ENGINEERING.md)
internal/                     (abridged — one package per domain)
  domain/                   — pure domain models, zero deps
  auth/                     — API key auth (3 key types, per-route permissions)
  customer/                 — customer CRUD + billing profiles
  pricing/                  — meters, rating rules, plans, price overrides
  subscription/             — lifecycle (draft → trialing → active → canceled → archived)
  usage/                    — event ingestion + multi-dim aggregation
  invoice/                  — lifecycle (draft → finalized → paid, or voided / uncollectible) + PDF
  billing/                  — billing engine + scheduler + preview
  payment/                  — Stripe PaymentIntent + webhook receiver
  dunning/                  — payment retry state machine
  credit/                   — event-sourced prepaid balance ledger
  creditnote/               — credit notes + refunds
  webhook/                  — outbound webhooks (HMAC-signed delivery)
  email/                    — email outbox + sender
  testclock/                — test clocks + the worker that bills skipped time
  audit/                    — immutable append-only audit log
  platform/leader/          — leader leases for the background jobs
  platform/postgres/        — RLS-aware database layer
  platform/migrate/         — embedded SQL migrations

web-v2/                     — operator dashboard (React 19 + TypeScript + Tailwind)
```

## Background work

Every copy of Velox runs the same background loops. Jobs that must run once
across all copies take a **leader lease** first: a row in the
`leader_leases` table that lets exactly one copy run one tick of that job.
The lease expires 10 seconds after its last renewal, so another copy takes
over when a leader dies. The five leased jobs:

| Job | What it does |
|---|---|
| `billing` | Runs each tick for live mode, then test mode. Retries stuck work (charges with an unclear result, tax). Charges invoices waiting for payment. Checks usage thresholds and ends trials. Closes billing periods into invoices. Resumes paused collection and expires credits |
| `dunning` | Runs the payment retries that are due, queues their emails, and takes final actions |
| `webhook_outbox` | Sends queued webhook events to your endpoints |
| `email_outbox` | Sends queued emails |
| `webhook_delivery` | Retries webhook deliveries that failed |

An **outbox** is a database table of messages waiting to be sent
(`webhook_outbox`, `email_outbox`). Most messages are written in the same
transaction as the change that caused them, so a crash cannot lose them.

A **test clock** is a pretend clock for test customers. When you move one
forward, a worker in the copy that took the request bills the skipped time
for that clock's customers, so a month of renewals and retries takes
seconds.

More on each term: [glossary](glossary.md#running-velox). When a job is
stuck: [A background job is stuck](ops/runbook/background-job-stuck.md).

## Tech stack

**Backend** — Go 1.26, chi/v5 router, PostgreSQL 16 with RLS, `shopspring/decimal` for money, `signintech/gopdf` for invoices, `wneessen/go-mail` to build emails (Velox's own SMTP code sends them), Prometheus metrics.

**Frontend** — React 19, TypeScript, Vite, TailwindCSS, shadcn/ui, Lucide icons.

**Payments** — Stripe (PaymentIntents + Checkout Sessions). No Stripe Billing dependency.

## Tax providers

Tax is calculated by one of three providers in `internal/tax`: `none`, `manual` and `stripe_tax`. Each tenant picks one in `tenant_settings.tax_provider`. The billing engine resolves the provider when it builds the invoice, and reuses the same provider at finalize and at credit-note issue. How an operator chooses and runs them: [Tax calculation](ops/tax-calculation.md).

- **`none`** returns zero per line. `Commit` and `Reverse` do nothing. An empty `tax_provider` falls through to `none`. An unrecognised value is an error that stops invoice creation for that tenant, so a corrupted settings row stalls billing instead of taxing at zero.
- **`manual`** applies `tenant_settings.tax_rate` (a percent: `7.25` is 7.25%) and `tenant_settings.tax_name` to every line. It handles tax-inclusive pricing (`Request.TaxInclusive`), exempt customers (`StatusExempt`) and reverse-charge customers (`StatusReverseCharge`). `Commit` and `Reverse` do nothing, because there is no state outside Velox to record.
- **`stripe_tax`** calls Stripe's Tax Calculations API at invoice build, creates a `tax_transaction` at finalize (`Commit`), and reverses it at credit-note issue (`Reverse`). The Stripe client is resolved from the context (`tenant_id` and `livemode`) through the `StripeClientResolver` interface, so each tenant's calls reach its own Stripe account in the right mode.

### The provider interface

| Method | Called at | Changes state at Stripe? |
|---|---|---|
| `Name` | — | no |
| `Calculate` | invoice build | no |
| `Commit` | invoice finalize | yes (`stripe_tax`) |
| `Reverse` | credit-note issue | yes (`stripe_tax`) |

- `Commit` makes the tax decision permanent at Stripe. A Stripe Tax calculation expires after 24 hours; a `tax_transaction` does not, and it shows in Stripe's tax reports. Its ID is saved on `invoices.tax_transaction_id`. The invoice ID is the transaction's reference, and Stripe keeps references unique per account, so a retried finalize is idempotent.
- `Reverse` reverses an existing `tax_transaction` when a credit note is issued. The credit-note ID is the reversal's reference, so a retried issue is idempotent too.
- `none` and `manual` return an empty `ReversalResult`, so the credit-note flow needs no branch per provider.

### Failure: defer, never guess

When `stripe_tax` fails, the error propagates, the engine defers the invoice to `tax_status=pending`, and a scheduled retry runs `Calculate` again. It never guesses a rate. The three failure reasons:

| Reason | Meaning |
|---|---|
| `no_country` | The customer has no country |
| `no_client_for_mode` | No Stripe client for the request's mode |
| `api_error` | Stripe Tax returned an error |

Each failure increments `velox_tax_outcome_total{outcome="deferred", reason}` and logs a `warn`. Successful calculations are not counted.

`block` is the only `OnFailure` value. The `fallback_manual` policy, which used the tenant's manual rate during a Stripe Tax outage, was removed (see [ADR-041](adr/041-tax-fallback-manual-removed.md)). It traded correctness for availability. An invoice at a guessed tax rate is a compliance risk: a wrong rate exposes regulated marketplaces and tenants registered for EU VAT under OIDAR. Deferring with a retry is safe, the pile of deferred invoices is itself the alert, and a short outage clears on the next tick.

A test-mode invoice resolves to the test Stripe client and a live-mode invoice to the live client, for two reasons:

- Test-mode keys have their own rate limits, so a test-mode load test cannot use up a tenant's live quota.
- Calling Tax on the wrong account is wrong accounting: a calculation leaves nothing behind at Stripe, but `Commit` does.

If only one mode has a key, calculations in the other mode defer with `no_client_for_mode`.

### What is saved on the invoice

Tax decisions are recorded on the invoice itself, not in a separate audit table:

| Column | Source |
|---|---|
| `invoices.tax_provider` | The provider that produced this invoice |
| `invoices.tax_calculation_id` | Stripe Tax `calc_xxx` (`stripe_tax` only) |
| `invoices.tax_transaction_id` | Stripe Tax `tx_xxx`, set at `Commit` (`stripe_tax` only) |
| `invoices.tax_reverse_charge` | Reverse-charge flag |
| `invoices.tax_exempt_reason` | The exempt customer's reason |
| `invoice_line_items.tax_rate` | Rate applied to this line (a percent: `7.25` is 7.25%) |
| `invoice_line_items.tax_name` | Label shown on the invoice |

The line fields are what the customer sees on the PDF. The invoice fields link to Stripe: they let `Reverse` find the original `tax_transaction`, and let an operator match a Velox invoice to Stripe's tax reports without reading logs.

`internal/tax/taxid.go` checks the shape of customer tax IDs (VAT, GST and others) and puts the checked ID on the invoice, so the PDF shows the right legal text. It does not ask a tax authority whether the ID exists.
