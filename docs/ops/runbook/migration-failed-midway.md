# A database upgrade stopped

A migration is a versioned script that changes the database schema. This
page is for when `hardcount migrate`, or Hardcount starting with
`RUN_MIGRATIONS_ON_BOOT=true`, stopped with an error, or Hardcount refuses to
start because of the migration state. Back to the [runbook index](index.md).

From v0.9.0, a failed migration leaves nothing to repair: fix the cause and
run the upgrade again. The rest of this page says how to find the cause for
each kind of error.

## Before anything else

1. Make sure only one copy of Hardcount is trying to start. With Docker Compose,
   stop the API. It restarts every few seconds while it refuses, and with
   `RUN_MIGRATIONS_ON_BOOT=true` every restart runs the failed migration
   again from the start. A slow one, such as an index build or a data
   change on a large table, then loads the database on every restart until
   you stop it:

   ```bash
   docker compose stop hardcount-api
   ```

2. See the plan. With Docker Compose, use `docker compose run --rm hardcount-api`
   in place of `hardcount`:

   ```bash
   hardcount migrate status
   ```

   **You see:** the database's version, this release's, the state, the
   migrations still to apply, and `running now: no`. If it says
   `running now: yes`, another process is migrating: wait for it.

Then find your case below by the error text.

## A normal migration failed

**Symptom:**

```
migration 00182_add_invoice_memo.sql failed: ERROR: ... (SQLSTATE 23502). It runs in one transaction, so none of it was applied. Fix the cause and run the upgrade again.
```

**Why:** each migration runs in one transaction, and goose records the
version in that same transaction. A failure undoes all of it, so the
database is still at the version before.

**Fix:** read the Postgres error. It names the problem, often with a
`detail:` or `hint:` part: a full disk, a lost connection, a missing
permission, or rows the migration did not expect. Fix that, then run
`hardcount migrate` again (or start Hardcount).

If the error is about rows the migration did not expect, the migration
itself may need a fix: report it with the error text.

If it says `waited 5s for a table lock`, see
[It waited 5s for a table lock](#it-waited-5s-for-a-table-lock).

## 00201 found rows no Hardcount version wrote

**Symptom:**

```
migration 00201_remove_dead_contract.sql failed: ERROR: migration 00201 removes values that no Hardcount code writes, and this database has rows that hold them:
  1 subscription(s) with status archived ...
```

**Why:** migration 201 removes values that no Hardcount version ever wrote,
such as subscription status `archived` and the `billed_entries` table. A
row that holds one was written outside Hardcount, for example by hand in
`psql`. Nothing was changed.

**Fix:** find the rows the error lists. Set each to a value that is true
for it, or delete it if nothing needs it. Then run `hardcount migrate` again.

## 00206: a dunning schedule is not whole days

**Symptom:**

```
migration 00206_dunning_retry_days.sql failed: ERROR: migration 00206 writes each dunning schedule as whole days, and these policies cannot be converted exactly:
  dunning policy hc_dpol_...: retry_schedule entry 1 (36h) is not a whole number of days from 1 to 30
```

**Why:** migration 206 rewrites each dunning policy's schedule as
`retry_days`, a list of whole days from 1 to 30. A wait one of the
policy's retries uses, such as `"36h"`, or a wait over 30 days, has no
exact value in days. The error also lists values Hardcount's own policy form
never saved, such as a retry count over 15. Rounding would retry on a day
nobody chose, so the migration changed nothing. A one-retry policy saved
without a schedule (stored as `null`) is not an error: it converts to its
grace period.

**Fix:** choose the days yourself. For each policy the error lists,
change only the value it names. Do not delete the policy: one that has
had a dunning run cannot be deleted, and without the account's default
policy, customers with no policy of their own are not dunned at all.

For a wait, entry 1 of `retry_schedule` is index 0. In `psql` as the
database owner, this sets entry 1 to `72h` (3 days) and keeps the others:

```sql
BEGIN;
SET LOCAL app.bypass_rls = 'on';
UPDATE dunning_policies SET retry_schedule = jsonb_set(retry_schedule, '{0}', '"72h"') WHERE id = '<policy id>';
COMMIT;
```

For a count or first wait, set `max_retry_attempts` (1 to 15) or
`grace_period_days` (1 to 30) the same way.

Then run `hardcount migrate` again.

## 00208 found rows it cannot keep as they bill

**Symptom:** one or both of these lines:

```
migration 00208_meter_one_aggregation.sql failed: ERROR: migration 00208 gives each meter one aggregation for all its pricing rules, and this database has rows it cannot keep as they bill:
  2 pricing rule(s) whose aggregation_mode is not their meter's aggregation
  1 meter(s) whose aggregation is not sum, count, max or last
```

**Why:** from this release a meter has one aggregation, and every pricing
rule of the meter bills under it. The migration stops for two kinds of rows:

- A pricing rule whose own mode differed from its meter's aggregation, or
  was `last_ever`. It would bill differently.
- A meter whose aggregation is not `sum`, `count`, `max` or `last`, such as
  an empty value written by raw SQL. This release adds a check that refuses
  any other value.

It does not convert them, and nothing was changed. This release has no
upgrade path from Velox 0.8.x, so a production database never reaches this
step: install fresh (see the v0.9.0 release notes).

**Fix:** on a development database, find the rows (as the database owner,
with `SET LOCAL app.bypass_rls = 'on'` in a transaction):

```sql
SET app.bypass_rls = 'on';
SELECT r.id, r.meter_id, r.aggregation_mode, m.aggregation
FROM meter_pricing_rules r JOIN meters m ON m.id = r.meter_id
WHERE r.aggregation_mode IS DISTINCT FROM CASE m.aggregation WHEN 'last' THEN 'last_during_period' ELSE m.aggregation END;

SELECT id, key, aggregation FROM meters WHERE aggregation NOT IN ('sum', 'count', 'max', 'last');
```

Delete each rule, or put it on a meter with the aggregation it needs. Set
each meter's aggregation to the one it should bill by. Then run
`hardcount migrate` again. Or re-create the database.

## 00210 found prices it does not convert

**Symptom:** a line like this, with one count for each kind of row found:

```
migration 00210_price_shapes.sql failed: ERROR: migration 00210 renames the price shapes and moves provider costs to cents, and this database has rows it does not convert:
  3 rating rule version(s)
  1 customer price override(s)
  5 provider cost rate(s)
  12 invoice line(s) priced flat
```

**Why:** from this release a price's shape has new names (`flat` is
`per_unit`, an open last tier is `"up_to": null`), a package rounds a part
package up to a whole one, and a provider cost rate is in cents, not
dollars. Every row of these tables would need converting, and so would
every invoice line labelled `flat`. The migration converts nothing, and
nothing was changed. This release has no upgrade path from Velox 0.8.x, so a
production database never reaches this step: install fresh (see the v0.9.0
release notes).

