# Upgrade Hardcount

An upgrade moves Hardcount to a newer release. Its database changes
(migrations) run as part of the upgrade. Take a backup first: it is the
only way back.

## Before you upgrade

1. **Read the notes** for every release between yours and the new one. A
   release with steps to take lists them in the [changelog](../CHANGELOG.md)
   under **Before upgrading**, and in its
   [GitHub release](https://github.com/gethardcount/hardcount/releases) under
   **Before you upgrade**. What a release may change: [Versioning](versioning.md).
2. **Take a backup.** On one VM, run `sudo hardcount-backup`
   ([set it up](production-one-vm.md#7-back-up-every-night)) and wait for
   `Verified:  yes`. With a managed Postgres, take a snapshot, or note the
   time if it has point-in-time restore.
3. **See the plan.** Run `hardcount migrate status` with the new release. It
   only reads: it shows the database's version, the migrations the new
   release would apply, and whether a migration is running now. If it says
   `no migration record`, stop: the upgrade will not run
   ([why](ops/runbook/migration-failed-midway.md#tables-but-no-migration-record)).

### Coming from Velox

Hardcount was called Velox before v0.9.0. v0.9.0 installs fresh only: it
refuses a database made by Velox, a 0.8.x release or a build of Velox's
`main` ([why](ops/runbook/migration-failed-midway.md#the-database-was-made-by-velox)).
A Velox install on one VM does not get that far: its `.env` names `VELOX_*`
settings, so `docker compose pull` (or `build`, from source) stops with
`required variable HARDCOUNT_APP_DB_PASSWORD is missing a value`.

Install Hardcount fresh, with an empty database: a new VM, or a new Compose
data volume. Start from [Install a pinned release](production-one-vm.md#1-install-a-pinned-release).

## Upgrade on one VM

These steps follow [Production on one VM](production-one-vm.md), with
Hardcount in `/opt/hardcount`. Database changes run by themselves as the new
release starts. This example moves to `v0.9.1`.

1. Do the three steps in [Before you upgrade](#before-you-upgrade).
2. Get the new release's files and point `.env` at its images.

   ```bash
   HARDCOUNT_VERSION=v0.9.1
   cd /opt/hardcount
   git fetch --tags
   git checkout "$HARDCOUNT_VERSION"
   cd deploy/compose
   sed -i "s|^HARDCOUNT_IMAGE=.*|HARDCOUNT_IMAGE=ghcr.io/gethardcount/hardcount:${HARDCOUNT_VERSION}|; s|^HARDCOUNT_DASHBOARD_IMAGE=.*|HARDCOUNT_DASHBOARD_IMAGE=ghcr.io/gethardcount/hardcount-dashboard:${HARDCOUNT_VERSION}|" .env
   ```

   **You see:** `HEAD is now at …` from git.

3. Pull the new images and see what the upgrade will change.

   ```bash
   docker compose pull
   docker compose run --rm hardcount-api migrate status
   ```

   **You see:** a few `Container … Running` lines, then the plan. It lists
   the migrations to apply, or says `up to date, nothing to apply`.

4. Start the new images.

   ```bash
   docker compose up -d
   docker compose exec hardcount-api /usr/local/bin/hardcount version
   ```

   **You see:** `hardcount v0.9.1`, and
   `curl -fsS http://localhost:8080/health/ready` answers `"status":"ok"`
   again.

## Upgrade two or more copies

With `RUN_MIGRATIONS_ON_BOOT=true`, several copies can start at once: they
take turns through a Postgres lock, and each one applies only what is still
missing. A waiting copy gives up after 30 minutes and is restarted. What
that does not cover is old code running on a new database, so the release
notes decide the order:

- **A release whose notes say it has migrations:** stop every old copy, run
  `hardcount migrate` once (or start one new copy with
  `RUN_MIGRATIONS_ON_BOOT=true`), then start the other new copies.
- **A release with no migrations:** upgrade one copy at a time.

With `RUN_MIGRATIONS_ON_BOOT=false`, the default outside Compose, run
`hardcount migrate` before you start a new release: Hardcount refuses to
start while a migration is missing. The setup for several copies is in
[Running two or more copies](ops/high-availability.md).

## If the upgrade stops

A migration that fails is undone, so nothing needs repair. Hardcount does
not start, and the error names the cause.

On one VM, `docker compose logs --tail 20 hardcount-api` shows the error.
Compose keeps starting the API again, and each start tries the upgrade
again, so stop it while you find the cause:

```bash
docker compose stop hardcount-api
```

Then follow [A database upgrade stopped](ops/runbook/migration-failed-midway.md).
If the error says `waited 5s for a table lock`, something held a lock on a
table the upgrade changes, often a long-running query: see
[An upgrade waited for a table lock](ops/runbook/migration-failed-midway.md#it-waited-5s-for-a-table-lock).
When the cause is fixed, start the API again:

```bash
docker compose up -d hardcount-api
```

## Going back

To go back, restore the backup you took before the upgrade. Never roll the
database back with `hardcount migrate rollback`: it runs only with
`APP_ENV=local`, because it loses data the newer release wrote. Without a
backup, a database upgrade cannot be undone.

**A restore loses everything written after the backup.** The old release
kept running after the backup, and the new one ran after the upgrade. The
restore removes what both wrote: usage events your app sent, invoices
Hardcount built and charged, payments and refunds, and changes made in the
dashboard or through the API. The longer the new release runs, the more a
restore loses.

On one VM:

1. Stop the API, so nothing more is written.

   ```bash
   cd /opt/hardcount/deploy/compose
   docker compose stop hardcount-api
   ```

   **You see:** a `Container … Stopped` line.

2. Find the backup you took before the upgrade. Its name holds the time it
   started, in UTC. Use that file, not a newer nightly backup: a backup taken
   after the upgrade already holds the new release's changes.

   ```bash
   ls -l /var/backups/hardcount/
   ```

   **You see:** files such as `hardcount_20261019_143000.dump`, a backup
   started at 14:30:00 UTC on 19 October 2026.

3. List the invoices created or changed since that time, and keep the list.
   Run this in the database shell
   (`docker compose exec postgres psql -U hardcount -d hardcount`), with your
   backup's time:

   ```sql
   SELECT id, livemode, status, payment_status, total_amount_cents, updated_at
   FROM invoices
   WHERE updated_at >= '2026-10-19 14:30:00+00'
   ORDER BY updated_at;
   ```

   **You see:** one row for each invoice that the restore removes, or takes
   back to how it was at the backup.

4. Empty the database and restore the backup into it. `dropdb` deletes the
   live database, so check first that step 2 found the right file. Use your
   backup's file name in the last line.

   ```bash
   set -a; . ./.env; set +a
   docker compose exec -T postgres dropdb -U hardcount hardcount
   docker compose exec -T postgres createdb -U hardcount hardcount
   docker run --rm \
     --network "container:$(docker compose ps -q postgres)" \
     -v /opt/hardcount/scripts:/scripts:ro \
     -v /var/backups/hardcount:/var/backups/hardcount:ro \
     -e DATABASE_URL="postgres://hardcount:${POSTGRES_PASSWORD}@localhost:5432/hardcount?sslmode=disable" \
     -e HARDCOUNT_RESTORE_CONFIRM=yes \
     postgres:16 bash /scripts/restore.sh /var/backups/hardcount/hardcount_20261019_143000.dump
   ```

   **You see:** `All critical tables present.`, a "Restore Summary" block
   whose `Migration:` line is the old release's version, and
   `Restore complete.` The limited database user keeps its access, also to
   tables a later upgrade creates: the backup carries those rights.

5. Put back the old release's files and images, and start it. This example
   goes back to `v0.9.0`.

   ```bash
   HARDCOUNT_VERSION=v0.9.0
   cd /opt/hardcount
   git checkout "$HARDCOUNT_VERSION"
   cd deploy/compose
   sed -i "s|^HARDCOUNT_IMAGE=.*|HARDCOUNT_IMAGE=ghcr.io/gethardcount/hardcount:${HARDCOUNT_VERSION}|; s|^HARDCOUNT_DASHBOARD_IMAGE=.*|HARDCOUNT_DASHBOARD_IMAGE=ghcr.io/gethardcount/hardcount-dashboard:${HARDCOUNT_VERSION}|" .env
   docker compose run --rm hardcount-api migrate status
   docker compose up -d
   docker compose exec hardcount-api /usr/local/bin/hardcount version
   ```

   **You see:** `state:        up to date, nothing to apply`, then
   `hardcount v0.9.0`, and `curl -fsS http://localhost:8080/health/ready`
   answers `"status":"ok"`.

6. Put back what the restore removed.
   - **Usage events.** Have your app send again every event it sent after
     the backup time, with the same `idempotency_key`. Hardcount counts each
     key once, so an event the backup already holds is not counted twice. An
     event that is not sent again is not billed.
   - **Payments.** The restored database does not know about a payment
     Stripe took after the backup. A subscription's invoice built after the
     backup is built again by the next billing run, and the customer's card
     can be charged again. Check each invoice on your list from step 3 in
     Stripe, and refund a second charge.
   - **Everything else.** Run the
     [checks after a restore](ops/backup-considerations.md#checks-after-a-restore).

With a managed Postgres, stop every copy, restore the snapshot or the point
in time you noted into the database, start the old release on every copy,
and then do step 6. How a restore works, and what to check before you
start: [Restore a backup](ops/backup-considerations.md#4-restore-a-backup).

**Next:** [Versioning](versioning.md): what a release may change, and where
it says so.
