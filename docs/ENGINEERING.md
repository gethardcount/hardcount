# How Hardcount is engineered

Hardcount moves money, so these are the rules the code is built to. Each rule
comes with its proof and a link to where you can check it. How the code itself
is written is in the [code standard](dev/code-standard.md).

## Money

- **Every money effect lands once, or not at all.** Retries are expected, so each charge, invoice and ledger entry is guarded by a stored key, a conditional update or a unique index. Take a billing period. Every cycle close locks the subscription row and checks that its period has not moved ([ADR-115](adr/115-one-closer-for-the-billing-period.md)). Behind that, a unique index refuses a second live cycle invoice for the same period, so a crash or two leaders racing cannot bill it twice. Card charges also send Stripe a stable idempotency key, which covers Stripe's own retry window.
  **Proof:** a billing process killed with `SIGKILL` at five points left 0 duplicate and 0 lost invoices. With the index dropped, four racing engines billed 103 invoices for 40 periods, so the test can see a failure (run on 2026-08-15, before ADR-115). [Correctness under failure](benchmarks/failure-correctness.md)
- **Webhooks are sent at least once.** Each webhook event is saved in the same transaction as the change it reports, so a crash cannot drop it, and a change whose event cannot be saved fails. Each event carries an ID the receiver can use to drop a repeat ([Webhooks](webhooks.md#three-rules-for-your-receiver)). The emails Hardcount sends on its own are saved with their change too, or sent again until a marker on the invoice records them. A payment or dunning email that cannot be saved is logged as an error and skipped, and its change is saved without it.
  **Proof:** against real Postgres, an event queued in a transaction that rolls back is not saved ([`outbox_integration_test.go`](../internal/webhook/outbox_integration_test.go)). CI fails if the webhook package gains an exported function or method that takes an event type but not the caller's transaction ([`webhook_emit_catalog_test.go`](../internal/arch/webhook_emit_catalog_test.go)). It also fails on an email queued in a transaction of its own, unless that send is listed with its reason ([`email_outbox_standalone_test.go`](../internal/arch/email_outbox_standalone_test.go)). [ADR-040](adr/040-outbox-always-on.md)
- **Constraints first, then a checker.** A rule that a database constraint can express (CHECK, UNIQUE, a foreign key, a partial unique index) lives in the schema. A read-only checker (hardcount-doctor, 33 checks) covers what no constraint can, such as sums across tables and work that should have happened by now.
  **Proof:** CI runs it over the golden-path demo data, and the 13-month clock soak test needs a clean sweep after every billing close. [`cmd/hardcount-doctor`](../cmd/hardcount-doctor)
- **Fail loudly.** If a read that a bill needs fails, the bill stops with an error; it does not bill a default. A subscription that keeps failing is parked for an operator, and the rest of the run goes on. One exception, in test mode only: if a test clock cannot be read, billing uses the current time and logs a warning.
  **Proof:** [ADR-119](adr/119-billing-failure-record-keyed-to-the-due-instant.md)
- **For card charges, Stripe's record decides what was paid.** A charge with an unclear result is held until a webhook or Hardcount's list of the customer's payments in Stripe settles it. When Stripe shows no payment 24 hours after the attempt, the invoice is charged again. An operator can also record a payment made outside Stripe.
  **Proof:** [Card payment failure handling](payments.md) lists each case and the known gaps; [ADR-127](adr/127-a-lost-charge-result-is-resolved-by-asking-stripe.md)
- **Money math is exact.** Amounts are stored as integer cents and per-unit rates as exact decimals, never as floats. Every amount carries its currency, the shared money helpers round half to even, and billing dates are computed in the tenant's time zone.
  **Proof:** [ADR-005](adr/005-integer-cents-for-money.md), [ADR-045](adr/045-decimal-per-unit-pricing-rates.md), [ADR-129](adr/129-credit-carries-its-own-currency.md), [ADR-058](adr/058-billing-date-math-tenant-timezone.md), and the property tests listed under "Checks CI runs".
- **An issued document doesn't change.** An invoice is finalized only once its amounts are final: an invoice still waiting on tax stays a draft. A finalized invoice or issued credit note keeps the buyer and seller details it was issued with, and a correction is a credit note or a void, not an edit. The credit ledger only appends entries, except two running counters on each grant (`consumed_cents`, `cn_retired_cents`).
  **Proof:** [ADR-109](adr/109-issued-document-snapshot.md), [ADR-017](adr/017-tax-retry-and-auto-finalize.md), [ADR-004](adr/004-event-sourced-credit-ledger.md)

## Safety

- **Secure by default, and fail closed.** Tenant data is read and written under row-level security. Staging and production refuse to start with a database role that can bypass it, and code that skips it (`TxBypass`) must be on a reviewed list. A control that cannot decide refuses rather than allows: in production the API rate limiter refuses requests when Redis is down, and an idempotency key that can't be reserved is refused, not ignored.
  **Proof:** the RLS and `TxBypass` checks under "Checks CI runs"; the boot check in [`cmd/hardcount/main.go`](../cmd/hardcount/main.go) (`checkRLSCapability`)
- **Every release upgrades safely from the last one.** A release must not change any invoice that existed before it, and a change you must act on is listed under "Before upgrading" in the CHANGELOG.
  **Proof:** the CI `upgrade` job installs the previous release, bills a month, upgrades to the new code on the same database and fails on any difference in any invoice ([`upgrade-from-previous-tag.sh`](../scripts/ci/upgrade-from-previous-tag.sh)). [Versioning](versioning.md)

## Simplicity

- **One Postgres.** All billing data lives there; Redis is used for rate limiting only, so several copies of the app share one count. No queue broker, warehouse or workflow engine until a measurement says Postgres is not enough.
  **Proof:** one `db.m7g.4xlarge` took 12,000 usage events a second at batch 10 (ten events per request), p99 22.6 ms, in 4 of 5 ten-minute runs on stock settings. [How much traffic Hardcount handles](scale.md)
- **Prefer one path for each job.** A second path for the same job is where bugs have hidden.
  **Proof:** a per-subscription timezone snapshot shipped and was then removed ([ADR-074](adr/074-subscription-billing-timezone-snapshot.md) → [ADR-077](adr/077-org-level-billing-timezone.md)).
- **The smallest change that is correct everywhere.** A fix covers every place that touches the state, and an extra mechanism needs a reason: the defect it prevents, written in its ADR or PR. [Decision records](adr/README.md)
- **Build from evidence.** A feature or scale mechanism waits for a real need: it has happened, or a user needs it now. Work that isn't needed yet is listed with what would bring it back. [Roadmap: not building](roadmap.md#not-building)

## Proof

- **Code changes ship with an automated test at the right layer.** Go tests against real Postgres for money and state, Playwright for the dashboard. A fix is proven by watching its test fail without the fix. What only a real provider can show (Stripe, email, the PDF) is checked by hand before each release. [How we test](testing.md)
- **Rigor matches how hard a mistake is to undo.** Changes to money or a billing state machine (invoices, payments, credits, dunning, subscriptions, tax) follow the money-path playbook: list every place that touches the state before writing, then pass twelve yes/no gates. It asks for tests against real Postgres and for breaking the fix once on purpose, to show the test fails. Other changes get a normal review. [Money-path playbook](dev/money-path-robustness-playbook.md)
- **Benchmarks come with their method.** Each published benchmark states its method, its hardware and what the run does not show, next to the number. A run whose sent count does not match the rows stored is discarded. [Sustained throughput](benchmarks/sustained-throughput.md)
- **Docs must not lie, and stay short.** A doc changes in the same PR as the behavior it describes, and a CI job checks the links between docs. [Writing style](dev/writing-style.md)
- **AI tools write much of the code; the maintainer owns every change.** The maintainer decides what is built and answers for each change. Commits made with AI help say so in their trailer.

## Checks CI runs

These run on every pull request and every push to main:

- **Tenant isolation:** tests check the RLS policies ([`rls_isolation_test.go`](../internal/platform/postgres/rls_isolation_test.go)). Code that skips RLS (`TxBypass`) fails a test until it is listed ([`txbypass_allowlist_test.go`](../internal/arch/txbypass_allowlist_test.go)).
- **One auto-charge per invoice:** when two claimers race, exactly one wins ([`auto_charge_claim_integration_test.go`](../internal/invoice/auto_charge_claim_integration_test.go)).
- **Package boundaries:** a new import between domain packages fails until it is allowlisted ([`boundaries_test.go`](../internal/arch/boundaries_test.go)).
- **Test-clock time:** a bare `time.Now()` in clock-aware packages fails lint unless the line says why ([`.golangci.yml`](../.golangci.yml)).
- **hardcount-doctor:** a clean sweep after each close of a 13-month billing soak ([`clock_soak_e2e_test.go`](../internal/api/clock_soak_e2e_test.go)).
- **Generated cases for the money math:** property tests for [billing dates](../internal/domain/billing_dates_property_test.go), [pricing](../internal/domain/pricing_property_test.go), [proration](../internal/subscription/proration_property_test.go), [tax apportionment](../internal/tax/apportionment_property_test.go) and the [credit waterfall](../internal/credit/waterfall_property_integration_test.go).
- **Upgrades:** the previous release is installed, upgraded to the new code, and every invoice is compared ([`upgrade-from-previous-tag.sh`](../scripts/ci/upgrade-from-previous-tag.sh)).

Two drills are run by hand: [`partition-drill.sh`](../scripts/partition-drill.sh) cuts the leader's link to Postgres and measures the takeover (9 s), and [`restore-drill.sh`](../scripts/restore-drill.sh) runs backup, restore and a row-count check.

## Going deeper

- [Decision records](adr/README.md), including the reversed ones.
- The two defects the benchmarks found in Hardcount itself (#818, fixed; #819, open): [How much traffic Hardcount handles](scale.md#measured-runs).
- [Architecture](architecture.md): package layout and design rules.
