# Data leak between accounts suspected

Every account's data is in one shared database. The database calls an
account a tenant (`tenant_id`). On most work, Postgres Row-Level Security
(RLS) hides every account's rows from every other account. Some work skips
RLS and filters by account in its own query.
Back to the [runbook index](index.md).

**This is a SEV-1.** Follow [Escalation](escalation.md) alongside this page.

## Symptom

- An account reports seeing another account's data.
- Or a support ticket includes data from a different account than the
  operator's session.

## Why

A transaction opened for one account sets `app.tenant_id`, and RLS shows it
only that account's rows. A leak means one of these failed:

- A table holding account data has RLS turned off.
- A query ran with the wrong `app.tenant_id`.
- A query ran with `app.bypass_rls` on, which skips RLS, and its own
  account filter was wrong or missing. Background jobs and sign-in use this
  mode. So do two areas a signed-in user works in for one account: Stripe
  credential settings, and team members and invites. Check every area on
  the [full list](../../architecture.md#design-rules).

In the code, the first kind of transaction is `postgres.TxTenant` and the
`app.bypass_rls` kind is `postgres.TxBypass`.

## Check

1. **Contain it first** ([Escalation, SEV-1 step 1](escalation.md#sev-1)).
   Keep Postgres up for forensics.
2. **Check that RLS is on for every account-scoped table:**

   ```sql
   SELECT relname, relrowsecurity, relforcerowsecurity
   FROM pg_class
   WHERE relnamespace = 'public'::regnamespace AND relkind IN ('r', 'p')
     AND NOT (relrowsecurity AND relforcerowsecurity)
   ORDER BY relname;
   ```

   **You see:** exactly these seven, which hold no account data or are read
   before the account is known: `goose_db_version`, `leader_leases`,
   `login_throttle`, `password_reset_tokens`, `restore_catchup`, `tenants`,
   `users`. Any other table here, or one with row security on but not forced
   (which lets its owner skip it), means isolation is broken.
3. **Check the leaked query.** Was it run with `app.tenant_id` set
   correctly? Was `app.bypass_rls` involved?

## Fix

- Patch the code path that bypassed RLS.
- Search `audit_log` for the affected pair of accounts.
- Notify both customers, and run a breach review.

## Verify

The query in Check step 2 returns only the tables you expect, and the code
path that leaked now returns only the rows of the caller's account.
