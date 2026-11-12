# How Hardcount is built

Hardcount bills people, so one thing comes first: **the money is right.**

These are the rules the code is built to. They fit on one screen. The evidence for each is further down, for anyone who wants to check.

## The money is right

1. **Once, or not at all.** A charge, an invoice or a credit entry never happens twice and is never lost, even through a crash or a retry.
2. **A notice is saved with its change.** A webhook is written in the same database transaction as the change it reports.
3. **The database enforces every rule it can.** A checker covers the rest.
4. **Stripe's answer decides what was paid.** A charge is recorded on the invoice before it is confirmed. If the result is unclear, Hardcount asks Stripe.
5. **Money math is exact.** Whole cents with their currency, never a float.
6. **An issued invoice or credit note never changes.** A correction is a new document.

## How we keep it simple

A money rule always wins over these. Within the list, the earlier one wins.

1. **Fail, don't guess.** A fact that cannot be read is an error, never a default.
2. **Fix it where it starts.** A fix that adds a guard or a flag says why the cause itself cannot be fixed.
3. **One of each.** One database, one path for each job, one word for each idea.
4. **Write it for a person.** Comments are few and say why. History lives in git.
5. **Build only what is needed.** Nothing is built ahead of a real need.

## Safe by default

- **Each tenant's data is fenced off by the database itself.** A control that cannot decide refuses.
- **Every release upgrades safely from the last one.** It never changes an invoice that already exists.

## How we know

- **Every change ships with a test.** A fix is proven by watching its test fail without the fix. [How we test](testing.md)
- **Money changes get more care.** They follow the [money-path playbook](dev/money-path-robustness-playbook.md).
- **Docs must not lie, and stay short.** [Writing style](dev/writing-style.md)
- **AI tools write much of the code. The maintainer owns every change.**

How the code itself is written is in the [code standard](dev/code-standard.md).

## Evidence

Everything below backs up a rule above.

### The money is right

1. **Once, or not at all.** Each charge, invoice and ledger entry is guarded by a stored key, a conditional update or a unique index. Take a billing period: every close locks the subscription row and checks that its period has not moved ([ADR-115](adr/115-one-closer-for-the-billing-period.md)), and a unique index refuses a second invoice for the same period. Card charges also send Stripe a stable idempotency key, so a retried request cannot charge twice. Tested by killing the billing process at five points: [Correctness under failure](benchmarks/failure-correctness.md).
2. **Saved with the change.** An event queued in a transaction that rolls back is not saved ([`outbox_integration_test.go`](../internal/webhook/outbox_integration_test.go)). CI fails if webhook code can queue an event outside the caller's transaction ([`webhook_emit_catalog_test.go`](../internal/arch/webhook_emit_catalog_test.go)). It also fails if an email is queued in a transaction of its own without being listed with its reason ([`email_outbox_standalone_test.go`](../internal/arch/email_outbox_standalone_test.go)). A webhook can arrive more than once, and each carries an ID so the receiver can ignore a repeat ([Webhooks](webhooks.md#three-rules-for-your-receiver)). Most emails are saved in the same transaction as their change. A few are sent after it, and retried until the invoice records that they went. One exception: a payment or dunning email that cannot be saved is logged as an error and skipped, and its change is still saved. [ADR-040](adr/040-outbox-always-on.md)
3. **The database enforces.** A rule that a constraint can express (CHECK, UNIQUE, a foreign key, a partial unique index) lives in the schema. The read-only checker (hardcount-doctor, 33 checks) covers what no constraint can, such as totals across tables and work that should have happened by now. CI runs it over demo data, and after every period close of a 13-month simulated billing run. [`cmd/hardcount-doctor`](../cmd/hardcount-doctor)
4. **Stripe's answer decides.** A charge creates its PaymentIntent (Stripe's object for one payment) unconfirmed, puts it on the invoice, and only then confirms it, which is when money moves. Nothing charges the invoice again until a webhook or a read at Stripe settles it. A PaymentIntent that was never confirmed is canceled at Stripe, so it can never take money, and the next attempt creates a new one. An operator can also record a payment made outside Stripe, but not while a card charge is at Stripe. [Card payment failure handling](payments.md) lists each case and the known gaps. Tests: [`two_step_charge_integration_test.go`](../internal/payment/two_step_charge_integration_test.go), [`two_step_safety_integration_test.go`](../internal/payment/two_step_safety_integration_test.go), [`two_step_review3_integration_test.go`](../internal/payment/two_step_review3_integration_test.go). [ADR-137](adr/137-charge-stamps-invoice-before-confirming.md)
5. **Exact math.** Rates are exact decimals. Billing dates are worked out in the tenant's time zone. The shared money helpers round half to even. [ADR-005](adr/005-integer-cents-for-money.md), [ADR-045](adr/045-decimal-per-unit-pricing-rates.md), [ADR-129](adr/129-credit-carries-its-own-currency.md), [ADR-058](adr/058-billing-date-math-tenant-timezone.md)
6. **Issued documents.** An invoice is finalized only once its amounts are final: one still waiting on tax stays a draft. A finalized invoice or issued credit note keeps the buyer and seller details it was issued with. The credit ledger only appends entries, except two running counters on each grant (`consumed_cents`, `cn_retired_cents`). [ADR-109](adr/109-issued-document-snapshot.md), [ADR-017](adr/017-tax-retry-and-auto-finalize.md), [ADR-004](adr/004-event-sourced-credit-ledger.md)

