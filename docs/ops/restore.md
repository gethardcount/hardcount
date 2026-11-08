# Restore Hardcount

This page brings Hardcount's database back from a backup, after data loss or a lost server. It is for whoever runs Hardcount. Setting up the backups, and testing that they restore, is on [Back up Hardcount](backup-considerations.md).

A restore takes the database back to the moment of the backup, so everything written after it is gone from Hardcount. Stripe still holds the payments, refunds, disputes and saved cards made since. So a restored database is marked, and Hardcount refuses to start on it until `hardcount restore catch-up` has applied what Stripe did since the backup. The two jobs that charge cards stay paused after that: billing, which makes and charges invoices, and dunning, which retries failed payments. They wait until you have redone the changes made after the backup.

**How to run the commands.** On one VM (the [Docker Compose install](../production-one-vm.md)), run the `docker compose` commands in `/opt/hardcount/deploy/compose`, and the SQL in the database shell: `docker compose exec postgres psql -U hardcount -d hardcount`. Elsewhere, run `hardcount` with the same settings as the server, and the SQL in `psql` as the database owner, the user in `DATABASE_URL`. Keep the `SET app.bypass_rls = 'on';` that starts some blocks. Without it, an owner that is not a superuser, as on a managed database, sees no rows and changes none, with no error ([why](runbook/index.md#before-you-start)).

## The steps in order

1. Stop Hardcount, pick the backup, and get the server ready ([Before you restore](#before-you-restore)).
2. [Restore](#restore) the backup. This marks the database as restored and pauses billing and dunning.
3. [Bring the schema up to date](#bring-the-schema-up-to-date).
4. [Run the catch-up](#run-the-catch-up), and acknowledge what it lists.
5. [Check the restored database](#check-the-restored-database).
6. [Start Hardcount](#start-hardcount).
7. [Redo the changes made after the backup](#redo-the-changes-made-after-the-backup), and [handle what the catch-up listed](#handle-what-the-catch-up-listed).
8. [Resume billing, then dunning](#resume-billing-then-dunning).

A backup more than 29 days old cannot be caught up: follow [Restore a backup older than 29 days](runbook/restore-old-backup.md) from step 4.

## Before you restore

**Restoring a snapshot or a point in time** (a managed Postgres, pgbackrest or WAL-G)? Do items 1 and 2, then go to [From a snapshot or point in time](#from-a-snapshot-or-point-in-time). **Restoring a dump?** Do every item that applies.

1. **Stop every copy of Hardcount.** On one VM, run `docker compose stop hardcount-api`. Elsewhere, scale every copy to zero. Hardcount checks the restore mark only when it starts, so a copy still running would keep charging. You need not hold requests or close the payment pages.
2. **Pick the backup.** A backup made by Velox, Hardcount's name before v0.9.0, cannot be restored ([Coming from Velox](../upgrades.md#coming-from-velox)).
   - **A dump:** pick the newest dump from before the data loss. `latest.dump` may already hold the damage. List them with `sudo ls -l /var/backups/hardcount/`. A dump's name holds the time it started, in UTC: `hardcount_20261019_030000.dump` started at 03:00:00 UTC on 19 October 2026. On one VM, stop the nightly backup until the restore is done: `sudo mv /etc/cron.d/hardcount-backup ~/`.
   - **A snapshot or a point in time:** note its time.
3. **On a new VM, set up the old install first.** Do step 1 of [Install a pinned release](../production-one-vm.md#1-install-a-pinned-release), with the release that your saved `.env` names in `HARDCOUNT_IMAGE`. Copy your saved `.env` in place of `.env.example`, and skip step 2: the saved file holds the passwords and keys. Then start only the database, which Compose creates empty, with the limited user:

   ```bash
   chmod 600 .env
   docker compose up -d postgres
   ```

   **You see:** a `Container … Started` line. Skip items 4 to 7. Create `/var/backups/hardcount` as in [section 7](../production-one-vm.md#7-back-up-every-night), and copy the dump into it. Set up HTTPS as in [section 3](../production-one-vm.md#3-add-https-with-caddy).

   If you already started Compose with a new `.env`, keep its passwords, which match the database Compose made. Copy only `HARDCOUNT_ENCRYPTION_KEY` and `HARDCOUNT_EMAIL_BIDX_KEY` into it from the saved one, and do item 5.
4. **On a new server, use the old install's keys.** Set `HARDCOUNT_ENCRYPTION_KEY` and `HARDCOUNT_EMAIL_BIDX_KEY` to the values the backup's install used, from your secrets store.
   - No other encryption key can read the encrypted columns, such as customer details and each account's Stripe keys ([why](backup-considerations.md#1-keep-the-encryption-key)). With a wrong one, the catch-up and every request that reads them fail, and the logs show `decrypt`.
   - The email index key lets Hardcount find a customer by email address. A new one finds none of the customers indexed with the old one, so Hardcount keeps emailing addresses that bounced, with no error.
5. **Restore into an empty database.** The script cannot remove a table that the backup does not hold, and such a table can stop the restore or a later upgrade. To empty the database, delete it and create it again as its owner. On one VM:

   ```bash
   docker compose exec -T postgres dropdb -U hardcount hardcount
   docker compose exec -T postgres createdb -U hardcount hardcount
   ```

   **You see:** no output. To keep the old database for a later look, rename it instead of deleting it. Replace the first line with `docker compose exec -T postgres psql -U hardcount -d postgres -c 'ALTER DATABASE hardcount RENAME TO hardcount_old'`.

   Elsewhere, run the same `dropdb` and `createdb` as the owner. Or create a new database as the owner, and change the database name in `DATABASE_URL` and `APP_DATABASE_URL` to it.
6. **On a new Postgres server, create the limited user.** The limited user, `hardcount_app`, is the user that Hardcount's requests run as, so that Row-Level Security applies to them ([Postgres requirements](postgres-requirements.md)). Users belong to the server, so a backup cannot carry them, and the script stops without this one. Run this as the server's admin user (`psql "<admin URL>"`), with the password that `APP_DATABASE_URL` holds. The second line sets the timeout that [Postgres requirements](postgres-requirements.md#timeouts-on-the-limited-user) calls required.

   ```sql
   CREATE ROLE hardcount_app LOGIN PASSWORD '<the password in APP_DATABASE_URL>';
   ALTER ROLE hardcount_app SET idle_in_transaction_session_timeout = '30s';
   ```

   **You see:** `CREATE ROLE`, then `ALTER ROLE`.
7. **Use tools that match the server.** On one VM, skip this item: the restore command below runs Postgres 16 tools. Elsewhere, see the server's version with `psql "$DATABASE_URL" -tAc 'SHOW server_version'`.
   - `pg_restore` must not be newer than the server, or the script stops. Run a matching one in a container: `PG_RESTORE="docker run --rm -i --network host postgres:16-alpine pg_restore"`.
   - `psql` must be at least as recent as `pg_restore`. An older one stops at `invalid command \restrict`, and nothing changes.

## Restore

### From a dump

`scripts/restore.sh` restores the dump in one transaction: either all of it is restored, or nothing changes. The same transaction marks the database as restored and pauses billing and dunning. It needs `pg_restore` and `psql` on the machine that runs it.

1. Run it against the target database. Put your file's name in the last line.

   ```bash
   DATABASE_URL="postgres://hardcount:PASSWORD@db-host:5432/hardcount?sslmode=require" \
     ./scripts/restore.sh /var/backups/hardcount/hardcount_20261019_030000.dump
   ```

   On one VM, the database has no port open on the VM. So run the script in a Postgres container next to it:

   ```bash
   set -a; . ./.env; set +a
   docker run --rm -i \
     --network "container:$(docker compose ps -q postgres)" \
     -v /opt/hardcount/scripts:/scripts:ro \
     -v /var/backups/hardcount:/var/backups/hardcount:ro \
     -e DATABASE_URL="postgres://hardcount:${POSTGRES_PASSWORD}@localhost:5432/hardcount?sslmode=disable" \
     postgres:16 bash /scripts/restore.sh /var/backups/hardcount/hardcount_20261019_030000.dump
   ```

   **You see:** a warning box with the `Target` database and the `Source` file, then `Type 'yes' to proceed:`. Check both, then type `yes`.

2. Wait for it to finish.

   **You see:** `All critical tables present.`, a "Restore Summary" block, and `Restore complete. Hardcount will not start until you run 'hardcount restore catch-up'.`

   If it stops, its exit code says what to do:
   - `1`: nothing changed. The `FATAL` line names the setting or the file to fix. Fix it, and run the script again.
   - `2`: the restore was rolled back, and nothing changed. The `psql` lines above it name the error. The cause is usually a database that is not empty (item 5) or a `psql` older than `pg_restore` (item 7). Fix it, and run the script again.
   - `3`: the restore finished, but a critical table is missing. Do not go on. Restore an earlier dump into an empty database.

### From a snapshot or point in time

Restore with your provider's or your tool's own restore command. If it made a new instance, change `DATABASE_URL` and `APP_DATABASE_URL` to its address. Then, before anything else, mark the database as restored. On one VM, run `docker compose run --rm hardcount-api restore begin`; elsewhere, `hardcount restore begin`.

**You see:** `Marked as restored at … by hardcount restore begin.`, the anchor line, and `Billing and dunning are paused. Hardcount will not start until you run 'hardcount restore catch-up'.` The anchor is the last time the restored data shows Hardcount running, a few seconds before the snapshot.

It refuses, with exit code `2`, when a background job wrote to the database in the last minute: a copy of Hardcount still runs on it. Stop it (item 1), and run `restore begin` again. A copy that ran on the restored database also moved the anchor past the backup: pass the catch-up `--since` with a time three days before the backup. If the restore itself finished less than a minute ago, wait a minute and run it again.

## Bring the schema up to date

Run the `hardcount migrate status` of the release you will start, against the restored database. It reads the database and does not start Hardcount. On one VM:

```bash
docker compose run --rm hardcount-api migrate status
```

**You see:** `state:        up to date, nothing to apply`. Other answers:

- `behind` lists the migrations the release will apply. Apply them now with `hardcount migrate` (on one VM, `docker compose run --rm hardcount-api migrate`). **You see:** `applied <n> migrations: the database is at version <v>`.
- `ahead`: the backup came from a newer release. Install the release the backup came from, or a newer one, and run the check again.
- `no migration record`: the backup was restored without its migration table. Restore it again, with the table, into an empty database. A Velox 0.8.x backup gives this answer too, and a later Velox build gives `made by Velox`: neither can be restored ([Coming from Velox](../upgrades.md#coming-from-velox)).

## Run the catch-up

First note the database's time, `SELECT now();`, for [Queued emails and webhooks](#queued-emails-and-webhooks). Then run the catch-up. On one VM:

```bash
docker compose run --rm hardcount-api restore catch-up
```

Elsewhere, run `hardcount restore catch-up` with the server's settings, which include the database URLs and `HARDCOUNT_ENCRYPTION_KEY`. For each Hardcount account and mode with Stripe keys, it:

1. closes Hardcount's open payment pages at Stripe, so no payment can arrive after the next step;
2. reads every Stripe payment, refund, dispute and saved card from 72 hours before the anchor, and applies them, oldest first, as Stripe's webhooks would have;
3. lists what it must not apply. That is a payment whose invoice the restore lost or left as a draft, a refund that would refund or reverse tax a second time, and an event it could not apply.

**You see:** the anchor and the time it read events from, a line for each account, such as `hc_ten_… live: payment pages closed 1; events 37 (…): applied 12, already recorded 23, not this account's 2, held with a listed payment or refund 0`, and then `catch-up complete; billing and dunning are still paused`. Hardcount can start now.

If it stops, its exit code says what to do. Running it again is safe: an event already applied is not applied twice.

| Exit | What it means | Do |
|---|---|---|
| `1` | A temporary error, such as Stripe or the database not answering. The `stopped:` line names it. | Run it again. If the same `stopped:` line comes back, a rerun will not fix it. `decrypt` means a wrong `HARDCOUNT_ENCRYPTION_KEY` (item 4 of [Before you restore](#before-you-restore)). For anything else, do not start Hardcount, and get help with the `stopped:` line. |
| `2` | It did not start. The message says why. | Fix what it names: a database not marked as restored needs `restore begin`, and a schema behind needs `migrate`. A `--stripe-key` for an account the database does not hold needs the right tenant ID. A backup older than 29 days needs [Restore a backup older than 29 days](runbook/restore-old-backup.md). |
| `3` | It listed payments, refunds or events, under three headings. | Acknowledge each now, as below. You handle them at step 7, once Hardcount runs and while billing is still paused. |
| `4` | A live key does not work: Stripe refused it, for example one rolled since the backup, or it is a test-mode key. | Run it again with the account's current live secret key, as below. |

Test-mode accounts never stop it: their problems are reported only.

**Acknowledge what it listed (exit `3`).** Copy the lines under the three headings first: you need them at step 7. Then:

1. Refund now, in the Stripe Dashboard, each listed payment you will give back rather than keep as credit ([how to choose](#payments-whose-invoice-the-restore-lost)). Its refund is then held with it and recorded with it.
2. For an event listed as `failed:`, run the catch-up again first: one that failed for a moment applies then. A `payment_intent.succeeded` that fails on every run is a payment Hardcount did not record: handle it as a listed payment before you acknowledge it.
3. Run it again with `--acknowledge` and the id of each listed payment (`pi_…`), refund (`re_…`) and event (`evt_…`), such as `restore catch-up --acknowledge pi_… --acknowledge re_…`. An acknowledged payment or refund is recorded with no change. An acknowledged event is tried once more, and recorded without being applied only if it fails again.

**You see:** each listed line ends with `acknowledged`, then `catch-up complete; billing and dunning are still paused`.

**A key for this run only.** `--stripe-key <account>:<live|test>=@<file>` reads a Stripe secret key of that mode from a file, and uses it for this run without saving it. Use it for exit `4`, and for an account that connected Stripe, or changed its key, after the backup: the catch-up reads only the accounts the restored database has keys for. On one VM, the file must be readable inside the container:

```bash
read -rs STRIPE_KEY   # paste the key, then press Enter
printf '%s' "$STRIPE_KEY" | sudo install -m 400 -o 65532 /dev/stdin /opt/hardcount/stripe-key
unset STRIPE_KEY
docker compose run --rm -v /opt/hardcount/stripe-key:/stripe-key:ro hardcount-api \
  restore catch-up --stripe-key hc_ten_…:live=@/stripe-key
sudo rm /opt/hardcount/stripe-key
```

**Never run the catch-up on a copy of your live database,** such as a restore you test. It writes to your live Stripe account: it closes your customers' open payment pages, and the events it applies set customers' default cards and reverse tax in Stripe Tax ([starting a copy](backup-considerations.md#4-run-the-restore-drill)).

## Check the restored database

### Row-Level Security is on

Row-Level Security (RLS) is the Postgres feature that keeps each account's rows apart. This lists the tables where it is off, or where the table's owner can skip it:

```sql
SELECT relname, relrowsecurity, relforcerowsecurity
FROM pg_class
WHERE relnamespace = 'public'::regnamespace AND relkind IN ('r', 'p')
  AND NOT (relrowsecurity AND relforcerowsecurity)
ORDER BY relname;
```

**You see:** no table but these seven, which hold no account data or are read before the account is known: `goose_db_version`, `leader_leases`, `login_throttle`, `password_reset_tokens`, `restore_catchup`, `tenants`, `users`.

Any other table means its rows are not kept apart. Do not start Hardcount. Restore again, into an empty database. If the table is still listed, the backup itself lacks the protection: see [Data leak between accounts suspected](runbook/rls-leak-suspected.md).

### The money tables look right

`tenant_id` is the ID of a Hardcount account.

```sql
SET app.bypass_rls = 'on';
SELECT max(created_at) AS newest_change FROM audit_log;
SELECT tenant_id, count(*) FROM invoices GROUP BY tenant_id;
SELECT tenant_id, count(*) FROM credit_notes GROUP BY tenant_id;
SELECT tenant_id, currency, sum(total_amount_cents) FROM invoices
  WHERE status = 'paid' AND livemode AND created_at >= now() - interval '90 days'
  GROUP BY tenant_id, currency;
```

**You see:** `newest_change`, the time of the newest row in the audit log, a little before the backup time on an install in use. Then a row for each account, and each account's paid totals of the last 90 days in live mode, per currency. Hardcount keeps no copy of these numbers outside the database: compare them with the old database if you kept it, your accounting, or Stripe's payments for the same days. If `newest_change` is much older than the backup time, or an account is missing, you restored the wrong file or the wrong point in time. Restore again before you go on.

## Start Hardcount

1. **If Hardcount's address changed, point Stripe at the new one.** In each Stripe account, open Hardcount's endpoint in the **Webhooks** tab of Workbench, and change the address part of its URL. Keep the path, `/v1/webhooks/stripe/<id>`. Or point the old name at the new server in DNS.
2. **Decide on the [queued emails and webhooks](#queued-emails-and-webhooks)** before you start: they go out when Hardcount starts.
3. **Start it.** On one VM:

   ```bash
   docker compose up -d
   timeout 120 bash -c 'until curl -fsS http://localhost:8080/health/ready; do sleep 2; done'
   ```

   Elsewhere, use your Hardcount address followed by `/health/ready`.

   **You see:** perhaps a few `502` lines while it starts, then `"status":"ok"`. Billing and dunning are still paused: `SELECT role, paused_by FROM leader_status WHERE role IN ('billing', 'dunning');` shows `restore` on both.

### Queued emails and webhooks

Hardcount queues outgoing emails and webhooks in two tables, `email_outbox` and `webhook_outbox`, and sends every `pending` row once it runs. A row the backup holds may have been sent already after the backup, so the customer or your endpoint gets it twice. The catch-up queues the receipts and webhooks of the payments it applied, and the old server may have sent those too.

The safe default: leave the rows `pending`, and accept a few duplicates. Webhook receivers should already handle the same event twice. To skip the rows the backup holds instead, follow [Skip the emails and webhooks queued before a restore](runbook/manual-interventions.md#skip-the-emails-and-webhooks-queued-before-a-restore), with the time you noted before the catch-up, before you start Hardcount.

## Redo the changes made after the backup

The restore removed every change made after the backup time. Make them again while billing and dunning are paused:

- **First, what stops a payment:** an invoice voided, written off or marked paid outside Stripe after the backup. Until you redo it, its payment link takes payment again, and dunning would retry it.
- **Your app's calls.** Have your app send again every usage event it sent after the backup time, with the same `idempotency_key`: Hardcount counts each key once, and an event that is not sent again is not billed. Then have it repeat its other calls. The IDs it saved for objects created after the backup no longer exist.
- **Dashboard changes.** If you kept the old database (item 5 of [Before you restore](#before-you-restore)), its audit log lists them. Run this in it, with your backup time:

  ```sql
  SET app.bypass_rls = 'on';
  SELECT created_at, actor_type, action, resource_type, resource_id FROM audit_log
  WHERE created_at >= '2026-10-19 03:00:00+00' ORDER BY created_at;
  ```

- **What the catch-up listed,** below. Creating a subscription or an invoice again bills it and charges the card, so handle the customer's listed payment first.

## Handle what the catch-up listed

Do this for each line you copied, before you resume billing. Amounts are in the currency's smallest unit, such as cents.

### Payments whose invoice the restore lost

Each line names a payment Stripe took after the backup whose invoice the restore lost (`not in the database`) or left as a draft (`still a draft`). When billing resumes, it builds that invoice again, or finalizes the draft, and charges the card a second time, unless the money is already on the customer's balance.

1. **Find what is left of the payment.** Open it in the Stripe Dashboard (search for its `pi_…`). What is left is its amount less its refunds. Nothing is left while a dispute on it is open, or after you lost one.
2. **Make sure the customer exists.** The line names the Hardcount customer, `hc_cus_…`. If the restore lost the customer too, create it again first, as your app or team did after the backup. Its saved card stays on the old Stripe customer: send it a new card link with **Send setup link**.
3. **Keep the money as credit,** if you did not refund it before you acknowledged it. Grant the customer credit for what is left, in the payment's currency, with the payment's `pi_…` in the description: **Grant credits** on the customer's page, or `POST /v1/credits/grant` ([Adding credit](../concepts/credits.md#adding-credit)). Hardcount uses credit before it charges a card, so the invoice billing builds again is paid from it. If you refunded it, billing charges that invoice as usual.

A refund event of a listed payment that reaches Hardcount after you acknowledged it, such as a later update of a refund held with it, finds no invoice. Stripe sends it again for up to three days, and the logs show `webhook processing failed` with `no invoice holds it yet`. That is expected, and safe to ignore.

### Refunds not applied

Each line names a refund made after the backup, of a payment the restored database holds. Hardcount made it for a credit note the restore lost, or someone made it in Stripe on an invoice reported to Stripe Tax. Until you have handled it, issue no credit note that refunds its invoice: the invoice shows that money as refundable again. Handle each as [Refunds a restore's catch-up did not apply](runbook/restore-listed-refunds.md) says.

### Events not applied

- **`customer hc_cus_… is not in the database`:** a card saved after the backup by a customer the restore lost, with no listed payment to hold it. Once you have created the customer again, send it **Send setup link**.
- **`failed: …`:** the error says why. An event acknowledged after it failed again was recorded without being applied: do by hand what it would have done. For a dispute, see [Card disputes](runbook/card-disputes.md).

## Resume billing, then dunning

1. **Resume billing.** If it was paused before the backup for another reason, leave it paused until you know why someone paused it.

   ```sql
   SELECT leader_unpause('billing');
   ```

   **You see:** `t`.
2. **Wait for its first run to end.** It runs at once if its last run in the backup is older than its interval ([When invoices are made](../api/timing-and-limits.md#when-invoices-are-made)), and otherwise when the interval ends. Then count the live subscriptions still past their billing date or their cancel date:

   ```sql
   SET app.bypass_rls = 'on';
   SELECT role, last_tick_age_s FROM leader_status WHERE role = 'billing';
   SELECT count(*) AS due, min(next_billing_at) AS oldest
   FROM subscriptions
   WHERE livemode AND test_clock_id IS NULL AND status IN ('active', 'trialing')
     AND (next_billing_at <= now() - interval '1 hour'
          OR (status = 'active' AND cancel_at <= now() - interval '1 hour'));
   ```

   **You see:** `last_tick_age_s` starts again from a few seconds when the run ends, and `due` falls to 0. `1 hour` is the default invoice grace, `INVOICE_GRACE_PERIOD`: use your value if you set one. A subscription whose billing fails stays counted: see [Subscriptions whose billing is failing](runbook/billing-failing.md). If nothing changes, see [A background job is stuck](runbook/background-job-stuck.md).
3. **Resume dunning,** which retries the failed payments that are due. Leave it paused if it was paused before the backup for another reason.

   ```sql
   SELECT leader_unpause('dunning');
   SELECT role, paused_at IS NOT NULL AS paused FROM leader_status WHERE role IN ('billing', 'dunning');
   ```

   **You see:** `t`, then `paused` is `f` on both rows.
4. **Turn the nightly backup on again**, if you turned it off: `sudo mv ~/hardcount-backup /etc/cron.d/`.

**Next:** [Health checks, alerts and logs](runbook/monitoring.md).
