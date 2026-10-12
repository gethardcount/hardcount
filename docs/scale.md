# Will it take our volume?

Velox runs on one Postgres. This page says where that stops being enough, what has been measured, and what comes before adding a dependency.

## Published runs

Two runs are published in full: method, gates, evidence, and what each one does *not* show.

**[Correctness under failure](benchmarks/failure-correctness.md)**: what happens to your invoices when the billing process dies mid-run.

- **Scenarios.** A leader (the process elected to run the billing cycle) is `SIGKILL`ed at five kill points. The kill points are chosen by watching the database, not by sleeping. Four leaders race the same cycle at once. A partition drill severs a real network link to time the takeover.
- **Result.** **0 duplicate invoices, 0 lost invoices, 0 cents of drift.** The money-invariant doctor is clean after every scenario.
- **Negative control.** This is what makes the result a measurement rather than a claim. Drop `idx_invoices_billing_idempotency`, and the same run bills **103 invoices for 40 periods, $2,575 against $1,000 of real periods, with every leader reporting success.**
- **Reproduce.** From a clean checkout with Docker and two `go test` commands.

**[Sustained throughput](benchmarks/sustained-throughput.md)**: what the ingest path holds on named AWS hardware, with every event reconciled against Postgres.

- A run whose sent count does not match the rows stored is discarded rather than reported.
- The headline figures and the capacity cliffs are in [Will it take our volume?](#will-it-take-our-volume) below.
- The doc adds the method, the `pgbench` control denominator, and the *What this does not show* section.
- It also gives the closed-loop ceilings. Each sender waits for a response before sending again, so these are maxima, not service levels.
- It states the two defects the runs found in Velox itself beside the numbers, rather than fixing them quietly. Both are linked with their numbers in [the volume section](#will-it-take-our-volume).

## Will it take our volume?

It is a fair question, and the answer has three parts.

**Where the Postgres-only ceiling actually is.**

Lago is the closest comparable, and it *does* ship a Kafka + ClickHouse tier. It routes everything under **10,000 events/sec** to its ordinary REST API, batched above ~1,000/s. It only recommends streaming above that, noting that *"Many customers start on REST and switch to Kafka only when they outgrow it."*

Their 10,000/sec reference point is one self-hosted deployment they describe but don't name. To their credit, they publish the whole story rather than only the flattering half: *"A major global payments company runs Lago self-hosted, processing thousands of transactions per second. They started on Postgres (validated at 10K events/sec) and later migrated to ClickHouse + Kafka for higher throughput …"* ([source](https://docs.getlago.com/guide/events/ingesting-usage), verified 2026-08-17).

So 10k/sec is where a Postgres-first billing stack stops being obviously sufficient, not where it stops working. Ten thousand a second is roughly 26 billion events a month. That figure is theirs, not ours. What Velox itself has measured is below.

**What we have actually measured, and what we haven't.**

Conditions: AWS, one AZ, the live-mode path, 200 customers, and every event reconciled against the database.

| Instance | Load | p99 | Held |
|---|---|---|---|
| `db.m7g.2xlarge` (32 GB) | **1,000 events/sec** | **8.2 ms** | **across five 10-minute repeats** |
| same instance | 5,000 ev/s | 51 ms | until the table's index working set (~60M rows, 30 GB) outgrew the instance: a capacity cliff stated with its numbers |
| `db.m7g.4xlarge` (64 GB), after fixing the hot row the first run found ([#818](https://github.com/getvelox/velox/issues/818)) | **12,000 ev/s at batch 10 (1,200 requests/s)** | **22.6 ms** | each 4 of 5 ten-minute repeats |
| same instance, same run | **15,000 ev/s at batch 100** | **43.8 ms** | same as the row above |
| same instance, third instrumented run, WAL segment pool sized as the runbook now says | 12,000 ev/s | worst 10-second p99 of 52 ms | **5 of 5** |

- The third run caught the tail stalls live: WAL segment creation when RDS's recycled-segment pool runs dry.
- The runs also found the per-customer usage summary that scans linearly ([#819](https://github.com/getvelox/velox/issues/819)). It is stated beside the numbers.
- [`docs/benchmarks/sustained-throughput.md`](benchmarks/sustained-throughput.md) carries the method, the gates, the evidence files, the closed-loop ceilings and pgbench denominators.
- It also has a plain list of what was *not* tested: steady traffic only, 10-minute windows, single AZ.
- The whole thing reproduces with one command from `scripts/bench-rig/`.

**The ladder, which stays boring for a long time.** Before Velox needs a new dependency, these steps come first:

1. Use the batch endpoint. One database commit spreads the write cost across up to 1,000 events.
2. Add API replicas. Ingest is stateless, so this should help, but it has not been measured.
3. Partition `usage_events` by month.
4. Set a retention window on raw events.
5. Move analytics to a read replica.

Each rung is ordinary Postgres operations. A columnar store only earns its place in two cases: you want arbitrary slicing over years of raw events, or sustained ingest well past the figure above. At that point it belongs beside Velox as a read-side sidecar, not underneath it. Money never leaves Postgres.

**And if you already run Kafka, keep it.** Velox does not want to own your transport. Point a consumer at the batch ingest endpoint, and your existing pipeline feeds it directly. Velox is deliberately the last mile: rating, invoicing, credits, dunning, collection.

One structural note makes all of the above easier than it looks. Velox scales as many separate deployments, not one shared cluster. Every company running Velox runs its own deployment, carrying only its own volume. So the aggregate pressure that forces a shared SaaS platform onto Kafka never accumulates in any single instance.
