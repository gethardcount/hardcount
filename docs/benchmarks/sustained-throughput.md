# Sustained throughput

> **Record of runs on 2026-08-16 and 2026-08-17, kept as measured (status checked
> 2026-10-17).** The code has changed since, and the runs have not been repeated.
> The short version of these figures is [How much traffic Velox handles](../scale.md).

## Result

The test ran on `db.m7g.4xlarge` with the #818 hot-row fix. On stock
settings, Velox sustained **12,000 events/s at batch 10 (p99 22.6 ms)** and
**15,000 events/s at batch 100 (p99 43.8 ms)** in 4 of 5 ten-minute repeats.
With the WAL pool sized, both passed 5 of 5 (12k: worst 10-s p99 52 ms; 15k:
p99 40.2–42.8 ms).

- **Conditions:** AWS `ap-south-1`, everything in one availability zone,
  open-loop load (requests sent at a fixed rate whether or not the server
  keeps up), one tenant.
- **Dates:** 2026-08-16 (`db.m7g.2xlarge`) and 2026-08-16/17
  (`db.m7g.4xlarge`, with the fix for #818). Both supersede the 2026-08-15
  figures, which are kept in [Appendix B](../dev/records/sustained-throughput-runs-2026-08.md#appendix-b--superseded-figures).
- **Method:** every figure names its hardware, configuration and cost, and a
  run that cannot be reconciled is not published. See
  [Method](#method-and-the-gates--stated-once).
- **Issues named on this page:** #818 is the hot-row wall (fixed), #819 is
  the linear-scan usage summary (open), and #823 is the WAL written per
  event (filed, not built here).

### At a glance

The rate rows are open-loop and 5 × 10 min, and every event was reconciled.
The closed-loop and `pgbench` rows are ceilings, not service levels. The full
tables and the evidence for each row are in
[Appendix A](../dev/records/sustained-throughput-runs-2026-08.md#appendix-a--the-run-journal).

| what a self-hoster can plan on | on `db.m7g.2xlarge` (32 GB) | on `db.m7g.4xlarge` (64 GB), #818 fixed |
|---|---|---|
| single events, 200 ev/s | p99 **4.9 ms** | — |
| batch 10, the recommended client shape | 1,000 ev/s at p99 **8.2 ms** (5/5), and a wall at ~570 req/s (#818) | **12,000 ev/s** (1,200 req/s) at p99 **22.6 ms** (4/5) stock. **5/5, worst 10-s p99 52 ms with the WAL pool sized** (third run) |
| batch 100 | 5,000 ev/s until the table outgrew RAM (~60M rows) | **15,000 ev/s** at p99 **43.8 ms** (4/5) stock. **p99 40.2–42.8 ms, 5/5 with the WAL pool sized** (third run) |
| batch 100 at 10,000 ev/s | 0/2, a knee | p99 47.3 ms (4/5) on a table growing 61M → 85M rows |
| batch 100 at 25,000 ev/s | — | 2/5 stock with drops. With a 16 GB pool: **4/5, p50 36 ms, p99 49–120 ms, 0 drops**; the sampler shows that pool is still one size too small at that rate |
| what stops it | RAM (index working set falls out of cache) | At 15–25k: the 100 GB gp3 volume (3,000 IOPS / 125 MiB/s). At any rate after a lull: RDS's default WAL segment pool (third run; one parameter fixes it). The app was never the limit (RDS CPU ≤ 67 %, app node ≥ 75 % idle) |
| closed-loop maximum, batch 500 (not a service level) | 25,424 ev/s | **41,172 ev/s** (p50 187 / p99 226 ms) |
| the database's own floor for this row shape (`pgbench`, 16 clients) | 6,840 one-row commits/s | 7,184 one-row commits/s. **52,713 rows/s at batch 500.** Velox's closed loop is **78 % of that, and 101 % of the same with the RLS protocol** |
| reads under load | list endpoints 2–9 ms at every rate; per-customer usage summary is a linear scan, ~2.7 µs/event (#819) | same |

## Reproducing

```bash
cd scripts/bench-rig
./run.sh                 # the whole thing: calibrate -> clean-check -> provision
                         # -> watchdog -> bringup -> seed 20M -> SUSTAINED protocol
                         # -> closed-loop ceiling -> pgbench denominator -> teardown.
                         # Stops at the first step that fails; one log in
                         # ~/.velox-bench-rig/run-<timestamp>.log, readable while
                         # it runs. KEEP=1 leaves the rig up (billing).
```

`run.sh` only sequences the scripts below. Each is runnable on its own, and
that is how a failed step is re-run:

```bash
./calibrate/calibrate.sh          # 0. instrument: must print CALIBRATED (six cases)
./teardown.sh --check             #    account: exit 0 = clean; 2 = COULD NOT LOOK
./provision.sh --yes              # 1. hardware — billing starts here
( nohup ./watchdog.sh 240 >/tmp/velox-rig-watchdog.log 2>&1 & )
./bringup.sh                      # 2. running, seeded (live mode, 200 customers), verified
TARGET=aws ./seed-history.sh 20000000            # 3. history, into the fixtures' partition
TARGET=aws SUSTAINED=1 CONFIGS="single:200:1 batched:1000:10" PROBE_RATE=5 ./measure.sh
TARGET=aws K6_MODE=max VUS=16 CONFIGS="ceiling:0:500" DURATION=90s ./measure.sh
TARGET=aws BATCH=500 VELOX_EVS=<ceiling ev/s> ./db-ceiling.sh   # 5. denominator
./teardown.sh                     # 6. must end CLEAN
```

What each script guards:

- **`db-ceiling.sh`** is the control every Velox throughput figure had been
  missing. A headline "events per second" has no denominator without the
  database's own ceiling for the same row shape. Both legs verify their row
  count in the intended livemode partition, and the script refuses to run
  against fixtures seeded in the other mode.
- **`measure.sh`** exits non-zero if **any repeat of any configuration**
  fails its gate. A configuration is only reported as held at a rate when
  every repeat passed.
- **`bringup.sh`** exists because provisioning hardware is not the same as
  having something to measure. It refuses to continue on any of the failures
  that used to be silent:
  - a cross-AZ RDS;
  - a dirty schema;
  - an app role that cannot read the tables the migration just created;
  - a server that fell back to the **admin** pool, which would measure a
    configuration with no RLS on the request path (nobody self-hosts that);
  - a `201` that wrote no row.

Before a run, read [Traps that cost time](#traps-that-cost-time-and-will-cost-yours).

---

## Method and the gates — stated once

Every current figure on this page was measured under one protocol. The
superseded 2026-08-15 figures in
[Appendix B](../dev/records/sustained-throughput-runs-2026-08.md#appendix-b--superseded-figures) and the laptop history predate
it. The run chapters in
[Appendix A](../dev/records/sustained-throughput-runs-2026-08.md#appendix-a--the-run-journal) carry their per-run specifics.

- **Open-loop by default.** Requests are offered on a fixed schedule,
  whatever the server does, and drops are counted and reported. Closed-loop
  figures (workers that wait for each reply) appear only as *ceilings*, never
  as service levels. A closed loop slows down with the server, so it hides
  queueing delay.
- **Every sustained repeat is gated.** A repeat passes only when all of these
  hold:
  - k6 (the load generator) thresholds held;
  - 0 dropped, 0 failed;
  - rows written == events claimed;
  - Σ quantity == Σ gained: the quantities the client sent sum to exactly
    what the database gained (content, not just row count);
  - ≥1,000 samples;
  - no drift: last-third p99 vs first-third (p99 is the
    ninety-ninth-percentile latency).

  A run whose sent count does not match the rows stored is not published.
- **Ladder rungs pass on the ingest gate alone.** The rate ladder is a quick
  sweep up through offered rates, and a rung is one short step of it
  (2 × 90 s). A rung is judged on delivered rate, drops and reconciliation
  only. That is why a rung can read "held" with drift noted beside it.
- **"Sustained" means 5 × 10 minutes** with a cool-down between repeats, and a
  configuration is reported at the repeat count that passed (4/5 is written
  4/5, not rounded up).
- **A single latency is a median; a range is min–max.** For a sustained row,
  a single p50 or p99 (the 47.3 ms at a glance, the 35.8 ms p50 in a run table)
  is the median across the passing repeats. A range (41.6–49.3 ms) is the lowest
  and highest value among those same repeats. Failed repeats are in neither
  (`measure.sh`). A "worst 10-s p99" is a different statistic: the single worst
  10-second window.
- **Every throughput figure has a denominator.** `pgbench` on the same
  database and row shape (`db-ceiling.sh`) gives the database's own commit
  floor, so "Velox does N" is always beside "the database alone does M".
- **Tail detection is two-grained.** Windows use 10-second buckets. A
  per-second *freeze-seconds* metric also counts individual stalled seconds.
  The bucket rule alone can miss a single frozen second in five or six,
  depending on how stalls align to bucket edges. The caveat and its evidence
  are in [What this leaves](#what-this-leaves).
- **Instrumentation grew per run.** Runs 1–2 had only 60-second CloudWatch
  on the database side, plus the raw k6 sample stream and a 5-second `vmstat`
  on the nodes. Run 3 added 1-second Enhanced Monitoring (RDS's OS-level
  metric stream), Performance Insights (RDS's wait-event view), a 5-second DB
  sampler and the Postgres log. That is why run 2's two tail events (short
  spells of slow requests, labelled E1 and E2) could only be candidates that
  night. Later instrumented series (2026-08-19) attributed E1 to the vacuum
  storm and left E2 most-likely-but-unproven. Run-3 events were attributed
  live. Every claim is read from the retained evidence files, not from a
  console.

## What stops it — the five walls

Five limits the runs hit:

1. **The hot row** (#818, found and fixed). Every request updated its API
   key's last-used row, so one busy key is one contended row.
2. **RAM**, at ~60M rows on a 32 GB instance.
3. **Write IOPS and the WAL segment pool** (Postgres's stock of ready-made WAL
   files).
4. **The vacuum rewrite storm.**
5. **The linear-scan read path** (#819, open).

### What the ladder found: the wall was a hot row, not the hardware

On the first runs, at batch 10, the open-loop knee (the rate where the latency
curve turns upward) sat between 4,000 and 6,000 ev/s (events per second). That
was about **570 requests per second**, at any batch size. It was not the
machine:

| candidate | test | result |
|---|---|---|
| RDS CPU / app CPU | CloudWatch + on-box `vmstat` during the 6k runs | RDS 39–44 %, app node 88 % idle |
| the 20-connection pool | control-vs-treatment: `DB_MAX_OPEN_CONNS=60` | **worse** (p50 2–3 s, one repeat drifting ×3.9) |
| commit / fsync rate | `pgbench` one-row transactions on the same RDS | **6,840 commits/s** at 16 clients, 14,284 at 64 — 10× the plateau |
| what the backends were actually doing | `pg_stat_activity` sampled during a 6k run | **50–55 of 61 backends waiting on `UPDATE api_keys SET last_used_at = $1 WHERE id = $2`** — `Lock:transactionid`, `Lock:tuple`, `LWLock:LockManager` |

Every request then wrote `last_used_at` on the API key it authenticated with.
One high-volume client meant one row, and a row lock hands off at roughly one
RTT (~1.75 ms), so ~570 requests/s. More connections only added waiters. Batch
100 and 500 stayed under 150 requests/s, which is why 10,000 and 15,000 ev/s
held while 6,000 at batch 10 did not. Fixed in
[#818](https://github.com/getvelox/velox/issues/818): Velox now writes
`last_used_at` at most once a minute per key. Run 2 moved the knee from ~570 to
1,600–2,000 requests/s.

### RAM — the index working set falls out of cache

On `db.m7g.2xlarge` (32 GB), steady inserts turn **read-IOPS-bound at roughly
60M rows / 30 GB of table + index**. Across three repeats at 5,000 ev/s, p99
went 51 ms → 116 ms → 9.9 s as the working set outgrew memory. Read IOPS
climbed 108 → 2,486/s against the volume's 3,000 baseline. This is a capacity
cliff, not a Velox defect. Levers: RAM, provisioned IOPS, fewer or smaller
indexes (five on `usage_events`, one GIN), or time-partitioning so the hot set
stays small. Full evidence:
[run 1](../dev/records/sustained-throughput-runs-2026-08.md#the-headline-2026-08-16-measured-under-the-protocol-below).

### Write IOPS and the WAL segment pool — checkpoints, and the freeze rhythm

Two related walls sit on the same volume.

**Capacity.** At 15–25k ev/s batch 100 on stock settings, checkpoint-era write
bursts saturated the 100 GB gp3 volume on the 2026-08-16/17 runs. That
volume has 3,000 IOPS, and disk queue depth reached 37–65 in the stall minutes. The 25k collapse, though, did
not replicate on a 2026-08-19 stock series, and it stays "storage-bound, not
replicated". Where the volume truly cannot absorb the rate, only a bigger
volume or less WAL per event helps.

**The pool.** After a quiet spell, commits freeze ~0.25 s every ~5 s while
RDS's WAL pool refills. 16 GB covers 12–15k ev/s on this rig; at 25k size to
~24–32 GB. Mechanism and sizing rule:
[RFM-001](../failure-modes/rfm-001-wal-pool-exhaustion.md). Series evidence:
[run 3](../dev/records/sustained-throughput-runs-2026-08.md#third-run-2026-08-17-what-a-tail-event-is-caught-live).

### The vacuum rewrite storm — E1, attributed

The insert-triggered autovacuum's completing pass rewrites nearly every page
added since the previous pass. At RDS's stock cost limits it writes at the
volume's full throughput. The result is a multi-second global slowdown in
which **p50 lifts** and 96–99 % of requests are affected. The pool freezes
differ: they leave p50 flat in the instrumented series. (The one most-likely
exception is E2's per-second p50 lift, an artifact of ~36 ms batch-100
requests against ~0.2 s stalls.) The signature to spot at a glance: average IO
size collapses ~107 KB → ~6 KB while queue depth jumps into the hundreds, at a
logged vacuum completion. The full attribution evidence is in
[Appendix A](../dev/records/sustained-throughput-runs-2026-08.md#re-reading-run-2-2026-08-19).

**The lever is tested** (2026-08-20, at 25k ev/s batch 100, same rig, control
vs treatment, fresh 20M table each):

- **Stock `autovacuum_vacuum_insert_scale_factor` (0.2):** each pass rewrites
  everything added since the last. The burst grows with the table (64k → 485k
  pages dirtied across five repeats). At 80M rows one completing pass froze
  every commit for **11 consecutive seconds**: worst request 3,279 ms, worst
  1-s p99 ≈2.9 s, 132 drops, and the repeat failed its gate. The series
  carried 15 marker-classified vacuum freeze-seconds.
- **`autovacuum_vacuum_insert_scale_factor
  = 0.02`:** passes are ~10× smaller (≤48k pages, several landing between
  repeats). The worst freeze was 0.63 s, vacuum freeze-seconds were 3, and the
  series ran **5/5 with zero drops**.

Medians were identical (p50 55.7 vs 55.9 ms), so the setting costs nothing on
the median. It bounds the storm to the growth slice instead of 20 % of an
ever-growing table. A first cross-rig comparison suggested a +20 ms median
cost; a same-rig control refuted it. The two rigs' volumes differ, which is why
the control was required.

Scope: measured at 25k b100 (b = batch size). At 12k b10 the debt accrues at
half the rate and requests are 8 ms, so the residual storms would surface in
p99 only; that is untested. The pacing lever
(`autovacuum_vacuum_cost_delay`/`cost_limit`) was held in reserve and stays
untested, because it was not needed at this rate.

### What the read probe found: the per-customer usage summary is a linear scan

These runs probed `GET /v1/usage-summary/{customer_id}`, removed on 2026-10-23
([#1368](https://github.com/getvelox/velox/pull/1368)). The rig now probes
`GET /v1/usage-events/aggregate?customer_id=`, which runs the same aggregate
query.

While ingest ran, a second k6 scenario read what a finance user reads:
`usage-summary` for a random customer over 30 days, the invoice list, and the
customer list. It ran at 5 requests/s with a p99 budget of 500 ms. The two
list endpoints never left **2–9 ms** at any write rate. `usage-summary` did
not care about the write rate either; it cared about **how many events the
customer had**:

| rows in table | ≈ events per customer | usage-summary p50 | p99 |
|---:|---:|---:|---:|
| 22M | 110k | 332 ms | 342 ms |
| 25M | 125k | 354 ms | 362 ms |
| 39M | 195k | 523 ms | 554 ms |

About 2.7 µs per event, linear: a `COUNT + SUM GROUP BY meter` over the
customer's rows with no rollup (no precomputed aggregate to read instead).
Above ~180k events per customer per month it misses a 500 ms budget regardless
of load. This is also a product finding, with a number on it. The classic fix
(a per-customer daily rollup) is well understood and is filed as
[#819](https://github.com/getvelox/velox/issues/819). The read gate is
reported separately from the ingest gate for exactly this reason: at 10,000
ev/s ingest held and the summary missed its budget, and both are true.

## What this leaves

- **Run 2's two unattributed events (E1 and E2).** Re-read at 1 second from
  their raw k6 samples on 2026-08-19, they have *different* fingerprints. The
  full re-reading, with its timestamps and evidence, is in
  [Appendix A](../dev/records/sustained-throughput-runs-2026-08.md#re-reading-run-2-2026-08-19).

  | event | run 2 repeat | fingerprint | status |
  |---|---|---|---|
  | E2 | 10k repeat 4 (79M rows) | whole seconds of +50–100 ms on every request, recurring every 5–9 s | most likely the WAL segment-creation rhythm; not proven |
  | E1 | 12k repeat 5 (49–56M rows) | **1–3 s global freezes** recurring every ~30 s | the end of an insert-triggered autovacuum pass, attributed on 2026-08-19; run 2's own instance is *most likely* this |
  | — | the 15k/25k failures | storage stalls | **the stock-25k collapse did not replicate**; they stay "storage-bound, not replicated" |

- **A detector caveat.** The 10-second-bucket rule (≥2 consecutive buckets
  above 3× the run's median p99) can miss one frozen second in five or six,
  depending on how the stalls align to bucket edges. Two identical freeze
  episodes on the 2026-08-19 rig scored "no window" and "a window" by
  alignment alone. The tooling now also reports per-second
  **freeze-seconds** (worst request >150 ms and >10 % of that second's
  requests over 50 ms) with their cadence. Every archived series was
  re-scanned with it. The runs published as clean carry 0–3 freeze-seconds
  per 10 min (the sized 12k series: 0 in all five; the sized 15k: 1), and the
  freezes sit exactly where the events were. So the published tails were not
  hiding a rhythm, and the detector gap changed no published number.
- **Product side, quantified from the same evidence.** Velox writes ~1.2 KB of
  WAL and 7.7 WAL records per 200-byte event. Full-page images are *not* the
  driver (0.05 per event with `wal_compression=zstd`). The event insert runs
  as **one statement per event**: 36.1M calls for 36.1M events at batch 10,
  each a `WITH rate AS (SELECT … provider_cost_rates …) INSERT …`. Each event
  also takes three foreign-key `FOR KEY SHARE` lookups: customers, meters, and
  the *same* tenants row 12,000×/s (visible as `LWLock:MultiXactGen`). Index
  growth per event: customer_meter 121 B, idempotency 117 B, pkey 88 B,
  tenant_time 37 B, GIN 9 B, heap 247 B. The levers are multi-row inserts per
  batch and a cheaper strategy for hot parent rows. They cut the WAL rate that
  every number in this section scales with. Filed as
  [#823](https://github.com/getvelox/velox/issues/823), not built here.

## What this does not show

- **Steady state only.** Constant, evenly spaced arrivals; no spikes, no
  diurnal shape, no retries or duplicate keys, server-side timestamps, uniform
  batches, one tenant, one meter. A steady, well-behaved client is the right
  first benchmark, not a "real traffic" claim.
- **"Sustained" means 5 × 10 minutes** with a cool-down between, not an hour
  unbroken. That is long enough for autovacuum and checkpoints to appear (they
  did: see footnote ¹ in [the run journal](../dev/records/sustained-throughput-runs-2026-08.md#the-headline-2026-08-16-measured-under-the-protocol-below), and the third run). It is not long enough for
  multi-hour effects.
- **The read probe is one budget over three endpoints**, reported per endpoint.
  Its `usage-summary` result scales with events-per-customer, so the budget it
  meets depends on how big your customers are.
- **No nginx** in front (deploy/compose puts one there). **Single-AZ**, no
  pooler, no replica, and on-demand pricing. Storage and the load generator
  are excluded from the $/hr.
- **On the `db.m7g.2xlarge`, 15,000 ev/s was only a 2 × 90 s ladder rung**
  (drift ×1.4, a knee approaching). It was not a sustained result. The
  sustained 15,000 ev/s figure on this page is the 4xlarge. It passed 4 of 5
  ten-minute repeats, with drift ×0.88–1.01. Repeat 5 hit one 50-second write-IOPS stall at 56M rows
  (455 drops). Reported as 4/5, not rounded up.
- **The tail attribution (third run) rests on one control event and four
  provocation arms** on one instance shape. The two run-2 events it does not
  explain are named as unexplained. Two settings tested (`min_wal_size` alone,
  `max_wal_size` alone) did not hold up under provocation; the combined
  setting is verified only in the arms and series shown.
- **No dashboard in the loop, but the evidence is captured.** Every run leaves
  the raw k6 sample stream, a 5-second `vmstat` from the app node, and the RDS
  CloudWatch series over its window (`~/.velox-bench-rig/results-*/`). The
  third run adds, per series, the 5-second DB sampler, 1-second Enhanced
  Monitoring and Performance Insights pulls, and the Postgres log. Every claim
  on this page was read from those files, not from a console.

## Measuring responsiveness, not just throughput

The read probe checks that reads stay fast while ingest runs at peak.

```bash
k6 run -e BASE=http://<app-private-ip>:8080 -e API_KEY=... -e CUSTOMER_ID=... \
       -e RATE=1000 -e BATCH=10 -e DURATION=10m \
       -e PROBE_RATE=5 -e PROBE_P99_MS=500 ingest.js
```

`PROBE_RATE` adds a second, concurrent scenario that reads what a human waits
on: the usage aggregate for a random customer, the invoice list, and the customer
list. (The aggregate reads that customer's rows in the very table being
written.) `PROBE_P99_MS` (default 500) is a **threshold**. So a run whose read
path degrades under write load **exits non-zero**, instead of publishing a
throughput figure beside an unusable product. Ingest latency is reported from
the ingest scenario only; the probe's samples never pool into it. Under
`measure.sh` a `DEGRADED` probe fails the run. Known limits: the invoice list
is empty on a bench tenant, and the three endpoints share one verdict.

## Traps that cost time and will cost yours

**Amazon Linux ships Go with `GOTOOLCHAIN=local`.** Three things then go
wrong: `go build` refuses when `go.mod` requires a newer patch release;
`GOSUMDB=off` then blocks downloading the right one; and cloud-init runs
without `HOME`, so the module cache cannot be found. Build with
`HOME=/root GOPATH=/root/go GOTOOLCHAIN=auto
GOSUMDB=sum.golang.org`. The container image is unaffected, because the
Dockerfile pins its own toolchain.

**Create the app role *and* its default privileges before the first migration.**
`CREATE ROLE velox_app` alone is not enough: most tables get their grant from
`ALTER DEFAULT PRIVILEGES`. Skipping it produces a cluster that migrates
cleanly and then returns 500 on ingest across 11 tables.

**`usage_events.livemode` is set by a trigger, not by your INSERT.** The
`set_livemode` trigger overwrites the column from the `app.livemode` session
GUC (a Postgres runtime configuration parameter). The same holds for
`customers`, `meters` and `api_keys`. Fixtures, key, seeded history and pgbench
rows must all be in the same partition. Every script here detects the
fixtures' mode and refuses a mismatch, because an earlier run silently
benchmarked a table it did not think it was measuring.

**On the instances, three things the container-based rehearsal could not
show:**

- `ec2-user` is not in the docker group (`sudo docker`);
- `--monitoring` wants `Enabled=true`;
- a bulk seed leaves autovacuum busy for minutes. The first measured repeat
  after a 22M-row load hit a 5-second stall and failed its gate, so `run.sh`
  settles after seeding.
