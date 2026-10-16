# Engineering: the evidence

The principles are one page: [ENGINEERING.md](ENGINEERING.md). This page is the detail behind them.

Velox is an open-source usage-based billing engine (Go + PostgreSQL). This page
is the short version of how it is engineered, for engineers who have not seen
the repo. It covers what is guaranteed, what was measured, what gates a change
that touches money, what was decided and reversed, and what the measurements
found wrong with Velox itself. Most claims link to the artifact behind them.

In short:

- **No double billing under crashes.** 0 duplicate and 0 lost invoices across
  five `SIGKILL` points. In a 2026-08-15 run with the index dropped and four
  billing engines racing in one test process, Velox billed 103 invoices for
  40 periods.
- **Throughput.** 12,000 events/sec at batch 10 (ten events per request),
  p99 22.6 ms, on a `db.m7g.4xlarge`. It passed 4 of 5 ten-minute repeats on
  stock settings and 5 of 5 with the WAL pool sized.
- **Two defects in Velox itself**, found by the benchmarks: #818 is fixed and
  #819 is open.

---

## 1. The guarantee: a row lock, then a database constraint

Two things stop a billing period being billed twice. First, every cycle close
takes a row lock on the subscription and checks that its period has not moved
([ADR-115](adr/115-one-closer-for-the-billing-period.md)); a writer that loses
the race writes nothing. Second, migration 0101 adds a partial unique index: a
uniqueness rule the database itself enforces on a subset of rows. With it, the
database cannot commit a second live cycle invoice for the same period, even if
the first check is bypassed:

```sql
CREATE UNIQUE INDEX idx_invoices_billing_idempotency
    ON invoices (tenant_id, subscription_id, billing_period_start, billing_period_end)
    WHERE status <> 'voided' AND source_plan_changed_at IS NULL;
```

A live cycle invoice is a normal billing-period invoice. The WHERE clause above
excludes voided invoices and plan-change invoices. If a period already has a
live cycle invoice, the database cannot commit a second one.

The negative control below removes the index and shows the race billing twice,
which proves the measurement can see failure. It was run on 2026-08-15, before
ADR-115 added the period-close lock, so it shows what the index alone was
guarding at that time. It has not been re-run on the current code.
([failure-correctness.md](benchmarks/failure-correctness.md))

## 2. What was measured

- **[Correctness under failure](benchmarks/failure-correctness.md).** A real
  billing process (the leader) is killed with `SIGKILL` mid-run at five
  different points. Each point is chosen by watching the database state, not
  by a timer. A second process then runs the same cycle. With 40
  subscriptions, one period each and $1,000 in total, every kill point gave
  **0 duplicate invoices, 0 lost invoices, 0 cents of drift**.
  - Negative control (2026-08-15, before ADR-115): four billing engines
    racing as goroutines in one test process, with the index dropped. It billed **103 invoices for 40 periods, $2,575.00 instead
    of $1,000.00, and every leader reported success**. A second run of it
    billed 129 invoices, $3,225.00. Nothing in the application layer noticed.
- **[Sustained throughput](benchmarks/sustained-throughput.md)** on a
  `db.m7g.4xlarge`. A rate counts as passing a repeat only when that repeat
  meets its gate on the throughput page.

| Setting | Result |
|---|---|
| Stock RDS settings | **12,000 events/sec at batch 10 (p99 22.6 ms)** and **15,000 at batch 100 (p99 43.8 ms)**, each passing 4 of 5 ten-minute repeats |
| Cause of the tail stalls | A third, instrumented run caught them live: WAL-segment creation when RDS's recycled-segment pool runs dry |
| WAL pool sized: `min_wal_size = max_wal_size = 16 GB` (two dynamic settings, no reboot) | Both rates run **5 of 5**: 12,000 with a worst 10-second p99 of 52 ms; 15,000 at p99 40.2–42.8 ms |

Every event is reconciled against Postgres. A run whose sent count does not
match the rows stored is discarded rather than reported. `pgbench` on the same
row shape supplies the baseline for what the hardware itself can do. Stalls the
WAL mechanism does not explain are named as unexplained rather than left out.

Both pages carry a *What this does not show* section, and the throughput page
keeps a table of its own superseded figures. The retractions are part of the
result.

## 3. Two protocols gate money-path work

