# Write I/O after a large backfill

A backfill loads a lot of old usage at once. Afterwards, Postgres spends a
few minutes on maintenance. Back to the [runbook index](index.md).

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

Autovacuum and the checkpointer work through the new rows.

## Check

Watch `WriteIOPS` return to baseline.

## Fix

This follows a load of tens of millions of past events at once: a direct
load into the database (`POST /v1/usage-events/backfill` takes one event per
request). If you then need p99 under 10 ms right away, wait for
`WriteIOPS` to return to baseline first.
Or schedule large backfills off-peak.

## Verify

`WriteIOPS` is back at baseline and p99 latency is back to normal.
