# Restore Hardcount

This page brings Hardcount's database back from a backup, after data loss or a lost server. It is for whoever runs Hardcount. Setting up the backups, and testing that they restore, is on [Back up Hardcount](backup-considerations.md).

A restore takes the database back to the moment of the backup. Everything written after it is gone from Hardcount: usage events, invoices, payments, refunds, and changes made in the dashboard or through the API. Stripe still holds the payments and the refunds.

So Hardcount's background jobs stay paused until this page has matched Hardcount with Stripe. A background job is a loop that one copy of Hardcount runs at a time, such as billing. If Hardcount started straight after a restore, its billing job could charge a customer again for an invoice paid after the backup.

The match does not stop every second charge. When billing resumes, it builds again a subscription invoice that the restore lost, and charges it. An invoice that the backup holds as a draft is charged when it is finalized. [Check 6](#6-resume-the-jobs-and-watch-billing-catch-up) finds those payments and says what to do with each.

**On one VM** (the [Docker Compose install](../production-one-vm.md)), run the `docker compose` commands on this page in `/opt/hardcount/deploy/compose`. Run the SQL in the database shell: `docker compose exec postgres psql -U hardcount -d hardcount`.

## Before you restore

1. **Stop every copy of Hardcount, and keep them stopped until [check 4](#4-match-hardcount-with-stripe).** A copy that runs on the restored database can start billing and charging within seconds. On one VM, run `docker compose stop hardcount-api`. Elsewhere, scale every copy to zero.
2. **On a new server, use the encryption key of the old install.** Set `HARDCOUNT_ENCRYPTION_KEY` to the key the backup's install used, from your secrets store. On one VM, it is in the `.env` file you saved. No other key can read the encrypted columns, such as customer details and each account's Stripe keys ([why](backup-considerations.md#1-keep-the-encryption-key)). With a wrong key, every request that reads them fails, and Hardcount logs an error that contains `decrypt`. The Stripe events of check 4 fail too.
3. **Restore into an empty database.** The script replaces everything the backup holds, including the migration record, the list of database changes already made. It leaves alone a table the backup does not hold, such as one that a newer release created. Such a table causes one of two problems:
   - It refers to a table in the backup (a foreign key). The restore cannot replace that table, so it stops and changes nothing.
   - It refers to no table in the backup. The table stays, but the migration record goes back to the backup's version. The next upgrade runs the migration that made the table again, and can stop on `already exists`.

   To empty the database, delete it and create it again as its owner, the user in `DATABASE_URL`. On one VM:

   ```bash
   docker compose exec -T postgres dropdb -U hardcount hardcount
   docker compose exec -T postgres createdb -U hardcount hardcount
   ```

   **You see:** no output. To keep the old database for a later look, rename it instead of deleting it. Replace the first line with `docker compose exec -T postgres psql -U hardcount -d postgres -c 'ALTER DATABASE hardcount RENAME TO hardcount_old'`.

   Elsewhere, run the same `dropdb` and `createdb` as the owner. Or create a new database as the owner, and change the database name in `DATABASE_URL` and `APP_DATABASE_URL` to it.
4. **On a new Postgres server, create the limited user first.** The limited user, `hardcount_app`, is the user that Hardcount's requests run as, so that Row-Level Security applies to them ([Postgres requirements](postgres-requirements.md)). Users belong to the server, not to the database, so a backup cannot carry them. Without the user, the restore cannot work, so the script stops before it starts ([how this was found](../dev/records/restore-drill-first-run-2026-08-11.md)). On one VM, Compose created the user when Postgres first started: skip this item.

   Use the password that `APP_DATABASE_URL` holds. The second line sets the timeout that [Postgres requirements](postgres-requirements.md#timeouts-on-the-limited-user) calls required.

   ```sql
   CREATE ROLE hardcount_app LOGIN PASSWORD '<the password in APP_DATABASE_URL>';
   ALTER ROLE hardcount_app SET idle_in_transaction_session_timeout = '30s';
   ```

5. **Use tools that match the server.**
   - `pg_restore` must not be newer than the target server. The script stops if it is. Run a matching one in a container: `PG_RESTORE="docker run --rm -i --network host postgres:16-alpine pg_restore"`.
   - `psql` must be at least as recent as `pg_restore`. An older one stops at `invalid command \restrict`, and nothing changes.
   - The script skips the version check when `HARDCOUNT_RESTORE_TOOL_MISMATCH_OK=yes` is set, or when it cannot read the server's version.

## Restore

First note the backup time, in UTC. Checks 3 and 4 use it.

- A dump's name holds the time it started: `hardcount_20261019_030000.dump` started at 03:00:00 UTC on 19 October 2026. For `latest.dump`, run `ls -l /var/backups/hardcount/latest.dump` to see the file it points to.
- After a point-in-time restore, it is the time you restore to. After a provider snapshot, it is the time the snapshot was made.

### With a managed Postgres

Restore the snapshot or the point in time with your provider's tool. If the restore made a new instance, change `DATABASE_URL` and `APP_DATABASE_URL` to its address. Items 3 to 5 of [Before you restore](#before-you-restore) do not apply: the instance brings back its users, and the provider's tool matches it. Then go to [Pause the background jobs](#pause-the-background-jobs).

### From a dump

`scripts/restore.sh` restores the dump in one transaction: either all of it is restored, or nothing changes. Each restored table gets the access rights it has in the backup, even when the target database gives new tables more, as a new Compose install does. It needs `pg_restore` and `psql` on the machine that runs it.

1. Run it against the target database.

   ```bash
   DATABASE_URL="postgres://hardcount:PASSWORD@db-host:5432/hardcount?sslmode=require" \
     ./scripts/restore.sh /var/backups/hardcount/latest.dump
   ```

   **You see:** a warning box naming the target and the backup, then `Type 'yes' to proceed:`. Type `yes`. To skip the question in a script, set `HARDCOUNT_RESTORE_CONFIRM=yes`.

   On one VM, the database has no port open on the VM. So run the script in a Postgres container next to it, and put your file's name in the last line. `HARDCOUNT_RESTORE_CONFIRM=yes` answers the question, so check the name first.

   ```bash
   set -a; . ./.env; set +a
   docker run --rm \
     --network "container:$(docker compose ps -q postgres)" \
     -v /opt/hardcount/scripts:/scripts:ro \
     -v /var/backups/hardcount:/var/backups/hardcount:ro \
     -e DATABASE_URL="postgres://hardcount:${POSTGRES_PASSWORD}@localhost:5432/hardcount?sslmode=disable" \
     -e HARDCOUNT_RESTORE_CONFIRM=yes \
     postgres:16 bash /scripts/restore.sh /var/backups/hardcount/latest.dump
   ```

2. Wait for it to finish.

   **You see:** `All critical tables present.`, a "Restore Summary" block, and `Restore complete. Verify application behavior before resuming traffic.`

   Exit codes: `0` success, `1` a setting or the file is wrong, `2` the restore failed and was rolled back, `3` a critical table is missing after the restore.

## Pause the background jobs

Do this before anything starts Hardcount. The database calls a job a *role*. Run this in `psql` as the database owner:

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

**You see:** five rows, each with a `paused_by`. A job that was paused before the backup keeps its earlier name and reason: note it for [check 6](#6-resume-the-jobs-and-watch-billing-catch-up). Do not start Hardcount until all five rows show a pause.

The pause is a row in the database, so it holds on every copy and through restarts, until check 6 ends it. These five are all the background jobs that charge a card or send an email or a webhook:

| Job | What it does |
|---|---|
| `billing` | Builds invoices and charges cards |
| `dunning` | Retries failed payments, and queues the emails that go with them |
| `email_outbox` | Sends queued emails |
| `webhook_outbox` | Sends queued webhooks |
| `webhook_delivery` | Retries webhooks that failed |

One more piece of work resumes when Hardcount starts: a test clock that was moving forward at the backup time. It bills test customers only, in Stripe's test mode.

The pause does not stop requests. [Check 4, step 3](#4-match-hardcount-with-stripe) lists the requests that still charge a card.

## Checks after a restore

Run them in order. Hardcount stays stopped for checks 1 to 3. Run the SQL in `psql` as the database owner. The blocks that read or change account data begin with `SET app.bypass_rls = 'on';`. Without it, on a managed database whose admin user is not a superuser, they see no rows and change none, with no error, so a check looks passed ([why](runbook/index.md#before-you-start)).

### 1. The schema version matches

Run the `hardcount migrate status` of the release you will start, against the restored database. It reads the database and does not start Hardcount. On one VM:

```bash
docker compose run --rm hardcount-api migrate status
```

**You see:** `state:        up to date, nothing to apply`. Other answers:

- `behind` lists the migrations the release will apply. Apply them now with `hardcount migrate` (on one VM, `docker compose run --rm hardcount-api migrate`). **You see:** `applied <n> migrations: the database is at version <v>`.
- `no migration record` means the backup was restored without its migration table: restore it again, with the table, into an empty database.
- A backup made by Velox, Hardcount's name before v0.9.0, cannot be restored into Hardcount ([Coming from Velox](../upgrades.md#coming-from-velox)).

The record itself, if you want to look:

```sql
SELECT max(version_id) FROM public.goose_db_version;
```

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

### 4. Match Hardcount with Stripe

The restored database does not know about the payments Stripe took after the backup. Billing's own check with Stripe, the reconciler, does not find them: it asks only about charges whose result Hardcount has not saved. This check finds each payment and records it in Hardcount, while the jobs are still paused.

1. **If Hardcount's address changed, point Stripe at the new one.** In each Stripe account, open Hardcount's endpoint in the **Webhooks** tab of Workbench, and change the address part of its URL. Keep the path, `/v1/webhooks/stripe/<id>`. Or point the old name at the new server in DNS. Until then, Stripe sends its events, and the ones you resend in steps 7 and 8, to the old address.
2. **Start Hardcount.** First note the database's time, `SELECT now();`, for check 5. Then start it. On one VM, run `docker compose up -d hardcount-api`.

   **You see:** `/health/ready` answers 200, and the `leader_status` query from [Pause the background jobs](#pause-the-background-jobs) still shows five paused jobs. The 200 does not mean the jobs run.
3. **Until check 6, change nothing through Hardcount.** The pause stops only the background jobs. These requests still charge a card at once:
   - A billing run started through the API (`POST /v1/billing/run`).
   - Creating a subscription, and every change to one: plan, quantity, items, cancel, pause or resume collection, and retry billing. A change first bills a period that has ended, and the restore may have lost that period's invoice.
   - Finalizing an invoice, and **Charge now**.

   Refunds and credit notes wait too: the restored invoices do not show the refunds made after the backup, so Hardcount can refund that money again. Let your app send only the usage events of step 4, and hold its other calls.

   Customers can still pay on the hosted invoice page. One who paid an invoice after the backup sees it unpaid, and can pay it again. If your proxy can, close the paths that start with `/v1/public/` until check 6. They serve the hosted invoice page and the payment links, among other public pages. When a customer pays an invoice twice, Hardcount logs `payment anomaly: second successful charge on an already-paid invoice`. Refund one of the two payments.
4. **Have your app send usage events again.** Send every event it sent after the backup time, with the same `idempotency_key`. Hardcount counts each key once, so an event the backup holds is not counted twice. An event that is not sent again is not billed.
5. **List the payments Stripe took after the backup time.** Do this in each Stripe account your Hardcount accounts use. In the Stripe Dashboard, open **Payments** and filter by date and by the status **Succeeded**. Or use the Stripe CLI with that account's live secret key. `1792378800` is the example backup time of [Restore](#restore), in Unix seconds (`date -u -d '2026-10-19 03:00:00' +%s` on Linux):

   ```bash
   stripe payment_intents search --api-key sk_live_… --limit 100 \
     --query "status:'succeeded' AND created>=1792378800"
   ```

   If the answer says `"has_more": true`, run it again with `--page` set to its `next_page`. Each payment Hardcount made names its invoice in its metadata, as `hardcount_invoice_id`. A payment without it was not made by Hardcount: skip it.
6. **Look up each payment's invoice in the restored database.**

   ```sql
   SET app.bypass_rls = 'on';
   SELECT id, status, payment_status, stripe_payment_intent_id FROM invoices WHERE id = 'hc_inv_…';
   ```

   | You see | Do |
   |---|---|
   | `paid`, and `stripe_payment_intent_id` is this payment's ID | Nothing |
   | `paid` another way: another payment's ID, or none | The customer paid this invoice twice. Refund one of the two payments in Stripe. |
   | `finalized` or `uncollectible` | Resend the payment's event (step 7) |
   | `draft`, any other status, or no row | Note the payment's ID, amount, date and `hardcount_customer_id` for check 6. Do not resend it: Hardcount records a card payment only on a finalized or uncollectible invoice. |

7. **Resend each payment's `payment_intent.succeeded` event to Hardcount.** In the Stripe Dashboard, open the event from the **Events** tab in Workbench and click **Resend**. Or run `stripe events resend evt_… --webhook-endpoint we_… --api-key sk_live_…`, where `we_…` is Hardcount's endpoint in the **Webhooks** tab. Hardcount skips only the events its database holds, and the restored one holds none from after the backup.

   **You see:** the step 6 query shows `paid`. If it does not, check that Stripe sends to Hardcount's current address (step 1). If the invoice still is not paid, note the payment for check 6, as for a draft.

   Stripe resends an event only within 15 days of it. For an older one, record the payment on the invoice instead ([Mark an invoice paid](runbook/manual-interventions.md#mark-an-invoice-paid-offline-payment-received)), with the payment's ID in the note.
8. **Resend the refunds, disputes and saved cards.** Do this after step 7, so that each payment is recorded before its refund. List the events of these types since the backup time, and resend each one as in step 7, oldest first. In the **Events** tab, filter by type and date. Or with the Stripe CLI:

   ```bash
   stripe events list --api-key sk_live_… --limit 100 \
     -d "created[gte]=1792378800" \
     -d "types[]=refund.created" -d "types[]=refund.updated" \
     -d "types[]=refund.failed" -d "types[]=charge.refund.updated" \
     -d "types[]=charge.dispute.created" -d "types[]=charge.dispute.updated" \
     -d "types[]=charge.dispute.closed" \
     -d "types[]=setup_intent.succeeded" -d "types[]=checkout.session.completed"
   ```

   The list starts with the newest event. If the answer says `"has_more": true`, run it again with `--starting-after` set to the last event's ID. What Hardcount does with each:
   - **A refund made in the Stripe Dashboard** is recorded on its invoice, or flagged for you: see [A refund made in Stripe](runbook/refund-made-in-stripe.md).
   - **A refund made through Hardcount** has `hardcount_cn_id` in its metadata. Hardcount cannot take it back in: the credit note that made it is gone, and Hardcount does not record its own refund as one made in Stripe. The restored invoice shows that money as refundable again, so do not refund it again. Record the refund in your own books.
   - **A dispute** is recorded on the invoice its payment paid ([Card disputes](runbook/card-disputes.md)). A dispute of a payment you noted for check 6 is not recorded: note it too.
   - **A saved card** (`setup_intent.succeeded`, `checkout.session.completed`) is saved on its customer again.

### 5. Queued emails and webhooks

Hardcount queues outgoing emails and webhooks in two tables, `email_outbox` and `webhook_outbox`, and sends them later. After a restore, every `pending` row is sent when the jobs resume. If the backup is older than some sends, customers get those emails or webhooks twice.

The safe default: leave the rows `pending` and accept a few duplicates. Webhook receivers should already handle the same event twice.

If you are sure you want to skip them, mark them sent, and write down that you did. Put the time you noted at check 4, step 2, in both lines. The rows queued after it stay `pending`: they are the receipts and webhooks of the payments you recorded at check 4.

```sql
SET app.bypass_rls = 'on';
UPDATE email_outbox SET status = 'dispatched', dispatched_at = now(),
  payload = payload - '{setup_url,update_url,password_reset_url,invite_url,public_token,pdf}'::text[]
  WHERE status = 'pending' AND created_at < '2026-10-20 09:15:00+00';
UPDATE webhook_outbox SET status = 'dispatched', dispatched_at = now()
  WHERE status = 'pending' AND created_at < '2026-10-20 09:15:00+00';
```

The `payload - '{…}'` part removes the links and the PDF from the email rows, so they are not kept for emails that were never sent.

### 6. Resume the jobs, and watch billing catch up

1. **Resume the jobs.** Leave out a job that was paused before the backup, until you know why someone paused it.

   ```sql
   SELECT leader_unpause('billing');
   SELECT leader_unpause('dunning');
   SELECT leader_unpause('email_outbox');
   SELECT leader_unpause('webhook_outbox');
   SELECT leader_unpause('webhook_delivery');
   ```

   **You see:** `t` for each job you resume.

2. **Check that each job runs.**

   ```sql
   SELECT role, paused_at IS NOT NULL AS paused, last_tick_age_s FROM leader_status ORDER BY role;
   ```

   **You see:** `paused` is `f` on every row you resumed. A job runs at once if its last run in the backup is older than its interval, and otherwise when the interval ends. Its `last_tick_age_s` then drops to a few seconds. Billing's interval is in [When invoices are made](../api/timing-and-limits.md#when-invoices-are-made).

   Billing now bills every subscription that was due but not billed, and charges the invoices waiting for a charge. Dunning runs the payment retries that are due.

3. **Watch billing catch up.** Set `API` to your Hardcount address (on one VM, `http://localhost:8080`). Set `METRICS_TOKEN` to the value of Hardcount's setting of that name. Without that setting, `/metrics` is open, except in production, where it answers 401 ([What to watch](../self-host.md#what-to-watch)).

   ```bash
   curl -sS "$API/metrics" -H "Authorization: Bearer $METRICS_TOKEN" \
     | grep -E '^hardcount_billing_(due_subscriptions|oldest_due_age_seconds)\{mode="live"\}'
   ```

   **You see:** once the first billing run ends, `hardcount_billing_oldest_due_age_seconds` is below one billing interval, and `hardcount_billing_due_subscriptions` is back to its usual level, often 0. If they do not fall, see [A background job is stuck](runbook/background-job-stuck.md). A subscription whose billing fails is counted apart: see [Subscriptions whose billing is failing](runbook/billing-failing.md).

4. **Handle the payments you noted in check 4.** For each one, look at the customer's invoices:

   ```sql
   SET app.bypass_rls = 'on';
   SELECT id, status, payment_status, total_amount_cents, billing_period_start, billing_period_end, created_at
   FROM invoices WHERE customer_id = 'hc_cus_…' ORDER BY created_at DESC LIMIT 5;
   ```

   Find the invoice that matches the payment. For a payment you noted at a draft, it is that invoice. Otherwise it is a new invoice with a `billing_period_end` before the payment's date, and an amount close to the payment's. Then:
   - **It is paid:** the customer paid twice. Refund the earlier payment, the one from your list, in the Stripe Dashboard. The invoice then matches the one payment left. No invoice holds the refunded payment, so for up to three days Hardcount logs a warning each time Stripe resends that refund's event. That is expected.
   - **Its charge failed, or it waits for the customer to pay** (`payment_status` `failed` or `pending`): record the earlier payment on it now, before the customer pays it ([Mark an invoice paid](runbook/manual-interventions.md#mark-an-invoice-paid-offline-payment-received)). Put the payment's ID in the note.
   - **Its charge is still running** (`payment_status` `processing` or `unknown`): wait for the result, then follow one of the two cases above. Hardcount refuses to record a payment while a charge runs.
   - **It is still a draft:** it is charged when it is finalized, by billing or by you. Come back to it then.
   - **No invoice matches:** the subscription or invoice behind the payment was lost too, and perhaps its customer. Nothing charges it again for now. Keep the payment's ID for your accounts. If you create the subscription or the invoice again, Hardcount bills and charges it as a new one. Then refund the earlier payment, as in the first case.

**Next:** [Health checks, alerts and logs](runbook/monitoring.md).
