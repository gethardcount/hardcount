# A database upgrade failed midway: "schema_migrations is dirty"

A migration is a versioned script that changes the database schema. This
page is for when one stopped before it finished, and Velox now refuses to
start. Back to the [runbook index](index.md).

## Symptom

The server will not start, and `velox migrate` and `velox migrate rollback`
refuse too. The log says:

```
schema_migrations is dirty at version 180 (0180_dunning_policy_seeded.up.sql): a migration started and did not finish ...
```

`velox migrate status` prints `dirty: true`.

## Why

Before it runs a migration, Velox marks that version **dirty** in the
`schema_migrations` table. It clears the mark when the migration finishes.
If the migration fails, or Velox is stopped partway through, the mark
stays. Velox cannot tell how much of the migration ran, so it stops and
waits for a person to check.

Most migrations run in one transaction. If one of those failed, Postgres
undid all of it, and the database is still at the version before. A few
migrations run outside a transaction (the error says so). Those can be
partly applied.

## Check

1. Make sure no migration is still running. Only one copy of Velox should
   be trying to start. Stop the others. With Docker Compose, stop the API
   too, because it restarts every few seconds while it refuses to start:

   ```bash
   docker compose stop velox-api
   ```
2. See which migration failed. With Docker Compose, use
   `docker compose run --rm velox-api` in place of `velox`. It prints a few
   "Container … Running" lines before the answer:

   ```bash
   velox migrate status
   ```

   **You see:** `version: 180, dirty: true`, the migration's file name,
   and the two commands that can repair it.

3. Find the first error. Search the log of the start that failed for
   `run migrations` or `migration failed`. Fix the cause first, for example
   a full disk, a lost connection or a missing permission.
4. Open the migration's file in
   [`internal/platform/migrate/sql/`](../../../internal/platform/migrate/sql/)
   for your release, and check in `psql` which of its changes are in the
   database. For example, for a new column:

   ```sql
   \d dunning_policies
   ```

## Fix

Pick the case that matches what you found. The examples use version 180.

- **None of its changes are there** (the usual case). Record the version
  before it, then run the upgrade again:

  ```bash
  velox migrate force 179
  velox migrate
  ```

- **All of its changes are there.** Record it as done, then apply any
  later migrations:

  ```bash
  velox migrate force 180
  velox migrate
  ```

- **Only some are there.** Undo them by hand in `psql` until none are
  there, then follow the first case.

Then start Velox again.

`velox migrate force` only changes the recorded version. It never changes
the schema. It refuses when:

- a migration is running right now,
- nothing is dirty, or
- the version is not the dirty one or right next to it.

Use the Velox release whose upgrade failed, because older releases do not
ship its migration.

If you cannot tell what state the schema is in, restore the backup you took
before the upgrade and try the upgrade again.

## Verify

- `velox migrate status` prints `dirty: false` and the latest version.
- Velox starts (with Docker Compose: `docker compose up -d velox-api`), and `/health/ready` answers 200.