- **[Money-path robustness playbook](dev/money-path-robustness-playbook.md):
  how to build.** The rule: before writing a line, list the state's complete
  site-set. That is every place that writes the state, fires an effect from
  it, reads it behind a guard, calls into it, or can crash mid-change. The
  playbook has twelve gates (yes/no checklist questions), and a "no" blocks
  the PR.
  - Why it exists: one PR ([#325](https://github.com/getvelox/velox/pull/325)),
    a dunning change (dunning is the automatic retrying and reminding for an
    unpaid invoice). It was reviewed four times. Each round caught a
    *different* instance of the *same* root problem, and each fix exposed the
    next. Reviewing harder was not the answer. The cause was reasoning
    *locally* about the function in the diff, when the real surface was the
    whole state machine.
- **[Manual-test strategy](dev/manual-test-strategy.md): how to prove.** "I
  clicked it and nothing broke" is not a test result here.
  - Every flow is checked from five angles: behavior, money math, honest UI
    copy, design, and what the screen actually looks like.
  - Rules say what must happen before a test checkbox may be ticked.
  - The list of testing techniques has one admission rule: a technique is
    listed only if it once caught a real bug in this codebase. Where a
    technique cites a PR, that PR records the bug it caught.

  The strategy documents current practice, not aspirations.

## 4. Three decisions, one reversal, and one thing the monitoring missed

- **PaymentIntent-only Stripe** ([ADR-001](adr/001-paymentintent-only-stripe.md)).
  Velox owns invoices, dunning and the payment lifecycle end-to-end. Stripe
  executes the card charge as a plain PaymentIntent; Velox creates no Stripe
  Billing objects. Why: Stripe Billing adds a percentage fee, and using it would mean
  two systems tracking the same invoices.
- **Coupons cut after shipping** ([ADR-039](adr/039-cut-coupons-pre-launch.md)).
  A full coupon surface shipped in a Stripe-parity sprint. It was deleted once
  it was clear nobody had asked for it. The credit ledger is the discount
  primitive.
- **Splitting the billing timezone from the display timezone: designed and
  deliberately not built**
  ([ADR-092](adr/092-split-billing-timezone-from-display.md)). The split was
  prototyped and adversarially reviewed. It was then recorded, not built,
  together with a named condition that would trigger building it. The
  existing fix that absorbs the billing-period seam when the single org
  timezone changes
  ([ADR-091](adr/091-org-timezone-change-seam-absorb.md)) already closes the
  only actual defect.
- **One reversal** ([ADR-074](adr/074-subscription-billing-timezone-snapshot.md) →
  [ADR-077](adr/077-org-level-billing-timezone.md)). A per-subscription
  timezone snapshot shipped and was then deleted. The organization is the unit
  a timezone attaches to, and the snapshot caused a class of about eight
  display bugs. The superseded ADR records what was wrong with it.
- **What the monitoring missed.** `velox-doctor` is a read-only command that
  scans a Velox database for states no correct code could produce. It swept
  the deliberately sabotaged negative-control database from the correctness
  benchmark, which held 129 invoices where 40 should exist. At that time the
  28th check did not yet exist, and all **27 checks reported zero violations**
  ([failure-correctness.md](benchmarks/failure-correctness.md)). It has 30
  checks now. Its other limit: the sweep must run on an admin connection. Under a
  row-level-security-scoped role it sees zero rows and reports clean.

## 5. The benchmarks found two defects, and both are ours

Both are filed against Velox, with the evidence, beside the numbers they
affect. One is fixed and re-measured; one is open.

- **[#818](https://github.com/getvelox/velox/issues/818): a hot row, not the
  hardware.** Every request updated `last_used_at` on the API key it
  authenticated with. One high-volume client is one row. A row lock hands off
  at about one round trip (~1.75 ms on this rig), so one API key was capped at
  **~570 requests/s, whatever the instance size**. It was found by sampling `pg_stat_activity` during a rate
  that would not hold: 50–55 of 61 backends were waiting on that one `UPDATE`.
  Fixed and re-measured on the rig at batch 10 (ten events per request):
  - The knee, where the latency curve bends upward, moved from ~570 req/s to
    between 1,600 and 2,000.
  - 12,000 ev/s now holds at p99 22.6 ms (4 of 5 stock repeats; 5 of 5 with
    the WAL pool sized as above).
- **[#819](https://github.com/getvelox/velox/issues/819): a linear scan.** The
  per-customer usage summary is a `COUNT + SUM GROUP BY meter` over the
  customer's rows with no rollup. It costs about **2.7 µs per event**, so above
  ~180k events per customer per month it misses a 500 ms read budget, whatever
  the write rate. Filed with the number; not yet fixed. This is why the read
  gate is reported separately from the ingest gate. At 10,000 ev/s, ingest held
  and the summary missed its budget, and both are true.

---

## The disciplines

Velox moves money, so correctness is the product, not a feature. The disciplines that show up in the code:

- **Money-path changes follow a written protocol, not judgment.** Any change to an invoice, payment, credit, or state machine goes through the [money-path robustness playbook](dev/money-path-robustness-playbook.md). Before writing a line, you enumerate the state's *complete* site-set: every writer, effect-firer, gated reader and crash point. Local reasoning is exactly how money bugs ship.
- **Invariants are enforced by machines, not convention.**
  - Tenant isolation is Postgres RLS, proven by tests that fail if a query escapes its tenant ([`rls_isolation_test.go`](../internal/platform/postgres/rls_isolation_test.go)).
  - Exactly-once auto-charge is a compare-and-swap claim: when two claimers race, exactly one wins ([`auto_charge_claim_integration_test.go`](../internal/invoice/auto_charge_claim_integration_test.go)).
  - A new cross-domain import fails the architecture test until justified in an allowlist ([`boundaries_test.go`](../internal/arch/boundaries_test.go)).
  - A bare `time.Now()` in the packages that handle clock-pinned entities (those whose time comes from a test clock) fails golangci-lint unless the line says why it needs wall-clock time (a `forbidigo` rule in [`.golangci.yml`](../.golangci.yml)).
  - A **money-invariant doctor** (`cmd/velox-doctor`) runs 30 read-only checks for states no legal writer can produce. It must run as a database owner or admin; under the app's RLS role it sees no rows. It runs in CI over the data the golden-path demo writes through the real API. It also runs inside a 13-month billing soak ([`clock_soak_e2e_test.go`](../internal/api/clock_soak_e2e_test.go)), which closes a subscription month thirteen times through the real server and demands a clean sweep after every close.
  - The rule behind all of these: if a mistake can recur, a machine catches the next one.
- **Failure modes are measured, not asserted.**
  - "Crash-safe" and "idempotent" are the two easiest things in billing to claim and the two hardest to check. So both are published as runs with a negative control, not as design notes. See [Benchmarks](scale.md#published-runs).
  - Where the money math has more cases than anyone can enumerate by hand, the tests generate them: billing dates ([`internal/domain/billing_dates_property_test.go`](../internal/domain/billing_dates_property_test.go)), pricing ([`internal/domain/pricing_property_test.go`](../internal/domain/pricing_property_test.go)), proration ([`internal/subscription/proration_property_test.go`](../internal/subscription/proration_property_test.go)), tax apportionment ([`internal/tax/apportionment_property_test.go`](../internal/tax/apportionment_property_test.go)), and the credit waterfall ([`internal/credit/waterfall_property_integration_test.go`](../internal/credit/waterfall_property_integration_test.go)).
  - The operational paths that only ever fail in production are drilled on purpose. [`scripts/partition-drill.sh`](../scripts/partition-drill.sh) cuts a real network link between the leader and Postgres and measures how long the leader role stays held (9 s on the lease, per [failure-correctness.md](benchmarks/failure-correctness.md)). [`scripts/restore-drill.sh`](../scripts/restore-drill.sh) runs the whole backup → restore → row-count-validate loop against an ephemeral Postgres. [`scripts/migration-safety-test.sh`](../scripts/migration-safety-test.sh) replays the migration set against a populated database, to catch the lock a migration would take at scale.
- **Real Postgres behind the unit tests.** Some unit tests use in-memory fakes for speed. The stores, migrations and RLS policies are also tested against real Postgres, and CI runs those tests on every pull request and every push to main. That includes concurrent-claimer collision tests and mutation-verified assertions (break the logic on purpose; the test must fail). The repo has about 132k lines of Go test code and about 102k lines of hand-written production Go.
- **Decisions are written down, including the reversals.** [110+ ADRs](adr/) record the load-bearing calls and the reversals. For example, a per-subscription timezone snapshot was built, shipped, then *deleted* once org-level proved the complete abstraction (ADR-074 → 077). When a design keeps spawning guard machinery, the model is treated as wrong, not the guards as missing.
- **Audited like production, pre-launch.** A [117-finding end-to-end audit](dev/audit-2026-07-02-full-product.md) was worked through in 16 PRs ([plan](dev/audit-2026-07-02-remediation-plan.md)). Every high and medium finding is fixed or has a named deferral; about 41 low findings remain in the backlog. An [HA-readiness audit](dev/ha-readiness-2026-07-06.md) names every single-point-of-failure before the word "production" gets used.

---

Deeper: [`docs/adr/`](adr/) (110+ decision records, including the ones that were
reversed) · [architecture](architecture.md) · [the invariants machines
enforce](#the-disciplines) · [self-hosting](self-host.md) ·
[Postgres requirements](ops/postgres-requirements.md)
