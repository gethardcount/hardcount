# How much traffic Velox handles

Velox keeps all its billing data in one Postgres database. In production it
also needs Redis, which it uses only for rate limits. This page answers one
question: how many usage events per second can that database take, and what
do you do when you need more?

A usage event is one record of something a customer used, such as 1,200 input
tokens on one model. Think of the database as a road: we measured how many
cars it carries before traffic slows, and which part wears out first.

## Short answer

1. **1,000 events per second** on a mid-size AWS database (`db.m7g.2xlarge`,
   32 GB memory), with 10 events in each request. It passed all five
   10-minute runs.
2. **12,000 events per second** on a larger one (`db.m7g.4xlarge`, 64 GB
   memory), with 10 events in each request. It passed all five runs after one
   Postgres setting change ([WAL pool size](failure-modes/rfm-001-wal-pool-exhaustion.md)),
   and four of five without it.
3. The database was the limit every time, never the Velox process: first its
   memory, then how fast its disk could write.

One small VM takes far less than these figures.

**Need more?** In this order: send events in batches (up to 1,000 per
request), give the database more memory, then give it faster disks. The
[full list](#if-you-need-more) has the later steps.

## Measured runs

Conditions: AWS `ap-south-1`, one availability zone, a 100 GB `gp3` disk (AWS's
general-purpose SSD) and stock database settings unless a row says otherwise.
The load was one tenant, one meter and 200 customers. Requests were sent at a
fixed rate whether or not the server kept up. Each row is a series of
10-minute runs, and every event was counted in the database afterwards.

p99 means 99 of every 100 requests finished within that time.

| Database | Load | p99 | Runs passed |
|---|---|---|---|
| `db.m7g.2xlarge` (32 GB) | 1,000 events/s, 10 per request | 8.2 ms | 5 of 5 |
| `db.m7g.2xlarge` (32 GB) | 5,000 events/s, 100 per request | 51 ms, then 116 ms, then 9.9 s | 1 of 3 |
| `db.m7g.4xlarge` (64 GB) | 12,000 events/s, 10 per request | 22.6 ms | 4 of 5 |
| `db.m7g.4xlarge` (64 GB) | 15,000 events/s, 100 per request | 43.8 ms | 4 of 5 |
| `db.m7g.4xlarge`, WAL pool sized | 12,000 events/s, 10 per request | 52 ms in the worst 10 seconds | 5 of 5 |
| `db.m7g.4xlarge`, WAL pool sized | 15,000 events/s, 100 per request | 40.2–42.8 ms | 5 of 5 |

- **The 5,000 row slowed down as the table grew.** Once the table and its
  indexes (about 60 million rows, about 30 GB) no longer fit in the 32 GB of
  memory, each run was slower than the one before.
- **The WAL pool rows.** WAL is the log Postgres writes before every change.
  On RDS, after a quiet spell, the stock of ready-made log files can run out,
  and every write waits while a new one is made. The third run caught this
  live. [The fix is one setting](failure-modes/rfm-001-wal-pool-exhaustion.md).
- **Two problems the runs found in Velox itself.** The first runs found one
  database row that every request updated (the API key's last-used time)
  ([#818](https://github.com/getvelox/velox/issues/818)). It was fixed before
  the `db.m7g.4xlarge` rows above. The per-customer usage summary reads every
  event one by one ([#819](https://github.com/getvelox/velox/issues/819)). It
  is still open.
- **What was not tested:** traffic spikes (the load was steady), runs longer
  than 10 minutes, and more than one availability zone.
- **Method and evidence:** [sustained throughput](benchmarks/sustained-throughput.md)
  has the gates, the evidence files, the most a closed loop could push, and
  what the database alone does on the same rows (`pgbench`).
- **Run it yourself:** `scripts/bench-rig/run.sh` runs the whole sequence on
  your own AWS account, which is billed. To get the rows above, set `DB_CLASS`
  and `CONFIGS` to match them. For the WAL pool rows, also set the WAL settings
  through `RDS_PARAMS`.

## If you need more

These steps come before Velox needs any new dependency:

1. Use the batch endpoint. It writes the whole batch in one database commit,
   with up to 1,000 events or 1 MB per request, whichever comes first.
2. Give the database more memory, then faster disks. This is what the runs
   above ran out of.
3. Add API copies. Ingest keeps no state in the Velox process, so this should
   help, but it has not been measured.
4. Split `usage_events` by month (partitioning). Not built. It needs a Velox
   migration, because the idempotency-key unique rule must then include the
   month.
5. Set a retention window on raw events. Not built.
6. Move analytics to a read replica. Not built. Velox has no reader
   connection setting today.

Each step uses tools Postgres already has. Steps 4 to 6 also need changes in
Velox that are not built yet. A columnar store only earns its place in two
cases: you want to slice years of raw events any way you like, or you need
sustained ingest well past the figures above. At that point it belongs beside
Velox as a read-side add-on, not underneath it. Money never leaves Postgres.

**If you already run Kafka, keep it.** Velox does not want to own your
transport. Point a consumer at the batch ingest endpoint, and your existing
pipeline feeds it directly. Velox is the last mile: rating, invoicing,
credits, dunning (retrying a failed payment and emailing the customer) and
collection.

Velox is built to be self-hosted. Each company's deployment carries only its
own volume, so the combined load that pushes a shared SaaS platform onto Kafka
does not build up in one instance.

## How this compares with Lago's guidance

Lago, another open-source billing engine, ships a Kafka + ClickHouse tier. It
routes everything under **10,000 events/sec** to its ordinary REST API,
batched above ~1,000/s. It only recommends streaming above that, noting that
*"Many customers start on REST and switch to Kafka only when they outgrow
it."*

Lago's docs also describe one unnamed self-hosted deployment: *"A major global
payments company runs Lago self-hosted, processing thousands of transactions
per second. They started on Postgres (validated at 10K events/sec) and later
migrated to ClickHouse + Kafka for higher throughput …"*
([source](https://getlago.com/docs/guide/events/ingesting-usage), checked
2026-10-14).

So in Lago's guidance, 10,000 a second is where you start to consider
streaming, not where Postgres stops working. That is roughly 26 billion
events a month. That figure is theirs, not ours; Velox's own figures are
above.

## Published runs

Both runs are published in full, with the method, the evidence and what each
one does *not* show.

- [Sustained throughput](benchmarks/sustained-throughput.md): the figures on
  this page.
- [Correctness under failure](benchmarks/failure-correctness.md): what
  happens to your invoices when the billing process dies mid-run. In every
  run, 0 invoices were billed twice and 0 were lost.
