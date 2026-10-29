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
   SELECT relname, relrowsecurity, relforcerowsecurity
   FROM pg_class
   WHERE relnamespace = 'public'::regnamespace AND relkind IN ('r', 'p')
     AND NOT (relrowsecurity AND relforcerowsecurity)
   ORDER BY relname;
   ```

   **You see:** exactly these six, which hold no tenant data or are read
   before a tenant is known: `goose_db_version`, `leader_leases`,
   `login_throttle`, `password_reset_tokens`, `tenants`, `users`. Any other
   table here, or one with row security on but not forced (which lets its
   owner skip it), means isolation is broken.
3. **Check the leaked query.** Was it run with `app.tenant_id` set
   correctly? Was `app.bypass_rls` involved?

## Fix

- Patch the code path that bypassed RLS.
- Search `audit_log` for the affected pair of tenants.
- Notify both customers, and run a breach review.

## Verify

The query in Check step 2 returns only the tables you expect, and the code
path that leaked now returns only the caller's tenant's rows.
