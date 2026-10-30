# Architecture

Hardcount is one Go binary on one PostgreSQL database. The code has one package
per billing domain (customers, pricing, usage, invoices, payments and so on),
and each package owns its store, service and HTTP handler.

This page is for contributors: which package does each part, and what runs
in the background. The same flow without code is in
[How Hardcount works](how-hardcount-works.md). The decisions behind this layout are
in the [ADRs](adr/).

## The life of a usage event

```
your app → usage → billing → invoice → payment → paid
                                          │
                                     card fails
                                          ▼
                                       dunning
```

1. **`usage`** stores each event from `POST /v1/usage-events` (or `/batch`)
   and answers `201`.
2. **The billing scheduler** closes a period on its first run after the
   period end plus `INVOICE_GRACE_PERIOD` (default 1 hour). It runs every
   hour, or every 5 minutes when `APP_ENV=local`.
3. **`billing`** asks `usage` for the period's totals per meter and
   `pricing` for the plan's prices, adds the plan's flat fees, and builds the
   invoice through `invoice`.
4. **`invoice`** finalizes it. Tax comes from the tenant's tax provider
   ([Tax providers](#tax-providers)); if tax cannot be worked out yet, the
   invoice waits as a draft and a later run retries.
5. **`payment`** charges what credit did not cover, with a Stripe
   PaymentIntent on the customer's saved card.
6. **`dunning`** retries a failed charge on the tenant's dunning policy, and
   takes the policy's final action if every retry fails.

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
- **Row-Level Security (RLS).** Most tenant-scoped work runs inside a transaction where Postgres enforces tenant isolation (`postgres.TxTenant`). A tenant is one account's data in the shared database. Integration tests prove it. Some work runs under `postgres.TxBypass`, where RLS does not apply, and those queries must filter by tenant themselves. A test ([`txbypass_allowlist_test.go`](../internal/arch/txbypass_allowlist_test.go)) fails when code adds a new use without listing it:
  - background jobs, and the metrics that count their queues;
  - sign-in, sessions, API key lookup and creating an account;
  - public links, such as the hosted invoice page;
  - incoming webhooks;
  - two signed-in areas: Stripe credential settings (`internal/tenantstripe`) and team members and invites (`internal/dashmembers`);
  - `hardcount-bench-seed`, which sets up a database for a benchmark.
- **Every tenant-scoped operation states its mode.** Test mode and live mode are separate data in one tenant. Each entry point puts the mode on the request context with `postgres.WithLivemode`: the auth middleware from the key or session, public links from their row, background jobs per row or per mode. Nothing guesses a missing mode:
  - `postgres.Livemode` returns `ErrLivemodeUnset`, and `BeginTx` refuses a `TxTenant` without a mode or a tenant ID;
  - the database refuses an insert into a table with a `livemode` column when `app.livemode` is not `on` or `off`, and its RLS policies show no row (migrations 00213 and 00214).
- **PaymentIntent-only Stripe.** No Stripe Billing or Stripe Invoices. Hardcount owns invoices end to end; Stripe executes the card charge.
- **Billing engine as coordinator.** `billing` is the one package that works across domains. It calls usage, pricing, invoice, payment and the others only through small interfaces it defines itself (`UsageAggregator`, `PricingReader`, `InvoiceWriter`, `InvoiceCharger` and so on). Other domains import only shared pieces (`auth`, `audit`, `session`, and the `tax` value types), apart from a few peer imports, each listed with its reason in `internal/arch/boundaries_test.go`. That test fails on any new one. The engine can be tested with fakes.
- **Ledger and audit records for money.** Credit grants, usage, expiry and adjustments are entries in one credit ledger. The ledger is not strictly append-only: each grant entry keeps a running count of what has been used of it (`consumed_cents`, `cn_retired_cents`), updated in place. The audit log is append-only: triggers refuse any change or delete, and the limited database user (`hardcount_app`) has no right to make one. A database owner could still remove the triggers.

## Package layout

```
cmd/hardcount/                  — single Go binary
cmd/hardcount-doctor/           — read-only money-invariant sweep (see ENGINEERING.md)
internal/                     (abridged — one package per domain)
  domain/                   — pure domain models, zero deps
  auth/                     — API key auth (2 stored key types, secret and ingest, plus dashboard sessions, per-route permissions)
  customer/                 — customer CRUD + billing profiles
  pricing/                  — meters, plans and their prices, price overrides
  subscription/             — lifecycle (created trialing or active, or as a draft until activated; trialing → active → canceled)
  usage/                    — event ingestion + multi-dim aggregation
  invoice/                  — lifecycle (draft → finalized → paid, or voided / uncollectible) + PDF
  billing/                  — billing engine + scheduler + preview
  payment/                  — Stripe PaymentIntent + webhook receiver
  dunning/                  — payment retry state machine
  credit/                   — prepaid credit ledger (grants, usage, expiry, adjustments)
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

Every copy of Hardcount runs the same background loops. Jobs that must run once
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
(`webhook_outbox`, `email_outbox`). Many messages are written in the same
transaction as the change that caused them, so a crash cannot lose those
([which webhooks](webhooks.md#three-rules-for-your-receiver)).

When you move a [test clock](glossary.md#testing-with-simulated-time)
forward, a worker in the copy that took the request bills the skipped time
for that clock's customers.

More on each term: [Words on these pages](ops/runbook/index.md#words-on-these-pages). When a job is
stuck: [A background job is stuck](ops/runbook/background-job-stuck.md).

## Tech stack

**Backend** — Go 1.26, chi/v5 router, PostgreSQL 16 with RLS, `shopspring/decimal` for money, `signintech/gopdf` for invoices, `wneessen/go-mail` to build emails (Hardcount's own SMTP code sends them), Prometheus metrics.

**Frontend** — React 19, TypeScript, Vite, TailwindCSS, shadcn/ui, Lucide icons.

**Payments** — Stripe (PaymentIntents + Checkout Sessions). No Stripe Billing dependency.

## Tax providers

Tax is calculated by one of three providers in `internal/tax`: `none`, `manual` and `stripe_tax`. Each tenant picks one in `tenant_settings.tax_provider`. The billing engine resolves the provider when it builds the invoice, and reuses the same provider at finalize and at credit-note issue. How an operator chooses and runs them: [Tax calculation](ops/tax-calculation.md).

- **`none`** returns zero per line. `Commit` and `Reverse` do nothing. An empty `tax_provider` falls through to `none`. An unrecognised value is an error that stops invoice creation for that tenant, so a corrupted settings row stalls billing instead of taxing at zero.
- **`manual`** applies `tenant_settings.tax_rate` (a percent: `7.25` is 7.25%) and `tenant_settings.tax_name` to every line, except a prepaid-commit purchase line, which no provider is asked to tax ([Tax on prepaid credit](ai-pricing.md#tax-on-prepaid-credit)). It handles tax-inclusive pricing (`Request.TaxInclusive`), exempt customers (`StatusExempt`) and reverse-charge customers (`StatusReverseCharge`). `Commit` and `Reverse` do nothing, because there is no state outside Hardcount to record.
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

When `stripe_tax` fails, the error propagates, the engine defers the invoice to `tax_status=pending`, and a scheduled retry runs `Calculate` again. It never guesses a rate. The four failure reasons:

| Reason | Meaning |
|---|---|
| `no_country` | The customer has no country |
| `no_client_for_mode` | No Stripe client for the request's mode |
| `credentials_unreadable` | The tenant's Stripe credentials for the mode could not be read |
| `api_error` | Stripe Tax returned an error |

Each failure increments `hardcount_tax_outcome_total{outcome="deferred", reason}` and logs a `warn`. Successful calculations are not counted.

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

The line fields are what the customer sees on the PDF. The invoice fields link to Stripe: they let `Reverse` find the original `tax_transaction`, and let an operator match a Hardcount invoice to Stripe's tax reports without reading logs.

`internal/tax/taxid.go` checks the shape of customer tax IDs (VAT, GST and others) and puts the checked ID on the invoice, so the PDF shows the right legal text. It does not ask a tax authority whether the ID exists.

**Next:** set up your machine: [CONTRIBUTING](../CONTRIBUTING.md#development-setup).