**Fix:** a development database cannot keep these rows. Re-create it (drop
and create the database, then run `hardcount-bootstrap`), or point Hardcount at a
new one. Never run `docker compose down -v` on a volume another checkout
uses.

## 00212 found prices it does not move

**Symptom:** a line like this, with one count for each kind of row found:

```
migration 00212_plan_prices.sql failed: ERROR: migration 00212 moves usage prices from meters to plans, and this database has prices it does not move:
  3 rating rule version(s)
  2 meter pricing rule(s)
  1 customer price override(s)
  2 plan(s) with meters
  12 invoice line(s) priced by a rating rule
```

**Why:** from this release a usage price belongs to a plan (ADR-130). Rating
rules, meter pricing rules and a meter's default price are gone, a price
override names a plan price, and a plan's meters are the meters it has a
price for. The migration moves no price, and nothing was changed. This
release has no upgrade path from Velox 0.8.x, so a production database never
reaches this step: install fresh (see the v0.9.0 release notes).

**Fix:** a development database cannot keep these rows. Re-create it (drop
and create the database, then run `hardcount-bootstrap`), or point Hardcount at a
new one. Never run `docker compose down -v` on a volume another checkout
uses.

## It waited 5s for a table lock

First stop the API: see [Before anything else](#before-anything-else).

**Symptom:**

```
migration 00181_example.sql waited 5s for a table lock and stopped: another database session, usually a long-running transaction, holds a lock on a table it changes. Postgres undid the whole migration and the database is still at version 180, so nothing needs repair ...
```

**Why:** to change a table, a migration needs a lock on it. While the
migration waits for that lock, every other query on the table waits behind
it, so a migration stuck behind one long transaction can stall that table
for everyone. To keep that wait short, Hardcount gives each migration 5 seconds
to get its locks. If it cannot, the migration stops and leaves nothing
behind. Index builds (`CREATE INDEX CONCURRENTLY`) have no time limit,
because they do not block reads or writes while they wait.

**Check:** find what holds the lock. In `psql` as the database owner, list
the open transactions, oldest first:

```sql
SELECT pid, now() - xact_start AS open_for, state, usename, left(query, 80) AS query
FROM pg_stat_activity
WHERE xact_start IS NOT NULL AND pid <> pg_backend_pid()
ORDER BY xact_start
LIMIT 10;
```

**You see:** one or more sessions open for longer than a few seconds.
Common causes: a report or export still running, a `psql` session left in
a transaction (`state` is `idle in transaction`), or a backup tool.

**Fix:** pick one, then run the upgrade again.

- **Wait.** Run the upgrade again when the long transaction has finished
  or traffic is lower.
- **End the blocking session**, if it is safe to stop it:

  ```sql
  SELECT pg_terminate_backend(12345);  -- the pid from the Check step
  ```

- **Wait longer for the lock.** Set `MIGRATION_LOCK_TIMEOUT` for this one
  run, for example `MIGRATION_LOCK_TIMEOUT=30s hardcount migrate`. With
  Docker Compose: `docker compose run --rm -e MIGRATION_LOCK_TIMEOUT=30s hardcount-api migrate`.
  While the migration waits, queries on that table wait too. `0` means no
  limit.

If the upgrade instead says `another Hardcount process is migrating this
database`, it waited 30 minutes for another process's migration, not for
a table. `hardcount migrate status` shows that process under `running now`:
wait for it.

## An index build failed

**Symptom:**

```
migration 00181_usage_rollup_index.sql failed: ERROR: could not create unique index "idx_x" (SQLSTATE 23505); detail: Key (id)=(2) is duplicated.; constraint: idx_x. It runs without a transaction, so Postgres may have left the index INVALID. Running the upgrade again is safe: the file drops the index and builds it again. Fix the cause first.
```

**Why:** an index build (`CREATE INDEX CONCURRENTLY`) does not block
writes, but it cannot run in a transaction. When it fails, Postgres can
leave the index behind, marked INVALID: Postgres keeps it up to date but
never uses it.

**Fix:** fix the cause the error names. For a unique index, `detail:`
names a duplicated key: remove or merge the duplicate rows. Then run
`hardcount migrate` again. Every index-build file drops its index first, so
the INVALID index goes away and is built again.

## Tables, but no migration record

**Symptom:** `hardcount migrate`, or Hardcount at start-up, stops with:

```
this database has tables but no migration record (goose_db_version has no row for 180), so Hardcount cannot tell which version they are at. Nothing was changed. ...
```

`hardcount migrate status` prints
`state:        no migration record, hardcount migrate will not run`.

**Why:** the database has a `tenants` table but no row 180 in
`goose_db_version`, the migration record. Hardcount cannot tell which version
the tables are at, so it changes nothing. This happens when a backup was
restored without its migration table, when the record was dropped by hand,
or when the database is from Velox 0.8.x, Hardcount's old name. Hardcount
looks for a `tenants` table in `public` and in every schema on the
connecting user's `search_path`, so Velox 0.8.x tables kept in the admin
user's own schema count too.

**Fix:** restore the backup again, with its migration table, into an empty
database, or point Hardcount at an empty database. Then run `hardcount migrate`.
Upgrading from Velox 0.8.x is not supported; install fresh.

If `hardcount migrate` instead stops with
`migration 00180 baseline failed: ... already exists`, the database holds
some of the objects the baseline creates but no `tenants` table. Point
Hardcount at an empty database.

## The database was made by Velox

**Symptom:** `hardcount migrate`, or Hardcount at start-up, stops with:

```
this database was made by Velox, Hardcount's old name (its tables give new rows vlx_ ids), and Hardcount does not take over a Velox database. Nothing was changed. ...
```

`hardcount migrate status` prints
`state:        made by Velox, hardcount migrate will not run`.

**Why:** Hardcount was called Velox until v0.9.0. A build of Velox made this
database: its `tenants` table gives new rows `vlx_` ids, where Hardcount's
gives `hc_` ids. v0.9.0 installs fresh only, so Hardcount changes nothing.
A Velox 0.8.x database, or one from a build of `main` before the switch to
goose (#1198), stops with [no migration record](#tables-but-no-migration-record).

**Fix:** point `DATABASE_URL` (and `APP_DATABASE_URL`) at an empty database
and start again. The limited database user is now `hardcount_app`
([the two database users](../postgres-requirements.md#the-two-database-users)).

## Verify

- `hardcount migrate status` prints `state:        up to date, nothing to apply`
  and `running now:  no`.
- Hardcount starts (with Docker Compose: `docker compose up -d hardcount-api`), and
  `/health/ready` answers 200.
