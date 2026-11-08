# Back up Hardcount

This page is for whoever runs Hardcount. It sets up backups and proves that they restore. To bring a backup back during an incident, use [Restore Hardcount](restore.md).

Back up two things, and keep them apart: the Postgres database and the encryption key. A backup cannot be read without its key.

On the one-VM Docker Compose install, follow [Production on one VM, step 7](../production-one-vm.md#7-back-up-every-night): it runs these scripts in a container, every night.

## 1. Keep the encryption key

`HARDCOUNT_ENCRYPTION_KEY` encrypts personal data (email, names, phone numbers, tax IDs), webhook signing secrets and each account's Stripe keys before Hardcount saves them. This is on top of any disk encryption your Postgres has.

- Store the key in a secrets manager (such as AWS Secrets Manager, GCP Secret Manager or HashiCorp Vault), apart from the database backups.
- Without the key, every encrypted column in a backup is unreadable. Nothing else in the backup can take the place of the key.
- Keep the same key. Hardcount has no tool to re-encrypt data with a new key, so changing it makes the old data unreadable.

## 2. Pick how to back up the database

| Method | When to use it | What you can lose |
|---|---|---|
| **Point-in-time restore (PITR)**, recommended | Managed Postgres turns it on with a setting (RDS automated backups, Cloud SQL, Aurora). On your own Postgres, use pgbackrest or WAL-G. | Almost nothing: you can restore to any second within the retention period |
| **Nightly dump** with `scripts/backup.sh` | One VM, or as a second copy you can restore anywhere | Everything written since the last dump |

Avoid these:

- **Dumping table by table.** Hardcount adds tables often, and a list of tables goes out of date.
- **Disk snapshots without the write-ahead log (WAL).** You lose everything since the last snapshot. Hourly snapshots still lose about 30 minutes on average.

On managed Postgres, the provider's snapshots and PITR are your main backup. Test them: every few months, restore one into a new instance, and run `hardcount-doctor` against it as at the end of [the restore drill](#4-run-the-restore-drill). The drill itself proves only that a dump restores. A dump is also a copy you can restore anywhere, so your billing data is not tied to one cloud.

## 3. Take a backup

`scripts/backup.sh` dumps the database with `pg_dump`, checks that the dump can be read, can upload it to S3, and deletes local dumps older than the retention period. It needs `pg_dump` and `pg_restore` on the machine that runs it.

1. Run it with the admin database URL.

   ```bash
   DATABASE_URL="postgres://hardcount:PASSWORD@db-host:5432/hardcount?sslmode=require" \
     ./scripts/backup.sh
   ```

   **You see:** timestamped lines from `Starting backup of database 'hardcount'...` to `Verification passed: archive contains N table entries.`, then a "Backup Summary" block with `Verified:  yes`.

2. Check the file.

   ```bash
   ls -l /var/backups/hardcount/
   ```

   **You see:** `hardcount_<date>_<time>.dump`, and `latest.dump` pointing at the newest one. The date and time are when the backup started, in UTC.

Options, all set as environment variables:

| Variable | Default | What it does |
|---|---|---|
| `BACKUP_DIR` | `/var/backups/hardcount` | Where dumps are written |
| `BACKUP_S3_BUCKET` | unset | Also upload each dump to this S3 bucket. Needs the `aws` command; without it, the upload is skipped with a warning. |
| `BACKUP_S3_PREFIX` | `logical-backups` | The folder inside the bucket |
| `BACKUP_RETENTION_DAYS` | `30` | Local dumps older than this are deleted |
| `PG_DUMP` | `pg_dump` | A different `pg_dump` to run, for example one in a container |
| `HARDCOUNT_BACKUP_TOOL_MISMATCH_OK` | unset | `yes` dumps even when `pg_dump` is newer than the server, for a planned move to a newer Postgres |

Exit codes: `0` success, `1` a setting is wrong, `2` `pg_dump` failed, `3` the dump could not be read back, `4` the S3 upload failed.

**Use a `pg_dump` that matches the server.** A `pg_dump` newer than the Postgres server writes a dump that server cannot restore, so the script stops. It asks the server's version with `psql`. Without `psql`, it skips the check and dumps anyway. Run a matching `pg_dump` in a container:

```bash
PG_DUMP="docker run --rm -i --network host postgres:16-alpine pg_dump" ./scripts/backup.sh
```

The dump travels through standard output, so the container needs no mounted folders.

## 4. Run the restore drill

A backup counts only once you have restored it. The first drill found four defects in the scripts, among them a restore that had never worked on a new server ([record](../dev/records/restore-drill-first-run-2026-08-11.md)).

`scripts/restore-drill.sh` does the whole loop on a throwaway Postgres container, without touching your database or your backup folder:

1. It counts the rows in five key tables of the source database.
2. It takes a backup with `backup.sh` into a temporary folder.
3. It starts a Postgres container with the same major version as the source, on port 15432.
4. It restores the backup there with `restore.sh`.
5. It counts the same rows again, compares them, and removes the container.

Run it every few months, and after every Postgres upgrade. It needs Docker, `pg_dump`, `pg_restore` and `psql` on the machine that runs it.

```bash
SOURCE_DATABASE_URL="postgres://hardcount:PASSWORD@db-host:5432/hardcount?sslmode=require" \
  ./scripts/restore-drill.sh
```

**You see:** `OK` and a count for each table (`tenants`, `customers`, `subscriptions`, `invoices`, `customer_credit_ledger`), then a "Drill Summary" block with `Result:       PASS` and the backup and restore times.

Each run adds one line to `~/.hardcount/drill.log`, so you can see how long backups and restores take over time. Compare the total with how long you can afford to be down.

| Variable | Default | What it does |
|---|---|---|
| `SOURCE_DATABASE_URL` | `DATABASE_URL` | The database to back up and compare |
| `DRILL_PG_IMAGE` | the source's major, else `postgres:16-alpine` | The Postgres image for the throwaway container |
| `DRILL_PG_PORT` | `15432` | The port of the throwaway container. The drill stops if it is in use. |
| `DRILL_LOG` | `~/.hardcount/drill.log` | Where the result lines go |
| `CRITICAL_TABLES` | the five tables above | The tables whose row counts must match |

Exit codes: `0` passed, `1` a setting is wrong, `2` the backup failed, `3` the restore failed, `4` row counts differ, `5` the container did not start.

**Last, check the money rules on a restored copy.** `hardcount-doctor` reads the database and reports money records that break Hardcount's rules. The drill removes its copy, so restore into a spare database instead, such as the test database of [Test a restore](../production-one-vm.md#test-a-restore), and run the doctor before you delete it. The release image does not include it: run it from a source checkout of the same release.

```bash
PGOPTIONS='-c app.bypass_rls=on' \
  DATABASE_URL="postgres://hardcount:PASSWORD@db-host:5432/hardcount_restore_test?sslmode=require" \
  go run ./cmd/hardcount-doctor
```

**You see:** `hardcount-doctor: <n> checks in <time>`, then `OK — no invariant violations`.

`PGOPTIONS` lets the doctor read every account's rows. Without it, only a superuser, or a user with `BYPASSRLS`, sees them. Any other user sees no rows, and the doctor then reports no problems even when there are some ([why](runbook/index.md#before-you-start)).

Do not start Hardcount on a copy of your live database. The copy holds your live Stripe keys and your customers' email addresses, and Hardcount has no setting that makes it safe to start. Its background jobs would bill your customers, charge their cards a second time, and email them.

If you must start it, first pause its five background jobs, and never resume them on the copy. Billing and dunning return an empty value on a copy `restore.sh` made, which has paused them already:

```sql
SELECT leader_pause('billing',          'your-name', 'copy');
SELECT leader_pause('dunning',          'your-name', 'copy');
SELECT leader_pause('email_outbox',     'your-name', 'copy');
SELECT leader_pause('webhook_outbox',   'your-name', 'copy');
SELECT leader_pause('webhook_delivery', 'your-name', 'copy');
```

A copy `restore.sh` made is also marked as restored, so Hardcount refuses to start on it: remove the marker with `DELETE FROM restore_catchup;`. Do not run `hardcount restore catch-up` on a copy: it writes to your live Stripe account. It closes your customers' open payment pages, and the events it applies set customers' default cards and reverse tax in Stripe Tax. Do not charge, refund or change anything through the copy's dashboard or API, and do not open its payment pages: they charge real cards. A test clock that was moving forward resumes at start, but it bills test customers only.

## What is in the database

A restore must bring back every table. Only the rows below can be lost in an emergency:

| Rows | What is lost with them |
|---|---|
| `webhook_outbox` rows | Queued webhooks are not sent, and failed ones cannot be [sent again](runbook/webhook-outbox-backed-up.md#send-failed-rows-again) |
| `email_outbox` rows with `status = 'pending'` | Queued emails are not sent. The other rows are the Sent emails history: keep them. |
| `login_throttle` rows | The count of wrong passwords starts again from zero |
| `usage_events` rows of billing periods already invoiced | No money is lost: the invoices hold the totals. The usage and margin reports for those periods lose them, and a resent old event is stored again ([why](postgres-requirements.md#how-the-data-grows)). |

These are not in the database, and need no database backup:

- Hardcount itself and its container images: download or build them again.
- Settings and secrets (the `.env` file or your secrets manager): back them up separately.
- Invoice PDFs: Hardcount builds them again from the invoice.

**Next:** [Restore Hardcount](restore.md).
