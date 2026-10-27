# Audit log gaps

Velox writes an audit row for changes made through the API and dashboard.
Usage ingest and inbound webhooks are exempt (listed in
`internal/api/audit_routes.go`). This page is for the two metrics that say a
row failed or is missing. Back to the [runbook index](index.md).

## Audit write errors

**Symptom:** `velox_audit_write_errors_total` rate > 0/s.

**Why:** the `outcome` label says which of two very different cases you
have:

- **`outcome="row_lost"`: evidence is permanently lost.** The change
  committed and its audit row did not, and nothing retries it. The row
  cannot be rebuilt.
- **`outcome="mutation_refused"`: nothing is missing.** The audit write
  failed inside the business transaction, so the change was rolled back
  with it. The two succeed or fail together. This is an availability
  problem, not an evidence problem: the customer got an error, and the log
  is intact.

**Check:**

1. Split the rate by the `outcome` label.
2. For `row_lost`, search the error log for lines that start with
   `audit: failed`. Their `tenant_id` field names the tenant. The metric has
   no tenant label; the tenant is in the error log
   ([why](../../adr/090-audit-in-tx-emission.md#note-2026-10-22-why-velox_audit_write_errors_total-carries-no-tenant_id-label)).

**Fix:**

- **`row_lost`:** treat it as a compliance incident.
  1. Take the tenant from the error log.
  2. Identify the affected changes by their missing audit rows.
- **`mutation_refused`:** investigate the database, not the audit trail.

**Verify:** the `velox_audit_write_errors_total` rate returns to 0.

## A change with no audit row

**Symptom:** `velox_audit_uncovered_mutation_total{route}` shows any
increase: a route changed data and wrote no audit row.

**Why:** the counter should be **flat zero**. Every route that changes data
is declared in `internal/api/audit_routes.go` as `explicit` (it writes an
audit row) or `exempt` (it does not need to). A non-zero counter means one
of three things, most likely first:

1. A route declared `explicit` has a code path that skips writing the row.
2. A 2xx path that really changes nothing (a cache or idempotency replay, a
   save with no changes) needs an `audit.MarkSkip` declaration.
3. A new route shipped without a declaration. CI blocks this (the
   route-walk test fails the build), unless someone edited the registry to
   silence it.

**Check:** find the route from the `route` label and the
`UNCOVERED MUTATION` error log.

**Fix:** fix the code path that skipped the audit row, or add the missing
declaration. Do not add an exemption without recording what is being given
up.

**Verify:** the counter stops rising for that route.

Related: [ADR-090](../../adr/090-audit-in-tx-emission.md).
