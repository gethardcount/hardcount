# Write I/O after a large backfill

A backfill loads a lot of old usage at once. Afterwards, Postgres spends a
few minutes on maintenance. This page is for the high write I/O that
follows. Back to the [runbook index](index.md).

## Symptom

After a bulk load, write I/O stays high for a few minutes and a commit can
stall.

Measured on the benchmark rig (2026-08-16):

- A 20M-row bulk load into `usage_events` was followed by about 5 minutes
  of RDS write IOPS at about 2× baseline. Autovacuum and the checkpointer
  were working through the load.
- One commit stall of about 5 s came about 20 s after ingest resumed at
  200 events/s.
- Steady traffic in the following 90 minutes, across three rates, showed
  nothing similar.

## Why

This is Postgres maintenance, not a Velox defect.

## Check

Watch `WriteIOPS` return to baseline. Watch `ReadIOPS` too: a latency tail
that rises while CPU stays flat means the index working set is falling out
of cache. See the
[sustained-throughput benchmark](../../benchmarks/sustained-throughput.md)
for the numbers on `db.m7g.2xlarge`.

## Fix

If you backfill tens of millions of events
(`POST /v1/usage-events/backfill`, or a direct load) and then need p99
under 10 ms right away, wait for `WriteIOPS` to return to baseline first.
Or schedule large backfills off-peak.

## Verify

`WriteIOPS` is back at baseline and p99 latency is back to normal.
