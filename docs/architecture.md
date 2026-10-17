# Architecture

One Go binary, one package per domain. Each domain owns its store, service,
and handler. The decisions behind this layout live in the [ADRs](adr/).

## Design rules

- **Per-domain packages.** Each domain owns its store, service, and handler. No peer domain touches another's internals. The imports that legitimately cross domains are listed edge by edge in an allowlist, enforced by an [architecture test](../internal/arch/boundaries_test.go).
- **Row-Level Security (RLS).** Every tenant-scoped query runs inside a transaction where Postgres enforces tenant isolation. Integration tests prove it.
- **PaymentIntent-only Stripe.** No Stripe Billing or Stripe Invoices. Velox owns invoices end to end; Stripe executes the card charge.
- **Billing engine as coordinator.** The engine orchestrates the domains through narrow interfaces. It is not one object that does everything.
- **Append-only records for money.** Credits, the audit log, and the outbound webhook outbox are written as append-only events.

## Package layout

```
cmd/velox/                  — single Go binary
cmd/velox-doctor/           — read-only money-invariant sweep (see ENGINEERING.md)
internal/                     (abridged — one package per domain)
  domain/                   — pure domain models, zero deps
  auth/                     — API key auth (3 key types, 17 permissions)
  customer/                 — customer CRUD + billing profiles
  pricing/                  — meters, rating rules, plans, price overrides
  subscription/             — lifecycle (draft → trialing → active → canceled → archived)
  usage/                    — event ingestion + multi-dim aggregation
  invoice/                  — state machine (draft → finalized → paid) + PDF
  billing/                  — billing engine + scheduler + preview
  payment/                  — Stripe PaymentIntent + webhook receiver
  dunning/                  — payment retry state machine
  credit/                   — event-sourced prepaid balance ledger
  creditnote/               — credit notes + refunds
  webhook/                  — outbound webhooks (HMAC-signed delivery)
  audit/                    — immutable append-only audit log
  platform/postgres/        — RLS-aware database layer
  platform/migrate/         — embedded SQL migrations

web-v2/                     — operator dashboard (React 19 + TypeScript + Tailwind)
```

## Tech stack

**Backend** — Go 1.26, chi/v5 router, PostgreSQL 16 with RLS, `shopspring/decimal` for money, `signintech/gopdf` for invoices, Prometheus metrics.

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

`stripe_tax` fails with one of three reasons: `no_country` (the customer has no country), `no_client_for_mode` (no Stripe client for the request's mode) or `api_error` (Stripe Tax returned an error). In every case the error propagates, the engine defers the invoice to `tax_status=pending`, and a scheduled retry runs `Calculate` again. `block` is the only `OnFailure` value; the `fallback_manual` policy was removed (see [ADR-041](adr/041-tax-fallback-manual-removed.md)).

An earlier design fell back to the tenant's manual rate during a Stripe Tax outage. It traded correctness for availability and was cut. An invoice at a guessed tax rate is a compliance risk: a wrong rate exposes regulated marketplaces and tenants registered for EU VAT under OIDAR. Deferring with a retry is safe, the pile of deferred invoices is itself the alert, and a short outage clears on the next tick.

Each failure increments `velox_tax_outcome_total{outcome="deferred", reason}` and logs a `warn`. Successful calculations are not counted.

A test-mode invoice resolves to the test Stripe client and a live-mode invoice to the live client. That matters for two reasons. Test-mode keys have their own rate limits, so a test-mode load test cannot use up a tenant's live quota. And calling Tax on the wrong account is wrong accounting: a calculation leaves nothing behind at Stripe, but `Commit` does. If only one mode has a key, calculations in the other mode defer with `no_client_for_mode`.

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