### How we keep it simple

- **Fail, don't guess.** If something a bill needs cannot be read, the bill stops with an error. A subscription that keeps failing is set aside for an operator, and the rest of the run goes on ([ADR-119](adr/119-billing-failure-record-keyed-to-the-due-instant.md)). Known gap: in test mode, when a test clock or its subscription cannot be read, billing still falls back to the current time.
- **Fix it where it starts.** A fix covers every place that touches the state. An extra mechanism needs a reason, written in its decision record or pull request. [Decision records](adr/README.md)
- **One of each.** All billing data lives in Postgres. Redis only counts requests for rate limiting, so several copies of the app share one count. No queue broker, warehouse or workflow engine is added until a measurement says Postgres is not enough. [How much traffic Hardcount handles](scale.md)
- **Build only what is needed.** Work that is not needed yet is listed with what would bring it back. [Roadmap: not building](roadmap.md#not-building)

### Safe by default

- Tenant data is read and written under row-level security: the database itself hides one tenant's rows from another. Staging and production refuse to start with a database role that can skip it (`checkRLSCapability` in [`cmd/hardcount/main.go`](../cmd/hardcount/main.go)). Code that skips it must be on a reviewed list.
- In production the API rate limiter refuses requests when Redis is down, and an idempotency key that cannot be reserved is refused, not ignored.
- The CI `upgrade` job installs the previous release, bills a month, upgrades on the same database and fails on any difference in any invoice ([`upgrade-from-previous-tag.sh`](../scripts/ci/upgrade-from-previous-tag.sh)). A change you must act on is listed under "Before upgrading" in the CHANGELOG. [Versioning](versioning.md)

### How we know

Go tests run against real Postgres for money and state, and Playwright for the dashboard. What only a real provider can show (Stripe, email, the PDF) is checked by hand before each release. Each published benchmark states how it was measured, on what hardware, and what the run does not show ([Sustained throughput](benchmarks/sustained-throughput.md)). Commits made with AI help say so.

## Checks that run on every change

These run on every pull request and every push to main.

| Check | What fails it |
|---|---|
| Tenant isolation | A broken row-level-security policy ([`rls_isolation_test.go`](../internal/platform/postgres/rls_isolation_test.go)); code that skips it without being listed ([`txbypass_allowlist_test.go`](../internal/arch/txbypass_allowlist_test.go)) |
| One auto-charge per invoice | Two claimers racing and both winning ([`auto_charge_claim_integration_test.go`](../internal/invoice/auto_charge_claim_integration_test.go)) |
| Package boundaries | A new import between domain packages that is not listed ([`boundaries_test.go`](../internal/arch/boundaries_test.go)) |
| Test-clock time | A bare `time.Now()` in a clock-aware package, unless the line says why ([`.golangci.yml`](../.golangci.yml)) |
| hardcount-doctor | Anything but a clean sweep after each period close of the 13-month run ([`clock_soak_e2e_test.go`](../internal/api/clock_soak_e2e_test.go)) |
| Money math | Generated cases for [billing dates](../internal/domain/billing_dates_property_test.go), [pricing](../internal/domain/pricing_property_test.go), [proration](../internal/subscription/proration_property_test.go), [tax apportionment](../internal/tax/apportionment_property_test.go) and the [credit waterfall](../internal/credit/waterfall_property_integration_test.go) |
| Upgrades | Any invoice that differs after the previous release is upgraded to the new code |

Two drills are run by hand. [`partition-drill.sh`](../scripts/partition-drill.sh) cuts the leader's link to Postgres and measures the takeover. [`restore-drill.sh`](../scripts/restore-drill.sh) runs backup, restore and a row-count check.

## Going deeper

- [Decision records](adr/README.md), including the reversed ones.
- What the benchmarks found wrong in Hardcount itself: [How much traffic Hardcount handles](scale.md#measured-runs).
- [Architecture](architecture.md): package layout and design rules.
