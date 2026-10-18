# Commits stall after a quiet spell on RDS

Postgres writes every change to its write-ahead log (WAL) in 64 MB files
called segments, and keeps a pool of ready-made ones. On RDS with default
settings that pool can run dry after a quiet spell, and every commit then
waits while a new file is written. This page is for that stall. Back to the
[runbook index](index.md).

The full story, with the evidence, is
[RFM-001: WAL pool exhaustion](../../failure-modes/rfm-001-wal-pool-exhaustion.md).

## Symptom

After a quiet spell, every commit freezes for about 0.2–0.3 s every
about 5 s, until the next checkpoint refills the pool.

- p99 jumps 5–10×; p50 does not move.
- CPU, memory and CloudWatch's 60-second I/O averages look normal (the
  stall minute read 1,039 IOPS, 42 MB/s, queue depth 1.3).

## Why

Measured on the benchmark rig (2026-08-17, `db.m7g.4xlarge`, 100 GB gp3,
PG 16.14, 12,000 events/s, 1-second instrumentation). The tail events the
third run caught are **WAL segment creation under `WALWriteLock`.**

- RDS uses 64 MB WAL segments and keeps a pool of pre-made ones.
- Recycling at each checkpoint completion refills the pool to about one
  checkpoint's worth. By the recycling arithmetic in `xlog.c`, that leaves
  less than one segment of margin at this write rate.
- When the pool runs dry, the committing backend that needs the next
  segment writes 64 MB of zeros and fsyncs it. Every other commit waits.

Two things drain the pool, both after a quiet spell:

1. Small checkpoints lower the checkpointer's distance estimate, so fewer
   old segments are recycled. `min_wal_size` (RDS default 192 MB) is the
   floor that would stop it.
2. RDS's retained segments (`wal_keep_size` 2 GB) sit inside the
   `max_wal_size` window after an idle period. So the pool on resume is
   shallower than in steady state, and the first timer-driven checkpoint
   refills too late. `max_wal_size` bounds the depth.

Both were reproduced on demand; neither setting alone was enough
(provocation arms A–C in
[sustained-throughput.md](../../benchmarks/sustained-throughput.md), third
run).

## Check

How to see it if you suspect it:

- `pg_stat_activity` (or the rig sampler) shows a client backend in
  `IO:WALInitWrite`/`WALInitSync` with a pile-up on `LWLock:WALWrite`.
- `TransactionLogsDiskUsage` grows under load right after it fell at a
  checkpoint.
- Enhanced Monitoring (1 s) shows device writes near the volume's
  throughput ceiling, with the average request size falling toward about
  13 KB while `pg_stat_io` writers are steady.
- Performance Insights at 1 s usually misses the 0.2 s stalls, and a
  `WALWrite` pile-up alone does not prove it is this.

Numbers and the full attribution:
[sustained-throughput.md](../../benchmarks/sustained-throughput.md), third
run.

## Fix

In the instance's parameter group, set:

**`min_wal_size = max_wal_size ≥ wal_keep_size + (1 + checkpoint_completion_target) × peak WAL rate × checkpoint_timeout`, with margin.**

| Peak rate | WAL rate | Set |
|---|---|---|
| 12–15k events/s | 14–20 MB/s | 2 + 8–11 GB, so **16 GB** |
| 25,000 events/s | 29.6 MB/s | **24–32 GB**, or shorten `checkpoint_timeout`. The pool at 16 GB still hit zero twice in 35 minutes |

Both settings are dynamic (no reboot). The cost is disk: `pg_wal` stays
near that size permanently. Two cautions:

1. **Raising the setting does not create segments.** The pool grows only
   as segments are created, one commit pause of about 0.25 s per 64 MB
   file, until about 16 GB of WAL has been written.
   - That is about a minute of pauses in total, paid once in the
     instance's life.
   - You do not notice it if traffic ramps up, or if an initial data load
     runs first (the load creates the files while nobody waits on
     commits).
   - It is a rough 15 minutes or so if you start at peak on an empty pool.
   - The alternative is the stock behavior: the same pauses after *every*
     quiet spell, forever.
   - `pg_switch_wal()` is not granted to `rds_superuser`, so there is no
     cheap way to grow the pool in advance on RDS. A bulk load or about
     15 minutes of peak-rate traffic grows it. (Measured: applying 16 GB to
     a shallow pool made the next 5 minutes *worse*, provocation arm D.)
2. **At higher write rates than measured, work the number out again.**

## Verify

Check the Postgres log at peak. Checkpoints should read
`checkpoint starting: time`, not `wal`. A WAL-driven checkpoint puts you
back on the one-segment margin.

Verified on the rig, one series each, on `db.m7g.4xlarge` + 100 GB gp3:

- With the pool at depth, the idle-then-resume recipe did not stall. That
  same recipe stalled stock, floor-only, depth-only and freshly raised
  settings (worst 10-s p99 24.7 ms against 162–254 ms).
- A 5 × 10 min series at 12,000 events/s ran with all checkpoints
  time-driven, no tail window, and the pool never below 45 segments.
