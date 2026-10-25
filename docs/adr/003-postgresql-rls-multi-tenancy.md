# ADR-003: PostgreSQL Row-Level Security for Multi-Tenancy

**Date:** 2026-04-15
**Status:** Accepted

## Status
Accepted

## Date
2026-04-14

## Summary

All tenants share one PostgreSQL database and the same tables, and Row-Level Security (RLS) keeps each tenant's rows apart. Each tenant table has a `tenant_id` column and a `tenant_isolation` policy that shows a row only when it matches the transaction's `app.tenant_id`, or when `app.bypass_rls` is `on`. `postgres.BeginTx` sets these per transaction. `TxTenant` is for most tenant work. `TxBypass` is for work whose query filters by tenant itself: background jobs, sign-in, public links, incoming webhooks, and two signed-in areas, Stripe credential settings and team members ([list](../architecture.md#design-rules), pinned by `internal/arch/txbypass_allowlist_test.go`). The app connects as a role that is neither a superuser nor `BYPASSRLS`, and with `APP_ENV` set to anything but `local` Velox refuses to start otherwise ([Postgres requirements](../ops/postgres-requirements.md)). Status: active.

## Context
Velox is multi-tenant: multiple organizations share the same deployment, each seeing only their own data. The three standard approaches are: (1) database-per-tenant, (2) schema-per-tenant, (3) shared tables with row-level isolation.

We needed tenant isolation that is secure by default — a bug in application code should not leak one tenant's invoices to another. At the same time, we wanted to avoid the operational complexity of managing hundreds of databases or schemas as the tenant count grows.

## Decision
All tenants share the same PostgreSQL tables. Every table includes a `tenant_id` column. PostgreSQL Row-Level Security (RLS) policies enforce isolation at the database level:

```sql
CREATE POLICY tenant_isolation ON invoices
  USING (tenant_id = current_setting('app.tenant_id'));
```

Every transaction sets the tenant context via `set_config('app.tenant_id', $1, true)` in `postgres.BeginTx()`. The `TxTenant` mode sets `app.bypass_rls = off` and configures the tenant ID. A separate `TxBypass` mode (for cross-tenant operations like the billing scheduler) sets `app.bypass_rls = on`.

The application database user has RLS enforced; only the migration user can bypass policies. This means even a SQL injection vulnerability cannot cross tenant boundaries.

**Correction (2026-10-23):** the last sentence is wrong. Every `tenant_isolation` policy shows all rows when `app.bypass_rls` is `on` (`internal/platform/migrate/baseline/00180_baseline.sql`, for example the `api_keys` policy), and the app role can set that setting. So an injected statement could set it and read every tenant's rows. RLS contains application bugs, such as a missing `tenant_id` filter; parameterized queries are the defense against SQL injection ([design review 2026-07-10](../dev/records/design-review-2026-07-10.md), item 4).

## Consequences

### Positive
- Defense-in-depth: tenant isolation is enforced by PostgreSQL, not just application WHERE clauses
- Single database to operate, back up, monitor, and migrate
- Shared indexes and connection pools across tenants — efficient resource utilization
- Adding a new tenant is a row insert, not a DDL operation

**Correction (2026-10-25):** the first bullet suggests the queries filter by tenant and RLS is a second layer. Most do not. A typical store query is `SELECT ... FROM invoices WHERE id = $1` (`internal/invoice/postgres.go`), with no `tenant_id` or `livemode` filter of its own. For these queries the `tenant_isolation` policy is the only filter: it keeps tenants apart, and it keeps test rows apart from live ones. Only `TxBypass` work filters by tenant in its own SQL.

### Negative
- Large tenants sharing indexes with small tenants can cause query plan degradation (noisy neighbor)
- RLS adds a small overhead to every query (PostgreSQL must evaluate the policy predicate)
- Debugging requires setting `app.tenant_id` in psql sessions, which is unfamiliar to most developers
- `TxBypass` mode is a sharp edge — misuse could expose cross-tenant data

### Trade-offs
- We accept the noisy-neighbor risk because Velox v1 targets startups and mid-market (not enterprises with 100M+ rows per tenant). If a tenant outgrows shared tables, we can shard by moving them to a dedicated database — the `tenant_id` column makes this a data migration, not a code change.

## Alternatives Considered
- **Database-per-tenant**: Strongest isolation but operationally expensive. Each tenant needs its own connection pool, migrations run N times, and cross-tenant analytics require federation. Rejected for v1.
- **Schema-per-tenant**: Moderate isolation with simpler migrations than database-per-tenant, but still requires per-schema connection management and makes shared indexing impossible. Rejected.
- **Application-level WHERE clauses only**: Simpler to implement but one missed WHERE clause leaks data. In a billing system handling financial data, this risk is unacceptable. RLS is the safety net.
