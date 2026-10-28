# ADR-003: PostgreSQL Row-Level Security for Multi-Tenancy

**Date:** 2026-04-15
**Status:** Accepted

## Status
Accepted

## Date
2026-04-14

## Summary

All tenants share one PostgreSQL database and the same tables, and Row-Level Security (RLS) keeps each tenant's rows apart. Each tenant table has a `tenant_id` column and a `tenant_isolation` policy that shows a row only when it matches the transaction's `app.tenant_id`, or when `app.bypass_rls` is `on`. `postgres.BeginTx` sets these per transaction. `TxTenant` is for most tenant work. `TxBypass` is for work whose query filters by tenant itself: background jobs, sign-in, public links, incoming webhooks, and two signed-in areas, Stripe credential settings and team members ([list](../architecture.md#design-rules), pinned by `internal/arch/txbypass_allowlist_test.go`). The app connects as a role that is neither a superuser nor `BYPASSRLS`, and with `APP_ENV` set to anything but `local` Hardcount refuses to start otherwise ([Postgres requirements](../ops/postgres-requirements.md)). Status: active.

## Context
Hardcount is multi-tenant: multiple organizations share the same deployment, each seeing only their own data. The three standard approaches are: (1) database-per-tenant, (2) schema-per-tenant, (3) shared tables with row-level isolation.

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
- We accept the noisy-neighbor risk because Hardcount v1 targets startups and mid-market (not enterprises with 100M+ rows per tenant). If a tenant outgrows shared tables, we can shard by moving them to a dedicated database — the `tenant_id` column makes this a data migration, not a code change.

## Alternatives Considered
- **Database-per-tenant**: Strongest isolation but operationally expensive. Each tenant needs its own connection pool, migrations run N times, and cross-tenant analytics require federation. Rejected for v1.
- **Schema-per-tenant**: Moderate isolation with simpler migrations than database-per-tenant, but still requires per-schema connection management and makes shared indexing impossible. Rejected.
- **Application-level WHERE clauses only**: Simpler to implement but one missed WHERE clause leaks data. In a billing system handling financial data, this risk is unacceptable. RLS is the safety net.

## Precedent (2026-10-26)

RLS with a tenant setting that the app sets is the standard design for shared tables on PostgreSQL.

- **AWS** calls it required. Its [Prescriptive Guidance](https://docs.aws.amazon.com/prescriptive-guidance/latest/saas-multitenant-managed-postgresql/rls.html) says "Row-level security (RLS) is required to maintain tenant data isolation in a pooled model with PostgreSQL." A *pooled model* means shared tables with a tenant column, as Hardcount has. The guidance prefers a runtime setting over one database user per tenant. Its example policy, `tenant_id = current_setting('app.current_tenant')`, has the same shape as ours. The [AWS Database Blog post](https://aws.amazon.com/blogs/database/multi-tenant-data-isolation-with-postgresql-row-level-security/) (May 2020) gives the reasoning and the caveats.
- **PostgREST** works the same way. It is the REST layer of every Supabase project. For each request it sets the caller's claims in a transaction-scoped setting, `request.jwt.claims`, and RLS policies read that setting ([PostgREST docs](https://postgrest.org/en/stable/schema_structure.html)).

Open-source billing engines such as Lago filter by tenant in application code. That works when one query layer that code cannot skip adds the filter. Hardcount's SQL is hand-written and has no such layer, so the database is the place that can enforce the filter.

**Why the settings last one transaction, not one session.** AWS's example sets the tenant once per database session, when a Java request borrows its connection. Hardcount sets it with `set_config(..., true)`, so it ends at commit or rollback. This choice has three reasons:

1. Go's `database/sql` pool can run two queries outside a transaction on two different connections. A session setting made on one does not reach the other. Only a transaction keeps every statement on one connection.
2. A transaction setting clears itself. No tenant or mode is left on a connection that goes back to the pool, so there is no reset step to forget.
3. It works behind a pooler in transaction mode, such as PgBouncer, which AWS warns can break session settings.

The cost is that every tenant query runs inside `BeginTx`, including reads.

**Where Hardcount is weaker than these designs.** PostgREST switches to a separate database role for each request. In Hardcount, the app's own role can turn on `app.bypass_rls` (see the 2026-10-23 correction above).
