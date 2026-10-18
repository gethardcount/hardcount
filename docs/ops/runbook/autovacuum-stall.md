# Autovacuum stalls every commit for seconds

Autovacuum is Postgres's background cleanup. On a large table that only
grows, such as `usage_events`, one of its passes can briefly use all of the
disk's write capacity. This page is for the stall that causes. Back to the
[runbook index](index.md).

## Symptom

For 1–6 s, **p50 lifts for everyone** (36 → 64 ms, 96–99 % of requests
affected). This differs from the [WAL pool stall](rds-wal-pool.md), which
leaves p50 alone. The sign in Enhanced Monitoring: I/Os per second ×10–20
while the average I/O size drops from about 100 KB to about 6 KB.

## Why

Measured on the benchmark rig (2026-08-19, `db.m7g.4xlarge`, 100 GB gp3,
25,000 events/s, stock RDS parameters, vacuum log on).

- PG13 and later run an insert-triggered autovacuum pass over
  `usage_events` after about 20 % growth.
- The end of that pass rewrites nearly every page added since the previous
  pass. This is hint bits and opportunistic freezing, which
  `data_checksums=on` pushes through full-page writes.
- One pass dirtied 540k pages (4.4 GB) and froze 6.6M tuples at 107 MB/s.
- RDS's `autovacuum_vacuum_cost_limit` (1,200 on this class, 2 ms delay)
  lets it write at the volume's full throughput.
- So the disk queue runs to hundreds, with tens of thousands of 6 KB writes
  per second, and every commit's WAL write waits behind it.

## Check

Turn on the vacuum log and match a stall to the end of an autovacuum pass
on `usage_events`, together with the Enhanced Monitoring sign above.

## Fix

Set **`autovacuum_vacuum_insert_scale_factor = 0.02`** (dynamic; or as a
per-table setting on the events table). It limits each burst to the 2 %
growth slice instead of 20 % of an ever-growing table, so the burst no
longer grows with the table.

## Verify

Tested control against treatment on the same rig at 25,000 events/s:

| Setting | Result |
|---|---|
| Stock | The pass at 80M rows froze every commit for 11 s (worst request 3.3 s, worst 1-s p99 ≈2.9 s, dropped requests, failed repeat) |
| 0.02 | Passes about 10× smaller, worst freeze 0.63 s, five of five repeats passed with zero drops, median unchanged (55.7 vs 55.9 ms) |

Measured at 25k events/s, batch 100. At lower rates the remaining bursts
are smaller still. The pacing settings
(`autovacuum_vacuum_cost_delay`/`cost_limit`) were not needed and stay
untested. Details and evidence:
[sustained-throughput.md](../../benchmarks/sustained-throughput.md), third
run, "What this leaves".
