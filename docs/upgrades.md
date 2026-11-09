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
A backup made by Velox cannot be restored into Hardcount either:
`hardcount migrate status` reports `no migration record` for a Velox 0.8.x
backup, and `made by Velox` for one from a later build.

Install Hardcount fresh, with an empty database: a new VM, or a new Compose
data volume. Start from [Install a pinned release](production-one-vm.md#1-install-a-pinned-release).

## Upgrade on one VM

These steps follow [Production on one VM](production-one-vm.md), with
Hardcount in `/opt/hardcount`. Database changes run by themselves as the new
release starts. This example moves to `v0.9.3`.

1. Do the three steps in [Before you upgrade](#before-you-upgrade).
2. Get the new release's files and point `.env` at its images.

   ```bash
   HARDCOUNT_VERSION=v0.9.3
   cd /opt/hardcount
   git fetch --tags
   git checkout "$HARDCOUNT_VERSION"
   cd deploy/compose
   sed -i "s|^HARDCOUNT_IMAGE=.*|HARDCOUNT_IMAGE=ghcr.io/gethardcount/hardcount:${HARDCOUNT_VERSION}|; s|^HARDCOUNT_DASHBOARD_IMAGE=.*|HARDCOUNT_DASHBOARD_IMAGE=ghcr.io/gethardcount/hardcount-dashboard:${HARDCOUNT_VERSION}|" .env
   ```

   **You see:** `HEAD is now at …` from git.

   If git instead says `Your local changes to the following files would be
   overwritten by checkout`, it did not check out the release. You edited a
   file the release changes, often `docker-compose.yml`. Copy your edits into
   `docker-compose.override.yml`
   ([how](settings.md#your-own-changes-the-override-file)), undo them with
   `git -C /opt/hardcount checkout -- <each file git named>`, and run this
   step again.

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

   **You see:** `hardcount v0.9.3`, and
   `curl -fsS http://localhost:8080/health/ready` answers `"status":"ok"`
   again.

## Upgrade two or more copies

With `RUN_MIGRATIONS_ON_BOOT=true`, several copies can start at once: they
take turns through a Postgres lock, and each one applies only what is still
missing. A waiting copy gives up after 30 minutes and is restarted. Taking
turns does not protect old copies that are still running against the
upgraded database. So the release notes decide the order:

- **A release whose notes say it has migrations:** stop every old copy, run
  `hardcount migrate` once (or start one new copy with
  `RUN_MIGRATIONS_ON_BOOT=true`), then start the other new copies.
- **A release with no migrations:** upgrade one copy at a time.

With `RUN_MIGRATIONS_ON_BOOT=false`, the default outside Compose, run
`hardcount migrate` before you start a new release: Hardcount refuses to
start while a migration is missing. The setup for several copies is in
[Run two or more copies](ops/high-availability.md).

## If the upgrade stops

A migration that fails is undone, so nothing needs repair by hand. An index
build is not undone: it runs without a transaction and can leave the index
INVALID. Running the upgrade again drops it and builds it again. Hardcount does
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
[It waited 5s for a table lock](ops/runbook/migration-failed-midway.md#it-waited-5s-for-a-table-lock).
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

3. Empty the database and restore the backup into it. `dropdb` deletes the
   database Hardcount runs on, so check first that step 2 found the right file. Use your
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
   `Restore complete.` The limited database user keeps its access. The backup
   carries its access rights, including the rights to tables that a later
   upgrade creates.

4. Put back the old release, catch the database up with Stripe, and start
   the release. The restore marked the database as restored and paused
   billing and dunning, so the old release will not start until the
   catch-up has run.
   - Put back the old release's files and images, and see its plan. This
     example goes back to `v0.9.2`.

     ```bash
     HARDCOUNT_VERSION=v0.9.2
     cd /opt/hardcount
     git checkout "$HARDCOUNT_VERSION"
     cd deploy/compose
     sed -i "s|^HARDCOUNT_IMAGE=.*|HARDCOUNT_IMAGE=ghcr.io/gethardcount/hardcount:${HARDCOUNT_VERSION}|; s|^HARDCOUNT_DASHBOARD_IMAGE=.*|HARDCOUNT_DASHBOARD_IMAGE=ghcr.io/gethardcount/hardcount-dashboard:${HARDCOUNT_VERSION}|" .env
     docker compose run --rm hardcount-api migrate status
     ```

     **You see:** `state:        up to date, nothing to apply`.
   - Run the checks of
     [Check the restored database](ops/restore.md#check-the-restored-database)
     before the catch-up: they catch a wrong backup before anything writes
     to Stripe.
   - Run the catch-up with the old release, as in
     [Run the catch-up](ops/restore.md#run-the-catch-up), which also lists
     its exit codes:
     `docker compose run --rm hardcount-api restore catch-up`.
     **You see:** `catch-up complete; billing and dunning are still paused`.

     A release from before the catch-up answers `unknown command: restore`,
     and it does not wait for the marker. For it, remove the marker table in
     the database shell
     (`docker compose exec postgres psql -U hardcount -d hardcount`) with
     `DROP TABLE restore_catchup;`. Then match Hardcount with Stripe as that
     release's own `/opt/hardcount/docs/ops/restore.md` says, from the step
     after its restore, instead of the rest of this page.
   - Decide on the
     [queued emails and webhooks](ops/restore.md#queued-emails-and-webhooks),
     and set the holds of item 3 of
     [Start Hardcount](ops/restore.md#start-hardcount): block the payment
     pages, and hold the calls that charge. Then start the release.

     ```bash
     docker compose up -d
     docker compose exec hardcount-api /usr/local/bin/hardcount version
     ```

     **You see:** `hardcount v0.9.2`, and
     `curl -fsS http://localhost:8080/health/ready` answers
     `"status":"ok"`.

5. Put back what the restore removed, and resume billing and dunning.
   Follow
   [Redo the changes made after the backup](ops/restore.md#redo-the-changes-made-after-the-backup)
   and the sections after it, to the end of that page.

With a managed Postgres, stop every copy. Restore the snapshot or the point
in time you noted into the database, and mark it as restored with the old
release's `hardcount restore begin`. A release from before the catch-up has
no such command: follow its own restore page, as step 4 says. Put the old release's image or binary
in place on every copy. Then run that release's `migrate status`, which says
`up to date`, the
[checks of the restored database](ops/restore.md#check-the-restored-database),
and its `hardcount restore catch-up`. Set the holds of
[Start Hardcount](ops/restore.md#start-hardcount), start the old release on
every copy, and do step 5.
How a restore works, and what to check before you start:
[Restore Hardcount](ops/restore.md).

**Next:** [Versioning](versioning.md): what a release may change, and where
it says so.
