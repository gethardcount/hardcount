# Restore Hardcount

This page brings Hardcount's database back from a backup, after data loss or a lost server. It is for whoever runs Hardcount. Setting up the backups, and testing that they restore, is on [Back up Hardcount](backup-considerations.md).

A restore takes the database back to the moment of the backup, so everything written after it is gone from Hardcount. Stripe still holds the payments and refunds made since, so Hardcount's background jobs, such as billing, stay paused until this page has matched the two. The match cannot prevent every second charge: billing builds again an invoice that the restore lost, and charges it. [Handle the payments you noted](#handle-the-payments-you-noted) gives the earlier payment back.

**How to run the commands.** On one VM (the [Docker Compose install](../production-one-vm.md)), run the `docker compose` commands in `/opt/hardcount/deploy/compose`, and the SQL in the database shell: `docker compose exec postgres psql -U hardcount -d hardcount`. Elsewhere, run the SQL in `psql` as the database owner, the user in `DATABASE_URL`. Keep the `SET app.bypass_rls = 'on';` that starts some blocks. Without it, an owner that is not a superuser, as on a managed database, sees no rows and changes none, with no error ([why](runbook/index.md#before-you-start)).

## The steps in order

1. Stop Hardcount, and hold traffic ([Before you restore](#before-you-restore), item 1).
2. Pick the backup, and note its time (item 2).
3. Get the server ready: a new VM, the keys, an empty database, the limited user and matching tools (items 3 to 7).
4. [Restore](#restore) the backup.
5. [Pause the five background jobs](#pause-the-background-jobs).
6. [Check the restored database](#check-the-restored-database): the schema version, RLS and the money tables.
7. [Start Hardcount](#match-hardcount-with-stripe), and have your app send its usage events again.
8. Match the payments Stripe took after the backup, then its refunds, disputes and saved cards (same section).
9. [Decide on the queued emails and webhooks](#queued-emails-and-webhooks).
10. [Resume billing](#resume-billing), and wait for its first run to end.
11. [Handle the payments you noted](#handle-the-payments-you-noted).
12. [Resume the other jobs](#resume-the-other-jobs), and open the payment pages and the dashboard again.
13. [Redo the changes made after the backup](#redo-the-changes-made-after-the-backup).

## Before you restore

**Restoring a snapshot or a point in time** (a managed Postgres, pgbackrest or WAL-G)? Do items 1, 2 and 4, then go to [From a snapshot or point in time](#from-a-snapshot-or-point-in-time). **Restoring a dump?** Do every item that applies.

1. **Stop Hardcount, and hold everything that can charge a card.** The holds stay until [Resume the other jobs](#resume-the-other-jobs).
   - **Stop every copy of Hardcount.** On one VM, run `docker compose stop hardcount-api`. Elsewhere, scale every copy to zero.
   - **Hold your app's calls to Hardcount, except usage events.** Once Hardcount starts again, these requests charge a card at once, on the backup's state:
     - a billing run (`POST /v1/billing/run`);
     - creating a subscription, activating one, or ending its trial;
     - any change to a subscription: plan, quantity, items, cancel, scheduling a cancel or undoing one, cancelling a pending item change, pausing or resuming collection, and retrying billing. A change first bills a period that has ended, and the restore may have lost that period's invoice;
     - finalizing an invoice, and collecting its payment (`POST /v1/invoices/{id}/collect`).

     Hold every other write to a subscription or an invoice too. Hold every refund and credit note: the restored invoices do not show the refunds made after the backup, so Hardcount can refund that money again.
   - **Tell your team to stay out of the dashboard.** Its buttons, such as **Collect payment** and **Charge now**, make the same requests.
   - **Close the customer payment pages.** A customer who paid after the backup sees the invoice unpaid, and can pay it again. Block the paths that start with `/v1/public/` at your proxy. With the one-VM Caddy, add the line `respond /v1/public/* 503` inside the site block of `/etc/caddy/Caddyfile`, above `reverse_proxy`, and run `sudo systemctl reload caddy`. If you cannot close them, watch the logs for `payment anomaly: second successful charge on an already-paid invoice`, and refund the payment from your list of [Match Hardcount with Stripe](#match-hardcount-with-stripe), step 4.
2. **Pick the backup, and note its time.** A backup made by Velox, Hardcount's name before v0.9.0, cannot be restored ([Coming from Velox](../upgrades.md#coming-from-velox)).
   - **A dump:** pick the newest dump from before the data loss. `latest.dump` may already hold the damage. List them with `sudo ls -l /var/backups/hardcount/`. A dump's name holds the time it started, in UTC: `hardcount_20261019_030000.dump` started at 03:00:00 UTC on 19 October 2026. On one VM, stop the nightly backup until the restore is done: `sudo mv /etc/cron.d/hardcount-backup ~/`.
   - **A point in time** is the time you restore to. **A snapshot** is the time it was made.

   Note the time in UTC, and in Unix seconds for the Stripe steps: `date -u -d '2026-10-19 03:00:00' +%s` on Linux, or `date -u -j -f '%Y-%m-%d %H:%M:%S' '2026-10-19 03:00:00' +%s` on macOS.

   **You see:** `1792378800`.
3. **On a new VM, set up the old install first.** Do step 1 of [Install a pinned release](../production-one-vm.md#1-install-a-pinned-release), with the release that your saved `.env` names in `HARDCOUNT_IMAGE`. Copy your saved `.env` in place of `.env.example`, and skip step 2: the saved file holds the passwords and keys. Then start only the database, which Compose creates empty, with the limited user:

   ```bash
   chmod 600 .env
   docker compose up -d postgres
   ```

   **You see:** a `Container … Started` line. Skip items 4 to 7. Create `/var/backups/hardcount` as in [section 7](../production-one-vm.md#7-back-up-every-night), and copy the dump into it. Set up HTTPS as in [section 3](../production-one-vm.md#3-add-https-with-caddy), with the `respond` line of item 1.

   If you already started Compose with a new `.env`, keep its passwords, which match the database Compose made. Copy only `HARDCOUNT_ENCRYPTION_KEY` and `HARDCOUNT_EMAIL_BIDX_KEY` into it from the saved one, and do item 5.
4. **On a new server, use the old install's keys.** Set `HARDCOUNT_ENCRYPTION_KEY` and `HARDCOUNT_EMAIL_BIDX_KEY` to the values the backup's install used, from your secrets store.
   - No other encryption key can read the encrypted columns, such as customer details and each account's Stripe keys ([why](backup-considerations.md#1-keep-the-encryption-key)). With a wrong one, every request that reads them fails, and the logs show `decrypt`. The Stripe events of [Match Hardcount with Stripe](#match-hardcount-with-stripe) fail too.
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

### From a snapshot or point in time

Restore with your provider's or your tool's own restore command. If it made a new instance, change `DATABASE_URL` and `APP_DATABASE_URL` to its address. Then go to [Pause the background jobs](#pause-the-background-jobs).

### From a dump

`scripts/restore.sh` restores the dump in one transaction: either all of it is restored, or nothing changes. It needs `pg_restore` and `psql` on the machine that runs it.

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

   **You see:** `All critical tables present.`, a "Restore Summary" block, and `Restore complete. Verify application behavior before resuming traffic.` Keep the holds: the steps below come first.

   If it stops, its exit code says what to do:
   - `1`: nothing changed. The `FATAL` line names the setting or the file to fix. Fix it, and run the script again.
   - `2`: the restore was rolled back, and nothing changed. The `psql` lines above it name the error. The cause is usually a database that is not empty (item 5) or a `psql` older than `pg_restore` (item 7). Fix it, and run the script again.
   - `3`: the restore finished, but a critical table is missing. Do not start Hardcount. Restore an earlier dump into an empty database.

## Pause the background jobs

A background job is a loop that one copy of Hardcount runs at a time, such as billing. Pause the five that charge a card or send an email or a webhook, before anything starts Hardcount. The database calls a job a *role*.

```sql
SELECT leader_pause('billing',          'your-name', 'restore');
SELECT leader_pause('dunning',          'your-name', 'restore');
SELECT leader_pause('email_outbox',     'your-name', 'restore');
SELECT leader_pause('webhook_outbox',   'your-name', 'restore');
SELECT leader_pause('webhook_delivery', 'your-name', 'restore');
```

**You see:** `t` five times. A job that was already paused in the backup returns an empty value instead. Then check all five:

```sql
SELECT role, paused_by, pause_reason FROM leader_status ORDER BY role;
```

**You see:** five rows, each with a `paused_by`. A job that was paused before the backup keeps its earlier name and reason: note it for [Resume billing](#resume-billing) and [Resume the other jobs](#resume-the-other-jobs). Do not start Hardcount until all five rows show a pause.

The pause is a row in the database, so it holds on every copy and through restarts. Requests are not paused: item 1 of [Before you restore](#before-you-restore) holds them. A test clock that was moving forward at the backup time resumes when Hardcount starts, but it bills test customers only.

## Check the restored database

Run the three checks in order, with Hardcount stopped.

### 1. The schema version matches

Run the `hardcount migrate status` of the release you will start, against the restored database. It reads the database and does not start Hardcount. On one VM:

```bash
docker compose run --rm hardcount-api migrate status
```

**You see:** `state:        up to date, nothing to apply`. Other answers:

- `behind` lists the migrations the release will apply. Apply them now with `hardcount migrate` (on one VM, `docker compose run --rm hardcount-api migrate`). **You see:** `applied <n> migrations: the database is at version <v>`.
- `ahead`: the backup came from a newer release. Install the release the backup came from, or a newer one, and run the check again.
- `no migration record`: the backup was restored without its migration table. Restore it again, with the table, into an empty database. A Velox 0.8.x backup gives this answer too, and a later Velox build gives `made by Velox`: neither can be restored ([Coming from Velox](../upgrades.md#coming-from-velox)).

### 2. Row-Level Security is on

Row-Level Security (RLS) is the Postgres feature that keeps each account's rows apart. This lists the tables where it is off, or where the table's owner can skip it:

```sql
SELECT relname, relrowsecurity, relforcerowsecurity
FROM pg_class
WHERE relnamespace = 'public'::regnamespace AND relkind IN ('r', 'p')
  AND NOT (relrowsecurity AND relforcerowsecurity)
ORDER BY relname;
```

**You see:** no table but these six, which hold no account data or are read before the account is known: `goose_db_version`, `leader_leases`, `login_throttle`, `password_reset_tokens`, `tenants`, `users`.

Any other table means its rows are not kept apart. Do not start Hardcount. Restore again, into an empty database. If the table is still listed, the backup itself lacks the protection: see [Data leak between accounts suspected](runbook/rls-leak-suspected.md).

### 3. The money tables look right

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

**You see:** first, `newest_change`, the time of the newest row in the audit log, Hardcount's record of changes. On an install in use, it is a little before the backup time. Then a row for each account, and each account's paid totals of the last 90 days in live mode, per currency.

Hardcount keeps no copy of these numbers outside the database. Compare them with what you have: the old database if you kept it, your accounting, or Stripe's payments for the same days. If `newest_change` is much older than the backup time, or an account is missing, you restored the wrong file or the wrong point in time. Restore again before you go on.

## Match Hardcount with Stripe

The restored database does not know about the payments Stripe took after the backup. Billing's own check with Stripe asks only about charges whose result Hardcount has not saved, so it does not find them. These steps record each payment while the jobs are still paused.

1. **If Hardcount's address changed, point Stripe at the new one.** In each Stripe account, open Hardcount's endpoint in the **Webhooks** tab of Workbench, and change the address part of its URL. Keep the path, `/v1/webhooks/stripe/<id>`. Or point the old name at the new server in DNS. Until then, Stripe sends its events, and the ones you resend in steps 6 and 7, to the old address.
2. **Start Hardcount**, with the holds of item 1 of [Before you restore](#before-you-restore) in place. First note the database's time, `SELECT now();`, for [Queued emails and webhooks](#queued-emails-and-webhooks). Then start it. On one VM:

   ```bash
   docker compose up -d
   timeout 120 bash -c 'until curl -fsS http://localhost:8080/health/ready; do sleep 2; done'
   ```

   Elsewhere, use your Hardcount address followed by `/health/ready`.

   **You see:** perhaps a few `502` lines while it starts, then `"status":"ok"`. The `leader_status` query of [Pause the background jobs](#pause-the-background-jobs) still shows five paused jobs. `"status":"ok"` does not mean the jobs run.
3. **Have your app send its usage events again.** Send every event it sent after the backup time, with the same `idempotency_key`. Hardcount counts each key once, so an event the backup holds is not counted twice. An event that is not sent again is not billed.
4. **List the payments Stripe took, from a day before the backup time.** Do this in each Stripe account that your Hardcount accounts use. This query lists those accounts:

   ```sql
   SET app.bypass_rls = 'on';
   SELECT tenant_id, stripe_account_id, stripe_account_name, secret_key_last4
   FROM stripe_provider_credentials WHERE livemode;
   ```

   In the Stripe Dashboard, open **Payments** and filter by the status **Succeeded** and by date. Its dates are in your Stripe account's time zone, so start the filter on the day before the backup. Or use the Stripe CLI with that account's live secret key. `1792292400` is the example backup time of item 2, less one day (86400 seconds):

   ```bash
   stripe payment_intents search --api-key sk_live_… --limit 100 \
     --query "status:'succeeded' AND created>=1792292400"
   ```

   The extra day catches a payment that started before the backup and finished after it, and the time zone gap. The extra rows do no harm: a payment that the backup already holds falls in the first row of the table in step 5. If the answer says `"has_more": true`, run it again with `--page` set to its `next_page`. Each payment Hardcount made names its invoice in its metadata, as `hardcount_invoice_id`. A payment without it was not made by Hardcount: skip it.
5. **Look up each payment's invoice in the restored database.**

   ```sql
   SET app.bypass_rls = 'on';
   SELECT id, status, payment_status, stripe_payment_intent_id FROM invoices WHERE id = 'hc_inv_…';
   ```

   | You see | Do |
   |---|---|
   | `paid`, and `stripe_payment_intent_id` is this payment's ID | Nothing |
   | `paid` another way: another payment's ID, or none | The customer paid this invoice twice. Refund this payment in Stripe, the one from your list, not the one the invoice records. |
   | `finalized` or `uncollectible` | Resend the payment's event (step 6) |
   | `draft`, any other status, or no row | Note the payment's ID, amount, currency, date and `hardcount_customer_id` for [Handle the payments you noted](#handle-the-payments-you-noted). Do not resend it: Hardcount records a card payment only on a finalized or uncollectible invoice. |

6. **Resend each payment's `payment_intent.succeeded` event to Hardcount.** In the Stripe Dashboard, open the event from the **Events** tab in Workbench and click **Resend**. Or run `stripe events resend evt_… --webhook-endpoint we_… --api-key sk_live_…`, where `we_…` is Hardcount's endpoint in the **Webhooks** tab.

   **You see:** the step 5 query shows `paid`. If it does not, check that Stripe sends to Hardcount's current address (step 1). If the invoice still is not paid, note the payment as for a draft.

   Stripe resends an event only within 15 days of it. For an older one, record the payment on the invoice instead ([Mark an invoice paid](runbook/manual-interventions.md#mark-an-invoice-paid-offline-payment-received)), with the payment's ID in the note.
7. **Resend the refunds, disputes and saved cards.** Do this after step 6, so that each payment is recorded before its refund. List the events of these types from the same start time, and resend each one as in step 6, oldest first. In the **Events** tab, filter by type and date. Or with the Stripe CLI:

   ```bash
   stripe events list --api-key sk_live_… --limit 100 \
     -d "created[gte]=1792292400" \
     -d "types[]=refund.created" -d "types[]=refund.updated" \
     -d "types[]=refund.failed" -d "types[]=charge.refund.updated" \
     -d "types[]=charge.dispute.created" -d "types[]=charge.dispute.updated" \
     -d "types[]=charge.dispute.closed" \
     -d "types[]=setup_intent.succeeded" -d "types[]=checkout.session.completed"
   ```

   The list starts with the newest event. If the answer says `"has_more": true`, run it again with `--starting-after` set to the last event's ID. What Hardcount does with each:
   - **A refund made in the Stripe Dashboard, a dispute or a saved card:** Hardcount records it, or flags it on the invoice for you to finish ([A refund made in Stripe](runbook/refund-made-in-stripe.md), [Card disputes](runbook/card-disputes.md)).
   - **A refund made through Hardcount** has its credit note's ID, `hardcount_cn_id`, in its metadata. Look up the credit note, and the invoice by the refund's payment:

     ```sql
     SET app.bypass_rls = 'on';
     SELECT id, status, refund_status FROM credit_notes WHERE id = 'hc_cn_…';
     SELECT id, tax_transaction_id FROM invoices WHERE stripe_payment_intent_id = 'pi_…';
     ```

     If the credit note is there, but its `refund_status` still differs from the refund's status in Stripe after the resend, click **Retry refund** on it. Hardcount finds the refund by its metadata and records it, without refunding again. If the credit note is gone, the invoice shows that money as refundable again. When the invoice's `tax_transaction_id` is empty, record the refund with a credit note, as in step 1 of [A refund made in Stripe](runbook/refund-made-in-stripe.md#fix). Send the amount as `out_of_band_amount_cents`, in place of `refund_amount_cents` and `stripe_refund_id`, and put the refund's ID in the reason. When it is set, a new credit note would reverse the invoice's tax in Stripe Tax a second time. So record the refund in your own books only, and do not refund that money again.

   A refund or a dispute of a payment you noted is not recorded: note it with the payment.

## Queued emails and webhooks

Hardcount queues outgoing emails and webhooks in two tables, `email_outbox` and `webhook_outbox`, and sends them later. Every `pending` row is sent when the jobs resume. If the backup is older than some sends, customers get those emails or webhooks twice.

The safe default: leave the rows `pending`, and accept a few duplicates. Webhook receivers should already handle the same event twice. To skip them instead, follow [Skip the emails and webhooks queued before a restore](runbook/manual-interventions.md#skip-the-emails-and-webhooks-queued-before-a-restore), with the time you noted when you started Hardcount.

## Resume billing

Resume billing alone first. It builds again the invoices the restore lost, and charges them. You then handle the payments you noted before dunning retries a charge or an email asks a customer to pay.

1. **Resume billing.** If it was paused before the backup, leave it paused until you know why someone paused it.

   ```sql
   SELECT leader_unpause('billing');
   ```

   **You see:** `t`.
2. **Wait for its first run to end.** It runs at once if its last run in the backup is older than its interval ([When invoices are made](../api/timing-and-limits.md#when-invoices-are-made)), and otherwise when the interval ends. Then count the live subscriptions still past their billing date:

   ```sql
   SET app.bypass_rls = 'on';
   SELECT role, last_tick_age_s FROM leader_status WHERE role = 'billing';
   SELECT count(*) AS due, min(next_billing_at) AS oldest
   FROM subscriptions
   WHERE livemode AND test_clock_id IS NULL AND status IN ('active', 'trialing')
     AND next_billing_at <= now() - interval '1 hour';
   ```

   **You see:** `last_tick_age_s` starts again from a few seconds when the run ends, and `due` falls to 0. `1 hour` is the default invoice grace, `INVOICE_GRACE_PERIOD`: use your value if you set one. A subscription whose billing fails stays counted: see [Subscriptions whose billing is failing](runbook/billing-failing.md). If nothing changes, see [A background job is stuck](runbook/background-job-stuck.md).

## Handle the payments you noted

Do this for each payment you noted at [Match Hardcount with Stripe](#match-hardcount-with-stripe). List the customer's newest invoices:

```sql
SET app.bypass_rls = 'on';
SELECT id, status, payment_status, stripe_payment_intent_id, currency, total_amount_cents,
       billing_reason, billing_period_end, created_at
FROM invoices WHERE customer_id = 'hc_cus_…' ORDER BY created_at DESC LIMIT 5;
```

Find the invoice that matches the payment. If the backup holds the payment's invoice, it is that invoice. Otherwise, it is an invoice created after you resumed billing, in the payment's currency, for about the payment's amount. Do not match on the billing period. An invoice billed in advance covers the coming period, and a `threshold` invoice's period ends when it was made, so both can end after the payment. Then:

- **It holds this payment** (`stripe_payment_intent_id` is the payment you noted): nothing to do. This happens to a draft that the backup held: its charge sent the same request as before the restore. Stripe keeps its answer to a request for 24 hours, so it returned the first payment and charged nothing new.
- **It is paid with another payment:** the customer paid twice. Refund the earlier payment, the one from your list, in the Stripe Dashboard. If part of it was refunded since, refund what is left. The invoice then matches the one payment left. No invoice holds the refunded payment, so for up to three days Hardcount logs a warning each time Stripe resends that refund's event. That is expected.
- **Its charge failed, or it waits for the customer to pay** (`payment_status` `failed` or `pending`): record the earlier payment on it now ([Mark an invoice paid](runbook/manual-interventions.md#mark-an-invoice-paid-offline-payment-received)). Put the payment's ID in the note.
- **Its charge is still running** (`payment_status` `processing` or `unknown`): wait for the result, then follow one of the cases above. Hardcount refuses to record a payment while a charge runs.
- **It is still a draft:** it is charged when it is finalized, by billing or by you. Come back to it then.
- **No invoice matches:** the subscription or invoice behind the payment was lost too, and perhaps its customer. Nothing charges it again for now. Keep the payment's ID for your accounts, and see [Redo the changes made after the backup](#redo-the-changes-made-after-the-backup).

## Resume the other jobs

1. **Lift the holds.** Remove the `respond` line from the Caddyfile and run `sudo systemctl reload caddy`, or open the paths at your proxy. Tell your team that the dashboard is open again.
2. **Resume the other four jobs.** Leave out a job that was paused before the backup, until you know why someone paused it.

   ```sql
   SELECT leader_unpause('dunning');
   SELECT leader_unpause('email_outbox');
   SELECT leader_unpause('webhook_outbox');
   SELECT leader_unpause('webhook_delivery');
   SELECT role, paused_at IS NOT NULL AS paused, last_tick_age_s FROM leader_status ORDER BY role;
   ```

   **You see:** `t` for each job you resume, then `paused` is `f` on every row you resumed. Dunning runs the payment retries that are due, and the queued emails and webhooks go out.
3. **Turn the nightly backup on again**, if you turned it off: `sudo mv ~/hardcount-backup /etc/cron.d/`.

## Redo the changes made after the backup

The restore removed every change made after the backup time. Make them again:

- **Your app's calls.** Let the held calls through, and have your app repeat the calls it made after the backup time. The IDs it saved for objects created after the backup no longer exist.
- **Dashboard changes.** If you kept the old database (item 5 of [Before you restore](#before-you-restore)), its audit log lists them. Run this in it, with your backup time:

  ```sql
  SET app.bypass_rls = 'on';
  SELECT created_at, actor_type, action, resource_type, resource_id FROM audit_log
  WHERE created_at >= '2026-10-19 03:00:00+00' ORDER BY created_at;
  ```

Creating a subscription or an invoice again bills it and charges the card. If the customer paid for it after the backup, refund the earlier payment, as in [Handle the payments you noted](#handle-the-payments-you-noted).

**Next:** [Health checks, alerts and logs](runbook/monitoring.md).
