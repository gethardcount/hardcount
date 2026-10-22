# RFM-001: Commits freeze every few seconds after a quiet spell (WAL pool runs dry)

Shipped as guidance in v0.4.0 ([#852](https://github.com/getvelox/velox/pull/852)).
Operator steps: [runbook](../ops/runbook/rds-wal-pool.md).

Diagnosed on AWS during Velox's
[sustained-throughput benchmark](../benchmarks/sustained-throughput.md);
reproducible with the rig scripts in [`scripts/bench-rig/`](../../scripts/bench-rig/).

## The symptom

Your write latency is fine. Then, for about a quarter of a second, every
commit stops. Five seconds later it happens again, until it stops on its own.

What makes it hard to catch:

- **The median request is unaffected** (p50 — the middle of your latency
  distribution — looks exactly as it always did).
- **The slowest 1 in 100 requests get 5–10× worse** (p99). A small, unlucky
  slice takes seconds.
- **It follows a lull.** Traffic comes back after a quiet spell, and the
  freezes start.
- **CPU, memory and query plans look normal.**

## The mechanism

Before Postgres changes any data, it first writes what it is about to do
into a log — the write-ahead log, or WAL — so that a crash can be recovered
from. That log is stored as fixed-size files called segments: 16 MB each by
default, 64 MB on Amazon RDS.

Every few minutes Postgres runs a **checkpoint**: it flushes changed data to
disk, which means the older log files are no longer needed. Rather than
delete them all, Postgres **renames some of them for reuse** — keeping a pool
of ready-made files, so a future commit never has to wait for a new file to
be created.

One setting, `min_wal_size`, decides how many files that pool keeps. RDS
ships with **192 MB** — three 64 MB files.

Now run a write-heavy workload after a quiet period. The pool is shallow. A
commit needs a segment that isn't ready, so it **creates one** — allocating a
64 MB file — while holding the lock every other committing transaction needs.
Everyone waits. When the next checkpoint refills the pool, the freezes stop
until the pool runs shallow again.

That is the whole problem: **every commit stops while one of them creates a
file.**

## The signature — how to prove it's this one

Checkpoints, autovacuum, locks and WAL creation all look like slow writes;
only per-second pool depth tells them apart. Three observations, together,
distinguish it:

1. **Coincidence.** Sample the pool depth once per second. Each freeze-second
   coincides with a pool-empty reading. In one instrumented series, 29 of 32
   freeze-seconds had the pool at zero.
2. **Negative coincidence.** Zero of those freeze-seconds coincide with a
   logged autovacuum completion. (A vacuum storm looks different: average I/O size collapses
   ~107 KB → ~6 KB with queue depth in the hundreds. See
   [Autovacuum stalls every commit](../ops/runbook/autovacuum-stall.md).)
3. **Arithmetic.** Measured WAL rate × checkpoint interval, compared against
   configured `min_wal_size`. If one checkpoint cycle writes more WAL than
   the pool holds, the pool must run dry inside every cycle.

The median-versus-slowest split above supports the diagnosis but does not
prove it: the median stays flat because only the requests that land inside a
freeze window are hurt.

## The fix

Size the pool to hold at least one checkpoint cycle's worth of WAL, with
slack. Verified under both provocation and full measured series:

```
min_wal_size = max_wal_size ≥ wal_keep_size + (1 + checkpoint_completion_target) × WAL rate × checkpoint_timeout
```

(1.9 at RDS's default target of 0.9.)

Worked example from the rig — a `db.m7g.4xlarge` ingesting through an HTTP
API, every event reconciled against the database:

| Ingest rate | Sized pool | Result |
|---|---|---|
| 12,000 events/s (batch 10) | 16 GB | **5 of 5** ten-minute repeats, worst 10-second p99 **52 ms** (stock: 4/5) |
| 15,000 events/s (batch 100) | 16 GB | **5 of 5**, p99 **40.2–42.8 ms** (stock: 4/5) |
| 25,000 events/s (batch 100) | 16 GB | 4 of 5, **0 drops** (stock: 2 of 5 with drops) — the sampler shows 16 GB is still one size too small here; size to ~24–32 GB |

Applying it takes no restart. Raising it does not create the files: the
pool grows only as WAL is written, so the pauses go on until about that much
WAL has been written. Set it when you provision the database, or before a
bulk load.

```sql
ALTER SYSTEM SET min_wal_size = '16GB';
ALTER SYSTEM SET max_wal_size = '16GB';
SELECT pg_reload_conf();
```

On RDS, set the same parameters in the parameter group.

**Verify:** once the pool has grown, the freeze rhythm stops. Watch p99 across repeats rather
than the median — the median never showed the problem in the first place.

**The cost** is disk: pg_wal stays near that size. Crash recovery can also
take a little longer. Shortening `checkpoint_timeout` is the
alternative lever if the space matters more than the extra checkpoint
frequency.

## What this entry does not claim

- **The pool is not the only wall on this path.** In the same campaign,
  checkpoint-era write bursts saturated a 100 GB gp3 volume (3,000 IOPS) at
  15–25k events/s. Where the volume genuinely cannot absorb the rate, a
  deeper pool does not help — that is a capacity problem, not this disease.
  A collapse observed at 25k on one stock series did not replicate on
  another, and is published as *storage-bound, not replicated*.
- **At very high rates on stock settings, the freezes can stop on their own**
  as Postgres grows the pool. But the stalls still happened first — and the
  pool goes shallow again after every restart or quiet spell, so they come
  back next time. Sizing it up front means they stop once the pool has grown.
- Everything above was measured on one rig shape, one AZ, in ten-minute
  windows. The method, gates, evidence files and the full *what this does not
  show* section are in the
  [benchmark document](../benchmarks/sustained-throughput.md).

## Reproduce it

The rig that produced these numbers is in
[`scripts/bench-rig/`](../../scripts/bench-rig/) — provisioning, load
generation, the per-second sampler whose pool-empty ticks make the
coincidence visible, and the census script that classifies freeze-seconds by
mechanism marker.

**Next:** apply the fix: [Commits stall after a quiet spell on RDS](../ops/runbook/rds-wal-pool.md).
