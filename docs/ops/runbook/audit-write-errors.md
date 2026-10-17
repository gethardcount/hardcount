# Audit write errors

Velox writes an audit log row for every change to data. This page is for
when writing those rows fails. Back to the [runbook index](index.md).

## Symptom

`velox_audit_write_errors_total` rate > 0/s.

## Why

The `outcome` label says which of two very different cases you have:

- **`outcome="row_lost"`: evidence is permanently lost.** The change
  committed and its audit row did not, and nothing retries it. The row
  cannot be rebuilt.
- **`outcome="mutation_refused"`: nothing is missing.** The audit write
  failed inside the business transaction, so the change was rolled back
  with it. The two succeed or fail together. This is an availability
  problem, not an evidence problem: the customer got an error, and the log
  is intact.

## Check

1. Split the rate by the `outcome` label.
2. For `row_lost`, search the error log for lines that start with
   `audit: failed`. Their `tenant_id` field names the tenant. The metric
   carries no tenant label on purpose; see
   [the note below](#why-the-metric-carries-no-tenant_id-label).

## Fix

- **`row_lost`:** treat it as a compliance incident.
  1. Take the tenant from the error log.
  2. Identify the affected changes by their missing audit rows.
- **`mutation_refused`:** investigate the database, not the audit trail.

## Verify

The `velox_audit_write_errors_total` rate returns to 0.

## Why the metric carries no `tenant_id` label

Prometheus client counters never age out. So a tenant label would create a
permanent time series for every tenant that ever had a single failure.
That is unbounded cardinality: it grows with the customer base and outlives
the incident. One shared database blip during a busy hour could create
thousands. The tenant is in the **error log**, which you can search and
which expires. The metric answers only *"is evidence at risk, and in which
direction"*.

Related: [ADR-090](../../adr/090-audit-in-tx-emission.md).
