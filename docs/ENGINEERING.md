# How Velox is engineered

Velox moves money, so these are the rules the code is built to. Each rule
comes with its proof and a link to where you can check it.

## Money

- **A billing period is billed once.** Every cycle close locks the subscription row and checks that its period has not moved ([ADR-115](adr/115-one-closer-for-the-billing-period.md)). Behind that, a unique index in the database refuses a second live cycle invoice for the same period, so a crash or two leaders racing cannot bill it twice. Other money writes use their own dedup index or a conditional update. Card charges also send Stripe a stable idempotency key.
  **Proof:** a billing process killed with `SIGKILL` at five points left 0 duplicate and 0 lost invoices. With the index dropped, four racing engines billed 103 invoices for 40 periods, so the test can see a failure (run on 2026-08-15, before ADR-115). [Correctness under failure](benchmarks/failure-correctness.md)
- **Constraints first, then a checker.** Database constraints block the bad states they can express. For rules that span rows, a read-only checker (velox-doctor, 30 checks) finds bad rows after they are written.
  **Proof:** CI runs it over the golden-path demo data, and the 13-month clock soak test needs a clean sweep after every billing close. [`cmd/velox-doctor`](../cmd/velox-doctor)
- **Fail loudly.** If a read that a bill needs fails, the bill stops with an error; it does not bill a default. A subscription that keeps failing is parked for an operator. One exception, in test mode only: if a test clock cannot be read, billing uses the current time and logs a warning.
  **Proof:** [ADR-119](adr/119-billing-failure-record-keyed-to-the-due-instant.md)
- **For card charges, Stripe's record decides what was paid.** A charge with an unclear result is held until a webhook or Velox's list of the customer's payments in Stripe settles it. When Stripe shows no payment 24 hours after the attempt, the invoice is charged again. An operator can also record a payment made outside Stripe.
  **Proof:** [Card payment failure handling](payments.md) lists each case and the known gaps; [ADR-107](adr/107-unknown-is-terminal-until-a-human.md)

## Simplicity

- **One Postgres.** All billing data lives there; Redis is used for rate limiting only. No queue broker, warehouse or workflow engine until a measurement says Postgres is not enough.
  **Proof:** one `db.m7g.4xlarge` took 12,000 usage events a second at p99 22.6 ms, in 4 of 5 ten-minute runs. [How much traffic Velox handles](scale.md)
- **Prefer one path for each job.** A second path for the same job is where bugs have hidden.
  **Proof:** a per-subscription timezone snapshot shipped and was then removed ([ADR-074](adr/074-subscription-billing-timezone-snapshot.md) → [ADR-077](adr/077-org-level-billing-timezone.md)).
- **The smallest correct change first.** An extra mechanism needs a reason: the defect it prevents, written in its ADR or PR. [Decision records](adr/README.md)

## Proof

- **Code changes ship with an automated test at the right layer.** Go tests against real Postgres for money and state, Playwright for the dashboard. A fix is proven by watching its test fail without the fix. What only a real provider can show (Stripe, email, the PDF) is checked by hand before each release. [How we test](testing.md)
- **Money changes get the full process.** Changes to money or a billing state machine (invoices, payments, credits, dunning, subscriptions, tax) follow the money-path playbook: list every place that touches the state before writing, then pass twelve yes/no gates. It asks for tests against real Postgres and for breaking the fix once on purpose, to show the test fails. Other changes get a normal review. [Money-path playbook](dev/money-path-robustness-playbook.md)
- **Benchmarks come with their method.** Each published benchmark states its method, its hardware and what the run does not show, next to the number. A run whose sent count does not match the rows stored is discarded. [Sustained throughput](benchmarks/sustained-throughput.md)

## Checks CI runs

These run on every pull request and every push to main:

- **Tenant isolation:** tests fail if a query reaches another tenant's rows ([`rls_isolation_test.go`](../internal/platform/postgres/rls_isolation_test.go)).
- **One auto-charge per invoice:** when two claimers race, exactly one wins ([`auto_charge_claim_integration_test.go`](../internal/invoice/auto_charge_claim_integration_test.go)).
- **Package boundaries:** a new import between domain packages fails until it is allowlisted ([`boundaries_test.go`](../internal/arch/boundaries_test.go)).
- **Test-clock time:** a bare `time.Now()` in clock-aware packages fails lint unless the line says why ([`.golangci.yml`](../.golangci.yml)).
- **velox-doctor:** a clean sweep after each close of a 13-month billing soak ([`clock_soak_e2e_test.go`](../internal/api/clock_soak_e2e_test.go)).
- **Generated cases for the money math:** property tests for [billing dates](../internal/domain/billing_dates_property_test.go), [pricing](../internal/domain/pricing_property_test.go), [proration](../internal/subscription/proration_property_test.go), [tax apportionment](../internal/tax/apportionment_property_test.go) and the [credit waterfall](../internal/credit/waterfall_property_integration_test.go).

Two drills are run by hand: [`partition-drill.sh`](../scripts/partition-drill.sh) cuts the leader's link to Postgres and measures the takeover (9 s), and [`restore-drill.sh`](../scripts/restore-drill.sh) runs backup, restore and a row-count check.

## Going deeper

- [Decision records](adr/README.md), including the reversed ones.
- The two defects the benchmarks found in Velox itself (#818, fixed; #819, open): [How much traffic Velox handles](scale.md#measured-runs).
- [Architecture](architecture.md): package layout and design rules.
