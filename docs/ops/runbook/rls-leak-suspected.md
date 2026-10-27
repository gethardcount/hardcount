# Data leak between tenants suspected

Each tenant (one account's data) shares the database with others. On most
work, Postgres Row-Level Security (RLS) hides every tenant's rows from every
other tenant. Some work skips RLS and filters by tenant in its own query.
Back to the [runbook index](index.md).

**This is a SEV-1.** Follow [Escalation](escalation.md) alongside this page.

## Symptom

- A tenant reports seeing another tenant's data.
- Or a support ticket includes data from a different tenant than the
  operator's session.

## Why

A transaction opened for one tenant (`postgres.TxTenant`) sets
`app.tenant_id`, and RLS shows it only that tenant's rows. A leak means one of these failed:

- A table holding tenant data has RLS turned off.
- A query ran with the wrong `app.tenant_id`.
- A query ran with `app.bypass_rls` on (`postgres.TxBypass`), which skips
  RLS, and its own tenant filter was wrong or missing. Background jobs and
  sign-in use this mode. So do two areas a signed-in user works in for one
  tenant: Stripe credential settings and team members and invites. Check
  every area on the [full list](../../architecture.md#design-rules).

## Check

1. **Contain it first** ([Escalation, SEV-1 step 1](escalation.md#sev-1)).
   Keep Postgres up for forensics.
2. **Check that RLS is on for every tenant-scoped table:**

   ```sql
   SELECT schemaname, tablename, rowsecurity
   FROM pg_tables WHERE schemaname = 'public' AND rowsecurity = false
   ORDER BY tablename;
   ```

   **You see:** only tables that hold no tenant data. Anything unexpected
   here means isolation is broken.
3. **Check the leaked query.** Was it run with `app.tenant_id` set
   correctly? Was `app.bypass_rls` involved?

## Fix

- Patch the code path that bypassed RLS.
- Search `audit_log` for the affected pair of tenants.
- Notify both customers, and run a breach review.

## Verify

The query in Check step 2 returns only the tables you expect, and the code
path that leaked now returns only the caller's tenant's rows.
