# A database upgrade stopped

A migration is a versioned script that changes the database schema. This
page is for when `velox migrate`, or Velox starting with
`RUN_MIGRATIONS_ON_BOOT=true`, stopped with an error, or Velox refuses to
start because of the migration state. Back to the [runbook index](index.md).

From v0.9.0, a failed migration leaves nothing to repair: fix the cause and
run the upgrade again. The rest of this page says how to find the cause for
each kind of error.

## Before anything else

1. Make sure only one copy of Velox is trying to start. With Docker Compose,
   stop the API. It restarts every few seconds while it refuses, and with
   `RUN_MIGRATIONS_ON_BOOT=true` every restart runs the failed migration
   again from the start. A slow one, such as an index build or a data
   change on a large table, then loads the database on every restart until
   you stop it:

   ```bash
   docker compose stop velox-api
   ```

2. See the plan. With Docker Compose, use `docker compose run --rm velox-api`
   in place of `velox`:

   ```bash
   velox migrate status
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
`velox migrate` again (or start Velox).

If the error is about rows the migration did not expect, the migration
itself may need a fix: report it with the error text.

If it says `waited 5s for a table lock`, see
[It waited 5s for a table lock](#it-waited-5s-for-a-table-lock).

## It waited 5s for a table lock

First stop the API: see [Before anything else](#before-anything-else).

**Symptom:**

```
migration 00181_example.sql waited 5s for a table lock and stopped: another database session, usually a long-running transaction, holds a lock on a table it changes. Postgres undid the whole migration and the database is still at version 180, so nothing needs repair ...
```

**Why:** to change a table, a migration needs a lock on it. While the
migration waits for that lock, every other query on the table waits behind
it, so a migration stuck behind one long transaction can stall that table
for everyone. To keep that wait short, Velox gives each migration 5 seconds
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
  run, for example `MIGRATION_LOCK_TIMEOUT=30s velox migrate`. With
  Docker Compose: `docker compose run --rm -e MIGRATION_LOCK_TIMEOUT=30s velox-api migrate`.
  While the migration waits, queries on that table wait too. `0` means no
  limit.

If the upgrade instead says `another Velox process is migrating this
database`, it waited 30 minutes for another process's migration, not for
a table. `velox migrate status` shows that process under `running now`:
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
`velox migrate` again. Every index-build file drops its index first, so
the INVALID index goes away and is built again.

## Velox tables, but no migration record

**Symptom:** `velox migrate`, or Velox at start-up, stops with:

```
this database has Velox tables but no migration record (goose_db_version has no row for 180), so Velox cannot tell which version they are at. Nothing was changed. ...
```

`velox migrate status` prints
`state:        no migration record, velox migrate will not run`.

**Why:** the database has Velox's tables but no row 180 in
`goose_db_version`, the migration record. Velox cannot tell which version
the tables are at, so it changes nothing. This happens when a backup was
restored without its migration table, when the record was dropped by hand,
or when the database is from Velox 0.8.x.

**Fix:** restore the backup again, with its migration table, into an empty
database, or point Velox at an empty database. Then run `velox migrate`.
Upgrading from Velox 0.8.x is not supported; install fresh.

If `velox migrate` instead stops with
`migration 00180 baseline failed: ... already exists`, the database holds
some of the objects the baseline creates but no `tenants` table. Point
Velox at an empty database.

## Verify

- `velox migrate status` prints `state:        up to date, nothing to apply`
  and `running now:  no`.
- Velox starts (with Docker Compose: `docker compose up -d velox-api`), and
  `/health/ready` answers 200.
